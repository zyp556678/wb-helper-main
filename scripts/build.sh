#!/usr/bin/env bash
# 构建脚本：先构建前端产物，再编译 Go 二进制（前端产物会被 go:embed 打进二进制）。
#
# 用法：
#   ./scripts/build.sh          # 完整构建（前端 + 后端）
#   ./scripts/build.sh go       # 只构建后端（前端产物已存在时用）
#
# 工具链：优先使用仓库内的便携 Go（.toolchain/go），没有则回退系统 go。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# ---- 定位 Go ----
if [ -x "$ROOT/.toolchain/go/bin/go.exe" ]; then
  GO_BIN="$ROOT/.toolchain/go/bin/go.exe"
  export GOROOT="$(cygpath -w "$ROOT/.toolchain/go" 2>/dev/null || echo "$ROOT/.toolchain/go")"
elif [ -x "$ROOT/.toolchain/go/bin/go" ]; then
  GO_BIN="$ROOT/.toolchain/go/bin/go"
  export GOROOT="$ROOT/.toolchain/go"
else
  GO_BIN="go"
fi
export GOPATH="$ROOT/.toolchain/gopath"
export GOCACHE="$ROOT/.toolchain/gocache"

# 工具链**不在仓库里**（.toolchain/ 有 7.5G，且含 >100MB 的文件，传不上 GitHub）。
# 全新 clone 必然走到这里，所以给一条能照着做的提示，而不是让 `go: command not found`
# 直接糊在脸上 —— 那句话不说明「要装什么」也不说明「装到哪」。
if ! "$GO_BIN" version >/dev/null 2>&1; then
  echo "!! 未找到可用的 Go（当前取的是：$GO_BIN）" >&2
  echo "   本项目不自带 Go 工具链，需要先装 Go 1.27+：https://go.dev/dl/" >&2
  echo "   或者把便携版放到 .toolchain/go/（本脚本会优先用它）。" >&2
  exit 1
fi

# ---- 定位 Node ----
NODE_BIN="${NODE_BIN:-}"
if [ -z "$NODE_BIN" ]; then
  # 第一个候选是 Windows（Git Bash）下的便携 Node。`${USERNAME:-}` 的默认值不能省：
  # USERNAME 只有 Git Bash 会设，而本脚本开了 set -u，Linux 上直接写 $USERNAME 会以
  # "unbound variable" 中断 —— CI 的 npm 作业就是这么挂的。
  for cand in \
    "C:/Users/${USERNAME:-}/.workbuddy/binaries/node/versions/22.22.2-3" \
    "$(dirname "$(command -v node 2>/dev/null || true)")" ; do
    if [ -n "$cand" ] && [ -x "$cand/node.exe" ]; then NODE_BIN="$cand"; break; fi
    if [ -n "$cand" ] && [ -x "$cand/node" ]; then NODE_BIN="$cand"; break; fi
  done
fi

TARGET="${1:-all}"

if [ "$TARGET" = "all" ]; then
  if [ -n "$NODE_BIN" ]; then
    echo "==> 构建前端（$NODE_BIN）"
    ( cd web && PATH="$NODE_BIN:$PATH" "$NODE_BIN/npm" install --no-audit --no-fund && PATH="$NODE_BIN:$PATH" "$NODE_BIN/npm" run build )
  else
    echo "!! 未找到 Node，跳过前端构建；将使用 web/dist 下已有的产物"
  fi
fi

echo "==> 编译 Go 二进制"
"$GO_BIN" vet ./...
"$GO_BIN" build -trimpath -ldflags="-s -w" -o workbuddy-gateway.exe .

# 本机代理是可选的第二个产物：随发行包一起交付（同目录），
# 服务端部署时不需要它——网关找不到时静默跳过，本机相关页面自动隐藏。
if [ "$TARGET" = "all" ]; then
  echo "==> 编译本机代理"
  "$GO_BIN" build -trimpath -ldflags="-s -w" -o wb-local-agent.exe ./cmd/wb-local-agent
fi

echo "==> 完成: $ROOT/workbuddy-gateway.exe"
ls -lh workbuddy-gateway.exe wb-local-agent.exe 2>/dev/null || ls -lh workbuddy-gateway.exe

echo ""
echo "提示：需要跨平台发行包（Linux/macOS/Windows）时用 ./scripts/release.sh，"
echo "      它会交叉编译并把两个二进制、配置样例、部署脚本一起打进 dist/ 下的压缩包。"
