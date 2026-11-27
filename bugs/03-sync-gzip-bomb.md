# 03 · Agent 上报 gzip 解压炸弹（解压后无上限）

- **严重度**：P1（内存耗尽 / 拒绝服务，且未鉴权即可命中）
- **状态**：已修复（2026-11-27）
- **位置**：`internal/api/api.go:34`（新增 `maxSyncBodyBytes`）、`internal/api/api.go:196`
- **入口**：`POST /api/v1/agent/sync`（`authMW` 对同步接口放行，鉴权在 handler 内、解码之后）

## 成因

```go
r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 只限制压缩体 ≤1MiB
gr, _ := gzip.NewReader(r.Body)
rd = gr                                         // 解压流直接喂给 json.Decoder，无上限
```

`MaxBytesReader` 约束的是**压缩后**字节数，gzip 的膨胀比可达 1000:1 以上：约 8KB 的压缩体即可解出 GB 级 JSON。而 `json.Decoder` 需要在内存中构建 `meta` 等结构。

## 影响

- 控制面软内存上限 `GOMEMLIMIT=96MiB`（`cmd/sentinel/main.go`），大对象分配会导致 GC 疯狂抢占 CPU，最终被容器 `mem_limit=192m` OOM-kill；
- 鉴权发生在解码之后，因此**任何能连到 8110 的主机**都能触发，无需任何 token；
- 即使不 OOM，也会造成长时间 CPU 抖动，挤压同一进程里的规则求值与上报处理。

## 修复

```go
const maxSyncBodyBytes = 4 << 20

rd = io.LimitReader(gr, maxSyncBodyBytes)
```

超过 4MiB 时 `json.Decode` 报 `unexpected EOF` → `400 bad json`，内存占用与耗时都有上界。

## 验证

- 构造解压后 8MiB 的 payload（压缩后仅 8241 字节）：

```bash
python3 -c 'import gzip,json; b={"host":{"id":"h","meta":{"pad":"A"*(8<<20)}}};open("/tmp/bomb.gz","wb").write(gzip.compress(json.dumps(b).encode()))'
curl -s -o /dev/null -w '%{http_code}\n' -H 'Authorization: Bearer <token>' \
     -H 'Content-Encoding: gzip' --data-binary @/tmp/bomb.gz localhost:8110/api/v1/agent/sync
```

修复后返回 `400`（`bad json: unexpected EOF`），修复前会被完整解码。
- 正常上报（193 字节 gzip）仍 `200`，响应里带策略：`{"ok":true,"interval_s":15,"policy":{...}}`。
