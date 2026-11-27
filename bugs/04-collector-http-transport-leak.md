# 04 · HTTP 采集每次新建 Transport 且不关闭空闲连接（fd 泄漏）

- **严重度**：P1（资源泄漏，长期运行必然耗尽 fd）
- **状态**：已修复（2026-11-27）
- **位置**：`internal/collectors/collectors.go:264`
- **影响对象**：`type: http` 的拉取型采集器（`collectors.yaml` 或面板探测节点之外的 HTTP 目标）

## 成因

```go
tr := &http.Transport{TLSClientConfig: ..., MaxIdleConns: 2}
client := &http.Client{Timeout: timeout, Transport: tr}
...
resp, _ := client.Do(req)
defer resp.Body.Close()   // 只关闭 body，连接被放进该 Transport 的空闲池
```

- `Transport` 是**每次调用新建**的，函数返回后已无引用，但连接仍被 Transport 的空闲池和负责保活的 goroutine 持有；
- `IdleConnTimeout` 零值 = 不超时，`CloseIdleConnections()` 从未被调用；
- `resp.Body.Close()` 只把连接归还空闲池，不会关闭底层 TCP。

## 影响

每轮采集泄漏 1 个 fd（默认 `interval` 60s → 约 1440 个/天，TLS 目标更明显）。累计到 `ulimit -n`（容器内常为 1024）后：

- 新连接（包括 Agent 上报的监听 accept、PG 连接）全部 `too many open files`；
- 表现为采集器先失败 → `collector_failing` 事件 → 随后整个控制面不可用，且重启才能恢复。

## 修复

```go
tr := &http.Transport{...}
defer tr.CloseIdleConnections()
```

一次采集结束即关闭该 Transport 的空闲连接，fd 归还内核。

## 验证

- `go vet` / `go build` / `go test -race ./internal/collectors/` 通过；
- 人工回归：配置一个 `type: http` 采集器指向本机健康检查端点，`interval: 10s` 跑 30 分钟后
  `ls /proc/$(pgrep -f 'sentinel serve')/fd | wc -l` 稳定不随轮次增长（修复前每轮 +1）。
- 功能回归：`POST /api/v1/collectors/{id}/run` 仍返回 200，`http_ok` / `http_latency_ms` / `http_status` 正常落库。
