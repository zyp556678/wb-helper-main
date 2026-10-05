import { useCallback, useEffect, useState } from "react";
import {
  ArrowRightLeft,
  CircleAlert,
  Coins,
  Loader2,
  RefreshCw,
  ServerCog,
  Users,
} from "lucide-react";
import { notifySuccess } from "@/lib/notify";

import { StatTile } from "@/components/stats-bar";
import { SiteRouteCard } from "@/components/site-route-card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { MetricRow, Section } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import { describeError, fetchSites } from "@/lib/api";
import { formatCount, formatCredits, formatRelativeTime } from "@/lib/format";
import type { SiteView, SitesResponse } from "@/lib/types";
import { useNowSeconds } from "@/lib/use-now-seconds";

export function SitesPage() {
  const [data, setData] = useState<SitesResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const now = useNowSeconds(1000);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      setData(await fetchSites());
      setError(null);
    } catch (err) {
      setError(describeError(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const firstLoad = loading && data === null;

  if (firstLoad) {
    return (
      <div className="mx-auto w-full max-w-[1180px] space-y-6 px-6 py-8 sm:px-8 sm:py-9">
        <Skeleton className="h-8 w-40" />
        <div className="grid gap-4 lg:grid-cols-2">
          <Skeleton className="h-[430px] rounded-xl" />
          <Skeleton className="h-[430px] rounded-xl" />
        </div>
      </div>
    );
  }

  if (error !== null && data === null) {
    return (
      <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
        <Card className="border-destructive/40 bg-destructive/5 p-6">
          <div className="flex items-start gap-3">
            <CircleAlert className="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden="true" />
            <div className="min-w-0 space-y-1">
              <div className="text-sm font-medium">读取双站视图失败</div>
              <p className="text-xs leading-5 text-muted-foreground">{error}</p>
            </div>
          </div>
          <Button className="mt-4" size="sm" variant="outline" onClick={() => void load()}>
            重试
          </Button>
        </Card>
      </div>
    );
  }

  const sites = data?.sites ?? [];
  const preference = data?.preference ?? { enabled: false, rules: [], note: "" };

  return (
    <div className="mx-auto w-full max-w-[1180px] min-w-0 space-y-12 px-6 py-8 sm:px-8 sm:py-9">
      <header className="mb-6 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">双站视图</h1>
          <p className="mt-2 text-sm leading-6 text-muted-foreground">
            国内站与国际站的账号分布、模型覆盖与价格结论对照。两站在上游是两套独立的域名、凭据与计费，
            在网关内部归入同一个账号池，这一页用来确认调度实际在往哪一侧倾斜。
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-1.5 pt-1">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={loading}
            onClick={() => {
              void load().then(() => notifySuccess("已刷新"));
            }}
          >
            {loading ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <RefreshCw className="size-3.5" aria-hidden="true" />
            )}
            刷新
          </Button>
        </div>
      </header>

      <div className="grid gap-4 lg:grid-cols-2">
        {sites.map((site) => (
          <SiteCard key={site.site} site={site} now={now} />
        ))}
      </div>

      {/* 「怎么指定走哪一站」放在「两站现状」之后：先看清现状，再决定往哪边指。
          它自己取数、自己处理失败（读不到就整块不渲染），不参与上面那张卡的加载态。 */}
      <SiteRouteCard />

      <PreferenceCard
        enabled={preference.enabled}
        rules={preference.rules}
        note={preference.note}
        preferFreeSite={data?.prefer_free_site ?? false}
      />
    </div>
  );
}

/** 单个站点卡：账号汇总 + 模型覆盖 + 价格分布。 */
function SiteCard({ site, now }: { site: SiteView; now: number }) {
  const { accounts, pricing, catalog } = site;
  const [showAllPaid, setShowAllPaid] = useState(false);

  const total = Math.max(pricing.free + pricing.paid + pricing.unknown, 1);
  const paidVisible = showAllPaid ? pricing.paid_models : pricing.paid_models.slice(0, 10);

  return (
    <Card className="flex flex-col gap-0 rounded-2xl py-0 shadow-none">
      <CardHeader className="flex flex-row items-start justify-between gap-3 space-y-0 border-b border-border pb-4">
        <div className="min-w-0">
          <CardTitle className="flex items-center gap-2 text-sm">
            {site.label}
            <span className="font-mono text-xs font-normal text-muted-foreground">
              {site.site}
            </span>
          </CardTitle>
          <div className="mt-1.5 space-y-0.5 font-mono text-xs text-muted-foreground">
            <div className="truncate" title={site.base_url}>
              API {site.base_url}
            </div>
            <div className="truncate" title={site.origin}>
              Web {site.origin}
            </div>
          </div>
        </div>
        <Badge variant={site.usable ? "default" : "outline"} className="shrink-0">
          {site.usable ? "可用" : "未启用"}
        </Badge>
      </CardHeader>

      <CardContent className="flex-1 space-y-4">
        {site.note ? (
          <div className="flex items-start gap-2 rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2">
            <CircleAlert
              className="mt-0.5 size-3.5 shrink-0 text-amber-700 dark:text-amber-300"
              aria-hidden="true"
            />
            <p className="text-xs leading-5 text-muted-foreground">{site.note}</p>
          </div>
        ) : null}

        {/* 账号汇总 */}
        <MetricRow cols={4} className="rounded-lg bg-muted/30">
          <StatTile
            label="账号"
            value={String(accounts.total)}
            hint={`可用 ${accounts.active} / ${accounts.total}`}
            icon={Users}
            tone="default"
          />
          <StatTile
            label="在途请求"
            value={String(accounts.in_flight)}
            hint="当前打向该站点的并发"
            divided
            icon={ServerCog}
            tone="default"
          />
          <StatTile
            label="剩余积分"
            value={formatCredits(accounts.credits_remaining)}
            hint={
              accounts.quota_known > 0
                ? `已查 ${accounts.quota_known}/${accounts.total} 个账号`
                : "尚未查询额度"
            }
            icon={Coins}
            tone={accounts.credits_remaining > 0 ? "success" : "default"}
            divided
          />
          <StatTile
            label="目录模型"
            value={String(catalog.models)}
            hint={
              catalog.fetched_at > 0
                ? `${formatRelativeTime(catalog.fetched_at, now)}更新`
                : "本轮未拉取"
            }
            icon={ArrowRightLeft}
            tone={catalog.models > 0 ? "default" : "warning"}
            divided
          />
        </MetricRow>

        {/* 价格分布 */}
        <div className="space-y-2">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium">价格结论分布</span>
            <span className="tabular-nums text-muted-foreground">
              免费 {pricing.free} · 收费 {pricing.paid} · 未知 {pricing.unknown}
            </span>
          </div>
          <div className="flex h-2 overflow-hidden rounded-full bg-muted">
            <div
              className="bg-primary/80"
              style={{ width: `${(pricing.free / total) * 100}%` }}
              title={`免费 ${pricing.free}`}
            />
            <div
              className="bg-amber-500/70"
              style={{ width: `${(pricing.paid / total) * 100}%` }}
              title={`收费 ${pricing.paid}`}
            />
          </div>
          <p className="text-xs leading-5 text-muted-foreground">
            {pricing.probe.last_probe_at > 0
              ? `实测探测：免费 ${pricing.probe.free} · 收费 ${pricing.probe.paid} · 未判定 ${pricing.probe.unknown}（最近 ${formatRelativeTime(pricing.probe.last_probe_at, now)}）`
              : "尚无实测探测结论，价格全部来自目录倍率"}
          </p>
        </div>

        {/* 免费模型 */}
        <div className="space-y-2">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium">确认免费的模型</span>
            <span className="tabular-nums text-muted-foreground">
              {pricing.confirmed_free.length} 个
            </span>
          </div>
          {pricing.confirmed_free.length === 0 ? (
            <p className="text-xs leading-5 text-muted-foreground">
              该站点暂无确认免费的模型。「免费站点优先」只在两站结论相反时才生效。
            </p>
          ) : (
            <div className="flex flex-wrap gap-1">
              {pricing.confirmed_free.slice(0, 24).map((id) => (
                <Badge key={id} variant="outline" className="font-mono text-xs">
                  {id}
                </Badge>
              ))}
              {pricing.confirmed_free.length > 24 ? (
                <span className="text-xs text-muted-foreground">
                  +{pricing.confirmed_free.length - 24}
                </span>
              ) : null}
            </div>
          )}
        </div>

        {/* 收费模型 */}
        <div className="space-y-2">
          <div className="flex items-center justify-between text-xs">
            <span className="font-medium">收费模型与倍率</span>
            <span className="tabular-nums text-muted-foreground">
              {pricing.paid_models.length > 10
                ? `显示 ${paidVisible.length}/${pricing.paid_models.length}`
                : pricing.paid_models.length + " 个"}
            </span>
          </div>
          {pricing.paid_models.length === 0 ? (
            <p className="text-xs leading-5 text-muted-foreground">
              该站点没有已确认收费的模型。
            </p>
          ) : (
            <>
              <div className="max-h-[188px] overflow-y-auto rounded-md border border-border/60">
                <table className="w-full text-xs">
                  <tbody>
                    {paidVisible.map((m) => (
                      <tr key={m.id} className="border-b border-border/40 last:border-0">
                        <td className="max-w-0 truncate px-2.5 py-1.5 font-mono" title={m.id}>
                          {m.id}
                        </td>
                        <td className="whitespace-nowrap px-2.5 py-1.5 text-right tabular-nums text-amber-700 dark:text-amber-300">
                          {m.multiplier_label}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              {pricing.paid_models.length > 10 ? (
                <button
                  type="button"
                  className="cursor-pointer text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
                  onClick={() => setShowAllPaid((v) => !v)}
                >
                  {showAllPaid ? "收起" : `展开剩余 ${pricing.paid_models.length - 10} 个`}
                </button>
              ) : null}
            </>
          )}
        </div>

        {site.account_ids.length > 0 ? (
          <div className="space-y-1.5 border-t border-border/60 pt-3">
            <div className="text-xs font-medium">该站点账号</div>
            <div className="space-y-1">
              {site.account_ids.map((id) => (
                <div key={id} className="truncate font-mono text-xs text-muted-foreground">
                  {id}
                </div>
              ))}
            </div>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

/** 免费站点优先的状态与当前生效规则。 */
function PreferenceCard({
  enabled,
  rules,
  note,
  preferFreeSite,
}: {
  enabled: boolean;
  rules: { model: string; preferred: string[]; avoid: string[]; reason: string; requests: number }[];
  note: string;
  preferFreeSite: boolean;
}) {
  return (
    <Section
      id="sites-preference"
      title={
        <>
          免费站点优先
          <Badge variant={enabled ? "default" : "outline"} className="ml-2 align-middle">
            {enabled ? "已开启" : "已关闭"}
          </Badge>
        </>
      }
      description={
        <>
          {note}
          <span className="mt-0.5 block">
            开关在「配置 → 账号池」里，配置项 <code className="font-mono">pool.prefer_free_site</code>
          </span>
        </>
      }
    >
      <CardContent className="space-y-3">
        {!preferFreeSite ? (
          <div className="rounded-lg border border-border/60 bg-muted/40 px-3 py-2 text-xs leading-5 text-muted-foreground">
            倾斜已关闭：调度完全按积分与闲置度加权，不看站点价格。两站价格差异不大时这是合理选择。
          </div>
        ) : rules.length === 0 ? (
          <div className="rounded-lg border border-border/60 bg-muted/40 px-3 py-2 text-xs leading-5 text-muted-foreground">
            当前没有生效的倾斜规则。要产生规则，需要满足两个条件：两站都配置了账号（这样才有两侧的价格结论），
            且某个模型的结论是「一侧免费、另一侧收费」。若只有单站账号、或两侧结论相同/未知，都<span className="text-foreground">不做倾斜</span>
            —— 拿未知当免费去倾斜，等于把流量压到可能收费的一侧。
          </div>
        ) : (
          <div className="overflow-x-auto rounded-md border border-border/60">
            <table className="w-full min-w-[560px] text-xs">
              <thead className="bg-muted/40 text-xs text-muted-foreground">
                <tr>
                  <th className="px-3 py-2 text-left font-medium">模型</th>
                  <th className="px-3 py-2 text-left font-medium">优先使用</th>
                  <th className="px-3 py-2 text-left font-medium">尽量避开</th>
                  <th className="px-3 py-2 text-right font-medium">请求数</th>
                </tr>
              </thead>
              <tbody>
                {rules.map((rule) => (
                  <tr key={rule.model} className="border-t border-border/40">
                    <td className="px-3 py-2 font-mono">{rule.model}</td>
                    <td className="px-3 py-2">
                      <div className="flex flex-wrap gap-1">
                        {rule.preferred.map((s) => (
                          <Badge key={s} className="text-xs">
                            {s}
                          </Badge>
                        ))}
                      </div>
                    </td>
                    <td className="px-3 py-2">
                      <div className="flex flex-wrap gap-1">
                        {rule.avoid.length === 0 ? (
                          <span className="text-xs text-muted-foreground">-</span>
                        ) : (
                          rule.avoid.map((s) => (
                            <Badge key={s} variant="outline" className="text-xs">
                              {s}
                            </Badge>
                          ))
                        )}
                      </div>
                    </td>
                    <td className="px-3 py-2 text-right tabular-nums">{formatCount(rule.requests)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        <p className="text-xs leading-5 text-muted-foreground">
          倾斜是<span className="text-foreground">软优先</span>：优先站点里有可用账号时才收窄选择范围；
          优先站点全部冷却或在途占满时，会自动回落到另一侧继续服务，不会因为省钱让请求失败。
        </p>
      </CardContent>
    </Section>
  );
}
