# 06 · `hosts.meta` 永不刷新、`collector_state.last_ok_at` 被失败上报抹成 NULL

- **严重度**：P2（逻辑错误，可观测数据失真）
- **状态**：已修复（2026-11-27）
- **位置**
  - `internal/store/store.go:196`（`Ingest` 的 `ON CONFLICT` 分支）
  - `internal/store/query.go:455`（`SaveCollectorState` 的 `DO UPDATE`）

## 成因 A：hosts.meta 只能写一次

```sql
INSERT INTO sentinel.hosts (..., meta) VALUES (..., $7)
ON CONFLICT (id) DO UPDATE SET
    name = ..., os = ..., arch = ..., kind = ..., addr = ..., last_seen = now()
-- 没有 meta
```

首次上报后，`meta` 永远停留在第一次的值。`api.handleSync` 每次都会带上 `{"kernel":..., "agent":...}`，但只有 `INSERT` 路径生效。

## 成因 B：失败上报把 last_ok_at 写成 NULL

```sql
ON CONFLICT (host_id) DO UPDATE SET
    failures = EXCLUDED.failures, last_error = EXCLUDED.last_error,
    last_ok_at = EXCLUDED.last_ok_at,   -- 失败路径传的是 nil 指针 → NULL
```

采集失败时 `collectors.runOne` 只填 `Failures` / `LastError` / `NextRunAt`，`LastOKAt` 为 `nil`。

## 影响

- 面板节点详情的「Agent 版本 / 内核」在 Agent 升级后**永远显示旧值**，无法判断是否灰度成功；
- 采集器「上次成功时间」在第一次失败后清零，排障时无法区分「从未成功」与「曾经成功过，刚刚坏了」，也让将来基于 `last_ok_at` 的陈旧度告警失去依据。

## 修复

```sql
-- hosts
meta = CASE WHEN EXCLUDED.meta <> '{}'::jsonb THEN EXCLUDED.meta ELSE sentinel.hosts.meta END,

-- collector_state
last_ok_at = COALESCE(EXCLUDED.last_ok_at, sentinel.collector_state.last_ok_at),
```

`meta` 只在本次上报非空对象时覆盖 —— probe / collector 主机上报 `nil` meta，不会把 Agent 的 meta 清空；`last_ok_at` 只在成功路径更新，失败路径保留历史值。

## 验证

真库集成（临时 PG）：

1. 上报 `meta={"agent":"v1"}` → 再报 `{"agent":"v2"}` → `SELECT meta FROM hosts` 为 v2；
2. 之后上报一个不带 meta 的 `HostInfo`（probe 场景）→ meta 仍为 v2，未被清空；
3. `SaveCollectorState(failures=0, last_ok_at=T)` → `SaveCollectorState(failures=2, last_error="timeout")` → 读回 `last_ok_at` 仍为 T，`failures=2`、`last_error="timeout"`。
