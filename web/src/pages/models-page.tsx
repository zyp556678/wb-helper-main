/**
 * 模型与档位。
 *
 * 版式参考 sudocode.chat/pricing 的模型广场：**居中 Hero + 搜索框 → 左侧筛选栏 →
 * 右侧卡片网格**，内容换成这个项目真正有的东西 ——
 *
 *   - 上游模型目录（30 个模型，含中文描述、能力、上游运营徽章）
 *   - **双站计费倍率**（国内站 / 国际站各一份，含促销与实测探测结论）
 *   - **推理档位**（上游多数时候只下发默认档）
 *   - 黑白名单开关与价格探测（会真实消耗额度）
 *
 * ## 与参考页的三处有意偏离
 *
 *   1. **不做分页**：参考页有分页是因为它按全量市场分页；我们的目录是 30 个模型，
 *      分页只会把一屏能看完的东西切成两屏。改为在工具栏显示「已显示 / 总数」。
 *   2. **卡片用文字标签而不是价格**：参考页卖的是单一人民币单价，我们有的是
 *      倍率系数 + 免费/收费的**实测结论**，两者的可信度与含义都不同，不能照搬数字样式。
 *   3. **保留说明区块**：参考页靠页脚链接承载口径说明，面板没有页脚，
 *      口径必须留在页内（倍率含义、探测会花钱、黑白名单规则）。
 */
import { useCallback, useMemo, useState } from "react";
import {
  FlaskConical,
  Inbox,
  LayoutGrid,
  List,
  Loader2,
  RefreshCw,
  Search,
  TriangleAlert,
} from "lucide-react";
import {
  notifySuccess,
  notifyError,
  notifyWarning,
  notifyMessage,
} from "@/lib/notify";

import { AccessInfoCard } from "@/components/access-info-card";
import { ModelCard } from "@/components/model-card";
import { ModelFilterRail } from "@/components/model-filter-rail";
import { ModelSortToggle } from "@/components/model-sort-toggle";
import { ModelsSummary } from "@/components/models-summary";
import { ModelsTable } from "@/components/models-table";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Section } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { describeError, probeModels, refreshModels, saveConfig } from "@/lib/api";
import { formatRelativeTime, formatUnixSeconds } from "@/lib/format";
import {
  EMPTY_FILTERS,
  applyFilters,
  pendingProbeSites,
  probeableSites,
  sortModelViews,
  tierCounts,
  toModelView,
  type ModelFilters,
  type ModelSort,
  type ModelView,
} from "@/lib/model-view";
import type { PanelModel } from "@/lib/types";
import { useModels } from "@/lib/use-models";
import { useNowSeconds } from "@/lib/use-now-seconds";
import { cn } from "@/lib/utils";

/** 「探测全部未知价格」一次最多提交的模型数。 */
const PROBE_BATCH_LIMIT = 10;

type ViewMode = "card" | "table";

/** 探测结果摘要：免费 / 收费 / 仍未知 各多少个站点。 */
function summarizeProbe(results: { verdict: string }[]): string {
  const free = results.filter((result) => result.verdict === "free").length;
  const paid = results.filter((result) => result.verdict === "paid").length;
  return `免费 ${free} 个站点 · 收费 ${paid} 个站点 · 仍未知 ${results.length - free - paid} 个站点`;
}

export function ModelsPage() {
  const {
    catalog,
    filter,
    loading,
    error,
    unauthorized,
    reload,
    refreshSilently,
    applyFilter,
    authCheckEnabled,
    configuredApiKey,
  } = useModels();
  const now = useNowSeconds();

  const [sort, setSort] = useState<ModelSort>("unknown-first");
  const [query, setQuery] = useState("");
  const [viewMode, setViewMode] = useState<ViewMode>("card");
  const [filters, setFilters] = useState<ModelFilters>(EMPTY_FILTERS);
  const [refreshing, setRefreshing] = useState(false);
  const [probingAll, setProbingAll] = useState(false);
  const [probingId, setProbingId] = useState<string | null>(null);
  const [blockingId, setBlockingId] = useState<string | null>(null);

  const all = catalog?.models;
  const allowlist = useMemo(() => filter?.allowlist ?? [], [filter]);
  const blockedIds = useMemo(() => new Set(filter?.blocklist ?? []), [filter]);

  /** 全量视图：所有派生口径（档位、徽章、已确认站点）都在这里算一次。 */
  const views = useMemo(
    () => (all ?? []).map((model) => toModelView(model, blockedIds)),
    [all, blockedIds],
  );
  const counts = useMemo(() => tierCounts(views), [views]);

  const keyword = query.trim().toLowerCase();
  const visible = useMemo(
    () => sortModelViews(applyFilters(views, filters, keyword), sort),
    [views, filters, keyword, sort],
  );
  const visibleModels: PanelModel[] = useMemo(() => visible.map((view) => view.model), [visible]);

  const blockedCount = useMemo(() => views.filter((view) => view.blocked).length, [views]);
  const filtered = visible.length !== views.length;

  /** 可探测站点：目录里报了有账号的站点。没账号的站点探不了，不该出现在待探测里。 */
  const probeable = useMemo(() => probeableSites(catalog), [catalog]);
  const pendingSitesOf = useCallback(
    (model: PanelModel) => pendingProbeSites(model, probeable),
    [probeable],
  );
  const unknownModelCount = useMemo(
    () => views.filter((view) => pendingSitesOf(view.model).length > 0).length,
    [views, pendingSitesOf],
  );

  /** 批量探测目标：待探测的模型取前 10 个，每个模型提交其所有待探测站点。 */
  const batchTargets = useMemo(
    () =>
      sortModelViews(
        views.filter((view) => pendingSitesOf(view.model).length > 0),
        "unknown-first",
      )
        .slice(0, PROBE_BATCH_LIMIT)
        .flatMap((view) =>
          pendingSitesOf(view.model).map((site) => ({ site, model: view.model.id })),
        ),
    [views, pendingSitesOf],
  );

  const firstLoad = loading && catalog === null;
  const source = catalog?.source ?? "";
  const fetchedAt = catalog?.fetched_at ?? 0;
  const fetchedAbsolute = formatUnixSeconds(fetchedAt);
  const siteSummary = (catalog?.sites ?? [])
    .map((site) => `${site.label || site.site} ${site.model_count}`)
    .join(" · ");

  const handleRefreshCatalog = useCallback(async () => {
    setRefreshing(true);
    try {
      const result = await refreshModels();
      notifySuccess(`模型目录已刷新：${result.model_count} 个模型`, {
        description: result.source || undefined,
      });
      refreshSilently();
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setRefreshing(false);
    }
  }, [refreshSilently]);

  const handleProbeAll = useCallback(async () => {
    if (batchTargets.length === 0) {
      notifyMessage("没有价格未知的模型需要探测");
      return;
    }
    setProbingAll(true);
    try {
      const result = await probeModels({ models: batchTargets });
      notifySuccess(`已探测 ${result.results.length} 个站点`, {
        description: summarizeProbe(result.results),
      });
      refreshSilently();
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setProbingAll(false);
    }
  }, [batchTargets, refreshSilently]);

  const handleProbeModel = useCallback(
    async (model: PanelModel) => {
      const targets = pendingSitesOf(model).map((site) => ({ site, model: model.id }));
      if (targets.length === 0) return;

      setProbingId(model.id);
      try {
        const result = await probeModels({ models: targets });
        notifySuccess(`${model.id} 探测完成`, { description: summarizeProbe(result.results) });
        refreshSilently();
      } catch (err) {
        notifyError(describeError(err));
      } finally {
        setProbingId(null);
      }
    },
    [pendingSitesOf, refreshSilently],
  );

  const handleToggleBlocked = useCallback(
    async (model: PanelModel, blocked: boolean) => {
      const current = filter?.blocklist ?? [];
      const next = blocked
        ? current.includes(model.id)
          ? current
          : [...current, model.id]
        : current.filter((id) => id !== model.id);

      setBlockingId(model.id);
      try {
        const result = await saveConfig({ models: { blocklist: next } });
        if (result.config) applyFilter(result.config.models_filter);
        notifySuccess(blocked ? `已禁用 ${model.id}` : `已启用 ${model.id}`);
        if (result.need_restart.length > 0) {
          notifyWarning("部分改动需重启进程生效", { description: result.need_restart.join("、") });
        }
        refreshSilently();
      } catch (err) {
        notifyError(describeError(err));
      } finally {
        setBlockingId(null);
      }
    },
    [applyFilter, filter, refreshSilently],
  );

  const handleToggleBlockedView = useCallback(
    (view: ModelView, blocked: boolean) => void handleToggleBlocked(view.model, blocked),
    [handleToggleBlocked],
  );

  return (
    <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
      {/* Hero：居中标题 + 搜索（参考页的入口形态） */}
      <header id="models-hero" className="mx-auto max-w-2xl text-center">
        <h1 className="text-[34px] font-semibold tracking-tight">模型与档位</h1>
        <p className="mt-3 text-sm text-muted-foreground">
          价格透明 · <span className="tabular-nums">{catalog?.model_count ?? 0}</span> 个模型
          {siteSummary ? <span className="tabular-nums"> · {siteSummary}</span> : null}
        </p>
        <p className="mt-2 text-xs leading-5 text-muted-foreground">
          上游模型目录、各站点计费倍率与推理档位，可探测价格并维护模型黑白名单。
        </p>
        <div className="relative mx-auto mt-5 max-w-xl">
          <Search
            className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden="true"
          />
          <Input
            type="search"
            value={query}
            spellCheck={false}
            autoComplete="off"
            aria-label="搜索模型"
            placeholder="搜索模型 id、名称、描述或标签…"
            onChange={(event) => setQuery(event.target.value)}
            className="h-10 pl-9 text-sm"
          />
        </div>
        <div className="mt-3 flex flex-wrap items-center justify-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
          {source ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="max-w-[280px] cursor-default truncate font-mono">
                  数据来源 {source}
                </span>
              </TooltipTrigger>
              <TooltipContent side="top">当前目录的采集来源与版本标识</TooltipContent>
            </Tooltip>
          ) : null}
          {catalog ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="cursor-default">
                  更新于 {fetchedAt > 0 ? formatRelativeTime(fetchedAt, now) : "未知"}
                </span>
              </TooltipTrigger>
              <TooltipContent side="top">{fetchedAbsolute ?? "后端未给出时间"}</TooltipContent>
            </Tooltip>
          ) : (
            <span>尚未加载目录</span>
          )}
          <span className="tabular-nums">已禁用 {blockedCount}</span>
        </div>
      </header>

      {error ? (
        <Card className="mt-6 gap-3 border-destructive/30 bg-destructive/5 p-4">
          <div className="text-sm font-medium text-destructive">读取模型目录失败</div>
          <p className="text-xs leading-5 text-muted-foreground">{error}</p>
          {unauthorized ? (
            <p className="text-xs leading-5 text-muted-foreground">
              网关要求鉴权：请到「账号」页右上角配置访问密钥后重试。
            </p>
          ) : null}
          <div>
            <Button type="button" variant="outline" size="sm" onClick={reload} disabled={loading}>
              重试
            </Button>
          </div>
        </Card>
      ) : null}

      <div className="mt-7">
        <AccessInfoCard authCheckEnabled={authCheckEnabled} configuredApiKey={configuredApiKey} />
      </div>

      <div className="mt-4">
        <ModelsSummary
          modelCount={catalog?.model_count ?? 0}
          counts={counts}
          loading={firstLoad}
        />
      </div>

      {/* 双栏：左筛选栏（吸顶）/ 右工具栏 + 内容 */}
      <div className="mt-6 grid min-w-0 grid-cols-1 gap-6 lg:grid-cols-[236px_minmax(0,1fr)]">
        <ModelFilterRail
          all={views}
          filters={filters}
          onChange={setFilters}
          className="lg:sticky lg:top-4 lg:self-start"
        />

        <section className="min-w-0" aria-label="模型目录">
          <div className="flex min-w-0 flex-wrap items-center justify-between gap-3">
            <div className="flex min-w-0 items-baseline gap-2">
              <h2 className="text-[13px] font-medium leading-5">模型目录</h2>
              <span className="text-xs text-muted-foreground tabular-nums">
                {visible.length}
                {filtered ? ` / ${views.length}` : ""} 个模型
              </span>
            </div>
            <div className="flex max-w-full flex-wrap items-center justify-end gap-2">
              <ModelSortToggle value={sort} onChange={setSort} />
              <ViewModeToggle value={viewMode} onChange={setViewMode} />
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() => void handleRefreshCatalog()}
                    disabled={refreshing}
                  >
                    {refreshing ? (
                      <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                    ) : (
                      <RefreshCw className="size-3.5" aria-hidden="true" />
                    )}
                    刷新目录
                  </Button>
                </TooltipTrigger>
                <TooltipContent side="top">从上游重新拉取模型目录，不消耗额度</TooltipContent>
              </Tooltip>
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button
                    type="button"
                    size="sm"
                    onClick={() => void handleProbeAll()}
                    disabled={probingAll || unknownModelCount === 0}
                  >
                    {probingAll ? (
                      <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                    ) : (
                      <FlaskConical className="size-3.5" aria-hidden="true" />
                    )}
                    探测未知价格
                    {unknownModelCount > 0 ? (
                      <span className="tabular-nums">（{unknownModelCount}）</span>
                    ) : null}
                  </Button>
                </TooltipTrigger>
                <TooltipContent side="top" className="max-w-[320px]">
                  对价格未知的模型各发一次极短对话请求实测计费，会真实消耗账号额度；
                  一次最多提交 {PROBE_BATCH_LIMIT} 个模型
                </TooltipContent>
              </Tooltip>
            </div>
          </div>

          {allowlist.length > 0 ? (
            <div className="mt-3 flex items-start gap-2 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs leading-5 text-amber-700 dark:text-amber-300">
              <TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
              <span>
                白名单非空（<span className="font-mono">{allowlist.join("、")}</span>
                ），网关已改为「仅放行白名单内的模型」：下方的启用开关只会把模型移出黑名单，不在白名单内的模型仍然不会被调度。
              </span>
            </div>
          ) : null}

          {firstLoad ? (
            <div className="mt-3 grid gap-3 sm:grid-cols-2">
              {[0, 1, 2, 3].map((index) => (
                <Skeleton key={index} className="h-52 rounded-xl" />
              ))}
            </div>
          ) : visible.length === 0 ? (
            <div className="mt-3 flex flex-col items-center gap-2 rounded-xl border border-dashed py-14 text-center">
              <Inbox className="size-6 text-muted-foreground" aria-hidden="true" />
              <div className="text-sm font-medium">
                {keyword || filtered ? "没有符合条件的模型" : "模型目录为空"}
              </div>
              <p className="max-w-sm text-xs leading-5 text-muted-foreground">
                {keyword || filtered
                  ? "试试放宽筛选条件，或清空搜索框。"
                  : "点右上角「刷新目录」从上游重新拉取模型列表。"}
              </p>
              {keyword || filtered ? (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    setQuery("");
                    setFilters(EMPTY_FILTERS);
                  }}
                >
                  清除搜索与筛选
                </Button>
              ) : null}
            </div>
          ) : viewMode === "card" ? (
            <div className="mt-3 grid min-w-0 gap-3 sm:grid-cols-2">
              {visible.map((view) => (
                <ModelCard
                  key={view.model.id}
                  view={view}
                  now={now}
                  pendingSites={pendingSitesOf(view.model)}
                  probing={probingId === view.model.id}
                  blockBusy={blockingId !== null}
                  siteFilter={filters.sites.length === 1 ? filters.sites[0] : null}
                  onProbe={() => void handleProbeModel(view.model)}
                  onToggleBlocked={(blocked) => handleToggleBlockedView(view, blocked)}
                />
              ))}
            </div>
          ) : (
            <Card className="mt-3 gap-0 overflow-hidden py-0">
              <ModelsTable
                models={visibleModels}
                blockedIds={blockedIds}
                probeable={probeable}
                now={now}
                probingId={probingId}
                blockingId={blockingId}
                siteFilter={filters.sites.length === 1 ? filters.sites[0] : null}
                onProbe={handleProbeModel}
                onToggleBlocked={handleToggleBlocked}
              />
            </Card>
          )}
        </section>
      </div>

      <Section
        id="models-pricing-notes"
        title="关于倍率与价格判定"
        description="倍率口径、价格探测方式与黑白名单规则。"
        className="mt-12"
      >
        <CardContent className="text-xs leading-5 text-muted-foreground">
          <ul className="list-disc space-y-1.5 pl-4">
            <li>
              「倍率」是上游对该模型计费的相对系数：0.29x 表示按标准价的 0.29 倍计费，0.00x
              即不扣额度。页面上的倍率文案由网关算好后直接展示，前端不做换算。
              卡片上的档位按
              <span className="font-medium text-foreground">已确认站点中的最低倍率</span>
              分档 —— 网关默认倾向便宜的那一侧，最低倍率才是实际会付的价。
            </li>
            <li>
              免费 / 收费靠实测判定：探测前后对比账号剩余积分与消耗（余额差分），因此
              <span className="font-medium text-amber-700 dark:text-amber-300">
                「探测价格」会真实消耗账号额度
              </span>
              。价格未确认的模型网关约每 12 小时自动跑一轮，也可以在这里手动触发。
            </li>
            <li>
              页首的免费档 / 低价档 / 中价档 / 高价档 / 倍率未确认是
              <span className="font-medium text-foreground">按模型</span>
              数的六个互不重叠的桶，合计等于模型总数。它与网关的探测汇总不是同一个口径 ——
              后者按
              <span className="font-medium text-foreground">（站点 × 模型）</span>
              条目计数，只统计已探测过的条目，未探测的模型完全不计入。
            </li>
            <li>
              黑名单内的模型不参与调度；白名单非空时规则变为「仅放行白名单内的模型」，此时不在白名单里的模型即使未进黑名单也不会被调度。
            </li>
            <li>
              卡片上的彩色徽章（如「限时免费」）来自上游下发的运营标签，颜色由上游指定，
              随促销活动变动；「仅推理」表示该模型不接受关闭思考。
            </li>
          </ul>
        </CardContent>
      </Section>
    </div>
  );
}

/** 视图切换：卡片（默认，参考页形态）/ 表格（逐列比对用）。 */
function ViewModeToggle({
  value,
  onChange,
}: {
  value: ViewMode;
  onChange: (next: ViewMode) => void;
}) {
  const options: { value: ViewMode; label: string; icon: typeof LayoutGrid }[] = [
    { value: "card", label: "卡片", icon: LayoutGrid },
    { value: "table", label: "表格", icon: List },
  ];
  return (
    <div
      role="group"
      aria-label="展示方式"
      className="flex items-center gap-0.5 rounded-lg bg-foreground/[0.05] p-0.5"
    >
      {options.map(({ value: option, label, icon: Icon }) => {
        const active = option === value;
        return (
          <button
            key={option}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(option)}
            className={cn(
              "inline-flex cursor-pointer items-center gap-1.5 rounded-md px-2.5 py-1 text-xs outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50",
              active
                ? "bg-background font-medium text-foreground shadow-xs"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            <Icon className="size-3.5" aria-hidden="true" />
            {label}
          </button>
        );
      })}
    </div>
  );
}
