# 02 · 分钟聚合部分失败时丢弃其余 bucket

- **严重度**：P1（静默数据丢失）
- **状态**：已修复（2026-11-27）
- **位置**：`internal/rollup/rollup.go:85` `func (r *Rollup) Flush`
- **触发条件**：`INSERT ... metric_rollup_1m` 任一条失败（DB 重启、`statement_timeout`、连接被踢）

## 成因

```go
snapshot := r.buckets          // 整张内存表被摘走
r.buckets = map[key]*agg{}     // 内存里已清空
for k, a := range snapshot {
    if err := r.st.InsertRollup(...); err != nil {
        ... 把这个 bucket 放回 ...
        return err             // ← 直接返回，快照里剩下的 bucket 既没落库也没放回
    }
}
```

`Flush` 每 30s 跑一次。第一条插入失败即 `return`，`snapshot` 中尚未处理的 bucket 随着函数返回被垃圾回收，数据永久丢失。

## 影响

- `metric_rollup_1m` 出现成片空洞；
- 面板 `GET /api/v1/hosts/{id}/metrics` 在窗口 > 6h 时**只读汇总表**，因此表现为历史曲线整段缺失；
- 丢失量取决于 map 迭代顺序与失败时机，运维侧无法从日志发现（旧实现除 `Warn("rollup flush failed")` 外没有迹象表明「丢了哪些」）。

## 修复

`internal/rollup/rollup.go`

- 单条失败只影响自己：记录首个错误、`failed` 计数、把该 bucket 放回内存等下一轮重试；
- 其余 bucket 继续写入，最后返回首个错误；
- 部分失败时打 `rollup flush partially failed`（含 failed/total），便于观测。

## 验证

- 新增 `internal/rollup/rollup_test.go`
  - `TestFlushKeepsAllBucketsOnPartialFailure`：注入「前 1 次插入必失败」的假 store，4 个 bucket 两轮后应全部落库；
  - `TestAddRetryMergesIntoExistingBucket`：放回的失败数据与新采样按 `sum/n` 正确合并（40 + 60 → avg 50）。
- 缺陷复现：把 `return err` 还原到副本后，
  `go test ./internal/rollup/` → `want 4 buckets persisted, got 1`。
- 修复后：`go test -race ./internal/rollup/` 通过。
- 人工回归：`docker pause sentinel-pg` 1 分钟后恢复，确认聚合无整分钟空洞，日志只有 `partially failed` 而无静默缺口。
