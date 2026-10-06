#!/usr/bin/env bash
# 构建桌面版：前端 → 网关 → 桌面壳 → 打包并同步到 dist/。
#
# 为什么要有这个脚本：桌面壳的构建产物落在 `.toolchain/` 下，而用户实际拿的是
# `dist/` 里的安装包，中间**没有自动同步**。分成几步手动跑时极易漏掉最后一步 ——
# 现象是「改了代码、构建也成功、装上去却一点没变」，很容易误判成修复无效。
# 真实踩过一次：dist 里的安装包比源码晚了 17 分钟，白装了两遍。
#
# 用法：
#   bash scripts/build-desktop.sh
#
# 只改网页时用 `bash scripts/build.sh` 就够了（快得多）；本脚本面向「要出安装包」。
set -euo pipefail

# 依赖代理：切片 15 起引入了 modernc.org/sqlite（纯 Go 的 SQLite，用于读写
# WorkBuddy 客户端的会话库）。Go 的**默认代理 proxy.golang.org 在国内常不可达**，
# 表现是 `go: downloading ... dial tcp ... connection attempt failed` ——
# 看着像代码问题，实际只是网络。这里给一个可用默认值，仍可用环境变量覆盖。
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# ---- 定位 Node（与 build.sh 同口径）----
NODE_BIN=""
# `${USERNAME:-}` 的默认值不能省：USERNAME 只有 Git Bash 会设，而本脚本开了 set -u，
# Linux 上直接写 $USERNAME 会以 "unbound variable" 中断。
for cand in \
  "C:/Users/${USERNAME:-}/.workbuddy/binaries/node/versions/22.22.2-3" \
  "$(dirname "$(command -v node 2>/dev/null || true)")"; do
  if [ -n "$cand" ] && { [ -x "$cand/node.exe" ] || [ -x "$cand/node" ]; }; then
    NODE_BIN="$cand"
    break
  fi
done
if [ -z "$NODE_BIN" ]; then
  echo "!! 未找到 Node，无法构建桌面版" >&2
  exit 1
fi

# ---- 定位 Python（打包脚本只用标准库）----
PY_BIN="$(command -v python || command -v python3 || true)"
if [ -z "$PY_BIN" ]; then
  echo "!! 未找到 Python，无法打包" >&2
  exit 1
fi

echo "==> 1/5 构建前端与网关"
bash scripts/build.sh

# ---- 先确保 WebView2Loader.dll 就位 ----
#
# 这个顺序不能反。DLL 由 webview2-com-sys 的 build script 在 **cargo 编译时**解出来，
# 而它又被 tauri.conf.json 的 bundle.resources 声明为必需资源 —— 也就是必须在
# `tauri build` 开始**之前**就躺在 desktop/src-tauri/ 下。
# 全新 clone 上两边都不存在，直接跑 prepare-resources 必然失败（先有鸡还是先有蛋）。
# 所以这里在缺 DLL 时先编译一次，让它把 DLL 解出来。
#
# 有 .toolchain/cargo-target 时不用编译（上一轮构建留下的 DLL 就够用）。
if [ ! -f desktop/src-tauri/WebView2Loader.dll ]; then
  echo "==> 2/5 首次构建：编译一次以解出 WebView2Loader.dll"
  (
    cd desktop
    # shellcheck disable=SC1091
    source scripts/env.sh
    # shellcheck disable=SC1091
    ( cd src-tauri && cargo build --quiet ) || true
  )
  if [ ! -f desktop/src-tauri/WebView2Loader.dll ]; then
    echo "!! WebView2Loader.dll 仍未就位。请确认 Rust 工具链已装好：" >&2
    echo "   bash desktop/scripts/setup-rust-toolchain.sh" >&2
    echo "   （构建产物里会有一份，prepare-resources 也能自动找到）" >&2
    exit 1
  fi
else
  echo "==> 2/5 WebView2Loader.dll 已就位，跳过预编译"
fi

echo "==> 3/5 准备桌面资源（网关二进制）"
PATH="$NODE_BIN:$PATH" node desktop/scripts/prepare-resources.mjs

echo "==> 4/5 构建桌面壳并生成安装包"
(
  cd desktop
  # shellcheck disable=SC1091
  source scripts/env.sh
  PATH="$NODE_BIN:$PATH" node ./node_modules/@tauri-apps/cli/tauri.js build
)

echo "==> 5/5 打包并同步到 dist/"
"$PY_BIN" scripts/package-desktop.py

echo
echo "完成。dist/ 产物："
ls -la --time-style=+%H:%M:%S \
  dist/workbuddy-gateway-desktop_0.9.0_x64-setup.exe \
  dist/workbuddy-gateway-desktop_0.9.0_x64-portable.zip
