# WorkBuddy Gateway

把 [CangShui/workbuddy-gateway](https://github.com/CangShui/workbuddy-gateway)、
[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)、
[changexbc/workbuddy-switch](https://github.com/changexbc/workbuddy-switch) 合并为一个项目：
一个 Go 网关主进程 + 一套 React 管理面板 + 一个可选的本机代理进程。

网关对外提供 OpenAI 与 Anthropic 两种兼容端点，把请求分发到账号池中的上游账号，
并附带选号调度、账号治理、模型目录与倍率、用量统计、任务体系与桌面应用。

设计文档见仓库根目录的 `三合一可行性分析与项目说明.md`，其中「零、已拍板的技术决策」记录了七条已定方向。

## 已定方向（摘要）

1. 反代层以 wb-gateway 的实现为基准（双站反代、模型完全透传、模型目录双源 + 倍率实测、
   流式分片规范化、tool_call 序列自愈、响应头/流中空闲分段超时、`/v1/responses`）。
2. 治理与面板来自 wb2api-panel（三因子加权选号、熔断、冷却、在途租约、降权、会话粘性、
   定时任务、成长任务体系、提示词体系、指纹脱敏、在线配置、分频道日志、realm 路由）。
3. 前端视觉与组件体系参考 wb-switch（设计令牌、明暗双主题、shadcn 风格组件、recharts 图表）。
4. 本机能力（本机客户端切号、会话复制、本地日志与限额台账）保留为独立的本机代理进程，由 Go 网关代管生命周期。
5. 桌面形态参考 wb-switch 建 Tauri 薄壳（桌面 App + 浏览器双形态）。
6. 提示词默认 append，custom 作为「安全优先」档位，passthrough 收进高级选项。
7. 功能与前端同步交付，按纵向切片推进。

## 功能概览

- **兼容端点**：`POST /v1/chat/completions`（流式 + 非流式）、`POST /v1/responses`、
  `POST /v1/messages`（Anthropic Messages API，Claude Code 可直接接入）、
  `POST /v1/messages/count_tokens`、`GET /v1/models`、`GET /healthz`、`GET /status`。
- **账号池与治理**：统一凭据模型（兼容两种历史格式）、三因子加权选号、熔断、软/硬冷却、
  连败降权、在途租约、会话粘性、令牌自动刷新、凭据热加载。
- **模型与倍率**：目录双源合并（实时接口 + npm 静态目录），余额差分价格探测，
  模型黑白名单与「模型专属账号名单」。
- **双站路由**：国内站 / 国际站软优先（免费优先），也支持显式指定（`CN-` / `AI-` 模型名前缀、
  `?site=` 参数或 `X-WB-Site` 请求头）。
- **任务体系**：签到、令牌保活、额度刷新、任务中心（成长任务扫描与执行）、夜猫子。
- **统计与日志**：按小时聚合的持久化用量统计、请求级 trace、脱敏的请求归档。
- **管理面板**：账号、监控、模型与档位、双站视图、任务中心、日志、用量统计、配置等页面，
  治理参数保存即热生效。
- **部署形态**：命令行二进制、Docker、systemd、npm 全局安装、各平台安装包，以及桌面应用。

完整的实现进度与设计取舍见 [PROGRESS.md](PROGRESS.md)。

## 安装方式

四条路，装的是同一个网关。除容器外都不需要管理员权限。

**零、桌面应用（Windows / macOS / Linux）**

三个平台各有一个**带原生窗口 + 托盘**的桌面版。窗口里承载的就是那套 React 面板，
与命令行版、浏览器版是同一份界面、同一份账号池。关闭窗口 = 收进托盘（网关继续常驻），
真正的退出走托盘菜单的「退出」；托盘菜单还提供显示面板 / 在浏览器打开 / 重启网关 /
**开机自启动** / 打开数据目录 / 退出。

| 平台 | 产物 | 安装位置 | 数据目录 |
| --- | --- | --- | --- |
| Windows x64 | `workbuddy-gateway-desktop_<版本>_x64-setup.exe` | `%LOCALAPPDATA%\WorkBuddy Gateway`（每用户、免 UAC） | `%LOCALAPPDATA%\wb-gateway` |
| macOS Apple Silicon | `workbuddy-gateway-desktop_<版本>_arm64.dmg` | 拖进 `/Applications` | `~/.wb-gateway` |
| Linux x86_64 | `workbuddy-gateway-desktop_<版本>_amd64.deb` | `/usr/bin` + `/usr/lib/WorkBuddy Gateway/` | `~/.wb-gateway` |

构建入口（三个都需要 Rust 工具链 —— 桌面壳是 Tauri 应用，三个平台各绑一套 WebView，
**无法交叉编译**）：

```bash
./installer/windows/build-desktop-setup.sh    # 需 MSVC
./installer/macos/build-desktop-pkg.sh        # 只能在 macOS 上跑
./installer/linux/build-desktop-deb.sh        # 需 WebKitGTK 开发包
```

打 tag 时 `.github/workflows/release.yml` 会用三个平台的 runner 各构建一次，只把这三种
窗口版包发到 Release。

**桌面版与服务端包是两件事**：服务端包（`workbuddy-gateway_<版本>_amd64.deb`）装的是
systemd 服务、只有浏览器访问、**没有窗口**；两者 dpkg 包名不同
（`work-buddy-gateway` / `workbuddy-gateway`），可以共存，但共用 8317 端口，因此只能有
一个设置开机自启。

> **Windows 有两条工具链路线**：CI 与一般机器走 **MSVC**（上表那个脚本）；
> 作者本机因 aka.ms 不可达而走 **windows-gnu**（`scripts/build-desktop.sh` +
> `desktop/scripts/setup-rust-toolchain.sh` 的工具链修补，另出绿色版
> `workbuddy-gateway-desktop_<版本>_x64.exe`）。两条路对 `WebView2Loader.dll`
> 的要求正好相反，细节见 `desktop/README.md`。
>
> GNOME 下托盘图标需要 AppIndicator 扩展；没装扩展时窗口一切正常，只是没有托盘图标。

**开机自启动默认关闭**，两个地方可以开：托盘菜单的「开机自启动」勾选项，或面板
「配置 → 开机自启动」的开关（桌面版、命令行版、容器版的面板里都有这一项，读的是
系统里的真实自启状态）。

**自启一律静默**：自启项里带着 `--silent`，桌面壳被它拉起时**不显示面板窗口**，
只把图标放进托盘（想用面板时点托盘图标，或从开始菜单 / 应用菜单再启动一次 ——
第二次启动会把已在运行的实例的窗口拉到前面）。理由很直接：用户关机时并没有开着这个
窗口，开机却弹出一个面板，属于每次开机都要手动关掉的骚扰。

> 旧版本写入的自启项没有这个参数（面板会显示「但开机时会弹出面板窗口」并给出提示）：
> 把开关关掉再打开一次即可重写成静默形态。各平台落点与上面列的一致，
> 命令行形态的宿主是 `launch-hidden.vbs`，它本身就是隐藏启动。

> ⚠️ 桌面版与命令行安装包**共用 8317 端口**，因此只能有一个设置自启。若系统里已存在
> 命令行版的计划任务 `WorkBuddyGateway`，开启时会被拦下并提示二选一，不会让两个进程
> 在登录时抢端口（那会表现为「有时能连上、有时连不上」）。
>
> 各平台的自启落点：Windows 写注册表 `HKCU\...\Run` 的 `WorkBuddyGatewayDesktop`
> 值；macOS 写 `~/Library/LaunchAgents/com.workbuddy.gateway.desktop.plist`；
> Linux 写 `~/.config/autostart/workbuddy-gateway-desktop.desktop`。
> 都指向**桌面壳**而不是网关本身 —— 直接自启 `workbuddy-gateway.exe serve` 会得到一个
> 没有界面、用户无处关闭的后台进程。

**一、平台安装包**（双击安装，最省事）

| 平台 | 文件 | 数据目录 |
| --- | --- | --- |
| Windows x64 / ARM64 | `workbuddy-gateway_<版本>_x64-setup.exe` / `_arm64-setup.exe` | `%LOCALAPPDATA%\wb-gateway` |
| Linux x64 / ARM64 | `workbuddy-gateway_<版本>_amd64.deb` / `_arm64.deb` | `/var/lib/workbuddy-gateway` |
| macOS Intel / Apple Silicon | `workbuddy-gateway_<版本>_amd64.pkg` / `_arm64.pkg`（含 `.dmg`） | `~/.wb-gateway` |

Windows 安装包会问两个问题（是否随登录启动、是否装完打开面板），并创建开始菜单/桌面快捷方式与
卸载项；Linux 装成 systemd 服务；macOS 装一个每用户 LaunchAgent。安装包均**未签名**，
首次运行需要手动允许（详见 `DEPLOY.md`）。

**二、npm 全局安装**

```bash
npm i -g workbuddy-gateway
workbuddy-gateway          # 起服务并自动打开面板
workbuddy-gateway status   # 其余子命令原样透传
```

二进制通过 npm 平台包分发（`optionalDependencies`），不依赖 GitHub 可达，
npmmirror 镜像也能装。数据目录在 Windows 上取 `%LOCALAPPDATA%\wb-gateway`、
其它平台取 `~/.wb-gateway` —— 与桌面版、平台安装包落在同一处，三个入口共用同一份账号池。

> 数据目录一律优先取环境变量 `WB_GATEWAY_DATA_DIR`（桌面壳就是用它把后端固定到
> `%LOCALAPPDATA%\wb-gateway` 的），其次才是启动时所在目录。凭据扫描工作目录下的
> `workbuddy*.json`，返回的是**带工作目录的完整路径**，所以「进程 CWD」和「工作目录」
> 可以不一致 —— 桌面壳启动后端时这两者本来就是分开的。

**三、免安装压缩包**

解压即用：`./workbuddy-gateway serve`。包内含网关、可选的本机代理、配置样例、文档与部署脚本。

> 与 switch 的差异：switch 是 Tauri 桌面应用（`.app` / 托盘 / 自带自动更新）。
> 本项目的桌面形态已交付（`desktop/`，Windows 产出 NSIS 安装包，托盘常驻、关闭收进托盘；
> macOS/Linux 仍走各平台服务化安装路径）。尚未实现的只有代码签名与自动更新。
> 完整对照见 `DEPLOY.md` 第零节。

## 快速开始

### 准备凭据

把凭据文件放进工作目录（自动发现 `workbuddy*.json`），或用 `-auth` / `-auth-dir` 指定。
凭据格式为任一种历史格式（见 [PROGRESS.md](PROGRESS.md) 的「统一凭据模型」）。

### 构建

```bash
# 完整构建（前端 + 后端）
bash scripts/build.sh

# 只构建后端（web/dist 已存在时）
bash scripts/build.sh go
```

前端独立开发：

```bash
cd web
npm install
npm run dev      # http://localhost:5173，/panel/api 代理到 127.0.0.1:8317
npm run build    # 产出 web/dist，随后 go build 会把它打进二进制
```

### 运行

```bash
./workbuddy-gateway.exe serve                 # 默认 127.0.0.1:8317
./workbuddy-gateway.exe serve -port 9000 -api-key sk-xxx
./workbuddy-gateway.exe status                # 只看账号池状态，不启服务
```

面板地址：`http://127.0.0.1:8317/panel/`

### 客户端接入

```bash
curl -N http://127.0.0.1:8317/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"hy3","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:8317/v1", api_key="none")
resp = client.chat.completions.create(model="hy3", messages=[{"role": "user", "content": "你好"}])
print(resp.choices[0].message.content)
```

接入 Claude Code：把 `ANTHROPIC_BASE_URL` 指向 `http://127.0.0.1:8317`、
`ANTHROPIC_API_KEY` 设成网关的 api-key（或留空）即可。

## 配置

运行时配置见 [CONFIG.md](CONFIG.md)。工作目录下的 `config.json` 覆盖运行层字段，
可复制 `config.example.json` 起步。

## 安全提示

- `workbuddy*.json` 含真实访问凭据，**严禁提交到 Git 或公开分享**（`.gitignore` 已排除）。
- 默认只监听 `127.0.0.1`。需要局域网/公网访问时改 `-addr 0.0.0.0` 并**务必**设置 `-api-key`，或置于反向代理之后。
- 本项目是非官方网关，使用上游账号涉及目标平台服务条款与封号风险；仅限本人授权账号、本机或私有环境使用。

## 目录结构

```
main.go                      CLI 入口（serve / status / version / help）
cmd/wb-local-agent/          本机代理（可选产物，只读地读本机客户端数据）
internal/config              配置加载（命令行 + config.json，支持分段热改补丁）
internal/auth                统一凭据模型（edition / realm 双格式兼容）
internal/pool                账号池（加权选号、治理、令牌刷新、热加载）
internal/upstream            反代层（站点 Profile、对话转发、令牌刷新、错误分类、成长域、官方用量）
internal/outbound            出站改写管线（提示词三模式、脱敏、工具序列自愈、思维链）
internal/catalog             模型目录（双源合并 + 缓存）与价格探测
internal/session             会话粘性
internal/scheduler           定时任务（签到 / 保活 / 额度刷新 / 任务中心 / 夜猫子）
internal/tasks               任务中心（成长任务扫描、分类、执行队列）
internal/stats               用量统计（按小时聚合持久化）
internal/metrics             滚动窗口指标
internal/eventlog            事件日志环形缓冲
internal/localagent          本机代理代管器（发现 / 拉起 / 探活 / 反代）
internal/update              检查更新（GitHub Release 探测、版本比较、安装包下载）
internal/server              HTTP 层（OpenAI 兼容端点 + 面板 API + 静态资源）
web/                         React 前端（构建产物 web/dist 由 web/embed.go 嵌入）
scripts/build.sh             日常构建
scripts/release.sh           多平台发行打包
deploy/                      systemd 单元与安装脚本、Windows 计划任务脚本
Dockerfile / docker-compose.yml
DEPLOY.md                    部署文档（二进制 / systemd / Windows / Docker）
PROGRESS.md                  实现进度与功能说明
CONFIG.md                    运行时配置说明
三合一可行性分析与项目说明.md  设计文档与切片进度
repos/                       三个上游项目的只读克隆（不参与构建）
```
