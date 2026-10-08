/**
 * 请求流水（对照 wb2api-panel 的 reqlog 面板）。
 *
 * ## 与「日志」页的分工
 *
 *   - **日志页**是**事件流**：出站改写、拦截重试、账号治理……回答「发生了什么」。
 *   - **本页**是**请求级明细**：每次对话请求一条，带状态码/耗时/TTFB/token/结局。
 *     回答「刚才那次 502 是哪一次请求、走的哪个账号、卡了多久」。
 *
 * 两者数据源不同（事件环形缓冲 vs reqlog 指标 + JSONL 归档），不合成一个页面。
 *
 * ## 两个数据源
 *
 *   - `/panel/api/request-metrics`：进程内指标 + **最近 100 条** + 归档状态。始终可用。
 *   - `/panel/api/request-logs`：从**磁盘归档**按条件查，能翻到 100 条之外的历史。
 *
 * 默认看进程内的最近 100 条（最快、无 IO）；一旦用了筛选，就改走归档查询 ——
 * 因为筛选的意义正是「在更长的历史里找那一条」。
 *
 * ## 关于账号脱敏
 *
 * 记录里的账号是「昵称(uid8)」标签，不是完整 UID：归档是长期留盘的，
 * 完整 UID 不该落进去。所以这里显示的也是脱敏形式，属预期行为。
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Activity, Archive, CircleAlert, RefreshCw, SearchX, Timer, TrendingDown } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { MetricRow, PillGroup, Section, StatMetric } from "@/components/section";
import { describeError, fetchRequestLogs, fetchRequestMetrics, isAbortError, isUnauthorized } from "@/lib/api";
import { formatCount, formatCredits, formatTokens } from "@/lib/format";

/**
 * 速率显示：≥100 取整（小数位没有信息量），否则留一位。
 *
 * 缺失或 0 都显示 "—" 而不是 "0.0 tok/s"：那两种情况的含义是"这次没算出来"
 * （上游没报输出 token、或请求失败），写成 0 会被读成"生成得极慢"。
 */
function formatTokensPerSec(v: number | undefined): string {
  if (!v || v <= 0) return "—";
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} tok/s`;
}
import type { RequestEvent, RequestMetricsResponse } from "@/lib/types";
import { useVisibleInterval } from "@/lib/use-visible-interval";
import { cn } from "@/lib/utils";

/** 轮询间隔：请求流水变化快，2 秒够用且不至于打满面板。 */
const POLL_MS = 2000;

/** 归档查询返回上限（后端上限 1000）。 */
const ARCHIVE_LIMIT = 500;

const OUTCOME_OPTIONS = [
  { value: "", label: "全部" },
  { value: "success", label: "成功" },
  { value: "http_error", label: "上游拒绝" },
  { value: "stream_error", label: "流中断" },
] as const;

type OutcomeFilter = (typeof OUTCOME_OPTIONS)[number]["value"];

/** 结局的中文名与配色。 */
function outcomeMeta(outcome: string): { label: string; className: string } {
  switch (outcome) {
    case "success":
      return { label: "成功", className: "text-emerald-600 dark:text-emerald-400" };
    case "stream_error":
      return { label: "流中断", className: "text-amber-700 dark:text-amber-400" };
    case "interrupted":
      return { label: "已取消", className: "text-muted-foreground" };
    default:
      return { label: "上游拒绝", className: "text-destructive" };
  }
}

/** 把毫秒数渲染成紧凑形式（<1s 用 ms，否则用 s）。 */
function formatMs(ms: number | undefined): string {
  if (ms === undefined || ms === null) return "—";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / 1024 / 1024).toFixed(1)} MiB`;
}

/** 时间戳（RFC3339）→ 本地「MM-DD HH:mm:ss」。 */
function formatTime(raw: string): string {
  const d = new Date(raw);
  if (Number.isNaN(d.getTime())) return raw;
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export function RequestsPage() {
  const [metrics, setMetrics] = useState<RequestMetricsResponse | null>(null);
  const [archiveRows, setArchiveRows] = useState<RequestEvent[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [unauthorized, setUnauthorized] = useState(false);
  const [autoRefresh, setAutoRefresh] = useState(true);

  const [outcome, setOutcome] = useState<OutcomeFilter>("");
  const [account, setAccount] = useState("");
  const [model, setModel] = useState("");
  const [debouncedAccount, setDebouncedAccount] = useState("");
  const [debouncedModel, setDebouncedModel] = useState("");

  const controllerRef = useRef<AbortController | null>(null);
  const filtered = outcome !== "" || debouncedAccount.trim() !== "" || debouncedModel.trim() !== "";

  // 输入防抖：逐字触发会把归档查询打爆（每次都要扫盘）。
  useEffect(() => {
    const t = window.setTimeout(() => setDebouncedAccount(account), 300);
    return () => window.clearTimeout(t);
  }, [account]);
  useEffect(() => {
    const t = window.setTimeout(() => setDebouncedModel(model), 300);
    return () => window.clearTimeout(t);
  }, [model]);

  const load = useCallback(
    async (withLoading: boolean) => {
      controllerRef.current?.abort();
      const controller = new AbortController();
      controllerRef.current = controller;
      if (withLoading) setLoading(true);

      try {
        const m = await fetchRequestMetrics(controller.signal);
        setMetrics(m);
        setUnauthorized(false);

        // 有筛选条件时改查归档：筛选的意义就是在更长的历史里找那一条。
        if (filtered) {
          const rows = await fetchRequestLogs(
            {
              outcome: outcome || undefined,
              account: debouncedAccount || undefined,
              model: debouncedModel || undefined,
              limit: ARCHIVE_LIMIT,
            },
            controller.signal,
          );
          setArchiveRows(rows.entries ?? []);
        } else {
          setArchiveRows(null);
        }
        setError(null);
      } catch (err) {
        if (isAbortError(err)) return;
        setError(describeError(err));
        setUnauthorized(isUnauthorized(err));
      } finally {
        if (!controller.signal.aborted) setLoading(false);
      }
    },
    [filtered, outcome, debouncedAccount, debouncedModel],
  );

  // 条件变化立即拉一次
  useEffect(() => {
    void load(true);
  }, [load]);

  // 之后按开关轮询（页面不可见时暂停）
  useVisibleInterval(() => void load(false), POLL_MS, autoRefresh);

  useEffect(() => () => controllerRef.current?.abort(), []);

  const rows = useMemo<RequestEvent[]>(
    () => archiveRows ?? metrics?.recent ?? [],
    [archiveRows, metrics],
  );

  const firstLoad = loading && metrics === null;
  const archive = metrics?.archive;

  return (
    <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
      <header className="mb-6 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">请求流水</h1>
          <p className="mt-2 text-sm leading-6 text-muted-foreground">
            每次对话请求一条记录：状态码、结局、耗时、TTFB、token。默认看进程内最近{" "}
            {metrics?.recent?.length ?? 0} 条，用筛选可翻磁盘归档（最多 {ARCHIVE_LIMIT} 条）。
            账号已脱敏为「昵称(uid8)」。
          </p>
        </div>
        <div className="mt-1 flex shrink-0 items-center gap-1">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={() => setAutoRefresh((v) => !v)}
            aria-pressed={autoRefresh}
          >
            {autoRefresh ? "自动刷新：开" : "自动刷新：关"}
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={() => void load(true)}
            disabled={loading}
            aria-label="刷新请求流水"
          >
            <RefreshCw className={cn("size-3.5", loading && "animate-spin")} aria-hidden="true" />
            刷新
          </Button>
        </div>
      </header>

      {error ? (
        <Card className="mb-5 gap-3 border-destructive/30 bg-destructive/5 p-4">
          <div className="text-sm font-medium text-destructive">读取请求流水失败</div>
          <p className="text-xs leading-5 text-muted-foreground">{error}</p>
          {unauthorized ? (
            <p className="text-xs leading-5 text-muted-foreground">
              网关要求鉴权：请到「账号」页右上角配置访问密钥后重试。
            </p>
          ) : null}
          <div>
            <Button type="button" variant="outline" size="sm" onClick={() => void load(true)} disabled={loading}>
              重试
            </Button>
          </div>
        </Card>
      ) : null}

      <Section
        id="request-metrics"
        title="进程指标"
        description={
          metrics
            ? `自 ${formatTime(metrics.started_at)} 起累计`
            : "统计进程启动以来的请求，重启即归零。"
        }
      >
        <Card className="gap-0 overflow-hidden py-0">
          {firstLoad ? (
            <div className="grid grid-cols-2 gap-3 p-5 sm:grid-cols-4">
              {[0, 1, 2, 3].map((i) => (
                <Skeleton key={i} className="h-12 rounded-md" />
              ))}
            </div>
          ) : (
            <MetricRow cols={4}>
              <StatMetric
                icon={Activity}
                label="完成"
                value={formatCount(metrics?.completed ?? 0)}
                hint={`在途 ${metrics?.in_flight ?? 0}`}
              />
              <StatMetric
                icon={Activity}
                label="成功率"
                value={`${metrics?.success_rate ?? 0}%`}
                hint={`HTTP ${metrics?.http_success_rate ?? 0}%`}
                divided
              />
              <StatMetric
                icon={TrendingDown}
                label="失败"
                value={formatCount(metrics?.failed ?? 0)}
                hint={metrics?.failed ? "含流中断与上游拒绝" : "暂无失败"}
                divided
                valueClassName={metrics?.failed ? "text-destructive" : undefined}
              />
              <StatMetric
                icon={Timer}
                label="平均耗时"
                value={formatMs(metrics?.avg_duration_ms)}
                hint="从收到请求到结束"
                divided
              />
            </MetricRow>
          )}
        </Card>
      </Section>

      <Section
        id="request-archive"
        title="磁盘归档"
        description={
          archive?.enabled
            ? "脱敏元数据（不含提示词/正文/凭证）；队列满会丢弃而非拖慢请求。"
            : "归档未启用：进程内指标仍可用，但重启后不保留历史。"
        }
      >
        <Card className="gap-0 overflow-hidden py-0">
          <div className="flex flex-wrap items-center gap-x-6 gap-y-2 px-4 py-3.5 text-xs sm:px-5">
            <span className="inline-flex items-center gap-1.5">
              <Archive className="size-3.5 text-muted-foreground" aria-hidden="true" />
              {archive?.enabled ? "已启用" : "已关闭"}
            </span>
            <span className="text-muted-foreground">
              文件 <span className="tabular-nums text-foreground">{formatCount(archive?.files ?? 0)}</span> 个 ·
              体积 <span className="tabular-nums text-foreground">{formatBytes(archive?.bytes ?? 0)}</span>
            </span>
            <span className="text-muted-foreground">
              丢弃{" "}
              <span
                className={cn(
                  "tabular-nums",
                  (archive?.dropped_writes ?? 0) > 0 ? "text-amber-700 dark:text-amber-400" : "text-foreground",
                )}
              >
                {formatCount(archive?.dropped_writes ?? 0)}
              </span>{" "}
              条
            </span>
            {archive?.dir ? (
              <span className="max-w-full truncate font-mono text-xs text-muted-foreground" title={archive.dir}>
                {archive.dir}
              </span>
            ) : null}
          </div>
          {archive?.last_error ? (
            <div className="flex items-start gap-2 border-t bg-destructive/[0.06] px-4 py-2 text-xs text-destructive sm:px-5">
              <CircleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
              <span>最近一次归档写入失败：{archive.last_error}</span>
            </div>
          ) : null}
        </Card>
      </Section>

      <Section
        id="request-rows"
        title="请求明细"
        description={
          filtered ? `归档查询结果（最多 ${ARCHIVE_LIMIT} 条）` : "进程内最近 100 条，新的在前"
        }
        action={
          <div className="flex flex-wrap items-center gap-2">
            <PillGroup
              value={outcome}
              options={OUTCOME_OPTIONS}
              onChange={setOutcome}
              ariaLabel="按结局筛选"
            />
            <input
              value={account}
              onChange={(e) => setAccount(e.target.value)}
              placeholder="账号"
              aria-label="按账号筛选"
              className="h-8 w-28 rounded-md border bg-background px-2 text-xs outline-none focus-visible:ring-1 focus-visible:ring-ring"
            />
            <input
              value={model}
              onChange={(e) => setModel(e.target.value)}
              placeholder="模型"
              aria-label="按模型筛选"
              className="h-8 w-32 rounded-md border bg-background px-2 text-xs outline-none focus-visible:ring-1 focus-visible:ring-ring"
            />
          </div>
        }
      >
        <Card className="gap-0 overflow-hidden py-0">
          {firstLoad ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2, 3, 4].map((i) => (
                <Skeleton key={i} className="h-9 rounded-md" />
              ))}
            </div>
          ) : rows.length === 0 ? (
            <div className="flex flex-col items-center gap-2 py-14 text-center">
              {filtered ? (
                <>
                  <SearchX className="size-6 text-muted-foreground" aria-hidden="true" />
                  <div className="text-sm font-medium">归档里没有匹配的请求</div>
                  <p className="text-xs text-muted-foreground">放宽筛选条件，或确认归档已启用。</p>
                </>
              ) : (
                <>
                  <Activity className="size-6 text-muted-foreground" aria-hidden="true" />
                  <div className="text-sm font-medium">暂无请求</div>
                  <p className="text-xs text-muted-foreground">
                    发起一次对话请求后，这里会出现它的状态码、耗时与 token。
                  </p>
                </>
              )}
            </div>
          ) : (
            <div className="min-w-0 overflow-x-auto">
              <table className="w-full min-w-[900px] text-left text-xs">
                <thead className="sticky top-0 z-10 bg-muted/95 text-muted-foreground backdrop-blur">
                  <tr>
                    <th className="px-3 py-2.5 font-medium">时间</th>
                    <th className="px-3 py-2.5 font-medium">结局</th>
                    <th className="px-3 py-2.5 text-right font-medium">状态</th>
                    <th className="px-3 py-2.5 font-medium">账号</th>
                    <th className="px-3 py-2.5 font-medium">模型</th>
                    <th className="px-3 py-2.5 text-right font-medium">耗时</th>
                    <th className="px-3 py-2.5 text-right font-medium">TTFB</th>
                    <th className="px-3 py-2.5 text-right font-medium">Token</th>
                    <th className="px-3 py-2.5 text-right font-medium">速率</th>
                    <th className="px-3 py-2.5 font-medium">请求 ID</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((row) => {
                    const meta = outcomeMeta(row.outcome);
                    return (
                      <tr
                        key={`${row.request_id}-${row.time}`}
                        className="border-t border-border/60 align-top"
                      >
                        <td className="whitespace-nowrap px-3 py-3 text-muted-foreground">
                          {formatTime(row.time)}
                        </td>
                        <td className={cn("whitespace-nowrap px-3 py-3 font-medium", meta.className)}>
                          {meta.label}
                        </td>
                        <td className="whitespace-nowrap px-3 py-3 text-right tabular-nums">{row.status}</td>
                        <td className="max-w-[160px] truncate px-3 py-3" title={row.account}>
                          {row.account || "—"}
                        </td>
                        <td className="max-w-[160px] truncate px-3 py-3" title={row.model}>
                          {row.model || "—"}
                        </td>
                        <td className="whitespace-nowrap px-3 py-3 text-right tabular-nums">
                          {formatMs(row.duration_ms)}
                        </td>
                        <td className="whitespace-nowrap px-3 py-3 text-right tabular-nums text-muted-foreground">
                          {row.ttfb_ms ? formatMs(row.ttfb_ms) : "—"}
                        </td>
                        <td className="whitespace-nowrap px-3 py-3 text-right tabular-nums">
                          {row.total_tokens ? formatTokens(row.total_tokens) : "—"}
                          {row.credit_known && row.credit ? (
                            <span className="ml-1 text-muted-foreground">({formatCredits(row.credit)})</span>
                          ) : null}
                        </td>
                        <td className="whitespace-nowrap px-3 py-3 text-right tabular-nums text-muted-foreground">
                          {formatTokensPerSec(row.tokens_per_sec)}
                        </td>
                        <td
                          className="max-w-[170px] truncate px-3 py-3 font-mono text-xs text-muted-foreground"
                          title={row.request_id}
                        >
                          {row.request_id}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </Section>
    </div>
  );
}
