import { useCallback, useEffect, useState } from "react";
import { CloudDownload, Loader2, RefreshCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { describeError, fetchOfficialUsage } from "@/lib/api";
import { formatCount, formatCredits, formatRelativeTime } from "@/lib/format";
import type { OfficialUsageResponse } from "@/lib/types";

/**
 * 官方口径用量卡片。
 *
 * 定位说明（页面上也会写出来）：这是**上游计费系统**给的原始请求行，
 * 而用量统计的图表是网关自己按小时桶算的。两者对照才能发现漏记、重复计、
 * token 解析偏差。既然口径不同，就不该把两条曲线混在一张图里假装是一回事。
 */
export function OfficialUsageCard({ days }: { days: number }) {
  const [data, setData] = useState<OfficialUsageResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(
    async (silent = false) => {
      if (!silent) setLoading(true);
      try {
        const result = await fetchOfficialUsage(days);
        setData(result);
        setError(null);
      } catch (err) {
        if (!silent) setError(describeError(err));
      } finally {
        if (!silent) setLoading(false);
      }
    },
    [days],
  );

  useEffect(() => {
    void load();
  }, [load]);

  const totalRequests = (data?.accounts ?? []).reduce((sum, a) => sum + a.requests, 0);
  const totalCredit = (data?.accounts ?? []).reduce((sum, a) => sum + a.credit, 0);
  const totalRows = (data?.accounts ?? []).reduce((sum, a) => sum + (a.total || 0), 0);
  // 注意 `data?.rows?.length` 里第二个可选链不能省：后端在无数据时曾返回
  // `rows: null`，而这里只保护了 data 却没保护 rows，会直接抛
  // "Cannot read properties of null (reading 'length')" 并把整棵 React 树带走。
  const truncated = totalRows > (data?.rows?.length ?? 0);

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <CardTitle className="flex items-center gap-1.5 text-sm">
              <CloudDownload className="size-3.5" aria-hidden="true" />
              官方口径用量
            </CardTitle>
            <CardDescription className="mt-1 text-xs">
              上游计费系统的原始请求记录（最近 {days} 天）
              {data ? (
                <span className="ml-1 text-muted-foreground">
                  ·{" "}
                  <span title={new Date(data.fetched_at * 1000).toLocaleString()}>
                    {formatRelativeTime(data.fetched_at, Math.floor(Date.now() / 1000))}
                  </span>
                  读取
                </span>
              ) : null}
            </CardDescription>
          </div>
          <Button type="button" size="sm" variant="outline" disabled={loading} onClick={() => void load()}>
            {loading ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <RefreshCw className="size-3.5" aria-hidden="true" />
            )}
            刷新
          </Button>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        {error ? <p className="text-xs text-destructive">{error}</p> : null}
        {loading && data === null ? (
          <div className="space-y-2">
            <Skeleton className="h-16" />
            <Skeleton className="h-32" />
          </div>
        ) : null}

        {data && !error ? (
          <>
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
              <MiniStat label="官方请求数" value={formatCount(totalRequests)} />
              <MiniStat label="官方 credit" value={formatCredits(totalCredit)} />
              <MiniStat
                label="今日 credit"
                value={formatCredits((data.accounts ?? []).reduce((s, a) => s + a.credit_today, 0))}
              />
              <MiniStat label="上游总条数" value={formatCount(totalRows)} />
            </div>

            {/* 账号 */}
            <div className="overflow-x-auto">
              <table className="w-full min-w-[480px] text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs text-muted-foreground">
                    <th className="py-1.5 pr-3 font-medium">账号</th>
                    <th className="py-1.5 pr-3 text-right font-medium">请求</th>
                    <th className="py-1.5 pr-3 text-right font-medium">credit</th>
                    <th className="py-1.5 text-right font-medium">今日</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {(data.accounts ?? []).map((a) => (
                    <tr key={a.id}>
                      <td className="py-1.5 pr-3">
                        <span className="text-xs">{a.nickname || a.id}</span>
                        <span className="ml-1.5 text-xs text-muted-foreground">{a.site_label}</span>
                        {a.error ? (
                          <span className="ml-1.5 text-xs text-destructive" title={a.error}>
                            读取失败
                          </span>
                        ) : null}
                      </td>
                      <td className="py-1.5 pr-3 text-right tabular-nums">{formatCount(a.requests)}</td>
                      <td className="py-1.5 pr-3 text-right tabular-nums">{formatCredits(a.credit)}</td>
                      <td className="py-1.5 text-right tabular-nums">{formatCredits(a.credit_today)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            {/* 官方每日 */}
            {(data.days ?? []).length > 0 ? (
              <div>
                <p className="mb-1.5 text-xs font-medium text-foreground">官方每日</p>
                <div className="max-h-48 overflow-y-auto overflow-x-auto">
                  <table className="w-full min-w-[320px] text-sm">
                    <tbody className="divide-y divide-border">
                      {(data.days ?? []).map((d) => (
                        <tr key={d.date}>
                          <td className="py-1.5 pr-3 font-mono text-xs">{d.date}</td>
                          <td className="py-1.5 pr-3 text-right tabular-nums">
                            {formatCount(d.requests)} 次
                          </td>
                          <td className="py-1.5 text-right tabular-nums">{formatCredits(d.credit)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </div>
            ) : null}

            {/* 模型与客户端 */}
            <div className="grid gap-4 sm:grid-cols-2">
              <div>
                <p className="mb-1.5 text-xs font-medium text-foreground">按模型</p>
                <ul className="space-y-1 text-xs">
                  {(data.models ?? []).slice(0, 8).map((m) => (
                    <li key={m.model} className="flex items-center justify-between gap-2">
                      <span className="truncate font-mono">{m.model}</span>
                      <span className="shrink-0 tabular-nums text-muted-foreground">
                        {formatCount(m.requests)} 次 · {formatCredits(m.credit)}
                      </span>
                    </li>
                  ))}
                  {(data.models ?? []).length === 0 ? (
                    <li className="text-muted-foreground">无数据</li>
                  ) : null}
                </ul>
              </div>
              <div>
                <p className="mb-1.5 text-xs font-medium text-foreground">客户端来源</p>
                <ul className="space-y-1 text-xs">
                  {(data.clients ?? []).slice(0, 8).map((c) => (
                    <li key={c.client} className="flex items-center justify-between gap-2">
                      <span className="truncate">{c.client}</span>
                      <span className="shrink-0 tabular-nums text-muted-foreground">
                        {formatCount(c.requests)} 次
                      </span>
                    </li>
                  ))}
                  {(data.clients ?? []).length === 0 ? (
                    <li className="text-muted-foreground">无数据</li>
                  ) : null}
                </ul>
              </div>
            </div>

            {/* 明细 */}
            {(data.rows ?? []).length > 0 ? (
              <div>
                <p className="mb-1.5 text-xs font-medium text-foreground">
                  最近明细（{data.rows.length} 条{truncated ? "，仅展示最近部分" : ""}）
                </p>
                <div className="max-h-56 overflow-y-auto rounded-md border border-border/60">
                  <table className="w-full min-w-[380px] text-xs">
                    <tbody className="divide-y divide-border/60">
                      {(data.rows ?? []).slice(0, 40).map((r, i) => (
                        <tr key={`${r.request_time}-${r.request_id ?? i}`}>
                          <td className="py-1 pl-2 pr-2 font-mono text-xs text-muted-foreground">
                            {r.request_time.slice(5)}
                          </td>
                          <td className="py-1 pr-2 font-mono">{r.model}</td>
                          <td className="py-1 pr-2 text-right tabular-nums">{formatCredits(r.credit)}</td>
                          <td className="py-1 pr-2 text-xs text-muted-foreground">
                            {r.client || "-"}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </div>
            ) : null}

            <p className="text-xs leading-5 text-muted-foreground">
              口径说明：这一栏来自上游计费接口（原始请求行），上面三张图来自网关自己的小时桶统计。
              两者不一致时以上游为准——网关口径的偏差只影响趋势图，不影响实际计费。
              单次查询上游最多返回 3000 条，超出时只取最近的部分。
            </p>
          </>
        ) : null}
      </CardContent>
    </Card>
  );
}

function MiniStat({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-border/60 bg-muted/40 px-3 py-2">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-0.5 text-lg font-semibold tabular-nums">{value}</div>
    </div>
  );
}

/** 把时间范围映射成官方查询的天数。 */
export function rangeToDays(range: string): number {
  if (range === "30d") return 30;
  if (range === "7d") return 7;
  return 1;
}
