#!/usr/bin/env bash
# 一条命令启动：影院会员券跨场次改签差额系统
# 用法: ./run.sh            （默认 :8080，自动重置演示数据）
#       PORT=9090 ./run.sh
#       ./run.sh --no-reset 保留数据库
set -euo pipefail
cd "$(dirname "$0")"

# 选择 go：优先 PATH，其次 ~/sdk/go
if ! command -v go >/dev/null 2>&1; then
  if [ -x "$HOME/sdk/go/bin/go" ]; then
    export PATH="$HOME/sdk/go/bin:$PATH"
  else
    echo "未找到 Go，请安装 Go 1.24+" >&2
    exit 1
  fi
fi
export GOTOOLCHAIN=local
export GOSUMDB="${GOSUMDB:-off}"

PORT="${PORT:-8080}"
TTL="${TTL:-60s}"
RESET_FLAG="-reset"
if [ "${1:-}" = "--no-reset" ]; then RESET_FLAG=""; fi

mkdir -p data
echo "▶ 编译并启动 http://localhost:${PORT} （TTL=${TTL}）"
exec go run . -addr ":${PORT}" -db data/cinema.db -ttl "$TTL" $RESET_FLAG
