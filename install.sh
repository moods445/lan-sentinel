#!/usr/bin/env bash
# LanSentinel 一键安装：控制面 / Agent，可选二进制或 Docker
#
# 用法：
#   ./install.sh server --mode docker   [--dir DIR] [--port 8110] [--dsn DSN] [--no-db] [--no-build]
#   ./install.sh server --mode binary   [--dsn DSN] [--listen :8110]
#   ./install.sh agent  --mode binary   [--server URL] [--token T] [--host-id ID] [--mounts /,/data]
#   ./install.sh agent  --mode docker   [--server URL] [--token T] [--host-id ID] [--dir DIR]
#   ./install.sh all    --mode docker|binary [--server URL] [--token T]
#
# 也可用环境变量：SENTINEL_MODE / SENTINEL_SERVER / SENTINEL_TOKEN / SENTINEL_HOST_ID
#                SENTINEL_DIR / SENTINEL_DSN / SENTINEL_PANEL_TOKEN
#
# 说明：
#   - server 的 docker 模式默认带一个本地 PostgreSQL（--no-db 可关闭）；
#   - 镜像默认从当前仓库构建（--no-build 跳过，使用已有同名镜像）；
#   - Linux 二进制模式装 systemd（OpenWrt 自动用 procd），macOS 走 launchd；
#   - Docker 模式仅支持 Linux（Agent 需要 pid=host 只读挂载 /proc）。
set -euo pipefail

ROOT=$(cd "$(dirname "$0")" && pwd)
VERSION=${VERSION:-0.2.0}
IMAGE=${SENTINEL_IMAGE:-lan-sentinel:$VERSION}
AGENT_IMAGE=${SENTINEL_AGENT_IMAGE:-lan-sentinel-agent:$VERSION}

MODE=${SENTINEL_MODE:-}
SERVER=${SENTINEL_SERVER:-}
TOKEN=${SENTINEL_TOKEN:-}
HOST_ID=${SENTINEL_HOST_ID:-$(hostname 2>/dev/null || echo unknown)}
MOUNTS=${SENTINEL_MOUNTS:-/}
DIR=${SENTINEL_DIR:-}
DSN=${SENTINEL_DSN:-}
PANEL_TOKEN=${SENTINEL_PANEL_TOKEN:-}
LISTEN=${SENTINEL_LISTEN:-:8110}
PORT=${SENTINEL_PORT:-8110}
WITH_DB=1
DO_BUILD=1

log()  { printf '\033[36m[install]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

usage() {
  cat <<'USAGE'
LanSentinel 一键安装：控制面 / Agent，可选二进制或 Docker

用法：
  ./install.sh server --mode docker   [--dir DIR] [--port 8110] [--dsn DSN] [--no-db] [--no-build]
  ./install.sh server --mode binary   [--dsn DSN] [--listen :8110]
  ./install.sh agent  --mode binary   [--server URL] [--token T] [--host-id ID] [--mounts /,/data]
  ./install.sh agent  --mode docker   [--server URL] [--token T] [--host-id ID] [--dir DIR]
  ./install.sh all    --mode docker|binary [--server URL] [--token T]

环境变量：
  SENTINEL_MODE / SENTINEL_SERVER / SENTINEL_TOKEN / SENTINEL_HOST_ID
  SENTINEL_DIR / SENTINEL_DSN / SENTINEL_PANEL_TOKEN

说明：
  - server 的 docker 模式默认带一个本地 PostgreSQL（--no-db 可关闭）；
  - 镜像默认从当前仓库构建（--no-build 跳过，使用已有同名镜像）；
  - Linux 二进制模式装 systemd（OpenWrt 自动用 procd），macOS 走 launchd；
  - Docker 模式仅支持 Linux（Agent 需要 pid=host 只读挂载 /proc）。
USAGE
  exit "${1:-0}"
}

COMPONENT=""
while [ $# -gt 0 ]; do
  case "$1" in
    server|agent|all) COMPONENT=$1 ;;
    --mode) MODE=${2:-}; shift ;;
    --binary) MODE=binary ;;
    --docker) MODE=docker ;;
    --server) SERVER=${2:-}; shift ;;
    --token) TOKEN=${2:-}; shift ;;
    --host-id) HOST_ID=${2:-}; shift ;;
    --mounts) MOUNTS=${2:-}; shift ;;
    --dir) DIR=${2:-}; shift ;;
    --dsn) DSN=${2:-}; shift ;;
    --panel-token) PANEL_TOKEN=${2:-}; shift ;;
    --listen) LISTEN=${2:-}; shift ;;
    --port) PORT=${2:-}; shift ;;
    --no-db) WITH_DB=0 ;;
    --no-build) DO_BUILD=0 ;;
    -h|--help) usage 0 ;;
    *) die "未知参数：$1（用 --help 查看用法）" ;;
  esac
  shift
done

[ -n "$COMPONENT" ] || usage 1
[ -n "$MODE" ] || die "请用 --mode docker 或 --mode binary 指定安装方式"
case "$MODE" in docker|binary) ;; *) die "--mode 只能是 docker 或 binary" ;; esac

rand_token() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 16
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

as_root() {
  if [ "$(id -u)" = "0" ]; then "$@"; else sudo "$@"; fi
}

require_root() {
  [ "$(id -u)" = "0" ] || die "该安装方式需要 root（可用 sudo 重新执行）"
}

is_openwrt() {
  [ -f /etc/rc.common ] && [ ! -d /run/systemd/system ]
}

# ---------- 二进制准备 ----------

build_binary() {
  local out=$1 pkg=$2
  [ -x "$out" ] && return 0
  command -v go >/dev/null 2>&1 || die "未找到 $out，且本机没有 Go，无法从源码构建"
  log "构建 $(basename "$out") ..."
  ( cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o "$out" "$pkg" )
}

server_bin() {
  local b=${SENTINEL_BIN:-$ROOT/bin/sentinel}
  build_binary "$b" ./cmd/sentinel
  printf '%s' "$b"
}

agent_bin() {
  local os arch suffix="" b
  os=$(uname -s); arch=$(uname -m)
  case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; armv7l) arch=armv7 ;; esac
  case "$os" in
    Linux)  b=$ROOT/bin/sentinel-agent ;;
    Darwin) suffix="-$arch"; b=$ROOT/bin/sentinel-agent-darwin-$arch ;;
    *) die "暂不支持在 $os 上自动安装，请参考 README 手动安装" ;;
  esac
  if [ "$os" = "Linux" ] && [ "$arch" != "$(uname -m | sed 's/x86_64/amd64/')" ] && [ "$arch" != "amd64" ]; then
    b=$ROOT/bin/sentinel-agent-linux-$arch
  fi
  if [ ! -x "$b" ] && command -v go >/dev/null 2>&1; then
    log "构建 Agent ($os/$arch) ..."
    ( cd "$ROOT" && CGO_ENABLED=0 GOOS=$(echo "$os" | tr 'A-Z' 'a-z') GOARCH="$arch" \
        go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o "$b" ./cmd/sentinel-agent )
  fi
  [ -x "$b" ] || die "未找到 Agent 二进制：$b（请先 make cross 或安装 Go）"
  printf '%s' "$b"
}

# ---------- 控制面 ----------

install_server_docker() {
  need docker
  docker compose version >/dev/null 2>&1 || die "需要 docker compose 插件"
  local dir=${DIR:-/opt/lan-sentinel}
  as_root mkdir -p "$dir/config" "$dir/data"
  local envf=$dir/.env
  if [ -f "$envf" ]; then
    warn "$envf 已存在，保留原配置不覆盖"
    TOKEN=$(grep -E '^AGENT_TOKEN=' "$envf" | cut -d= -f2- || true)
    PANEL_TOKEN=$(grep -E '^PANEL_TOKEN=' "$envf" | cut -d= -f2- || true)
  else
    PANEL_TOKEN=${PANEL_TOKEN:-$(rand_token)}
    TOKEN=${TOKEN:-$(rand_token)}
    as_root tee "$envf" >/dev/null <<EOF
# 由 install.sh 生成：$(date -Iseconds)
SENTINEL_LISTEN=:${PORT}
PG_DSN=${DSN:-postgres://sentinel_app:sentinel@postgres:5432/sentinel?sslmode=disable&pool_max_conns=8}
PANEL_TOKEN=${PANEL_TOKEN}
AGENT_TOKEN=${TOKEN}
SENTINEL_IMAGE=${IMAGE}
LOG_LEVEL=info
TZ=Asia/Shanghai
EOF
    as_root chmod 600 "$envf"
    log "已生成 $envf（请妥善保存 PANEL_TOKEN / AGENT_TOKEN）"
  fi
  if [ ! -f "$dir/config/rules.yaml" ]; then
    as_root cp "$ROOT/config/rules.yaml" "$dir/config/rules.yaml"
    as_root cp "$ROOT/config/collectors.yaml" "$dir/config/collectors.yaml"
  fi

  local db_service=""
  if [ "$WITH_DB" = "1" ] && [ -z "$DSN" ]; then
    db_service='
  postgres:
    image: postgres:16-alpine
    container_name: lan-sentinel-pg
    restart: unless-stopped
    environment:
      POSTGRES_USER: sentinel_app
      POSTGRES_PASSWORD: sentinel
      POSTGRES_DB: sentinel
    volumes:
      - ./pgdata:/var/lib/postgresql/data
    mem_limit: 256m'
  fi

  as_root tee "$dir/docker-compose.yml" >/dev/null <<EOF
services:
  sentinel:
    image: ${IMAGE}
    container_name: lan-sentinel
    restart: unless-stopped
    env_file: [.env]
    environment:
      SENTINEL_LISTEN: ":${PORT}"
      CONFIG_DIR: /config
      DATA_DIR: /data
      TZ: Asia/Shanghai
    ports: ["${PORT}:${PORT}"]
    volumes:
      - ./config:/config:ro
      - ./data:/data${db_service}
EOF

  if [ "$DO_BUILD" = "1" ]; then
    [ -f "$ROOT/Dockerfile" ] || die "当前目录没有 Dockerfile，无法构建；请加 --no-build 使用已有镜像"
    log "构建镜像 $IMAGE ..."
    ( cd "$ROOT" && docker build -t "$IMAGE" . )
  fi
  ( cd "$dir" && as_root docker compose up -d )
  log "控制面已启动：http://127.0.0.1:${PORT}"
  log "配置文件：$envf"
}

install_server_binary() {
  require_root
  [ -n "$DSN" ] || die "二进制模式需要 --dsn（PostgreSQL 连接串）"
  local bin; bin=$(server_bin)
  install -m 0755 "$bin" /usr/local/bin/sentinel
  mkdir -p /etc/lan-sentinel
  PANEL_TOKEN=${PANEL_TOKEN:-$(rand_token)}
  TOKEN=${TOKEN:-$(rand_token)}
  cat > /etc/lan-sentinel/server.env <<EOF
SENTINEL_LISTEN=${LISTEN}
PG_DSN=${DSN}
PANEL_TOKEN=${PANEL_TOKEN}
AGENT_TOKEN=${TOKEN}
LOG_LEVEL=info
TZ=Asia/Shanghai
EOF
  chmod 600 /etc/lan-sentinel/server.env
  cat > /etc/systemd/system/sentinel.service <<'EOF'
[Unit]
Description=LanSentinel Control Plane
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/lan-sentinel/server.env
ExecStart=/usr/local/bin/sentinel serve
Restart=always
RestartSec=3
MemoryMax=192M
Nice=-5

[Install]
WantedBy=multi-user.target
EOF
  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl enable --now sentinel
    systemctl --no-pager --lines=5 status sentinel || true
  else
    die "未检测到 systemd，请手动运行：/usr/local/bin/sentinel serve"
  fi
  log "控制面已启动：http://127.0.0.1${LISTEN}"
  log "配置：/etc/lan-sentinel/server.env（含 PANEL_TOKEN / AGENT_TOKEN）"
}

# ---------- Agent ----------

write_agent_json() {
  local file=$1
  local mounts_json
  mounts_json=$(printf '%s' "$MOUNTS" | awk -F, '{for(i=1;i<=NF;i++){gsub(/^ +| +$/,"",$i); printf "%s\"%s\"", (i>1?",":""), $i}}')
  cat > "$file" <<EOF
{
  "server": "$SERVER",
  "token": "$TOKEN",
  "host_id": "$HOST_ID",
  "interval_s": 15,
  "mounts": [$(printf '%s' "$mounts_json")],
  "data_dir": "$2"
}
EOF
}

install_agent_binary_linux() {
  require_root
  local bin; bin=$(agent_bin)
  install -m 0755 "$bin" /usr/local/bin/sentinel-agent
  mkdir -p /etc/lan-sentinel /var/lib/lan-sentinel
  write_agent_json /etc/lan-sentinel/agent.json /var/lib/lan-sentinel
  chmod 600 /etc/lan-sentinel/agent.json

  if is_openwrt; then
    cat > /etc/init.d/sentinel-agent <<'EOF'
#!/bin/sh /etc/rc.common
START=95
STOP=10
USE_PROCD=1
start_service() {
	procd_open_instance
	procd_set_param command /usr/local/bin/sentinel-agent -config /etc/lan-sentinel/agent.json
	procd_set_param respawn 3600 5 5
	procd_set_param stdout 1
	procd_set_param stderr 1
	procd_close_instance
}
EOF
    chmod 0755 /etc/init.d/sentinel-agent
    /etc/init.d/sentinel-agent stop 2>/dev/null || true
    /etc/init.d/sentinel-agent enable
    /etc/init.d/sentinel-agent start
    log "Agent 已安装（OpenWrt procd）：$HOST_ID -> $SERVER"
    return
  fi

  cat > /etc/systemd/system/sentinel-agent.service <<'EOF'
[Unit]
Description=LanSentinel Agent (metrics + controlled actions)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/sentinel-agent -config /etc/lan-sentinel/agent.json
Restart=always
RestartSec=30
MemoryMax=32M
CPUQuota=2%
Nice=10
IOSchedulingClass=idle

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable sentinel-agent
  systemctl restart sentinel-agent
  log "Agent 已安装：$HOST_ID -> $SERVER"
}

install_agent_binary_darwin() {
  local bin; bin=$(agent_bin)
  if [ "$(id -u)" = "0" ]; then
    sh "$ROOT/agent/install-macos.sh" "$SERVER" "$TOKEN" "$HOST_ID"
  else
    sh "$ROOT/agent/install-macos.sh" --user "$SERVER" "$TOKEN" "$HOST_ID"
  fi
  log "Agent 已安装（launchd）：$HOST_ID -> $SERVER"
}

install_agent_docker() {
  need docker
  [ "$(uname -s)" = "Linux" ] || die "Docker 模式 Agent 仅支持 Linux（需要 pid=host 只读挂载 /proc）"
  local dir=${DIR:-/var/lib/lan-sentinel}
  # 容器内看到的是宿主机根目录挂载点，默认值需换算
  [ "$MOUNTS" = "/" ] && MOUNTS=/host/root
  as_root mkdir -p "$dir/data"
  if [ "$DO_BUILD" = "1" ]; then
    [ -f "$ROOT/Dockerfile.agent" ] || die "当前目录没有 Dockerfile.agent；请加 --no-build 使用已有镜像"
    log "构建镜像 $AGENT_IMAGE ..."
    ( cd "$ROOT" && docker build -f Dockerfile.agent -t "$AGENT_IMAGE" . )
  fi
  as_root docker rm -f lan-sentinel-agent >/dev/null 2>&1 || true
  as_root docker run -d \
    --name lan-sentinel-agent \
    --restart unless-stopped \
    --pid host \
    --network host \
    --memory 32m --cpus 0.05 \
    -e "SENTINEL_SERVER=$SERVER" \
    -e "SENTINEL_TOKEN=$TOKEN" \
    -e "SENTINEL_HOST_ID=$HOST_ID" \
    -v /:/host/root:ro \
    -v /sys:/sys:ro \
    -v "$dir/data:/data" \
    "$AGENT_IMAGE" -mounts "$MOUNTS" -data-dir /data
  log "Agent 容器已启动：$HOST_ID -> $SERVER"
}

# ---------- 主流程 ----------

install_server() {
  case "$MODE" in
    docker) install_server_docker ;;
    binary) install_server_binary ;;
  esac
}

install_agent() {
  case "$MODE" in
    docker) install_agent_docker ;;
    binary)
      case "$(uname -s)" in
        Linux) install_agent_binary_linux ;;
        Darwin) install_agent_binary_darwin ;;
        *) die "暂不支持 $(uname -s)，请参考 README 手动安装" ;;
      esac ;;
  esac
}

case "$COMPONENT" in
  server) install_server ;;
  agent)
    [ -n "$SERVER" ] || die "安装 Agent 需要 --server http://控制面:8110"
    install_agent ;;
  all)
    install_server
    [ -n "$SERVER" ] || SERVER="http://127.0.0.1:${PORT}"
    [ -n "$TOKEN" ] || TOKEN=${SENTINEL_AGENT_TOKEN:-}
    install_agent ;;
esac

log "完成。面板默认为 http://<主机>:${PORT}，首次配置见 README 的「采集策略」。"
