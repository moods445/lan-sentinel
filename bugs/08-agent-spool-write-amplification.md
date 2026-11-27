# 08 · Agent 补传时 spool 被逐批全量重写（写放大）

- **严重度**：P2（性能瓶颈，弱机 IO/CPU 预算内不可忽略）
- **状态**：已修复（2026-11-27）
- **位置**：`internal/agent/client.go:169` `func (c *Client) flush`

## 成因

```go
for len(c.pending) > 0 {
    if err := c.send(ctx, c.pending[0]); err != nil { ... return err }
    c.pending = c.pending[1:]
    if c.persisted > 0 { c.persist() }   // 每成功一批 → 重写整个 spool 文件 + rename
}
```

`persist()` → `spool.Replace(pending)`：`MkdirAll` + 全量序列化 + 写临时文件 + `os.Rename`。离线期间 `pending` 会涨到上限 200 批，恢复网络后排空一次就要做约 200 次全量落盘。

## 影响

- 单个 Agent 的 unit 限制为 `CPUQuota=2%`、`MemoryMax=32M`，恢复瞬间的连续写入会抢占自身时间片，拖慢采集与上报，反过来延长排空时间；
- Unraid 上 spool 位于 `/mnt/user`（fuse/shard），小文件频繁重写代价更高，且会缩短闪存寿命；
- 200 批 × 每次重写 ≈ 数百 MB 的累计写入，与「极轻量 Agent」的设计目标相悖。

## 修复

排空后统一重写一次：

```go
for len(c.pending) > 0 {
    if err := c.send(ctx, c.pending[0]); err != nil { c.persist(); return err }
    c.pending = c.pending[1:]
}
c.persist()   // persisted == len(pending) 时内部直接 return；pending 为空则清空 spool
```

一致性依据：服务端 `metric_samples` 有 `(host_id, metric, ts)` 唯一索引 + `ON CONFLICT DO NOTHING`，`metric_latest` 用 `WHERE EXCLUDED.ts >= ts` 幂等更新；因此「中途崩溃导致未落盘批次重发」不会产生重复数据（仅可能重复一条 `host.ingested` 流事件，与修复前行为一致）。

## 验证

- 新增 `internal/agent/client_test.go`
  - `TestSyncSpoolDrain`：正常上报后 pending/spool 为空 → 服务停掉连续 2 次失败（pending=2，spool 2 行）→ 换新地址恢复上报，断言排空后 pending=0、spool 0 行、服务端恰好收到 3 次请求（2 批积压 + 本轮新批）；
  - `TestStepHonorsPolicyFilter`：策略下发后下一批只上报白名单指标，`interval` 同步更新为 20s。
- `go test -race ./internal/agent/` 通过。
- 人工回归：断网 30 分钟后恢复，`strace -f -e trace=openat,rename ./bin/sentinel-agent` 观察 spool 重写次数从「每批一次」降为「每轮一次」。
