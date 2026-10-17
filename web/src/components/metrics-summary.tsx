import { Activity, Ban, Clock, Coins, Layers, ShieldCheck } from "lucide-react";

import { MetricRow } from "@/components/section";
import { StatTile } from "@/components/stats-bar";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { formatCredits } from "@/lib/format";
import type { MetricsSummary as MetricsSummaryData } from "@/lib/types";

/**
 * 汇总卡上的「剩余积分」：全部账号都是不限量时显示「不限」而不是 0。
 *
 * 不限量账号不计入 credits_remaining（它的数值恒为 0），若照常格式化，
 * 全企业号部署会显示成「剩余 0 积分 / 来自 N 个已查额度账号」——
 * 读起来就是额度耗尽，而这正是要避免的误读。
 */
export function creditsLabel(data: MetricsSummaryData | null): string {
  const known = data?.quota_known ?? 0;
  const unlimited = data?.unlimited ?? 0;
  if (known > 0 && unlimited >= known) return "不限";
  return formatCredits(data?.credits_remaining ?? 0);
}

/** 与上面配套的说明文案：混合池要点明「其中 N 个不限量，未计入合计」。 */
export function creditsHint(data: MetricsSummaryData | null): string {
  const known = data?.quota_known ?? 0;
  const unlimited = data?.unlimited ?? 0;
  if (known === 0) return "尚未查询额度";
  if (unlimited >= known) return `${known} 个账号为不限量（企业版），没有「剩余」概念`;
  if (unlimited > 0) {
    return `来自 ${known} 个已查额度的账号（其中 ${unlimited} 个不限量，未计入合计）`;
  }
  return `来自 ${known} 个已查额度的账号`;
}

export interface MetricsSummaryProps {
  summary: MetricsSummaryData | null;
  loading: boolean;
}

/** 监控页汇总条：6 格指标，视觉沿用账号页 StatsBar。 */
export function MetricsSummary({ summary, loading }: MetricsSummaryProps) {
  if (loading && !summary) {
    return (
      <Card className="gap-0 overflow-hidden rounded-xl py-0 shadow-none">
        <MetricRow cols={6}>
          {[0, 1, 2, 3, 4, 5].map((index) => (
            <div key={index} className="px-4 py-5 sm:py-3">
              <Skeleton className="mx-auto h-4 w-20" />
              <Skeleton className="mx-auto mt-3 h-8 w-24" />
              <Skeleton className="mx-auto mt-1 h-3 w-28" />
            </div>
          ))}
        </MetricRow>
      </Card>
    );
  }

  const data = summary;
  const total = data?.total ?? 0;
  const inFlight = data?.in_flight ?? 0;

  return (
    <Card className="gap-0 overflow-hidden rounded-xl py-0 shadow-none">
      <MetricRow cols={6}>
        <StatTile
          label="账号总数"
          value={String(total)}
          hint="凭据文件总数"
          icon={Layers}
          tone="default"
        />
        <StatTile
          label="可用"
          value={String(data?.active ?? 0)}
          hint="可正常参与调度"
          icon={ShieldCheck}
          tone="success"
          divided
        />
        <StatTile
          label="冷却"
          value={String(data?.cooldown ?? 0)}
          hint="限流 / 熔断冷却中"
          icon={Clock}
          tone="warning"
          divided
        />
        <StatTile
          label="禁用"
          value={String(data?.disabled ?? 0)}
          hint="已从池中禁用"
          icon={Ban}
          tone="danger"
          divided
        />
        <StatTile
          label="在途请求"
          value={String(inFlight)}
          hint={inFlight > 0 ? "正在处理的上游请求" : "当前空闲"}
          icon={Activity}
          tone={inFlight > 0 ? "success" : "default"}
          divided
        />
        <StatTile
          label="剩余积分"
          value={creditsLabel(data)}
          hint={creditsHint(data)}
          icon={Coins}
          tone="default"
          divided
        />
      </MetricRow>
    </Card>
  );
}
