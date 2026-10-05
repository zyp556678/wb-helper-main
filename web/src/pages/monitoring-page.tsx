import { useCallback, useState } from "react";
import { CalendarCheck, Coins, Inbox, Loader2, MessageSquarePlus, PawPrint, RefreshCw } from "lucide-react";
import {
  notifySuccess,
  notifyError,
} from "@/lib/notify";

import { AccountStatusTable } from "@/components/account-status-table";
import { MetricsSummary } from "@/components/metrics-summary";
import { ModelStatsTable } from "@/components/model-stats-table";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { describeError, runSchedulerTask } from "@/lib/api";
import { formatDuration } from "@/lib/format";
import { useMetrics } from "@/lib/use-metrics";
import { useNowSeconds } from "@/lib/use-now-seconds";
import type { SchedulerTask } from "@/lib/types";
import { cn } from "@/lib/utils";

export function MonitoringPage() {
  const { metrics, loading, error, unauthorized, reload } = useMetrics();

  // 每秒本地递减，保证冷却倒计时在两次轮询之间也持续更新。
  const now = useNowSeconds();
  const [pending, setPending] = useState<SchedulerTask | null>(null);

  const firstLoad = loading && metrics === null;
  const accounts = metrics?.accounts ?? [];
  const models = metrics?.models ?? [];
  const sticky = metrics?.sticky ?? null;

  const handleRun = useCallback(
    async (task: SchedulerTask) => {
      setPending(task);
      try {
        const result = await runSchedulerTask(task);
        if (result.ok) {
          notifySuccess(result.detail || "任务已完成");
        } else {
          notifyError(result.detail || "任务执行失败");
        }
        reload();
      } catch (err) {
        notifyError(describeError(err));
      } finally {
        setPending(null);
      }
    },
    [reload],
  );

  const busy = pending !== null;

  return (
    <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
      <header className="mb-6 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">监控</h1>
          <p className="mt-2 text-sm leading-6 text-muted-foreground">
            账号池实时运行状态、模型请求统计与会话粘性快照，每 5 秒自动刷新。
          </p>
        </div>
        <div className="flex shrink-0 flex-wrap items-center justify-end gap-1.5 pt-1">
          <Button
            type="button"
            size="sm"
            disabled={busy}
            onClick={() => void handleRun("checkin")}
          >
            {pending === "checkin" ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <CalendarCheck className="size-3.5" aria-hidden="true" />
            )}
            立即签到
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={() => void handleRun("balance")}
          >
            {pending === "balance" ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <Coins className="size-3.5" aria-hidden="true" />
            )}
            刷新余额
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={() => void handleRun("travel")}
          >
            {pending === "travel" ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <PawPrint className="size-3.5" aria-hidden="true" />
            )}
            旅行巡检
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={() => void handleRun("activity")}
          >
            {pending === "activity" ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <MessageSquarePlus className="size-3.5" aria-hidden="true" />
            )}
            补活跃
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            disabled={loading}
            onClick={reload}
            aria-label="刷新运行指标"
          >
            <RefreshCw className={cn("size-3.5", loading && "animate-spin")} aria-hidden="true" />
            刷新
          </Button>
        </div>
      </header>

      {error ? (
        <Card className="mb-5 gap-3 border-destructive/30 bg-destructive/5 p-4">
          <div className="text-sm font-medium text-destructive">读取失败</div>
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

      <MetricsSummary summary={metrics?.summary ?? null} loading={firstLoad} />

      <section className="mt-5" aria-label="账号运行状态">
        <div className="flex items-center justify-between gap-3">
          <h2 className="text-sm font-medium text-muted-foreground">
            账号运行状态
            {accounts.length > 0 ? (
              <span className="ml-1.5 tabular-nums text-muted-foreground">
                {accounts.length}
              </span>
            ) : null}
          </h2>
          {sticky ? (
            <span className="text-xs text-muted-foreground">
              会话粘性：{sticky.enabled ? "已启用" : "已关闭"} · 当前绑定{" "}
              <span className="tabular-nums">{sticky.bindings}</span> 个会话 · TTL{" "}
              {formatDuration(sticky.ttl_seconds)}
            </span>
          ) : null}
        </div>
        <Card className="mt-3 gap-0 overflow-hidden py-0">
          {firstLoad ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2].map((index) => (
                <Skeleton key={index} className="h-9 rounded-md" />
              ))}
            </div>
          ) : accounts.length === 0 ? (
            <div className="flex flex-col items-center gap-2 py-12 text-center">
              <Inbox className="size-6 text-muted-foreground" aria-hidden="true" />
              <div className="text-sm font-medium">暂无账号运行数据</div>
              <p className="text-xs text-muted-foreground">网关还没有可用账号，先到「账号」页添加。</p>
            </div>
          ) : (
            <AccountStatusTable accounts={accounts} now={now} />
          )}
        </Card>
      </section>

      <section className="mt-5" aria-label="模型统计">
        <h2 className="text-sm font-medium text-muted-foreground">
          模型统计
          {models.length > 0 ? (
            <span className="ml-1.5 tabular-nums text-muted-foreground">{models.length}</span>
          ) : null}
        </h2>
        <Card className="mt-3 gap-0 overflow-hidden py-0">
          {firstLoad ? (
            <div className="space-y-2 p-4">
              {[0, 1].map((index) => (
                <Skeleton key={index} className="h-9 rounded-md" />
              ))}
            </div>
          ) : models.length === 0 ? (
            <div className="flex flex-col items-center gap-2 py-10 text-center">
              <Inbox className="size-6 text-muted-foreground" aria-hidden="true" />
              <div className="text-sm font-medium">暂无模型调用记录</div>
              <p className="text-xs text-muted-foreground">有请求经过网关后这里会显示统计。</p>
            </div>
          ) : (
            <ModelStatsTable models={models} />
          )}
        </Card>
      </section>
    </div>
  );
}
