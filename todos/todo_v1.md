# TODO v1

最小可编码单元清单。完成一项将 `[ ]` 改为 `[x]`，并与实际进度保持同步。

- [x] Agent 采集 IO 明细：Linux `/proc/diskstats` + `/proc/PID/io|stat|statm`，macOS/Windows 走 gopsutil（60s 低频，设备速率/IOPS/忙碌度/延迟/队列 + 热点进程）
- [x] sync 协议与存储：`detail` 快照字段，落 `host_io_latest`（仅最新值，7 天清理），IO 摘要写窄表指标供趋势图
- [x] 控制面接口：`GET /api/v1/hosts/{id}/detail`（兼容旧 `/io`）
- [x] 明细扩展到 CPU / 内存 / 网络 / 负载：每核使用率、内存/缓存/Swap、每网卡速率与错误、loadavg 与阻塞进程
- [x] 面板：CPU/内存/网络/负载/IO 五个明细面板，列表默认前 3 行、其余折叠可展开
- [x] 面板：右侧半透明浮动目录（锚点跳转 + 滚动高亮），窄屏自动隐藏
- [x] 面板：明细表格默认支持表头排序，展开/收起按钮移至表格右上角
- [x] 验证：`go test ./...`、`go vet`、linux/darwin/windows 交叉编译、临时 PG + Agent 联调、浏览器渲染
