#!/bin/sh
# 在 Linux 被监控机上安装 sentinel-agent（systemd）
# 用法：sudo ./agent/install.sh [control-plane-url] [agent-token]
set -e

SRC=$(dirname "$0")/../bin/sentinel-agent
[ -x "$SRC" ] || SRC=$(dirname "$0")/sentinel-agent
DEST=/usr/local/bin/sentinel-agent
CONF_DIR=/etc/lan-sentinel
DATA_DIR=/var/lib/lan-sentinel

SERVER=${1:-http://192.168.1.102:8110}
TOKEN=${2:-}
HOST_ID=$(hostname)

install -m 0755 "$SRC" "$DEST"
mkdir -p "$CONF_DIR" "$DATA_DIR"
chmod 0755 "$DATA_DIR"

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
chmod 0640 "$CONF_DIR/agent.json"

cat > /etc/systemd/system/sentinel-agent.service <<'EOF'
[Unit]
Description=LanSentinel Agent (metrics + controlled actions)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/sentinel-agent -config /etc/lan-sentinel/agent.json
Restart=always
RestartSec=5
# 弱机保护：内存与 CPU 上限，进程本身不监听端口
MemoryMax=32M
CPUQuota=2%
Nice=10
IOSchedulingClass=idle

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now sentinel-agent

sleep 2
systemctl --no-pager --lines=5 status sentinel-agent || true
echo "installed: sentinel-agent -> $SERVER (host_id=$HOST_ID)"
