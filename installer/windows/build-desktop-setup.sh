#!/usr/bin/env bash
# 构建 Windows 桌面版安装包（带原生窗口 + 托盘的 Tauri 壳）。
#
# 必须在 Windows 上执行：.exe 的资源改写与 IExpress 自解压打包都是 Windows 工具链，
# 无法交叉产出。用 Git Bash（MSYS）跑。
#
# 用法：
#   ./installer/windows/build-desktop-setup.sh              # 当前架构
#   ARCH=arm64 ./installer/windows/build-desktop-setup.sh   # 指定架构
#
# ── 与旧流程的关键差异：Tauri 只编壳，不再负责打包 ──
#
# 以前这里是 `tauri build`（默认 targets=nsis），安装语义由 Tauri 生成的 NSIS 模板
# 决定。那套模板带来的问题在 internal/installer 的包注释里写清楚了（升级时选
# 「安装前卸载」必弹「无法卸载！」，因为 NSIS 跑完旧卸载器后用 ${FileExists} 判失败，
# 而旧卸载器永远删不掉被占用的文件）。
#
# 现在改成两段：
#   1. `tauri build --no-bundle` 只产出壳的可执行文件；
#   2. 把「壳 + 网关 + 安装脚本」打成 IExpress 自解压包，由网关自己的 install 子命令
#      执行安装 —— 与命令行版走**同一套**安装语义。
#
# 好处是三端行为一致、可单测、改动不再依赖打包工具链；代价是外观变成系统自解压
# 样式而非 NSIS 向导（两者都未签名）。
#
# ── MSVC 与 gnu 两条工具链 ──
#
# 本脚本按 **MSVC** 处理 WebView2Loader.dll（静态链接、不需要该 DLL）。作者本机的
# windows-gnu 路线（需要 DLL）见 desktop/scripts/setup-rust-toolchain.sh 与
# scripts/build-desktop.sh。
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
if ! command -v iexpress >/dev/null 2>&1; then
  echo "!! 未找到 iexpress（Windows 自带组件）" >&2
  exit 1
fi

# ── MSVC 与 gnu 两条工具链都要支持 ──
#
# 差别只有一个：WebView2Loader.dll。
#   - MSVC：静态链接，**不需要**该 DLL；
#   - gnu ：动态链接，**必须**随包分发，否则壳启动后静默退出（进程活着、无窗口）。
#
# 所以不写死走哪条，而是**先让环境决定**，再看结果。

# 第一步：如果仓库内补齐过 gnu 工具链（`.toolchain/rust` 里有），就 source env.sh。
#
# 顺序不能反 —— 这是个踩过的坑：先前是「先用系统 rustup 探测 host、是 gnu 才
# source env.sh」，而系统 rustup 装的通常是 msvc，于是永远探测出 msvc、
# 永远不 source，gnu 那条路根本到不了。探测结果本身依赖环境，用它当开关是循环。
if [ -d ".toolchain/rust/toolchains" ]; then
  echo "==> 采用仓库内 gnu 工具链（.toolchain/）"
  # shellcheck disable=SC1091
  source desktop/scripts/env.sh
fi

RUST_HOST="$(rustc -vV | sed -n 's/^host: //p')"
case "$RUST_HOST" in
  *windows-gnu*)
    echo "==> Rust 目标: $RUST_HOST（gnu，需随包携带 WebView2Loader.dll）"
    ;;
  *windows-msvc*) echo "==> Rust 目标: $RUST_HOST（msvc，静态链接）" ;;
  *) echo "!! 无法确认 Rust 主机目标（rustc -vV 得到 '$RUST_HOST'）" >&2; exit 1 ;;
esac

# ---- 1) 壳要代管的 Go 产物 ----
BIN_DIR="$ROOT/dist/windows_${ARCH}"
GO_VERSION="$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go | head -1)"
[ -n "$GO_VERSION" ] || { echo "!! 未能从 main.go 解析版本号" >&2; exit 1; }

TARBALL="$ROOT/dist/workbuddy-gateway_${GO_VERSION}_windows_${ARCH}.tar.gz"
if [ ! -f "$TARBALL" ]; then
  echo "==> dist/ 下没有 windows_${ARCH} 的发行包，先编译"
  bash scripts/release.sh "windows/${ARCH}"
fi

# 解包判据是「tarball 比已解出的二进制新」，**不是**「二进制不存在」——
# 后者在 dist/windows_<arch>/ 残留上一版二进制时会静默跳过解包，
# 于是包名是新的、里面是旧的。
NEED_UNPACK=1
if [ -f "$BIN_DIR/workbuddy-gateway.exe" ] && [ -f "$BIN_DIR/wb-local-agent.exe" ] \
   && [ "$BIN_DIR/workbuddy-gateway.exe" -nt "$TARBALL" ]; then
  NEED_UNPACK=0
  echo "==> dist/windows_${ARCH} 的二进制比发行包新，跳过解包"
fi
if [ "$NEED_UNPACK" = 1 ]; then
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

# ---- 4) 只编壳，不打系统安装包 ----
#
# --no-bundle 是关键：我们要的是 target/release/ 下的壳可执行文件本身，
# 而不是 Tauri 生成的 NSIS 安装器。打包在下一步由我们自己做。
echo "==> tauri build --no-bundle（只编壳）"
#
# resources 按工具链给：gnu 动态链接 WebView2Loader.dll，必须作为资源打进壳
# （tauri.windows.conf.json 里本来就声明了它，这里不能覆盖掉）；MSVC 静态链接，
# 那个文件在仓库里根本不存在，声明了反而让打包失败（Tauri 找不到声明的资源会报错）。
# 实测 Tauri 的 --config 对数组是**替换**而非合并，所以只能二选一。
if [ -f "desktop/src-tauri/WebView2Loader.dll" ]; then
  RES_JSON='["bin/*","WebView2Loader.dll"]'
else
  RES_JSON='["bin/*"]'
fi
( cd desktop && npx tauri build --no-bundle --config "{\"bundle\":{\"resources\":$RES_JSON}}" )

RELEASE_DIR="${CARGO_TARGET_DIR:-$ROOT/desktop/src-tauri/target}/release"
SHELL_EXE="$RELEASE_DIR/workbuddy-gateway-desktop.exe"
if [ ! -f "$SHELL_EXE" ]; then
  echo "!! 未找到壳可执行文件：$SHELL_EXE" >&2
  ls -1 "$RELEASE_DIR"/*.exe 2>/dev/null | sed 's/^/     /' >&2
  exit 1
fi

# ---- 5) 组装载荷并打成自解压安装包 ----
echo "==> 组装安装包载荷"
OUT_DIR="$ROOT/dist"
STAGE="$OUT_DIR/.desktop-setup-stage-$ARCH"
rm -rf "$STAGE"
mkdir -p "$STAGE"

cp "$SHELL_EXE" "$STAGE/workbuddy-gateway-desktop.exe"
cp "$BIN_DIR/workbuddy-gateway.exe" "$STAGE/"
cp "$BIN_DIR/wb-local-agent.exe" "$STAGE/" 2>/dev/null || true

# 安装脚本与随包文件，与命令行版共用同一套（安装语义在 Go 里，这里只是铺文件）。
for f in app.ico run.cmd setup.cmd uninstall.cmd launch-hidden.vbs INSTALL.txt; do
  [ -f "installer/windows/$f" ] || { echo "!! 缺少 installer/windows/$f" >&2; exit 1; }
  cp "installer/windows/$f" "$STAGE/"
done

# gnu 目标动态链接 WebView2Loader.dll，缺了壳启动后静默退出；MSVC 静态链接、
# 带上也没坏处（多一个文件而已）。所以「有就带」而不是按目标硬编码 —— 两条工具链
# 共用这一段，不会出现「改了一边忘了另一边」。
if [ -f "desktop/src-tauri/WebView2Loader.dll" ]; then
  cp "desktop/src-tauri/WebView2Loader.dll" "$STAGE/"
  echo "    随包携带 WebView2Loader.dll（gnu 目标需要）"
fi

DESKTOP_VERSION="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' desktop/src-tauri/tauri.conf.json | head -1)"
[ -n "$DESKTOP_VERSION" ] || { echo "!! 未能从 tauri.conf.json 解析版本号" >&2; exit 1; }

TARGET="$OUT_DIR/workbuddy-gateway-desktop_${DESKTOP_VERSION}_${LABEL}-setup.exe"
rm -f "$TARGET"

echo "==> 生成 IExpress 指令并打包"
STAGE_WIN="$(cygpath -w "$STAGE")"
TARGET_WIN="$(cygpath -w "$TARGET")"
SED="$OUT_DIR/desktop-setup-$ARCH.sed"
rm -f "$SED"
python - "$STAGE" "$STAGE_WIN" "$TARGET_WIN" "$DESKTOP_VERSION" "$SED" <<'PY'
import io, os, sys

stage, stage_win, target_win, version, sed_path = sys.argv[1:6]
if not stage_win.endswith("\\"):
    stage_win += "\\"

names = sorted(os.listdir(stage))

lines = [
    "[Version]",
    "Class=IEXPRESS",
    "SEDVersion=3",
    "[Options]",
    "PackagePurpose=InstallApp",
    # 显示解压进度窗口：静默解压几十 MB 会让人以为双击没反应
    "ShowInstallProgramWindow=1",
    "HideExtractAnimation=0",
    "UseLongFileName=1",
    "InsideCompressed=0",
    "CAB_FixedSize=0",
    "CAB_ResvCodeSigning=0",
    "RebootMode=N",
    "InstallPrompt=",
    "DisplayLicense=",
    "FinishMessage=",
    "TargetName=" + target_win,
    "FriendlyName=WorkBuddy Gateway " + version + " Setup",
    "AppLaunched=setup.cmd",
    "PostInstallCmd=<None>",
    "AdminQuietInstCmd=",
    "UserQuietInstCmd=",
    "SourceFiles=SourceFiles",
    "[SourceFiles]",
    "SourceFiles0=" + stage_win,
    "[SourceFiles0]",
]
for i in range(len(names)):
    lines.append("%%FILE%d%%=" % i)
lines.append("[Strings]")
for i, n in enumerate(names):
    lines.append('FILE%d="%s"' % (i, n))

with io.open(sed_path, "w", encoding="ascii", newline="\r\n") as f:
    f.write("\n".join(lines) + "\n")
print("  payload files:", len(names))
for n in names:
    print("   -", n)
PY

# iexpress 是 GUI 程序：退出码不可靠（所以以**产物是否存在**为准），而且**实测会偶发失败**
# —— 同一份输入重跑一次就成功。因此这里重试最多 3 次，并把输出留在文件里。
IEXPRESS_LOG="$OUT_DIR/desktop-setup-$ARCH.iexpress.log"
iexpress_ok=false
for attempt in 1 2 3; do
  rm -f "$TARGET"
  iexpress /N /Q "$(cygpath -w "$SED")" >"$IEXPRESS_LOG" 2>&1 || true
  if [ -f "$TARGET" ]; then
    iexpress_ok=true
    break
  fi
  echo "   第 $attempt 次未产出，重试…" >&2
  sleep 2
done

if [ "$iexpress_ok" != true ]; then
  echo "!! 未生成 $TARGET（iexpress 连续 3 次失败）" >&2
  echo "   iexpress 输出（$IEXPRESS_LOG）：" >&2
  sed 's/^/     /' "$IEXPRESS_LOG" 2>/dev/null | tail -20 >&2 || true
  exit 1
fi
rm -f "$IEXPRESS_LOG"
rm -rf "$STAGE" "$SED"

echo
echo "==> 完成: $TARGET"
ls -lh "$TARGET"
echo "    壳版本 $DESKTOP_VERSION / 内含网关版本 $GO_VERSION / 目标 $RUST_HOST"
echo ""
echo "安装:   双击运行，无需管理员（每用户安装）"
echo "数据:   %LOCALAPPDATA%\\wb-gateway（与命令行版共用同一份账号池）"
