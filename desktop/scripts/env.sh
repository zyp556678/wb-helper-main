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
