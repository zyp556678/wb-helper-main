/**
 * 模型目录的**派生视图**：把后端给的原始 `PanelModel` 换算成页面直接可用的形状。
 *
 * 为什么不写在页面组件里：这些规则（徽章标签怎么解析、倍率怎么分档、筛选怎么组合）
 * 都是**口径**，散在 JSX 里会随每次改版漂移。集中在这里，页面只负责画。
 */
import type { ModelSiteInfo, ModelSiteKey, PanelModel } from "@/lib/types";

// -----------------------------------------------------------------------------
// 上游徽章标签
// -----------------------------------------------------------------------------

export interface ModelBadge {
  label: string;
  /** 上游给的颜色，形如 `#FF0000`；解析失败时为空串，由调用方回落到中性色。 */
  color: string;
}

/** `badge:<名称>:#<颜色>` —— 上游用标签数组传徽章，格式固定三段。 */
const BADGE_PREFIX = "badge:";

/**
 * 解析上游徽章标签。
 *
 * 上游把「限时免费」「夜间折扣」这类运营标记塞在 `tags` 里，形如
 * `badge:限时免费:#FF0000`。颜色是上游给的**十六进制原文**，直接用而不是
 * 映射到自己的色板 —— 运营改色时不该需要前端跟着发版。
 *
 * 解析失败（段数不对、颜色为空）时整条丢弃：宁可少一个徽章，
 * 也不要画出一个没有颜色、看起来像坏掉的黑块。
 */
export function parseModelBadges(model: Pick<PanelModel, "tags">): ModelBadge[] {
  const out: ModelBadge[] = [];
  for (const tag of model.tags ?? []) {
    if (!tag.startsWith(BADGE_PREFIX)) continue;
    const rest = tag.slice(BADGE_PREFIX.length);
    const sep = rest.lastIndexOf(":");
    if (sep <= 0) continue;
    const label = rest.slice(0, sep).trim();
    const color = rest.slice(sep + 1).trim();
    if (!label || !isHexColor(color)) continue;
    out.push({ label, color });
  }
  return out;
}

function isHexColor(value: string): boolean {
  return /^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$/.test(value);
}

/** 普通标签（去掉徽章定义后的部分），如 `craft`、`text-to-image`。 */
export function plainTags(model: Pick<PanelModel, "tags">): string[] {
  return (model.tags ?? []).filter((tag) => !tag.startsWith(BADGE_PREFIX));
}

// -----------------------------------------------------------------------------
// 计费档位
// -----------------------------------------------------------------------------

export type PriceTierKey = "free" | "ultra-low" | "low" | "mid" | "high" | "unknown";

export interface PriceTier {
  key: PriceTierKey;
  label: string;
  /** 上界（含）；null 表示「以上」或「不可比较」。 */
  max: number | null;
  hint: string;
  tone: "success" | "default" | "warning" | "danger";
}

/**
 * 档位分桶。
 *
 * 为什么按**已确认站点中的最低倍率**分档，而不是固定看国内站：
 * 网关默认开着「免费站点优先」，调度时会把请求倾向到便宜的那一侧，
 * 所以「实际会付多少」对应的是两侧里最低的倍率，而不是某一侧的值。
 */
export const PRICE_TIERS: PriceTier[] = [
  { key: "free", label: "免费", max: 0, hint: "倍率 0.00x，不扣额度", tone: "success" },
  { key: "ultra-low", label: "极低", max: 0.1, hint: "≤ 0.10x", tone: "success" },
  { key: "low", label: "低", max: 0.3, hint: "≤ 0.30x", tone: "default" },
  { key: "mid", label: "中", max: 1, hint: "≤ 1.00x", tone: "warning" },
  { key: "high", label: "高", max: null, hint: "> 1.00x", tone: "danger" },
  { key: "unknown", label: "未确认", max: null, hint: "两侧都未确认", tone: "default" },
];

export function priceTierOf(tier: PriceTierKey): PriceTier {
  return PRICE_TIERS.find((t) => t.key === tier) ?? PRICE_TIERS[PRICE_TIERS.length - 1];
}

/** 站点展示名；与后端 site_label 同口径，但这里用于前端派生，不依赖后端下发。 */
export const SITE_LABEL: Record<ModelSiteKey, string> = { cn: "国内站", intl: "国际站" };

/**
 * 站点在**模型名前缀**里的写法，含分隔符（如 `CN-`）。
 *
 * 必须与后端 `internal/server/site_route.go` 的 `sitePrefixes` + `sitePrefixSeparator`
 * 保持一致 —— 这里写错，模型页复制出来的字符串贴到调用里就路由不到目标站点。
 *
 * 用「大写 + 连字符」是因为上游模型名全小写且大量含连字符，大写前缀与真实模型命名
 * 空间天然隔离，不会把某个真实模型永久劫持成路由前缀。
 *
 * 站点页会从 `/panel/api/site-route` 拿到后端下发的同名字段（单一事实来源）；
 * 这里的常量供**模型页**在拿到目录数据时本地派生，避免为每个模型再打一次接口。
 */
export const SITE_PREFIX: Record<ModelSiteKey, string> = { cn: "CN-", intl: "AI-" };

export const SITE_KEYS: ModelSiteKey[] = ["cn", "intl"];

/** 某站点是否已确认价格（known 且倍率可比较）。 */
export function siteConfirmed(info: ModelSiteInfo | undefined): boolean {
  return Boolean(info?.known && typeof info.multiplier === "number");
}

export interface BestPrice {
  site: ModelSiteKey;
  multiplier: number;
}

/** 已确认站点中的最低倍率；两侧都没确认时返回 null。 */
export function bestConfirmedMultiplier(model: PanelModel): BestPrice | null {
  let best: BestPrice | null = null;
  for (const site of SITE_KEYS) {
    const info = model.sites?.[site];
    if (!siteConfirmed(info)) continue;
    const value = info?.multiplier as number;
    if (best === null || value < best.multiplier) best = { site, multiplier: value };
  }
  return best;
}

export function tierOf(model: PanelModel): PriceTierKey {
  const best = bestConfirmedMultiplier(model);
  if (best === null) return "unknown";
  const v = best.multiplier;
  if (v <= 0) return "free";
  if (v <= 0.1) return "ultra-low";
  if (v <= 0.3) return "low";
  if (v <= 1) return "mid";
  return "high";
}

/**
 * 可探测的站点 = 目录里报了「有账号」的站点。
 *
 * 为什么需要这个收敛：价格探测要**用某个账号发一次真实请求**，没有账号的站点
 * 根本探不了。而模型自身只知道「这个站点价格未确认」—— 于是一个没配国际站账号的
 * 部署里，全部模型都会因为「国际站未确认」被算成待探测，
 * 界面显示「探测未知价格（36）」，点下去却是几十次注定失败的请求外加白花的额度。
 * 目录里 `sites[].accounts` 是后端已经算好的账号数，直接拿来当过滤器。
 *
 * 一个账号都没有时（还没登录）回落到全部站点：此时该暴露的是「需要先加账号」，
 * 而不是把探测入口藏起来让人找不到。
 */
export function probeableSites(catalog: { sites?: { site: string; accounts: number }[] } | null): ModelSiteKey[] {
  const withAccounts = (catalog?.sites ?? [])
    .filter((site) => site.accounts > 0)
    .map((site) => site.site as ModelSiteKey)
    .filter((site) => SITE_KEYS.includes(site));
  return withAccounts.length > 0 ? withAccounts : SITE_KEYS;
}

/** 该模型在**可探测站点**里还没确认价格的站点（探测按钮该提交的目标）。 */
export function pendingProbeSites(model: PanelModel, probeable: ModelSiteKey[]): ModelSiteKey[] {
  return probeable.filter((site) => !siteConfirmed(model.sites?.[site]));
}

// -----------------------------------------------------------------------------
// 推理档位
// -----------------------------------------------------------------------------

export type ReasoningKey = "supported" | "only" | "none";

export const REASONING_OPTIONS: { key: ReasoningKey; label: string; hint: string }[] = [
  { key: "supported", label: "支持推理", hint: "上游声明该模型支持思考" },
  { key: "only", label: "仅推理", hint: "不接受关闭思考（reasoning_only）" },
  { key: "none", label: "不支持推理", hint: "上游未声明支持推理" },
];

/**
 * 推理能力归类。
 *
 * 注意 `reasoning_supported` **缺失不等于不支持** —— 上游用精简目录时会漏发该字段
 * （见 internal/upstream/modelcatalog.go 的说明）。但页面上必须给一个确定的分组，
 * 所以这里按「显式为 true 才算支持」处理，并把「字段缺失」也归到不支持一侧，
 * 在档位说明里写清楚这个口径。
 */
export function reasoningOf(model: PanelModel): ReasoningKey {
  if (model.reasoning_only === true) return "only";
  if (model.reasoning_supported === true) return "supported";
  return "none";
}

/** 模型声明的可选档位（上游多数时候只给默认档，列表可能为空）。 */
export function effortList(model: PanelModel): { efforts: string[]; fallback: string } {
  return {
    efforts: model.reasoning_supported_efforts ?? [],
    fallback: model.reasoning_default_effort || "",
  };
}

// -----------------------------------------------------------------------------
// 视图模型
// -----------------------------------------------------------------------------

export interface ModelView {
  model: PanelModel;
  /** 权威黑名单判定（来自 config.models_filter，而不是模型自身的 blocked 字段）。 */
  blocked: boolean;
  tier: PriceTierKey;
  best: BestPrice | null;
  badges: ModelBadge[];
  tags: string[];
  reasoning: ReasoningKey;
  /** 已确认价格的站点。 */
  confirmedSites: ModelSiteKey[];
}

export function toModelView(model: PanelModel, blockedIds: Set<string>): ModelView {
  return {
    model,
    blocked: blockedIds.has(model.id),
    tier: tierOf(model),
    best: bestConfirmedMultiplier(model),
    badges: parseModelBadges(model),
    tags: plainTags(model),
    reasoning: reasoningOf(model),
    confirmedSites: SITE_KEYS.filter((site) => siteConfirmed(model.sites?.[site])),
  };
}

// -----------------------------------------------------------------------------
// 筛选
// -----------------------------------------------------------------------------

export type CapabilityKey = "tool" | "image";
export type StateKey = "enabled" | "blocked";

export interface ModelFilters {
  /** 已确认价格的站点；空数组 = 不限。 */
  sites: ModelSiteKey[];
  /** 计费档位；空数组 = 不限。 */
  tiers: PriceTierKey[];
  /** 推理能力；空数组 = 不限。 */
  reasoning: ReasoningKey[];
  /** 能力；空数组 = 不限。 */
  caps: CapabilityKey[];
  /** 启用状态；空数组 = 不限。 */
  state: StateKey[];
}

export const EMPTY_FILTERS: ModelFilters = {
  sites: [],
  tiers: [],
  reasoning: [],
  caps: [],
  state: [],
};

export const CAPABILITY_OPTIONS: { key: CapabilityKey; label: string; hint: string }[] = [
  { key: "tool", label: "工具调用", hint: "支持 function calling" },
  { key: "image", label: "图片输入", hint: "接受图片作为输入" },
];

export const STATE_OPTIONS: { key: StateKey; label: string; hint: string }[] = [
  { key: "enabled", label: "已启用", hint: "未命中黑名单" },
  { key: "blocked", label: "已禁用", hint: "命中黑名单，网关不调度" },
];

/** 组内 OR、组间 AND —— 与参考页的复选框筛选一致。 */
function matches(view: ModelView, filters: ModelFilters): boolean {
  if (filters.sites.length > 0) {
    if (!filters.sites.some((site) => view.confirmedSites.includes(site))) return false;
  }
  if (filters.tiers.length > 0 && !filters.tiers.includes(view.tier)) return false;
  if (filters.reasoning.length > 0 && !filters.reasoning.includes(view.reasoning)) return false;
  if (filters.caps.length > 0) {
    const ok = filters.caps.every((cap) =>
      cap === "tool" ? view.model.supports_tool_call : view.model.supports_images,
    );
    if (!ok) return false;
  }
  if (filters.state.length > 0) {
    const key: StateKey = view.blocked ? "blocked" : "enabled";
    if (!filters.state.includes(key)) return false;
  }
  return true;
}

/** 搜索匹配：id / 名称 / 描述 / 标签 / 厂商码，忽略大小写。 */
export function matchesQuery(view: ModelView, keyword: string): boolean {
  if (!keyword) return true;
  const haystack = [
    view.model.id,
    view.model.name,
    view.model.description,
    view.model.vendor,
    ...view.tags,
    ...view.badges.map((b) => b.label),
  ]
    .join(" ")
    .toLowerCase();
  return haystack.includes(keyword);
}

export function applyFilters(
  views: ModelView[],
  filters: ModelFilters,
  keyword: string,
): ModelView[] {
  return views.filter((view) => matches(view, filters) && matchesQuery(view, keyword));
}

/** 按档位计数。六个桶**互不重叠**，合计等于模型总数（未知单列），可直接当分区用。 */
export function tierCounts(views: ModelView[]): Record<PriceTierKey, number> {
  const out = { free: 0, "ultra-low": 0, low: 0, mid: 0, high: 0, unknown: 0 } as Record<
    PriceTierKey,
    number
  >;
  for (const view of views) out[view.tier] += 1;
  return out;
}

/** 已启用的筛选组数，用于「清除全部（N）」。 */
export function activeFilterCount(filters: ModelFilters): number {
  return (
    filters.sites.length +
    filters.tiers.length +
    filters.reasoning.length +
    filters.caps.length +
    filters.state.length
  );
}

/** 切换某个多选项；已存在则移除。 */
export function toggleValue<T>(list: T[], value: T): T[] {
  return list.includes(value) ? list.filter((item) => item !== value) : [...list, value];
}

// -----------------------------------------------------------------------------
// 排序
// -----------------------------------------------------------------------------

export type ModelSort = "unknown-first" | "cheapest" | "name";

/**
 * 视图排序。
 *
 * - `unknown-first`（默认）：倍率未确认的最前 —— 它们是唯一需要人工动作（探测）的一批，
 *   排在后面会被埋在几十张卡片里找不到。其后按请求数降序（用得多的先看到），再按 id。
 * - `cheapest`：已确认的最低倍率升序，未确认的垫底。网关默认倾向便宜的站点，
 *   按「实际会付的价」排序比按名字更贴近这个页面的用途。
 * - `name`：按模型名（缺 name 用 id），中文按拼音。
 */
export function sortModelViews(views: ModelView[], sort: ModelSort): ModelView[] {
  const next = [...views];
  if (sort === "name") {
    return next.sort(
      (a, b) =>
        (a.model.name || a.model.id).localeCompare(b.model.name || b.model.id, "zh-Hans-CN") ||
        a.model.id.localeCompare(b.model.id, "zh-Hans-CN"),
    );
  }
  if (sort === "cheapest") {
    return next.sort((a, b) => {
      // 未确认垫底：没有倍率就没有可比性，混在低价里会误导。
      if (a.best === null && b.best === null) return a.model.id.localeCompare(b.model.id);
      if (a.best === null) return 1;
      if (b.best === null) return -1;
      if (a.best.multiplier !== b.best.multiplier) return a.best.multiplier - b.best.multiplier;
      return a.model.id.localeCompare(b.model.id);
    });
  }
  return next.sort((a, b) => {
    const unknownDiff = Number(a.best === null) - Number(b.best === null);
    if (unknownDiff !== 0) return -unknownDiff;
    const requestDiff = b.model.requests - a.model.requests;
    if (requestDiff !== 0) return requestDiff;
    return a.model.id.localeCompare(b.model.id, "zh-Hans-CN");
  });
}
