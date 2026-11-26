#!/bin/sh
# 在 OpenWrt（procd）上安装 sentinel-agent
# 用法：sudo sh ./agent/install-openwrt.sh [control-plane-url] [agent-token] [host-id]
set -e

DIR=$(dirname "$0")
SRC=$DIR/../bin/sentinel-agent-linux-arm64
[ -x "$SRC" ] || SRC=$DIR/sentinel-agent-linux-arm64
[ -x "$SRC" ] || SRC=$DIR/sentinel-agent
[ -x "$SRC" ] || { echo "missing arm64 agent binary: $SRC" >&2; exit 1; }

SERVER=${1:-http://192.168.1.102:8110}
TOKEN=${2:-}
HOST_ID=${3:-$(cat /proc/sys/kernel/hostname)}
DEST=/usr/bin/sentinel-agent
CONF_DIR=/etc/lan-sentinel
DATA_DIR=/root/lan-sentinel

[ "$(id -u)" = "0" ] || { echo "run as root" >&2; exit 1; }

mkdir -p "$CONF_DIR" "$DATA_DIR"
cp "$SRC" "$DEST"
chmod 0755 "$DEST"

cat > "$CONF_DIR/agent.json" <<EOF
{
  "server": "$SERVER",
  "token": "$TOKEN",
  "host_id": "$HOST_ID",
  "interval_s": 15,
  "mounts": ["/"],
  "data_dir": "$DATA_DIR"
}
EOF
chmod 0600 "$CONF_DIR/agent.json"

cat > /etc/init.d/sentinel-agent <<'EOF'
#!/bin/sh /etc/rc.common

START=95
STOP=10
USE_PROCD=1

start_service() {
	procd_open_instance
	procd_set_param command /usr/bin/sentinel-agent -config /etc/lan-sentinel/agent.json
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

sleep 2
/etc/init.d/sentinel-agent status || true
echo "installed: sentinel-agent -> $SERVER (host_id=$HOST_ID)"
