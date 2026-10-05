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

> **依赖源注意事项**：Rust 工具链使用 `x86_64-pc-windows-gnu`，rustup 自带的
> self-contained 链接器（`ld.exe` / `x86_64-w64-mingw32-gcc.exe`）使构建**不需要
> Visual Studio Build Tools**。crates 源已在 `.toolchain/cargo/config.toml` 中
> 指向 rsproxy 镜像 —— 本机网络下官方 CDN 会被中间层返回「大小相同但内容不同」的
> 替换文件（实测 rustup-init.exe 官方 sha256 与实际内容不一致），镜像经交叉校验可信。

出可执行文件（调试）：

```bash
cd desktop/src-tauri && cargo build
```

出安装包（NSIS，每用户安装、免 UAC）：

```bash
node desktop/scripts/prepare-resources.mjs   # 先构建 Go 侧二进制
cd desktop/src-tauri && cargo tauri build    # 需要 @tauri-apps/cli
```

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

## 已知取舍

- **未做 Job Object**：壳若被强杀，它拉起的 Go 进程会短暂成为孤儿。兜底手段是
  「复用已有实例」逻辑 —— 下次启动会直接接上而不是抢端口；正常退出路径
  （托盘退出、窗口关闭、`RunEvent::Exit`）都会收掉子进程。
- **未签名**：安装包没有代码签名，SmartScreen 会提示，与现有安装包一致。
- **未接自动更新**：Tauri updater 需要签名密钥与发布端点，暂未配置。
