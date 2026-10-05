# 部署 WorkBuddy 网关

## 零、先选安装方式

三条路，按你的习惯挑一条即可。三条装的都是同一个二进制，只是"怎么装上去"不同。

**一、平台安装包**（最接近普通软件，双击即可）

| 平台 | 文件 | 装到哪 | 数据在哪 |
| --- | --- | --- | --- |
| Windows x64 / ARM64 | `workbuddy-gateway_<版本>_x64-setup.exe` | `%LOCALAPPDATA%\Programs\workbuddy-gateway` | `%LOCALAPPDATA%\wb-gateway` |
| Linux x64 / ARM64 | `workbuddy-gateway_<版本>_amd64.deb` | `/usr/bin` + systemd 服务 | `/var/lib/workbuddy-gateway` |
| macOS Intel / Apple Silicon | `workbuddy-gateway_<版本>_amd64.pkg`（含 `.dmg`） | `/usr/local/bin` + 每用户 LaunchAgent | `~/.wb-gateway` |

- Windows：双击 → 回答两个问题（是否随登录启动、是否装完打开面板）→ 完成。
  开始菜单与桌面会各有一个快捷方式；卸载走「设置 → 应用」或 `uninstall.ps1`。
  无需管理员权限（每个用户装在自己的目录里）。
- Linux：`sudo dpkg -i workbuddy-gateway_<版本>_amd64.deb`，装完自动启用并启动服务。
  `dpkg -r` 卸载但**保留**数据目录，`dpkg --purge` 才连数据一起删。
- macOS：双击 `.pkg`。会在 `~/Library/LaunchAgents` 装一个 LaunchAgent（登录自启），
  并创建 `~/.wb-gateway` 作为数据目录。

三个平台的安装包都**未做代码签名**，Windows SmartScreen 与 macOS Gatekeeper 会提示来源未知。
这与同类小工具（包括 switch）的情况一致，需手动允许。macOS 上如果提示"已损坏"：

```bash
xattr -d com.apple.quarantine workbuddy-gateway_<版本>_arm64.pkg
```

### 开机自启动在各平台的落点

网关本身不做自启，需要宿主（桌面壳）来管它的生命周期。三种形态的落点：

| 形态 | Windows | macOS | Linux |
| --- | --- | --- | --- |
| 桌面版 | 注册表 `HKCU\...\Run` 的 `WorkBuddyGatewayDesktop` 值 | `~/Library/LaunchAgents/com.workbuddy.gateway.desktop.plist` | `~/.config/autostart/workbuddy-gateway-desktop.desktop` |
| 命令行安装包 | 计划任务 `WorkBuddyGateway`（`install.ps1 -AutoStart`） | 同上的 LaunchAgent | systemd `enable` |

**桌面版默认不开自启**，可以在两个地方打开（两者操作同一条记录）：

- 托盘菜单 → 「开机自启动」
- 面板 → 配置 → 开机自启动

面板里的开关**在桌面版、命令行版、容器版里都有**，读的是系统里的真实自启状态
（注册表 / plist / .desktop），不依赖桌面壳。

> ⚠️ 桌面版与命令行安装包**共用 8317 端口**，因此只能有一个设置自启。若系统里已存在
> 命令行版的计划任务 `WorkBuddyGateway`，面板或托盘开启时会被拦下并提示二选一。
> 两个同时自启的表现是「有时能连上、有时连不上」，属于最难排查的一类故障。

自启项指向的是**宿主**（桌面壳 / `launch-hidden.vbs`），**不是网关自己** ——
直接自启 `workbuddy-gateway.exe serve` 会得到一个没有界面、用户无处关闭的后台进程。

**二、npm 全局安装**（给习惯 npm 的人，也是三条里唯一能一行搞定的）

```bash
npm i -g workbuddy-gateway
workbuddy-gateway          # 起服务并自动打开面板
workbuddy-gateway status   # 也可以只跑子命令
```

平台二进制通过 npm 的 `optionalDependencies` 分发（主包 + `workbuddy-gateway-<platform>-<arch>`
平台包），**不依赖 GitHub 可达**，国内镜像（npmmirror）也能装。

数据目录固定为 `~/.wb-gateway`（不是当前目录——全局安装后你会在任意目录执行命令，
拿当前目录当数据目录会把凭据写到"你当时恰好 cd 到哪儿"）。可用 `WB_GATEWAY_DATA_DIR` 覆盖。

**三、免安装压缩包**

`workbuddy-gateway_<版本>_<os>_<arch>.tar.gz`（Windows 另有 `.zip`）解压即用：

```bash
tar -xzf workbuddy-gateway_<版本>_linux_amd64.tar.gz
cd linux_amd64 && ./workbuddy-gateway serve
```

包内含两个二进制（网关 + 可选的本机代理）、配置样例、文档，以及该平台对应的部署脚本。

### 与 switch 的差异（如实说明）

switch 是 Tauri 桌面应用，它的安装包里有 `.app` / 图形界面 / 托盘 / 自带自动更新，
macOS 是"把图标拖进应用程序"。本项目的界面是**浏览器里的面板**，桌面壳（切片 9）还没做，
所以：

- macOS 产出的是 `.pkg`（装命令行网关 + LaunchAgent），不是 drag-to-Applications 的 `.dmg` 应用；
- 三个平台都走"浏览器打开面板"，没有托盘图标；
- 安装包不做自动更新，升级方式见第六节。

等桌面壳落地后，macOS 的 dmg 与 Windows 安装包会换成带图标与自动更新的形态。

---

## 一、直接跑二进制（压缩包方式的细节）

先记住一件会影响判断的事：**网关启动时会先同步拉一次模型目录，然后才开始监听端口**。
所以进程起来后最初约 30 秒内 `/healthz` 会连接拒绝。这是刻意取舍（否则客户端在启动瞬间
调 `/v1/models` 会拿到空列表），不是启动失败，健康检查和 systemd 的 `Restart` 都要按这个来配。

发行包解压后有两个文件（Windows 是 `.exe`）：

```
workbuddy-gateway      # 网关，面板已内嵌在这一个文件里
wb-local-agent         # 本机代理，可选；只在「本机形态」下有用
config.example.json    # 配置样例
```

```bash
./workbuddy-gateway serve            # 默认 127.0.0.1:8317
./workbuddy-gateway serve --addr 0.0.0.0 --port 8317 --api-key <你的密钥>
./workbuddy-gateway status           # 只打印一次账号池状态后退出
./workbuddy-gateway version
```

打开 `http://127.0.0.1:8317/panel/` 就是面板。

凭据放哪儿：默认在工作目录里自动发现 `workbuddy*.json`；也可以用 `--auth <文件>`
或 `--auth-dir <目录>` 指定。想省事的做法是直接在面板上点「添加账号」走一遍 OAuth，
凭据会自己落盘并热加载进池。

### 几个值得知道的参数

| 参数 | 说明 |
| --- | --- |
| `--addr` | 监听地址，默认 `127.0.0.1`。改成 `0.0.0.0` 就对外暴露了，见下面的安全提醒 |
| `--port` | 端口，默认 8317 |
| `--api-key` | 访问密钥。设了之后面板与 `/v1/*` 都要带 `Authorization: Bearer <key>` |
| `--auth-dir` | 凭据目录（推荐用于服务器：数据与二进制分开） |
| `--proxy` | 上游请求走代理 |
| `--reload-interval` | 凭据热加载扫描间隔（秒），默认 5 |
| `--models-refresh` | 模型目录刷新间隔（分钟） |
| `--verbose` | 详细日志 |

工作目录下的 `config.json` 是运行期配置（面板上改的东西写这里），
与启动参数是两层：**启动参数管装配期字段（端口、超时、代理），`config.json` 管可热改字段
（治理、调度、提示词、站点价格倾斜）**。只有启动参数覆盖不了的部分才需要重启。

### 安全提醒

网关自己不做 TLS、也不做反向代理。要对外提供服务，二选一：

- 只绑宿主回环：`--addr 127.0.0.1`，前面用 nginx/Caddy 转发并终止 TLS；
- 或者绑 `0.0.0.0` 但**必须**设 `--api-key`。

两样都不做等于把面板（含账号管理与凭据操作）裸奔在网络上。

---

## 二、Linux：systemd

```bash
tar -xzf workbuddy-gateway_<版本>_linux_amd64.tar.gz
cd workbuddy-gateway_<版本>_linux_amd64
sudo ./install-systemd.sh
```

脚本做的事（幂等，重复执行只覆盖二进制并重启）：

1. 建系统用户 `wb-gateway` 与数据目录 `/var/lib/wb-gateway`
2. 安装二进制到 `/opt/wb-gateway`
3. 装 `wb-gateway.service` 并 `enable --now`

常用命令：

```bash
journalctl -u wb-gateway -f          # 看日志
systemctl restart wb-gateway         # 重启
systemctl stop wb-gateway            # 停止
```

单元文件里已经配好的几处，都是有理由的，改之前建议先读注释：

- `After=network-online.target`：网络没起来时目录首拉会退化到缓存，日志里出现一条可误读的告警。
- `Restart=always` + `RestartSec=60`：如果是配置写错导致启动即退，高频重启只会把日志刷满。
- `KillSignal=SIGTERM` + `TimeoutStopSec=20`：优雅退出会先关停本机代理（若在跑），再刷一次统计落盘。
  直接 `kill -9` 会丢掉最后一分钟的统计（统计数据可再生，不算严重，但没必要）。
- `ProtectSystem=full` / `ProtectHome=true` / `ReadWritePaths=/var/lib/wb-gateway`：最小权限。
  注意 `ProtectHome=true` 会让网关看不见 `/home` 下的凭据文件，所以数据目录必须在
  `/var/lib/wb-gateway`（默认就是）。

## 三、Windows：计划任务

```powershell
powershell -ExecutionPolicy Bypass -File .\install-windows.ps1
```

默认装到 `%LOCALAPPDATA%\wb-gateway\bin`，数据目录 `%LOCALAPPDATA%\wb-gateway`，
并注册一个**登录时触发**的计划任务。

```powershell
powershell -ExecutionPolicy Bypass -File .\install-windows.ps1 -Port 9000 -ApiKey "sk-xxx"
Start-ScheduledTask    -TaskName WorkBuddyGateway
Stop-ScheduledTask     -TaskName WorkBuddyGateway
Unregister-ScheduledTask -TaskName WorkBuddyGateway -Confirm:$false   # 卸载
```

为什么用计划任务而不是 Windows 服务：网关要读写**当前用户**目录下的凭据、缓存与统计。
LocalSystem 服务的家目录和账户都不一样，账号池会看起来是空的。登录触发在当前用户下跑，
路径与权限跟手工执行完全一致。

## 四、Docker

```bash
# 1) 构建（会依次跑前端构建、Go 编译、最小运行时三阶段）
docker build -t wb-gateway:latest .

# 2) 运行
docker run -d --name wb-gateway \
  -p 127.0.0.1:8317:8317 \
  -v wb-gateway-data:/app/data \
  wb-gateway:latest
```

要访问密钥就加一行环境变量之外的东西 —— 注意 `--api-key` 是命令行参数，不是环境变量：

```bash
docker run -d --name wb-gateway \
  -p 127.0.0.1:8317:8317 \
  -v wb-gateway-data:/app/data \
  wb-gateway:latest serve --addr 0.0.0.0 --port 8317 --api-key <你的密钥>
```

镜像里的 `CMD` 已经是 `serve --addr 0.0.0.0 --port 8317`，所以上面直接追加参数即可
（不要重复写 `serve`）。更推荐的做法是**只发布到宿主回环**
（`-p 127.0.0.1:8317:8317`），这样即使忘了设密钥也不会暴露到网络。

要点：

- **必须挂卷到 `/app/data`**。凭据、`config.json`、模型缓存、统计全在里面，
  不挂卷容器一重建账号池就空了。
- 镜像**不含**本机代理，这是刻意的：本机代理要读写宿主机的客户端配置与会话库，
  容器里没有这些东西可读。网关找不到它时会静默跳过，面板上的「本机客户端」入口自动隐藏。
- 健康检查打的是 `/healthz`，这个路径在鉴权中间件里被显式豁免，所以设了密钥也能过。
  但 `start-period` 给了 40 秒，原因就是开头说的目录首拉。

### docker compose

```yaml
services:
  wb-gateway:
    build: .
    image: wb-gateway:latest
    container_name: wb-gateway
    restart: unless-stopped
    ports:
      - "127.0.0.1:8317:8317"
    volumes:
      - wb-gateway-data:/app/data
    # 时间相关的定时任务（签到、保活、任务中心）按本地整点排程，
    # 容器里务必把时区设对，否则「9 点签到」会在错误的时刻触发。
    environment:
      - TZ=Asia/Shanghai

volumes:
  wb-gateway-data:
```

---

## 五、本机代理（可选）

本机代理给面板提供「本机客户端」页：只读地看本机客户端版本、数据目录占用、
会话/项目/技能计数、日志尾部，以及客户端里注册的钩子。

它由网关**代管生命周期**，你不用手动起：

1. 网关按固定顺序找二进制：与网关同目录 → `./bin` → `~/.wb-gateway/bin`
2. 找不到就静默跳过（服务端部署的正常形态），`capabilities.local = false`，前端隐藏入口
3. 找到就拉起子进程，端口让子进程自选（避免「挑完端口到绑定之间被占用」的竞态），
   靠子进程 stdout 的 `LISTENING <addr>` 一行判断就绪
4. 每 10 秒探活；异常退出按 1s→60s 有界退避重启，稳定跑满一分钟则重置退避
5. 网关退出时先关子进程 stdin（而不是发信号，Windows 上对无控制台子进程发信号不可靠），
   最多等 5 秒再强制结束

安全上有两条硬约束，都不建议改：**只绑回环**、**校验一次性令牌**。
所有 `/panel/local/*` 请求由网关剥掉外部凭据、换成一次性令牌再转发，
本机代理拒绝不带令牌的请求 —— 否则同机上别的进程可以借道调用它的接口。

安装位置建议固定（与网关同目录，或 `~/.wb-gateway/bin`）：macOS 的完全磁盘访问权限
是按二进制路径授权的，位置一变授权就失效，用户得重新拖一次。

## 六、升级

数据目录（凭据、`config.json`、模型缓存、统计）与程序文件是分开的，所以升级只换程序即可。

```bash
# 安装包形态：重新下发同一个安装包，装的时候会先停掉旧实例再覆盖
#   Windows : 再跑一次 setup.exe（数据在 %LOCALAPPDATA%\wb-gateway，不动）
#   Linux   : sudo dpkg -i workbuddy-gateway_<新版本>_amd64.deb
#   macOS   : 再双击一次 .pkg

# npm 形态
npm i -g workbuddy-gateway@latest

# 压缩包形态：覆盖二进制后重启
systemctl stop wb-gateway
install -m 0755 workbuddy-gateway /opt/wb-gateway/workbuddy-gateway
systemctl start wb-gateway

# Docker：重新 build 后重建容器（数据在卷里，不会丢）
docker build -t wb-gateway:latest . && docker rm -f wb-gateway && docker run -d ...
```

三个不建议的做法：直接 `kill -9`（丢最后一分钟统计）、
把数据目录和二进制放在一起然后整目录替换（容易连账号池一起覆盖掉）、
用 `dpkg --purge` 卸载后再安装（会把数据目录一起删掉）。

## 七、常见问题

**面板打不开，`/healthz` 连接拒绝**
先等 30 秒。启动时会同步拉模型目录再监听端口，这段时间端口还没开。

**账号池是空的**
凭据文件不在发现路径里。默认只在工作目录找 `workbuddy*.json`；用 `--auth-dir` 指定目录，
或直接在面板上「添加账号」。Linux 上还注意 systemd 单元里的 `ProtectHome=true`
会让网关看不见 `/home` 下的文件。

**改了配置但重启后没生效**
`config.json` 里只有可热改字段才即时生效；端口、代理、上游超时、`server.read_timeout` 需要重启。
面板保存后会提示「需重启进程生效」的就是后者。

**看不到「本机客户端」页**
`GET /panel/api/local/capabilities` 返回的 `available` 为 false，通常是本机代理二进制
不在查找路径里。接口的 `searched_paths` 会列出找过的位置，`reason` 给出可操作提示。

**定时任务在错误的时间触发**
签到、保活、任务中心都按**本地整点**排程。容器里记得设 `TZ`，
systemd 下确认系统时区正确。

**指定了站点却没生效，或者报「模型不存在」**
两种表现、两个原因：

- *没生效（不报错、请求照常成功，就是没走你指定的那一站）*：
  写法不在词表里。`?site=` / 请求头接受 `cn` / `china` / `domestic` / `mainland` 与
  `intl` / `international` / `global` / `ai` / `workbuddy.ai` / `codebuddy.ai` / `overseas`；
  模型名前缀**只认** `CN-` 与 `AI-`（写成 `international-xxx` 会被当成一个普通模型名，
  因为 `international` 不是前缀）。打错的来源会被**静默忽略**并继续往下找 ——
  这是有意设计：把打错的 `?site=usa` 理解成「走国内」是最坏的结果。
  面板「双站视图 → 指定站点」卡片列出的就是这份词表，可以直接复制。
- *报模型不存在（404）*：模型名带上了前缀但没被剥掉，通常是调用方自己把
  `AI-xxx` 拼进了上游请求而不是发给网关。前缀只对**网关的** `/v1/chat/completions` 生效。

**指定了站点却报「账号池里没有该站点的账号」（503）**
这是**有意不回落**：指定站点往往正是为了避开会花钱或不被允许的那一侧，
静默改用另一站等于违背意图。到「账号」页添加该站点的账号，或去掉站点指定改用默认调度。

**指定国内站，国内账号额度用完了，会不会拿国际站账号把请求发出去？**
不会。指定站点后整个「换号重试」循环都被限制在该站内：额度耗尽（余额为 0）、
上游报余额不足 / 限流 / 令牌失效时，只会改用**同站的另一个账号**，绝不跨站。
该站账号全部不可用时直接返回 503，而不是借用另一站 —— 跨站意味着账单出现在
你可能并不打算付费的那一侧。反之指定国际站时同理。这条约束有端到端测试锁住。
