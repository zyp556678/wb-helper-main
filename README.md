# WorkBuddy Gateway

三合一项目：把 [CangShui/workbuddy-gateway](https://github.com/CangShui/workbuddy-gateway)、
[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)、
[changexbc/workbuddy-switch](https://github.com/changexbc/workbuddy-switch) 合并为一个项目：
一个 Go 网关主进程 + 一套 React 管理面板 + 一个可选的本机代理进程（切片 7 已接入）。

设计文档见仓库根目录的 `三合一可行性分析与项目说明.md`，其中「零、已拍板的技术决策」记录了七条已定方向。

## 已定方向（摘要）

1. 反代层以 wb-gateway 的实现为基准（双站反代、模型完全透传、模型目录双源 + 倍率实测、
   流式分片规范化、tool_call 序列自愈、响应头/流中空闲分段超时、`/v1/responses`）。
2. 治理与面板来自 wb2api-panel（三因子加权选号、熔断、冷却、在途租约、降权、会话粘性、
   定时任务、成长任务体系、提示词体系、指纹脱敏、在线配置、分频道日志、realm 路由）。
3. 前端视觉与组件体系参考 wb-switch（设计令牌、明暗双主题、shadcn 风格组件、recharts 图表）。
4. 本机能力（本机客户端切号、会话复制、本地日志与限额台账）保留为独立的本机代理进程，由 Go 网关代管生命周期。
5. 桌面形态参考 wb-switch 建 Tauri 薄壳（桌面 App + 浏览器双形态），已在切片 9 交付
   （`desktop/`：Tauri v2 薄壳 + 托盘常驻 + NSIS 安装包）。
6. 提示词默认 append，custom 作为「安全优先」档位，passthrough 收进高级选项。
7. 功能与前端同步交付，按纵向切片推进。

## 当前进度

**切片 0（地基）已完成**：

- Go 网关骨架：`serve` / `status` / `version` / `help` 命令，`config.json` 运行层覆盖。
- **统一凭据模型**：同时读两种历史格式，不强制迁移。
  - wb-gateway 的嵌套形 + `edition` 字段
  - wb2api-panel 的嵌套形/扁平形 + `realm` 字段
  - 站点归一化优先级：`edition`（显式）→ `realm`（显式）→ `domain` 后缀 → 默认国内站
  - 写回时就地打补丁，保留未知键
- 账号池：轮询选号、健康状态、令牌过期判断与自动刷新、凭据热加载（默认每 5 秒扫描）、失效账号处置。
- 反代层：`/v2/chat/completions` SSE 转发、模型目录拉取、分段超时（响应头 + 流空闲看门狗）。
- 对外端点：`POST /v1/chat/completions`（流式 + 非流式聚合）、`POST /v1/responses`、
  **`POST /v1/messages`（Anthropic Messages API，Claude Code 可直接接入）**、
  `POST /v1/messages/count_tokens`、`GET /v1/models`、`GET /healthz`、`GET /status`。
  设了 `--api-key` 时同时接受 `Authorization: Bearer <key>` 与 `x-api-key: <key>`（后者是 Anthropic 客户端的习惯）。
- 面板：`/panel/` 提供前端构建产物（go:embed），`/panel/api/overview`、`/panel/api/accounts`、`/panel/api/config`。
- 前端：Vite + React 19 + TS + Tailwind v4 + Radix，侧栏骨架 + 账号页只读列表 + 明暗主题。

**切片 2（治理与调度）已完成**：

- 选号：三因子加权（积分比例 ×10 + 闲置补偿 + 快过期加成）+ Top5 短名单 + 加权随机 + 防惊群，
  全冷却时按最早到期兜底；`Strategy="roundrobin"` 可切回轮询语义。
- 治理：账号级软冷却（有界指数退避，封顶可配）、余额耗尽硬冷却至次日 04:00、模型级冷却
  （对齐上游重置墙钟，**切模型立即可用**）、熔断（连续失败 + 指数退避）、连败降权、在途租约。
- 会话粘性：同一会话复用同一账号；客户端不给会话标识时用 system + 首条 user 内容派生会话键。
- 错误分类：余额 / 会话 / 限流 / 模型级限流 / 内容拦截 / 请求畸形 / 5xx / 网络，
  其中内容拦截与请求畸形**不罚账号**（请求侧问题，换号也会再撞）。
- 定时任务：签到（幂等识别「今天已签到」）、令牌保活、周期额度刷新，按本地整点独立排程与开关。
- 可观测：每模型请求数 / 失败数 / 累计 token / 首字与总耗时（5 小时滚动窗口）。
- 面板：新增「监控」页与「配置」页，治理与调度参数**保存即热生效**（上游超时需重启）。

**切片 3（模型与倍率）已完成**：

- 目录双源合并：实时接口 + npm 静态目录（接口优先、按 ID 去重），失败用本地缓存兜底；
  启动时同步拉一次，避免客户端在启动瞬间拿到空模型列表。
- 上游目录全字段透出：倍率（credits，含促销折算）、上下文长度与可选长度、最大输出、
  思考默认档与能力标记、多模态与工具调用能力、描述、标签、厂商。
- 价格探测用**余额差分**（读余额 → 极短对话 → 等结算 → 再读余额），每模型 12 小时一轮。
  重要口径：**余额差分只能证明收费，不能证明免费**——计费可能延迟结算，读不到差值不代表不扣费；
  只有目录也明确免费时才判免费，否则保持未知或按目录判收费。
- `/v1/models` 附带 `context_length` / `max_output_tokens` / `multiplier` /
  `reasoning_default_effort` 等字段，供 Codex、Claude Code 等客户端正确取值。
- 模型黑白名单可在面板热改，命中黑名单的请求返回 403 且不消耗任何额度。
- 面板新增「模型与档位」页（倍率、探测详情、可用账号数、档位、禁用开关、批量探测）。

**切片 7（本机代理接入）已完成**：

发行包现在是**两个二进制**：`workbuddy-gateway`（网关，单文件含面板）+ `wb-local-agent`（本机代理）。
服务端部署只需第一个；第二个缺失时网关静默跳过，本机相关页面自动隐藏——这是正常路径，不是故障。

- 代管（`internal/localagent`）：按 同目录 → `./bin` → `~/.wb-gateway/bin` 发现二进制；
  用 `127.0.0.1:0` 让子进程自选端口（避免「选中到绑定之间被占用」的竞态），
  以 stdout 的 `LISTENING` 行作为就绪信号（不靠 sleep 猜时间）；每 10 秒探活；
  异常退出按 1s→60s 有界退避重启；退出时先关子进程 stdin 请它优雅收尾，5 秒超时再强杀。
- 反代：`/panel/local/*` → 本机代理。剥掉外部 `Authorization` 换成一次性令牌后转发；
  目标地址固定回环、不接受外部传入（否则这个端点会变成开放代理）；同样要过面板鉴权。
- 最小本机代理（`cmd/wb-local-agent`）：**只绑回环 + 全部请求校验一次性令牌**。
  能力是 `overview / paths / logs / hooks` 四项，**全部只读**。
  未实现的能力（会话复制、进程扫描、SQLite 读写、编辑器配置注入、钩子安装）在
  `/api/health` 的 `unimplemented` 字段里明示，面板也如实展示。
- 面板新增「本机客户端」页；`capabilities.available=false` 时侧栏入口自动隐藏，
  页内给出「为什么不可用 + 已查找过哪些路径」。

**一处范围调整，需要如实说明**：设计文档原先写的是「保留 switch 的 Rust 本机代理并瘦身」，
实际落地改成用 Go 写了一个**契约相同的最小只读实现**。原因是：没有 Rust 工具链就无法在本地
编译验证那 4.2 万行实现，而代管/反代/降级这些**编排**逻辑必须要有真实对端才能验证——
否则只能靠读代码假装正确（切片 4 已经吃过一次这个亏）。Rust 版仍可作为可替换实现接入，
路径与令牌契约一致。写本机的那部分能力刻意不做：在具备「写前备份 + 幂等安装 + 逐字节还原」
保护之前，做薄了比没有更危险。

**Anthropic 兼容端点（`/v1/messages`）**：

- 协议转换：`system`（字符串或 text 块数组）、`messages` 的 content blocks（text / image / tool_use /
  tool_result）、`tools(input_schema)`、`tool_choice`、`thinking` → 上游 chat 形态；响应按
  Anthropic 语义回译（流式发 `message_start` → `content_block_*` → `message_delta` → `message_stop`，
  非流式在本地聚合成完整 Message）。
- `stop_reason` 映射：`stop→end_turn`、`tool_calls→tool_use`、`length→max_tokens`。
- 思维链（上游 `reasoning_content`）**只在请求启用 thinking 时**回译为 `thinking` 块
  （`enabled` 与 `adaptive` 都算启用）——没启用的客户端不处理这种块。
- `usage` 按 Anthropic 口径输出：上游的 `prompt_tokens` 含缓存命中，这里会减掉并单列
  `cache_read_input_tokens`，否则客户端把两者相加会算两遍。
- `count_tokens` 是**本地估算**（CJK 约 1 token/字、ASCII 约 4 字符/token），不调用上游、不消耗额度。
- 上游流中断或没有 `finish_reason` 时**不下发 `message_stop`**，而是发 `error` 事件 ——
  伪造完成会让客户端把残缺输出当成完整回复。
- Claude Code 接入：把 `ANTHROPIC_BASE_URL` 指向 `http://127.0.0.1:8317`、`ANTHROPIC_API_KEY`
  设成网关的 api-key（或留空）即可。

**切片 8（双站与交付）已完成**：

- **免费站点优先**：同一模型在国内站/国际站之间只有「一侧确认免费、另一侧确认收费」时才倾斜；
  两侧都免费、都收费、或任一侧结论未知，都不倾斜（把未知当免费去倾斜等于把流量压到可能收费的一侧）。
  倾斜是**软优先** —— 优先站点里没有健康候选时自动回落到全部站点。开关 `pool.prefer_free_site`
  默认开启，可在面板热改。判定逻辑由目录层给出，选号层（加权与轮询两条策略）都支持。
- **双站视图**：`GET /panel/api/sites` 按站点聚合账号分布、目录覆盖、价格结论分布、
  已确认免费的模型列表，以及当前生效的倾斜规则；面板新增「双站视图」页。
- **部署形态**：`Dockerfile`（三阶段构建，运行时镜像不含本机代理）、`docker-compose.yml`、
  systemd 单元与一键安装脚本、Windows 计划任务安装脚本，以及 `DEPLOY.md` 部署文档。
- **发行打包**：`./scripts/release.sh` 交叉编译五平台（linux/amd64+arm64、windows/amd64、
  darwin/amd64+arm64），每个包里含两个二进制、配置样例、说明文档与该平台对应的部署脚本。

> 部署相关的坑（启动时先拉目录再监听端口、必须挂卷 `/app/data`、容器时区影响定时任务、
> 为什么 Windows 用计划任务而不是服务）都写在 `DEPLOY.md` 里，装之前建议先扫一眼。

**切片 8 顺带修掉的一个跨切片隐患**：配置的分段补丁此前对 `pool` / `cooldown` 是**整段覆盖**，
局部提交会把同段的其它字段清零 —— 而 `max_in_flight = 0` 是合法值（不限并发），
被清零等于并发限制静默失效；`schedule` 段还漏接了切片 5 新增的四个排程字段，
导致「任务中心 / 夜猫子」的定时设置能保存、能落盘、重启后却永远不生效。
现在改为基于「客户端实际提交了哪些键」做逐字段合并，并配了 6 条回归测试。

**站点路由（显式指定走国内站还是国际站）**：

上面那条「免费站点优先」是**网关替你决定**往哪一站发；站点路由则是**你说了算**。
两者的关系是：不指定站点时走默认调度（含软优先），一旦指定就按你说的走。

三种写法，优先级从高到低：

| 优先级 | 写法 | 适合 |
| --- | --- | --- |
| 1 | `CN-<模型名>` / `AI-<模型名>`（模型名前缀） | 最省事：粘到任何支持 `model` 字段的工具里即可生效 |
| 2 | `?site=intl`（URL 参数） | 单次请求粒度，改一个参数即可验证 |
| 3 | `X-WB-Site: intl`（请求头） | 整个客户端都固定走某一站 |

- **前缀会被剥掉再发给上游**。`AI-gpt-5` 里的 `AI-` 只用于网关内部路由，上游不认识它。
  这点写漏的症状是「一用前缀就报模型不存在」，与站点路由本身毫无字面联系。
- **前缀用大写 + 连字符**：上游模型名全是小写且大量含连字符（`deepseek-v4.1-flash`），
  大写前缀与真实模型命名空间天然隔离，不会把某个真实模型永久劫持成路由前缀。
  分隔符必须写（`AI-xxx` 可以，`AIxxx` 不行），前缀段也必须**完全等于** `cn` / `ai`
  （`cn2-xxx` 不是前缀）；不满足就当成普通模型名原样转发，宁可报模型不存在也不静默走错站。
  大小写不敏感，`cn-` / `Cn-` 同样生效。
- **换号只在同一站内进行**：指定站点后，整个「换号重试」循环都受该站约束 ——
  国内站账号额度耗尽（余额为 0）、上游报余额不足 / 限流 / 令牌失效时，只会改用
  **同站的另一个国内账号**，绝不会为了把请求发出去而跳到国际站；反之亦然。
- **强制不回落**：指定了站点而该站没有可用账号时**直接报错**（503 `no_account_for_site`），
  不会静默改用另一站。理由是指定站点往往正是为了避开会花钱或不被允许的那一侧。
- **别名**：`?site=` / 请求头接受 `cn` / `china` / `domestic` / `mainland` 与
  `intl` / `international` / `global` / `ai` / `workbuddy.ai` / `codebuddy.ai` / `overseas`；
  写成 `auto` / `default` / `any` / `none` 表示「这次用默认逻辑」。模型名前缀**只认** `CN-` 与 `AI-`。
- **面板**：「双站视图」页新增「指定站点」卡片，把上述词表**由后端同一下发**
  （`GET /panel/api/site-route`）并支持一键复制，避免界面教出后端不认的写法 ——
  这种错误是静默的：用户照着写，请求被当成「未指定」，不报错也不生效。
- **模型页一键复制**：模型卡与表格里的每个模型名旁都有复制按钮，给的是可直接使用的
  `model` 值；每一行的站点价格旁另有「锁定该站」的复制按钮，给的是 `CN-<模型名>` / `AI-<模型名>`
  这类带前缀写法。两者刻意分开，避免用户分不清粘出来的是哪一种。

**安装包（延续切片 8 的交付部分）**：三条安装路径与 switch 一一对照 —— 平台安装包、npm 全局安装、
免安装压缩包。其中 npm 与 Windows 安装包在本机做过真实的安装/卸载往返验证；Linux 的 `.deb`
与 macOS 的 `.pkg`/`.dmg` 需要对应系统上的工具链（`dpkg-deb` / `pkgbuild`+`hdiutil`），
本机是 Windows，只能在 CI 或对应平台上产出与验证。

切片 0 到 9 均已交付（含桌面壳），网关主链路、面板与桌面形态均可完整使用。

**切片 6（统计与日志）已完成**：

- 用量统计（`internal/stats`）：按小时聚合**持久化**到 `wb-stats.json`（保留 30 天，损坏静默重建）。
  记录每模型请求数 / 失败数 / 输出 token / 首字合计，以及**余额采样**（同一小时只留最后一个点）。
  它与「监控」页的滚动窗口指标是两个相反的取向：监控要短窗口新样本，统计要长历史。
- 官方口径用量：POST 上游 `/billing/meter/get-user-request-usage`。
  **注意两个坑**：它是 **POST**（GET 会拿到空/404），且**没有 `/v2` 前缀**（带前缀 404），
  而同期其它 billing 端点（额度汇总、签到）是带 `/v2` 的。返回原始请求行，
  映射成按天 / 按模型 / 按客户端来源 / 按账号 / 最近明细五组视图。
  面板上单列一张卡，并写明「不一致时以上游为准」。
- 请求级 trace：每次请求生成 8 位 trace，贯穿出站改写、拦截重试、完成统计三处事件，
  日志页点 trace 即可串起整条链路；关键词搜索覆盖 trace 与 fields 值。
- 面板新增「用量统计」页：请求量与失败数柱状图、Token 面积图、积分余额折线、模型排行表、
  24h/7d/30d 切换、重置统计。
  recharts 通过 `React.lazy` 按需加载（主包 947 kB → 526 kB）；30 天的 720 个点按小时合并
  降采样到 ≤140 点，其中余额取该组**最后一个点**而不是求和或平均（它是存量指标）。
- 顺带修了两处：余额采样周期 5 分钟 → 1 分钟（否则趋势图末点长期停在旧值）；
  退出时补一次统计刷盘。

**切片 4 遗留验证项已补齐**：用 httptest 精确构造「首次 400 内容拦截 → 重试 200」，
端到端验证降级重试。这个测试当场抓出一个真 bug——**降级重试只在 `/v1/responses` 接了线，
`/v1/chat/completions` 传的是 nil，主端点的重试分支从来没触发过**；
另有一个更隐蔽的问题：重试占用了「换号次数」预算，单账号时重试永远排不进来。
两处都已修复。这也说明「写了重试逻辑」和「重试真的会跑」是两件事。

**切片 5（任务体系）已完成**：

- 成长域客户端（`internal/upstream/growth.go`）：任务列表 / 报名 / 领奖 / 对话活跃上报 /
  连登 / 猫咪旅行 / 抽奖 / 校园日。域名分野已写清：任务列表与报名走 CLI 域，
  领奖、连登、旅行、抽奖、上报走 Web 域。
- 任务中心（`internal/tasks`）：扫描 → 分类 → 执行队列。PC 口径与小程序口径两份列表
  合并去重（mp 侧独有的标为「小程序」）。
- 分类四档，面板如实标注：**可领奖 / 可自动完成 / 可报名 / 需人工处理（附具体原因）**。
- 自动动作分三档：
  1. **对话类（默认开）**：`chat_5`、`Model_chat_GLM5.2`、`black_cat`（仅 23:00-08:00 计分）、
     领养链 `first_buddy`（上报 → 同意协议 → 领取第一只 Buddy）。模型类与夜猫子会**真的发一次
     对话**再上报 —— 这类任务的判据里既有事件也有对话本身，只发事件进度不动。
  2. **客户端事件链（`tasks.desktop_events_enabled`，默认关）**：模板 / 灵感案例 / 设计画布 /
     资料库介绍 / 换肤 / `Buddy_App` / `automation_1` / `RichMeow_Chat`，以及
     **专家系**（`expert_5`、`Expert_team_use_3`、`Expert_lighthouse`、`skill_1`）与
     **小程序口径**（`school_season`、`Sequential_Tasks_1..7`）。
     专家系不是「只发事件」：专家 id 取自真实市场列表、对话是真的对话、事件的 requestId
     取自服务端返回的 SSE —— 伪造的只是「用户点了召唤」这一个动作。
  3. **需人工**：确实没有接口可调的（公众号关注、公益捐款等）。
- 定时排程里的「保号类任务」= 签到 / 活跃上报 / token 保活 / 余额刷新四类。它们默认与选号
  一致地**跳过已禁用账号**；勾选 `schedule.include_disabled_in_tasks` 后改为照常执行
  （依旧不参与选号，也不会因此被自动解冻）—— 「禁用」的语义只是「不再参与选号」，
  而不是「停止一切上游保号行为」。对「一次只放开一个号、用禁用做流量开关」的轮换养号用法，
  闲置待命的号恰恰最需要签到与续期。该开关只覆盖这四类；猫猫旅行、夜猫子、连登管家与
  成长任务队列仍跳过禁用号。
- 开关默认关闭的理由：这些事件链是**按参考实现的实测样本复刻的形状**，不是官方接口。
  形状可离线钉住（单测覆盖），但「上游是否接受、是否会计分」无法离线验证，
  且伪造客户端事件有被风控识别的风险，代价由账号承担。做成显式开关，是为了让打开它
  成为一个有意识的决定，而不是某次升级后的意外变化。
- **每个动作后自动领奖**：上报 200 ≠ 计分（上游计分是异步的，实测数秒后才落账），
  所以动作结束后会在有界预算内轮询回读进度，达标就顺手把奖励领掉。
  不这么做的话，用户还得回列表再点一次「领取」—— 而那一步经常被忘掉，第二天进度重置、奖励白丢。
- 账号级「一键完成」（`/panel/api/accounts/{id}/tasks/auto` 与 `/auto_all`）：同步返回逐项结果，
  面板弹窗当场展示「哪几项做了、进度从哪到哪、领了多少」。与跨账号队列共用同一把
  per-account 锁，同一账号不会并发跑两条链路。
- 执行队列**串行**跑并每步留 1.05 秒间隔：成长域对同一账号的连续操作敏感，
  用并发换来的那点时间不值得冒风控风险。结果逐条回写，面板可轮询进度。
  对话事件另有 45s + 抖动的真人节奏（连发会被判无效并回滚进度，白报比不报更糟）。
- 定时排程新增两类：任务中心每日自动执行（`tasks_hours`，默认 9 点）、
  夜猫子独立时点（`blackcat_hours`，默认 23 点，因为它的计分窗口是 23:00-08:00）。
- 面板新增「任务中心」页；配置页定时任务卡片补上这两类任务的开关与小时选择。
- 一个实现细节值得记：上游对旅行与抽奖的响应字段形态不稳定（`depart_at`/`arrive_at` 是毫秒
  时间戳，`location`/`letter`/`module` 是对象），这些字段一律用 `any` 承接——
  强类型会让「读状态」整体失败，而读状态失败会把整个任务面板拖垮，代价远大于放弃类型信息。

**切片 4（出站改写与协议面）已完成**：

- 出站改写收敛成一条**有序管线**（`internal/outbound`）。顺序是有依赖的，散开写一定会出现
  「改了 A 又被 B 还原」的隐蔽 bug：提示词模式 → 首条保底 system → 强制 stream →
  stream_options 兜底 → `max_completion_tokens` 翻译 → tool_choice / role / image_url 归一 →
  工具序列自愈 → 思维链开关与档位降级 → reasoning_content 回填 → 指纹脱敏。
- 提示词三模式，默认 `append`：`passthrough` 完全透传（最忠实、最易被拦）、
  `append` 保留客户端 system 并追加一条网关提示词（默认）、`custom` 删除全部客户端 system
  （拦截率最低、但会丢客户端项目规范）。可在面板上热切。
- 指纹脱敏两层：header 与 `cc_*` 键值整段剥离、模板句最小改写（换一个词、语义不变）、
  反探测串 `11128` 改写为 `11-128`。带 Claude Code 指纹的请求在 append 模式下可正常通过。
- 内容策略拦截后**换中性提示词就地重试一次**（不换号：同一请求换号也会再撞同一个审核）。
- 工具序列自愈：按 tool call ID 对称裁剪（无结果的调用、无调用的结果、重复项），
  并把散装的单调用批次重排成并行批次。专治上游 11148——那种错误会让断裂之后的
  **每一条**消息都报错、整段会话彻底不可用。
- DeepSeek 思维链：出站注入 `thinking.type=enabled` 并补默认档（只给开关不给档位上游仍按不思考应答），
  档位按模型 `supportedEfforts` 自动降级，assistant 消息双向回填 `reasoning_content` / `reasoning`。
- `/v1/responses`：入站转 chat、出站转 Responses，流式事件序列完整；
  出错时发 `response.failed` 而**不**发 `response.completed`（否则客户端会把残缺的工具调用当完整结果执行）。
- 新增事件日志（内存环形缓冲 500 条，不落盘）与面板「日志」页：按级别 / 频道筛选、搜索、
  自动刷新、清空、展开 fields 详情。

**切片 1（账号与登录）已完成**：

- 面板内 OAuth 添加账号：授权链接 + 二维码 + 轮询，成功后凭据落盘并热加载进池，自动查一次额度。
- 额度查询与展示：总额度 / 已用 / 剩余、套餐归一化（Pro试用 / pro / 免费）、额度进度条。
- 单账号运维：刷新积分、启用 / 禁用（写 `.disabled` 标记，跨重启保持）、移除。
- 批量刷新全部账号积分。
- 集成在 `/panel/api/login/*`、`/panel/api/accounts/{id}/*`、`/panel/api/quota_all`。

关于「移除」的语义：账号池的来源是凭据发现目录，所以单纯从内存摘掉会被下一次热加载加回来。
因此移除有两种模式——默认把凭据**归档**到 `.removed/`（离开发现路径、可随时移回恢复），
勾选后直接**删除**文件（不可恢复）。

## 安装方式

四条路，装的是同一个网关。除容器外都不需要管理员权限。

**零、桌面应用（Windows / Linux，切片 9）**

`dist/workbuddy-gateway-desktop_0.9.0_x64-setup.exe`：Tauri 壳 + 网关 + 本机代理
一起安装到 `%LOCALAPPDATA%\WorkBuddy Gateway`（每用户安装、免 UAC），开始菜单启动
「WorkBuddy Gateway」。关闭窗口收进托盘（网关继续常驻），托盘菜单提供显示面板 /
在浏览器打开 / 重启网关 / **开机自启动** / 打开数据目录 / 退出。数据目录仍是 `%LOCALAPPDATA%\wb-gateway`，
**与命令行版共用同一份账号池**；卸载保留数据目录。绿色版用
`workbuddy-gateway-desktop_0.9.0_x64.exe`（需保持其与 `bin/` 的相对布局）。
构建方式与工具链修补见 `desktop/README.md`。

Linux 桌面对应 `dist/workbuddy-gateway-desktop_0.9.0_amd64.deb`：同一个壳，装成
`/usr/bin/workbuddy-gateway-desktop`（菜单里显示「WorkBuddy Gateway」），网关与本机代理
作为壳的资源落在 `/usr/lib/WorkBuddy Gateway/bin/`，数据目录是 `~/.wb-gateway`。
它与**服务端包**（`workbuddy-gateway_<版本>_amd64.deb`，装 systemd 服务、只有浏览器访问）
是两件事：要窗口就装前者，要开机常驻的无界面服务就装后者。两者 dpkg 包名不同
（`work-buddy-gateway` / `workbuddy-gateway`），可以共存，但共用 8317 端口。
构建：`./installer/linux/build-desktop-deb.sh`。
> GNOME 下托盘图标需要 AppIndicator 扩展；没装扩展时窗口一切正常，只是没有托盘图标。

**开机自启动默认关闭**，两个地方可以开：托盘菜单的「开机自启动」勾选项，或面板
「配置 → 开机自启动」的开关（桌面版、命令行版、容器版的面板里都有这一项，读的是
系统里的真实自启状态）。

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
> 本项目的桌面形态已在切片 9 交付（`desktop/`，Windows 产出 NSIS 安装包，
> 托盘常驻、关闭收进托盘；macOS/Linux 仍走各平台服务化安装路径）。
> 尚未实现的只有代码签名与自动更新。完整对照见 `DEPLOY.md` 第零节。

## 快速开始

### 准备凭据

把凭据文件放进工作目录（自动发现 `workbuddy*.json`），或用 `-auth` / `-auth-dir` 指定。
凭据格式为任一种历史格式（见上方「统一凭据模型」）。

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

## 运行时配置

工作目录下的 `config.json`（可复制 `config.example.json`）覆盖运行层字段：

```json
{
  "upstream": {
    "headerTimeoutSeconds": 300,
    "idleTimeoutSeconds": 120,
    "transientRetries": 2
  },
  "debug":    { "enabled": false },
  "models":   { "blocklist": [], "allowlist": [], "accounts": {} },
  "logging":  { "request_archive_enabled": true, "request_retention_days": 7, "request_archive_max_mb": 100 },
  "server":   { "read_timeout": "300s" },
  "schedule": { "include_disabled_in_tasks": false }
}
```

- `upstream.headerTimeoutSeconds`：发出请求到收到响应头的上限。上游对 3MB+ 大请求排队预处理可能接近 1 分钟，
  默认放宽到 5 分钟兜底。
- `upstream.idleTimeoutSeconds`：流式响应的空闲读超时。持续有数据永不超时，超过该时长无新数据才判卡死并中断。
- `upstream.transientRetries`：瞬时网络错误重试次数，默认 `2`，**显式写 `0` 表示禁用**。
  网关与上游 CDN 边缘节点之间的单条 TCP 连接可能被对端重置、被关闭，或命中已被回收的
  keep-alive 连接——这类错误属于瞬时故障，值得换条新连接重试一次。
  但**只在请求头尚未写出时重试**：一旦请求头写出，上游就可能已经收到并开始生成，
  此时重放会重复生成与重复计费，所以如实报错而不是偷偷重试。客户端主动取消与上游
  返回的 HTTP 错误响应也不重试。
- `models.blocklist` / `allowlist`：**模型**黑白名单，黑名单优先；白名单非空时只放行列表内模型。
- `models.accounts`：**模型专属的账号名单**，控制「这个模型能用哪些凭据文件」，
  与上面的模型黑白名单互不替代。只接受凭据**文件名**（如 `intl-a.json`），不接受路径或通配符；
  同一模型内黑名单优先，白名单为空表示不限制；模型名忽略大小写。请求调度与失败换号都不绕过名单，
  后台价格探测与本机 `/admin/probe` 也会跳过不允许的账号。若某模型配了名单但**没有任何账号符合**，
  请求返回 `403 model_account_disabled` 且不会调用上游。

```json
{
  "models": {
    "accounts": {
      "deepseek-v4.1-flash": { "allowlist": ["intl-a.json", "intl-b.json"], "blocklist": ["intl-b.json"] },
      "hy3": { "blocklist": ["old-account.json"] }
    }
  }
}
```

  上面这段配置下，`deepseek-v4.1-flash` 最终只允许 `intl-a.json`，`hy3` 仅排除 `old-account.json`，
  未配置的模型不受任何限制。`config.json` 在服务启动时读取，修改后需重启网关。
- `server.read_timeout`：入站请求读取（含 body 上传）的总时长上限，默认 `300s`；`"0"` = 不限制；
  负值或不可解析的写法会**拒绝启动**（静默钳 0 等于把保护悄悄关掉）。大上下文 / 文件块经反代链
  慢速上传被掐成 `400 read body: ... i/o timeout` 时调大它。属装配期字段：**改动需重启进程**。
- `schedule.include_disabled_in_tasks`：保号类四任务（签到 / 活跃上报 / 保活 / 余额刷新）
  是否覆盖**已禁用**账号，默认 `false`（禁用即跳过，与选号过滤一致）。打开后禁用号照常保号、
  依旧不参与选号、也不会被自动解冻 —— 适合「一次只放开一个账号、用禁用做流量开关」的轮换养号用法。
  热生效，无需重启。详见「任务体系」一节。
- `logging.request_archive_enabled`：请求级 **JSONL 归档**开关，默认 `true`。
  对话类端点（`/v1/chat/completions`、`/v1/responses`）每次请求记一条**脱敏**元数据
  （时间、请求 ID、账号「昵称(uid8)」标签、模型、状态码、结局、耗时、TTFB、重试次数、
  token、消耗），落在工作目录的 `request-logs/requests-YYYY-MM-DD.jsonl`。
  **绝不写提示词、响应正文、Authorization 或完整 UID。**
  归档走有界队列，**队列满时丢弃并计数**（`archive.dropped_writes` 可查），
  不允许日志写盘拖住模型请求。按天 + 按大小（单文件 16 MiB）轮转。
  进程内指标（最近 100 条 + 完成/成功/失败/成功率/平均耗时）**始终启用**，与本开关无关。
- `logging.request_retention_days`：归档保留天数，默认 `7`。
- `logging.request_archive_max_mb`：归档总容量上限（MiB），默认 `100`；超出后从最旧的开始删。
- 查看：面板 `GET /panel/api/request-metrics`（进程内指标 + 最近 100 条 + 归档状态）、
  `GET /panel/api/request-logs?limit=&outcome=&account=&model=`（从归档读，limit 默认 200、最大 1000）。
  响应头 `X-Request-Id` 与记录里的 `request_id` 一致，便于和上游日志对齐。

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
internal/server              HTTP 层（OpenAI 兼容端点 + 面板 API + 静态资源）
web/                         React 前端（构建产物 web/dist 由 web/embed.go 嵌入）
scripts/build.sh             日常构建
scripts/release.sh           多平台发行打包
deploy/                      systemd 单元与安装脚本、Windows 计划任务脚本
Dockerfile / docker-compose.yml
DEPLOY.md                    部署文档（二进制 / systemd / Windows / Docker）
三合一可行性分析与项目说明.md  设计文档与切片进度
repos/                       三个上游项目的只读克隆（不参与构建）
```
