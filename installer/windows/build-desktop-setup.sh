#!/usr/bin/env bash
# 构建 Windows 桌面版 NSIS 安装包（带原生窗口 + 托盘的 Tauri 壳）。
#
# 必须在 Windows 上执行：NSIS 与 .exe 的资源改写都是 Windows 工具链，无法交叉产出。
# 用 Git Bash（MSYS）跑。
#
# 用法：
#   ./installer/windows/build-desktop-setup.sh              # 当前架构
#   ARCH=arm64 ./installer/windows/build-desktop-setup.sh   # 指定架构（需对应架构的 Go 产物）
#
# ── 与作者本机流程的关键差异（不是笔误，是环境不同）──
#
# 本机走 **x86_64-pc-windows-gnu** + 手工补齐的 mingw 工具链
# （见 desktop/scripts/setup-rust-toolchain.sh —— 那是为了绕开本机 aka.ms 不可达、
# 装不上 VS Build Tools）。本脚本面向 CI 与其它**有 MSVC** 的 Windows：
#
#   1) 因此**不 source** desktop/scripts/env.sh。它的 Windows 分支是为本机那套便携
#      gnu 工具链写的（反斜杠路径 + mingw/self-contained 的 PATH 注入），在有 MSVC
#      的环境里套上去只会把 RUSTUP_HOME 指到一个空目录。
#   2) WebView2Loader.dll 的处理相反：gnu 动态链接、必须随包分发；MSVC 静态链接、
#      **不需要**。而 tauri.windows.conf.json 里为 gnu 显式声明了这个资源，MSVC 下
#      该文件不存在会让打包直接失败，所以这里用 `--config` 把 resources 覆盖成只有
#      `bin/*`。（实测 Tauri 的 --config 对数组是**替换**而非合并，这一条才真正生效。）
#
# 两条路都保留：想用 gnu 就照 desktop/README.md 走 scripts/build-desktop.sh。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) ;;
  *) echo "!! 本脚本只能在 Windows（Git Bash）上运行（当前 $(uname -s)）" >&2; exit 1 ;;
esac

ARCH="${ARCH:-}"
if [ -z "$ARCH" ]; then
  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo "!! 无法识别的架构 $(uname -m)，请显式传 ARCH=amd64|arm64" >&2; exit 1 ;;
  esac
fi
# 内部统一用 Go 的架构名（amd64/arm64），只在产物文件名上转成 Windows 习惯的 x64。
case "$ARCH" in
  amd64) LABEL=x64 ;;
  arm64) LABEL=arm64 ;;
  *) echo "!! ARCH 只支持 amd64 / arm64（当前: $ARCH）" >&2; exit 1 ;;
esac

if ! cargo --version >/dev/null 2>&1; then
  echo "!! 未找到 cargo。装 Rust（MSVC 目标）：https://rustup.rs" >&2
  exit 1
fi
command -v node >/dev/null 2>&1 || { echo "!! 未找到 Node，无法调用 Tauri CLI" >&2; exit 1; }

# 确认走的是 MSVC：gnu 目标下这个脚本的 DLL 处理是错的（那里**需要** DLL）。
RUST_HOST="$(rustc -vV | sed -n 's/^host: //p')"
case "$RUST_HOST" in
  *windows-gnu*)
    echo "!! 当前 Rust 主机目标是 $RUST_HOST（gnu）。" >&2
    echo "   本脚本按 MSVC 处理 WebView2Loader.dll，gnu 下会漏掉那个必需的 DLL。" >&2
    echo "   用 gnu 请改走：bash scripts/build-desktop.sh" >&2
    exit 1
    ;;
  *windows-msvc*) echo "==> Rust 目标: $RUST_HOST" ;;
  *) echo "!! 无法确认 Rust 主机目标（rustc -vV 得到 '$RUST_HOST'）" >&2; exit 1 ;;
esac

# ---- 1) 壳要代管的 Go 产物 ----
BIN_DIR="$ROOT/dist/windows_${ARCH}"
GO_VERSION="$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go | head -1)"
[ -n "$GO_VERSION" ] || { echo "!! 未能从 main.go 解析版本号" >&2; exit 1; }

if [ ! -f "$BIN_DIR/workbuddy-gateway.exe" ] || [ ! -f "$BIN_DIR/wb-local-agent.exe" ]; then
  TARBALL="$ROOT/dist/workbuddy-gateway_${GO_VERSION}_windows_${ARCH}.tar.gz"
  if [ ! -f "$TARBALL" ]; then
    echo "==> dist/ 下没有 windows_${ARCH} 产物，先编译"
    bash scripts/release.sh "windows/${ARCH}"
  fi
  echo "==> 解出 windows_${ARCH} 二进制"
  tar -xzf "$TARBALL" -C "$ROOT/dist"
fi
[ -f "$BIN_DIR/workbuddy-gateway.exe" ] || { echo "!! 缺 $BIN_DIR/workbuddy-gateway.exe" >&2; exit 1; }

# ---- 2) 壳的构建依赖 ----
if [ ! -d desktop/node_modules/@tauri-apps/cli ]; then
  echo "==> 安装桌面壳构建依赖（@tauri-apps/cli）"
  ( cd desktop && npm install --no-audit --no-fund )
fi

# ---- 3) 把 Go 产物复制进壳的资源目录 ----
echo "==> 准备壳资源"
node desktop/scripts/prepare-resources.mjs

# ---- 4) 编译并打包 ----
# 覆盖 resources 的理由见文件头第 2 点：MSVC 不需要 WebView2Loader.dll，
# 而基础配置里为 gnu 留着它。targets 仍是 tauri.conf.json 里的 nsis。
echo "==> tauri build（nsis）"
( cd desktop && npx tauri build --config '{"bundle":{"resources":["bin/*"]}}' )

# ---- 5) 归拢到 dist/，命名与作者原有的桌面包一致 ----
DESKTOP_VERSION="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' desktop/src-tauri/tauri.conf.json | head -1)"
# 没设 CARGO_TARGET_DIR 时 cargo 默认落在 desktop/src-tauri/target。
BUNDLE_DIR="${CARGO_TARGET_DIR:-$ROOT/desktop/src-tauri/target}/release/bundle/nsis"
BUILT="$(ls -1 "$BUNDLE_DIR"/*-setup.exe 2>/dev/null | head -1)"
if [ -z "$BUILT" ]; then
  echo "!! 未在 $BUNDLE_DIR 找到 *-setup.exe" >&2
  exit 1
fi

OUT="$ROOT/dist/workbuddy-gateway-desktop_${DESKTOP_VERSION}_${LABEL}-setup.exe"
cp -f "$BUILT" "$OUT"

echo
echo "==> 完成: $OUT"
ls -lh "$OUT"
echo "    壳版本 $DESKTOP_VERSION / 内含网关版本 $GO_VERSION / 目标 $RUST_HOST"
echo ""
echo "安装:   双击运行，无需管理员（每用户安装）"
echo "数据:   %LOCALAPPDATA%\\wb-gateway（与命令行版共用同一份账号池）"
