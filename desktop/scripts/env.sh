# 项目构建环境（Rust 桌面壳 + Go 网关）
#
# 用法：
#   source desktop/scripts/env.sh
#
# 把这些设定固化下来的原因：本项目的构建环境有三处非默认配置，散在命令行里
# 必然会被遗忘或抄错。
#
#   1. RUSTUP_HOME / CARGO_HOME / GOCACHE 指向项目内 .toolchain/，不污染用户目录
#   2. Rust 使用 x86_64-pc-windows-gnu —— 构建**不需要** Visual Studio Build Tools
#   3. PATH 需包含补齐后的 self-contained 与 msys2 binutils
#
# 若工具链尚未补齐，先跑：
#   bash desktop/scripts/setup-rust-toolchain.sh

# --- 隔离目录 ---
#
# 路径按脚本自身位置推导，**不写死绝对路径**：写死的话换个目录克隆
# （或别人 clone 到自己的路径下）整份构建环境就全废，而且症状是
# 「找不到 cargo / rustc」这种毫无指向性的报错。
_ENV_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_ROOT="$(cd "$_ENV_DIR/../.." && pwd)/.toolchain"

# 平台判定：MSYS2 / Git-Bash 的 uname 报 MINGW64_NT-* 或 MSYS_NT-*，Linux 报 Linux。
# 不拿「有没有 cygpath」当判据 —— Linux 上装了 mingw 交叉工具链时同样会有 cygpath，
# 那会把 Linux 误判成 Windows，进而设出 `...\.toolchain\rust` 这种带反斜杠的路径。
case "$(uname -s 2>/dev/null)" in
  MINGW*|MSYS*|CYGWIN*) _IS_WINDOWS=1 ;;
  *) _IS_WINDOWS=0 ;;
esac

if [ "$_IS_WINDOWS" = "1" ]; then
  # Rust/Go 都是 Windows 原生程序，要的是 Windows 形态的路径，不是 MSYS 的 /d/...。
  if command -v cygpath >/dev/null 2>&1; then
    _ROOT_WIN="$(cygpath -w "$_ROOT")"
  else
    _ROOT_WIN="$_ROOT"
  fi

  export RUSTUP_HOME="$_ROOT_WIN\\rust"
  export CARGO_HOME="$_ROOT_WIN\\cargo"
  export GOCACHE="$_ROOT_WIN\\gocache"

  _CARGO_BIN="$_ROOT/cargo/bin"
  _MINGW_BIN="$_ROOT/msys2/mingw64/bin"
  _GO_BIN="$_ROOT/go/bin"
  _SC="$_ROOT/rust/toolchains/stable-x86_64-pc-windows-gnu/lib/rustlib/x86_64-pc-windows-gnu/bin/self-contained"

  export PATH="$_SC:$_CARGO_BIN:$_MINGW_BIN:$_GO_BIN:$PATH"
else
  # Linux / macOS：同一个 .toolchain/ 隔离目录，只是路径是普通的 Unix 形态。
  # 下面那一大段 mingw / self-contained / dlltool 修补全部不适用 —— 那些是
  # `x86_64-pc-windows-gnu` 专属的问题，Linux 上用的是系统 gcc 链接器。
  export RUSTUP_HOME="$_ROOT/rust"
  export CARGO_HOME="$_ROOT/cargo"
  export GOCACHE="$_ROOT/gocache"

  export PATH="$_ROOT/cargo/bin:$_ROOT/go/bin:$PATH"
fi

# cargo 的构建产物目录固定到 .toolchain/ 内 —— 这是仓库既有约定
# （见 .gitignore 第 52 行与 prepare-resources.mjs 找回 WebView2Loader.dll 的路径）。
# 放在这里而不是各脚本里，是为了让 `cargo build` 与打包脚本看到同一个 target 目录：
# 手工 `cargo build` 一次就能自愈缺失的 DLL / 复用增量产物，不必两处各编一遍。
export CARGO_TARGET_DIR="$_ROOT/cargo-target"

# ─────────────────────────────────────────────────────────────────────
# 为什么需要 windows-gnu 而不是 MSVC
#
# 本机 aka.ms（VS Build Tools 引导器）不可达，MSVC 路线被堵死；而 gnu 路线在补齐
# 工具链后完全可用，还省掉 2–4GB 下载。
#
# 为什么工具链需要补齐（三个叠加缺陷）
#
# rustup 的 windows-gnu 自带 self-contained 工具集号称自足，实测残缺：
#
#   1. 缺 as.exe —— dlltool 需要汇编器把生成的 .s 汇编成 .o
#   2. 缺运行库 DLL —— libintl-8 / libiconv-2 / zlib1 / libzstd
#   3. rustc 固定传 `--temp-prefix kernel32.dll:`，尾部冒号在 Windows 上是非法
#      路径字符，导致产物为 0 字节
#
# 三者中任一存在，windows-sys / parking_lot_core / getrandom / windows-result /
# windows-strings 这些使用 raw-dylib 的 crate 就会编译失败，报错只有一行
# `<path>\dlltool.exe: CreateProcess` —— 因为 binutils 经 libiberty 的
# pex-win32.c 启动子进程时只赋值 errmsg、不打印 GetLastError()。
#
# 修复见 desktop/scripts/setup-rust-toolchain.sh：
#   · 从 msys2 镜像取完整 binutils（比 GitHub release 快约两个数量级：1.8MB/s vs 14KB/s）
#   · 从 Git 自带 /mingw64/bin 取四个运行库 DLL
#   · 用 desktop/toolchain/dlltool-shim 编译的参数适配器改写带冒号的 --temp-prefix
#   · 真品 dlltool 保留原名放在 self-contained/dlltool-bin/（与 as.exe、DLL 同目录）
#
# 最后一个坑：GNU 工具会用**自身文件名**推导配套工具名。把真品改名为
# dlltool-real.exe 会让它找不到 as，报同样的 "CreateProcess" 错误。
#
# 依赖源也必须走镜像
#
# 本机网络下 static.rust-lang.org / static.crates.io 会被中间层返回
# 「大小相同但内容不同」的替换文件 —— 实测 rustup-init.exe 官方 sha256 为
# 6d5b5709...，实收内容却是 5d1d7045...，两者字节数完全一致。
# 故 rustup 与 crates 统一走 rsproxy.cn（已交叉校验与官方 sha256 一致），
# crates 镜像配置见 .toolchain/cargo/config.toml。
#
# 另：GitHub release 通道实测仅 5–14 KB/s，需要 GitHub 资源时走
# gh-proxy.com / ghproxy.net（可用，但同样慢）；大文件优先找国内官方镜像。
# ─────────────────────────────────────────────────────────────────────
