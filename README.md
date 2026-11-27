# LanSentinel

局域网 7×24 AI 值守样例。当前实现 **M1 + M2**：Go 单二进制控制面（PG 存储、Agent 采集、规则判定、事件与决策日志、内嵌主机墙）与极轻量 Agent。

完整设计说明（架构、数据模型、协议、规则语义、部署运维、资源实测、验收记录、FAQ）见 **[docs/项目说明.md](docs/项目说明.md)**。本文只保留速览与快速开始。

## 已实现

| 能力 | 说明 |
| --- | --- |
| `sentinel serve/migrate/healthcheck` | 单二进制；启动即迁移；`/healthz` 探活；GOMEMLIMIT 96MiB |
| `POST /api/v1/agent/sync` | 上报 + 领任务合一的唯一主通道，15s 一次，gzip，无任务时响应极小 |
| 跨平台 Agent | Linux amd64/arm64、macOS amd64/arm64、Windows amd64；systemd / launchd / 计划任务 / OpenWrt procd |
| Agent 采集 | Linux 直读 `/proc`、`/sys`、`statfs`，macOS/Windows 走系统 API；**零子进程、零监听端口**；断网 spool 补传；自身 CPU 超预算自动降频到 60s |
| 规则引擎 | 每 10s 求值：`metric_threshold`（含 `per_scope`、`for` 持续时长）、`heartbeat`、`collector_failing`；指纹去重 + 冷却 + 抑制留痕 |
| 事件与决策 | `events` + `decisions` + `stream_events`（outbox，同事务写入），条件消失自动关闭并进入冷却 |
| 拉取兜底 | `probe`（TCP 探活）、`http`（JSON/prometheus）、`snmp`（v2c 只读）；并发 ≤2、超时 ≤4s、退避与抖动 |
| 存储 | `metric_samples` 按天 UNLOGGED 分区 + 48h DROP；`metric_latest` 供主机墙；`metric_rollup_1m` 分钟聚合 |
| 增量流 | `GET /api/v1/stream?since=&wait=30s` 长轮询，语义与后续 Kafka/MCP 消费一致 |
| 面板 | `/` 内嵌单页（无构建链，首屏 ~28KB），5s 自动刷新；点击主机卡片打开详情（指标曲线、事件、决策日志）；可选 `PANEL_TOKEN` |
| 热加载 | `POST /api/v1/admin/reload`（或 SIGHUP 重启进程外手动触发）重载 `rules.yaml` / `collectors.yaml` |

尚未实现（后续里程碑）：动作目录与自动处置、流水线/租约/审批、MCP、回放页面。完整清单见 **[todos/todo.md](todos/todo.md)**。

## 一键安装

推 tag（`v*`）会触发 GitHub Actions：跑测试、交叉编译、发布 Release 资产，并把多架构镜像推到 GHCR。

```bash
BASE=https://raw.githubusercontent.com/moods445/lan-sentinel/master/install.sh

# 控制面 + Agent，全 Docker（镜像来自 GHCR，自带 PostgreSQL）
curl -fsSL $BASE | sudo bash -s -- all --mode docker --port 8110

# 只装控制面（Docker，使用已有 PG）
curl -fsSL $BASE | sudo bash -s -- server --mode docker \
  --dsn 'postgres://user:pass@host:5432/sentinel?sslmode=disable'

# 控制面二进制（从 Release 下载并校验 sha256）
curl -fsSL $BASE | sudo bash -s -- server --mode binary --dsn 'postgres://...'

# Agent 二进制（Linux systemd / OpenWrt procd / macOS launchd）
curl -fsSL $BASE | sudo bash -s -- agent --mode binary \
  --server http://192.168.1.102:8110 --token <AGENT_TOKEN>

# Agent Docker
curl -fsSL $BASE | sudo bash -s -- agent --mode docker \
  --server http://192.168.1.102:8110 --token <AGENT_TOKEN>
```

- 默认使用最新 Release；固定版本加 `--version v0.2.1`；
- 镜像：`ghcr.io/moods445/lan-sentinel:<tag|latest>`、`ghcr.io/moods445/lan-sentinel-agent:<tag|latest>`（amd64 + arm64）；
- 项目约定不本地构建 Docker 镜像，一律使用 GHCR 已构建好的镜像；
- 首次发布后需在 GitHub 的 Packages 设置里把两个包改为 **Public**，否则匿名拉取会 401；
- 全部参数见 `./install.sh --help`。

## 快速开始（本机）

```bash
# 1. 依赖：Go 1.25+，以下二选一
make demo-db                      # 临时 PG16，端口 55432
# 或使用已有 PG：自行设 PG_DSN

# 2. 构建
make build                        # bin/sentinel + bin/sentinel-agent（静态、-s -w）

# 3. 起控制面
PG_DSN='postgres://sentinel_app:sentinel@127.0.0.1:55432/sentinel?sslmode=disable' \
AGENT_TOKEN=test-token CONFIG_DIR=./config DATA_DIR=/tmp/ls-data ./bin/sentinel serve

# 4. 起 Agent（同机或任意被监控机）
SENTINEL_TOKEN=test-token ./bin/sentinel-agent \
  -server http://127.0.0.1:8110 -host-id debian-server -mounts / -data-dir /tmp/ls-agent-data

# 5. 打开面板
open http://127.0.0.1:8110/
```

自检：

```bash
./bin/sentinel-agent -print                   # 打印一次采集结果
curl localhost:8110/healthz
curl localhost:8110/api/v1/hosts
curl 'localhost:8110/api/v1/events?status=open'
curl 'localhost:8110/api/v1/stream?since=0&wait=1s'
```

## Unraid 部署（只跑程序，镜像来自 GHCR）

**不在本机构建镜像**：推 tag 时 GitHub Actions 构建并推送多架构镜像到 GHCR，Unraid 直接拉取。

```bash
cp .env.example .env            # 填 PG_DSN / AGENT_TOKEN；镜像已默认指向 GHCR
make deploy UNRAID=unraid       # scp compose + 配置 → 远端 docker compose pull && up -d
```

或只在 Unraid 上操作（`/mnt/user/appdata/lan-sentinel` 已有 compose 与 `.env`）：

```bash
cd /mnt/user/appdata/lan-sentinel
docker compose pull && docker compose up -d --force-recreate
```

- 版本在 `.env` 固定：`SENTINEL_IMAGE=ghcr.io/moods445/lan-sentinel:v0.2.1`、
  `SENTINEL_AGENT_IMAGE=ghcr.io/moods445/lan-sentinel-agent:v0.2.1`（不填则用 `:latest`）；
  升级只改这两行再 `docker compose pull`，不做任何本地构建。
- 首次需在 GitHub Packages 里把两个包设为 **Public**，否则 Unraid 拉取会 401。

首次需建独立库与角色（**交互式 `\password`**，明文不进 SQL 历史；密码由你自己保管并写入 `.env` 的 `PG_DSN`）：

```bash
ssh unraid
docker exec -it postgresql16 psql -U root -d postgres
# psql 里执行：
CREATE ROLE sentinel_app LOGIN;
CREATE DATABASE sentinel TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C';
ALTER DATABASE sentinel SET timezone='Asia/Shanghai';
ALTER DATABASE sentinel SET statement_timeout='10s';
ALTER DATABASE sentinel SET work_mem='8MB';
GRANT CONNECT ON DATABASE sentinel TO sentinel_app;
\password sentinel_app        -- 输入两遍密码，客户端本地做 SCRAM
\q
```

然后把同一个密码填进 `/mnt/user/appdata/lan-sentinel/.env` 的 `PG_DSN`，再：

```bash
ssh unraid 'sh /mnt/user/appdata/lan-sentinel/init.sh'
```

- 端口 `8110`（8090 归 beszel），`mem_limit=192m`、`cpus=0.75`、日志 10m×2。
- 配置只读挂载 `./config:/config:ro`；数据在 Unraid 的 `postgresql16` 容器内，库名 `sentinel`。
- Unraid 自身也作为被监控机：`agent` 服务 `pid=host` + `network_mode=host`，只读挂载 `/`、`/sys`，
  容器内进程不监听端口，`mem_limit=32m`、`cpus=0.05`。

## Agent 安装

```bash
make build && make cross          # 本机 + linux/arm64、darwin、windows
```

Linux（systemd）：

```bash
sudo sh ./agent/install.sh http://192.168.1.102:8110 <agent-token>
```

macOS（launchd，需 sudo）：

```bash
sudo sh ./agent/install-macos.sh http://192.168.1.102:8110 <agent-token>
# 无 sudo 时用当前用户 LaunchAgent（仅登录后运行）
sh ./agent/install-macos.sh --user http://192.168.1.102:8110 <agent-token>
```

Windows（管理员 PowerShell，开机计划任务）：

```powershell
powershell -ExecutionPolicy Bypass -File .\agent\install-windows.ps1 `
  -Server http://192.168.1.102:8110 -Token <agent-token>
```

OpenWrt（procd，arm64）：

```bash
sh ./agent/install-openwrt.sh http://192.168.1.102:8110 <agent-token>
```

Linux unit 内置 `MemoryMax=32M`、`CPUQuota=2%`、`Nice=10`，写 `/etc/lan-sentinel/agent.json`，
spool 在 `/var/lib/lan-sentinel/`；macOS 写 `/Library/LaunchDaemons`，Windows 写 `C:\ProgramData\LanSentinel`。

## 采集策略（控制面下发）

面板右上角「采集策略」配置全局默认，主机详情抽屉里可为单机覆盖，保存后 Agent 在下一次 sync 自动应用，
无需重装或重启。策略包含：

- `enabled_metrics`：指标白名单，支持末尾通配符（`temp:*`、`disk_used_pct:*`），留空 = 全量采集；
- `interval_s`：上报间隔（0 或留空 = 沿用 Agent 本地配置）；
- `mounts`：监控挂载点（最多 3 个，留空 = 沿用本地配置）。

主机覆盖中留空的字段继承全局。Agent 会把最近一次策略写入 `<data_dir>/policy.json`，断网重启后继续沿用，
因此控制面短暂不可达也不会突然恢复全量采集。

## 添加节点

面板右上角「添加节点」提供两种接入方式：

- **能装 Agent**：生成一键安装命令（含控制面地址与 `AGENT_TOKEN`），在目标机用 root 执行，约 15 秒后
  出现在主机墙；Linux systemd / OpenWrt procd / macOS launchd 均支持。
- **不能装 Agent**：填入 IP（可选端口与名称），控制面创建 `probe` 节点，每 30 秒做一次 TCP 可达性探测，
  产出 `probe_ok` 与 `probe_latency_ms`，适合路由器、交换机、打印机、UPS。

探测节点保存在 `collector_targets` 表，控制面重启后自动恢复；面板内可移除（同时清理该主机的最新值与
采集状态）。Agent 节点无需登记，首次上报即自动出现。

## AI Agent 接入

面板右上角「AI 接入」提供三段可直接复制的内容：

- **接入方式**：HTTP API 清单（主机、事件、决策、曲线、长轮询流、采集器、策略），MCP 尚未提供；
- **值守提示词**：7×24 值守的连接方式、`next_seq` 游标续读、巡检节奏与输出结构；
- **授权与排障**：只读自动执行、低/中/高风险分级、401 自动重新鉴权、429/5xx 退避，以及
  401 误令牌、`probe_down`、采集器失败三个授权演练步骤与自检脚本；内置「运行授权自检」按钮，
  验证错误令牌返回 401、当前令牌返回 200。

MCP 服务计划在 M6 提供，届时以 `stream_events` 为资源、查询能力为 tools，鉴权方式与 HTTP API 一致。

## 资源实测

| 进程 | 二进制 | RSS | CPU（空闲） |
| --- | --- | --- | --- |
| sentinel（Debian 本机，PG 本机） | 12.1MB | **16.3MB** | **0.029%** |
| sentinel-agent（Debian 本机） | 7.0MB | **11.7MB** | **0.013%** |
| sentinel（Unraid 容器，PG 本机） | 12.1MB | **9.7MB** | **0.00%**（限 192m/0.75 核） |
| sentinel-agent（Unraid 容器，pid=host） | 7.0MB | **4.7MB** | **0.00%**（限 32m/0.05 核） |

Agent 15s 一次 gzip 上报、30s 采样；控制面每 10s 规则求值、每 30s 聚合落库。

## HTTP 接口（节选）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/v1/agent/sync` | Agent 上报 + 领任务（gzip，Bearer token） |
| GET | `/api/v1/hosts` | 主机墙数据（最新指标 + 在线状态 + 未关闭事件数） |
| GET | `/api/v1/hosts/{id}/metrics?metric=&from=&to=` | 指标曲线（≤6h 原始，>6h 走 1m 汇总） |
| GET | `/api/v1/events?status=open&host=` | 事件列表 |
| GET | `/api/v1/decisions?host=` | 决策日志（open/resolve/suppressed…） |
| GET | `/api/v1/stream?since=&limit=&wait=30s` | 增量流长轮询（outbox，seq 单调） |
| POST | `/api/v1/admin/reload` | 热加载规则/采集器 |
| POST | `/api/v1/collectors/{host_id}/run` | 手动触发一次拉取 |
| GET | `/api/v1/policies` | 全局策略 + 各主机覆盖项 |
| PUT | `/api/v1/policies/global` | 保存全局采集策略 |
| PUT / DELETE | `/api/v1/policies/hosts/{id}` | 保存 / 删除单机覆盖 |
| GET | `/api/v1/nodes` | 添加节点信息（安装脚本、控制面地址、探测节点） |
| POST | `/api/v1/nodes/probe` | 添加无 Agent 探测节点（IP/端口） |
| DELETE | `/api/v1/nodes/probe/{id}` | 移除探测节点 |

## 目录

```
cmd/sentinel           控制面入口（serve|migrate|healthcheck）
cmd/sentinel-agent     Agent 入口（daemon|once|print）
internal/agent         Linux 采集、gzip sync、spool、自适应降频
internal/api           HTTP 接口 + 内嵌主机墙
internal/rules         规则引擎（开/关事件 + 决策留痕）
internal/collectors    probe / http / snmp 拉取兜底
internal/rollup        分钟聚合
internal/store         PostgreSQL（分区、幂等写入、事件、增量流）
sql/                   内嵌迁移脚本
config/                rules.yaml / collectors.yaml
deploy/unraid          Unraid 初始化脚本
```

## 设计约束（与方案文档一致）

- 自研 Agent 是唯一默认采集主体；拉取只用于装不了 Agent 的设备，探活兜底。
- 规则优先；AI 是旁路消费者，控制面从不主动调用模型，缺席不阻塞。
- 所有决策写 `decisions`；所有业务写与 `stream_events` 同事务（outbox）。
- Agent 零子进程、零端口、无 shell；动作执行留到 M5 的动作目录。
