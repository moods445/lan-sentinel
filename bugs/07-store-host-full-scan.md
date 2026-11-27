# 07 · `Store.Host()` 复用 `ListHosts()` 导致单主机查询退化为全表聚合

- **严重度**：P2（性能瓶颈）
- **状态**：已修复（2026-11-27）
- **位置**：`internal/store/store.go:297`

## 成因

```go
func (s *Store) Host(ctx context.Context, id string) (*HostRow, error) {
    list, err := s.ListHosts(ctx)   // 全部主机 + jsonb_object_agg 全部最新值
    for i := range list { if list[i].ID == id { return &list[i], nil } }
    return nil, nil
}
```

`ListHosts` 是 `hosts LEFT JOIN metric_latest` 的 `GROUP BY h.id` + `jsonb_object_agg`，会扫描全部主机的全部指标行并把 JSON 反序列化成 `map[string]float64`。

## 影响

调用点（热路径）：

- `GET /api/v1/hosts/{id}`、`GET /api/v1/hosts/{id}/metrics?metric=`（面板每次进详情页都会打）；
- `POST /api/v1/nodes/probe` 的「host id 是否被占用」检查；

代价随 `主机数 × 指标数` 增长：几十台机器、每台几十个指标时，每次点开详情都要做一次全表聚合 + 全量 JSON 解码，同时占用连接池（`MaxConns ≤ 8`）与内存，面板「点击卡片看详情」明显变慢并挤压 Agent 上报。

## 修复

改为带相关子查询的单行查询：

```sql
SELECT h.id, h.name, ..., COALESCE(h.meta,'{}'::jsonb),
       COALESCE((SELECT jsonb_object_agg(ml.metric, ml.value)
                 FROM sentinel.metric_latest ml WHERE ml.host_id = h.id), '{}'::jsonb)
FROM sentinel.hosts h WHERE h.id = $1
```

`pgx.ErrNoRows` → 返回 `(nil, nil)`，与旧实现「找不到返回 nil」的接口约定保持一致（`handleHost` 仍据此返回 404）。

## 验证

- 真库集成：`Host("verify-host")` 返回 name/kind/metrics 正确；`Host("no-such-host")` 返回 `nil, nil`；
- HTTP 冒烟：`GET /api/v1/hosts`（列表）与 `GET /api/v1/hosts/{id}`、`/metrics` 均正常，`GET /api/v1/hosts/nope` → 404；
- 面板「添加节点 → 填已被占用的名称」仍返回 409 冲突提示。
