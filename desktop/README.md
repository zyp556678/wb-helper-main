# 桌面壳（desktop/）

WorkBuddy 网关的原生桌面形态：**Tauri 薄壳 + 托盘常驻**，窗口内承载的仍是那套
React 面板，不复制任何界面代码。

```
desktop/
├── ui/index.html              启动页（等后端就绪期间显示，含失败诊断）
├── scripts/prepare-resources.mjs   把 Go 二进制复制进壳的资源目录
└── src-tauri/
    ├── src/main.rs            入口：单实例 → 托盘 → 后台启动后端
    ├── src/backend.rs         Go 进程代管：定位、探测、启动、就绪等待、收尾
    ├── src/tray.rs            托盘菜单、窗口显隐策略
    ├── tauri.conf.json        窗口与打包配置
    └── icons/                 从 installer/ 复用的图标资源
```

## 壳做什么、不做什么

壳**只做四件事**：

1. 找到并启动 `workbuddy-gateway.exe serve`
2. 轮询 `/healthz`，等到就绪
3. 用原生窗口加载 `http://127.0.0.1:8317/panel/`
4. 托盘常驻 + 退出时收掉自己拉起的进程

壳**不做**业务逻辑，也不使用 Tauri IPC。原因很直接：面板与 API **同源**
（都由本机 8317 提供），前端直接发 HTTP 即可，IPC 在这里只会多一层间接、
并且迫使壳去维护一套 commands 接口。这也让壳的依赖表保持在最小规模。

## 三个刻意的行为决策

**关闭窗口 ≠ 退出应用。** 关闭动作被解释为收进托盘。网关的核心价值是常驻后台
给 CLI / IDE 提供 API，关窗即停服会让桌面版弱于命令行版。真正的退出只走托盘菜单
的「退出」。

**复用而非独占端口。** 启动前先探测 8317：如果已经有网关在跑（用户先前用 CLI 或
安装包启动过），壳直接连上去，不报"端口被占用"，退出时也不会去杀一个不是它启动的
进程。这个区分由 `backend.rs` 里的 `OWNS_BACKEND` 记录。

**先显示启动页，再导航到面板。** 后端冷启动要读配置、校验凭据、拉模型目录，是
秒级到数十秒的耗时。窗口直连面板的结果是白屏或"无法连接"。启动页把这段等待显式化，
失败时给出可复制的错误文本与日志路径，而不是让用户面对一个没有信息的空白窗口。

## 构建

前置：Rust 工具链。本项目把它装在 `.toolchain/` 内（与 Go 工具链并列），
不污染用户环境。路径按脚本位置推导，所以 clone 到任何目录都不用改：

```bash
source desktop/scripts/env.sh    # 设好 RUSTUP_HOME / CARGO_HOME / GOCACHE / PATH
```

`env.sh` 会自动把 `.toolchain/` 转成 Windows 形态的路径（Rust/Go 是原生程序，
认不了 MSYS 的 `/d/...`），并把补齐过的 self-contained 工具集加进 `PATH`。
它内部按 `uname` 分平台：Linux / macOS 走普通 Unix 路径，下面那些 mingw /
self-contained / dlltool 的修补是 `x86_64-pc-windows-gnu` 专属问题，不会生效。
两种平台都把 `CARGO_TARGET_DIR` 固定到 `.toolchain/cargo-target`。

> **依赖源注意事项**：Rust 工具链使用 `x86_64-pc-windows-gnu`，rustup 自带的
> self-contained 链接器（`ld.exe` / `x86_64-w64-mingw32-gcc.exe`）使构建**不需要
> Visual Studio Build Tools**。crates 源已在 `.toolchain/cargo/config.toml` 中
> 指向 rsproxy 镜像 —— 本机网络下官方 CDN 会被中间层返回「大小相同但内容不同」的
> 替换文件（实测 rustup-init.exe 官方 sha256 与实际内容不一致），镜像经交叉校验可信。

出可执行文件（调试）：

```bash
cd desktop/src-tauri && cargo build
```

出安装包 —— 三个平台各一个入口，都是一步到位，产物落在 `dist/`：

```bash
./installer/windows/build-desktop-setup.sh    # → workbuddy-gateway-desktop_<v>_x64-setup.exe
./installer/macos/build-desktop-pkg.sh        # → workbuddy-gateway-desktop_<v>_arm64.dmg
./installer/linux/build-desktop-deb.sh        # → workbuddy-gateway-desktop_<v>_amd64.deb
```

每个脚本做的都是同一件事：解出 `dist/<os>_<arch>/` 的 Go 二进制 →
`prepare-resources.mjs` 把它们复制进 `src-tauri/bin/` → `tauri build`（Tauri 按平台自动
叠加 `tauri.<os>.conf.json`）→ 把产出的安装包按 `dist/` 的命名习惯复制一份。

平台差异全部按分支处理，没有复制第二套代码：

| 关注点 | Windows | macOS | Linux |
| --- | --- | --- | --- |
| 打包目标 | `nsis` + `tauri.windows.conf.json` | `tauri.macos.conf.json` 的 `dmg` | `tauri.linux.conf.json` 的 `deb` |
| WebView | WebView2 | 系统 WKWebView | 系统 WebKitGTK |
| 额外资源 | **仅 gnu 目标**需要 `WebView2Loader.dll` | 不需要 | 不需要 |
| Go 产物名 | `workbuddy-gateway.exe` | `workbuddy-gateway` | `workbuddy-gateway` |
| 数据目录 | `%LOCALAPPDATA%\wb-gateway` | `~/.wb-gateway` | `~/.wb-gateway` |
| 壳的资源目录 | exe 同目录（+ `bin/`） | `Contents/Resources/`（+ `bin/`） | `/usr/lib/WorkBuddy Gateway/`（+ `bin/`） |

最后一行是三个平台里最容易踩的坑：**只有 Windows** 的资源与壳在同一目录下，macOS 的
`.app` 与 Linux 的 deb 都把资源放到与可执行文件**不同级**的地方。所以
`backend.rs::locate()` 必须额外向 Tauri 要 `resource_dir()`（`PackageInfo.name` 就是
productName），只靠「exe 同目录 / 上一级」这类相对路径永远找不到 —— 症状是装完打开就报
「未找到 workbuddy-gateway」。

## Windows：MSVC 与 windows-gnu 两条路

同一个平台有两套可行工具链，本仓库两条都保留：

| | MSVC（CI / 一般机器） | windows-gnu（作者本机） |
| --- | --- | --- |
| 入口 | `installer/windows/build-desktop-setup.sh` | `scripts/build-desktop.sh` |
| 前置 | VS Build Tools（GitHub runner 自带） | `desktop/scripts/setup-rust-toolchain.sh` 补齐 mingw |
| `WebView2Loader.dll` | **静态链接，不需要** | **动态链接，必须随包分发** |
| 额外产物 | — | 绿色版 `*-portable.zip`（`scripts/package-desktop.py`） |

作者本机走 gnu 是因为其网络下 aka.ms（VS Build Tools 引导器）不可达 —— 那是本机网络
问题，CI 的 runner 上不存在，所以 CI 走 MSVC 更省事。

这个差异会直接影响打包配置：`tauri.windows.conf.json` 里为 gnu 显式声明了
`WebView2Loader.dll` 这个资源，而 MSVC 下该文件不存在，会让打包直接失败。所以 MSVC
那条路用 `--config` 把 `bundle.resources` 覆盖成只有 `bin/*`（实测确认 Tauri 的
`--config` 对数组是**替换**而非合并，这条覆盖才真正生效）。`prepare-resources.mjs`
也据此按 `rustc -vV` 的 host 分叉：gnu 缺 DLL 硬失败，msvc 直接跳过。

## Windows 安装包：本仓库自带一份 NSIS 模板

`src-tauri/nsis/installer.nsi` 是 Tauri 模板的副本，`tauri.conf.json` 用
`bundle.windows.nsis.template` 指过去。抄一份是为了加两处补丁，都只服务「Windows 上
手动双击安装包升级」这条路：

**一、残键守卫**（`PageReinstall`）。**注册表里留着旧安装的记录、文件却已经被删掉**时
（手工删了目录、或卸载/更新中途被掐断），上游模板只看注册表就判定「已安装」，摆出
「安装前卸载 / 请勿卸载」这页选择题 —— 而第一条必然失败，只弹一句「无法卸载！」再退回
本页，用户多半不知道改选第二项就能继续。守卫判据保守：记录给了安装位置、而该位置下
主程序已不在 → 删掉残键、跳过本页按全新安装继续；主程序还在的旧安装一律不碰。

**二、卸载前先结束占用文件的进程**（`PageLeaveReinstall` 的 `reinst_uninstall`）。安装包
自带的进程检查只关**壳**；壳拉起的 `bin\workbuddy-gateway.exe` 会变成孤儿继续锁着文件，
旧卸载器删不掉它们 → 同样只弹「无法卸载！」（开着软件双击安装包时必然踩到）。安装段里
本来就有这次清理，但它跑在旧卸载器**之后**，救不了这一步；补丁把它复用（同一个
`WB_STOP_INSTALLED_PROCESSES` 宏）提前到旧卸载器之前，幂等。

**三、静默调用旧卸载器，且不追加 `_?=<安装目录>`**（`PageLeaveReinstall` 的
`reinst_uninstall`）。上游按 NSIS 惯例走「就地卸载」，但本机实测这条路**必然失败**：
同一份卸载器、同一份目录，
带 `_?=` 时退出码 `-1`、一个文件都不删、注册表也不动（安装器于是只弹「无法卸载！」）；
不带 `_?=` 的**交互**调用更隐蔽 —— 父进程 3 秒内返回 0 却什么都没删（临时副本还没
干活，安装器的检查已经判失败）；而 `uninstall.exe /S` 会退出码 `0`、目录清空、
A/R 键删除、数据目录保留。补丁因此显式加 `/S`：静默卸载跑完才返回。

网关侧还有第三条防线（`internal/update/uninstall_entry.go`）：面板点的「安装并重启」会在
拉起向导前先清掉同样的残键，于是向导根本不会摆出那页。

**同步规矩**：模板与 `@tauri-apps/cli` 必须同版本 —— 前者抄自后者的
`crates/tauri-bundler/src/bundle/windows/nsis/installer.nsi`，所以 `package.json`
里把它钉死了（不用 `^2`）。升级 CLI 时按 `nsis/installer.nsi` 文件头的四步走：取回
新模板、合并上游改动、两个 package 文件一起改版本、最后触发一次
`workflow_dispatch` 的 windows 桌面包构建 —— 模板语法错误只有 makensis 能发现，
CI 是唯一会编译它的地方。

## 运行期布局

安装后（Windows，每用户安装）：

```
%LOCALAPPDATA%\WorkBuddy Gateway\
  WorkBuddy Gateway.exe     壳
  bin\workbuddy-gateway.exe 网关后端
  bin\wb-local-agent.exe    本机代理
```

数据目录固定在 `%LOCALAPPDATA%\wb-gateway`（与安装包、npm 入口一致），
因此**桌面版、命令行版、浏览器版看到的是同一份账号池**，不会各存一份。
壳会把后端的 stdout/stderr 追加到 `%LOCALAPPDATA%\wb-gateway\logs\gateway.log`：
壳是托盘应用、没有控制台，后台启动失败若没有任何落盘日志将无法排查。

安装后（Linux，deb）：

```
/usr/bin/workbuddy-gateway-desktop          壳（菜单项「WorkBuddy Gateway」）
/usr/lib/WorkBuddy Gateway/bin/workbuddy-gateway  网关后端
/usr/lib/WorkBuddy Gateway/bin/wb-local-agent     本机代理
/usr/share/applications/WorkBuddy Gateway.desktop 菜单项
/usr/share/icons/hicolor/256x256/apps/workbuddy-gateway-desktop.png 图标
```

Linux 的数据目录是 `~/.wb-gateway`（`backend.rs::base_data_dir` 取 `$HOME`，
Windows 才取 `%LOCALAPPDATA%`），日志同样在 `<数据目录>/logs/gateway.log`。
服务端包（`installer/linux/build-deb.sh`）把网关装到 `/usr/bin/workbuddy-gateway`
并配 systemd 单元，两者 dpkg 包名不同、可以共存，但共用 8317 端口。

## 已知取舍

- **未做 Job Object**：壳若被强杀，它拉起的 Go 进程会短暂成为孤儿。兜底手段是
  「复用已有实例」逻辑 —— 下次启动会直接接上而不是抢端口；正常退出路径
  （托盘退出、窗口关闭、`RunEvent::Exit`）都会收掉子进程。
- **未签名**：安装包没有代码签名，SmartScreen 会提示，与现有安装包一致。
- **未接自动更新**：Tauri updater 需要签名密钥与发布端点，暂未配置。
