import { Clock, Coins, Layers, PauseCircle, ShieldCheck, Users, type LucideIcon } from "lucide-react";

import { Card } from "@/components/ui/card";
import { MetricRow, StatMetric } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import { formatCredits } from "@/lib/format";
import { cn } from "@/lib/utils";

export interface StatTileProps {
  label: string;
  value: string;
  hint: string;
  icon: LucideIcon;
  tone: "default" | "success" | "danger" | "warning";
  /** 除第一格外都传 true：指标带靠竖线分列 */
  divided?: boolean;
}

const TONE_CLASS: Record<StatTileProps["tone"], string> = {
  default: "text-foreground",
  success: "text-primary-ink",
  danger: "text-destructive",
  warning: "text-amber-700 dark:text-amber-300",
};

/**
 * 汇总指标格。
 *
 * 现在只是 `StatMetric` 的一层薄封装（保留 tone 与 icon 的旧签名，调用点不用改）——
 * 原实现是「每格一个带边框的圆角小盒」，一条指标带会碎成五个盒子；
 * 与 switch 一致地改成「一条卡片切 N 份 + 竖线分隔」。
 */
export function StatTile({ label, value, hint, icon, tone, divided }: StatTileProps) {
  return (
    <StatMetric
      icon={icon}
      label={label}
      value={value}
      hint={hint}
      divided={divided}
      valueClassName={cn(TONE_CLASS[tone])}
    />
  );
}

export interface StatsBarProps {
  total: number;
  active: number;
  disabled: number;
  /** 暂停选号：只不派发，维护任务照常 —— 与 disabled 是两种状态，必须分列。 */
  paused: number;
  cooldown: number;
  creditsRemaining: number;
  quotaKnown: number;
  loading: boolean;
}

export function StatsBar({
  total,
  active,
  disabled,
  paused,
  cooldown,
  creditsRemaining,
  quotaKnown,
  loading,
}: StatsBarProps) {
  if (loading) {
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

  return (
    <Card className="gap-0 overflow-hidden rounded-xl py-0 shadow-none">
      <MetricRow cols={6}>
        <StatTile label="总数" value={String(total)} hint="凭据文件总数" icon={Layers} tone="default" />
        <StatTile
          label="可用"
          value={String(active)}
          hint="可正常使用"
          icon={ShieldCheck}
          tone="success"
          divided
        />
        <StatTile label="失效" value={String(disabled)} hint="已禁用" icon={Users} tone="danger" divided />
        <StatTile
          label="暂停选号"
          value={String(paused)}
          hint="不派发，维护任务照常"
          icon={PauseCircle}
          tone="warning"
          divided
        />
        <StatTile
          label="冷却"
          value={String(cooldown)}
          hint="限流冷却中"
          icon={Clock}
          tone="warning"
          divided
        />
        <StatTile
          label="剩余积分"
          value={formatCredits(creditsRemaining)}
          hint={quotaKnown > 0 ? `来自 ${quotaKnown} 个已查额度的账号` : "尚未查询额度"}
          icon={Coins}
          tone="default"
          divided
        />
      </MetricRow>
    </Card>
  );
}
