#!/usr/bin/env bash
# 构建 Linux 桌面版 .deb（带原生窗口 + 托盘常驻的 Tauri 壳）。
#
# 与 installer/linux/build-deb.sh 的区别（两者不是替代关系）：
#   build-deb.sh          服务端形态：只装 systemd 服务 + 面板，浏览器访问，没有窗口
#   build-desktop-deb.sh  桌面形态：装带窗口和托盘的 App，壳代管同一个网关
# 两者都装 `workbuddy-gateway` 到 /usr/bin，但桌面版的网关是壳的内部资源
# （落在 /usr/lib/<包名>/ 下），不会和 systemd 单元抢同一个文件。
#
# 必须在 Linux 上执行（需要 dpkg-deb 与 WebKitGTK 开发库）。
#
# 用法：
#   ./installer/linux/build-desktop-deb.sh              # 当前架构
#   ARCH=arm64 ./installer/linux/build-desktop-deb.sh   # 指定架构（需对应架构的 Go 产物）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

ARCH="${ARCH:-}"
if [ -z "$ARCH" ]; then
  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo "!! 无法识别的架构 $(uname -m)，请显式传 ARCH=amd64|arm64" >&2; exit 1 ;;
  esac
fi
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "!! ARCH 只支持 amd64 / arm64（当前: $ARCH）" >&2; exit 1 ;;
esac

if ! command -v dpkg-deb >/dev/null 2>&1; then
  echo "!! 未找到 dpkg-deb：deb 只能在 Debian/Ubuntu 上构建" >&2
  exit 1
fi

# 构建环境（Rust / Go / cargo 产物目录）统一由 env.sh 提供，含平台分支。
# shellcheck disable=SC1091
source desktop/scripts/env.sh

if ! cargo --version >/dev/null 2>&1; then
  echo "!! 未找到 cargo。先装 Rust 工具链到 .toolchain/：" >&2
  echo "   bash desktop/scripts/setup-rust-toolchain.sh" >&2
  echo "   （Linux 上也只需这一步，脚本会把工具链装进仓库内的 .toolchain/）" >&2
  exit 1
fi

if ! command -v node >/dev/null 2>&1; then
  echo "!! 未找到 Node，无法调用 Tauri CLI" >&2
  exit 1
fi

# Tauri 在 Linux 上依赖系统 WebKitGTK / GTK / 托盘库。缺了会在链接期报一堆
# pkg-config 找不到，这里提前给出可照做的安装命令。
MISSING=""
for p in webkit2gtk-4.1 gtk+-3.0 librsvg-2.0; do
  pkg-config --exists "$p" 2>/dev/null || MISSING="$MISSING $p"
done
if [ -n "$MISSING" ]; then
  echo "!! 缺少系统开发库:$MISSING" >&2
  echo "   sudo apt install libwebkit2gtk-4.1-dev libgtk-3-dev librsvg2-dev patchelf" >&2
  exit 1
fi

# ---- 1) 壳要代管的 Go 产物 ----
# 桌面壳把网关与本机代理作为**资源**打包，所以必须先有 Linux 版二进制。
# 优先用 dist/ 里已有的发行包，没有再现场交叉编译。
BIN_DIR="$ROOT/dist/linux_${ARCH}"
GO_VERSION="$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go | head -1)"
[ -n "$GO_VERSION" ] || { echo "!! 未能从 main.go 解析版本号" >&2; exit 1; }
TARBALL="$ROOT/dist/workbuddy-gateway_${GO_VERSION}_linux_${ARCH}.tar.gz"

if [ ! -f "$TARBALL" ]; then
  echo "==> dist/ 下没有 linux_${ARCH} 的发行包，先编译"
  bash scripts/release.sh "linux/${ARCH}"
fi

# 解包判据是「tarball 比已解出的二进制新」，**不是**「二进制不存在」。
#
# 后者有个很难发现的坑：dist/linux_<arch>/ 里若残留上一版的二进制（比如删了旧
# tarball 却忘了删解包目录），解包会被静默跳过，于是**包名是新的、里面装的是旧的** ——
# 构建一路成功，装上去现象一点没变。scripts/package-desktop.py 的文档里专门警告过
# 这类误判，这里同理。（真踩过，就在升 0.9.1 的时候。）
NEED_UNPACK=1
if [ -x "$BIN_DIR/workbuddy-gateway" ] && [ -x "$BIN_DIR/wb-local-agent" ] \
   && [ "$BIN_DIR/workbuddy-gateway" -nt "$TARBALL" ]; then
  NEED_UNPACK=0
  echo "==> dist/linux_${ARCH} 的二进制比发行包新，跳过解包"
fi
if [ "$NEED_UNPACK" = 1 ]; then
  echo "==> 解出 linux_${ARCH} 二进制"
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
# 目标/资源配置见 desktop/src-tauri/tauri.linux.conf.json（Tauri 会按平台自动叠加）。
echo "==> tauri build（deb）"
( cd desktop && npx tauri build )

# ---- 5) 归拢到 dist/，命名与 Windows 桌面包对齐 ----
DESKTOP_VERSION="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' desktop/src-tauri/tauri.conf.json | head -1)"
[ -n "$DESKTOP_VERSION" ] || { echo "!! 未能从 tauri.conf.json 解析版本号" >&2; exit 1; }
BUNDLE_DIR="$CARGO_TARGET_DIR/release/bundle/deb"

# 按**本次版本号**精确匹配，不能写成 `ls "$BUNDLE_DIR"/*.deb | head -1`：
# bundle 目录会累积历次构建的产物，而 ls 按字母序排列 —— 升到 0.9.1 之后，
# `head -1` 取到的仍然是 `WorkBuddy Gateway_0.9.0_amd64.deb`，
# 于是**包名是新的、内容是旧的**（真踩过，就在这次升版本时）。
# 版本号两侧带下划线，顺带防住 0.9.1 与 0.9.10 这类的名字误配。
BUILT="$(ls -1 "$BUNDLE_DIR"/*_"${DESKTOP_VERSION}"_*.deb 2>/dev/null | head -1)"
if [ -z "$BUILT" ]; then
  echo "!! 未在 $BUNDLE_DIR 找到 ${DESKTOP_VERSION} 版的 .deb" >&2
  echo "   该目录现有：" >&2
  ls -1 "$BUNDLE_DIR" 2>/dev/null | sed 's/^/     /' >&2
  exit 1
fi

OUT="$ROOT/dist/workbuddy-gateway-desktop_${DESKTOP_VERSION}_${ARCH}.deb"
cp -f "$BUILT" "$OUT"

echo
echo "==> 完成: $OUT"
ls -lh "$OUT"
echo "    壳版本 $DESKTOP_VERSION / 内含网关版本 $GO_VERSION"
echo ""
echo "安装:   sudo dpkg -i $(basename "$OUT")   # 依赖缺失时用 sudo apt -f install 补齐"
# 包名由 Tauri 从 productName「WorkBuddy Gateway」推导（转小写并把驼峰拆成连字符），
# 不是文件名里的 workbuddy-gateway-desktop。它与服务端包的 workbuddy-gateway
# **不同名**，因此两者可以并存 —— 但共用 8317 端口，只能有一个设置开机自启。
echo "卸载:   sudo dpkg -r work-buddy-gateway"
echo "文件名是 dist/ 里的命名习惯，dpkg 包名以 control 里的 Package 为准。"
echo "注意:   GNOME 下托盘图标需要 AppIndicator 扩展，否则只有窗口没有托盘图标。"
