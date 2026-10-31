# 上游合并更新准备（2026-11-01）

本文是**动手前的盘点与计划**，不含代码改动。合并完成后，逐项结论应并入
[PROGRESS.md](../PROGRESS.md) 的「上游跟进」一节，本文即可删除。

## 进度（2026-11-01）

| 切片 | 状态 | 提交 / 说明 |
| --- | --- | --- |
| 1 工具链 + 绿色基线 | ✅ | Go 1.27.3；`build` / `vet` / `test` 全绿 |
| 2 Windows RST 文案 | ✅ | `c3cd2cf` |
| 3 DSH 历史 11-128 兼容 | ✅ | `70de27a` |
| 4 `identity` 配置化 | ✅ | `b98f160` |
| 6 panel 协议修复（6021 / 11158 / 12153） | ✅ | `932a4e9` |
| 5 SSE 首字节前心跳 | ✅ | `2dd3d37`（三条流式路径全覆盖） |
| 5b 首字节前安全重放 | ⬜ 待做 | — |
| 7 `requestProfile` 官方出站兼容 | ⬜ 待做 | — |
| 8 监听端口热切换 | ⬜ 待做 | — |
| 9 随机签到计划 | ⬜ 待做 | — |

逐项细节与「未跟随」理由见 [PROGRESS.md](../PROGRESS.md) 的「上游跟进（2026-11-01）」。

## 一、结论摘要

1. **不存在 `git merge upstream/main` 这条路**。本项目与三个上游没有共同祖先
   （见[三合一可行性分析](../三合一可行性分析与项目说明.md)第 118–119 行），
   上游更新只能按切片**人工比对搬运**。本文的「准备」指把比对材料、缺口清单、
   验证基线和执行顺序先定下来。
2. **主要工作量在 wb-gateway**：上次跟进停在 `v1.14.21`（2026-10-21），
   上游已到 `v1.14.41`（2026-10-28），16 个提交、57 个文件、+9309/−692。
3. **wb2api-panel 是一笔一次性欠账**：`main` 从 2026-10-12 直接跳到 2026-10-21
   的 25 个 PR 批量合并（34 个非合并提交、+10985/−241），随后上游发布
   **停止维护声明**。它不会再变，搬完即完结。
4. **wb-switch 无需处理**：`main` 停在 `v0.1.62`（2026-10-12），零新增提交。
5. **当前有硬阻塞**：本机没有 Go，也没有 Node/pnpm（见第四节）。在装好之前
   无法编译、无法跑测试，也就无法验证任何搬运。
6. **最该先做的不是最大的那块**。本机是 Windows + DSH，两个上游修复正好命中
   这个组合：Windows 的 RST 文案识别、DSH 历史的 11-128 兼容。它们改动都小。

## 二、基线坐标

| 仓库 | 位置 | 基线 | 说明 |
| --- | --- | --- | --- |
| 本项目 | `wb-helper-main/` | `6f523ff`（2026-10-21），`main`，工作区干净 | 版本 `0.14.3`，tag `v0.14.3` |
| wb-gateway | `repos/workbuddy-gateway/` | `v1.14.21` = `d8235b0c` → HEAD `b65634a`（v1.14.41） | 16 提交 / 57 文件 / +9309 −692 |
| wb2api-panel | `repos/workbuddy2api-panel/` | `9282779`（2026-10-12）→ HEAD `73a7560` | 59 提交（34 非合并）/ 77 文件 / +10985 −241 |
| wb-switch | `repos/workbuddy-switch/` | `v0.1.62` = `22b318c1`（2026-10-12） | **零新增**，本轮跳过 |

`repos/` 已被 `.gitignore` 排除（第 41 行），三个只读克隆不污染工作区，也不参与构建 ——
这正是项目设计里约定的比对材料位置，本轮已按此补齐。

## 三、三个上游的新增范围

### 3.1 wb-gateway `v1.14.21 → v1.14.41`

| 提交 | 版本 | 主题 |
| --- | --- | --- |
| `78ed21b` | v1.14.23 | 统一出站兼容（`requestProfile`）+ 修复 SSE 心跳帧 |
| `afd0fb7` | v1.14.24 | 流式韧性（首字节前心跳 + 安全重放）与 DSH 历史兼容 |
| `bc92f1a` | v1.14.22 | 识别 Windows Winsock 的 RST 文案；启动暴露实际出站 profile |
| `fda6784` | v1.14.29 | `identity` 身份替换、Responses 工具保真、国内外上游分流 |
| `ddeace4` | v1.14.30 | 可热加载的单账号与全局并发控制 |
| `e9fa5e3` | v1.14.35 | 统计图悬浮联动（网页 UI） |
| `c739dab` | v1.14.36 | 随机签到计划与限次重试 |
| `16d0152` | v1.14.37 | 启动时按分钟依次补签 |
| `bb577cf` | v1.14.38 | 控制台精简 + 监听端口安全热切换 |
| `b529ca0` | v1.14.40 | 补齐默认渠道兼容替换 |
| `c9ae4f0` / `b65634a` | — / v1.14.41 | 保留 `identity` 控制、升级配置向后兼容；移除重复的 `promptcompat` 模块 |

新增源文件：`outboundcompat.go`、`responses_tools.go`、`stream_resilience.go`、
`dshguicontext.go`、`identity.go`、`concurrent.go`、`checkin.go`、`servers.go`。
新增文档：`docs/outbound-compatibility.md`、`docs/11-128-dsh-history.md`。

### 3.2 wb2api-panel `9282779 → 73a7560`

`main` 的首个父提交序列在 2026-10-13 至 2026-10-20 之间**为空**，全部 25 个
PR 合并在 2026-10-21 一次性落地。因此本项目 10-17 与 10-21 两次跟进里写的
「panel 自 2026-10-12 起无新增」在当时是准确的，只是没赶上那一天的批量合并。

上游在同一天发布了停止维护声明，理由三条：上游开始禁用反代、个人精力有限、
非法转卖泛滥；并披露近期 issue/PR 与实现大量由 AI 辅助完成。

**判断：这笔欠账是一次性的，不必按「持续跟进」的节奏处理。**

### 3.3 wb-switch

`main` 自 `v0.1.62` 起无新提交。仓库里另有 30 余个未合并的特性分支，但那是
上游未发布的工作，不构成「上游更新」，本轮不纳入。

## 四、阻塞项：工具链缺失

| 需要 | 现状 | 影响 |
| --- | --- | --- |
| Go ≥ 1.27（`go.mod` 声明 `go 1.27`） | **未安装**（PATH、注册表、常见安装目录均无） | 不能 `go build` / `go vet` / `go test`，搬运无法验证 |
| Node + pnpm | **未安装** | `web/` 是 React 19 + Vite + Tailwind v4，改面板页面时无法构建 |

仓库描述写明「不含凭据与工具链」，所以这是设计如此，不是环境坏了。
**这两项是动手前必须先解决的前置条件。**

## 五、缺口矩阵：wb-gateway

判定口径：`已覆盖` = 本地有等价实现（命名可能不同）；`缺` = 需要搬运；
`部分` = 有但不完整；`N/A` = 形态不同无对应改造点。

| 上游项 | 本地对应 | 判定 | 备注 |
| --- | --- | --- | --- |
| v1.14.22 Windows RST 文案 | `internal/upstream/client.go` `IsTransientNetworkError` | **缺** | 本地只认 `connection reset by peer` 等 POSIX 文案；Windows 的 `wsarecv: An existing connection was forcibly closed by the remote host` 不含这些串，会被判成永久错误而**不重试** |
| v1.14.22 启动打印出站 profile | — | 缺 | 依赖 `requestProfile` 先落地 |
| v1.14.23 `requestProfile` 官方出站兼容 | `internal/outbound/payload.go` | **缺** | 本地只有 `stream_options.include_usage` 兜底；无官方静态头、无 gzip、无协议参数转换、无前导 system 合并 |
| v1.14.23 / v1.14.24 SSE 心跳与安全重放 | — | **缺** | 本地不发首字节前心跳，也没有 `canReplayStream` 式的「首字节前才允许重放」边界 |
| v1.14.24 DSH 历史 11-128 兼容 | `internal/outbound/sanitizeprefix.go` | **部分** | 本地只处理 system 的**第一个**文本块；上游扩展到 system/assistant 的**每个** text 块，并补了 GUI 环境句（`the DeepSeek Harness Web GUI` → `a Web GUI`）、Messages 合并前处理、fallback/force 注入后的顺序 |
| v1.14.29 `identity` 身份替换 | 同上（硬编码） | **缺** | 本地是单条硬编码 `You are an AI agent powered by DeepSeek Harness` → 中性句；上游改为可配置 `identity` 段，默认把 Codex / Claude / Claude Code / DeepSeek Harness / ZCode 统一成 `WorkBuddy` |
| v1.14.29 Responses 工具保真 | `internal/outbound/toolrepair.go`、`internal/dsml` | **缺** | namespace 子工具、`custom`（如 `apply_patch`）、`web_search` 在 Chat 上游往返中会丢；本地无对应映射 |
| v1.14.29 国内外上游分流 | `internal/upstream/profile.go` | 已覆盖 | 本地双站 Profile 早已有 |
| v1.14.30 `concurrent.MaxAcc/MaxGlobal` | `internal/pool` `MaxInFlight` / `MaxInFlightGlobal` | **已覆盖** | 语义等价，默认值不同：上游 4 / 不限，本地 3 / 2 |
| v1.14.35 统计图悬浮联动 | `web/`（React） | N/A | 上游是原生 JS 面板，本项目是 React + recharts |
| v1.14.36 随机签到计划 + 限次重试 | `internal/config` `schedule.checkin_hours`（固定时点，默认 `[9,21]`） | **缺** | 上游按北京时间给每个账号持久化每日随机计划，失败只在 09:30–10:00 再试一次 |
| v1.14.37 启动按分钟补签 | — | **缺** | 与上一条同属签到调度改造 |
| v1.14.38 监听端口安全热切换 | — | **缺** | 本地端口改动需重启（DEPLOY.md 已明示） |
| v1.14.38 配置页完整 JSON 编辑 | 本地配置页 | 部分 | 需按本地 React 形态重新设计，不是照搬 |
| v1.14.40 默认渠道兼容替换 | — | **缺** | `workspace root → workspace base`、`are paths the user → are paths that the user` 两条替换 |
| v1.14.41 移除 `promptcompat` 统一 `identity` | — | N/A | 本项目从未引入 `promptcompat`，直接按 `identity` 落地即可 |

## 六、缺口矩阵：wb2api-panel

只列**协议/正确性**相关的；纯面板 UI 项（手机端适配、运行日志积分历史、
到期卡片折叠等）在本地 React 面板里已有等价形态或没有对应改造点，不搬。

| 上游项 | 本地对应 | 判定 |
| --- | --- | --- |
| `6021 queue.full` 归为渠道级繁忙，不触发池级冷却 | `internal/upstream/classify.go` | 缺 |
| `11158` 模型权限错误分流 | 同上 | 缺 |
| 同账号 refresh 单飞，避免 RT 复用撞 `12153` | `internal/upstream/oauth.go` | 缺 |
| `cn.chat_base` 覆盖国内版上游 base | `internal/upstream/profile.go` | 缺 |
| `dns_servers` 可指定 | `internal/config` | 缺 |
| 裸模型名跨域回退（默认关） | `internal/pool` | 缺 |
| 联网搜索端点 `POST /v1/search/messages` | — | 缺（新增入口，工作量不小） |
| 企业切换会话 `token_source=enterprise_switch` 门控 | `internal/auth` | 缺 |
| 按地区独立配置轮询与顺序耗尽调度 | `internal/pool` | 缺 |
| 模型级 `6004` 双时钟（`until` < `resetAt` 时明示两时刻） | 本地有 `6004` 处理 | 部分 |
| 到期提醒快照与「检查」同口径 | `internal/creditwatch` | 部分 |
| 账号备注 | `internal/accountmeta` | 已覆盖 |
| 任务补领清扫（先查后领） | `internal/tasks` | 已覆盖 |
| 缓存命中对称反推（修告警假阳性） | `internal/tokenstats/scan.go` | 已覆盖 |
| `models.hidden` 黑名单 | 本地模型黑白名单 | 已覆盖（命名不同） |
| 出站代理支持 `ProxyFromEnvironment` | `internal/config/config.go` | 已覆盖 |

## 七、建议切片与顺序

原则：**先修「本机正在踩的坑」，再补「能力」，最后做大改造。**
每个切片独立可验证、独立成一次提交，失败可单独回退。

| 序 | 切片 | 对应上游 | 量级 | 理由 |
| --- | --- | --- | --- | --- |
| 1 | 工具链就位 + 建立绿色基线 | — | S | 没有 `go test` 通过就没有验证手段，后面全是盲改 |
| 2 | Windows RST 文案识别 | v1.14.22 | S | 一个函数加几条 marker；本机 Windows 上重试目前实际失效 |
| 3 | DSH 历史 11-128 兼容扩展 | v1.14.24 | M | 本机跑 DSH，system/assistant 全文本块与 GUI 环境句是最可能被拦的路径 |
| 4 | `identity` 配置化 | v1.14.29 + v1.14.40 | M | 取代本地硬编码；顺带把 Codex / Claude / ZCode 纳入覆盖，并补两条默认渠道替换 |
| 5 | 首字节前 SSE 心跳 + 安全重放 | v1.14.24 | M | 长思考模型首字节前的空窗会被客户端判死；重放边界要严格照抄（只在请求头未写出时） |
| 6 | panel 协议类修复合并成一批 | 6021 / 11158 / 12153 单飞 | M | 三项都是错误分类与令牌刷新，改动集中、互相独立 |
| 7 | `requestProfile` 官方出站兼容 | v1.14.23 / v1.14.29 | **L** | 工作量最大（约 500 行 + 文档），且上游正在收紧反代，收益不确定 —— 放最后单独评估 |
| 8 | 端口热切换 | v1.14.38 | M | 纯运维便利，不紧急 |
| 9 | 随机签到计划 | v1.14.36 / v1.14.37 | L | 740 行；收益是降低固定时点的风控特征，与切片 1–6 无耦合 |

不搬：v1.14.35 统计图悬浮（React 面板无对应点）、v1.14.41（无 `promptcompat`）、
v1.14.30（本地已覆盖）、panel 的纯 UI 项。

## 八、验证方式

每个切片至少跑通：

```powershell
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./...
```

搬运上游测试时注意两点：

- 上游测试大量使用 `httptest` 与合成账号，不访问真实模型服务，可直接借用；
- 上游 `tests/web/app.test.cjs` 与 `tests/web/polling.browser.py` 针对原生 JS 面板，
  **不可**直接用于本地 React 面板，对应断言需重写。

## 九、分支与提交规范

- 沿用现有习惯：每个切片一个提交，提交信息带 `跟进 wb-gateway vX.Y.Z`，
  例如 `feat: 身份替换配置化；跟进 wb-gateway v1.14.29/v1.14.40`。
- 切片 1 完成后打一次基线 tag（建议 `v0.15.0-dev` 或直接顺延），便于回退比对。
- 收尾时把逐项结论（含「未跟随」及理由）写入 `PROGRESS.md` 的「上游跟进」一节，
  与既有 2026-10-17 / 2026-10-21 两节保持同样格式。

## 十、待决策

1. **是否安装 Go 1.27 与 Node/pnpm**（第四节阻塞项）。这是动手前提。
2. **`origin/feat/unified-installer` 分支怎么处理**：它领先 `main` 6 个提交
   （统一三端安装器 + 4 个安装器修复），落后 25 个提交，末次提交 2026-10-14，
   已明显过期。要么合并进来，要么明确弃用，不宜继续挂着。
3. **`requestProfile`（切片 7）是否要做**。它决定本轮是「跟随到 v1.14.41 的绝大部分能力」
   还是「全量跟随」。考虑到上游正在收紧反代，建议先做 1–6，再单独评估。
