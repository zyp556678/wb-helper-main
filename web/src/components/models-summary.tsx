import { CircleHelp, Coins, Gift, Layers, TrendingUp } from "lucide-react";

import { MetricRow } from "@/components/section";
import { StatTile } from "@/components/stats-bar";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import type { PriceTierKey } from "@/lib/model-view";

export interface ModelsSummaryProps {
  /** 目录中的模型总数。 */
  modelCount: number;
  /** 按计费档位的计数（六个桶互不重叠）。 */
  counts: Record<PriceTierKey, number>;
  loading: boolean;
}

/**
 * 模型页汇总条：一条卡片切 N 份 + 竖线分隔。
 *
 * 口径说明（改之前先读）：这里的六个档位桶是**按模型**数的，且互不重叠 ——
 * 合计等于模型总数。不要和 `probe_summary` 混起来用，那个是按
 * **(站点 × 模型)** 条目数的，单位不同、基数也不同（只统计已探测过的条目）。
 * 探测汇总改在下方「关于倍率与价格判定」里按原口径说明。
 */
export function ModelsSummary({ modelCount, counts, loading }: ModelsSummaryProps) {
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
        <StatTile
          label="模型总数"
          value={String(modelCount)}
          hint="目录中的模型数量"
          icon={Layers}
          tone="default"
        />
        <StatTile
          label="免费档"
          value={String(counts.free)}
          hint="倍率 0.00x，不扣额度"
          icon={Gift}
          tone="success"
          divided
        />
        <StatTile
          label="低价档"
          value={String(counts["ultra-low"] + counts.low)}
          hint="≤ 0.30x"
          icon={Coins}
          tone="success"
          divided
        />
        <StatTile
          label="中价档"
          value={String(counts.mid)}
          hint="0.31x – 1.00x"
          icon={Coins}
          tone="warning"
          divided
        />
        <StatTile
          label="高价档"
          value={String(counts.high)}
          hint="> 1.00x"
          icon={TrendingUp}
          tone="danger"
          divided
        />
        <StatTile
          label="倍率未确认"
          value={String(counts.unknown)}
          hint="两个站点都还没拿到可比倍率"
          icon={CircleHelp}
          tone="default"
          divided
        />
      </MetricRow>
    </Card>
  );
}
