#!/usr/bin/env bash
# 一条命令启动：电影院会员券跨场次改签差额系统
# 用法：
#   ./run.sh            启动（Go 缺失时自动安装到 ~/.local/go）
#   ./run.sh --reset   清空本地数据库后重新灌入演示数据
#   ./run.sh test       运行全部验证测试（并发抢座 / 单张改签 / 补款失败 / 幂等回执 / 超时）
set -euo pipefail
cd "$(dirname "$0")/backend"

# ---- 1. 确保 Go 可用 ----
if ! command -v go >/dev/null 2>&1; then
  if [ -x "$HOME/.local/go/bin/go" ]; then
    export PATH="$HOME/.local/go/bin:$PATH"
  else
    echo ">> 未检测到 Go，正在安装到 ~/.local/go …"
    arch="$(uname -m)"; case "$arch" in x86_64) goarch=amd64;; aarch64|arm64) goarch=arm64;; *) echo "不支持的架构: $arch"; exit 1;; esac
    tmp="$(mktemp -d)"
    curl -fsSL -o "$tmp/go.tgz" "https://go.dev/dl/go1.23.2.linux-${goarch}.tar.gz" || \
    curl -fsSL -o "$tmp/go.tgz" "https://golang.google.cn/dl/go1.23.2.linux-${goarch}.tar.gz"
    mkdir -p "$HOME/.local"
    tar -C "$HOME/.local" -xzf "$tmp/go.tgz"
    export PATH="$HOME/.local/go/bin:$PATH"
  fi
fi
export GOPROXY="${GOPROXY:-https://goproxy.cn,https://proxy.golang.org,direct}"
export CGO_ENABLED=0

MODE="run"
case "${1:-}" in
  --reset) rm -rf data; shift; MODE="run" ;;
  test)    MODE="test"; shift ;;
esac

if [ "$MODE" = "test" ]; then
  if command -v gcc >/dev/null 2>&1; then
    exec env CGO_ENABLED=1 go test -race -count=1 ./...
  else
    exec go test -count=1 ./...
  fi
fi

# ---- 2. 构建（前端已通过 go:embed 打包进二进制，无需 Node/npm）----
mkdir -p bin
CGO_ENABLED=0 go build -o bin/cinema .

# ---- 3. 启动：HTTP + 本地模拟支付回执接口 ----
echo ">> 启动成功：浏览器打开 http://localhost:8080"
exec ./bin/cinema --addr "${ADDR:-:8080}" --db "${DB:-data/cinema.db}" --ttl "${TTL:-120}" "$@"
