# 待跟进项（本次未修改）

这些不是「已确认的 bug」，而是审核中发现的设计风险 / 潜在瓶颈。**未动代码的原因写在每条的「为什么不改」里**，需要先确认契约或策略后再处理。

## 1. `stream_events` 每台主机每 15s 一条 `host.ingested`

- 位置：`internal/store/store.go` `Ingest` 末尾的 `appendStreamTx(..., "host.ingested", ...)`
- 现状：10 台机器 ≈ 5.7 万行/天，7 天保留 ≈ 40 万行；`GET /api/v1/stream` 的长轮询结果里绝大部分是 `host.ingested`。
- 风险：AI 值守端按 `event.opened` / `event.resolved` 消费时会被噪声刷屏，分页 `limit=200` 容易被 ingest 事件挤占，真正的告警要等好几轮才能读到。
- 为什么不改：这是对外的事件契约（README「增量流」段落与 UI 内的值守提示词都提到事件类型），改了会影响已有消费者。
- 建议：仅在有样本时写，或把它降级为分钟级摘要事件；同时给 `stream_events.type` 增加过滤参数。

## 2. 原始曲线查询无条数上限

- 位置：`internal/store/query.go` `QueryMetrics` 的 `span <= 6h` 分支
- 现状：`ORDER BY ts` 无 `LIMIT`。Agent 15s 采样 × 6h = 1440 点/指标，多个指标同时拉取时响应体可达数百 KB。
- 建议：与 rollup 分支一致，按 `limit` 封顶并在响应里返回 `truncated: true`。

## 3. 长轮询每 500ms 打一次 DB

- 位置：`internal/api/api.go` `handleStream`
- 现状：每个等待中的客户端 ≈ 2 次查询/秒（30s 长轮询 = 60 次）。
- 影响：个位数消费者无感；M6 接入 MCP 多消费者后连接池（`MaxConns ≤ 8`）会被长时间占用。
- 建议：改 PG `LISTEN/NOTIFY`（`stream_events` 插入后 `pg_notify`），或把轮询间隔改为 1–2s 并配合 `WaitGroup` 合并。

## 4. 未配置 `AGENT_TOKEN` 时匿名上报被接受

- 位置：`internal/api/api.go` `authorizeAgent` → `if tok == "" { return s.d.AgentToken == "" }`
- 现状：只要没设 `AGENT_TOKEN`，任何能连到 8110 的主机都能以任意 `host_id` 写数据（伪造指标、伪造事件）。
- 为什么不改：这是既有部署约定（本地开发默认无 token），直接改成拒绝会让现有环境全部上报失败。
- 建议：启动时若 `AGENT_TOKEN == ""` 打一条 `WARN`；文档里明确「生产必须设置」。

## 5. `/api/v1/nodes` 明文返回 agent token

- 位置：`internal/api/nodes.go` `handleNodes`（返回 `agent_token`）
- 现状：为了生成「一键安装命令」必须回传 token。设置了 `PANEL_TOKEN` 时受面板鉴权保护，未设置时等同完全公开。
- 建议：仅在确实有 `PANEL_TOKEN` 时回传明文，否则回传占位符并提示「请自行填写」。

## 6. 按主机令牌的鉴权分支不可达

- 位置：`internal/api/api.go` `authorizeAgent` 的 `HostTokenHash` 分支；`internal/store/query.go` `SetHostTokenHash`
- 现状：`hosts.token_hash` 全库无写入入口（没有注册/轮换接口），该分支在生产中恒为 `want == ""` → `false`。
- 建议：要么补「按主机签发/轮换令牌」的接口并写入 `token_hash`，要么删掉该分支，避免误以为已支持租户隔离。

## 7. 面板内联 `onclick` 拼接 id

- 位置：`internal/api/ui.html`（`openHostPolicy('...')`、`resetHostPolicy('...')` 等）
- 现状：把 `esc(id)` 塞进单引号 JS 字符串。`esc()` 会转义 `'`，但 HTML 属性解码后仍会还原成引号，**当前安全仅仅因为 `hostIDRe` 限制 host_id 为 `[A-Za-z0-9_.-]`**。
- 建议：改为 `data-id` + 事件委托；将来一旦放开 id 字符集（比如允许中文），这里立刻变成 XSS。

## 8. 分区保留判断与分区边界存在时区偏移

- 位置：`internal/store/store.go` `DropOldSamples`
- 现状：分区名按 UTC 日期，边界按 DB 时区（Asia/Shanghai），`day.Add(24h).Before(cutoff)` 的判断最多偏 8h。
- 影响：48h 保留 + 48h 上报限制下无实际影响（不会删到窗口内数据），但调整保留期时会暴露。
- 建议：改保留策略时一并统一语义（例如所有分区统一 `UTC` 边界，或分区名带时区标识）。

## 9. 上报值统一截断到 2 位小数

- 位置：`internal/api/api.go` `handleSync` 的 `round2(v)`
- 现状：`probe_latency_ms < 0.01` 会变成 0；`load1` 之类的浮点也只剩两位。
- 建议：若将来采集亚毫秒指标或需要原始精度，按指标类型区分精度。

---

## 回归清单（修复后建议逐条验证）

1. `go test -race -count=2 ./...`（含新增的 collectors / rollup / agent 测试）。
2. 崩溃：面板对 SNMP/probe 节点连续「手动触发」+ 定时采集 30 分钟，`/healthz` 全程 200，无 `concurrent map writes`。
3. 补传：Agent 断网跨零点 24–48h，恢复后 spool 一次排空、无跨日缺口、队列不卡死。
4. DB 抖动：`docker pause sentinel-pg` 1 分钟后恢复，1m 聚合无整分钟空洞。
5. 安全：8MiB/64MiB 解压体各打一次 `/api/v1/agent/sync`，返回 400 且 RSS 无台阶上涨。
6. 资源：跑 24h 后 `ls /proc/<pid>/fd | wc -l` 不随采集轮次增长。
7. 前端：SNMP 接口标签显示「接口 2 接收」；策略页、节点详情无 JS 报错。
