import type {
  Account,
  CreditResource,
  LogChannel,
  LogLevel,
  ModelSiteKey,
  PanelModel,
} from "@/lib/types";
import type { PackageSortMode } from "@/lib/package-sort";
const MINUTE = 60;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** Unix 秒 → 可读本地时间；0 / 非法值返回 null，由调用方决定占位文案。 */
export function formatUnixSeconds(seconds: number): string | null {
  if (!Number.isFinite(seconds) || seconds <= 0) return null;
  const date = new Date(seconds * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(
    date.getHours(),
  )}:${pad(date.getMinutes())}`;
}

/** 时长 → 「3 天 4 小时」「12 分」。 */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "0 分";
  if (seconds >= DAY) {
    const days = Math.floor(seconds / DAY);
    const hours = Math.floor((seconds % DAY) / HOUR);
    return hours > 0 ? `${days} 天 ${hours} 小时` : `${days} 天`;
  }
  if (seconds >= HOUR) {
    const hours = Math.floor(seconds / HOUR);
    const minutes = Math.floor((seconds % HOUR) / MINUTE);
    return minutes > 0 ? `${hours} 小时 ${minutes} 分` : `${hours} 小时`;
  }
  return `${Math.max(1, Math.floor(seconds / MINUTE))} 分`;
}

/** 网关运行时长（overview.uptime_seconds）。 */
export function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "刚刚启动";
  if (seconds >= DAY) {
    const days = Math.floor(seconds / DAY);
    const hours = Math.floor((seconds % DAY) / HOUR);
    return `${days} 天 ${hours} 小时`;
  }
  return formatDuration(seconds);
}

export function nowSeconds(): number {
  return Math.floor(Date.now() / 1000);
}

export type AccountState = "active" | "cooldown" | "disabled";

/** Token 剩余不足该时长时视为「即将到期」，卡片上做高亮。 */
export const EXPIRING_SOON_SECONDS = 7 * DAY;

export interface AccountView {
  state: AccountState;
  stateLabel: string;
  cooldownRemaining: number;
  tokenExpired: boolean;
  /** Token 仍有效但将在 7 天内到期。 */
  tokenExpiringSoon: boolean;
  tokenLabel: string;
  tokenRemaining: string | null;
}

/** 账号的展示态：已禁用 > 冷却 > 可用（与 overview 的 disabled/cooldown/active 口径一致）。 */
export function toAccountView(account: Account, now: number): AccountView {
  const cooling = account.cooldown_until > now;
  const state: AccountState = account.disabled ? "disabled" : cooling ? "cooldown" : "active";
  const stateLabel = state === "disabled" ? "已禁用" : state === "cooldown" ? "冷却" : "可用";

  const hasExpiry = account.token_expires_at > 0;
  const expiredByTime = hasExpiry && account.token_expires_at <= now;
  const tokenExpired = !account.token_valid || expiredByTime;
  const tokenExpiringSoon =
    !tokenExpired && hasExpiry && account.token_expires_at - now <= EXPIRING_SOON_SECONDS;

  let tokenRemaining: string | null = null;
  if (expiredByTime) {
    tokenRemaining = `已过期 ${formatDuration(now - account.token_expires_at)}`;
  } else if (!account.token_valid) {
    tokenRemaining = "凭据已失效";
  } else if (hasExpiry) {
    tokenRemaining = `剩余 ${formatDuration(account.token_expires_at - now)}`;
  }

  return {
    state,
    stateLabel,
    cooldownRemaining: cooling ? account.cooldown_until - now : 0,
    tokenExpired,
    tokenExpiringSoon,
    tokenLabel: formatUnixSeconds(account.token_expires_at) ?? "无过期时间",
    tokenRemaining,
  };
}

/** 站点标签兜底：后端未给 site_label 时按 site 取中文名。 */
export function siteLabel(account: Account): string {
  if (account.site_label) return account.site_label;
  return account.site === "intl" ? "国际站" : "国内站";
}

/**
 * 账号卡片的显示名（批次 4）：按 `display_field` 选择来源。
 *
 * `note` → 备注；`phone` → 手机号（本项目暂无数据源，回退昵称，与 wb-switch
 * 在「无手机号时回退账号名」的分支一致）；缺省或空 → 昵称。
 * 选中的字段为空时逐级回退，最终兜底「未命名账号」——任何情况下都不能
 * 渲染出空标题。
 */
export function accountDisplayName(account: Account): string {
  const fallback = account.nickname || account.uid || "未命名账号";
  const field = account.display_field;
  if (field === "note") {
    const note = account.note?.trim();
    if (note) return note;
  }
  return fallback;
}

/** 账号身份行文案：本项目凭据里没有 email 字段，用 uid 作等价身份信息。 */
export function accountIdentityLine(account: Account): string {
  return account.uid || account.file || "";
}

/** 积分数值：千分位、小数最多两位（后端可能是浮点）。 */
const CREDITS_FORMAT = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 2 });

export function formatCredits(value: number): string {
  if (!Number.isFinite(value)) return "0";
  return CREDITS_FORMAT.format(value);
}

/** 相对时间：Unix 秒 → 「刚刚」「3 分钟前」「2 小时前」；0 / 非法值 → 「从未」。 */
export function formatRelativeTime(seconds: number, now: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "从未";
  const diff = Math.max(0, now - seconds);
  if (diff < 5) return "刚刚";
  if (diff < MINUTE) return `${Math.floor(diff)} 秒前`;
  if (diff < HOUR) return `${Math.floor(diff / MINUTE)} 分钟前`;
  if (diff < DAY) return `${Math.floor(diff / HOUR)} 小时前`;
  return `${Math.floor(diff / DAY)} 天前`;
}

/** 倒计时：秒 → 「12:34」或「1:02:03」；<=0 返回空串。 */
export function formatCountdown(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "";
  const total = Math.floor(seconds);
  const h = Math.floor(total / HOUR);
  const m = Math.floor((total % HOUR) / MINUTE);
  const s = total % MINUTE;
  const pad = (n: number) => String(n).padStart(2, "0");
  if (h > 0) return `${h}:${pad(m)}:${pad(s)}`;
  return `${pad(m)}:${pad(s)}`;
}

/** 累计 token：超过 1e6 显示 x.xxM，否则千分位整数。 */
const TOKEN_FORMAT = new Intl.NumberFormat("zh-CN");

export function formatTokens(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0";
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(2)}M`;
  return TOKEN_FORMAT.format(Math.round(value));
}

/** 平均毫秒：无样本（null / 非法）返回「-」。 */
export function formatMillis(value: number | null): string {
  if (value === null || !Number.isFinite(value) || value < 0) return "-";
  return `${Math.round(value)}`;
}

/** 冷却类型的中文说明。 */
export const COOLDOWN_KIND_LABEL: Record<"soft" | "hard" | "breaker", string> = {
  soft: "软限流",
  hard: "硬冷却",
  breaker: "熔断",
};

/** 账号运行态的中文说明（metrics.accounts[].state）。 */
export const RUNTIME_STATE_LABEL: Record<"available" | "cooldown" | "disabled", string> = {
  available: "可用",
  cooldown: "冷却",
  disabled: "禁用",
};

export interface QuotaView {
  /** 是否已查询到额度。 */
  known: boolean;
  plan: string | null;
  paid: boolean;
  /** 额度已耗尽（后端标记或剩余归零）。 */
  exhausted: boolean;
  total: number;
  used: number;
  remaining: number;
  /** 已用占比，0~1，用于进度条。 */
  ratio: number;
  /** 进度条文案：剩余 X / 共 Y。 */
  label: string;
  /** Unix 秒，0 表示后端未给。 */
  updatedAt: number;
  /** 额度快照时间文案。 */
  updatedLabel: string | null;
}

/** 账号额度 → 展示视图；quota 为 null 时 known=false，其余字段取零值。 */
export function toQuotaView(account: Account): QuotaView {
  const quota = account.quota;
  if (!quota) {
    return {
      known: false,
      plan: null,
      paid: false,
      exhausted: false,
      total: 0,
      used: 0,
      remaining: 0,
      ratio: 0,
      label: "未查询",
      updatedAt: 0,
      updatedLabel: null,
    };
  }

  const total = Number(quota.total) || 0;
  const used = Number(quota.used) || 0;
  const remaining = Number(quota.remaining) || 0;
  const exhausted = Boolean(quota.exhausted) || remaining <= 0;
  const ratio = total > 0 ? Math.min(1, Math.max(0, used / total)) : exhausted ? 1 : 0;
  const updatedAt = Number(quota.updated_at) || 0;

  return {
    known: true,
    plan: quota.plan || null,
    paid: Boolean(quota.paid),
    exhausted,
    total,
    used,
    remaining,
    ratio,
    label: `剩余 ${formatCredits(remaining)} / 共 ${formatCredits(total)}`,
    updatedAt,
    updatedLabel: formatUnixSeconds(updatedAt),
  };
}

export type AccountSort = "expiry" | "file";

/** 排序用的账号稳定标识：优先 id，其次 file / uid。 */
export function accountKey(account: Account): string {
  return account.id || account.file || account.uid || "";
}

/**
 * 按 Token 到期时间排序用的键：**越早到期的键越小**（排越前）。
 *
 * 三种取值分别是什么、为什么这么取：
 *
 *   - 已过期 / 已失效 → `-Infinity`，永远排最前。
 *     注意不能简单用 `token_expires_at`：令牌失效（`token_valid === false`）时
 *     过期时间可能是将来的时间点，但那个令牌**当下就不能用了**，
 *     按时间排会把它埋到中间，恰好把最该处理的那张卡藏起来。
 *     这与旧排序「Token 失效排最前」的意图一致，只是换了表达方式。
 *   - 正常有到期时间 → 该时间戳本身。
 *   - 未标注到期时间（`token_expires_at <= 0`）→ `+Infinity`，排最后。
 *     它既不是「已过期」也不是「有明确期限」，放最前会挤掉真正紧急的账号。
 */
function expirySortKey(account: Account, now: number): number {
  const view = toAccountView(account, now);
  if (view.tokenExpired) return Number.NEGATIVE_INFINITY;
  if (account.token_expires_at > 0) return account.token_expires_at;
  return Number.POSITIVE_INFINITY;
}

/**
 * 列表排序：
 * - expiry（默认）：按 Token 到期时间升序 —— 最快到期的排最前，
 *   已过期/已失效的在最前，未标注过期时间的在最后。
 * - file：按凭据文件名（中文按拼音）排序。
 *
 * 到期时间相同时按文件名兜底，保证顺序稳定（否则每次重排都可能抖动）。
 */
export function sortAccounts(accounts: Account[], sort: AccountSort, now: number): Account[] {
  const next = [...accounts];
  if (sort === "file") {
    return next.sort((a, b) => accountKey(a).localeCompare(accountKey(b), "zh-Hans-CN"));
  }

  return next.sort((a, b) => {
    const diff = expirySortKey(a, now) - expirySortKey(b, now);
    if (diff !== 0) return diff;
    return accountKey(a).localeCompare(accountKey(b), "zh-Hans-CN");
  });
}

// -----------------------------------------------------------------------------
// 积分资源包（账号卡片的「近期到期」明细）
// -----------------------------------------------------------------------------

/**
 * 资源包进度条的配色档位。
 *
 *   - `expired`  已到期 —— 红。它已经不能用了，是最需要立刻处理的一档。
 *   - `soon`     7 天内到期 —— 橙。还能用但很快会消失（与后端 expiring_soon 同阈值）。
 *   - `plenty`   充裕 —— 品牌绿。正常状态，不该抢视觉焦点。
 *
 * 三档由后端给的 `expired` / `expiring_soon` 直接决定，前端不再自己算一遍阈值：
 * 「7 天」这个数字只应该有一个事实来源，两处各写一份迟早会漂移。
 */
export type CreditPackageTone = "expired" | "soon" | "plenty";

export interface CreditPackageView {
  /** 列表 key 用：商品码优先，缺失时退回名称+下标（由调用方拼）。 */
  key: string;
  /** 展示名：官方中文名优先（见 lib/credit-package-names）。 */
  name: string;
  remaining: number;
  total: number;
  used: number;
  /** 剩余占比 0~1，用于细进度条宽度。 */
  ratio: number;
  /** Unix 毫秒；0 表示长期有效。 */
  expireAt: number;
  /** 「10/05 21:14」；长期有效返回「长期」。 */
  expireLabel: string;
  expired: boolean;
  expiringSoon: boolean;
  tone: CreditPackageTone;
}

/**
 * 资源包到期时间 → 「MM/DD HH:MM」。
 *
 * 与积分统计页的 `formatExpiry`（`YYYY/MM/DD HH:MM`）刻意不同：卡片横向空间紧，
 * 同一行还要放金额与类型，四位年份是冗余信息（包的有效期不会跨年）；
 * 积分统计页是独立的详情表，宽裕，保留完整年份。
 *
 * 0 / 非法值 → 「长期」：后端把「2049 这类占位」归一成 0（见 upstream.resolveExpireAt），
 * 这里不能把它渲染成日期。
 */
export function formatExpiryShort(ms: number | null | undefined): string {
  if (!ms || !Number.isFinite(ms) || ms <= 0) return "长期";
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/**
 * 完整到期时间（**毫秒**入参，与 `formatExpiryShort` 同一口径）：`2026-10-06 19:59`。
 *
 * 为什么另开一个而不复用 `formatUnixSeconds`：那个收的是**秒**，而 `credit_resources`
 * 的 `expire_at` 是毫秒（`formatExpiryShort` 直接 `new Date(ms)` 就印证了这点）。
 * 拿毫秒去喂秒函数会得到一个 1970 年附近的日期，且不会报错 —— 这类单位错配只能靠
 * 「函数名里写清单位 + 注释说明」来防，光看调用点是看不出来的。
 *
 * 长期有效（0）返回「长期」：调用方需要能把它和真实日期区分开。
 */
export function formatExpiryFull(ms: number | null | undefined): string {
  if (!ms || !Number.isFinite(ms) || ms <= 0) return "长期";
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(
    d.getHours(),
  )}:${pad(d.getMinutes())}`;
}

/**
 * 逐包比较器：`end_asc` 到期升序（默认；长期有效垫底），`size_desc` 面额降序
 *（同面额再按到期升序）。与参考实现 pkDetailCompare 同一口径：面额取包的
 * **总额**（total，不是剩余）；长期有效（expireAt=0）没有可比日期、永远垫底 ——
 * 若按 0 参与升序会全部跑到最前面，把真正快过期的挤下去。
 */
export function compareCreditPackages(
  a: CreditPackageView,
  b: CreditPackageView,
  mode: PackageSortMode,
): number {
  if (mode === "size_desc") {
    const diff = b.total - a.total;
    if (diff !== 0) return diff;
  }
  const ka = a.expireAt > 0 ? a.expireAt : Number.POSITIVE_INFINITY;
  const kb = b.expireAt > 0 ? b.expireAt : Number.POSITIVE_INFINITY;
  return ka - kb;
}

/**
 * 资源包列表 → 展示视图。
 *
 * `mode` 跟随逐包明细弹窗顶部的排序选择（见 lib/package-sort）：
 * 默认 `end_asc`「到期时间升序」（最快到期的在最上面），`size_desc` 改为
 * 面额降序、同面额再按到期升序。
 *
 * 为什么排序放在这里而不是交给后端：后端返回的是上游原始顺序（按商品码），
 * 与「先处理哪个」无关；而卡片这一块标题就叫「近期到期」，
 * 若按原始顺序渲染，标题与内容会自相矛盾。
 * 到期比较的细节（长期有效垫底）见 compareCreditPackages。
 */
export function toCreditPackageViews(
  resources: readonly CreditResource[] | null | undefined,
  nameOf: (resource: CreditResource, fallback: string) => string,
  mode: PackageSortMode = "end_asc",
): CreditPackageView[] {
  if (!resources || resources.length === 0) return [];
  const views = resources.map((resource, index) => {
    const total = Number(resource.total) || 0;
    const remaining = Number(resource.remaining) || 0;
    const used = Number(resource.used) || 0;
    // 进度条口径是「还剩多少」而不是「用了多少」——明细行左边写的金额是剩余额，
    // 条子跟着金额走，否则会出现「写着 962 但条子快满了」的观感矛盾。
    const ratio = total > 0 ? Math.min(1, Math.max(0, remaining / total)) : 0;
    const expired = Boolean(resource.expired);
    const expiringSoon = Boolean(resource.expiring_soon);
    const tone: CreditPackageTone = expired ? "expired" : expiringSoon ? "soon" : "plenty";
    return {
      key: resource.package_code || `${resource.package_name || "resource"}-${index}`,
      name: nameOf(resource, "未命名资源包"),
      remaining,
      total,
      used,
      ratio,
      expireAt: Number(resource.expire_at) || 0,
      expireLabel: formatExpiryShort(resource.expire_at),
      expired,
      expiringSoon,
      tone,
    };
  });

  return views.sort((a, b) => compareCreditPackages(a, b, mode));
}

// -----------------------------------------------------------------------------
// 切片 3：模型目录的展示与排序
// -----------------------------------------------------------------------------

/** 站点展示顺序：国内站在前，与目录接口的 sites[] 一致。 */
export const MODEL_SITE_KEYS: ModelSiteKey[] = ["cn", "intl"];

/** 整数千分位；0 / 非法值 → 「-」（用于上游可能未标注的上下文长度、最大输出）。 */
const COUNT_FORMAT = new Intl.NumberFormat("zh-CN");

export function formatCount(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "-";
  return COUNT_FORMAT.format(Math.round(value));
}

/**
 * 数量千分位：**0 是有效值**，返回 "0"（而不是 "-"）。
 *
 * 与 `formatCount` 的分工必须分清，混用会读出错误信息：
 *   - `formatCount` 表达「这个字段可能压根没有值」—— 上游未标注的上下文长度、
 *     最大输出这类字段，0 与「缺」共用 "-"。
 *   - 本函数表达「确实数到了 0 个」—— 请求数、条数这类由我们自己计数的量。
 *     若用 formatCount，「0 次请求」会显示成「- 次请求」，读起来像数据没取到。
 */
export function formatQuantity(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0";
  return COUNT_FORMAT.format(Math.round(value));
}

// -----------------------------------------------------------------------------
// 切片 4：日志页的展示文案
// -----------------------------------------------------------------------------

/** 日志级别 → 中文文案。 */
const LOG_LEVEL_LABEL: Record<LogLevel, string> = {
  info: "信息",
  warn: "警告",
  error: "错误",
};

/** 日志级别 → 中文；未知级别原样显示。 */
export function logLevelLabel(level: string): string {
  return LOG_LEVEL_LABEL[level as LogLevel] ?? level;
}

/** 日志频道 → 中文文案。 */
const LOG_CHANNEL_LABEL: Record<LogChannel, string> = {
  request: "请求",
  outbound: "出站",
  governance: "治理",
  task: "任务",
  system: "系统",
};

/** 日志频道 → 中文；未知频道原样显示。 */
export function logChannelLabel(channel: string): string {
  return LOG_CHANNEL_LABEL[channel as LogChannel] ?? channel;
}

/** Unix 秒 → 精确到秒的本地时间；0 / 非法值返回 null（日志 tooltip 用）。 */
export function formatUnixSecondsFull(seconds: number): string | null {
  if (!Number.isFinite(seconds) || seconds <= 0) return null;
  const date = new Date(seconds * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(
    date.getHours(),
  )}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

/** 推理档位的中文说明（reasoning_supported_efforts）。 */
export const REASONING_EFFORT_LABEL: Record<string, string> = {
  minimal: "极简",
  low: "低",
  medium: "中",
  high: "高",
  // 上游 2026-09 起的目录里出现 max 档（glm-5.3 系列）。漏了它会让档位徽章
  // 直接显示英文原文 "max" —— 混在「低/高」中间很突兀。
  max: "最高",
};

/** 模型稳定标识：id 优先，其次 name。 */
export function modelKey(model: PanelModel): string {
  return model.id || model.name || "";
}

/** 该模型尚未确认价格的站点（known=false，或后端未返回该站点）。 */
export function unknownModelSites(model: PanelModel): ModelSiteKey[] {
  return MODEL_SITE_KEYS.filter((site) => {
    const info = model.sites?.[site];
    return !info || !info.known;
  });
}

/** 价格是否仍有未知：任一站点 known=false。 */
export function isModelPriceUnknown(model: PanelModel): boolean {
  return unknownModelSites(model).length > 0;
}

/** 把字节数格式化成可读大小（B / KB / MB / GB）。 */
export function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let n = value;
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return (i === 0 ? String(Math.round(n)) : n.toFixed(n >= 100 ? 0 : 1)) + " " + units[i];
}

// -----------------------------------------------------------------------------
// Token 统计页的数值格式（与 wb-switch 一致）
// -----------------------------------------------------------------------------

/** compact 记数：1.2K / 3.4M / 5.6B（后缀大写，与 switch 的 formatTokenCompact 一致）。 */
const TOKEN_COMPACT = new Intl.NumberFormat("en-US", {
  notation: "compact",
  compactDisplay: "short",
  maximumFractionDigits: 1,
});

export function formatTokenCompact(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0";
  return TOKEN_COMPACT.format(value).replace(/k|m|b/, (s) => s.toUpperCase());
}

/** 精确记数（千分位），用于 tooltip 与明细。 */
const TOKEN_EXACT = new Intl.NumberFormat("en-US");

export function formatTokenExact(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0";
  return TOKEN_EXACT.format(Math.round(value));
}

/** 占比：分母为 0 时返回「—」—— 0.0% 会被读成「占比确实是 0」。 */
export function tokenPercentage(value: number, sum: number): string {
  if (!Number.isFinite(value) || !Number.isFinite(sum) || sum <= 0) return "—";
  const pct = (value / sum) * 100;
  return pct.toFixed(1) + "%";
}

/**
 * 把邮箱形态的账号名遮成 `a***@domain`。
 *
 * 为什么做：国际站账号的**昵称本身就是邮箱**（注册方式决定），
 * 而账号名会出现在截图、录屏、共享屏幕里 —— 这是本工具里最容易泄露个人信息的一处。
 *
 * 为什么只遮本地部分、保留域名与首字母：`a***@gmail.com` 与 `b***@gmail.com`
 * 仍然可区分，不影响辨认是哪个账号。**遮到不可辨认就等于把功能弄坏了。**
 *
 * 非邮箱形态的昵称（中文名、uid）原样返回 —— 只处理真正是邮箱的那种。
 */
export function maskEmail(name: string): string {
  const s = name.trim();
  const at = s.lastIndexOf("@");
  if (at <= 0 || at === s.length - 1) return s;
  const local = s.slice(0, at);
  const domain = s.slice(at + 1);
  // 域名里必须有点、且不含空白，才算邮箱；否则可能是别的东西碰巧带 @。
  if (!domain.includes(".") || /\s/.test(s)) return s;
  if (local.length <= 1) return `${local}***@${domain}`;
  return `${local[0]}***@${domain}`;
}

/**
 * 相对时间：近期用「N分钟前 / N小时前」，较早用「MM/DD HH:mm」。
 *
 * 分界取 24 小时 —— 与截图一致（「10分钟前」「19小时前」「09/30 15:44」）。
 * 注意入参是**毫秒**（会话库的时间戳单位）。
 */
export function relativeTime(ms: number, now: number = Date.now()): string {
  if (!ms) return "—";
  const diff = now - ms;
  if (diff < 0) return "刚刚";
  const min = Math.floor(diff / 60000);
  if (min < 1) return "刚刚";
  if (min < 60) return `${min}分钟前`;
  const hour = Math.floor(min / 60);
  if (hour < 24) return `${hour}小时前`;
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}/${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}
