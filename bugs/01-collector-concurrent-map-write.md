# 01 · 采集器手动触发与定时调度并发写 map，控制面进程崩溃

- **严重度**：P0（进程级崩溃，不可 recover）
- **状态**：已修复（2026-11-27）
- **位置**：`internal/collectors/collectors.go`
  - 原 `tick()` 在锁外写 `it.next`
  - 原 `runOne()` 读写 `it.failures` / `it.next` / `it.prev` 无同步
- **触发入口**：`POST /api/v1/collectors/{id}/run` → `Manager.RunOnce` 与 `Manager.Run`（每 1s `tick` 派生的 goroutine）

## 成因

同一个 `item` 会被两条路径同时操作：

1. `tick()` 每秒扫一遍 `m.items`，把到期的 item 交给 goroutine 执行 `runOne`；
2. 面板「手动触发一次拉取」直接调 `RunOnce` → `runOne`。

`runOne` 里 `collectSNMP` → `rate()` 会执行 `it.prev[key] = prevSample{...}`（Go map 写入），`failures`/`next` 也是裸字段。两条路径都不受同一把锁保护，因此：

- 并发写 `it.prev` map → **`fatal error: concurrent map writes`**，运行期直接终止进程（这类 runtime error 无法被 `recoverMW` 的 `recover()` 捕获）；
- `failures`/`next` 数据竞争 → 退避时间与连续失败计数错乱，`collector_failing` 规则忽早忽晚。

SNMP 采集器是本项目为交换机/路由器提供的正式能力，`interval` 60s、超时 4s，人工点「手动触发」正好落在窗口内的概率并不低。

## 影响

- 整个控制面进程退出（`restart: unless-stopped` 会拉起，但期间所有 Agent 上报 5xx、所有事件流中断）；
- 崩溃前正在写的 ingest 事务回滚，`-race` 下还能看到 `next`/`failures` 的竞争写。

## 修复

`internal/collectors/collectors.go`

- `item` 的调度状态改为原子量：`next atomic.Int64`（UnixNano）、`failures atomic.Int32`，`tick` 扫描时无需加锁；
- `prev` 与「一次完整采集」由 `item.mu` 保护，`runOne` 全程持锁，使**定时任务与手动触发串行**；
- `rebuildLocked()` 不再共享旧 `item` 的 `prev` map（改为复制；并在注释中说明为何不跨重建继承 SNMP 计数基线）。

配套结构性改进（避免修复本身引入阻塞）：

- `tick` 不再在持有 `Manager.mu` 时等待采集中的 `item.mu`。否则任一慢采集（最长 4s）会让面板侧 `SetExtra`/`Reload`（加节点、热加载）跟着阻塞，最坏超过 HTTP 10s 超时。

## 验证

- 新增 `internal/collectors/manager_test.go`
  - `TestConcurrentRunOnceAndTick`：20 轮「手动触发 + tick 派发」交叉执行（`closedPort` 目标，`Interval=1ms` 使其反复到期）；
  - `TestFailurePathStateUpdates`：失败分支正确累加 `failures` 并推后 `next`。
- 缺陷复现：把加锁改动回退到仓库副本后执行
  `go test -race ./internal/collectors/` →
  `WARNING: DATA RACE … (*Manager).runOne() … goroutine 23 / previous write … goroutine 21`。
- 修复后：`go test -race -count=2 ./internal/collectors/` 通过。
- 人工回归：对 SNMP/probe 节点连续点「手动触发」，同时保持定时采集 30 分钟，`/healthz` 全程 200，日志无 `concurrent map writes`。
