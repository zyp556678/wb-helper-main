/**
 * Token 统计（版式与交互对齐 wb-switch 的 TokenStatsPage，基准 v0.1.58）。
 *
 * 这一页回答两类问题：
 *   1. **请求量是谁产生的**（顶部「请求来源拆分」，本项目扩展，switch 没有）：
 *      官方账本是全部流量，减去网关反代计数即可得到「不经网关的直连」。
 *   2. **token 花在哪了**（其余区块，按数据源切换）：
 *      - 本地会话日志：客户端自己写的会话记录，覆盖**所有**客户端流量，
 *        有缓存读写、思考 token、项目与会话维度 —— 这是唯一能给出这些细分的口径。
 *      - 网关反代：网关自己的请求级计数，只有请求数与输入/输出 token。
 *
 * ## 与 switch 的对齐点（改动时不要再退回）
 *
 * - **总览与趋势各有独立范围**：总览 今日/近 7 天/近 30 天/总计（默认今日），
 *   趋势 近 30 天/今天/近 7 天/本月（默认近 30 天）。此前全页共用一套范围，
 *   打开就是「今天」，趋势只有一根柱子。
 * - **趋势按本地日历日补零**：缺数据的日子补 0，X 轴连续（否则柱间距不再代表时间间隔）。
 * - **堆叠段 5px 保底 + 零值不画**（`stacked-bar-visuals.ts`），仅真实顶部段加圆角。
 * - **图例 + 富 tooltip**：tooltip 有 Token 总量行与每行百分比。
 * - 数据源选择持久化到 localStorage。
 *
 * 两处**刻意偏离**（本仓库的既有约定，注释随代码走）：
 * - 字号最小 12px（switch 有 10/11px 的说明行，中文在 10px 会糊）；
 * - 模型筛选没有下拉原语，保留药丸组（switch 是 DropdownMenu，本仓库未引入 Radix dropdown）。
 *
 * 两个数据源的**能力边界不同**，因此区块会按 kind 降级（例如网关侧没有缓存命中率，
 * 就把那一格换成调用次数并注明原因）。把缺失字段显示成 0 会被读成「测到就是 0」，
 * 比不显示更糟。
 */
import { ComponentProps, useCallback, useEffect, useMemo, useState } from "react";
import {
  Bar,
  CartesianGrid,
  ComposedChart,
  Line,
  Rectangle,
  ResponsiveContainer,
  Tooltip as ChartTooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  Activity,
  ArrowDownToLine,
  ArrowUpFromLine,
  CircleAlert,
  Gauge,
  GitFork,
  Layers,
  ListTree,
  Loader2,
  MessagesSquare,
  RefreshCw,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyHint, MetricRow, PillGroup, Section, StatMetric } from "@/components/section";
import { RankingRows } from "@/components/ranking-rows";
import { TokenComposition } from "@/components/token-composition";
import { TokenHeatmap } from "@/components/token-heatmap";
import { TokenRequestDialog } from "@/components/token-request-dialog";
import { describeError, fetchOfficialUsage, fetchStatsDaily, fetchTokenStats } from "@/lib/api";
import {
  formatCredits,
  formatQuantity,
  formatTokenCompact,
  formatTokenExact,
  tokenPercentage,
} from "@/lib/format";
import { getStackedSegmentVisualLayout } from "@/lib/stacked-bar-visuals";
import { splitUsage } from "@/lib/usage-split";
import type {
  DailySnapshot,
  OfficialUsageResponse,
  TokenGroup,
  TokenSource,
  TokenStatistics,
  TokenTotals,
} from "@/lib/types";

/**
 * 趋势范围。默认「近 30 天」—— 趋势图回答的是「走势」，30 天是能一眼读出
 * 周节律（工作日/周末的落差）的最短窗口；「今天」放第一档是为了对齐总览的默认值，
 * 但它对趋势来说信息量最小，所以不做默认。
 */
const TREND_RANGES = [
  { key: "30d", label: "近 30 天" },
  { key: "today", label: "今天" },
  { key: "7d", label: "近 7 天" },
  { key: "month", label: "本月" },
] as const;
type TrendRangeKey = (typeof TREND_RANGES)[number]["key"];

/**
 * 分组行的显示名。
 *
 * 网关来源的「账号」分组里 `key` 是**凭据文件名**（workbuddy-xxxx.json），
 * 直接显示等于让用户对着一串 uuid 找账号。后端已经在 `title` 里给出显示名
 *（昵称，或按 display_field 选的备注，与账号页同一口径），这里优先用它。
 */
function groupLabel(g: TokenGroup): string {
  return g.title?.trim() || g.key;
}

/**
 * 分组行的副标题：显示名与 key 不同（也就是 key 确实是个文件名/ID）时，
 * 把 key 作为副标题保留 —— 昵称可能重名，出问题时仍要能对上是哪个文件。
 */
function groupDetail(g: TokenGroup): string | undefined {
  const title = g.title?.trim();
  return title && title !== g.key ? g.key : undefined;
}

/** 总览范围。默认「今日」：打开这页最常见的诉求是「我现在用了多少」。 */
const OVERVIEW_RANGES = [
  { key: "today", label: "今日" },
  { key: "7d", label: "近 7 天" },
  { key: "30d", label: "近 30 天" },
  { key: "total", label: "总计" },
] as const;
type OverviewRangeKey = (typeof OVERVIEW_RANGES)[number]["key"];

/**
 * 「请求来源拆分」区块的范围（本项目扩展，独立于上面两套范围）。
 * 它要额外打官方接口，窗口太大响应慢，默认 30 天。
 */
const SPLIT_RANGES = [
  { value: "1", label: "今天", daily: "today" },
  { value: "7", label: "近 7 天", daily: "7d" },
  { value: "30", label: "近 30 天", daily: "30d" },
  { value: "90", label: "近 90 天", daily: "90d" },
] as const;

interface TrendSeries {
  key: string;
  label: string;
  color: string;
}

/** 本地日志侧的四段（顺序即堆叠顺序）。 */
const LOCAL_TREND_SERIES: TrendSeries[] = [
  { key: "cacheRead", label: "缓存读取", color: "var(--data-series-emerald)" },
  { key: "uncachedInput", label: "新增输入", color: "var(--data-series-teal)" },
  { key: "output", label: "输出", color: "var(--data-series-violet)" },
  { key: "cacheWrite", label: "缓存写入", color: "var(--data-series-amber)" },
];

/** 网关侧没有缓存读写，趋势只堆输入与输出。 */
const GATEWAY_TREND_SERIES: TrendSeries[] = [
  { key: "input", label: "输入", color: "var(--data-series-teal)" },
  { key: "output", label: "输出", color: "var(--data-series-violet)" },
];

const CALLS_COLOR = "var(--data-series-indigo)";

/** 数据源偏好（本项目扩展：记住选择，刷新后不丢）。 */
const SOURCE_STORAGE_KEY = "workbuddy-gateway.token-stats.source";

function readPreferredSource(): string | null {
  try {
    return window.localStorage.getItem(SOURCE_STORAGE_KEY);
  } catch {
    return null;
  }
}

function persistPreferredSource(source: string): void {
  try {
    window.localStorage.setItem(SOURCE_STORAGE_KEY, source);
  } catch {
    // localStorage 在受限 WebView/隐私模式下可能不可写，不影响页面切换。
  }
}

// -----------------------------------------------------------------------------
// 日期与范围
// -----------------------------------------------------------------------------

const pad2 = (value: number) => String(value).padStart(2, "0");

function dateKey(date: Date): string {
  return `${date.getFullYear()}-${pad2(date.getMonth() + 1)}-${pad2(date.getDate())}`;
}

function daysAgoKey(days: number): string {
  const date = new Date();
  date.setHours(12, 0, 0, 0);
  date.setDate(date.getDate() - days);
  return dateKey(date);
}

/** 图表 X 轴与 tooltip 标题的日期：`MM/DD`。 */
function formatChartDate(date: string): string {
  return date.slice(5).replace("-", "/");
}

/** 页头与页脚的时间：本地时区 `MM/DD HH:mm`（与 switch 的 formatDateTime 一致）。 */
function formatDateTimeShort(ms?: number | null): string {
  if (!ms) return "—";
  return new Date(ms).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

interface RangeBounds {
  start: string;
  end: string;
}

function trendBounds(range: TrendRangeKey): RangeBounds {
  const today = dateKey(new Date());
  if (range === "today") return { start: today, end: today };
  if (range === "7d") return { start: daysAgoKey(6), end: today };
  if (range === "30d") return { start: daysAgoKey(29), end: today };
  const monthStart = new Date();
  monthStart.setHours(12, 0, 0, 0);
  monthStart.setDate(1);
  return { start: dateKey(monthStart), end: today };
}

function overviewBounds(range: Exclude<OverviewRangeKey, "total">): RangeBounds {
  const today = dateKey(new Date());
  if (range === "today") return { start: today, end: today };
  if (range === "7d") return { start: daysAgoKey(6), end: today };
  return { start: daysAgoKey(29), end: today };
}

function filterByBounds(groups: TokenGroup[], bounds: RangeBounds): TokenGroup[] {
  return groups
    .filter((group) => group.key >= bounds.start && group.key <= bounds.end)
    .sort((left, right) => left.key.localeCompare(right.key));
}

function emptyGroup(key: string): TokenGroup {
  return {
    key,
    total: 0,
    input: 0,
    output: 0,
    cacheRead: 0,
    cacheWrite: 0,
    uncachedInput: 0,
    records: 0,
    cacheHitRate: null,
  };
}

/**
 * 按范围边界补零：无流量的日子必须补 0 发出去，否则 X 轴跳过空白日，
 * 柱间距不再代表时间间隔（switch 的 fillRangePoints，逐行对齐）。
 * 整个范围都没有数据时返回空数组 —— 让图表走「暂无可展示数据」的空态，
 * 而不是画一整屏 0 柱。
 */
function fillRangePoints(points: TokenGroup[], bounds: RangeBounds): TokenGroup[] {
  if (points.length === 0) return [];
  const byDate = new Map(points.map((point) => [point.key, point]));
  const cursor = new Date(`${bounds.start}T12:00:00`);
  const endDate = new Date(`${bounds.end}T12:00:00`);
  const filled: TokenGroup[] = [];
  while (cursor <= endDate) {
    const key = dateKey(cursor);
    filled.push(byDate.get(key) ?? emptyGroup(key));
    cursor.setDate(cursor.getDate() + 1);
  }
  return filled;
}

function rangeTotals(points: TokenGroup[]): TokenTotals {
  const totals = points.reduce(
    (sum, point) => ({
      total: sum.total + point.total,
      input: sum.input + point.input,
      output: sum.output + point.output,
      cacheRead: sum.cacheRead + point.cacheRead,
      cacheWrite: sum.cacheWrite + point.cacheWrite,
      uncachedInput: sum.uncachedInput + point.uncachedInput,
      records: sum.records + point.records,
      cacheHitRate: null,
    }),
    { ...EMPTY_TOTALS } satisfies TokenTotals,
  );
  return {
    ...totals,
    cacheHitRate: totals.input > 0 ? totals.cacheRead / totals.input : null,
  };
}

/** 空的总量，用于数据源缺失时保持类型完整。 */
const EMPTY_TOTALS: TokenTotals = {
  total: 0,
  input: 0,
  output: 0,
  cacheRead: 0,
  cacheWrite: 0,
  uncachedInput: 0,
  records: 0,
  cacheHitRate: null,
};

/** 图表点的总量口径：input 已含 cacheRead，不再相加。 */
type TrendPoint = { date: string } & { [key: string]: number | string };

function pointTotal(point: TrendPoint): number {
  return (
    Number(point.input ?? 0) + Number(point.output ?? 0) + Number(point.cacheWrite ?? 0)
  );
}

// -----------------------------------------------------------------------------
// 图表部件（对齐 switch 的同名部件）
// -----------------------------------------------------------------------------

type TokenBarShapeProps = ComponentProps<typeof Rectangle> & {
  segmentKey?: string;
  seriesKeys?: string[];
  payload?: TrendPoint;
  /** recharts 3.x 下堆叠段的 value 是单值；2.x 是「下方累计值」的元组。 */
  value?: number | [number, number];
};

/**
 * 堆叠柱的单段形状。
 *
 *  1. **零值段不画**。recharts 的 `minPointSize` 对零值也生效，会凭空多出
 *     一小截柱子 —— 那是「这天有消耗」的错误陈述（本项目实测踩过），所以不用它。
 *  2. **非零段保底 5px**。占比极小的段按比例算不足 1px，肉眼看不见；
 *     保底由 `getStackedSegmentVisualLayout` 做，缺口从胖段按比例扣，总量守恒。
 */
function TokenBarShape(props: TokenBarShapeProps) {
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

function TrendLegend({ series }: { series: TrendSeries[] }) {
  return (
    <div
      className="flex min-w-0 flex-wrap items-center gap-x-4 gap-y-1.5 text-xs text-muted-foreground"
      aria-label="图表图例"
    >
      {series.map((item) => (
        <span key={item.key} className="inline-flex items-center gap-1.5 whitespace-nowrap">
          <span
            className="size-2.5 shrink-0 rounded-[3px]"
            style={{ backgroundColor: item.color }}
            aria-hidden="true"
          />
          {item.label}
        </span>
      ))}
      <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
        <span className="relative inline-flex h-2 w-4 shrink-0 items-center" aria-hidden="true">
          <span
            className="absolute inset-x-0 top-1/2 border-t-2 border-dashed"
            style={{ borderColor: CALLS_COLOR }}
          />
          <span
            className="relative z-10 mx-auto size-1.5 rounded-full border border-background"
            style={{ backgroundColor: CALLS_COLOR }}
          />
        </span>
        调用次数
      </span>
    </div>
  );
}

/** 富 tooltip：日期标题 + Token 总量行 + 每行 compact 值与百分比 + 调用次数。 */
function TrendTooltipContent({
  active,
  payload,
  series,
}: {
  active?: boolean;
  payload?: Array<{ payload?: TrendPoint }>;
  series: TrendSeries[];
}) {
  if (!active || !payload?.length) return null;
  const point = payload[0]?.payload;
  if (!point) return null;
  const total = pointTotal(point);

  return (
    <div className="grid min-w-[13rem] gap-2 rounded-lg border border-border/50 bg-background px-3 py-2.5 text-xs shadow-xl">
      <div className="font-medium text-foreground">{formatChartDate(point.date)}</div>
      <div className="flex items-center justify-between border-b border-border/60 pb-1.5">
        <span className="text-muted-foreground">Token 总量</span>
        <span
          className="whitespace-nowrap font-mono font-semibold tabular-nums text-foreground"
          title={`${formatTokenExact(total)} Token`}
          aria-label={`${formatTokenExact(total)} Token`}
        >
          {formatTokenCompact(total)} Token
        </span>
      </div>
      <div className="grid gap-1.5">
        {series.map((item) => {
          const value = Number(point[item.key] ?? 0);
          return (
            <div key={item.key} className="flex items-center gap-2">
              <span
                className="size-2.5 shrink-0 rounded-[3px]"
                style={{ backgroundColor: item.color }}
                aria-hidden="true"
              />
              <span className="flex-1 text-muted-foreground">{item.label}</span>
              <span
                className="whitespace-nowrap font-mono font-medium tabular-nums text-foreground"
                title={`${formatTokenExact(value)} Token`}
                aria-label={`${formatTokenExact(value)} Token`}
              >
                {formatTokenCompact(value)} Token
                <span className="ml-1 font-sans text-[11px] font-normal text-muted-foreground">
                  ({tokenPercentage(value, total)})
                </span>
              </span>
            </div>
          );
        })}
        <div className="flex items-center gap-2">
          <span
            className="h-0 w-3 shrink-0 border-t-2 border-dashed"
            style={{ borderColor: CALLS_COLOR }}
            aria-hidden="true"
          />
          <span className="flex-1 text-muted-foreground">调用次数</span>
          <span className="whitespace-nowrap font-mono font-medium tabular-nums text-foreground">
            {formatQuantity(Number(point.records ?? 0))} 次
          </span>
        </div>
      </div>
    </div>
  );
}

// -----------------------------------------------------------------------------
// 页面
// -----------------------------------------------------------------------------

export function StatsPage() {
  const [overviewRange, setOverviewRange] = useState<OverviewRangeKey>("today");
  const [trendRange, setTrendRange] = useState<TrendRangeKey>("30d");
  const [splitDays, setSplitDays] = useState(30);
  const [data, setData] = useState<TokenStatistics | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [sourceKey, setSourceKey] = useState("gateway");
  const [distDim, setDistDim] = useState<"primary" | "secondary">("primary");
  const [modelFilter, setModelFilter] = useState("all");
  const [detailOpen, setDetailOpen] = useState(false);

  const [daily, setDaily] = useState<DailySnapshot | null>(null);
  const [official, setOfficial] = useState<OfficialUsageResponse | null>(null);
  const [officialError, setOfficialError] = useState<string | null>(null);

  // 一次拉满一年：总览/趋势/热力图都在客户端按范围切片，切范围不再发请求
  // （与 switch 一致 —— 它的 getTokenStatistics 也不带窗口参数）。
  const load = useCallback(async () => {
    setLoading(true);
    try {
      const result = await fetchTokenStats(365);
      setData(result);
      setError(null);
      // 默认来源：上次记住的（若仍可用）> 第一个有数据的来源 > 列表第一个。
      setSourceKey((prev) => {
        const available = result.sources.map((s) => s.source);
        if (available.includes(prev)) return prev;
        const stored = readPreferredSource();
        if (stored && available.includes(stored)) return stored;
        const preferred = result.sources.find((s) => s.available && s.summary.records > 0);
        return preferred?.source ?? available[0] ?? prev;
      });
    } catch (err) {
      setError(describeError(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // 记住用户的选择（在状态落定后写，避免把「回退前的值」写进偏好）。
  useEffect(() => {
    persistPreferredSource(sourceKey);
  }, [sourceKey]);

  // 请求来源拆分用到的两套数据（官方账本 + 反代计数）单独取：
  // 官方那条要打上游，不能让整页等它。
  useEffect(() => {
    let alive = true;
    const splitRange =
      SPLIT_RANGES.find((r) => r.value === String(splitDays))?.daily ?? "30d";
    fetchStatsDaily(splitRange)
      .then((res) => {
        if (alive) setDaily(res);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [splitDays]);

  useEffect(() => {
    let alive = true;
    setOfficialError(null);
    fetchOfficialUsage(splitDays)
      .then((res) => {
        if (alive) setOfficial(res);
      })
      .catch((err) => {
        if (alive) {
          setOfficial(null);
          setOfficialError(describeError(err));
        }
      });
    return () => {
      alive = false;
    };
  }, [splitDays]);

  const sources = data?.sources ?? [];
  const active: TokenSource | undefined =
    sources.find((s) => s.source === sourceKey) ?? sources[0];
  const summary = active?.summary ?? EMPTY_TOTALS;
  const isGateway = active?.kind === "gateway";
  const splitRangeLabel =
    SPLIT_RANGES.find((r) => r.value === String(splitDays))?.label ?? "";
  const trendRangeLabel =
    TREND_RANGES.find((r) => r.key === trendRange)?.label ?? "近 30 天";

  /** 数据源扫到了文件但没有可用 usage → 走来源专属空态；否则照常渲染（各区块自处理空）。 */
  const hasAnyData = summary.records > 0 || (active?.daily?.length ?? 0) > 0;

  // 总览：独立范围。总计直接用 source.summary（后端窗口=一年）。
  const overviewSummary = useMemo(() => {
    if (!active) return EMPTY_TOTALS;
    if (overviewRange === "total") return active.summary;
    return rangeTotals(
      filterByBounds(active.daily ?? [], overviewBounds(overviewRange)),
    );
  }, [active, overviewRange]);

  // 趋势：按本地日历日补零后出图。
  const series = isGateway ? GATEWAY_TREND_SERIES : LOCAL_TREND_SERIES;
  const seriesKeys = useMemo(() => series.map((s) => s.key), [series]);
  const { chartData, trendTotals } = useMemo(() => {
    if (!active) return { chartData: [] as TrendPoint[], trendTotals: { ...EMPTY_TOTALS } };
    const groups =
      modelFilter === "all"
        ? (active.daily ?? [])
        : (active.dailyByModel?.[modelFilter] ?? []);
    const filled = fillRangePoints(filterByBounds(groups, trendBounds(trendRange)), trendBounds(trendRange));
    const points: TrendPoint[] = filled.map((group) => {
      const row: TrendPoint = { date: group.key, records: group.records };
      for (const key of seriesKeys) {
        row[key] = Number(group[key as keyof TokenGroup] ?? 0);
      }
      return row;
    });
    return { chartData: points, trendTotals: rangeTotals(filled) };
  }, [active, modelFilter, trendRange, seriesKeys]);

  const modelOptions = useMemo(
    () => (active?.models ?? []).map((m) => m.key).filter(Boolean),
    [active],
  );

  // 换来源后，之前选中的模型可能不存在了 —— 回落到「所有模型」。
  useEffect(() => {
    if (modelFilter !== "all" && !modelOptions.includes(modelFilter)) setModelFilter("all");
  }, [modelFilter, modelOptions]);

  const requestSplit = useMemo(
    () => splitUsage(daily?.totals.requests ?? 0, officialError ? null : official),
    [daily, official, officialError],
  );

  const splitPills = (
    <PillGroup
      value={String(splitDays)}
      options={SPLIT_RANGES.map((r) => ({ value: r.value, label: r.label }))}
      onChange={(v) => setSplitDays(Number(v))}
      ariaLabel="请求来源拆分范围"
    />
  );
  const overviewPills = (
    <PillGroup
      value={overviewRange}
      options={OVERVIEW_RANGES.map((r) => ({ value: r.key, label: r.label }))}
      onChange={(v) => setOverviewRange(v as OverviewRangeKey)}
      ariaLabel="总览范围"
    />
  );
  const trendPills = (
    <PillGroup
      value={trendRange}
      options={TREND_RANGES.map((r) => ({ value: r.key, label: r.label }))}
      onChange={(v) => setTrendRange(v as TrendRangeKey)}
      ariaLabel="趋势范围"
    />
  );

  const sourcePills = (
    <PillGroup
      value={sourceKey}
      options={sources.map((s) => ({
        value: s.source,
        label: s.label,
        disabled: !s.available && s.summary.records === 0,
      }))}
      onChange={(v) => setSourceKey(v)}
      ariaLabel="Token 数据来源"
    />
  );

  /** 分布维度：本地日志按项目/模型，网关按模型/账号（它不知道项目）。 */
  const distOptions = isGateway
    ? [
        { value: "primary" as const, label: "按模型" },
        { value: "secondary" as const, label: "按账号" },
      ]
    : [
        { value: "primary" as const, label: "按项目" },
        { value: "secondary" as const, label: "按模型" },
      ];
  const distGroups: TokenGroup[] = isGateway
    ? distDim === "primary"
      ? (active?.models ?? [])
      : (active?.sessions ?? [])
    : distDim === "primary"
      ? (active?.projects ?? [])
      : (active?.models ?? []);

  return (
    <div className="mx-auto w-full max-w-[1180px] min-w-0 space-y-12 px-4 py-6 sm:px-8 sm:py-9">
      <header className="mb-6 flex min-w-0 flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">Token 统计</h1>
          <p className="mt-2 max-w-2xl text-sm leading-6 text-muted-foreground">
            当前数据更新于 {formatDateTimeShort(data?.generatedAt)}
          </p>
        </div>
        <div className="flex max-w-full flex-wrap items-center justify-end gap-2">
          {hasAnyData && (active?.requests.length ?? 0) > 0 ? (
            <Button type="button" variant="outline" size="sm" onClick={() => setDetailOpen(true)}>
              <ListTree className="size-3.5" aria-hidden="true" />
              查看请求明细
            </Button>
          ) : null}
          <Button type="button" variant="outline" size="sm" disabled={loading} onClick={() => void load()}>
            {loading ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <RefreshCw className="size-3.5" aria-hidden="true" />
            )}
            刷新统计
          </Button>
        </div>
      </header>

      <Section
        id="usage-source-split"
        title="请求来源拆分"
        description={`${splitRangeLabel}内按「是否经过网关」拆分请求量。`}
        action={splitPills}
      >
        <CardContent className="min-w-0 space-y-4 px-4 pt-4 pb-4 sm:px-5">
          {requestSplit.state === "ready" ? (
            <>
              <RankingRows
                rows={[
                  {
                    key: "reverse",
                    title: "经过网关（反代）",
                    subtitle: "网关每次请求完成即累加，精确计数",
                    value: requestSplit.reverseRequests,
                    valueText: `${formatQuantity(requestSplit.reverseRequests)} 次`,
                  },
                  {
                    key: "direct",
                    title: "官方直连（不经网关）",
                    subtitle: "由官方账本减去反代计数推算",
                    value: requestSplit.directRequests ?? 0,
                    valueText: `${formatQuantity(requestSplit.directRequests ?? 0)} 次`,
                  },
                ]}
                total={requestSplit.officialRequests ?? 0}
              />
              <p className="text-xs leading-5 text-muted-foreground">
                合计 {formatQuantity(requestSplit.officialRequests ?? 0)} 次（WorkBuddy 官方账本）。
                「官方直连」是<span className="font-medium text-foreground">推算值</span>
                ：官方账本不区分流量是否经过网关，它只能由「官方合计 − 反代计数」得出。
                {requestSplit.officialCredit !== null
                  ? ` 官方账本同期消耗 ${formatCredits(requestSplit.officialCredit)} 积分，含两路。`
                  : ""}
              </p>
            </>
          ) : (
            <>
              <MetricRow cols={2}>
                <StatMetric
                  icon={Layers}
                  label="经过网关（反代）"
                  value={formatQuantity(requestSplit.reverseRequests)}
                  hint="精确计数"
                />
                <StatMetric icon={GitFork} label="官方直连" value="—" hint="无法推算" divided />
              </MetricRow>
              <p className="text-xs leading-5 text-muted-foreground">
                无法拆分来源：{officialError ?? requestSplit.reason ?? "官方账本不可用"}
                。下方 Token 明细不受影响。
              </p>
            </>
          )}
        </CardContent>
      </Section>

      {sources.length > 0 ? (
        <div className="min-w-0 space-y-2.5">
          <div className="flex min-w-0 flex-wrap items-center justify-between gap-3 px-1">
            <h2 className="text-[13px] font-medium leading-5">Token 数据来源</h2>
            {sourcePills}
          </div>
          {active?.note ? (
            <p className="px-1 text-xs leading-5 text-muted-foreground">{active.note}</p>
          ) : null}
        </div>
      ) : null}

      {error ? (
        <Card className="gap-0 rounded-xl border-destructive/40 py-0 shadow-none">
          <CardContent className="flex flex-wrap items-center justify-between gap-3 p-4">
            <div className="min-w-0">
              <div className="text-[13px] font-medium text-destructive">统计加载失败</div>
              <p className="mt-1 break-words text-xs text-muted-foreground">{error}</p>
            </div>
            <Button type="button" size="sm" variant="outline" onClick={() => void load()}>
              重试
            </Button>
          </CardContent>
        </Card>
      ) : !data && loading ? (
        <LoadingSkeleton />
      ) : !active ? (
        <EmptyHint>该来源暂无可用统计数据，请点击刷新重试。</EmptyHint>
      ) : !hasAnyData ? (
        <EmptyState source={active} />
      ) : (
        <>
          <Section
            id="token-overview"
            title="Token 总览"
            description={<TokenComposition summary={overviewSummary} />}
            action={overviewPills}
          >
            <MetricRow>
              <StatMetric
                icon={MessagesSquare}
                label="总 Token"
                value={formatTokenCompact(overviewSummary.total)}
              />
              <StatMetric
                icon={ArrowUpFromLine}
                label="输入 Token"
                value={formatTokenCompact(overviewSummary.input)}
                divided
              />
              <StatMetric
                icon={ArrowDownToLine}
                label="输出 Token"
                value={formatTokenCompact(overviewSummary.output)}
                divided
              />
              {isGateway ? (
                <StatMetric
                  icon={Activity}
                  label="调用次数"
                  value={formatQuantity(overviewSummary.records)}
                  divided
                />
              ) : (
                <StatMetric
                  icon={Gauge}
                  label="缓存命中率"
                  value={
                    overviewSummary.cacheHitRate === null
                      ? "—"
                      : `${(overviewSummary.cacheHitRate * 100).toFixed(1)}%`
                  }
                  divided
                />
              )}
            </MetricRow>
          </Section>

          <Section
            id="token-trend"
            title="Token 与调用趋势"
            description={
              isGateway
                ? "彩色堆叠柱表示每日输入/输出 Token，虚线表示调用次数。"
                : "彩色堆叠柱表示每日总 Token 及构成，虚线表示调用次数。"
            }
            action={
              <div className="flex max-w-full flex-wrap items-center gap-2">
                {!isGateway && modelOptions.length > 1 ? (
                  <PillGroup
                    value={modelFilter}
                    options={[{ value: "all", label: "所有模型" }].concat(
                      modelOptions.map((m) => ({ value: m, label: m })),
                    )}
                    onChange={setModelFilter}
                    ariaLabel="按模型筛选"
                  />
                ) : null}
                {trendPills}
              </div>
            }
          >
            <CardContent className="min-w-0 px-4 pt-3 pb-4 sm:px-5">
              {chartData.length === 0 ? (
                <EmptyHint>当前范围暂无可展示的 Token 数据。</EmptyHint>
              ) : (
                <>
                  <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
                    <TrendLegend series={series} />
                  </div>
                  <div className="mb-1 flex items-center justify-end px-1 text-xs font-medium text-muted-foreground">
                    <span className="font-normal">左轴：Token · 右轴：调用次数</span>
                  </div>
                  <ResponsiveContainer width="100%" height={288}>
                    <ComposedChart
                      data={chartData}
                      margin={{ top: 8, right: 8, left: 0, bottom: 0 }}
                      barCategoryGap="18%"
                    >
                      <CartesianGrid vertical={false} strokeDasharray="3 3" stroke="var(--border)" />
                      <XAxis
                        dataKey="date"
                        tick={{ fontSize: 12, fill: "var(--muted-foreground)" }}
                        stroke="var(--border)"
                        tickLine={false}
                        axisLine={false}
                        tickMargin={8}
                        minTickGap={24}
                        tickFormatter={(value) => formatChartDate(String(value))}
                      />
                      <YAxis
                        yAxisId="tokens"
                        width={48}
                        tick={{ fontSize: 12, fill: "var(--muted-foreground)" }}
                        stroke="var(--border)"
                        tickLine={false}
                        axisLine={false}
                        tickFormatter={(value: number) => formatTokenCompact(value)}
                      />
                      <YAxis
                        yAxisId="calls"
                        orientation="right"
                        width={46}
                        allowDecimals={false}
                        tick={{ fontSize: 12, fill: "var(--muted-foreground)" }}
                        stroke="var(--border)"
                        tickLine={false}
                        axisLine={false}
                        tickFormatter={(value: number) => formatTokenExact(value)}
                      />
                      <ChartTooltip
                        cursor={{ fill: "var(--muted)", opacity: 0.4 }}
                        content={<TrendTooltipContent series={series} />}
                      />
                      {series.map((s) => (
                        <Bar
                          key={s.key}
                          yAxisId="tokens"
                          dataKey={s.key}
                          stackId="token"
                          fill={s.color}
                          stroke="var(--background)"
                          strokeWidth={2}
                          maxBarSize={28}
                          shape={<TokenBarShape segmentKey={s.key} seriesKeys={seriesKeys} />}
                          isAnimationActive={false}
                        />
                      ))}
                      <Line
                        yAxisId="calls"
                        type="monotone"
                        dataKey="records"
                        stroke={CALLS_COLOR}
                        strokeDasharray="7 4"
                        strokeWidth={2}
                        strokeLinecap="round"
                        strokeLinejoin="round"
                        dot={false}
                        activeDot={{
                          r: 4,
                          fill: CALLS_COLOR,
                          stroke: "var(--background)",
                          strokeWidth: 2,
                        }}
                        isAnimationActive={false}
                      />
                    </ComposedChart>
                  </ResponsiveContainer>
                  <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
                    <span>
                      {trendRangeLabel}合计 {formatTokenCompact(trendTotals.total)} Token ·{" "}
                      {formatQuantity(trendTotals.records)} 次调用
                    </span>
                    <span>数据覆盖至 {formatDateTimeShort(active.coverageEndAt)}</span>
                  </div>
                  <p className="sr-only">
                    {chartData
                      .map(
                        (point) =>
                          `${point.date} 使用 ${formatTokenCompact(pointTotal(point))} Token，${formatQuantity(
                            Number(point.records ?? 0),
                          )} 次调用`,
                      )
                      .join("；")}
                  </p>
                </>
              )}
            </CardContent>
          </Section>

          <Section
            id="token-activity"
            title="Token 活动"
            description="最近一年按天显示 Token 活跃度。"
            action={<span className="text-xs font-medium text-foreground">每日</span>}
          >
            <CardContent className="min-w-0 px-4 pt-5 pb-5 sm:px-5">
              <TokenHeatmap daily={active.daily} />
            </CardContent>
          </Section>

          <Section
            id="token-distribution"
            title="用量分布"
            description={
              isGateway
                ? "按网关自己的计数量级汇总。"
                : "按本地会话日志汇总 Token 用量。"
            }
            action={
              <PillGroup
                value={distDim}
                options={distOptions}
                onChange={setDistDim}
                ariaLabel="用量分布维度"
              />
            }
          >
            <CardContent className="min-w-0 px-4 pt-4 pb-4 sm:px-5">
              {distGroups.length === 0 ? (
                <EmptyHint>暂无统计数据</EmptyHint>
              ) : (
                <RankingRows
                  rows={distGroups.slice(0, 10).map((g) => ({
                    key: g.key,
                    title: groupLabel(g),
                    subtitle: groupDetail(g),
                    value: g.total,
                    valueText: formatTokenCompact(g.total),
                    valueExact: `${formatTokenExact(g.total)} Token`,
                  }))}
                  total={summary.total}
                />
              )}
            </CardContent>
          </Section>

          <Section
            id="token-sessions"
            title={isGateway ? "消耗最高的账号" : "消耗最高的会话"}
            description={isGateway ? "按网关计数量级汇总。" : "按本地聚合 Token 从高到低排列。"}
          >
            <CardContent className="min-w-0 px-4 pt-4 pb-4 sm:px-5">
              {(active.sessions ?? []).length === 0 ? (
                <EmptyHint>暂无统计数据</EmptyHint>
              ) : (
                <RankingRows
                  rows={(active.sessions ?? []).slice(0, 10).map((s) => {
                    // 本地来源是会话（`title` 为会话名，缺失回退「未命名会话」）；
                    // 网关来源这一槽位装的是账号：`title` 是后端解析好的显示名
                    //（昵称，或按 display_field 选的备注），`key` 是凭据文件名。
                    const title = s.title?.trim() || (isGateway ? s.key : "未命名会话");
                    const detail = isGateway
                      ? groupDetail(s)
                      : [s.project, s.title ? undefined : s.sessionId]
                          .filter(Boolean)
                          .join(" · ");
                    return {
                      key: s.key,
                      title,
                      subtitle: detail || undefined,
                      value: s.total,
                      valueText: formatTokenCompact(s.total),
                      valueExact: `${formatTokenExact(s.total)} Token`,
                    };
                  })}
                  total={summary.total}
                />
              )}
            </CardContent>
          </Section>

          {active.parseErrors > 0 ? (
            <p className="flex items-center gap-1.5 px-1 text-xs text-amber-700 dark:text-amber-300">
              <CircleAlert className="size-3.5" aria-hidden="true" />
              已跳过 {formatQuantity(active.parseErrors)} 条无法解析的本地记录。
            </p>
          ) : null}

          <TokenRequestDialog
            open={detailOpen}
            onOpenChange={setDetailOpen}
            rows={active.requests ?? []}
            records={summary.records}
            label={active.label}
          />
        </>
      )}
    </div>
  );
}

/** 数据源扫到了文件但没有可用 usage —— 与「本机没有这个产品」是两回事。 */
function EmptyState({ source }: { source: TokenSource }) {
  return (
    <div className="rounded-xl border border-dashed px-4 py-16 text-center text-sm text-muted-foreground">
      {source.filesScanned > 0
        ? `已扫描 ${formatQuantity(source.filesScanned)} 个会话文件，但没有可用的 usage。`
        : "尚未发现该来源的本地会话日志。"}
      {source.note ? <p className="mt-2 text-xs leading-5">{source.note}</p> : null}
      {source.parseErrors > 0 ? (
        <p className="mt-2 text-xs text-amber-700 dark:text-amber-300">
          已跳过 {formatQuantity(source.parseErrors)} 条无法解析的本地记录。
        </p>
      ) : null}
    </div>
  );
}

function LoadingSkeleton() {
  return (
    <div className="min-w-0 space-y-12" role="status" aria-label="正在扫描本地会话日志…">
      <p className="flex items-center gap-2 text-sm text-muted-foreground">
        <span className="size-1.5 rounded-full bg-primary/70" aria-hidden="true" />
        正在扫描本地会话日志…
      </p>
      {[0, 1, 2].map((i) => (
        <div key={i} className="min-w-0 space-y-2.5">
          <Skeleton className="h-5 w-32" />
          <Card className="gap-0 rounded-xl py-0 shadow-none">
            <CardContent className="space-y-3 p-4 sm:p-5">
              <Skeleton className="h-24 w-full rounded-lg" />
            </CardContent>
          </Card>
        </div>
      ))}
    </div>
  );
}
