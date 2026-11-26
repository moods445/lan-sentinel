#!/usr/bin/env bash
# LanSentinel 一键安装：控制面 / Agent，二进制（GitHub Release）或 Docker（GHCR）
#
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/moods445/lan-sentinel/master/install.sh | sudo bash -s -- all --mode docker
#   ./install.sh server --mode docker   [--version v0.2.1] [--dir DIR] [--port 8110] [--dsn DSN] [--no-db] [--build]
#   ./install.sh server --mode binary   [--version v0.2.1] [--dsn DSN] [--listen :8110]
#   ./install.sh agent  --mode binary   [--server URL] [--token T] [--host-id ID] [--mounts /,/data]
#   ./install.sh agent  --mode docker   [--server URL] [--token T] [--host-id ID] [--dir DIR]
#   ./install.sh all    --mode docker|binary [--server URL] [--token T]
#
# 也可用环境变量：SENTINEL_MODE / SENTINEL_SERVER / SENTINEL_TOKEN / SENTINEL_HOST_ID
#                SENTINEL_VERSION / SENTINEL_DIR / SENTINEL_DSN / SENTINEL_PANEL_TOKEN
#                SENTINEL_REPO（默认 moods445/lan-sentinel）/ SENTINEL_GHCR
#
# 说明：
#   - 默认从 GitHub Release 下载二进制，从 GHCR 拉取镜像；
#   - 当前目录是源码仓库时，也可用 --build 本地构建；
#   - server 的 docker 模式默认带本地 PostgreSQL（--no-db 可关闭）；
#   - Linux 二进制装 systemd（OpenWrt 自动用 procd），macOS 走 launchd；
#   - Docker 模式仅支持 Linux（Agent 需要 pid=host 只读挂载 /proc）。
set -euo pipefail

SELF=${BASH_SOURCE[0]:-$0}
ROOT=$(cd "$(dirname "$SELF")" 2>/dev/null && pwd || pwd)
HAS_SRC=0
[ -f "$ROOT/go.mod" ] && [ -d "$ROOT/cmd/sentinel" ] && HAS_SRC=1

REPO=${SENTINEL_REPO:-moods445/lan-sentinel}
VERSION=${SENTINEL_VERSION:-latest}
VERSION=${VERSION#v}
GHCR=${SENTINEL_GHCR:-ghcr.io/$REPO}
if [ "$VERSION" = "latest" ]; then TAG=latest; else TAG=v$VERSION; fi

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
DO_BUILD=0
IMAGE=${SENTINEL_IMAGE:-$GHCR:$TAG}
AGENT_IMAGE=${SENTINEL_AGENT_IMAGE:-$GHCR-agent:$TAG}

WORK=$(mktemp -d 2>/dev/null || mktemp -d -t lansentinel)
trap 'rm -rf "$WORK"' EXIT

log()  { printf '\033[36m[install]\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

usage() {
  cat <<'USAGE'
LanSentinel 一键安装：控制面 / Agent，二进制（GitHub Release）或 Docker（GHCR）

用法：
  ./install.sh server --mode docker   [--version v0.2.1] [--dir DIR] [--port 8110] [--dsn DSN] [--no-db] [--build]
  ./install.sh server --mode binary   [--version v0.2.1] [--dsn DSN] [--listen :8110]
  ./install.sh agent  --mode binary   [--server URL] [--token T] [--host-id ID] [--mounts /,/data]
  ./install.sh agent  --mode docker   [--server URL] [--token T] [--host-id ID] [--dir DIR]
  ./install.sh all    --mode docker|binary [--server URL] [--token T]

远程一行安装：
  curl -fsSL https://raw.githubusercontent.com/moods445/lan-sentinel/master/install.sh | sudo bash -s -- all --mode docker

环境变量：
  SENTINEL_MODE / SENTINEL_SERVER / SENTINEL_TOKEN / SENTINEL_HOST_ID
  SENTINEL_VERSION / SENTINEL_DIR / SENTINEL_DSN / SENTINEL_PANEL_TOKEN
  SENTINEL_REPO / SENTINEL_GHCR / SENTINEL_IMAGE / SENTINEL_AGENT_IMAGE
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
    --version) VERSION=${2:-}; VERSION=${VERSION#v}
      if [ "$VERSION" = "latest" ]; then TAG=latest; else TAG=v$VERSION; fi
      IMAGE=${SENTINEL_IMAGE:-$GHCR:$TAG}; AGENT_IMAGE=${SENTINEL_AGENT_IMAGE:-$GHCR-agent:$TAG} ;;
    --build) DO_BUILD=1 ;;
    --no-build) DO_BUILD=0 ;;
    --no-db) WITH_DB=0 ;;
    -h|--help) usage 0 ;;
    *) die "未知参数：$1（用 --help 查看用法）" ;;
  esac
  shift
done

[ -n "$COMPONENT" ] || usage 1
[ -n "$MODE" ] || die "请用 --mode docker 或 --mode binary 指定安装方式"
case "$MODE" in docker|binary) ;; *) die "--mode 只能是 docker 或 binary" ;; esac

# ---------- 通用工具 ----------

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

fetch() {
  local url=$1 out=$2
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 --connect-timeout 10 -o "$out" "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$out" "$url"
  else
    die "需要 curl 或 wget 下载 $url"
  fi
}

release_url() {
  local name=$1
  if [ "$VERSION" = "latest" ]; then
    printf 'https://github.com/%s/releases/latest/download/%s' "$REPO" "$name"
  else
    printf 'https://github.com/%s/releases/download/v%s/%s' "$REPO" "$VERSION" "$name"
  fi
}

raw_url() {
  printf 'https://raw.githubusercontent.com/%s/HEAD/%s' "$REPO" "$1"
}

CHECKSUMS=0
verify_sha() {
  local file=$1 name want got
  name=$(basename "$file")
  command -v sha256sum >/dev/null 2>&1 || return 0
  if [ "$CHECKSUMS" = "0" ]; then
    CHECKSUMS=1
    fetch "$(release_url checksums.txt)" "$WORK/checksums.txt" 2>/dev/null || return 0
  fi
  [ -f "$WORK/checksums.txt" ] || return 0
  want=$(awk -v n="$name" '$2 == n { print $1; exit }' "$WORK/checksums.txt")
  [ -n "$want" ] || return 0
  got=$(sha256sum "$file" | awk '{print $1}')
  [ "$got" = "$want" ] || die "sha256 校验失败：$name"
  log "校验通过：$name"
}

download() {
  local name=$1 dest=$2
  log "下载 $name ..."
  fetch "$(release_url "$name")" "$dest"
  chmod 0755 "$dest"
  verify_sha "$dest"
  printf '%s' "$dest"
}

OS=$(uname -s | tr 'A-Z' 'a-z')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv7) ARCH=armv7 ;;
  i386|i686) ARCH=386 ;;
esac
BUILD_VER=$VERSION
[ "$BUILD_VER" = "latest" ] && BUILD_VER=dev

build_go() {
  local out=$1 pkg=$2 goos=$3 goarch=$4 goarm=${5:-}
  command -v go >/dev/null 2>&1 || return 1
  log "从源码构建 $(basename "$out") ..."
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOARM="$goarm" \
      go build -trimpath -ldflags="-s -w -X main.version=$BUILD_VER -X github.com/home-aio/lan-sentinel/internal/agent.agentVersion=$BUILD_VER" \
      -o "$out" "$pkg" )
}

server_bin() {
  local name="sentinel-linux-$ARCH" out=$WORK/$name
  case "$ARCH" in amd64|arm64) ;; *) die "控制面二进制暂只支持 linux amd64/arm64，当前：$ARCH" ;; esac
  if [ -x "$ROOT/bin/$name" ]; then printf '%s' "$ROOT/bin/$name"; return; fi
  if [ "$HAS_SRC" = "1" ] && build_go "$out" ./cmd/sentinel linux "$ARCH"; then printf '%s' "$out"; return; fi
  download "$name" "$out"
}

agent_bin() {
  local name suffix="" out
  case "$OS" in
    linux)
      case "$ARCH" in amd64|arm64|armv7|386) ;; *) die "不支持的架构：$ARCH" ;; esac ;;
    darwin)
      case "$ARCH" in amd64|arm64) ;; *) die "不支持的架构：$ARCH" ;; esac ;;
    *) die "暂不支持在 $OS 上自动安装，请参考 README 手动安装" ;;
  esac
  suffix="-$OS-$ARCH"
  [ "$OS" = windows ] && suffix="$suffix.exe"
  name="sentinel-agent$suffix"
  out=$WORK/$name
  if [ -x "$ROOT/bin/$name" ]; then printf '%s' "$ROOT/bin/$name"; return; fi
  if [ "$HAS_SRC" = "1" ] && build_go "$out" ./cmd/sentinel-agent "$OS" "$ARCH"; then printf '%s' "$out"; return; fi
  download "$name" "$out"
}

copy_config() {
  local dir=$1
  as_root mkdir -p "$dir"
  for f in rules.yaml collectors.yaml; do
    [ -f "$dir/$f" ] && continue
    if [ "$HAS_SRC" = "1" ] && [ -f "$ROOT/config/$f" ]; then
      as_root cp "$ROOT/config/$f" "$dir/$f"
    else
      fetch "$(raw_url "config/$f")" "$WORK/$f" && as_root cp "$WORK/$f" "$dir/$f"
    fi
  done
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
  copy_config "$dir/config"

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
    [ "$HAS_SRC" = "1" ] || die "--build 需要在源码仓库目录执行"
    [ -f "$ROOT/bin/sentinel" ] || build_go "$ROOT/bin/sentinel" ./cmd/sentinel linux "$(go env GOARCH 2>/dev/null || echo amd64)" \
      || die "无法构建 bin/sentinel"
    log "构建镜像 $IMAGE ..."
    ( cd "$ROOT" && docker build -t "$IMAGE" . )
  else
    log "使用镜像 $IMAGE（GHCR）"
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
  mkdir -p /etc/lan-sentinel/config /var/lib/lan-sentinel
  copy_config /etc/lan-sentinel/config
  PANEL_TOKEN=${PANEL_TOKEN:-$(rand_token)}
  TOKEN=${TOKEN:-$(rand_token)}
  cat > /etc/lan-sentinel/server.env <<EOF
SENTINEL_LISTEN=${LISTEN}
PG_DSN=${DSN}
PANEL_TOKEN=${PANEL_TOKEN}
AGENT_TOKEN=${TOKEN}
CONFIG_DIR=/etc/lan-sentinel/config
DATA_DIR=/var/lib/lan-sentinel
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
    systemctl enable sentinel
    systemctl restart sentinel
    systemctl --no-pager --lines=5 status sentinel || true
  else
    die "未检测到 systemd，请手动运行：/usr/local/bin/sentinel serve"
  fi
  log "控制面已启动：http://127.0.0.1${LISTEN}"
  log "配置：/etc/lan-sentinel/server.env（含 PANEL_TOKEN / AGENT_TOKEN）"
}

# ---------- Agent ----------

write_agent_json() {
  local file=$1 data_dir=$2 mounts_json
  mounts_json=$(printf '%s' "$MOUNTS" | awk -F, '{for(i=1;i<=NF;i++){gsub(/^ +| +$/,"",$i); printf "%s\"%s\"", (i>1?",":""), $i}}')
  cat > "$file" <<EOF
{
  "server": "$SERVER",
  "token": "$TOKEN",
  "host_id": "$HOST_ID",
  "interval_s": 15,
  "mounts": [$(printf '%s' "$mounts_json")],
  "data_dir": "$data_dir"
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
  local bin dest conf_dir data_dir log_dir label domain
  bin=$(agent_bin)
  label=com.lansentinel.agent
  if [ "$(id -u)" = "0" ]; then
    dest=/usr/local/bin/sentinel-agent
    conf_dir=/etc/lan-sentinel
    data_dir="/Library/Application Support/LanSentinel"
    log_dir=/Library/Logs/LanSentinel
    plist=/Library/LaunchDaemons/$label.plist
    domain=system
  else
    dest="$HOME/.local/bin/sentinel-agent"
    conf_dir="$HOME/.lan-sentinel"
    data_dir="$HOME/Library/Application Support/LanSentinel"
    log_dir="$HOME/Library/Logs/LanSentinel"
    plist="$HOME/Library/LaunchAgents/$label.plist"
    domain="gui/$(id -u)"
  fi
  mkdir -p "$(dirname "$dest")" "$conf_dir" "$data_dir" "$log_dir" "$(dirname "$plist")"
  install -m 0755 "$bin" "$dest"
  write_agent_json "$conf_dir/agent.json" "$data_dir"
  chmod 600 "$conf_dir/agent.json"
  cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>$label</string>
	<key>ProgramArguments</key>
	<array><string>$dest</string><string>-config</string><string>$conf_dir/agent.json</string></array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>Nice</key><integer>10</integer>
	<key>StandardOutPath</key><string>$log_dir/agent.log</string>
	<key>StandardErrorPath</key><string>$log_dir/agent.err.log</string>
</dict>
</plist>
EOF
  chmod 0644 "$plist"
  launchctl bootout "$domain/$label" 2>/dev/null || true
  launchctl bootstrap "$domain" "$plist"
  launchctl enable "$domain/$label"
  launchctl kickstart -k "$domain/$label"
  log "Agent 已安装（launchd）：$HOST_ID -> $SERVER"
}

install_agent_docker() {
  need docker
  [ "$(uname -s)" = "Linux" ] || die "Docker 模式 Agent 仅支持 Linux（需要 pid=host 只读挂载 /proc）"
  local dir=${DIR:-/var/lib/lan-sentinel}
  [ "$MOUNTS" = "/" ] && MOUNTS=/host/root
  as_root mkdir -p "$dir/data"
  if [ "$DO_BUILD" = "1" ]; then
    [ "$HAS_SRC" = "1" ] || die "--build 需要在源码仓库目录执行"
    [ -f "$ROOT/bin/sentinel-agent" ] || build_go "$ROOT/bin/sentinel-agent" ./cmd/sentinel-agent linux "$(go env GOARCH 2>/dev/null || echo amd64)" \
      || die "无法构建 bin/sentinel-agent"
    log "构建镜像 $AGENT_IMAGE ..."
    ( cd "$ROOT" && docker build -f Dockerfile.agent -t "$AGENT_IMAGE" . )
  else
    log "使用镜像 $AGENT_IMAGE（GHCR）"
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
      case "$OS" in
        linux) install_agent_binary_linux ;;
        darwin) install_agent_binary_darwin ;;
        *) die "暂不支持 $OS，请参考 README 手动安装" ;;
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
    install_agent ;;
esac

log "完成。面板默认 http://<主机>:${PORT}；版本 ${TAG}（${REPO}）"
