# 05 · 只给首个样本日期建分区，跨日补传会让 spool 队列永久卡死

- **严重度**：P1（数据长期缺失 + 客户端队列堵塞）
- **状态**：已修复（2026-11-27）
- **位置**：`internal/store/store.go:82`（新增 `ensureSamplePartitions`），调用点在 `Ingest`
- **相关 SQL**：`sql/001_init.sql` 的 `sentinel.ensure_partition(day date)`（分区名 `metric_samples_YYYYMMDD`）

## 成因

两个独立缺陷叠加：

1. **只保证一个日期**：`Ingest` 只对 `samples[0].TS` 调 `EnsurePartition`。一批样本跨零点（断网 spool 补传、Agent 时钟偏移、跨时区采集）时，其余日期没有分区，`INSERT` 直接报
   `no partition of relation "metric_samples" found for row`。
2. **命名与边界时区不一致**：Go 侧用 `ts.UTC().Format("2006-01-02")` 作为分区名，而 `ensure_partition` 用 `day::timestamptz` 生成边界，走**数据库会话时区**。部署文档要求 `ALTER DATABASE sentinel SET timezone='Asia/Shanghai'`，于是名为 `metric_samples_D` 的分区实际覆盖 UTC `[D-1 16:00, D 16:00)`：
   - UTC 16:00–24:00 的样本会落进**次日**分区（名字对不上）；
   - 若次日分区尚未创建，同样报「无分区」。

## 影响

- 控制面侧：整条 ingest **单事务失败**，`handleSync` 返回 500，这批样本（含主机心跳）全部丢失；
- Agent 侧：`flush()` 是 FIFO（始终重发 `pending[0]`），失败批次留在队首 → **后续所有批次（最多 200 批 / 5MB）永远发不出去**，即使新数据完全正常；
- 表现为「某台机器离线一段时间后，回来就再也不出现在主机墙上」，且必须手工清理 spool 才能恢复。

## 修复

新增 `ensureSamplePartitions`：对一批样本涉及的每个日期，补建 **前/当/后三天** 分区。

- 覆盖任意时区偏移（±14h）与时钟偏差，使写入与数据库会话时区无关；
- 最多处理 4 个样本日期（防御伪造的超长时间跨度）；
- `EnsurePartition` 自带 `days` 缓存，命中时不查库，热路径成本只是 3 次 map 查找。

## 验证

- 真库集成（临时 PG，`TZ=Asia/Shanghai`）：一次 `Ingest` 写入 3 个不同 UTC 日期的样本 → 全部成功，`SELECT tableoid::regclass, count(*) FROM sentinel.metric_samples GROUP BY 1` 显示三行分别落在
  `metric_samples_20261125 / 26 / 27`，无「无分区」错误。
- 人工回归：Agent 断开网络跨零点 24h 后恢复，确认 spool 一次排空、`/api/v1/hosts` 恢复该主机、`stream_events` 不再停留在同一批。
- 回归命令：`go test ./internal/store/`（该包依赖 PG，靠上面的集成方式验证）。
