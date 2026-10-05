import { useLayoutEffect, useMemo, useRef } from "react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { formatTokenCompact, formatTokenExact } from "@/lib/format";
import type { TokenGroup } from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * Token 活动热力图：53 周 × 7 天，一天一格。版式对齐 wb-switch 的 Heatmap。
 *
 * 为什么是 53 周而不是「最近 30 天」：按天趋势图看的是量级，而「我什么时候在用」
 * 是另一类问题 —— 停更的那几天、只在周末用，只有在一年尺度上才看得出来。
 * 因此**数据必须传全年**（调用方一次拉 365 天），不能传趋势窗口切片。
 *
 * 行序以**周日**开头（与 switch 的 `today - getDay()` 一致），不是周一。
 *
 * 色阶用 `--primary` 的四档透明度而不是四个色相：同一含义的深浅比换色相更容易读。
 * 注意这里调透明度是给**填充**用的（可读性契约禁止的是给文字降透明度）。
 */

/** 色阶：0 值用最淡的一档，其余按 sqrt 分四档。 */
const LEVEL_CLASS = [
  "bg-muted/70",
  "bg-primary/20",
  "bg-primary/40",
  "bg-primary/65",
  "bg-primary",
] as const;

const WEEKS = 53;

interface Cell {
  key: string;
  date: Date;
  label: string;
  value: number;
  records: number;
  level: number;
  week: number;
  day: number;
  future: boolean;
}

function startOfLocalDay(d: Date): Date {
  const copy = new Date(d);
  copy.setHours(12, 0, 0, 0);
  return copy;
}

function keyOf(d: Date): string {
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${d.getFullYear()}-${m}-${day}`;
}

function formatHeatmapDate(d: Date): string {
  return `${d.getMonth() + 1}月${d.getDate()}日`;
}

export function TokenHeatmap({ daily }: { daily: TokenGroup[] }) {
  const byDate = useMemo(() => {
    const m = new Map<string, TokenGroup>();
    for (const d of daily ?? []) m.set(d.key, d);
    return m;
  }, [daily]);

  const { cells, monthLabels, activeDays } = useMemo(() => {
    const today = startOfLocalDay(new Date());
    const todayKey = keyOf(today);
    // 从「本周周日」往前 52 周，凑成 53 列（最后一列是本周）。
    const start = new Date(today);
    start.setDate(start.getDate() - start.getDay() - (WEEKS - 1) * 7);

    const out: Cell[] = [];
    let peak = 0;
    let active = 0;

    for (let w = 0; w < WEEKS; w++) {
      for (let d = 0; d < 7; d++) {
        const date = new Date(start);
        date.setDate(start.getDate() + w * 7 + d);
        const key = keyOf(date);
        const hit = byDate.get(key);
        const value = hit?.total ?? 0;
        const future = key > todayKey;
        if (!future && value > 0) active++;
        if (value > peak) peak = value;
        out.push({
          key,
          date,
          label: formatHeatmapDate(date),
          value,
          records: hit?.records ?? 0,
          level: 0,
          week: w,
          day: d,
          future,
        });
      }
    }
    for (const c of out) {
      if (c.future || c.value <= 0) {
        c.level = 0;
      } else {
        // sqrt 分档：token 量级跨度极大，线性分档会让绝大多数格子挤在最浅一档。
        c.level = Math.min(4, Math.max(1, Math.ceil(Math.sqrt(c.value / Math.max(1, peak)) * 4)));
      }
    }

    // 月份标签：取「当周内出现的 1 号」；第一周没有 1 号就退回那一周的第一天。
    const months = Array.from({ length: WEEKS }, (_, w) => {
      const week = out.slice(w * 7, w * 7 + 7);
      const firstOfMonth = week.find((day) => day.date.getDate() === 1);
      let labelDate: Date | null = null;
      if (firstOfMonth && firstOfMonth.key <= todayKey) {
        labelDate = firstOfMonth.date;
      } else if (w === 0) {
        labelDate = week[0]?.date ?? null;
      }
      if (!labelDate || keyOf(labelDate) > todayKey) return null;
      return `${labelDate.getMonth() + 1}月`;
    });

    return { cells: out, monthLabels: months, activeDays: active };
  }, [byDate]);

  const scrollerRef = useRef<HTMLDivElement>(null);
  // 默认滚到最右（最近的时间），否则一年前的那一周先入眼，看到的是空档。
  useLayoutEffect(() => {
    const el = scrollerRef.current;
    if (el) el.scrollLeft = el.scrollWidth - el.clientWidth;
  }, [daily]);

  return (
    <div className="min-w-0">
      <div ref={scrollerRef} className="overflow-x-auto pb-1">
        <div
          className="min-w-[760px]"
          role="img"
          aria-label={`最近一年每日 Token 活动热力图，共 ${activeDays} 个活跃日`}
        >
          <div
            className="grid gap-1"
            style={{ gridTemplateColumns: `repeat(${WEEKS}, minmax(10px, 1fr))` }}
          >
            {cells.map((c) =>
              c.future ? (
                <div
                  key={c.key}
                  className="aspect-square min-w-0 rounded-[3px] opacity-0"
                  style={{ gridColumn: c.week + 1, gridRow: c.day + 1 }}
                />
              ) : (
                <Tooltip key={c.key} disableHoverableContent>
                  <TooltipTrigger asChild>
                    <div
                      className={cn("aspect-square min-w-0 rounded-[3px]", LEVEL_CLASS[c.level])}
                      style={{ gridColumn: c.week + 1, gridRow: c.day + 1 }}
                      aria-label={`${c.label}使用了 ${formatTokenExact(c.value)} 个 Token`}
                    />
                  </TooltipTrigger>
                  <TooltipContent
                    side="top"
                    sideOffset={7}
                    className="pointer-events-none rounded-lg bg-foreground px-2.5 py-1.5 text-xs leading-4 text-background shadow-md"
                  >
                    {c.label} 使用了 {formatTokenCompact(c.value)} 个 Token
                    {c.records > 0 ? ` · ${formatTokenExact(c.records)} 次调用` : ""}
                  </TooltipContent>
                </Tooltip>
              ),
            )}
          </div>
          <div
            className="mt-3 grid gap-1 text-xs text-muted-foreground"
            style={{ gridTemplateColumns: `repeat(${WEEKS}, minmax(10px, 1fr))` }}
          >
            {monthLabels.map((label, index) => (
              <span key={`${index}-${label ?? "empty"}`} className="whitespace-nowrap">
                {label}
              </span>
            ))}
          </div>
        </div>
      </div>
      {/* 星期的位置提示：只有左侧几个位置容易被认成「行号」 */}
      <div className="sr-only">行按周日到周六排列，共 {WEEKS} 列（每列一周）。</div>
    </div>
  );
}
