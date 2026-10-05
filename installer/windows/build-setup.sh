#!/usr/bin/env bash
# 构建 Windows 安装包 workbuddy-gateway_<版本>_<arch>-setup.exe。
#
# 用 Windows 自带的 IExpress 做成自解压包 + 运行 setup.cmd，而不是 NSIS：
#   * IExpress 是系统组件（System32\iexpress.exe），不需要额外装工具链，
#     因此这个安装包在开发机上就能构建和验证，而不是"只在 CI 里能出、本地无法复现"；
#   * 安装逻辑集中在一份可读的 install.ps1 里，不需要再维护一份 .nsi 脚本
#     （两份实现最容易出现"改了一边忘了另一边"）。
# 代价：外观是系统自解压样式而非 NSIS 向导，且同样没有代码签名。
#
# 用法：
#   ./installer/windows/build-setup.sh            # 默认 x64
#   ./installer/windows/build-setup.sh arm64      # Windows on ARM
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

ARCH="${1:-x64}"
case "$ARCH" in
  x64)   RELEASE_SUFFIX="windows_amd64" ;;
  arm64) RELEASE_SUFFIX="windows_arm64" ;;
  *) echo "!! 未知架构: $ARCH（可选 x64 / arm64）" >&2; exit 1 ;;
esac

VERSION="$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go | head -1)"
[ -n "$VERSION" ] || { echo "!! 未能从 main.go 解析版本号" >&2; exit 1; }

TARBALL="$ROOT/dist/workbuddy-gateway_${VERSION}_${RELEASE_SUFFIX}.tar.gz"
if [ ! -f "$TARBALL" ]; then
  echo "!! 找不到 $TARBALL，请先执行 ./scripts/release.sh" >&2
  exit 1
fi

if ! command -v iexpress >/dev/null 2>&1; then
  echo "!! 未找到 iexpress（Windows 自带组件，非 Windows 平台无法构建本安装包）" >&2
  exit 1
fi

OUT_DIR="$ROOT/dist"
STAGE="$OUT_DIR/.setup-stage-$ARCH"
rm -rf "$STAGE"
mkdir -p "$STAGE"

echo "==> 版本 $VERSION / $ARCH"

# ---- 组装载荷 ----
echo "==> 解压发行包"
tar -xzf "$TARBALL" -C "$STAGE"
SRC="$STAGE/$RELEASE_SUFFIX"
[ -d "$SRC" ] || { echo "!! 发行包结构异常：缺 $RELEASE_SUFFIX 目录" >&2; exit 1; }
for f in workbuddy-gateway.exe wb-local-agent.exe; do
  [ -f "$SRC/$f" ] || { echo "!! 发行包里缺 $f" >&2; exit 1; }
  mv "$SRC/$f" "$STAGE/"
done

echo "==> 加入安装脚本"
for f in app.ico run.cmd setup.cmd install.ps1 uninstall.ps1 launch-hidden.vbs INSTALL.txt; do
  [ -f "installer/windows/$f" ] || { echo "!! 缺少 installer/windows/$f" >&2; exit 1; }
  cp "installer/windows/$f" "$STAGE/"
done
rm -rf "$SRC"

# ---- 生成 IExpress 指令文件 ----
# SED 必须是 ANSI（这里全 ASCII）且用 CRLF 换行，否则 iexpress 会解析失败。
TARGET="$OUT_DIR/workbuddy-gateway_${VERSION}_${ARCH}-setup.exe"
rm -f "$TARGET"

echo "==> 生成 IExpress 指令"
STAGE_WIN="$(cygpath -w "$STAGE")"
TARGET_WIN="$(cygpath -w "$TARGET")"
SED="$OUT_DIR/setup-$ARCH.sed"
rm -f "$SED"
python - "$STAGE" "$STAGE_WIN" "$TARGET_WIN" "$VERSION" "$SED" <<'PY'
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
    # 显示解压进度窗口：静默解压十几 MB 会让人以为双击没反应
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
    # 解压完成后执行的命令（相对解压目录）
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

# ---- 调用 iexpress ----
# /N 只构建不弹界面，/Q 静默。
echo "==> 打包 setup.exe（iexpress）"
# iexpress 是 GUI 程序：退出码不可靠（所以以**产物是否存在**为准），而且**实测会偶发失败**
# —— 同一份输入重跑一次就成功（2026-09-30 实测：arm64 首次失败、立即重试成功）。
# 因此这里重试最多 3 次，并把输出留在文件里：原来 `>/dev/null` 把输出全丢了，
# 失败时只剩一句「iexpress 失败」，既看不出原因也没法重试，白花一轮排查。
IEXPRESS_LOG="$OUT_DIR/setup-$ARCH.iexpress.log"
iexpress_ok=false
for attempt in 1 2 3; do
  # 每次尝试前清掉产物，否则「产物存在」这个判据会把上一次的残留当成本次成功。
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
  echo "   可手工排查：iexpress /N \"$(cygpath -w "$SED")\"" >&2
  exit 1
fi
rm -f "$IEXPRESS_LOG"

rm -rf "$STAGE" "$SED"

echo "==> 完成: $TARGET"
ls -lh "$TARGET"
