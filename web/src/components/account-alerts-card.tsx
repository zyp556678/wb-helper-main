import { AlertTriangle, CircleAlert, Info, ShieldAlert } from "lucide-react";

import { Card } from "@/components/ui/card";
import type { AccountAlert } from "@/lib/types";
import { cn } from "@/lib/utils";

/* ────────────────────────────────────────────────────────────────────────
 * 账号健康告警（账号页顶部的平铺列表，对照 wb-gateway v1.14.7 / v1.14.17）。
 *
 * 为什么要单独一栏：账号失效、令牌过期、额度用尽、冷却、暂停这五种状态
 * 各自有不同的处置动作，散在几十张卡片里时只能逐个看。这里把「现在该先
 * 处理谁」按严重度摊平。
 *
 * 数据直接来自 `/panel/api/overview` 的 `alerts` —— 与账号卡片上的状态
 * 出自同一份池快照，因此两处**不可能互相矛盾**。
 * ──────────────────────────────────────────────────────────────────────── */

const SEVERITY_STYLE: Record<
  AccountAlert["severity"],
  { icon: typeof Info; box: string; text: string; icon_: string }
> = {
  urgent: {
    icon: ShieldAlert,
    box: "border-destructive/30 bg-destructive/5",
    text: "text-destructive",
    icon_: "text-destructive",
  },
  important: {
    icon: AlertTriangle,
    box: "border-amber-500/30 bg-amber-500/5",
    text: "text-amber-700 dark:text-amber-300",
    icon_: "text-amber-600 dark:text-amber-400",
  },
  warning: {
    icon: CircleAlert,
    box: "border-amber-500/20 bg-amber-500/[0.04]",
    text: "text-amber-700 dark:text-amber-300",
    icon_: "text-amber-600 dark:text-amber-400",
  },
  notice: {
    icon: Info,
    box: "border-border bg-muted/40",
    text: "text-muted-foreground",
    icon_: "text-muted-foreground",
  },
};

/** 把 Unix 秒转成「X 分钟前 / X 小时前 …」，0 表示未知。 */
export function formatAlertSince(since: number, now: number): string {
  if (!since) return "";
  const diff = Math.max(0, now - since);
  if (diff < 60) return "刚刚";
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  return `${Math.floor(diff / 86400)} 天前`;
}

export interface AccountAlertsCardProps {
  alerts: AccountAlert[];
  /** 每秒刷新的当前 Unix 秒（相对时间文案用）。 */
  now: number;
  /** 账号 id → 展示名（昵称优先，没有就用 id）。 */
  nameOf?: (id: string) => string;
  /** 没有告警时整卡不渲染 —— 「一切正常」不需要占一块版面。 */
}

export function AccountAlertsCard({ alerts, now, nameOf }: AccountAlertsCardProps) {
  if (alerts.length === 0) return null;
  return (
    <Card className="mb-5 gap-0 overflow-hidden py-0">
      <div className="flex items-center gap-2 border-b border-border/60 px-4 py-2.5">
        <AlertTriangle className="size-3.5 text-amber-600 dark:text-amber-400" aria-hidden="true" />
        <span className="text-sm font-medium">账号告警</span>
        <span className="tabular-nums text-xs text-muted-foreground">{alerts.length}</span>
        <span className="ml-auto text-xs text-muted-foreground">按严重度排序</span>
      </div>
      <ul className="divide-y divide-border/50">
        {alerts.map((alert) => {
          const style = SEVERITY_STYLE[alert.severity] ?? SEVERITY_STYLE.notice;
          const Icon = style.icon;
          const name = nameOf ? nameOf(alert.account) : alert.account;
          return (
            <li
              key={`${alert.account}-${alert.state}`}
              className={cn("flex items-start gap-3 px-4 py-2.5 text-xs", style.box)}
            >
              <Icon className={cn("mt-0.5 size-3.5 shrink-0", style.icon_)} aria-hidden="true" />
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
                  <span className={cn("font-medium", style.text)}>{alert.label}</span>
                  <span className="truncate font-medium text-foreground">{name}</span>
                  {alert.since ? (
                    <span className="text-muted-foreground">
                      自 {formatAlertSince(alert.since, now)}
                    </span>
                  ) : null}
                </div>
                <p className="mt-0.5 leading-5 text-muted-foreground">{alert.message}</p>
              </div>
            </li>
          );
        })}
      </ul>
    </Card>
  );
}
