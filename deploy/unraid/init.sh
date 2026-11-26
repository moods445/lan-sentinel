#!/bin/sh
# LanSentinel · Unraid 启动脚本（库与角色需先按 README 手工创建一次）
# 用法：ssh unraid 'sh /mnt/user/appdata/lan-sentinel/init.sh'
set -e

DIR=/mnt/user/appdata/lan-sentinel
[ -f "$DIR/.env" ] || { echo "缺少 $DIR/.env（参考 .env.example 生成）"; exit 1; }

mkdir -p "$DIR/config" "$DIR/data" "$DIR/agent/data"
# agent 容器以 nonroot 运行，spool 目录需要可写
chmod 777 "$DIR/agent/data"

if ! docker exec postgresql16 psql -U root -d postgres -tAc "select 1 from pg_database where datname='sentinel'" | grep -q 1; then
  echo "库 sentinel 不存在：请先按 README『Unraid 部署』里的 6 条 SQL 建库建角色（\\password 交互设密码）"
  exit 1
fi

cd "$DIR"
docker compose up -d --force-recreate

for i in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:8110/healthz >/dev/null 2>&1; then
    echo "healthy"
    break
  fi
  sleep 1
done

docker compose ps
curl -s http://127.0.0.1:8110/api/v1/hosts | head -c 400 || true
echo
