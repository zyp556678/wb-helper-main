import { Clock } from "lucide-react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  COOLDOWN_KIND_LABEL,
  formatCountdown,
  formatCredits,
  formatDuration,
  formatRelativeTime,
  formatUnixSeconds,
  RUNTIME_STATE_LABEL,
} from "@/lib/format";
import type { AccountMetric } from "@/lib/types";
import { cn } from "@/lib/utils";

/** 站点标签：与账号卡片同口径，国内站走品牌绿，国际站走冷色。 */
function SiteBadge({ metric }: { metric: AccountMetric }) {
  const isIntl = metric.site === "intl";
  const label = metric.site_label || (isIntl ? "国际站" : "国内站");
  return (
    <Badge
      className={cn(
        "border-transparent",
        isIntl
          ? "bg-sky-500/15 text-sky-700 dark:bg-sky-500/20 dark:text-sky-300"
          : "bg-primary/12 text-primary-ink",
      )}
    >
      {label}
    </Badge>
  );
}

/** 冷却剩余时间：取 cooldown_until / breaker_until 的较大者。 */
function cooldownRemaining(metric: AccountMetric, now: number): number {
  const until = Math.max(metric.cooldown_until, metric.breaker_until);
  return until > now ? until - now : 0;
}

function StateCell({ metric, now }: { metric: AccountMetric; now: number }) {
  if (metric.state === "disabled") {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <Badge variant="danger" className="cursor-default">
            {RUNTIME_STATE_LABEL.disabled}
          </Badge>
        </TooltipTrigger>
        <TooltipContent side="top">该账号已在配置中禁用</TooltipContent>
      </Tooltip>
    );
  }

  if (metric.state === "cooldown") {
    const remaining = cooldownRemaining(metric, now);
    const kindLabel =
      metric.cooldown_kind && metric.cooldown_kind in COOLDOWN_KIND_LABEL
        ? COOLDOWN_KIND_LABEL[metric.cooldown_kind]
        : "";
    const countdown = formatCountdown(remaining);
    const hint =
      metric.cooldown_reason ||
      (remaining > 0 ? `冷却剩余 ${formatDuration(remaining)}` : "冷却即将结束");

    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <Badge variant="warning" className="cursor-default gap-1.5">
            <Clock />
            {kindLabel ? `${RUNTIME_STATE_LABEL.cooldown} · ${kindLabel}` : RUNTIME_STATE_LABEL.cooldown}
            {countdown ? <span className="tabular-nums">（{countdown}）</span> : null}
          </Badge>
        </TooltipTrigger>
        <TooltipContent side="top">{hint}</TooltipContent>
      </Tooltip>
    );
  }

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="success" className="cursor-default">
          {RUNTIME_STATE_LABEL.available}
        </Badge>
      </TooltipTrigger>
      <TooltipContent side="top">可正常参与调度</TooltipContent>
    </Tooltip>
  );
}

function InFlightCell({ metric }: { metric: AccountMetric }) {
  const capped = metric.max_in_flight > 0;
  const saturated = capped && metric.in_flight >= metric.max_in_flight;
  return (
    <span
      className={cn(
        "tabular-nums",
        saturated ? "font-medium text-amber-700 dark:text-amber-300" : "text-foreground",
      )}
    >
      {metric.in_flight}/{capped ? metric.max_in_flight : "∞"}
    </span>
  );
}

export interface AccountStatusTableProps {
  accounts: AccountMetric[];
  /** 每秒刷新的当前 Unix 秒，用于倒计时本地递减。 */
  now: number;
}

/** 账号运行状态表：每行一个账号。 */
export function AccountStatusTable({ accounts, now }: AccountStatusTableProps) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>账号</TableHead>
          <TableHead>站点</TableHead>
          <TableHead>状态</TableHead>
          <TableHead>在途/上限</TableHead>
          <TableHead>成功/失败/连败</TableHead>
          <TableHead className="text-right">剩余积分</TableHead>
          <TableHead>上次使用</TableHead>
          <TableHead>最近错误</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {accounts.map((metric) => (
          <TableRow key={metric.id}>
            <TableCell>
              <div className="flex max-w-[260px] flex-col">
                <span className="truncate font-medium text-foreground">
                  {metric.nickname || "未命名账号"}
                </span>
                <span className="truncate font-mono text-xs text-muted-foreground">
                  {metric.id}
                </span>
              </div>
            </TableCell>
            <TableCell>
              <SiteBadge metric={metric} />
            </TableCell>
            <TableCell>
              <StateCell metric={metric} now={now} />
            </TableCell>
            <TableCell>
              <InFlightCell metric={metric} />
            </TableCell>
            <TableCell>
              <span className="tabular-nums">
                <span className="text-primary-ink">{metric.success_count}</span>
                <span aria-hidden className="px-1 text-muted-foreground">
                  /
                </span>
                <span className={metric.failure_count > 0 ? "text-destructive" : "text-muted-foreground"}>
                  {metric.failure_count}
                </span>
                <span aria-hidden className="px-1 text-muted-foreground">
                  /
                </span>
                <span
                  className={cn(
                    metric.fails > 0 ? "text-amber-700 dark:text-amber-300" : "text-muted-foreground",
                  )}
                >
                  {metric.fails}
                </span>
              </span>
            </TableCell>
            <TableCell className="text-right">
              {metric.credits_remaining === null ? (
                <span className="text-muted-foreground">未查询</span>
              ) : (
                <span className="tabular-nums text-foreground">
                  {formatCredits(metric.credits_remaining)}
                </span>
              )}
            </TableCell>
            <TableCell>
              <Tooltip>
                <TooltipTrigger asChild>
                  <span className="cursor-default text-foreground">
                    {formatRelativeTime(metric.last_used_at, now)}
                  </span>
                </TooltipTrigger>
                <TooltipContent side="top">
                  {formatUnixSeconds(metric.last_used_at) ?? "从未使用"}
                </TooltipContent>
              </Tooltip>
            </TableCell>
            <TableCell>
              {metric.last_error ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span className="block max-w-[240px] cursor-default truncate text-destructive">
                      {metric.last_error}
                    </span>
                  </TooltipTrigger>
                  <TooltipContent side="top" className="max-w-[420px] whitespace-pre-wrap break-words">
                    {metric.last_error}
                  </TooltipContent>
                </Tooltip>
              ) : (
                <span className="text-muted-foreground">—</span>
              )}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
