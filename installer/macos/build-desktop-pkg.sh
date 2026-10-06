#!/usr/bin/env bash
# 构建 macOS 桌面版 .dmg（带原生窗口 + 托盘的 Tauri 壳）。
#
# 必须在 macOS 上执行：dmg 打包要调 hdiutil（即便未签名也走这套流程），
# Linux / Windows 上无法交叉产出 dmg。
#
# 用法：
#   ./installer/macos/build-desktop-pkg.sh              # 当前架构
#   ARCH=amd64 ./installer/macos/build-desktop-pkg.sh   # 指定架构（需对应架构的 Go 产物）
#
# 与 installer/macos/build-pkg.sh 的区别（两者不是替代关系）：
#   build-pkg.sh         服务端/命令行形态：装到 /usr/local/bin，无窗口，只能开浏览器
#   build-desktop-pkg.sh 桌面形态：装成 /Applications 里的 .app，带原生窗口与托盘
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

if [ "$(uname -s)" != "Darwin" ]; then
  echo "!! 本脚本只能在 macOS 上运行（当前 $(uname -s)）" >&2
  exit 1
fi

ARCH="${ARCH:-}"
if [ -z "$ARCH" ]; then
  case "$(uname -m)" in
    arm64) ARCH=arm64 ;;
    x86_64) ARCH=amd64 ;;
    *) echo "!! 无法识别的架构 $(uname -m)，请显式传 ARCH=amd64|arm64" >&2; exit 1 ;;
  esac
fi
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "!! ARCH 只支持 amd64 / arm64（当前: $ARCH）" >&2; exit 1 ;;
esac

# 构建环境（Rust / Go / cargo 产物目录）统一由 env.sh 提供，含平台分支。
# shellcheck disable=SC1091
source desktop/scripts/env.sh

if ! cargo --version >/dev/null 2>&1; then
  echo "!! 未找到 cargo。先把 Rust 工具链装进 .toolchain/：" >&2
  echo "   curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | \\" >&2
  echo "     RUSTUP_HOME=\"$ROOT/.toolchain/rust\" CARGO_HOME=\"$ROOT/.toolchain/cargo\" \\" >&2
  echo "     sh -s -- -y --no-modify-path --profile minimal --default-toolchain stable" >&2
  exit 1
fi
command -v node >/dev/null 2>&1 || { echo "!! 未找到 Node，无法调用 Tauri CLI" >&2; exit 1; }

# ---- 1) 壳要代管的 Go 产物 ----
# 桌面壳把网关与本机代理作为**资源**打包，所以必须先有对应架构的二进制。
BIN_DIR="$ROOT/dist/darwin_${ARCH}"
GO_VERSION="$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go | head -1)"
[ -n "$GO_VERSION" ] || { echo "!! 未能从 main.go 解析版本号" >&2; exit 1; }

if [ ! -x "$BIN_DIR/workbuddy-gateway" ] || [ ! -x "$BIN_DIR/wb-local-agent" ]; then
  TARBALL="$ROOT/dist/workbuddy-gateway_${GO_VERSION}_darwin_${ARCH}.tar.gz"
  if [ ! -f "$TARBALL" ]; then
    echo "==> dist/ 下没有 darwin_${ARCH} 产物，先编译"
    bash scripts/release.sh "darwin/${ARCH}"
  fi
  echo "==> 解出 darwin_${ARCH} 二进制"
  tar -xzf "$TARBALL" -C "$ROOT/dist"
fi
[ -x "$BIN_DIR/workbuddy-gateway" ] || { echo "!! $BIN_DIR/workbuddy-gateway 不可执行" >&2; exit 1; }

# ---- 2) 壳的构建依赖 ----
if [ ! -d desktop/node_modules/@tauri-apps/cli ]; then
  echo "==> 安装桌面壳构建依赖（@tauri-apps/cli）"
  ( cd desktop && npm install --no-audit --no-fund )
fi

# ---- 3) 把 Go 产物复制进壳的资源目录 ----
echo "==> 准备壳资源"
node desktop/scripts/prepare-resources.mjs

# ---- 4) 编译并打包 ----
# 目标与资源清单见 desktop/src-tauri/tauri.macos.conf.json（Tauri 按平台自动叠加）。
echo "==> tauri build（dmg）"
( cd desktop && npx tauri build )

# ---- 5) 归拢到 dist/，命名与另外两个平台的桌面包对齐 ----
DESKTOP_VERSION="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' desktop/src-tauri/tauri.conf.json | head -1)"
BUNDLE_DIR="$CARGO_TARGET_DIR/release/bundle/dmg"
BUILT="$(ls -1 "$BUNDLE_DIR"/*.dmg 2>/dev/null | head -1)"
if [ -z "$BUILT" ]; then
  echo "!! 未在 $BUNDLE_DIR 找到 .dmg" >&2
  exit 1
fi

OUT="$ROOT/dist/workbuddy-gateway-desktop_${DESKTOP_VERSION}_${ARCH}.dmg"
cp -f "$BUILT" "$OUT"

echo
echo "==> 完成: $OUT"
ls -lh "$OUT"
echo "    壳版本 $DESKTOP_VERSION / 内含网关版本 $GO_VERSION"
echo ""
echo "安装:   打开 .dmg，把「WorkBuddy Gateway」拖进「应用程序」"
echo "        首次打开会被 Gatekeeper 拦下（未签名），右键 →「打开」即可"
echo "卸载:   退出应用后删掉 /Applications/WorkBuddy Gateway.app"
echo "数据:   ~/.wb-gateway（与命令行版、npm 入口共用同一份账号池）"
