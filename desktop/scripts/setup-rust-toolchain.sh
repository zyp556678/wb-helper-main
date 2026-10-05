#!/usr/bin/env bash
# 补齐 x86_64-pc-windows-gnu 的 C 工具链（幂等，可重复执行）。
#
# ══ 一句话 ═══════════════════════════════════════════════════════════
# rustup 的 windows-gnu 工具链自称自足，实测缺三个关键件；本脚本把它们补齐，
# 使 Tauri 能在**不安装 Visual Studio Build Tools**的前提下构建出 Windows 安装包。
#
# ══ 缺了什么，各自表现成什么样 ═══════════════════════════════════════
#
# 缺陷 1：dlltool 生成 import library 失败（0 字节）
#   影响：windows-sys / parking_lot_core / getrandom / windows-result /
#         windows-strings 等使用 raw-dylib 的 crate 全部编译失败
#   表象：<path>\dlltool.exe: CreateProcess       ← 没有原因，只有这一行
#   真因（三层叠加）：
#     a. 缺 as.exe          —— dlltool 要汇编器把生成的 .s 汇编成 .o
#     b. 缺运行库 DLL        —— libintl-8 / libiconv-2 / zlib1 / libzstd
#     c. --temp-prefix 冒号  —— rustc 固定传 `--temp-prefix kernel32.dll:`，
#                              尾部 `:` 在 Windows 上非法
#   为何错误信息没有原因：GNU binutils 经 libiberty 的 pex-win32.c 启动子进程，
#   失败时只赋值 errmsg = "CreateProcess"，不打印 GetLastError()。
#
# 缺陷 2：windres 预处理 .rc 失败（tauri-build 整体失败）
#   表象：windres: preprocessing failed.
#         panicked at tauri-winres-0.3.6/src/lib.rs:543
#   真因：windres 编译 .rc 前会调 `gcc -E` 做预处理，而工具链里那个 gcc
#         只是**链接驱动**，没有编译内核 cc1：
#             cannot execute 'cc1': CreateProcess: No such file or directory
#
# 缺陷 3：没有名为 gcc 的链接器
#   rustc 在 windows-gnu 下的默认 linker 名就是 `gcc`，而 self-contained 里
#   只有 x86_64-w64-mingw32-gcc.exe。
#
# ══ 一个反复踩到的隐蔽点 ═════════════════════════════════════════════
# GNU 工具会用**自身文件名**推导配套工具的名字，再从自己所在目录查找。
# 把真品改名为 dlltool-real.exe 会让它找不到 as，报出**同样**的 "CreateProcess"。
# 因此本脚本一律采用「外层顶替名字 + 真品放子目录保持原名」的结构。
#
# ══ 为什么不用别的路线 ═══════════════════════════════════════════════
#   · MSVC：本机 aka.ms（VS Build Tools 引导器）不可达
#   · 完整 mingw：GitHub release 实测 5–14 KB/s（110MB 要数小时）
#   · msys2 镜像：清华/中科大 1.8 MB/s，是本脚本的取包通道
#
# 用法：bash desktop/scripts/setup-rust-toolchain.sh

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TOOLCHAIN="$ROOT/.toolchain"
export RUSTUP_HOME="${RUSTUP_HOME:-$TOOLCHAIN/rust}"
export CARGO_HOME="${CARGO_HOME:-$TOOLCHAIN/cargo}"

TRIPLE="x86_64-pc-windows-gnu"
SC="$RUSTUP_HOME/toolchains/stable-$TRIPLE/lib/rustlib/$TRIPLE/bin/self-contained"
MSYS2="$TOOLCHAIN/msys2/mingw64/bin"
MIRROR="https://mirrors.tuna.tsinghua.edu.cn/msys2/mingw/mingw64"
GO_BIN="$TOOLCHAIN/go/bin/go.exe"
BUILD="$TOOLCHAIN/toolchain-build"
GIT_MINGW="/mingw64/bin"

log()  { printf '\033[36m[setup]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[setup] %s\033[0m\n' "$*"; }
die()  { printf '\033[31m[setup] 错误：%s\033[0m\n' "$*" >&2; exit 1; }

[ -d "$SC" ] || die "未找到 self-contained：$SC（请先安装 rustup 的 $TRIPLE 工具链）"
[ -x "$GO_BIN" ] || die "未找到 Go 工具链：$GO_BIN"
mkdir -p "$BUILD"

# ── 1. 取完整 binutils ────────────────────────────────────────────────
if [ ! -x "$MSYS2/as.exe" ]; then
  log "从 msys2 镜像取完整 binutils"
  mkdir -p "$TOOLCHAIN/msys2"
  pkg=$(curl -s --max-time 60 "$MIRROR/" \
        | grep -oE 'mingw-w64-x86_64-binutils-[0-9][^"<>]*\.pkg\.tar\.zst' | sort -u | tail -1)
  [ -n "$pkg" ] || die "未能解析 binutils 包名（镜像不可达？）"
  log "目标：$pkg"
  curl -L --max-time 900 -o "$TOOLCHAIN/msys2/binutils.pkg.tar.zst" "$MIRROR/$pkg"
  # bsdtar 支持 zstd；Git Bash 自带的 GNU tar 不支持
  ( cd "$TOOLCHAIN/msys2" && /c/Windows/System32/tar.exe -xf binutils.pkg.tar.zst )
fi
[ -x "$MSYS2/as.exe" ] || die "解压后仍无 as.exe"

log "补入 binutils 工具（ld 保留 rustup 原版，避免影响链接）"
for t in as addr2line c++filt dlltool ar objcopy objdump ranlib readelf size \
         strings strip windres windmc dllwrap elfedit gprof nm; do
  [ -f "$MSYS2/$t.exe" ] && cp -f "$MSYS2/$t.exe" "$SC/$t.exe"
done

# ── 2. 运行库 DLL ────────────────────────────────────────────────────
[ -d "$GIT_MINGW" ] || die "未找到 $GIT_MINGW（Windows 构建需要 Git for Windows 提供这几个运行库）"
log "补入运行库 DLL"
for d in libintl-8 libiconv-2 zlib1 libzstd; do
  cp -f "$GIT_MINGW/$d.dll" "$SC/$d.dll"
done

# ── 3. gcc / cc 链接器别名 ───────────────────────────────────────────
# rustc 的默认 linker 名就是 gcc；仅作链接驱动，不需要 cc1。
log "补 gcc.exe / cc.exe 别名"
cp -f "$SC/x86_64-w64-mingw32-gcc.exe" "$SC/gcc.exe"
cp -f "$SC/x86_64-w64-mingw32-gcc.exe" "$SC/cc.exe"

# ── 4. dlltool 适配器（解决 --temp-prefix 冒号）────────────────────────
log "编译 dlltool 适配器"
( cd "$ROOT/desktop/toolchain/dlltool-shim" \
  && GOCACHE="$TOOLCHAIN/gocache" "$GO_BIN" build -o "$BUILD/dlltool.exe" . )

BIN_D="$SC/dlltool-bin"
mkdir -p "$BIN_D"
if [ -f "$SC/dlltool.exe" ] && [ ! -f "$BIN_D/dlltool.exe" ]; then
  mv -f "$SC/dlltool.exe" "$BIN_D/dlltool.exe"      # 首次：把当前真品挪进子目录并保持原名
elif [ -f "$MSYS2/dlltool.exe" ]; then
  cp -f "$MSYS2/dlltool.exe" "$BIN_D/dlltool.exe"
fi
for f in as.exe libintl-8.dll libiconv-2.dll zlib1.dll libzstd.dll; do
  cp -f "$SC/$f" "$BIN_D/$f"
done
cp -f "$BUILD/dlltool.exe" "$SC/dlltool.exe"

# ── 5. rc 工具（解决 windres 缺预处理器）─────────────────────────────
log "编译 rc 工具（直通预处理器 + windres 转发器）"
( cd "$ROOT/desktop/toolchain/rc-tools/rcpp" \
  && GOCACHE="$TOOLCHAIN/gocache" "$GO_BIN" build -o "$BUILD/rcpp.exe" . )
( cd "$ROOT/desktop/toolchain/rc-tools/windres-shim" \
  && GOCACHE="$TOOLCHAIN/gocache" "$GO_BIN" build -o "$BUILD/windres.exe" . )

BIN_W="$SC/windres-bin"
mkdir -p "$BIN_W"
if [ -f "$SC/windres.exe" ] && [ ! -f "$BIN_W/windres.exe" ]; then
  mv -f "$SC/windres.exe" "$BIN_W/windres.exe"
elif [ -f "$MSYS2/windres.exe" ]; then
  cp -f "$MSYS2/windres.exe" "$BIN_W/windres.exe"
fi
for f in libintl-8.dll libiconv-2.dll zlib1.dll libzstd.dll; do
  cp -f "$SC/$f" "$BIN_W/$f"
done
cp -f "$BUILD/windres.exe" "$SC/windres.exe"
cp -f "$BUILD/rcpp.exe"    "$SC/rcpp.exe"

# ── 自检 ─────────────────────────────────────────────────────────────
log "自检"
probe=$(mktemp -d)
cat > "$probe/t.def" <<'EOF'
LIBRARY kernel32.dll
EXPORTS
GetLastError
EOF
if ( cd "$probe" && "$SC/dlltool.exe" -d t.def -D kernel32.dll -l t.lib -m i386:x86-64 \
      -f --64 --no-leading-underscore --temp-prefix 'kernel32.dll:' >/dev/null 2>&1 ) \
   && [ -s "$probe/t.lib" ]; then
  printf '  dlltool（含 rustc 的 --temp-prefix 冒号参数）: \033[32m通过\033[0m\n'
else
  printf '  dlltool: \033[31m失败\033[0m\n'; warn "请检查 $SC/dlltool-bin/ 与运行库 DLL"
fi
rm -rf "$probe"

printf '  windres : %s\n  rcpp    : %s\n  gcc     : %s\n' \
  "$(basename "$SC/windres.exe")" "$(basename "$SC/rcpp.exe")" "$(basename "$SC/gcc.exe")"
printf '\n下一步：source desktop/scripts/env.sh，再 cd desktop/src-tauri && cargo build\n'
