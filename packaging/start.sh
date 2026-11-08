#!/usr/bin/env bash
# ChinaCPA · Linux / macOS 启动脚本
# 用法：./start.sh            前台运行（Ctrl+C 退出）
#       ./start.sh -d         后台运行，日志写到 chinacpa.log
set -euo pipefail
cd "$(dirname "$0")"

BIN=./chinacpa
[ -x "$BIN" ] || chmod +x "$BIN" 2>/dev/null || true

if [ ! -f config.json ]; then
  echo "· 未找到 config.json，将使用内置默认配置（面板里改完会自动写回）"
fi

# listen 可能是 ":8313" 或 "127.0.0.1:8313"，端口取最后一个冒号后面；
# 不能 grep 所有数字再取第一个，否则会把 127.0.0.1 的 127 当成端口。
LISTEN=$(grep -o '"listen"[[:space:]]*:[[:space:]]*"[^"]*"' config.json 2>/dev/null | sed 's/.*"\([^"]*\)"$/\1/' | head -1)
PORT=${LISTEN##*:}
PORT=${PORT:-8313}
PASS=$(grep -o '"panel_password"[[:space:]]*:[[:space:]]*"[^"]*"' config.json 2>/dev/null | sed 's/.*:[[:space:]]*"//;s/"$//')
echo "· 面板地址 http://127.0.0.1:${PORT}/    面板密码 ${PASS:-123456}（在 config.json 的 panel_password 修改）"
echo "· 数据目录 ./data   首次启动会自动创建"
echo

if [ "${1:-}" = "-d" ]; then
  nohup "$BIN" -config config.json >> chinacpa.log 2>&1 &
  echo "· 已在后台启动，PID $!，日志：$(pwd)/chinacpa.log"
  echo "· 停止：kill $!"
  exit 0
fi

exec "$BIN" -config config.json
