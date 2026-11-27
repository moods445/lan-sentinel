# LanSentinel Bug 记录

> 来源：2026-11-27 全量代码审核（Go 25 个文件、SQL、内嵌前端、部署脚本）。
> 每条记录包含：位置 / 严重度 / 成因 / 影响 / 修复 / 验证方式。
> 状态只有两种：**已修复**（含回归测试）或 **待跟进**（有判断依据，需决策后再动）。

## 已修复

| # | 标题 | 严重度 | 位置 | 记录 |
| --- | --- | --- | --- | --- |
| 01 | 采集器手动触发与定时调度并发写 map，进程崩溃 | P0 崩溃 | `internal/collectors/collectors.go` | [01-collector-concurrent-map-write.md](01-collector-concurrent-map-write.md) |
| 02 | 分钟聚合部分失败时丢弃其余 bucket | P1 数据丢失 | `internal/rollup/rollup.go` | [02-rollup-flush-data-loss.md](02-rollup-flush-data-loss.md) |
| 03 | Agent 上报 gzip 解压炸弹 | P1 安全/内存 | `internal/api/api.go` | [03-sync-gzip-bomb.md](03-sync-gzip-bomb.md) |
| 04 | HTTP 采集每次新建 Transport 且不关空闲连接 | P1 资源泄漏 | `internal/collectors/collectors.go` | [04-collector-http-transport-leak.md](04-collector-http-transport-leak.md) |
| 05 | 只给首个样本日期建分区，补传队列会永久卡死 | P1 边界条件 | `internal/store/store.go` | [05-ingest-partition-missing.md](05-ingest-partition-missing.md) |
| 06 | `hosts.meta` 永不刷新、`last_ok_at` 被失败上报抹成 NULL | P2 逻辑错误 | `internal/store/store.go`、`internal/store/query.go` | [06-meta-and-last-ok-at.md](06-meta-and-last-ok-at.md) |
| 07 | `Store.Host()` 复用 `ListHosts()` 全表拉取 | P2 性能 | `internal/store/store.go` | [07-store-host-full-scan.md](07-store-host-full-scan.md) |
| 08 | Agent 补传时 spool 逐批全量重写 | P2 性能/弱机 IO | `internal/agent/client.go` | [08-agent-spool-write-amplification.md](08-agent-spool-write-amplification.md) |
| 09 | 长轮询 `since` 用 int 解析、`limit` 与底层上限不一致 | P2 边界条件 | `internal/api/api.go` | [09-stream-since-limit.md](09-stream-since-limit.md) |
| 10 | SNMP 接口指标标签 off-by-one、死代码与格式 | P3 规范 | `internal/api/ui.html`、`internal/config/config.go` | [10-ui-label-and-dead-code.md](10-ui-label-and-dead-code.md) |

## 待跟进（未修改）

判断依据与建议见 [open-followups.md](open-followups.md)：流事件噪声、原始曲线无上限、长轮询轮询打库、匿名 sync 与 token 暴露、按主机令牌无写入入口等 9 项。

## 通用验证命令

```bash
gofmt -l .
go vet ./...
go build ./...
go test -race -count=2 ./...
```

各类修复的针对性验证（崩溃复现、DB 抖动、补传跨日、解压炸弹、fd 增长）写在每条记录末尾；整体回归清单见 [open-followups.md](open-followups.md) 末节。
