#!/bin/sh
# 在 macOS 上安装 sentinel-agent
# 用法：sudo sh ./agent/install-macos.sh [control-plane-url] [agent-token] [host-id]
#      sh ./agent/install-macos.sh --user [control-plane-url] [agent-token] [host-id]
#      --user 模式安装到当前用户的 LaunchAgents，不需要 sudo，但只在用户登录时运行
set -e

USER_MODE=0
if [ "$1" = "--user" ]; then
	USER_MODE=1
	shift
fi

DIR=$(dirname "$0")
ARCH=$(uname -m)
case "$ARCH" in
	arm64) BIN=sentinel-agent-darwin-arm64 ;;
	*)     BIN=sentinel-agent-darwin-amd64 ;;
esac
SRC=$DIR/../bin/$BIN
[ -x "$SRC" ] || SRC=$DIR/$BIN
[ -x "$SRC" ] || SRC=$DIR/sentinel-agent
[ -x "$SRC" ] || { echo "missing $BIN binary: $SRC" >&2; exit 1; }

SERVER=${1:-http://192.168.1.102:8110}
TOKEN=${2:-}
HOST_ID=${3:-$(scutil --get LocalHostName 2>/dev/null || hostname)}
LABEL=com.lansentinel.agent

if [ "$USER_MODE" = "1" ]; then
	DEST="$HOME/.local/bin/sentinel-agent"
	CONF_DIR="$HOME/.lan-sentinel"
	DATA_DIR="$HOME/Library/Application Support/LanSentinel"
	LOG_DIR="$HOME/Library/Logs/LanSentinel"
	PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
	DOMAIN="gui/$(id -u)"
else
	[ "$(id -u)" = "0" ] || { echo "run as root (sudo), or use --user" >&2; exit 1; }
	DEST=/usr/local/bin/sentinel-agent
	CONF_DIR=/etc/lan-sentinel
	DATA_DIR="/Library/Application Support/LanSentinel"
	LOG_DIR=/Library/Logs/LanSentinel
	PLIST=/Library/LaunchDaemons/$LABEL.plist
	DOMAIN="system"
fi

mkdir -p "$(dirname "$DEST")" "$CONF_DIR" "$DATA_DIR" "$LOG_DIR" "$(dirname "$PLIST")"
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
[ "$USER_MODE" = "1" ] || chown root:wheel "$CONF_DIR/agent.json"

cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$DEST</string>
		<string>-config</string>
		<string>$CONF_DIR/agent.json</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ProcessType</key><string>Background</string>
	<key>LowPriorityIO</key><true/>
	<key>Nice</key><integer>10</integer>
	<key>StandardOutPath</key><string>$LOG_DIR/agent.log</string>
	<key>StandardErrorPath</key><string>$LOG_DIR/agent.err.log</string>
</dict>
</plist>
EOF
chmod 0644 "$PLIST"
[ "$USER_MODE" = "1" ] || chown root:wheel "$PLIST"
plutil -lint "$PLIST"

launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
launchctl bootstrap "$DOMAIN" "$PLIST"
launchctl enable "$DOMAIN/$LABEL"
launchctl kickstart -k "$DOMAIN/$LABEL"

sleep 2
launchctl print "$DOMAIN/$LABEL" | grep -E 'state|pid' | head -4 || true
echo "installed: sentinel-agent -> $SERVER (host_id=$HOST_ID, domain=$DOMAIN)"
