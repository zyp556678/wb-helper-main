/**
 * 积分统计（版式 **1:1 对齐 wb-switch 的 CreditStatsPage**）。
 *
 * ## 页面在回答什么
 *
 *   1. **还剩多少**（页头「剩余积分」+「积分明细」页签）—— 当前资源包与到期情况。
 *   2. **花了多少、花在哪**（总览指标带、官方积分消耗、按模型分类、账号消耗构成）
 *      —— 上游计费账本，按天 × 模型、按账号拆。
 *
 * 两者是账本的两端：只有余额看不出消耗速率，只有消耗看不出余额够不够。
 *
 * ## 两个数据源，各自失败各自标注
 *
 *   - `/panel/api/stats/official`（官方账本）→ 所有「消耗」数字。
 *   - `/panel/api/credits`（积分资源包）→「剩余积分」与「积分明细」页签。
 *
 * 不合成一个接口，是因为两者打上游的**不同端点**：账本挂了不该让「剩余积分」
 * 跟着变 0，反之亦然。所以并行发起、独立降级。
 *
 * ## 与 switch 的有意偏离（都只影响外观，不影响口径）
 *
 *   1. **账号筛选用原生 `<select>`**：本项目的 UI 原语里没有下拉菜单，
 *      为一个筛选控件引入 Radix dropdown 不划算。选项与行为一致。
 *   2. **「积分明细」默认「所有账号」**：switch 是按账号懒加载资源包的，
 *      默认只能落在第一个账号上；我们一次取回全部账号，默认看全部更合理，
 *      需要聚焦时再筛选。只有一个账号时两者渲染完全一致。
 *   3. **图例字号 12px**：switch 用 10px，本项目有「最小 12px」的可读性契约，
 *      按契约抬高（与 Token 统计页同一处偏离）。
 *
 * ## 为什么固定拉 31 天
 *
 * 面板的四个范围里「本月」最长需要 31 天（月初到月末），「近 30 天」需要 30 天。
 * 一次拉满 31 天，切换范围就只是前端切片，不再打上游 —— 上游那次调用要几秒，
 * 每切一次页签等一次是不可接受的。这也是 switch 的做法。
 */
import { useCallback, useEffect, useMemo, useState } from "react";
import { Bar, BarChart, CartesianGrid, Rectangle, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import {
  ArrowUpDown,
  CalendarDays,
  CalendarRange,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  CircleAlert,
  CircleCheck,
  Loader2,
  RefreshCw,
  Sparkles,
  TrendingDown,
  Users,
} from "lucide-react";
import type { CSSProperties, ComponentProps } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { CardHint, EmptyHint, MetricRow, PillGroup, Section, StatMetric } from "@/components/section";
import { ShareRows, type ShareRow } from "@/components/ranking-rows";
import { describeError, fetchCredits, fetchCreditStatistics, fetchOfficialUsage } from "@/lib/api";
import { creditResourceName } from "@/lib/credit-package-names";
import { formatCount, formatCredits } from "@/lib/format";
import { getStackedSegmentVisualLayout } from "@/lib/stacked-bar-visuals";
import { cn } from "@/lib/utils";
import type {
  CreditResource,
  CreditStatsDailyPoint,
  CreditStatsResponse,
  CreditsResponse,
  OfficialUsageAccount,
  OfficialUsageDay,
  OfficialUsageModel,
  OfficialUsageRequestRow,
  OfficialUsageResponse,
} from "@/lib/types";

/** 请求用量明细每页条数；明细由后端一次性返回，分页只控制单页渲染量。 */
const REQUEST_PAGE_SIZE = 100;

/** 明细可排序的两列。 */
type RequestSortKey = "time" | "credit";
type SortDirection = "desc" | "asc";

/**
 * 取一行的排序值。
 *
 * request_time 是「YYYY-MM-DD HH:mm:ss」文本 —— 定长且高位在前，字典序即时间序，
 * 所以直接返回字符串比较即可，不必解析成 Date（解析还得处理时区，反而更易错）。
 */
function requestSortValue(row: OfficialUsageRequestRow, key: RequestSortKey): number | string {
  if (key === "credit") return row.credit ?? 0;
  return row.request_time ?? "";
}

/** 按指定列与方向比较两行。 */
function compareRequests(
  left: OfficialUsageRequestRow,
  right: OfficialUsageRequestRow,
  key: RequestSortKey,
  direction: SortDirection,
): number {
  const a = requestSortValue(left, key);
  const b = requestSortValue(right, key);
  let order: number;
  if (typeof a === "number" && typeof b === "number") {
    order = a - b;
  } else {
    order = String(a).localeCompare(String(b));
  }
  if (order === 0) {
    // 同值时用请求 ID 兜底，保证排序稳定（V8 的 sort 虽然稳定，但不该依赖实现细节）
    order = (left.request_id ?? "").localeCompare(right.request_id ?? "");
  }
  return direction === "desc" ? -order : order;
}

/** 可点击排序的表头单元格。 */
function SortableHeader({
  label,
  sortKey,
  activeKey,
  direction,
  align = "left",
  onToggle,
}: {
  label: string;
  sortKey: RequestSortKey;
  activeKey: RequestSortKey;
  direction: SortDirection;
  align?: "left" | "right";
  onToggle: (key: RequestSortKey) => void;
}) {
  const active = activeKey === sortKey;
  return (
    <th
      className={cn("px-3 py-2.5 font-medium", align === "right" && "text-right")}
      aria-sort={active ? (direction === "desc" ? "descending" : "ascending") : "none"}
    >
      <button
        type="button"
        onClick={() => onToggle(sortKey)}
        title={
          active
            ? direction === "desc"
              ? "当前降序，点击切换升序"
              : "当前升序，点击切换降序"
            : `点击按「${label}」降序排序`
        }
        className={cn(
          "inline-flex items-center gap-1 rounded-sm transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring",
          align === "right" && "flex-row-reverse",
          active && "text-foreground",
        )}
      >
        {label}
        <ArrowUpDown className={cn("size-3", active ? "opacity-90" : "opacity-40")} aria-hidden="true" />
      </button>
    </th>
  );
}

// -----------------------------------------------------------------------------
// 范围与视图
// -----------------------------------------------------------------------------

type RangeKey = "today" | "7d" | "30d" | "month";

const RANGE_OPTIONS: { value: RangeKey; label: string }[] = [
  { value: "30d", label: "近 30 天" },
  { value: "today", label: "今天" },
  { value: "7d", label: "近 7 天" },
  { value: "month", label: "本月" },
];

const SHARE_RANGE_OPTIONS: { value: RangeKey; label: string }[] = [
  { value: "today", label: "今天" },
  { value: "7d", label: "近 7 天" },
  { value: "30d", label: "近 30 天" },
  { value: "month", label: "本月" },
];

/**
 * 官方账本的查询窗口。
 *
 * 31 天同时覆盖「近 30 天」（需要 30 天）与「本月」（月初到月末最长 31 天），
 * 于是四个范围可以共用一次拉取、前端切片，切换范围不再等上游。
 */
const OFFICIAL_DAYS = 31;

/** 档位视图：全部 / 国内版 / 国际版。国内版与国际版积分体系不同，不合并计算。 */
type SiteView = "all" | "cn" | "intl";

const SITE_VIEW_OPTIONS: { value: SiteView; label: string }[] = [
  { value: "all", label: "全部" },
  { value: "cn", label: "国内版" },
  { value: "intl", label: "国际版" },
];

function siteViewLabel(view: SiteView): string {
  return SITE_VIEW_OPTIONS.find((o) => o.value === view)?.label ?? view;
}

/** 模型趋势共享数据色板；颜色由浅色/深色主题 token 提供。 */
const MODEL_COLORS = [
  "var(--data-series-emerald)",
  "var(--data-series-teal)",
  "var(--data-series-violet)",
  "var(--data-series-amber)",
  "var(--data-series-rose)",
  "var(--data-series-indigo)",
  "var(--data-series-sky)",
  "var(--data-series-lime)",
  "var(--data-series-orange)",
  "var(--data-series-pink)",
  "var(--data-series-cyan)",
  "var(--data-series-slate)",
];

const TOOLTIP_STYLE: CSSProperties = {
  background: "var(--popover)",
  border: "1px solid var(--border)",
  borderRadius: 8,
  fontSize: 12,
  color: "var(--popover-foreground)",
};

// -----------------------------------------------------------------------------
// 日期与格式化
// -----------------------------------------------------------------------------

function dateKey(date: Date): string {
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

function dateDaysAgo(days: number): string {
  const date = new Date();
  date.setHours(12, 0, 0, 0);
  date.setDate(date.getDate() - days);
  return dateKey(date);
}

/** 图表 X 轴：`2026-09-24` → `09/24`。 */
function formatChartDate(date: string): string {
  return date.slice(5).replace("-", "/");
}

/** 毫秒时间戳 → `09/25 18:52`；0 / 非法值 → `—`。 */
function formatDateTime(ms: number | null | undefined): string {
  if (!ms || !Number.isFinite(ms)) return "—";
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** 到期时间：`2026/09/30 23:59`；0 表示长期有效。 */
function formatExpiry(ms: number | null | undefined): string {
  if (!ms || !Number.isFinite(ms)) return "长期有效";
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}/${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// -----------------------------------------------------------------------------
// 范围切片（与 switch 同口径）
// -----------------------------------------------------------------------------

/** 按范围截取日期轴上的点。日期轴已由后端补齐，这里只做过滤。 */
function chartPoints(daily: OfficialUsageDay[], range: RangeKey): OfficialUsageDay[] {
  const today = dateKey(new Date());
  if (range === "month") {
    const prefix = `${today.slice(0, 7)}-`;
    return daily.filter((point) => point.date.startsWith(prefix));
  }
  const firstDate = range === "today" ? today : range === "7d" ? dateDaysAgo(6) : dateDaysAgo(29);
  return daily.filter((point) => point.date >= firstDate && point.date <= today);
}

/**
 * 某个范围内的消耗合计。
 *
 * 「今天 / 近 7 天 / 本月」直接取后端算好的窗口值（窗口边界按本地日历日定义，
 * 前端各算一遍容易把「今天」算成「最近 24 小时」）；只有「近 30 天」没有对应的
 * 汇总字段，由逐日数据求和。
 */
function rangeUsage(
  summary: { usage_today: number; usage_7d: number; usage_this_month: number },
  daily: OfficialUsageDay[],
  range: RangeKey,
): number {
  switch (range) {
    case "today":
      return summary.usage_today;
    case "7d":
      return summary.usage_7d;
    case "month":
      return summary.usage_this_month;
    case "30d": {
      const from = dateDaysAgo(29);
      const to = dateKey(new Date());
      return daily
        .filter((point) => point.date >= from && point.date <= to)
        .reduce((sum, point) => sum + point.credit, 0);
    }
  }
}

// -----------------------------------------------------------------------------
// 档位过滤：把官方账本按站点重算
// -----------------------------------------------------------------------------

/**
 * 只保留指定档位的账号，并**重算**日序列、模型合计与窗口汇总。
 *
 * 日期轴以原始 `official.days` 为基准（而不是从筛选后的账号里重建）：
 * 重建会让曲线在筛选后变短，X 轴范围随筛选跳动，读图时容易误判成「流量掉了」。
 */
function filterOfficial(
  official: OfficialUsageResponse,
  accounts: OfficialUsageAccount[],
): OfficialUsageResponse {
  type Bucket = { requests: number; credit: number; models: Map<string, { requests: number; credit: number }> };
  const byDate = new Map<string, Bucket>();
  const mergedModels = new Map<string, { requests: number; credit: number }>();
  let today = 0;
  let week = 0;
  let month = 0;
  let requests = 0;
  let credit = 0;

  const addModel = (
    target: Map<string, { requests: number; credit: number }>,
    model: OfficialUsageModel,
  ) => {
    const entry = target.get(model.model) ?? { requests: 0, credit: 0 };
    entry.requests += model.requests;
    entry.credit += model.credit;
    target.set(model.model, entry);
  };

  for (const account of accounts) {
    today += account.usage_today ?? 0;
    week += account.usage_7d ?? 0;
    month += account.usage_this_month ?? 0;
    requests += account.requests ?? 0;
    credit += account.credit ?? 0;
    for (const model of account.models ?? []) addModel(mergedModels, model);
    for (const day of account.days ?? []) {
      const bucket = byDate.get(day.date) ?? { requests: 0, credit: 0, models: new Map() };
      bucket.requests += day.requests;
      bucket.credit += day.credit;
      for (const model of day.models ?? []) addModel(bucket.models, model);
      byDate.set(day.date, bucket);
    }
  }

  const sortModels = (map: Map<string, { requests: number; credit: number }>): OfficialUsageModel[] =>
    [...map.entries()]
      .map(([model, value]) => ({ model, requests: value.requests, credit: value.credit }))
      .sort((a, b) => b.credit - a.credit || a.model.localeCompare(b.model));

  const visibleIds = new Set(accounts.map((a) => a.id));

  return {
    ...official,
    status: accounts.length === 0 ? "unavailable" : official.status,
    summary: {
      usage_today: today,
      usage_7d: week,
      usage_this_month: month,
      requests,
      credit,
    },
    days: official.days.map((day) => {
      const bucket = byDate.get(day.date);
      return {
        date: day.date,
        requests: bucket?.requests ?? 0,
        credit: bucket?.credit ?? 0,
        models: bucket ? sortModels(bucket.models) : [],
      };
    }),
    models: sortModels(mergedModels),
    accounts,
    requests: official.requests.filter((row) => visibleIds.has(row.account_id)),
    errors: official.errors.filter((row) => visibleIds.has(row.account_id)),
  };
}

// -----------------------------------------------------------------------------
// 本地观察口径（切片 19）：官方账本不可用时的回退
// -----------------------------------------------------------------------------

/**
 * 本地逐日序列的窗口切片。日期轴已由后端补齐（范围起点=最早快照与保留窗口下界的较大者）。
 */
function localChartPoints(daily: CreditStatsDailyPoint[], range: RangeKey): CreditStatsDailyPoint[] {
  const today = dateKey(new Date());
  if (range === "month") {
    const prefix = `${today.slice(0, 7)}-`;
    return daily.filter((point) => point.date.startsWith(prefix));
  }
  const firstDate = range === "today" ? today : range === "7d" ? dateDaysAgo(6) : dateDaysAgo(29);
  return daily.filter((point) => point.date >= firstDate && point.date <= today);
}

/** 本地观察的窗口合计（口径与官方版的 rangeUsage 一致）。 */
function localRangeUsage(
  summary: { usage_today: number; usage_7d: number; usage_this_month: number },
  daily: CreditStatsDailyPoint[],
  range: RangeKey,
): number {
  switch (range) {
    case "today":
      return summary.usage_today;
    case "7d":
      return summary.usage_7d;
    case "month":
      return summary.usage_this_month;
    case "30d": {
      const from = dateDaysAgo(29);
      const to = dateKey(new Date());
      return daily.filter((p) => p.date >= from && p.date <= to).reduce((sum, p) => sum + p.usage, 0);
    }
  }
}

/**
 * 档位过滤：本地观察同样按站点重算（对齐 switch 的 recomputeLocalStats）。
 *
 * 日期轴以原始 `stats.daily` 为基准（而不是从筛选后的账号重建）：
 * 重建会让曲线在筛选后变短，X 轴范围随筛选跳动。
 */
function filterStats(stats: CreditStatsResponse, site: SiteView): CreditStatsResponse {
  if (site === "all") return stats;
  const accounts = stats.accounts.filter((account) => account.site === site);
  const ids = new Set(accounts.map((account) => account.account_id));

  let currentRemaining = 0;
  let currentCapacity = 0;
  let usageToday = 0;
  let usage7d = 0;
  let usageMonth = 0;
  const usageByDate = new Map<string, number>();
  for (const account of accounts) {
    usageToday += account.usage_today;
    usage7d += account.usage_7d;
    usageMonth += account.usage_this_month;
    // 与后端口径一致：余额/容量只统计 is_current 账号。
    if (account.is_current) {
      currentRemaining += account.current_remaining ?? 0;
      currentCapacity += account.total_capacity ?? 0;
    }
    for (const point of account.daily ?? []) {
      usageByDate.set(point.date, (usageByDate.get(point.date) ?? 0) + point.usage);
    }
  }

  const events = stats.events.filter((event) => ids.has(event.account_id));
  const today = dateKey(new Date());
  let todaySuccess = 0;
  let todayAlready = 0;
  let todayFailed = 0;
  // 「今日已签到账号」按每账号最近一次结果判定（与后端/switch 同口径）。
  const latestByIdentity = new Map<string, { ts: number; result: string }>();
  for (const event of events) {
    if (event.kind !== "checkin" || event.date !== today) continue;
    if (event.result === "success") todaySuccess += 1;
    else if (event.result === "already") todayAlready += 1;
    else todayFailed += 1;
    const identity = event.account_id || `legacy:${event.account_name}`;
    const previous = latestByIdentity.get(identity);
    if (!previous || event.ts >= previous.ts) {
      latestByIdentity.set(identity, { ts: event.ts, result: event.result ?? "error" });
    }
  }
  let checkedIn = 0;
  for (const item of latestByIdentity.values()) {
    if (item.result === "success" || item.result === "already") checkedIn += 1;
  }

  return {
    ...stats,
    summary: {
      current_remaining: currentRemaining,
      current_capacity: currentCapacity,
      usage_today: usageToday,
      usage_7d: usage7d,
      usage_this_month: usageMonth,
      today_checked_in_accounts: checkedIn,
      today_success: todaySuccess,
      today_already: todayAlready,
      today_failed: todayFailed,
    },
    daily: stats.daily.map((point) => ({
      date: point.date,
      usage: usageByDate.get(point.date) ?? 0,
    })),
    accounts,
    events,
  };
}

// -----------------------------------------------------------------------------
// 堆叠柱：数据与形状
// -----------------------------------------------------------------------------

interface ModelChartPoint {
  date: string;
  total: number;
  [model: string]: number | string;
}

/** 从官方逐日数据构建层叠序列；模型按总消耗降序全部保留。 */
function buildStacked(daily: OfficialUsageDay[]): { models: string[]; points: ModelChartPoint[] } {
  const totals = new Map<string, number>();
  for (const day of daily) {
    for (const model of day.models ?? []) {
      totals.set(model.model, (totals.get(model.model) ?? 0) + model.credit);
    }
  }
  const models = [...totals.entries()].sort((a, b) => b[1] - a[1]).map(([model]) => model);
  const points: ModelChartPoint[] = daily.map((day) => {
    const entry: ModelChartPoint = { date: day.date, total: day.credit };
    for (const model of day.models ?? []) {
      const key = model.model;
      entry[key] = (typeof entry[key] === "number" ? (entry[key] as number) : 0) + model.credit;
    }
    return entry;
  });
  return { models, points };
}

type CreditBarShapeProps = ComponentProps<typeof Rectangle> & {
  segmentKey?: string;
  seriesKeys?: string[];
  payload?: ModelChartPoint;
  /** recharts 在堆叠柱下会把「下方已累计值」放进 value 元组（2.x）；3.x 是单值。 */
  value?: number | [number, number];
};

/**
 * 堆叠柱的单段形状。
 *
 * 两件事，都是为了让图**如实**可读：
 *
 *  1. **零值段不画**。recharts 的 `minPointSize` 对零值也生效，会凭空多出
 *     一小截柱子 —— 那是「这天有消耗」的错误陈述，实测踩过，所以不用它。
 *  2. **非零段保底 5px**。某天 459 积分里只占 1.28 的模型，按比例算不足 1px，
 *     肉眼看不见；图例里有名字、柱子上找不到，读图的人会以为数据错了。
 *     保底由 `getStackedSegmentVisualLayout` 做，缺口从胖段按比例扣，总量守恒。
 */
function CreditBarShape(props: CreditBarShapeProps) {
  const {
    x = 0,
    y = 0,
    width = 0,
    height = 0,
    fill,
    stroke,
    strokeWidth,
    segmentKey,
    seriesKeys,
    payload,
    value,
  } = props;
  if (!width || !height || segmentKey === undefined) return null;

  const keys = seriesKeys ?? [];
  const segmentIndex = keys.indexOf(segmentKey);
  const values = keys.map((key) => Number(payload?.[key] ?? 0));
  // 零值直接不画：这是「不画」与「画一点点」的分界，不能交给 recharts 的
  // minPointSize 决定。
  if (segmentIndex < 0 || !(values[segmentIndex] > 0)) return null;

  const stackStart = Array.isArray(value)
    ? Number(value[0])
    : values.slice(0, segmentIndex).reduce((sum, v) => sum + v, 0);

  const layout = getStackedSegmentVisualLayout({
    values,
    segmentIndex,
    segmentHeight: height,
    segmentY: y,
    stackStart,
  });

  return (
    <Rectangle
      x={x}
      y={layout?.y ?? y}
      width={width}
      height={layout?.height ?? height}
      fill={fill}
      stroke={stroke ?? "var(--background)"}
      strokeWidth={strokeWidth ?? 2}
      radius={layout?.isTop ? [6, 6, 0, 0] : 0}
    />
  );
}

// -----------------------------------------------------------------------------
// 账号筛选
// -----------------------------------------------------------------------------

function AccountFilter({
  accounts,
  value,
  onChange,
  ariaLabel,
  allowAll = true,
}: {
  accounts: { id: string; label: string }[];
  value: string | null;
  onChange: (id: string | null) => void;
  ariaLabel: string;
  /** false 时隐藏「所有账号」，仅允许选择具体账号（积分明细按账号看）。 */
  allowAll?: boolean;
}) {
  const known = accounts.some((a) => a.id === value);
  const current = known ? (value as string) : allowAll ? "" : (accounts[0]?.id ?? "");
  return (
    <span className="relative inline-flex items-center">
      <Users className="pointer-events-none absolute left-2 size-3.5 text-muted-foreground" aria-hidden="true" />
      <select
        aria-label={ariaLabel}
        value={current}
        onChange={(e) => onChange(e.target.value || null)}
        className="h-8 max-w-[190px] cursor-pointer appearance-none truncate rounded-md bg-transparent pl-7 pr-6 text-xs text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
      >
        {allowAll ? <option value="">所有账号</option> : null}
        {accounts.map((a) => (
          <option key={a.id} value={a.id}>
            {a.label}
          </option>
        ))}
      </select>
      <ChevronDown className="pointer-events-none absolute right-1.5 size-3.5 text-muted-foreground" aria-hidden="true" />
    </span>
  );
}

function accountLabel(account: { nickname?: string; id: string }): string {
  return account.nickname || account.id;
}

// -----------------------------------------------------------------------------
// 页面
// -----------------------------------------------------------------------------

/**
 * 模块级缓存（对齐 switch）：页面间来回切换不重复打上游；
 * 超过 `STATS_AUTO_REFRESH_MS` 后由定时器静默重采。
 */
interface CreditStatsCache {
  official: OfficialUsageResponse | null;
  officialError: string | null;
  credits: CreditsResponse | null;
  creditsError: string | null;
  stats: CreditStatsResponse | null;
  loadedAt: number;
}

let moduleCache: CreditStatsCache | null = null;
const STATS_AUTO_REFRESH_MS = 30 * 60 * 1000;

export function CreditStatsPage() {
  const [official, setOfficial] = useState<OfficialUsageResponse | null>(moduleCache?.official ?? null);
  const [officialError, setOfficialError] = useState<string | null>(moduleCache?.officialError ?? null);
  const [credits, setCredits] = useState<CreditsResponse | null>(moduleCache?.credits ?? null);
  const [creditsError, setCreditsError] = useState<string | null>(moduleCache?.creditsError ?? null);
  const [stats, setStats] = useState<CreditStatsResponse | null>(moduleCache?.stats ?? null);
  const [loading, setLoading] = useState(moduleCache === null);
  const [viewSite, setViewSite] = useState<SiteView>("all");

  const load = useCallback(async (force = false) => {
    if (!force && moduleCache && Date.now() - moduleCache.loadedAt < STATS_AUTO_REFRESH_MS) {
      setOfficial(moduleCache.official);
      setOfficialError(moduleCache.officialError);
      setCredits(moduleCache.credits);
      setCreditsError(moduleCache.creditsError);
      setStats(moduleCache.stats);
      setLoading(false);
      return;
    }
    setLoading(true);
    // 三个来源并行取、独立降级：账本挂了不该让「剩余积分」跟着变 0；
    // 本地观察台账（纯本地快照）则永远可用 —— 官方挂掉时它就是回退口径。
    const [o, c, s] = await Promise.allSettled([
      fetchOfficialUsage(OFFICIAL_DAYS),
      fetchCredits(),
      fetchCreditStatistics(),
    ]);
    const next: CreditStatsCache = {
      official: o.status === "fulfilled" ? o.value : null,
      officialError: o.status === "rejected" ? describeError(o.reason) : null,
      credits: c.status === "fulfilled" ? c.value : null,
      creditsError: c.status === "rejected" ? describeError(c.reason) : null,
      stats: s.status === "fulfilled" ? s.value : null,
      loadedAt: Date.now(),
    };
    moduleCache = next;
    setOfficial(next.official);
    setOfficialError(next.officialError);
    setCredits(next.credits);
    setCreditsError(next.creditsError);
    setStats(next.stats);
    setLoading(false);
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(true), STATS_AUTO_REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [load]);

  /** 按档位过滤后的官方账本（全部时原样使用后端聚合值）。 */
  const scoped = useMemo(() => {
    if (!official) return null;
    if (viewSite === "all") return official;
    return filterOfficial(
      official,
      official.accounts.filter((a) => a.site === viewSite),
    );
  }, [official, viewSite]);

  /** 按档位过滤后的本地观察（官方不可用时的回退口径）。 */
  const scopedStats = useMemo(() => (stats ? filterStats(stats, viewSite) : null), [stats, viewSite]);

  /** 按档位过滤后的资源包（剩余积分与积分明细用同一份，避免两处口径不一致）。 */
  const scopedCredits = useMemo(() => {
    if (!credits) return null;
    if (viewSite === "all") return credits;
    const accounts = credits.accounts.filter((a) => a.site === viewSite);
    const currentRemaining = accounts.reduce(
      (sum, a) => sum + (a.resources ?? []).reduce((s, r) => s + r.remaining, 0),
      0,
    );
    const currentCapacity = accounts.reduce(
      (sum, a) => sum + (a.resources ?? []).reduce((s, r) => s + r.total, 0),
      0,
    );
    return { ...credits, accounts, summary: { current_remaining: currentRemaining, current_capacity: currentCapacity } };
  }, [credits, viewSite]);

  const officialAccounts = scoped?.accounts ?? [];
  const officialAvailable =
    scoped !== null && scoped.status !== "unavailable" && officialAccounts.length > 0;
  const variantEmpty = official !== null && viewSite !== "all" && officialAccounts.length === 0;

  const filterOptions = useMemo(
    () => officialAccounts.map((a) => ({ id: a.id, label: accountLabel(a) })),
    [officialAccounts],
  );

  /** 明细筛选项：官方账号优先，官方不可用时回退本地观察的账号（对齐 switch）。 */
  const detailAccounts = useMemo(() => {
    if (filterOptions.length > 0) return filterOptions;
    return (scopedStats?.accounts ?? []).map((a) => ({
      id: a.account_id,
      label: a.account_name || a.account_id,
    }));
  }, [filterOptions, scopedStats]);

  // 页头时间取官方采集时刻；官方不可用时用本地台账的生成时刻（对齐 switch）。
  const headerUpdatedAt = official?.collected_at ?? stats?.generated_at ?? 0;

  return (
    <div className="mx-auto w-full max-w-[1180px] min-w-0 px-4 py-6 sm:px-8 sm:py-9">
      <header className="mb-10 flex min-w-0 flex-wrap items-start justify-between gap-4 sm:mb-12">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">积分统计</h1>
          <p className="mt-2 max-w-2xl text-sm leading-6 text-muted-foreground">
            当前数据更新于 {formatDateTime(headerUpdatedAt)}
          </p>
        </div>
        <div className="flex min-w-0 max-w-full flex-wrap items-center justify-end gap-2">
          <PillGroup
            value={viewSite}
            options={SITE_VIEW_OPTIONS}
            onChange={setViewSite}
            ariaLabel="档位筛选"
          />
          <Button
            type="button"
            className="shrink-0"
            variant="outline"
            size="sm"
            disabled={loading}
            onClick={() => void load(true)}
          >
            {loading ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <RefreshCw className="size-3.5" aria-hidden="true" />
            )}
            刷新统计
          </Button>
        </div>
      </header>

      {!official && !credits && !stats && loading ? (
        <LoadingSkeleton />
      ) : variantEmpty ? (
        <div className="rounded-xl border border-dashed px-4 py-16 text-center text-sm text-muted-foreground">
          <p>暂无{siteViewLabel(viewSite)}账号的积分数据。</p>
          <p className="mt-2 text-xs leading-5">国内版与国际版积分体系不同，不会合并计算。</p>
        </div>
      ) : (
        <div className="min-w-0 space-y-12">
          <OfficialStatusAlert
            official={official}
            officialError={officialError}
            credits={credits}
            creditsError={creditsError}
          />

          {/* 积分总览：一条卡片切四份；官方不可用时用本地观察口径回退 */}
          <Card
            className="min-w-0 gap-0 overflow-hidden rounded-2xl bg-card/70 py-0 shadow-none"
            aria-label="积分总览"
          >
            <MetricRow>
              <StatMetric
                icon={Sparkles}
                label="剩余积分"
                value={
                  scopedCredits
                    ? formatCredits(scopedCredits.summary.current_remaining)
                    : scopedStats
                      ? formatCredits(scopedStats.summary.current_remaining)
                      : "—"
                }
              />
              <StatMetric
                icon={TrendingDown}
                label="今日消耗"
                value={
                  officialAvailable
                    ? formatCredits(scoped!.summary.usage_today)
                    : scopedStats
                      ? formatCredits(scopedStats.summary.usage_today)
                      : "—"
                }
                divided
              />
              <StatMetric
                icon={CalendarDays}
                label="近 7 天消耗"
                value={
                  officialAvailable
                    ? formatCredits(scoped!.summary.usage_7d)
                    : scopedStats
                      ? formatCredits(scopedStats.summary.usage_7d)
                      : "—"
                }
                divided
              />
              <StatMetric
                icon={CalendarRange}
                label="本月消耗"
                value={
                  officialAvailable
                    ? formatCredits(scoped!.summary.usage_this_month)
                    : scopedStats
                      ? formatCredits(scopedStats.summary.usage_this_month)
                      : "—"
                }
                divided
              />
            </MetricRow>
          </Card>

          {officialAvailable ? (
            <>
              <AccountUsageShare official={scoped!} siteView={viewSite} />
              <CreditTrend official={scoped!} accounts={filterOptions} />
              <ModelBreakdown official={scoped!} accounts={filterOptions} />
            </>
          ) : (
            <>
              {!scopedStats?.coverage_start_at &&
              (scopedStats?.events ?? []).some((event) => event.kind === "checkin") ? (
                <div className="flex items-start gap-2 rounded-xl border px-4 py-3">
                  <CircleCheck
                    className="mt-0.5 size-4 shrink-0 text-emerald-600 dark:text-emerald-400"
                    aria-hidden="true"
                  />
                  <div className="min-w-0">
                    <div className="text-[13px] font-medium">目前只有签到记录</div>
                    <p className="mt-0.5 text-xs leading-5 text-muted-foreground">
                      签到不会被计入积分消耗。首次成功采集积分资源后，趋势统计才会开始累计。
                    </p>
                  </div>
                </div>
              ) : null}
              <AccountUsageShareLocal stats={scopedStats} siteView={viewSite} />
              <CreditTrendLocal stats={scopedStats} />
            </>
          )}

          <CreditDetail
            official={official}
            credits={scopedCredits}
            creditsError={creditsError}
            accounts={detailAccounts}
          />

          <CardHint>
            「消耗」优先来自 WorkBuddy 官方请求用量接口（上游计费口径）
            {official ? `（${official.range_start} 至 ${official.range_end}）` : ""}
            ；官方不可用时回退为本地观察口径（连续快照中余额下降的正差值）。
            「剩余积分」与「积分明细」来自积分资源包接口。
            页头显示的是最近一次成功采集的时间。
          </CardHint>
        </div>
      )}
    </div>
  );
}

// -----------------------------------------------------------------------------
// 状态提示
// -----------------------------------------------------------------------------

function OfficialStatusAlert({
  official,
  officialError,
  credits,
  creditsError,
}: {
  official: OfficialUsageResponse | null;
  officialError: string | null;
  credits: CreditsResponse | null;
  creditsError: string | null;
}) {
  const rows: { key: string; title: string; detail: string }[] = [];

  if (officialError) {
    rows.push({
      key: "official",
      title: "官方用量暂不可用",
      detail: `${officialError}。今日、近 7 天和本月消耗将使用本地观察口径；官方接口恢复后刷新即可重新同步。`,
    });
  } else if (official && official.status === "unavailable") {
    rows.push({
      key: "official",
      title: "官方用量暂不可用",
      detail:
        "今日、近 7 天和本月消耗将使用本地观察口径；官方接口恢复后刷新即可重新同步。",
    });
  } else if (official && official.status === "partial") {
    const ok = official.accounts.filter((a) => a.ok).length;
    const failed = official.accounts.filter((a) => !a.ok);
    rows.push({
      key: "official-partial",
      title: "部分账号官方用量未同步",
      detail: `已同步 ${ok}/${official.accounts.length} 个当前账号；失败账号的官方数值显示为「—」。${
        failed.length > 0
          ? " " + failed.map((a) => `${accountLabel(a)}：${a.error ?? "读取失败"}`).join("；")
          : ""
      }`,
    });
  }

  if (creditsError) {
    rows.push({
      key: "credits",
      title: "积分资源包读取失败",
      detail: `${creditsError}。「剩余积分」与「积分明细」暂不可用，消耗类指标不受影响。`,
    });
  } else if (credits && credits.fail_count > 0) {
    rows.push({
      key: "credits-partial",
      title: "部分账号资源包未同步",
      detail: `${credits.ok_count}/${credits.accounts.length} 个账号读取成功，其余账号的明细显示失败原因。`,
    });
  }

  if (rows.length === 0) return null;

  return (
    <div className="min-w-0 space-y-2">
      {rows.map((row) => (
        <div key={row.key} className="flex items-start gap-2 rounded-xl border px-4 py-3">
          <CircleAlert className="mt-0.5 size-4 shrink-0 text-amber-700 dark:text-amber-400" aria-hidden="true" />
          <div className="min-w-0">
            <div className="text-[13px] font-medium">{row.title}</div>
            <p className="mt-0.5 break-words text-xs leading-5 text-muted-foreground">{row.detail}</p>
          </div>
        </div>
      ))}
    </div>
  );
}

// -----------------------------------------------------------------------------
// 账号消耗构成
// -----------------------------------------------------------------------------

/** 档位分组：全部视图下把行按站点分组（占比分母=组内合计）。 */
function groupRowsBySite(rows: ShareRow[], siteById: Map<string, string>, siteView: SiteView) {
  if (siteView !== "all") return [];
  const bySite = new Map<string, ShareRow[]>();
  for (const row of rows) {
    const site = siteById.get(row.key) ?? "cn";
    const list = bySite.get(site) ?? [];
    list.push(row);
    bySite.set(site, list);
  }
  if (bySite.size <= 1) return [];
  return [...bySite.entries()].map(([site, list]) => ({
    site,
    label: site === "intl" ? "国际版" : "国内版",
    total: list.filter((row) => !row.unavailable).reduce((sum, row) => sum + row.value, 0),
    rows: list,
  }));
}

/** 构成区内容：多档位时分组渲染（组内合计 + 组内占比），否则单一列表。 */
function UsageShareBody({
  rows,
  groups,
  countText,
  total,
}: {
  rows: ShareRow[];
  groups: ReturnType<typeof groupRowsBySite>;
  countText: string;
  total: number;
}) {
  if (groups.length > 1) {
    return (
      <div className="space-y-5">
        {groups.map((group) => (
          <div key={group.site} className="min-w-0">
            <div className="mb-2 flex flex-wrap items-center justify-between gap-2 text-xs">
              <Badge variant="outline">{group.label}</Badge>
              <span className="text-muted-foreground">合计 {formatCredits(group.total)} 积分</span>
            </div>
            <ShareRows rows={group.rows} />
          </div>
        ))}
      </div>
    );
  }
  return (
    <>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
        <span>{countText}</span>
        <span className="font-medium text-foreground">合计 {formatCredits(total)} 积分</span>
      </div>
      <ShareRows rows={rows} />
    </>
  );
}

function AccountUsageShare({
  official,
  siteView,
}: {
  official: OfficialUsageResponse;
  siteView: SiteView;
}) {
  const [range, setRange] = useState<RangeKey>("today");

  const rows: ShareRow[] = useMemo(() => {
    const list = official.accounts.map((account) => {
      if (!account.ok) {
        return {
          key: account.id,
          title: accountLabel(account),
          valueText: "官方用量不可用",
          value: 0,
          unavailable: true,
        };
      }
      const value = rangeUsage(
        {
          usage_today: account.usage_today ?? 0,
          usage_7d: account.usage_7d ?? 0,
          usage_this_month: account.usage_this_month ?? 0,
        },
        account.days ?? [],
        range,
      );
      return {
        key: account.id,
        title: accountLabel(account),
        valueText: `${formatCredits(value)} 积分`,
        value,
      };
    });
    return list.sort((a, b) => (b.value ?? 0) - (a.value ?? 0) || a.title.localeCompare(b.title));
  }, [official, range]);

  const measurable = rows.filter((row) => !row.unavailable);
  const grandTotal = measurable.reduce((sum, row) => sum + row.value, 0);
  const groups = useMemo(
    () => groupRowsBySite(rows, new Map(official.accounts.map((a) => [a.id, a.site])), siteView),
    [rows, official, siteView],
  );

  return (
    <Section
      id="credit-share"
      title="账号消耗构成"
      description={`来自 WorkBuddy 官方请求用量 · ${official.range_start} 至 ${official.range_end}`}
      action={
        <PillGroup
          value={range}
          options={SHARE_RANGE_OPTIONS}
          onChange={setRange}
          ariaLabel="账号消耗构成范围"
        />
      }
    >
      {measurable.length === 0 ? (
        <CardContent className="px-4 py-8 text-center text-sm text-muted-foreground sm:px-5">
          当前账号的官方用量暂不可用，刷新后重试。
        </CardContent>
      ) : grandTotal <= 0 ? (
        <CardContent className="px-4 py-8 text-center text-sm text-muted-foreground sm:px-5">
          当前范围暂未观察到积分消耗。
        </CardContent>
      ) : (
        <CardContent className="min-w-0 px-4 pt-4 pb-4 sm:px-5">
          <UsageShareBody
            rows={rows}
            groups={groups}
            countText={`共 ${rows.length} 个账号`}
            total={grandTotal}
          />
        </CardContent>
      )}
    </Section>
  );
}

/** 账号消耗构成（本地观察口径）：官方账本不可用时的回退版本。 */
function AccountUsageShareLocal({
  stats,
  siteView,
}: {
  stats: CreditStatsResponse | null;
  siteView: SiteView;
}) {
  const [range, setRange] = useState<RangeKey>("today");
  const accounts = stats?.accounts ?? [];

  const rows: ShareRow[] = useMemo(() => {
    const list = accounts.map((account) => {
      const hasSnapshot = (account.last_snapshot_at ?? 0) > 0;
      const value = hasSnapshot
        ? localRangeUsage(
            {
              usage_today: account.usage_today,
              usage_7d: account.usage_7d,
              usage_this_month: account.usage_this_month,
            },
            account.daily ?? [],
            range,
          )
        : 0;
      return {
        key: account.account_id,
        title: account.account_name || account.account_id,
        valueText: hasSnapshot ? `${formatCredits(value)} 积分` : "本地观察暂不可用",
        value,
        unavailable: !hasSnapshot,
      };
    });
    return list.sort((a, b) => (b.value ?? 0) - (a.value ?? 0) || a.title.localeCompare(b.title));
  }, [accounts, range]);

  const measurable = rows.filter((row) => !row.unavailable);
  const grandTotal = measurable.reduce((sum, row) => sum + row.value, 0);
  const groups = useMemo(
    () => groupRowsBySite(rows, new Map(accounts.map((a) => [a.account_id, a.site])), siteView),
    [rows, accounts, siteView],
  );
  const coverage = stats?.coverage_start_at ? formatDateTime(stats.coverage_start_at) : null;

  return (
    <Section
      id="credit-share"
      title="账号消耗构成"
      description={`来自本地观察（连续快照余额下降的正差值）${
        coverage ? ` · 数据覆盖自 ${coverage}` : ""
      }`}
      action={
        <PillGroup
          value={range}
          options={SHARE_RANGE_OPTIONS}
          onChange={setRange}
          ariaLabel="账号消耗构成范围"
        />
      }
    >
      {accounts.length === 0 ? (
        <CardContent className="px-4 py-8 text-center text-sm text-muted-foreground sm:px-5">
          暂无账号消耗数据。
        </CardContent>
      ) : measurable.length === 0 ? (
        <CardContent className="px-4 py-8 text-center text-sm text-muted-foreground sm:px-5">
          本地观察暂不可用，等待下一次积分资源采集。
        </CardContent>
      ) : grandTotal <= 0 ? (
        <CardContent className="px-4 py-8 text-center text-sm text-muted-foreground sm:px-5">
          当前范围暂未观察到积分消耗。
        </CardContent>
      ) : (
        <CardContent className="min-w-0 px-4 pt-4 pb-4 sm:px-5">
          <UsageShareBody
            rows={rows}
            groups={groups}
            countText={`共 ${rows.length} 个账号`}
            total={grandTotal}
          />
        </CardContent>
      )}
    </Section>
  );
}

// -----------------------------------------------------------------------------
// 官方积分消耗（按模型堆叠）
// -----------------------------------------------------------------------------

function CreditTrend({
  official,
  accounts,
}: {
  official: OfficialUsageResponse;
  accounts: { id: string; label: string }[];
}) {
  /** null = 所有账号汇总；本卡片独立，不影响其他卡片 */
  const [accountFilter, setAccountFilter] = useState<string | null>(null);
  /** 本卡片独立的时间范围，不影响其他卡片 */
  const [range, setRange] = useState<RangeKey>("30d");

  const active = accounts.find((a) => a.id === accountFilter);
  const account = active ? official.accounts.find((a) => a.id === active.id) : undefined;

  // 选中账号时切到该账号的逐日数据与窗口汇总；否则用全部账号的聚合
  const daily = account ? (account.days ?? []) : official.days;
  const summary = account
    ? {
        usage_today: account.usage_today ?? 0,
        usage_7d: account.usage_7d ?? 0,
        usage_this_month: account.usage_this_month ?? 0,
      }
    : {
        usage_today: official.summary.usage_today,
        usage_7d: official.summary.usage_7d,
        usage_this_month: official.summary.usage_this_month,
      };

  const basePoints = chartPoints(daily, range);
  const hasModelDetail = basePoints.some((point) => (point.models?.length ?? 0) > 0);
  const stacked = buildStacked(basePoints);
  const chartData = stacked.points;
  const series = hasModelDetail ? stacked.models : ["total"];
  const chartConfig: Record<string, string> = {};
  for (const model of series) {
    chartConfig[model] =
      model === "total"
        ? "var(--data-series-emerald)"
        : MODEL_COLORS[series.indexOf(model) % MODEL_COLORS.length];
  }

  const observedTotal = rangeUsage(summary, daily, range);
  const hasObserved = chartData.some((point) => point.total > 0);

  return (
    <Section
      id="credit-trend"
      title="官方积分消耗"
      description={`来自 WorkBuddy 官方请求用量 · ${official.range_start} 至 ${official.range_end}`}
      action={
        <>
          <AccountFilter
            accounts={accounts}
            value={accountFilter}
            onChange={setAccountFilter}
            ariaLabel="按账号筛选趋势"
          />
          <PillGroup value={range} options={RANGE_OPTIONS} onChange={setRange} ariaLabel="趋势范围" />
        </>
      }
    >
      <CardContent className="min-w-0 px-4 pt-4 pb-4 sm:px-5">
        {chartData.length === 0 ? (
          <EmptyHint>当前口径暂无可展示的观察数据。</EmptyHint>
        ) : (
          <>
            <ResponsiveContainer width="100%" height={224}>
              <BarChart data={chartData} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
                <CartesianGrid vertical={false} strokeDasharray="3 3" stroke="var(--border)" />
                <XAxis
                  dataKey="date"
                  tickLine={false}
                  axisLine={false}
                  tickMargin={8}
                  tick={{ fontSize: 12, fill: "var(--muted-foreground)" }}
                  stroke="var(--border)"
                  tickFormatter={(value) => formatChartDate(String(value))}
                  minTickGap={24}
                />
                <YAxis
                  tickLine={false}
                  axisLine={false}
                  width={42}
                  tick={{ fontSize: 12, fill: "var(--muted-foreground)" }}
                  stroke="var(--border)"
                  tickFormatter={(value: number) => formatCredits(value)}
                />
                <Tooltip
                  contentStyle={TOOLTIP_STYLE}
                  cursor={{ fill: "var(--muted)", opacity: 0.4 }}
                  content={<CreditTooltip />}
                />
                {series.map((model) => (
                  <Bar
                    key={model}
                    dataKey={model}
                    stackId="usage"
                    fill={chartConfig[model]}
                    stroke="var(--background)"
                    strokeWidth={2}
                    maxBarSize={28}
                    shape={<CreditBarShape segmentKey={model} seriesKeys={series} />}
                    isAnimationActive={false}
                  />
                ))}
              </BarChart>
            </ResponsiveContainer>

            {hasModelDetail ? (
              <div className="mt-3 flex flex-wrap items-center justify-center gap-x-4 gap-y-1.5 text-xs text-muted-foreground">
                {stacked.models.map((model, index) => (
                  <span key={model} className="inline-flex items-center gap-1.5">
                    <span
                      className="h-2 w-2 shrink-0 rounded-[2px]"
                      style={{ backgroundColor: MODEL_COLORS[index % MODEL_COLORS.length] }}
                      aria-hidden="true"
                    />
                    {model}
                  </span>
                ))}
              </div>
            ) : null}

            <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
              <span>
                {hasObserved
                  ? `当前口径合计 ${formatCredits(observedTotal)} 积分`
                  : "已采集快照，当前范围暂无积分消耗"}
              </span>
              <span>数据更新于 {formatDateTime(official.collected_at)}</span>
            </div>
            <p className="sr-only">
              {chartData
                .map((point) => `${point.date} 消耗 ${formatCredits(Number(point.total) || 0)} 积分`)
                .join("；")}
            </p>
          </>
        )}
      </CardContent>
    </Section>
  );
}

/** 官方账本不可用时的本地观察趋势：单层「总消耗」柱（余额下降的正差值）。 */
function CreditTrendLocal({ stats }: { stats: CreditStatsResponse | null }) {
  const [range, setRange] = useState<RangeKey>("30d");
  const [accountFilter, setAccountFilter] = useState<string | null>(null);
  const accounts = (stats?.accounts ?? []).map((a) => ({
    id: a.account_id,
    label: a.account_name || a.account_id,
  }));
  const active = accounts.find((a) => a.id === accountFilter);
  const account = active ? (stats?.accounts ?? []).find((a) => a.account_id === active.id) : undefined;

  const daily = account ? (account.daily ?? []) : (stats?.daily ?? []);
  const summary = account
    ? {
        usage_today: account.usage_today,
        usage_7d: account.usage_7d,
        usage_this_month: account.usage_this_month,
      }
    : {
        usage_today: stats?.summary.usage_today ?? 0,
        usage_7d: stats?.summary.usage_7d ?? 0,
        usage_this_month: stats?.summary.usage_this_month ?? 0,
      };

  const basePoints = localChartPoints(daily, range);
  const chartData = basePoints.map((point) => ({ date: point.date, total: point.usage }));
  const observedTotal = localRangeUsage(summary, daily, range);
  const hasObserved = chartData.some((point) => point.total > 0);
  // 没有任何快照 → 连坐标轴都不画（对齐 switch：先提示「首次采集后才开始累计」）。
  const hasDataSource = Boolean(stats?.coverage_start_at);

  return (
    <Section
      id="credit-trend"
      title="本地观察积分消耗"
      description="只统计连续快照中余额下降的正差值；官方用量暂不可用时保留此口径。"
      action={
        <>
          {accounts.length > 0 ? (
            <AccountFilter
              accounts={accounts}
              value={accountFilter}
              onChange={setAccountFilter}
              ariaLabel="按账号筛选趋势"
            />
          ) : null}
          <PillGroup value={range} options={RANGE_OPTIONS} onChange={setRange} ariaLabel="趋势范围" />
        </>
      }
    >
      <CardContent className="min-w-0 px-4 pt-3 pb-4 sm:px-5">
        {!hasDataSource ? (
          <EmptyHint>尚无积分快照。首次成功采集后，统计会从该时刻开始累计。</EmptyHint>
        ) : chartData.length === 0 ? (
          <EmptyHint>当前口径暂无可展示的观察数据。</EmptyHint>
        ) : (
          <>
            <ResponsiveContainer width="100%" height={224}>
              <BarChart data={chartData} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
                <CartesianGrid vertical={false} strokeDasharray="3 3" stroke="var(--border)" />
                <XAxis
                  dataKey="date"
                  tickLine={false}
                  axisLine={false}
                  tickMargin={8}
                  tick={{ fontSize: 12, fill: "var(--muted-foreground)" }}
                  stroke="var(--border)"
                  tickFormatter={(value) => formatChartDate(String(value))}
                  minTickGap={24}
                />
                <YAxis
                  tickLine={false}
                  axisLine={false}
                  width={42}
                  tick={{ fontSize: 12, fill: "var(--muted-foreground)" }}
                  stroke="var(--border)"
                  tickFormatter={(value: number) => formatCredits(value)}
                />
                <Tooltip
                  contentStyle={TOOLTIP_STYLE}
                  cursor={{ fill: "var(--muted)", opacity: 0.4 }}
                  content={<CreditTooltip />}
                />
                <Bar
                  dataKey="total"
                  fill="var(--data-series-emerald)"
                  stroke="var(--background)"
                  strokeWidth={2}
                  maxBarSize={28}
                  isAnimationActive={false}
                />
              </BarChart>
            </ResponsiveContainer>
            <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
              <span>
                {hasObserved
                  ? `当前口径合计 ${formatCredits(observedTotal)} 积分`
                  : "已采集快照，暂未观察到余额下降"}
              </span>
              <span>数据覆盖至 {formatDateTime(stats?.generated_at)}</span>
            </div>
            <p className="sr-only">
              {chartData
                .map((point) => `${point.date} 消耗 ${formatCredits(point.total)} 积分`)
                .join("；")}
            </p>
          </>
        )}
      </CardContent>
    </Section>
  );
}

/** 趋势图提示框：标题为日期，按堆叠顺序列出消耗（含 0 值与图例顺序一致）。 */
function CreditTooltip(props: {
  active?: boolean;
  label?: string | number;
  payload?: { dataKey?: string | number; value?: number | string; color?: string }[];
}) {
  const { active, label, payload } = props;
  if (!active || !payload || payload.length === 0) return null;
  return (
    <div style={TOOLTIP_STYLE} className="min-w-[160px] px-2.5 py-2">
      <div className="mb-1.5 font-medium">{formatChartDate(String(label ?? ""))} 消耗</div>
      {payload.map((item) => (
        <div
          key={String(item.dataKey)}
          className="flex items-center justify-between gap-3 tabular-nums"
        >
          <span className="inline-flex min-w-0 items-center gap-1.5">
            <span
              className="h-2 w-2 shrink-0 rounded-full"
              style={{ backgroundColor: item.color }}
              aria-hidden="true"
            />
            <span className="truncate">{String(item.dataKey)}</span>
          </span>
          <span className="shrink-0">{formatCredits(Number(item.value ?? 0))}</span>
        </div>
      ))}
    </div>
  );
}

// -----------------------------------------------------------------------------
// 按模型分类
// -----------------------------------------------------------------------------

function ModelBreakdown({
  official,
  accounts,
}: {
  official: OfficialUsageResponse;
  accounts: { id: string; label: string }[];
}) {
  /** null = 所有账号汇总；本卡片独立，不影响其他卡片 */
  const [accountFilter, setAccountFilter] = useState<string | null>(null);
  /** 本卡片独立的时间范围，不影响其他卡片 */
  const [range, setRange] = useState<RangeKey>("30d");

  const active = accounts.find((a) => a.id === accountFilter);
  const account = active ? official.accounts.find((a) => a.id === active.id) : undefined;
  const basePoints = chartPoints(account ? (account.days ?? []) : official.days, range);

  // 从逐日模型聚合求和（全量，不受明细条数上限影响）
  const models = useMemo(() => {
    const map = new Map<string, { requests: number; credit: number }>();
    for (const point of basePoints) {
      for (const item of point.models ?? []) {
        const entry = map.get(item.model) ?? { requests: 0, credit: 0 };
        entry.requests += item.requests;
        entry.credit += item.credit;
        map.set(item.model, entry);
      }
    }
    return [...map.entries()]
      .map(([model, value]) => ({ model, requests: value.requests, credit: value.credit }))
      .sort((a, b) => b.credit - a.credit || b.requests - a.requests || a.model.localeCompare(b.model));
  }, [basePoints]);

  const totalCredit = models.reduce((sum, m) => sum + m.credit, 0);
  const totalRequests = models.reduce((sum, m) => sum + m.requests, 0);

  const rows: ShareRow[] = models.map((m) => ({
    key: m.model,
    title: m.model === "—" || !m.model ? "未知模型" : m.model,
    valueText: `${formatCredits(m.credit)} 积分 · ${formatCredits(m.requests)} 次`,
    value: m.credit,
  }));

  return (
    <Section
      id="credit-models"
      title="按模型分类"
      description={<Badge variant="outline">{models.length} 个模型</Badge>}
      action={
        <>
          <AccountFilter
            accounts={accounts}
            value={accountFilter}
            onChange={setAccountFilter}
            ariaLabel="按账号筛选模型分类"
          />
          <PillGroup value={range} options={RANGE_OPTIONS} onChange={setRange} ariaLabel="模型分类时间范围" />
        </>
      }
    >
      {models.length === 0 ? (
        <CardContent className="px-4 py-8 text-center text-sm text-muted-foreground sm:px-5">
          {account && !account.ok ? "该账号官方用量暂不可用。" : "官方暂无可用的模型消耗明细。"}
        </CardContent>
      ) : (
        <CardContent className="min-w-0 px-4 pt-4 pb-4 sm:px-5">
          <div className="mb-4 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
            <span>共 {formatCredits(totalRequests)} 次请求</span>
            <span className="font-medium text-foreground">合计 {formatCredits(totalCredit)} 积分</span>
          </div>
          <ShareRows rows={rows} />
        </CardContent>
      )}
    </Section>
  );
}

// -----------------------------------------------------------------------------
// 积分明细（资源包 / 请求用量）
// -----------------------------------------------------------------------------

function CreditDetail({
  official,
  credits,
  creditsError,
  accounts,
}: {
  official: OfficialUsageResponse | null;
  credits: CreditsResponse | null;
  creditsError: string | null;
  accounts: { id: string; label: string }[];
}) {
  const [tab, setTab] = useState<"credits" | "requests">("credits");
  const [accountFilter, setAccountFilter] = useState<string | null>(null);

  // 默认落在第一个账号（对齐 switch 的单账号视图）；账号集合变化或旧值失效时回退第一个。
  const effectiveFilter = accounts.some((a) => a.id === accountFilter)
    ? accountFilter
    : (accounts[0]?.id ?? null);

  const active = accounts.find((a) => a.id === effectiveFilter);
  const activeAccount = active
    ? official?.accounts.find((a) => a.id === active.id)
    : undefined;
  const visibleCreditAccounts = active
    ? (credits?.accounts ?? []).filter((a) => a.id === active.id)
    : (credits?.accounts ?? []);

  const requestRows = useMemo(() => {
    const rows = official?.requests ?? [];
    return active ? rows.filter((row) => row.account_id === active.id) : rows;
  }, [official, active]);

  // 切换账号时回到「积分明细」页签（对齐 switch）：停在空的「请求用量」上会被读成数据丢了。
  useEffect(() => {
    setTab("credits");
  }, [effectiveFilter]);

  // 明细排序与分页（对照 wb-switch 的 CreditStatsPage）。
  // 后端一次性返回全部明细，这里只是前端视图：排序改的是渲染顺序，分页只控制渲染量。
  const [requestSort, setRequestSort] = useState<{ key: RequestSortKey; direction: SortDirection }>({
    key: "time",
    direction: "desc",
  });
  const [requestPage, setRequestPage] = useState(0);

  // 换账号 / 换排序 / 重新采集后都回到第一页。
  // 不重置的话，在新数据只有两页时停在第三页会看到一片空白 —— 用户会以为数据丢了。
  useEffect(() => {
    setRequestPage(0);
  }, [effectiveFilter, requestSort, official?.collected_at]);

  const sortedRequests = useMemo(
    () => [...requestRows].sort((a, b) => compareRequests(a, b, requestSort.key, requestSort.direction)),
    [requestRows, requestSort],
  );
  const requestPageCount = Math.max(1, Math.ceil(sortedRequests.length / REQUEST_PAGE_SIZE));
  // 钳一次：数据变少时（换账号）可能停在越界页
  const safeRequestPage = Math.min(Math.max(requestPage, 0), requestPageCount - 1);
  const requestPageStart = safeRequestPage * REQUEST_PAGE_SIZE;
  const requestPageRows = sortedRequests.slice(requestPageStart, requestPageStart + REQUEST_PAGE_SIZE);

  const toggleRequestSort = useCallback((key: RequestSortKey) => {
    setRequestSort((prev) =>
      // 同一列再点一次就翻转方向；换列时默认降序（时间/消耗都是「大的更有用」）
      prev.key === key ? { key, direction: prev.direction === "desc" ? "asc" : "desc" } : { key, direction: "desc" },
    );
  }, []);

  const truncated = official
    ? active
      ? Boolean(official.accounts.find((a) => a.id === active.id)?.detail_truncated)
      : official.accounts.some((a) => a.detail_truncated)
    : false;
  const reportedTotal = official
    ? active
      ? (official.accounts.find((a) => a.id === active.id)?.reported_total ?? 0)
      : official.accounts.reduce((sum, a) => sum + (a.reported_total ?? 0), 0)
    : 0;

  return (
    <Section
      id="credit-detail"
      title="积分明细"
      description={
        credits ? `最近采集 ${formatDateTime(credits.collected_at)}` : "暂无账号资源包。"
      }
      action={
        accounts.length > 0 ? (
          <AccountFilter
            accounts={accounts}
            value={effectiveFilter}
            onChange={setAccountFilter}
            ariaLabel="按账号筛选积分明细"
            allowAll={false}
          />
        ) : null
      }
      cardClassName="rounded-xl"
    >
      <CardHeader className="gap-0 border-b px-4 pt-3 pb-3 sm:px-5">
        <div className="flex max-w-full gap-1 rounded-lg bg-muted p-1" role="tablist" aria-label="积分详情类型">
          {(
            [
              ["credits", "积分明细"],
              ["requests", "请求用量"],
            ] as const
          ).map(([value, label]) => (
            <button
              key={value}
              type="button"
              role="tab"
              aria-selected={tab === value}
              className={`min-w-0 flex-1 cursor-pointer rounded-md px-2.5 py-1.5 text-xs transition-colors ${
                tab === value
                  ? "bg-background font-medium text-foreground shadow-sm"
                  : "text-muted-foreground hover:text-foreground"
              }`}
              onClick={() => setTab(value)}
            >
              {label}
            </button>
          ))}
        </div>
      </CardHeader>

      {tab === "credits" ? (
        creditsError ? (
          <CardContent className="px-4 py-8 text-center text-sm text-muted-foreground sm:px-5">
            积分资源包读取失败：{creditsError}
          </CardContent>
        ) : visibleCreditAccounts.length === 0 ? (
          <CardContent className="px-4 py-8 text-center text-sm text-muted-foreground sm:px-5">
            尚未采集当前资源包。
          </CardContent>
        ) : (
          <div className="min-w-0 divide-y divide-border/60">
            {visibleCreditAccounts.map((account) => (
              <div key={account.id} className="min-w-0">
                {visibleCreditAccounts.length > 1 ? (
                  <div className="px-4 py-2.5 text-xs font-medium sm:px-5">{accountLabel(account)}</div>
                ) : null}
                <ResourceBreakdown account={account} />
              </div>
            ))}
          </div>
        )
      ) : (
        <div className="min-w-0">
          {truncated ? (
            <div className="flex items-start gap-2 border-b bg-amber-500/[0.06] px-4 py-2.5 text-xs text-amber-800 dark:text-amber-300 sm:px-5">
              <CircleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
              <span>
                最多展示每账号最近 {official?.detail_limit_per_account ?? 0} 条请求明细；合计使用官方返回的全部{" "}
                {formatCount(reportedTotal)} 条请求。
              </span>
            </div>
          ) : null}
          {requestRows.length === 0 ? (
            <CardContent className="px-4 py-8 text-center text-sm text-muted-foreground sm:px-5">
              {!official || official.status === "unavailable" ? (
                "官方请求用量暂不可用；总览已回退到本地观察数据，官方接口恢复后刷新即可重新同步。"
              ) : activeAccount && !activeAccount.ok ? (
                <span className="text-destructive">
                  {activeAccount.error || "该账号官方用量读取失败。"}
                </span>
              ) : reportedTotal > 0 ? (
                "官方返回了请求总数，但明细未通过格式校验。"
              ) : (
                "该账号暂无官方用量记录。"
              )}
            </CardContent>
          ) : (
            <div className="min-w-0 overflow-x-auto">
              <table className="w-full min-w-[700px] text-left text-xs">
                <thead className="sticky top-0 z-10 bg-muted/95 text-muted-foreground backdrop-blur">
                  <tr>
                    <SortableHeader
                      label="请求时间"
                      sortKey="time"
                      activeKey={requestSort.key}
                      direction={requestSort.direction}
                      onToggle={toggleRequestSort}
                    />
                    {active ? null : <th className="px-3 py-2.5 font-medium">账号</th>}
                    <SortableHeader
                      label="消耗"
                      sortKey="credit"
                      activeKey={requestSort.key}
                      direction={requestSort.direction}
                      align="right"
                      onToggle={toggleRequestSort}
                    />
                    <th className="px-3 py-2.5 font-medium">模型</th>
                    <th className="px-3 py-2.5 font-medium">客户端</th>
                    <th className="px-3 py-2.5 font-medium">请求 ID</th>
                  </tr>
                </thead>
                <tbody>
                  {requestPageRows.map((row) => (
                    <tr key={`${row.request_id}-${row.request_time}`} className="border-t border-border/60 align-top">
                      <td className="whitespace-nowrap px-3 py-3 text-muted-foreground">{row.request_time}</td>
                      {active ? null : (
                        <td className="max-w-[140px] truncate px-3 py-3" title={row.account_name}>
                          {row.account_name}
                        </td>
                      )}
                      <td className="whitespace-nowrap px-3 py-3 text-right font-medium text-primary tabular-nums">
                        {formatCredits(row.credit)}
                      </td>
                      <td className="max-w-[180px] truncate px-3 py-3" title={row.model}>
                        {row.model}
                      </td>
                      <td className="max-w-[120px] truncate px-3 py-3 text-muted-foreground" title={row.client}>
                        {row.client}
                      </td>
                      <td
                        className="max-w-[170px] truncate px-3 py-3 font-mono text-xs text-muted-foreground"
                        title={row.request_id}
                      >
                        {row.request_id}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {sortedRequests.length > REQUEST_PAGE_SIZE ? (
            <div className="flex flex-wrap items-center justify-between gap-2 border-t px-4 py-2.5 text-xs text-muted-foreground sm:px-5">
              <span className="tabular-nums">
                第 {requestPageStart + 1}–{requestPageStart + requestPageRows.length} 条，共{" "}
                {sortedRequests.length} 条
              </span>
              <div className="flex items-center gap-2">
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-7 px-2"
                  disabled={safeRequestPage <= 0}
                  onClick={() => setRequestPage(safeRequestPage - 1)}
                >
                  <ChevronLeft className="size-3.5" aria-hidden="true" />
                  上一页
                </Button>
                <span className="tabular-nums">
                  {safeRequestPage + 1} / {requestPageCount}
                </span>
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-7 px-2"
                  disabled={safeRequestPage >= requestPageCount - 1}
                  onClick={() => setRequestPage(safeRequestPage + 1)}
                >
                  下一页
                  <ChevronRight className="size-3.5" aria-hidden="true" />
                </Button>
              </div>
            </div>
          ) : null}
        </div>
      )}
    </Section>
  );
}

/** 单账号的资源包列表：每行一条进度条（剩余 / 总量）。 */
function ResourceBreakdown({ account }: { account: CreditsResponse["accounts"][number] }) {
  if (!account.ok) {
    return (
      <div className="flex items-start gap-2 px-4 py-6 text-sm text-destructive sm:px-5">
        <CircleAlert className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
        <span>{account.error || "积分资源查询失败"}</span>
      </div>
    );
  }
  const resources = account.resources ?? [];
  if (resources.length === 0) {
    return (
      <div className="px-4 py-6 text-center text-sm text-muted-foreground sm:px-5">当前没有可展示的资源包。</div>
    );
  }
  return (
    <div className="divide-y divide-border/60">
      {resources.map((resource: CreditResource, index) => {
        const ratio =
          resource.total > 0 ? Math.min(100, Math.max(0, (resource.remaining / resource.total) * 100)) : 0;
        return (
          <div
            key={`${resource.package_code || resource.package_name || "resource"}-${index}`}
            className="min-w-0 px-4 py-2 sm:px-5"
          >
            <div className="flex min-w-0 items-center justify-between gap-2">
              <div className="min-w-0 truncate text-[13px] font-medium">
                {creditResourceName(resource, "未命名资源包")}
              </div>
              <div className="flex shrink-0 items-center gap-2.5">
                <span className="text-xs text-muted-foreground">
                  {resource.expired ? "已到期" : `到期 ${formatExpiry(resource.expire_at)}`}
                  {resource.used > 0 ? ` · 已用 ${formatCredits(resource.used)}` : ""}
                </span>
                <span className="text-xs font-medium tabular-nums">
                  {formatCredits(resource.remaining)} / {formatCredits(resource.total)}
                </span>
              </div>
            </div>
            <div className="mt-1 h-1 overflow-hidden rounded-full bg-muted" aria-hidden="true">
              <div className="h-full rounded-full bg-primary/75" style={{ width: `${ratio}%` }} />
            </div>
          </div>
        );
      })}
    </div>
  );
}

function LoadingSkeleton() {
  return (
    <div className="min-w-0 space-y-12">
      <p className="flex items-center gap-2 text-sm text-muted-foreground">
        <Loader2 className="size-4 animate-spin" aria-hidden="true" />
        正在采集账号积分并加载统计…
      </p>
      <div className="min-w-0 space-y-2.5">
        <Skeleton className="ml-1 h-5 w-24" />
        <Skeleton className="h-[132px] rounded-xl" />
      </div>
      <div className="min-w-0 space-y-2.5">
        <Skeleton className="ml-1 h-5 w-24" />
        <Skeleton className="h-72 rounded-xl" />
      </div>
    </div>
  );
}
