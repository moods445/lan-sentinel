# 09 · 长轮询 `since` 用 int 解析、`limit` 与底层上限不一致

- **严重度**：P2（边界条件：32 位平台溢出 + 分页语义错误）
- **状态**：已修复（2026-11-27）
- **位置**：`internal/api/api.go:379` `handleStream`、`internal/api/api.go:513`（新增 `queryInt64`）
- **相关**：`internal/store/query.go` `StreamSince` 内部把 `limit` 夹到 `1..500`（默认 200）

## 成因

```go
since := int64(queryInt(r, "since", 0))   // int 是 32 位（armv7/386 构建），seq 是 bigint
limit := queryInt(r, "limit", 200)        // 未与 store 的上限对齐
...
"has_more": len(evs) == limit,            // limit 可能是 100000，恒为 false
```

1. `seq` 是 `bigserial`（bigint），而 `queryInt` 走 `strconv.Atoi`。在 32 位平台（本项目交叉编译 `linux/armv7`、`linux/386`）超出 `int32` 的游标解析失败 → **静默回落 0**，客户端以为自己要「从头读」，实际重新消费全部历史事件；
2. 负数 `since` 未被处理，等价于从头读；
3. `limit` 透传 100000 时，Store 层截到 200，但响应里的 `has_more` 用未截断的 100000 比较 → 永远 `false`，分页客户端会漏读后续事件。

## 影响

- 32 位 Agent/消费者长期运行后（`stream_events` 序列号增长）游标失效，重复全量读取，`/api/v1/stream` 返回体积与 DB 压力突然放大；
- AI 值守端按 `has_more` 分页续读时会漏事件，导致「有告警但值守没读到」。

## 修复

```go
since := queryInt64(r, "since", 0)   // strconv.ParseInt(v, 10, 64)
if since < 0 { since = 0 }
limit := queryInt(r, "limit", 200)
if limit <= 0 || limit > 500 { limit = 200 }   // 与 store.StreamSince 完全一致
```

响应中的 `has_more` / `next_seq` 均基于同一 `limit`，语义恢复正确。

## 验证

- `curl 'localhost:8110/api/v1/stream?since=9223372036854775807&wait=0s'` →
  `{"count":0,"events":[],"next_seq":9223372036854775807}`（不再回落 0 重放）；
- `curl 'localhost:8110/api/v1/stream?since=0&limit=100000&wait=0s'` → `has_more:false` 且 `count` 为真实条数；
- `GOOS=linux GOARCH=armv7 go build ./cmd/sentinel` 交叉编译通过。
