import { useEffect, useState } from "react";
import type * as React from "react";
import { CircleAlert, CircleCheck, Loader2, Route } from "lucide-react";

import { CopyIconButton, useCopy } from "@/components/copy-button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { describeError, fetchSiteRoute } from "@/lib/api";
import type { SiteRouteResponse } from "@/lib/types";
import { cn } from "@/lib/utils";

/** 优先级在界面上的呈现：数字太大不好扫，用「优先 / 其次 / 再次」更直观。 */
const PRIORITY_LABEL: Record<number, string> = {
  1: "最高优先",
  2: "其次",
  3: "再次",
};

/**
 * 本机实际生效的站点路由写法。
 *
 * ## 为什么这一块必须由后端下发词表
 *
 * 「怎么指定走哪一站」有三种写法，每种能接受的字符串还不一样（`?site=` 认
 * intl / international / global / ai / workbuddy.ai …，而模型名前缀**只认** CN- / AI-）。
 * 这套词表在后端 `normalizeSiteQuery` 里，前端自己再抄一份必然漂移 ——
 * 一旦漂移，界面就会教出后端不认的写法，而**这种错误是静默的**：
 * 用户照着写，请求被当成「未指定站点」，没有报错，只会觉得「我明明指定了却没用」。
 *
 * 所以这里把 options / aliases 全部渲染后端给的值，前端一个字都不写死。
 *
 * ## 与「双站视图」的分工
 *
 * 上面的双站卡回答「两站现在各有多少账号、价格如何」；这一块回答
 * 「我想让某个请求固定走某一站，该怎么写」。后者是动作指引，前者是现状。
 */
export function SiteRouteCard() {
  const [data, setData] = useState<SiteRouteResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const { copiedKey, copy } = useCopy();

  useEffect(() => {
    const controller = new AbortController();
    let alive = true;
    void (async () => {
      try {
        const result = await fetchSiteRoute(controller.signal);
        if (!alive) return;
        setData(result);
        setError(null);
      } catch (err) {
        if (!alive) return;
        // 用户主动取消（切页/卸载）不算错误。
        if (controller.signal.aborted) return;
        setError(describeError(err));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => {
      alive = false;
      controller.abort();
    };
  }, []);

  if (loading) {
    return (
      <RouteCardShell>
        <CardHeader className="border-b border-border pb-4">
          <CardTitle className="flex items-center gap-2 text-sm">
            <Route className="size-4 text-muted-foreground" aria-hidden="true" />
            指定站点
          </CardTitle>
        </CardHeader>
        <CardContent className="flex items-center gap-2 py-4 text-xs text-muted-foreground">
          <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
          正在读取路由写法…
        </CardContent>
      </RouteCardShell>
    );
  }

  // 读不到就整块不显示：这是辅助说明，报错块本身没有可操作性，
  // 反而会把页面重心从「双站现状」拉到一个次要区块上。
  if (error !== null || data === null) return null;

  const options = [...data.options].sort((a, b) => a.priority - b.priority);

  return (
    <RouteCardShell>
      <CardHeader className="flex flex-row items-start justify-between gap-3 space-y-0 border-b border-border pb-4">
        <div className="min-w-0">
          <CardTitle className="flex items-center gap-2 text-sm">
            <Route className="size-4 text-muted-foreground" aria-hidden="true" />
            指定站点
          </CardTitle>
          <p className="mt-1.5 text-xs leading-5 text-muted-foreground">
            让一个请求固定走国内站或国际站。三种写法按下面的优先级判定，
            <span className="text-foreground">多个来源同时出现时高优先级的胜出</span>。
          </p>
        </div>
        <Badge variant="outline" className="shrink-0">
          不会跨站回落
        </Badge>
      </CardHeader>

      <CardContent className="space-y-4">
        {/* 三种写法 */}
        <div className="space-y-1.5">
          {options.map((opt) => {
            const key = `opt:${opt.kind}`;
            return (
              <div
                key={opt.kind}
                className="rounded-lg border border-border/60 px-3 py-2.5"
              >
                <div className="flex min-w-0 items-center gap-2">
                  <span className="shrink-0 text-xs font-medium">{opt.label}</span>
                  <Badge variant="outline" className="shrink-0 text-xs">
                    {PRIORITY_LABEL[opt.priority] ?? `优先级 ${opt.priority}`}
                  </Badge>
                  <code className="ml-auto min-w-0 truncate font-mono text-xs text-muted-foreground">
                    {opt.syntax}
                  </code>
                  <CopyIconButton
                    label={`${opt.label}示例`}
                    copied={copiedKey === key}
                    onCopy={() => void copy(opt.example, key, `${opt.label}示例`)}
                    className="size-5 shrink-0 p-0 text-muted-foreground"
                  />
                </div>
                <div className="mt-1.5 flex min-w-0 items-center gap-1.5">
                  <code className="min-w-0 truncate rounded bg-muted px-1.5 py-1 font-mono text-xs text-foreground">
                    {opt.example}
                  </code>
                </div>
                <p className="mt-1.5 text-xs leading-5 text-muted-foreground">{opt.note}</p>
              </div>
            );
          })}
        </div>

        {/* 每站可用的写法 */}
        <div className="space-y-2">
          <div className="text-xs font-medium">各站点可用的写法</div>
          <div className="overflow-x-auto rounded-md border border-border/60">
            <table className="w-full min-w-[520px] text-xs">
              <thead className="bg-muted/40 text-xs text-muted-foreground">
                <tr>
                  <th className="px-3 py-2 text-left font-medium">站点</th>
                  <th className="whitespace-nowrap px-3 py-2 text-left font-medium">模型名前缀</th>
                  <th className="px-3 py-2 text-left font-medium">
                    <span className="font-medium">?site= / X-WB-Site</span>
                    <span className="ml-1 font-normal">可写</span>
                  </th>
                </tr>
              </thead>
              <tbody>
                {data.sites.map((site) => {
                  const key = `prefix:${site.site}`;
                  return (
                    <tr key={site.site} className="border-t border-border/40">
                      <td className="px-3 py-2">
                        <div className="font-medium">{site.label}</div>
                        <div className="font-mono text-xs text-muted-foreground">
                          {site.site}
                        </div>
                      </td>
                      <td className="whitespace-nowrap px-3 py-2">
                        <div className="flex items-center gap-1">
                          {/* model_prefix 由后端下发且**已含分隔符**（如 `CN-`），
                              这里直接渲染，不要再拼 `/` 或 `-`。 */}
                          <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">
                            {site.model_prefix}
                          </code>
                          <CopyIconButton
                            label={`${site.label}的模型名前缀`}
                            copied={copiedKey === key}
                            onCopy={() =>
                              void copy(site.model_prefix, key, `${site.label}的模型名前缀`)
                            }
                            className="size-5 shrink-0 p-0 text-muted-foreground"
                          />
                        </div>
                      </td>
                      <td className="px-3 py-2">
                        <div className="flex flex-wrap gap-1">
                          {site.query_aliases.map((alias) => {
                            const aliasKey = `alias:${alias}`;
                            return (
                              <button
                                key={alias}
                                type="button"
                                title={`复制 ${alias}`}
                                onClick={() => void copy(alias, aliasKey, `写法 ${alias}`)}
                                className={cn(
                                  "inline-flex cursor-pointer items-center gap-1 rounded border border-border/60 px-1.5 py-0.5 font-mono text-xs outline-none transition-colors hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring/50",
                                  copiedKey === aliasKey && "border-primary/40 bg-primary/10",
                                )}
                              >
                                {copiedKey === aliasKey ? (
                                  <CircleCheck className="size-3 text-primary-ink" aria-hidden="true" />
                                ) : null}
                                {alias}
                              </button>
                            );
                          })}
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>

        {/* 注意事项。
            文字用 amber-700/300 而不是 text-muted-foreground：后者是中性灰，
            叠在琥珀色淡底上实测只有 2.65:1（同一令牌放在纯卡片底上是 5.51:1），
            过不了 WCAG AA 的 4.5:1。项目里其余琥珀底区块（模型页白名单提示、
            任务页）也都是这么处理的，这里跟随同一口径。 */}
        <div className="flex items-start gap-2 rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2">
          <CircleAlert
            className="mt-0.5 size-3.5 shrink-0 text-amber-700 dark:text-amber-300"
            aria-hidden="true"
          />
          <ul className="min-w-0 list-disc space-y-1 pl-4 text-xs leading-5 text-amber-700 dark:text-amber-300">
            {data.notes.map((note) => (
              <li key={note}>{note}</li>
            ))}
          </ul>
        </div>
      </CardContent>
    </RouteCardShell>
  );
}

/**
 * 外层容器。刻意与 SiteCard 用同一套外壳（圆角 2xl、无阴影、py-0），
 * 让两张卡放在一起时视觉一致；抽出来只是为了避免在 early return
 * 的分支里重复一遍这串 class。
 */
function RouteCardShell({ children }: { children: React.ReactNode }) {
  return <Card className="flex flex-col gap-0 rounded-2xl py-0 shadow-none">{children}</Card>;
}
