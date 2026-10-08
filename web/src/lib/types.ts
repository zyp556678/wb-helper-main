/** 后端 /panel/api 的响应契约（切片 1：账号与登录）。 */

export interface OverviewAccounts {
  total: number;
  active: number;
  disabled: number;
  cooldown: number;
  /** 已查到额度的账号的剩余积分合计。 */
  credits_remaining: number;
  /** 已查到额度的账号数。 */
  quota_known: number;
}

export interface OverviewModels {
  count: number;
  source: string;
}

export interface Overview {
  version: string;
  /** 网关已运行秒数。 */
  uptime_seconds: number;
  accounts: OverviewAccounts;
  models: OverviewModels;
}

/** 站点：国内站 / 国际站。 */
export type AccountSite = "cn" | "intl";

/**
 * 卡片显示字段（批次 4，对齐 wb-switch 的 DisplayField）。
 * 本项目没有手机号数据源，`phone` 仍入契约（存储与校验都支持），
 * 由前端在无数据时禁用并回退昵称。
 */
export type DisplayField = "nickname" | "phone" | "note";

/** 套餐名，后端给的三档；展示时按原文显示。 */
export type AccountPlan = "pro" | "Pro试用" | "免费";

/** 单账号额度快照；未查询过时整个对象为 null。 */
export interface AccountQuota {
  /** 总量，浮点。 */
  total: number;
  /** 已用，浮点。 */
  used: number;
  /** 剩余，浮点。 */
  remaining: number;
  plan: AccountPlan | string;
  paid: boolean;
  /** 后端判定的额度耗尽标记。 */
  exhausted: boolean;
  /** Unix 秒。 */
  updated_at: number;
}

export interface Account {
  /** 账号稳定标识 = 凭据文件名，所有单账号操作用它做路径参数。 */
  id: string;
  /** 凭据文件名，如 workbuddy.json。 */
  file: string;
  site: AccountSite;
  /** 站点展示名，后端给什么显示什么。 */
  site_label: string;
  uid: string;
  nickname: string;
  enterprise_id: string;
  /**
   * 是否为企业版账号（后端计算值，= enterprise_id 非空）。
   *
   * 企业版**没有个人成长体系**：签到 / 成长任务 / 连登管家 / 猫猫旅行 / 夜猫子
   * 在上游一律被拒（400 code 10001 或 403）。所以卡片要把这些入口藏掉 ——
   * 留一个点了必然报错的按钮，比没有按钮更糟。
   *
   * 用后端下发的标志而不是前端自己判 `enterprise_id !== ""`：那条规则只该有一份。
   */
  is_enterprise?: boolean;
  /**
   * 企业名（批次 4 加法字段）。本项目凭据模型里没有该数据源，
   * 后端按「有才下发」处理；账号信息弹窗在缺失时回退展示 enterprise_id。
   */
  enterprise_name?: string;
  /** 本地备注（批次 4）；空串 = 未填写。 */
  note?: string;
  /** 卡片显示字段（批次 4）；空/缺省 = 按昵称显示。 */
  display_field?: DisplayField | "";
  /**
   * 是否参与自动签到（批次 4）；false = 在排除名单里（手动签到不受影响）。
   * 旧后端/登录回执可能不带，缺失按 true 处理。
   */
  auto_checkin_enabled?: boolean;
  /**
   * 「账号 × 模型」级冷却（429/6004 模型限流）的当前台账，按恢复时间升序。
   * 缺失/空 = 没有受限模型（旧后端同样按空处理）。
   * `until` 是 **Unix 秒**的恢复时刻 —— 卡片用它做倒计时与「过期即不显示」。
   */
  model_cooldowns?: ModelCooldown[];
  disabled: boolean;
  disabled_reason: string;
  /** Unix 秒，0 表示无冷却。 */
  cooldown_until: number;
  cooldown_reason: string;
  /** Unix 秒，0 表示无过期时间。 */
  token_expires_at: number;
  token_valid: boolean;
  quota: AccountQuota | null;
  success_count: number;
  failure_count: number;
  last_error: string;
}

/** 一条「账号 × 模型」级冷却（后端 panelAccountView.model_cooldowns）。 */
export interface ModelCooldown {
  model: string;
  /** Unix 秒的恢复时刻。 */
  until: number;
  /**
   * 观测来源：gateway=网关代理请求时的 429；client=客户端日志里的限流
   *（WorkBuddy 客户端直连官方，网关看不到它的 429）。缺省按 gateway 处理。
   */
  source?: "gateway" | "client";
}

export interface AccountsResponse {
  accounts: Account[];
}

// -----------------------------------------------------------------------------
// 账号导入 / 导出（GET /panel/api/accounts/export 等）
//
// 导出文件是**可迁移的备份**，不是面板视图：带 access_token / refresh_token，
// 但不带冷却、熔断这些本机运行时状态（那些搬到另一台机器毫无意义）。
// -----------------------------------------------------------------------------

/** 导出文件里的一条账号记录。 */
export interface AccountExportRecord {
  id: string;
  uid: string;
  nickname: string;
  site: AccountSite;
  domain: string;
  enterprise_id: string;
  access_token: string;
  refresh_token: string;
  /** Unix 秒；0 表示无过期时间。注意是 camelCase —— 与 wb-switch 的导出一致。 */
  expiresAt: number;
  edition?: string;
  realm?: string;
}

/** 导出文档（带 app / version 便于回溯来源）。 */
export interface AccountExportDoc {
  app: string;
  version: string;
  exported_at: number;
  count: number;
  accounts: AccountExportRecord[];
}

/** 导入预览项。刻意不含 token —— 预览只需要够用户辨认账号。 */
export interface ImportPreviewItem {
  /** 该项在文件里的下标，导入时原样回传。 */
  index: number;
  uid: string;
  nickname: string;
  site: AccountSite;
  site_label: string;
  domain: string;
  enterprise_id: string;
  has_token: boolean;
  /** Unix 秒；0 表示无过期时间。 */
  expires_at: number;
  /**
   * 该项的 token 是 WorkBuddy 5.6 的加密信封（`$wbEncrypted`）：
   * 凭据存在，但本工具没有解密密钥，无法导入（需 wb-switch 或重新登录）。
   * 判据见后端 accounts_transfer.go 的 importTokenEnvelope。
   */
  encrypted: boolean;
  /** 池里已有同 uid（或目标文件已存在）。勾选前就要让用户看到。 */
  conflict: boolean;
  conflict_with: string;
  /** 非空表示这一项不可导入。 */
  reason: string;
}

export interface ImportPreviewResponse {
  accounts: ImportPreviewItem[];
  total: number;
}

export interface ImportAccountsResult {
  ok: boolean;
  /** 新写入的账号 ID（凭据文件名）。 */
  imported: string[];
  overwritten: string[];
  /** 跳过的原因，逐条人类可读。 */
  skipped: string[];
}

/** 单账号的今日签到状态（POST /panel/api/accounts/checkin-status）。 */
export interface CheckinStatusView {
  today_checked_in: boolean;
  /** 该站点不提供签到（国际站）—— 不是失败。 */
  unsupported: boolean;
  /** 非空表示这次没查成；此时 today_checked_in 无意义。 */
  error?: string;
  /** Unix 秒。命中服务端缓存时是原查询的时间。 */
  checked_at: number;
}

export interface CheckinStatusResponse {
  /** 键是账号 ID（凭据文件名）。 */
  accounts: Record<string, CheckinStatusView>;
  /** 服务端状态缓存的 TTL（秒）。 */
  ttl: number;
}

// -----------------------------------------------------------------------------
// 本机应用接入（GET /panel/api/local-apps 等）
//
// 把账号池里的账号写入本机某个应用的登录态。**网关本身是本机进程**，
// 读写本机文件是它的能力；面板只需调这些接口。
// -----------------------------------------------------------------------------

export type LocalAppTargetID =
  | "codebuddy-cli"
  | "workbuddy-desktop"
  | "jetbrains"
  | "vscode"
  | "codebuddy-ide";

export interface LocalAppTarget {
  id: LocalAppTargetID;
  label: string;
  /** 本机存在这个应用。 */
  installed: boolean;
  /** 我们实现了写入。false 时前端要禁用「设为当前」并显示 Blockers。 */
  writable: boolean;
  path: string;
  current_uid: string;
  /** 当前登录账号在池里的 ID（匹配不到时为空）。 */
  current_account: string;
  /**
   * 本目标**全部**当前登录账号的池内 ID（国内站/国际站各一个，国内站在前）。
   *
   * WorkBuddy 的国内站与国际站是两个互相隔离的客户端，可以各登录一个账号；
   * 只看 `current_account` 会让国际站那份登录在面板上不显示绿色「当前」状态。
   * 旧后端没有这个字段时，前端退化为只用 `current_account`。
   */
  current_accounts?: string[];
  current_label: string;
  note: string;
  /** 「能写但有风险/有条件」—— 非空时要二次确认。 */
  warning: string;
  /** 目标客户端自己记着的账号 uid 列表（目前只有 WorkBuddy 客户端有）。 */
  all_accounts: string[];
  /** 阻止写入的原因；非空时 writable 必为 false。 */
  blockers: string[];
}

export interface CLIStatus {
  env_token_masked: string;
  env_token_set: boolean;
  internet_env: string;
  base_url: string;
  has_storage: boolean;
  /** settings.json 里我们没动过的其他键名。 */
  other_keys: string[];
}

/**
 * 与 wb-switch 的共存冲突。
 *
 * 本机可能同时装着 wb-switch，它管的账号与本工具同一批、写同一批文件。
 * 两个工具写同一目标 = 后写覆盖先写（表现为「切了又变回去」）。
 */
export interface SwitchConflict {
  present: boolean;
  root: string;
  /** 为真表示 CLI 的 hooks 指向 wb-switch —— 它现在还会动手。 */
  hook_active: boolean;
  hook_commands: string[];
  /** 双方都会写的文件。 */
  contended: string[];
  detail: string;
}

export interface LocalAppsResponse {
  targets: LocalAppTarget[];
  cli?: CLIStatus;
  switch_conflict?: SwitchConflict;
}

export interface LocalAppsActionResult {
  ok: boolean;
  /** 逐条说明「实际做了什么」，直接展示给用户。 */
  notes: string[];
  error?: { code: number; message: string };
}

export interface LocalAppBackupFile {
  original: string;
  name: string;
  size: number;
  /** false 表示备份时该文件还不存在（恢复时会删除它）。 */
  existed: boolean;
}

export interface LocalAppBackup {
  id: string;
  target: LocalAppTargetID;
  files: LocalAppBackupFile[];
  created_at: number;
  reason: string;
}

export interface LocalAppBackupsResponse {
  backups: LocalAppBackup[];
}


/** 登录站点选项（GET /panel/api/login/sites）。 */
export interface LoginSite {
  site: AccountSite;
  label: string;
  hint: string;
}

export interface LoginSitesResponse {
  sites: LoginSite[];
}

/** 发起登录（POST /panel/api/login/start）。 */
export interface LoginStartResponse {
  login_id: string;
  site: AccountSite;
  site_label: string;
  auth_url: string;
  /** 授权链接有效期，秒。 */
  expires_in: number;
}

/** 轮询登录结果，按 status 分支；ok 时附带已热加载的账号。 */
export type LoginPollResponse =
  | { status: "pending" }
  | { status: "ok"; account: Account }
  | { status: "expired"; error?: string }
  | { status: "error"; error?: string };

export interface RemoveAccountResponse {
  ok: boolean;
  removed: string;
}

export interface QuotaAllResponse {
  refreshed: number;
  failed: number;
}

// -----------------------------------------------------------------------------
// 批次 4：账号备注 / 显示字段 / 批量刷新积分并签到
// -----------------------------------------------------------------------------

/** 单个账号的本地展示元数据（GET /panel/api/accounts/meta）。 */
export interface AccountMetaEntry {
  note?: string;
  display_field?: DisplayField | "";
}

export interface AccountsMetaResponse {
  /** 键是账号 ID（凭据文件名）。 */
  accounts: Record<string, AccountMetaEntry>;
  /** 备注长度上限（与后端 accountmeta.MaxNoteRunes 同源）。 */
  note_max_length: number;
}

/** PATCH /panel/api/accounts/{id}/meta 的响应。 */
export interface AccountMetaUpdateResponse {
  ok: boolean;
  meta: AccountMetaEntry;
}

/**
 * POST /panel/api/accounts/checkin_all 的结构化计数
 * （对齐 wb-switch 的 checkin_all 汇总）。
 */
export interface CheckinAllSummary {
  success: number;
  already: number;
  failed: number;
  /** 未参与签到：已关闭自动签到 / 该站点无签到接口 / 账号被禁用。 */
  skipped: number;
  /** 为真表示整轮因不在签到时间段内被跳过（此时计数为 0，积分仍会刷新）。 */
  outside_window: boolean;
  first_error?: string;
}

// -----------------------------------------------------------------------------
// 批次 5：设置页（签到日志 / 日志落点）
// -----------------------------------------------------------------------------

/** 签到日志一行（GET /panel/api/checkin/logs，倒序返回）。 */
export interface CheckinLogEntry {
  /** Unix 毫秒。 */
  ts: number;
  /** 本地日历日（YYYY-MM-DD），跨午夜时用来归日。 */
  date: string;
  account_id: string;
  account_name: string;
  site: string;
  /** success / already / error。 */
  result: string;
  error?: string;
}

export interface CheckinLogsResponse {
  logs: CheckinLogEntry[];
  /** 本次实际生效的窗口天数（默认 30、最大 30）。 */
  days: number;
  /** 台账保留天数（与后端 creditwatch.CheckinKeepDays 同源）。 */
  retention_days: number;
  /** 台账条数上限（面板需要显示这条说明）。 */
  max_records: number;
}

/** 日志落点（GET /panel/api/logs/paths）。路径来自配置，不存在也照实返回。 */
export interface LogPathsResponse {
  /** 网关数据目录（config.json / 台账 / 凭据自动发现都在这里）。 */
  data_dir: string;
  /** 请求 JSONL 归档目录。 */
  request_log_dir: string;
  request_log_dir_exists: boolean;
  /** 最近写入的请求日志文件绝对路径；没有归档时为空串。 */
  latest_request_log: string;
}

// -----------------------------------------------------------------------------
// 切片 2：监控与调度（GET /panel/api/metrics、/config、/scheduler/run）
// -----------------------------------------------------------------------------

/** 账号在池中的运行态（与 metrics.accounts[].state 对应）。 */
export type AccountRuntimeState = "available" | "cooldown" | "disabled";

/** 冷却类型：soft=软限流，hard=硬冷却，breaker=熔断；""=无冷却。 */
export type CooldownKind = "" | "soft" | "hard" | "breaker";

export interface MetricsSummary {
  total: number;
  active: number;
  cooldown: number;
  disabled: number;
  /** 当前在途请求数合计。 */
  in_flight: number;
  /** 已查到额度的账号的剩余积分合计。 */
  credits_remaining: number;
  /** 已查到额度的账号数。 */
  quota_known: number;
}

/** 单账号运行指标（GET /panel/api/metrics → accounts[]）。 */
export interface AccountMetric {
  id: string;
  site: AccountSite;
  site_label: string;
  nickname: string;
  uid: string;
  state: AccountRuntimeState;
  cooldown_kind: CooldownKind;
  /** Unix 秒，0=无冷却。 */
  cooldown_until: number;
  cooldown_reason: string;
  in_flight: number;
  /** 单账号在途上限；<=0 视为不限。 */
  max_in_flight: number;
  success_count: number;
  failure_count: number;
  /** 连续失败计数（熔断判定用）。 */
  fails: number;
  /** Unix 秒，0=未熔断。 */
  breaker_until: number;
  /** 未查询过额度时为 null。 */
  credits_remaining: number | null;
  /** Unix 秒，0=从未使用。 */
  last_used_at: number;
  last_error: string;
}

/** 单模型统计（GET /panel/api/metrics → models[]）。 */
export interface ModelMetric {
  model: string;
  requests: number;
  failures: number;
  /** 无样本时为 null。 */
  avg_ttft_ms: number | null;
  /** 无样本时为 null。 */
  avg_total_ms: number | null;
  /** 上游 usage 累计输出 token。 */
  tokens_total: number;
  available_accounts: number;
}

/** 会话粘性运行时快照。 */
export interface StickyInfo {
  enabled: boolean;
  bindings: number;
  ttl_seconds: number;
}

export interface MetricsResponse {
  uptime_seconds: number;
  summary: MetricsSummary;
  accounts: AccountMetric[];
  models: ModelMetric[];
  sticky: StickyInfo;
}

/** 可编辑配置块：冷却与熔断。 */
export interface ConfigCooldown {
  soft_rate_seconds: number;
  soft_rate_max_seconds: number;
  degrade_threshold: number;
  degrade_cooldown_seconds: number;
  degrade_cooldown_max_seconds: number;
}

/** 可编辑配置块：账号池。 */
export interface ConfigPool {
  max_in_flight: number;
  max_in_flight_global: number;
  breaker_threshold: number;
  breaker_cooldown_seconds: number;
  breaker_cooldown_max_seconds: number;
  idle_weight_per_hour: number;
  idle_weight_max: number;
  /**
   * 免费站点优先：同一模型在国内站/国际站之间只有「一侧确认免费、另一侧确认收费」时，
   * 把调度倾斜到免费那侧。倾斜是软优先，优先站点不可用时自动回落。
   */
  prefer_free_site: boolean;
  /**
   * 积分保底：账号余额**已实测**低于该值时，不再让它承接「实测收费」的模型。
   *
   * 目的是防止收费请求把最后一点余额打穿 —— 余额归零后连免费模型都会
   * 被上游冷却到次日签到（最坏约 11 小时不可用）。0 = 关闭。
   */
  credit_floor: number;
}

/** 可编辑配置块：会话粘性。 */
export interface ConfigSessionSticky {
  enabled: boolean;
  ttl_seconds: number;
  gc_interval_seconds: number;
}

/** 可编辑配置块：定时任务。 */
export interface ConfigSchedule {
  checkin_enabled: boolean;
  checkin_hours: number[];
  /**
   * 签到允许时间段（"HH:MM"，左闭右开）。两个都为空 = 不限制。
   *
   * 只在「遵守时间段」的调用上生效：排程签到与面板的「刷新并签到」。
   * 用户直接点单账号「签到」保持立即语义，不会被窗口否决。
   */
  checkin_start: string;
  checkin_end: string;
  /**
   * 自动签到排除名单（账号 ID）。名单内账号由排程与批量签到跳过，
   * 单账号手动签到不受影响。本批次只读回显，设置入口在批次 5。
   */
  checkin_excluded_accounts: string[];
  keepalive_enabled: boolean;
  keepalive_hours: number[];
  /**
   * 排程保活阈值（天，批次 5）：0 = 每天无条件刷新全部账号；
   * 大于 0 时只刷新剩余有效期不足该天数的账号。手动「令牌保活」不受它限制。
   */
  keepalive_days: number;
  /**
   * 排程保活惰性窗口（小时，批次 5）：距上次成功刷新不足该小时数的账号跳过，
   * 避免同一天多个保活时点重复刷新。非正数后端按默认 24 处理。
   */
  lazy_refresh_hours: number;
  balance_refresh_enabled: boolean;
  balance_refresh_minutes: number;
  /** 任务中心：每日自动执行（报名 + 可自动完成 + 领奖）。 */
  tasks_enabled: boolean;
  tasks_hours: number[];
  /** 夜猫子任务：计分窗口 23:00-08:00，默认 23 点跑。 */
  blackcat_enabled: boolean;
  blackcat_hours: number[];
  /** 旅行巡检：默认 09/21 两点（出发与领奖各需一趟才闭环）。 */
  travel_enabled: boolean;
  travel_hours: number[];
  /** 活跃上报：默认 10 点，点亮连登与领养前置。 */
  activity_enabled: boolean;
  activity_hours: number[];
  /**
   * 保号类四任务（签到 / 活跃上报 / 保活 / 余额刷新）是否覆盖**已禁用**账号。
   *
   * 默认 false = 禁用即跳过（既有行为）。打开后禁用号照常保号但依旧不参与选号 ——
   * 适合「一次只放开一个号、用禁用做流量开关」的轮换养号用法。
   */
  include_disabled_in_tasks: boolean;
}

/** 提示词模式：透传 / 追加（默认）/ 替换。 */
export type PromptMode = "passthrough" | "append" | "custom";

/** 可编辑配置块：提示词与出站改写（切片 4）。 */
export interface ConfigPrompt {
  mode: PromptMode;
  /** 网关自有提示词正文；空串 = 使用内置默认提示词。 */
  text: string;
  /** 提示词文件路径；非空时优先于 text（由后端读取）。 */
  file: string;
  /** 出站指纹脱敏开关。 */
  sanitize: boolean;
  /** 被内容策略拦截后换中性提示词重试一次。 */
  degraded_retry: boolean;
  /** 首条消息非 system 时补一条保底 system。 */
  strict_first_system: boolean;
  /** 只读：当前实际生效的提示词前 200 字，用于确认到底在用什么。 */
  effective_text_preview: string;
}

/** 入站 HTTP 参数（装配期字段：保存后需重启进程才生效）。 */
export interface ConfigServer {
  /** 入站请求读取（含 body 上传）总时长上限，形如 "300s"；"0" = 不限制。 */
  read_timeout: string;
}

/** 只读：上游超时与代理。 */
export interface ConfigUpstream {
  header_timeout_seconds: number;
  idle_timeout_seconds: number;
  proxy: boolean;
}

/** 只读：模型过滤名单。 */
export interface ConfigModelsFilter {
  blocklist: string[];
  allowlist: string[];
}

/** GET /panel/api/config：可安全展示的配置视图（不含明文密钥）。 */
export interface PanelConfig {
  listen: string;
  auth_check_enabled: boolean;
  /**
   * 网关**实际配置**的访问密钥；未配置时是空串。
   *
   * 下发它是为了让「模型与档位」页能把 Base URL + 真实密钥一起复制出去接别的工具。
   * 该接口本身在鉴权之后（配了密钥就必须带对才能进来），调用方本来就在
   * Authorization 头里带着这个值，所以不构成新的泄露面。
   */
  api_key: string;
  credential_source: string;
  /** 凭据热加载扫描间隔，秒。 */
  reload_interval: number;
  /** 模型目录刷新间隔，分钟。 */
  models_refresh: number;
  upstream: ConfigUpstream;
  /** 入站 HTTP 参数（保存后需重启生效）。后端未下发时按缺省 300s 兜底。 */
  server?: ConfigServer;
  debug_enabled: boolean;
  models_filter: ConfigModelsFilter;
  cooldown: ConfigCooldown;
  pool: ConfigPool;
  session_sticky: ConfigSessionSticky;
  schedule: ConfigSchedule;
  prompt: ConfigPrompt;
  /** 任务中心策略（切片 18）。 */
  tasks: ConfigTasks;
}

export interface ConfigTasks {
  /**
   * 允许为「需电脑端」类任务上报**客户端指纹事件链**。
   *
   * 默认 false。打开意味着按抓包样本复刻的形状去伪装客户端行为 ——
   * 形状是确定的，但上游是否接受无法离线验证，且伪造事件有被风控识别的风险。
   */
  desktop_events_enabled: boolean;
}

/** POST /panel/api/config：任意子集，只传要改的字段。 */
export interface ConfigPatch {
  cooldown?: Partial<ConfigCooldown>;
  pool?: Partial<ConfigPool>;
  session_sticky?: Partial<ConfigSessionSticky>;
  schedule?: Partial<ConfigSchedule>;
  /** 入站 HTTP 参数；改动需重启进程。 */
  server?: Partial<ConfigServer>;
  /** 任务中心策略。 */
  tasks?: Partial<ConfigTasks>;
  /** 提示词段；effective_text_preview 是只读回显，不能写。 */
  prompt?: Partial<Omit<ConfigPrompt, "effective_text_preview">>;
  /** 模型黑白名单；传整个名单数组，后端以 config.models_filter 为权威值。 */
  models?: Partial<ConfigModelsFilter>;
}

export interface SaveConfigResponse {
  /** 已热生效的字段路径，如 "cooldown.soft_rate_seconds"。 */
  applied: string[];
  /** 需重启进程才生效的字段路径。 */
  need_restart: string[];
  config: PanelConfig;
}

/** 可手动触发的定时任务。 */
export type SchedulerTask =
  | "checkin"
  | "keepalive"
  | "balance"
  | "tasks"
  | "blackcat"
  /** 旅行巡检：对每个账号推进一趟猫猫旅行（领养 / 出发 / 领奖）。 */
  | "travel"
  /** 活跃上报：每个账号补一条对话活跃事件。 */
  | "activity";

export interface SchedulerRunResponse {
  ok: boolean;
  task: SchedulerTask;
  detail: string;
  /** 整轮被跳过（目前只有「不在签到时间段内」这一种）。 */
  skipped?: boolean;
  /** 跳过的机器可读原因，如 outside_checkin_window。 */
  reason?: string;
}

// -----------------------------------------------------------------------------
// 切片 3：模型与档位（GET /panel/api/models、POST /models/refresh、POST /models/probe）
// -----------------------------------------------------------------------------

/** 价格判定：free=实测免费，paid=实测收费，""=未知。 */
export type ModelVerdict = "free" | "paid" | "";

/** 模型所属站点键，与账号站点同口径。 */
export type ModelSiteKey = AccountSite;

/** 单站点价格探测快照；从未探测过时为 null。 */
export interface ModelProbe {
  /** Unix 秒，0=未探测。 */
  last_probe_at: number;
  cost: number;
  credit: number;
  tokens: number;
  detail: string;
}

/** 模型在单个站点上的倍率与价格信息。 */
export interface ModelSiteInfo {
  /** 是否已确认该站点的价格。 */
  known: boolean;
  /** 未知时为 null；倍率数值由后端给出。 */
  multiplier: number | null;
  /**
   * 后端算好的展示文案，前端直接显示、不自行换算。可能取值：
   * `0.00x`（免费）/ `0.29x`（有效倍率）/ `收费(倍率未知)` / `-`（未知）。
   */
  multiplier_label: string;
  /** free | paid | ""（未知）。 */
  verdict: ModelVerdict | string;
  /** 促销标签，空串=无。 */
  promo_label: string;
  /** 促销截止文案，空串=无。 */
  promo_until: string;
  /** 上游倍率原文，形如 `x0.29 credits`；空串=上游未给。 */
  credits: string;
  probe: ModelProbe | null;
}

/** 模型的两个站点槽位；后端未覆盖某站点时该键不存在。 */
export interface ModelSites {
  cn?: ModelSiteInfo;
  intl?: ModelSiteInfo;
}

/** 模型目录中的单个模型。 */
export interface PanelModel {
  id: string;
  name: string;
  /** 上游中文描述（有中文时优先）；空串=上游未给。 */
  description: string;
  /** 0 = 上游未标注。 */
  context_length: number;
  /** 0 = 上游未标注。 */
  max_output_tokens: number;
  /** 上游允许的最大请求尺寸；0 = 未标注。 */
  max_allowed_size: number;
  /** 是否上游的默认模型。 */
  is_default: boolean;
  /** 上游声明支持推理；部分模型该字段缺失，缺省不等于「不支持」。 */
  reasoning_supported?: boolean | null;
  /** 仅推理：该模型不接受关闭思考。 */
  reasoning_only?: boolean | null;
  /** 是否接受图片输入。 */
  supports_images: boolean;
  /** 是否支持工具调用。 */
  supports_tool_call: boolean;
  /** 当前可调度该模型的账号数（已计入账号冷却与模型级冷却）。 */
  available_accounts: number;
  /** 本进程累计请求数。 */
  requests: number;
  /** 成功 / 失败次数（失败含选号失败、上游中断、聚合失败等）。 */
  success?: number;
  failed?: number;
  /** 上游 usage 累计 token（没有 usage 的请求不计入，不做估算）。 */
  tokens?: number;
  /** 最近 5 小时窗口内的平均首字耗时 / 平均总耗时（毫秒）；无样本为 null。 */
  avg_ttft_ms?: number | null;
  avg_total_ms?: number | null;
  /** 最近一次请求时间（Unix 秒）；0 表示本进程还没跑过这个模型。 */
  last_request_at?: number;
  /** 最近一次请求的结果：成功 / 失败:<原因>。 */
  last_status?: string;
  /** 是否被网关黑白名单禁用。 */
  blocked: boolean;
  sites: ModelSites;
  /** 缺省（或为 null）时代表上游不标注推理档位。 */
  reasoning_supported_efforts?: string[] | null;
  reasoning_default_effort?: string | null;
  /** 是否允许关闭思考；缺失=上游未标注。 */
  reasoning_can_disable?: boolean | null;
  /**
   * 上游原始标签。其中 `badge:<名称>:#<十六进制色>` 是**徽章定义**
   * （如 `badge:限时免费:#FF0000`），其余是普通标签（如 `craft`、`text-to-image`）。
   * 解析见 lib/model-view.ts 的 parseModelBadges / plainTags。
   */
  tags: string[];
  /** 上游厂商标识码。上游只给单字母，未给出可读名，因此只作次要信息展示。 */
  vendor: string;
}

/** 站点目录概览（models.sites[]）。 */
export interface ModelSiteSummary {
  site: AccountSite | string;
  label: string;
  model_count: number;
  accounts: number;
}

/** 价格探测汇总。 */
export interface ProbeSummary {
  free: number;
  paid: number;
  unknown: number;
  /** Unix 秒，0=从未探测。 */
  last_probe_at: number;
}

/** GET /panel/api/models：模型目录快照。 */
export interface ModelsResponse {
  /** 形如 live-api@2026-09-24T17:41:39+08:00。 */
  source: string;
  /** Unix 秒。 */
  fetched_at: number;
  model_count: number;
  sites: ModelSiteSummary[];
  models: PanelModel[];
  probe_summary: ProbeSummary;
}

/** POST /panel/api/models/refresh：目录刷新结果。 */
export interface RefreshModelsResponse {
  ok: boolean;
  source: string;
  model_count: number;
}

/** 单个探测目标。 */
export interface ModelProbeTarget {
  site: AccountSite;
  model: string;
}

/** POST /panel/api/models/probe：单个与批量二选一。 */
export type ModelsProbeRequest = ModelProbeTarget | { models: ModelProbeTarget[] };

export interface ModelProbeResult {
  site: AccountSite;
  model: string;
  verdict: ModelVerdict | string;
  cost: number;
  credit: number;
  tokens: number;
  detail: string;
}

export interface ModelsProbeResponse {
  ok: boolean;
  results: ModelProbeResult[];
}

// -----------------------------------------------------------------------------
// 切片 4：出站改写与协议面（GET /panel/api/logs、POST /panel/api/logs/clear）
// -----------------------------------------------------------------------------

export type LogLevel = "info" | "warn" | "error";

export type LogChannel = "request" | "outbound" | "governance" | "task" | "system";

/** 事件附加字段的值；后端给的是任意键值对象，值类型不固定。 */
export type LogFieldValue = string | number | boolean;

/** 单条事件日志（环形缓冲，新的在前）。 */
export interface LogEvent {
  id: number;
  /** Unix 秒。 */
  ts: number;
  level: LogLevel | string;
  channel: LogChannel | string;
  /** 相关模型，可能为空串。 */
  model: string;
  /** 相关账号凭据文件名，可能为空串。 */
  account: string;
  /** 机器可读的事件名，如 content_block_degraded_retry。 */
  event: string;
  message: string;
  /** 任意键值对象，展开详情时逐行展示；Go 侧 nil map 会序列化成 null。 */
  fields: Record<string, LogFieldValue> | null;
}

/** 单频道的日志条数（logs.channels[]）。 */
export interface LogChannelCount {
  channel: string;
  count: number;
}

/** GET /panel/api/logs：事件日志快照。 */
export interface LogsResponse {
  events: LogEvent[];
  /** 缓冲区中出现的频道及条数。 */
  channels: LogChannelCount[];
  /** 各级别条数；条数为 0 的级别可能缺键。 */
  levels: Partial<Record<LogLevel, number>>;
  /** 环形缓冲上限。 */
  capacity: number;
  /** 缓冲区中的事件总数。 */
  total: number;
}

/** POST /panel/api/logs/clear。 */
export interface ClearLogsResponse {
  ok: boolean;
  cleared: number;
}

// -----------------------------------------------------------------------------
// 切片 5：任务中心（GET/POST /panel/api/tasks*、/panel/api/growth/action）
// -----------------------------------------------------------------------------

/** 任务待办分类：可领奖 / 可报名 / 可自动完成 / 需人工 / 已完成。 */
export type TaskCategory = "claimable" | "accept" | "auto" | "manual" | "done";

/** 队列项的动作类型。 */
export type TaskQueueKind = "accept" | "auto" | "claim";

/** 队列项状态。 */
export type TaskQueueStatus = "pending" | "running" | "ok" | "failed" | "skipped";

/** 单账号的任务扫描摘要。 */
export interface TaskAccountScan {
  uid: string;
  id: string;
  nickname: string;
  site: AccountSite;
  site_label: string;
  task_count: number;
  pending_count: number;
  claimable_count: number;
  auto_count: number;
  manual_count: number;
  /** 连登天数；成长域不可用时为 0。 */
  streak: number;
  /** 旅行状态：""=未出发，"travelling"=旅行中，"arrived"=已到达。 */
  travel_state?: string;
  travel_reward?: number;
  lottery_chances?: number;
  /** 非空表示该账号读取任务失败。 */
  error?: string;
  /** 中性标注（不是故障），例如「国际站没有成长中心活动」。与 error 分开渲染。 */
  note?: string;
}

/** 聚合到 task_code 的任务视图（多账号合并）。 */
export interface GrowthTaskView {
  task_code: string;
  title: string;
  condition: string;
  description?: string;
  credit: number;
  energy: number;
  category: TaskCategory;
  /** 可自动/可领奖/可报名时的一句动作说明。 */
  action?: string;
  /** manual 分类时的具体原因。 */
  action_reason?: string;
  locked: boolean;
  mp_only: boolean;
  accept_status?: string;
  target: number;
  current: number;
  accounts: string[];
  account_count: number;
  claimable_count: number;
}

export interface TaskTotals {
  tasks: number;
  claimable: number;
  acceptable: number;
  auto: number;
  manual: number;
  done: number;
}

export interface TaskScanResponse {
  scanned_at: number;
  support_warning?: string;
  accounts: TaskAccountScan[];
  tasks: GrowthTaskView[];
  totals: TaskTotals;
}

export interface TaskRunOptions {
  accounts?: string[];
  task_codes?: string[];
  /** 空数组等价于三种都做。 */
  actions?: TaskQueueKind[];
}

export interface TaskRunResponse {
  ok: boolean;
  /**
   * 计划项数。
   *
   * -1 表示「已受理，项数待定」：后端不再阻塞等扫描结果，项数要轮询队列才知道。
   * 0 表示没有可执行的账号或待办任务。正数表示确切的项数（旧行为，向后兼容）。
   */
  planned: number;
  /** 请求是否已被受理（进入准备或执行阶段）。 */
  accepted?: boolean;
  detail?: string;
}

export interface TaskQueueItem {
  uid: string;
  account: string;
  account_name?: string;
  task_code: string;
  title?: string;
  kind: TaskQueueKind;
  status: TaskQueueStatus;
  message?: string;
  started_at?: number;
  finished_at?: number;
}

/** 队列阶段。`preparing` 是「正在扫描待办」，此时还没有项数。 */
export type TaskQueuePhase = "idle" | "preparing" | "running" | "done";

export interface TaskQueueState {
  running: boolean;
  /** 机读阶段；老后端可能不返回，前端需容忍缺省。 */
  phase?: TaskQueuePhase;
  /** 准备阶段的可读说明（如「正在扫描 2 个账号的任务…」）。 */
  prepare_hint?: string;
  started_at: number;
  finished_at?: number;
  total: number;
  done: number;
  failed: number;
  note?: string;
  items: TaskQueueItem[];
}

export interface TaskActionResponse {
  ok: boolean;
  action?: string;
  detail?: string;
}

// -----------------------------------------------------------------------------
// 账号级任务动作（面板「一键完成」弹窗）
// -----------------------------------------------------------------------------

/** 单项任务动作的结果。 */
export interface AccountTaskResultItem {
  task_code: string;
  description?: string;
  /** done = 执行成功；skipped = 无需动作（已完成/不在窗口）；error = 失败。 */
  status: "done" | "skipped" | "error";
  message: string;
  /** 动作前的进度（如 "2/5"）。 */
  progress_before?: string;
  /** 动作后回读到的进度。 */
  progress_after?: string;
  claimable?: boolean;
  claimed?: boolean;
  credit?: number;
  energy?: number;
  claim_error?: string;
  /** 尝试型动作：上游未证实可脚本化，跑了也可能不点亮。 */
  attempt?: boolean;
}

/** POST /accounts/{id}/tasks/auto 的响应。 */
export interface AccountTaskAutoResponse extends TaskActionResponse {
  message?: string;
  result?: AccountTaskResultItem;
  progress_before?: string;
  progress_after?: string;
  claimable?: boolean;
  claimed?: boolean;
  credit?: number;
  energy?: number;
  attempt?: boolean;
}

/** POST /accounts/{id}/tasks/auto_all 的响应。 */
export interface AccountTaskAutoAllResponse {
  ok: boolean;
  detail?: string;
  results?: AccountTaskResultItem[];
}

/** 开学季券码条目。 */
export interface TaskVoucherItem {
  grant_id: number;
  prize_name?: string;
  sku_code?: string;
  code: string;
  valid_from?: string;
  valid_to?: string;
  granted_at?: string;
}

/** 券码按账号分组返回（券码要复制去核销，拍平后分不清属于哪个号）。 */
export interface TaskVoucherGroup {
  account: string;
  uid: string;
  nickname: string;
  site_label: string;
  vouchers: TaskVoucherItem[];
  /** 中性标注（如「国际站没有开学季活动」）。 */
  note?: string;
  error?: string;
}

export interface TaskVouchersResponse {
  ok: boolean;
  detail?: string;
  groups?: TaskVoucherGroup[];
  total?: number;
}

// -----------------------------------------------------------------------------
// 切片 6：用量统计（GET /panel/api/stats）
// -----------------------------------------------------------------------------

/** 时间序列上的一个小时点（缺失小时已由后端补 0）。 */
export interface StatsPoint {
  ts: number;
  label: string;
  requests: number;
  failures: number;
  tokens: number;
  /** 该小时最后一次余额采样；没有采样时为 null。 */
  credits: number | null;
}

export interface StatsTotals {
  requests: number;
  failures: number;
  tokens: number;
  /** 窗口内最后一次余额采样值。 */
  credits_now: number | null;
  /** 窗口内首尾余额差（正数表示这段时间消耗的积分）。 */
  credits_used: number | null;
  /** 窗口内有请求的小时数。 */
  hours: number;
}

export interface StatsModelRollup {
  model: string;
  requests: number;
  failures: number;
  tokens: number;
  avg_ttft_ms: number | null;
}

export interface StatsResponse {
  range_hours: number;
  generated_at: number;
  since_at: number;
  totals: StatsTotals;
  series: StatsPoint[];
  top_models: StatsModelRollup[];
  hourly_avg_tokens: number;
}

export interface StatsPurgeResponse {
  ok: boolean;
}

// -----------------------------------------------------------------------------
// 切片 10：按天统计（GET /panel/api/stats/daily）
//
// 与上面的「按小时」结构刻意分开：按天视图要回答「这段时间的趋势与构成」，
// 所以额外带输入/输出拆分与账号维度；按小时视图只回答「最近 24 小时快不快」。
// -----------------------------------------------------------------------------

export interface DayPoint {
  ts: number;
  label: string;
  requests: number;
  failures: number;
  tokens: number;
  input_tokens: number;
  output_tokens: number;
  credits: number | null;
}

export interface AccountRollup {
  account: string;
  requests: number;
  failures: number;
  tokens: number;
  input_tokens: number;
  output_tokens: number;
  avg_ttft_ms: number | null;
}

export interface DailyTotals {
  requests: number;
  failures: number;
  tokens: number;
  input_tokens: number;
  output_tokens: number;
  credits_now: number | null;
  credits_used: number | null;
  /** 窗口内「有请求」的天数。算日均要用它做分母，否则会被空天摊薄。 */
  active_days: number;
}

export interface DailySnapshot {
  range_days: number;
  generated_at: number;
  since_at: number;
  totals: DailyTotals;
  series: DayPoint[];
  top_models: StatsModelRollup[];
  accounts: AccountRollup[];
}

// -----------------------------------------------------------------------------
// 切片 6 / 12：官方口径用量（GET /panel/api/stats/official）
//
// 字段形状对齐 wb-switch 的 CreditOfficialUsage，以便 1:1 复用它的页面版式。
// 同时保留 `days[].requests` / `accounts[].total` / `ok_count` 等**旧字段**——
// lib/usage-split.ts 依赖它们做「反代 vs 官方直连」的差分。
// -----------------------------------------------------------------------------

/** 某一天里单个模型的消耗。 */
export interface OfficialUsageDayModel {
  model: string;
  requests: number;
  credit: number;
}

export interface OfficialUsageDay {
  date: string;
  requests: number;
  credit: number;
  /** 该天的模型构成；后端已按消耗降序。无流量日为空数组。 */
  models?: OfficialUsageDayModel[];
}

export interface OfficialUsageModel {
  model: string;
  requests: number;
  credit: number;
}

export interface OfficialUsageClient {
  client: string;
  requests: number;
}

/** 官方账本的整体可用状态。 */
export type OfficialUsageStatus = "complete" | "partial" | "unavailable";

export interface OfficialUsageAccount {
  id: string;
  nickname: string;
  /** 站点键：cn=国内版，intl=国际版。 */
  site: string;
  site_label: string;
  /** false 时 error 一定有值，各窗口值与 days/models 均为空。 */
  ok: boolean;
  error?: string;
  requests: number;
  credit: number;
  credit_today: number;
  /** 今日消耗（与 credit_today 同值，命名对齐 switch）。 */
  usage_today: number;
  usage_7d: number;
  usage_this_month: number;
  request_count: number;
  /** 上游自报的真实条数，用于识别分页截断。 */
  reported_total: number;
  /** 单账号明细条数上限。 */
  detail_limit: number;
  detail_truncated: boolean;
  days: OfficialUsageDay[];
  models: OfficialUsageModel[];
  /** 兼容旧字段：上游自报条数。 */
  total: number;
}

/** 一条官方请求明细（带账号归属，便于按账号筛选）。 */
export interface OfficialUsageRequestRow {
  account_id: string;
  account_name: string;
  request_time: string;
  model: string;
  credit: number;
  client?: string;
  request_id?: string;
}

export interface OfficialUsageError {
  account_id: string;
  account_name: string;
  error: string;
}

export interface OfficialUsageSummary {
  usage_today: number;
  usage_7d: number;
  usage_this_month: number;
  requests: number;
  credit: number;
}

export interface OfficialUsageResponse {
  fetched_at: number;
  /** Unix 毫秒，页头「当前数据更新于 …」。 */
  collected_at: number;
  status: OfficialUsageStatus;
  range_start: string;
  range_end: string;
  range_days: number;
  detail_limit_per_account: number;
  summary: OfficialUsageSummary;
  /** 窗口内**每一天**都有（无流量日补 0），X 轴才不会跳过空白日。 */
  days: OfficialUsageDay[];
  models: OfficialUsageModel[];
  clients: OfficialUsageClient[];
  accounts: OfficialUsageAccount[];
  requests: OfficialUsageRequestRow[];
  errors: OfficialUsageError[];
  /** 与 requests 同内容，保留旧字段名。 */
  rows: OfficialUsageRequestRow[];
  ok_count: number;
  fail_count: number;
}

// -----------------------------------------------------------------------------
// 切片 12：积分资源包（GET /panel/api/credits）
// -----------------------------------------------------------------------------

/** 单个积分资源包（后端已脱敏：只有数值与到期时间）。 */
export interface CreditResource {
  package_code: string;
  package_name: string;
  total: number;
  remaining: number;
  used: number;
  /** Unix 毫秒；0 表示长期有效。 */
  expire_at: number;
  expired: boolean;
  /** 7 天内到期。 */
  expiring_soon: boolean;
  /** 上游原始状态码；-1 表示上游未给。 */
  status: number;
}

export interface PanelCreditAccount {
  id: string;
  nickname: string;
  site: string;
  site_label: string;
  ok: boolean;
  error?: string;
  resources: CreditResource[];
}

export interface CreditsResponse {
  fetched_at: number;
  collected_at: number;
  summary: {
    current_remaining: number;
    current_capacity: number;
  };
  accounts: PanelCreditAccount[];
  ok_count: number;
  fail_count: number;
}

// -----------------------------------------------------------------------------
// 切片 19：积分本地观察口径（GET /panel/api/credits/stats）
//
// 官方用量接口不可用时的回退依据：每次成功拉取资源包记一条余额快照，
// 「连续快照余额下降的正差值」即消耗（对齐 wb-switch 的 get_credit_statistics）。
// -----------------------------------------------------------------------------

export interface CreditStatsDailyPoint {
  date: string;
  usage: number;
}

export interface CreditStatsAccount {
  account_id: string;
  account_name: string;
  /** cn=国内版，intl=国际版。 */
  site: string;
  is_current: boolean;
  /** 仅 is_current 的账号给余额（与 switch 的「只统计当前账号」同口径）。 */
  current_remaining: number | null;
  total_capacity: number | null;
  last_snapshot_at: number | null;
  usage_today: number;
  usage_7d: number;
  usage_this_month: number;
  checked_in_today: boolean | null;
  /** success / already / error。 */
  checkin_status_today: string | null;
  last_checkin_at: number | null;
  last_checkin_result: string | null;
  daily: CreditStatsDailyPoint[];
}

export interface CreditStatsSummary {
  current_remaining: number;
  current_capacity: number;
  usage_today: number;
  usage_7d: number;
  usage_this_month: number;
  today_checked_in_accounts: number;
  today_success: number;
  today_already: number;
  today_failed: number;
}

export interface CreditStatsEvent {
  kind: "usage" | "checkin";
  ts: number;
  date: string;
  account_id: string;
  account_name: string;
  site: string;
  amount?: number;
  result?: string;
  error?: string;
}

export interface CreditStatsResponse {
  generated_at: number;
  retention_days: number;
  /** 最早一条快照的时间；无快照为 null。 */
  coverage_start_at: number | null;
  summary: CreditStatsSummary;
  daily: CreditStatsDailyPoint[];
  accounts: CreditStatsAccount[];
  events: CreditStatsEvent[];
}

// -----------------------------------------------------------------------------
// 切片 7：本机代理（/panel/api/local/capabilities、/panel/local/*）
// -----------------------------------------------------------------------------

/** 本机代理运行状态：disabled | missing | starting | running | stopped | failed。 */
export type LocalAgentState =
  | "disabled"
  | "missing"
  | "starting"
  | "running"
  | "stopped"
  | "failed";

export interface LocalCapabilities {
  enabled: boolean;
  state: LocalAgentState;
  /** 本机能力是否可用（前端据此决定是否显示本机页）。 */
  available: boolean;
  local: boolean;
  port?: number;
  started_at?: number;
  restarts?: number;
  last_probe?: number;
  agent_version?: string;
  agent_pid?: number;
  /** "readonly" 表示当前实现只读，不做写本机操作。 */
  mode?: string;
  agent_capabilities?: string[];
  /** 契约里有但本实现未做（如会话复制、进程扫描）。 */
  unimplemented?: string[];
  bin?: string;
  /** state 为 missing 时给出查找过的路径。 */
  searched_paths?: string[];
  last_error?: string;
  reason?: string;
  proxy_prefix?: string;
}

export interface LocalDatabaseInfo {
  name: string;
  size: number;
  mtime: number;
}

export interface LocalLogSummary {
  dir: string;
  file_count: number;
  latest_date: string;
  dates?: string[];
}

export interface LocalOverview {
  data_dir: string;
  data_dir_exists: boolean;
  scanned_at: number;
  counts?: {
    sessions: number;
    projects: number;
    skills: number;
    plugins: number;
  };
  client?: {
    settings_present: boolean;
    app_config?: Record<string, unknown>;
    from_log?: Record<string, unknown>;
  };
  databases?: LocalDatabaseInfo[];
  logs?: LocalLogSummary;
  storage?: {
    size_bytes: number;
    computed_at: number;
    cached: boolean;
  };
}

export interface LocalLogFile {
  name: string;
  size: number;
  mtime: number;
}

export interface LocalLogsResponse {
  logs_dir?: string;
  files?: LocalLogFile[];
  /** 带 file 参数时返回尾部内容。 */
  file?: string;
  lines?: string[];
}

export interface LocalHookEntry {
  event: string;
  entries: number;
  preview?: { matcher?: string; commands?: string[] }[];
}

export interface LocalHooksResponse {
  present: boolean;
  hooks?: LocalHookEntry[];
  hook_count?: number;
  settings_keys?: string[];
  message?: string;
  error?: string;
}

export interface LocalPathEntry {
  path: string;
  rel: string;
  exists: boolean;
  is_dir?: boolean;
  size?: number;
  mtime?: number;
}

export interface LocalPathsResponse {
  data_dir: string;
  paths: LocalPathEntry[];
}

// -----------------------------------------------------------------------------
// 切片 8：双站视图（GET /panel/api/sites）
// -----------------------------------------------------------------------------

/** 单站点上的账号汇总（口径与监控页一致）。 */
export interface SiteAccountStat {
  total: number;
  active: number;
  cooldown: number;
  disabled: number;
  in_flight: number;
  credits_remaining: number;
  quota_known: number;
}

/** 该站点上的收费模型（已按倍率降序，最多 40 条）。 */
export interface SitePaidModel {
  id: string;
  /** 后端算好的展示文案，前端直接展示，不要自己算。 */
  multiplier_label: string;
  multiplier: number;
}

export interface SiteCatalogStat {
  /** 该站点本轮目录里的模型数；0 表示拉不到（通常是没有可用凭据）。 */
  models: number;
  /** 目录快照时间，0 表示本轮没有该站点数据。 */
  fetched_at: number;
  source: string;
}

export interface SiteProbeStat {
  free: number;
  paid: number;
  unknown: number;
  last_probe_at: number;
}

export interface SitePricing {
  free: number;
  paid: number;
  unknown: number;
  /** 明确免费的模型 ID —— 这是「免费站点优先」真正会用到的集合。 */
  confirmed_free: string[];
  paid_models: SitePaidModel[];
  probe: SiteProbeStat;
}

export interface SiteView {
  site: AccountSite;
  label: string;
  base_url: string;
  origin: string;
  /** 该站点当前有「未禁用且带令牌」的账号，即请求真能打到它。 */
  usable: boolean;
  accounts: SiteAccountStat;
  account_ids: string[];
  catalog: SiteCatalogStat;
  pricing: SitePricing;
  /** 站点备注（解释「为什么这一侧是空的」），空串表示无特别说明。 */
  note: string;
}

/** 一条当前生效的「免费站点优先」倾斜规则。 */
export interface PreferenceRule {
  model: string;
  /** 优先使用的站点展示名。 */
  preferred: string[];
  /** 确认收费、会被尽量避开的站点展示名（结论未知的不会出现在这里）。 */
  avoid: string[];
  reason: string;
  /** 本进程内该模型的请求数。 */
  requests: number;
}

export interface SitesResponse {
  generated_at: number;
  prefer_free_site: boolean;
  sites: SiteView[];
  preference: {
    enabled: boolean;
    rules: PreferenceRule[];
    note: string;
  };
}

// -----------------------------------------------------------------------------
// 站点路由（GET /panel/api/site-route）
//
// 与 SitesResponse 的区别：那个描述「两站现在各是什么状况」，
// 这个描述「怎么让一个请求走指定的一站」。词表由后端下发（后端才是权威），
// 前端只负责渲染，避免界面教出后端不认的写法。
// -----------------------------------------------------------------------------

/** 一种可用的站点指定方式（URL 参数 / 请求头 / 模型名前缀）。 */
export interface SiteRouteOption {
  kind: "query" | "header" | "model_prefix";
  label: string;
  /** 从 1 开始，1 优先级最高。多个来源同时出现时按它判定。 */
  priority: number;
  syntax: string;
  example: string;
  note: string;
}

/** 一个站点可接受的写法集合。 */
export interface SiteRouteAliasGroup {
  site: AccountSite;
  label: string;
  base_url: string;
  origin: string;
  /** 可写进 `?site=` 与请求头的别名。 */
  query_aliases: string[];
  /** 唯一可用于模型名前缀的写法，**含分隔符**（如 `CN-`）；发往上游前会被剥掉。 */
  model_prefix: string;
}

export interface SiteRouteResponse {
  options: SiteRouteOption[];
  sites: SiteRouteAliasGroup[];
  notes: string[];
}

// -----------------------------------------------------------------------------
// 切片 11：Token 统计（GET /panel/api/token-stats）
//
// 字段名与 wb-switch 的同名结构逐字一致，以便 1:1 复用它的页面版式。
// -----------------------------------------------------------------------------

/** 一组 Token 聚合值。 */
export interface TokenTotals {
  /** 展示总量 = input + output + cacheWrite（input 已含 cacheRead，不再相加）。 */
  total: number;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  /** input 里未被缓存命中的部分 = input - cacheRead。 */
  uncachedInput: number;
  records: number;
  /** 缓存命中率 = cacheRead / input；input 为 0 时是 null（不是 0）。 */
  cacheHitRate: number | null;
}

/** 带 key 的聚合项（模型 / 项目 / 会话 / 按天）。 */
export interface TokenGroup extends TokenTotals {
  key: string;
  title?: string | null;
  project?: string;
  sessionId?: string;
}

/** 请求明细行。 */
export interface TokenRequestRow {
  timestamp: number;
  model: string;
  project: string;
  sessionId: string;
  title?: string | null;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  uncachedInput: number;
  thinking: number;
  total: number;
}

/** 数据源类型：本地会话日志（全字段）| 网关反代（只有请求与输入/输出）。 */
export type TokenSourceKind = "local-log" | "gateway";

export interface TokenSource {
  source: string;
  kind: TokenSourceKind;
  label: string;
  available: boolean;
  note?: string;
  summary: TokenTotals;
  models: TokenGroup[];
  projects: TokenGroup[];
  sessions: TokenGroup[];
  daily: TokenGroup[];
  /** 模型 → 该模型的按天序列（趋势图按模型筛选用）。网关来源下为空对象。 */
  dailyByModel: Record<string, TokenGroup[]>;
  requests: TokenRequestRow[];
  filesScanned: number;
  parseErrors: number;
  coverageStartAt?: number | null;
  coverageEndAt?: number | null;
}

export interface TokenStatistics {
  generatedAt: number;
  rangeDays: number;
  sources: TokenSource[];
}

// -----------------------------------------------------------------------------
// 开机自启动（切片 9）
// -----------------------------------------------------------------------------

/** 自启项的宿主类型：桌面壳 / 命令行安装包 / 无法判定。 */
export type AutostartKind = "desktop" | "cli" | "unknown";

/** 检测到的端口冲突（桌面版与命令行版共用 8317 端口）。 */
export interface AutostartConflict {
  kind: AutostartKind;
  /** 冲突方在系统里的名字（计划任务名 / 注册表值名）。 */
  name: string;
  message: string;
}

/**
 * 自启状态。
 *
 * 这是**系统里的真实状态**（注册表 / plist / .desktop），不是网关配置 ——
 * 因此不走 config 的保存链路，单独一个端点。
 */
export interface AutostartStatus {
  supported: boolean;
  enabled: boolean;
  kind: AutostartKind;
  /** 自启项指向的可执行文件（便于用户核对）。 */
  host?: string;
  /** 自启项所在的系统位置，用户想手动改时用得上。 */
  location?: string;
  conflict?: AutostartConflict;
  detail?: string;
}

// -----------------------------------------------------------------------------
// 请求级观测（reqlog）
// -----------------------------------------------------------------------------

/** 一次请求的结局。 */
export type RequestOutcome = "success" | "http_error" | "stream_error" | "interrupted";

/** 一条脱敏请求记录。账号只存「昵称(uid8)」标签，不含完整 UID。 */
export interface RequestEvent {
  time: string;
  request_id: string;
  path: string;
  account?: string;
  model?: string;
  status: number;
  ok: boolean;
  outcome: RequestOutcome | string;
  duration_ms: number;
  ttfb_ms?: number;
  attempts?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  credit?: number;
  credit_known: boolean;
}

/** JSONL 归档的存储状态。 */
export interface RequestArchiveStats {
  enabled: boolean;
  dir?: string;
  files: number;
  bytes: number;
  /** 队列满被丢弃的条数。非 0 说明写入跟不上，但请求没被拖慢。 */
  dropped_writes: number;
  last_error?: string;
}

/** 进程内请求指标（GET /panel/api/request-metrics）。 */
export interface RequestMetricsResponse {
  started_at: string;
  completed: number;
  in_flight: number;
  succeeded: number;
  failed: number;
  success_rate: number;
  http_success_rate: number;
  avg_duration_ms: number;
  /** 最近 100 条，新的在前。 */
  recent: RequestEvent[];
  archive: RequestArchiveStats;
}

/** GET /panel/api/request-logs 的响应。 */
export interface RequestLogsResponse {
  entries: RequestEvent[];
  limit: number;
}

// -----------------------------------------------------------------------------
// 本机会话库（GET /panel/api/local-sessions 等）
// -----------------------------------------------------------------------------

export interface LocalSession {
  id: string;
  cwd: string;
  user_id: string;
  title: string;
  status: string;
  model: string;
  mode: string;
  created_at: number;
  updated_at: number;
  deleted_at: number;
  /** 正文 JSONL 是否存在（缺正文的会话在界面上要区别对待）。 */
  has_body: boolean;
  body_bytes: number;
  /** 用户在客户端里改过的标题（后端已把它并入 title，这里保留原值备查）。 */
  custom_title?: string;
  /** 客户端侧栏的「任务」；其余按 cwd 最后一段归入「空间」（会话树分组用）。 */
  is_playground?: boolean;
  /** 所在会话库的档位（cn/intl）。 */
  variant?: string;
}

export interface LocalSessionOwner {
  account: string;
  label: string;
  /** 该 user_id 是否能在账号池里找到对应账号。 */
  known: boolean;
}

export interface LocalSessionsResponse {
  /** false 表示本机没有会话库（未安装/未使用过客户端），不是错误。 */
  available: boolean;
  db_path?: string;
  sessions: LocalSession[];
  user_ids: string[];
  /** user_id → 账号信息。 */
  owners: Record<string, LocalSessionOwner>;
  note?: string;
}

export interface LocalSessionCopyResult {
  ok: boolean;
  new_id: string;
  body_path: string;
  /** 正文里被重写 sessionId 的行数。 */
  body_lines: number;
  backup_id: string;
  notes: string[];
  error?: { code: number; message: string };
}

// -----------------------------------------------------------------------------
// 本机会话库（GET /panel/api/local-sessions 等）
// -----------------------------------------------------------------------------

export interface LocalSession {
  id: string;
  cwd: string;
  user_id: string;
  title: string;
  status: string;
  model: string;
  mode: string;
  created_at: number;
  updated_at: number;
  deleted_at: number;
  /** 正文 JSONL 是否存在（缺正文的会话在界面上要区别对待）。 */
  has_body: boolean;
  body_bytes: number;
}

export interface LocalSessionOwner {
  account: string;
  label: string;
  /** 该 user_id 是否能在账号池里找到对应账号。 */
  known: boolean;
}

export interface LocalSessionsResponse {
  /** false 表示本机没有会话库（未安装/未使用过客户端），不是错误。 */
  available: boolean;
  db_path?: string;
  sessions: LocalSession[];
  user_ids: string[];
  /** user_id → 账号信息。 */
  owners: Record<string, LocalSessionOwner>;
  note?: string;
}

export interface LocalSessionCopyResult {
  ok: boolean;
  new_id: string;
  body_path: string;
  /** 正文里被重写 sessionId 的行数。 */
  body_lines: number;
  backup_id: string;
  notes: string[];
  error?: { code: number; message: string };
}

/** 同步判定结论（与后端 localsessions.SyncVerdict 一致）。 */
export type SyncVerdict = "identical" | "fast_forward" | "ahead" | "diverge" | "unknown";

export interface SyncDecision {
  verdict: SyncVerdict;
  /** 给用户看的一句话解释。 */
  reason: string;
  source_only: number;
  target_only: number;
  /** 是否允许直接执行（只有快进是零覆盖的）。 */
  can_apply: boolean;
}

export interface LocalSessionSyncResult {
  ok: boolean;
  /** 是否真的写了目标正文。 */
  applied: boolean;
  decision: SyncDecision;
  backup_id: string;
  notes: string[];
  error?: { code: number; message: string };
}

// -----------------------------------------------------------------------------
// 关联会话（GET /panel/api/local-sessions/groups）
//
// 把「同一段对话的多份副本」聚成一组。分组依据是**内容本身**
//（归一化后的有序行摘要），不是登记表 —— 用户手动复制的副本也能进组。
// -----------------------------------------------------------------------------

/**
 * 组状态：与后端 localsessions.GroupStatus 一致。
 *
 * missing = 有成员正文不可读（缺失/损坏），无法确认内容完整；
 * unknown 后端当前不会产生，前端筛选仅在计数 > 0 时显示，保留以兼容未来扩展。
 */
/** 组状态（与 wb-switch 的 summary_status 一致）。 */
export type GroupStatus = "latest" | "behind" | "diverge" | "missing" | "unknown";

/** 单个成员相对组内「内容最全」成员的状态（与后端 localsessions.MemberStatus 一致）。 */
export type MemberVersionStatus =
  | "latest"
  | "behind"
  | "diverge"
  | "missing"
  | "unknown"
  /** 已判定失效但尚未被替换（登记表保留记录）。 */
  | "stale"
  /** 已被同账号的更新成员替换（登记表保留记录）。 */
  | "superseded";

export interface LocalSessionGroupMember {
  /** 登记表里的成员 id（同步/解除都以它为准）。 */
  member_id: string;
  session_id: string;
  uid: string;
  /** active / stale / superseded。 */
  state?: string;
  /** 这份副本所在会话库的档位（cn=国内站 ~/.workbuddy，intl=国际站 ~/.workbuddy-ai）。 */
  variant?: string;
  /** 该 uid 在账号池里对应的账号（匹配不到时为空）。 */
  account: string;
  label: string;
  title: string;
  updated_at: number;
  body_bytes: number;
  record_count: number;
  total_digest: string;
  readable: boolean;
  /** 相对组内「内容最全」成员的内容状态。 */
  version_status: MemberVersionStatus;
  /** 状态解释（「比「甲」少 3 条，可安全追加同步」这类）。 */
  reason: string;
  /**
   * 以此成员为准统一时，其他成员会被替换的独有记录数（**上界预览**，未计同步基线）。
   * 0 表示追加/一致，不覆盖任何内容；> 0 时确认框要给出覆盖警告。
   */
  overwrite_records: number;
  /**
   * 「最近 N 条可读内容」预览（只有**详情**接口返回；列表里为空）。
   *
   * 用途是让人确认「这是哪段对话」再决定以谁为准，而不是读全文 ——
   * 每条文本截断到 240 字符，最多 6 条。
   */
  content_preview?: ContentPreviewItem[];
}

/** 一条内容预览：说话人（用户/助手/记录）+ 截断后的文本。 */
export interface ContentPreviewItem {
  speaker: string;
  text: string;
}

/** 分叉组形状：共同旧版 + 独立分支数（只给计数，不画图）。 */
export interface GroupDivergence {
  /** 内容为共同旧版的成员（关联图里挂在「共同旧版」节点上）。 */
  common_member_ids: string[];
  /** 每条独立更新的成员；内层切片 = 一个互不为前缀的版本（关联图里一条分支一种颜色）。 */
  branch_member_ids?: string[][];
  /** 分支数（= branch_member_ids 的长度；旧后端只给计数）。 */
  branches: number;
}

export interface LocalSessionGroup {
  id: string;
  /** 后端给出的状态摘要文案（与 wb-switch 逐字一致，前端不再自己拼）。 */
  summary_text?: string;
  /** 工作区名（cwd 末段），界面上显示为「项目 X」。 */
  project: string;
  title: string;
  updated_at: number;
  status: GroupStatus;
  reason: string;
  members: LocalSessionGroupMember[];
  platform: string;
  /** 自动批量同步可用的安全源成员（空串表示没有）。 */
  safe_source_member_id: string;
  /** 仅在组存在分叉时给出。 */
  divergence?: GroupDivergence | null;
}

export interface GroupCounts {
  all: number;
  behind: number;
  diverge: number;
  latest: number;
  missing: number;
  unknown: number;
}

/** 会话库存储状态：ok=可读；unavailable=读不到（store_error 给原因，界面显示琥珀横幅）。 */
export type SessionStoreStatus = "ok" | "unavailable";

export interface LocalSessionGroupsResponse {
  available: boolean;
  db_path?: string;
  groups: LocalSessionGroup[];
  counts: GroupCounts;
  /** 正文不可读的会话数（界面上要能解释「为什么少了几条」）。 */
  unreadable: number;
  note?: string;
  store_status?: SessionStoreStatus;
  store_error?: string;
  /** 未完成的会话写入（只读汇总；真正恢复走 /local-sessions/recover）。 */
  pending_recovery?: PendingRecoverySummary;
}

/** 未完成会话写入的只读汇总（复制/同步中途被打断留下的现场）。 */
export interface PendingRecoverySummary {
  /** 未完成操作条数（两个档位合计）。 */
  count: number;
  /** 有未完成操作的档位（cn/intl）。 */
  variants: string[];
  /** 存在无法解析/无法读取的操作记录：必须人工处理，且会阻断后续复制。 */
  unparseable: boolean;
  /** 逐条可读说明。 */
  details: string[];
}

/** POST /panel/api/local-sessions/recover 的响应。 */
export interface RecoveryReportItem {
  recovered: string[];
  abandoned: string[];
  needs_recovery: { operation_id: string; reason: string; retryable: boolean }[];
}

/** 「关联新账号」可选的目标账号（GET 组详情时下发）。 */
export interface SessionAddTarget {
  /** 账号池标识（凭据文件名）。 */
  id: string;
  /** 复制时的会话归属 uid。 */
  uid: string;
  label: string;
  /** cn=国内站，intl=国际站。 */
  site: string;
}

/** GET /panel/api/local-sessions/groups/{id} 的响应。 */
export interface LocalSessionGroupDetailResponse {
  group: LocalSessionGroup;
  add_targets: SessionAddTarget[];
}

/** 组同步报告（POST .../groups/{id}/sync）。 */
export interface GroupSyncItem {
  member_id: string;
  ok: boolean;
  notes: string[];
}

export interface GroupSyncSkip {
  member_id: string;
  reason: string;
  /**
   * 机器可读的跳过原因码：previewStale=预览凭据失配（内容/关联/基线在预览之后变了），
   * recheckNotFastForward=重新检查后不再属于安全快进范围。
   */
  reason_code?: string;
}

export interface GroupSyncError {
  member_id: string;
  error: string;
}

export interface GroupSyncReport {
  /** 本次实际使用的来源成员（自动选择时也给出）。 */
  source_member_id: string;
  synced: GroupSyncItem[];
  skipped: GroupSyncSkip[];
  errors: GroupSyncError[];
  /** 本次写入留下了需要人工处理的现场（客户端已暂停重开）。 */
  needs_recovery?: boolean;
  /** 本次会话操作后重新打开的客户端档位（cn/intl）。 */
  restarted_variants?: string[];
}

/** 复制并关联的结果（POST .../groups/{id}/add）。 */
export interface AddGroupMemberResult {
  ok: boolean;
  /** linked=已复制并登记；alreadyLinked=目标已有有效副本（幂等，不是失败）。 */
  status?: "linked" | "alreadyLinked";
  new_id?: string;
  /** 登记组 id（新组为预分配的 UUID；已有组沿用）。 */
  group_id?: string;
  backup_id?: string;
  member_id?: string;
  notes?: string[];
}

// -----------------------------------------------------------------------------
// 扩展数据仓会话（GET /panel/api/local-sessions/ext 等）
//
// VS Code 内 CodeBuddy 插件 / CodeBuddy IDE 两棵同根同构的会话树，由网关直接读写
// （不经过本机代理）。与 WorkBuddy 侧的关键差异：会话 = 一个目录
// （`history/<md5(工作区)>/<会话 id>/{index.json,messages/,assets/}`），
// 复制项是 `(workspace_hash, conversation_id)`，写入前编辑器必须完全退出。
// 字段以后端 `internal/extsessions` 的结构体 JSON 标签为准。
// -----------------------------------------------------------------------------

/** 扩展会话客户端标识（与后端 extStore 接受的取值一致）。 */
export type ExtSessionClient = "vscode" | "codebuddy-ide";

/** 一条可复制的扩展会话（后端 extsessions.ExtSession，sessions.go）。 */
export interface ExtSession {
  /** 会话 id（32 位小写 hex）。 */
  id: string;
  /** 所在工作区目录名 = md5(工作区)（32 位小写 hex）。 */
  workspace_hash: string;
  title: string;
  /** Unix 毫秒（后端把索引里的时间换算成毫秒）。 */
  updated_at: number;
  /** 索引里的会话类型（后端原样透传，可能为空）。 */
  type: string;
  /** 会话是否含正文（`index.json` 有 messages，或磁盘 messages/ 下有文件）。 */
  has_history: boolean;
}

/** 扩展会话组内成员（后端 extsessions.MemberView，registry.go）。 */
export interface ExtSessionGroupMember {
  /** 登记表里的成员 id（同步/统一都以它为准）。 */
  member_id: string;
  session_id: string;
  uid: string;
  label: string;
  variant: string;
  /** 成员会话所在的工作区 hash（找不到目录时为空）。 */
  workspace_hash: string;
  /** active / stale / superseded（登记状态）。 */
  state: string;
  updated_at: number;
  record_count: number;
  /** 正文是否可读（false 时不能作为同步来源）。 */
  readable: boolean;
  /** latest / behind / diverge / missing / unknown（相对安全源的内容状态）。 */
  version_status: string;
  /** 状态解释文案（后端给出）。 */
  reason: string;
}

/** 扩展会话组（后端 extsessions.GroupView，registry.go）。 */
export interface ExtSessionGroup {
  id: string;
  title: string;
  /** latest / behind / diverge / missing / unknown。 */
  status: string;
  /** 后端给出的状态摘要（与 WorkBuddy 侧同一套口径）。 */
  summary_text: string;
  /** 组内可安全同步的来源成员 id（空串 = 没有）。 */
  safe_source_member_id: string;
  members: ExtSessionGroupMember[];
  updated_at: number;
}

/**
 * GET /panel/api/local-sessions/ext 的响应
 * （local_sessions_ext.go 的 handleExtSessions）。
 */
export interface ExtSessionsResponse {
  client: string;
  /** false = 未找到该扩展的数据目录（note 给原因），不是请求失败。 */
  available: boolean;
  data_root: string;
  /** 编辑器（VS Code / CodeBuddy IDE）是否在运行；运行中写入会被退出回写覆盖。 */
  running: boolean;
  groups: ExtSessionGroup[];
  /** 所选来源账号（uid）下可复制的会话；未选账号时为空。 */
  sessions: ExtSession[];
  /** 无法解析（损坏）的工作区索引数量。 */
  skipped?: number;
  /** ok=正常；unavailable=关联登记表不可读（store_error 给原因）。 */
  store_status?: "ok" | "unavailable";
  store_error?: string;
  note?: string;
}

/** 一次复制的结果（后端 extsessions.CopyReport，copy.go）。 */
export interface ExtSessionCopyReport {
  source_uid: string;
  target_uid: string;
  copied: { workspace_hash: string; old_id: string; new_id: string; messages: number }[];
  errors?: { workspace_hash: string; conversation_id: string; error: string }[];
  /** 索引备份落盘根目录（`<工作目录>/backups/<kind>/<utc>`）。 */
  backup: string;
}

/** POST /panel/api/local-sessions/ext/copy 的响应。 */
export interface ExtSessionCopyResult {
  ok: boolean;
  report: ExtSessionCopyReport;
  /** 编辑器生命周期等提示（如「已重新打开 VS Code 插件」）。 */
  notes?: string[];
  /** 副本已写入但登记关联失败（会话内容不受影响）。 */
  link_errors?: { workspace_hash: string; conversation_id: string; error: string }[];
}

/**
 * POST /panel/api/local-sessions/ext/groups/{id}/preview-pair 的响应
 * （后端 extsessions.PreviewResult，registry.go）。
 */
export interface ExtSessionPreviewPair {
  /** identical / fastForward / ahead / diverge / unknown。 */
  verdict: string;
  reason: string;
  source_only: number;
  target_only: number;
  /** fastForward / overwrite / unifyOverwrite；空 = 判定不允许写入。 */
  available_modes: string[];
  /** 服务端签发的预览凭据 id（本接口由后端自行复核，前端只需展示判定）。 */
  preview_token: string;
}

/** 组统一时单个目标的执行结果（后端 extsessions.SyncOutcome，registry.go）。 */
export interface ExtSessionSyncOutcome {
  member_id: string;
  applied: boolean;
  reason?: string;
  reason_code?: string;
  error?: string;
  notes?: string[];
}

/** POST /panel/api/local-sessions/ext/groups/{id}/unify 的响应。 */
export interface ExtSessionUnifyResult {
  ok: boolean;
  outcomes: ExtSessionSyncOutcome[];
  notes?: string[];
  /** applied=true 的项数（后端算好）。 */
  synced: number;
}
