import { cn } from "@/lib/utils";
import { formatTokenCompact, formatTokenExact, tokenPercentage } from "@/lib/format";
import type { TokenTotals } from "@/lib/types";

/**
 * 构成条：一条横条把总量切成「缓存命中 / 新增输入 / 输出 / 缓存写入」四段。
 *
 * 版式对齐 wb-switch 的 CompactComposition。为什么用横条而不是四个数字：
 * 这一块回答的是「这些 token 花在哪了」，四段长度的对比比四个数值更快。
 *
 * 一处**刻意偏离 switch**：它的图例用 `text-[10px]`，本项目有「最小 12px」的
 * 可读性契约（中文在 10px 会糊），这里统一用 `text-xs`。其余（段序、配色、
 * 高度、圆角、**固定四段含 0 值段**）保持一致 —— 过滤掉 0 值段会让图例项
 * 随数据跳动，读者无法建立稳定的颜色对应关系。
 */
export function TokenComposition({ summary }: { summary: TokenTotals }) {
  const total = summary.total;
  const segments = [
    { key: "cacheRead", label: "缓存占比", value: summary.cacheRead, cls: "bg-primary" },
    { key: "uncachedInput", label: "新增", value: summary.uncachedInput, cls: "bg-sky-500" },
    { key: "output", label: "输出", value: summary.output, cls: "bg-violet-500" },
    { key: "cacheWrite", label: "写入", value: summary.cacheWrite, cls: "bg-amber-500" },
  ];

  const ariaLabel = segments
    .map((s) => `${s.label} ${tokenPercentage(s.value, total)}`)
    .join("，");

  return (
    <div className="min-w-0 max-w-[360px] flex-1">
      <div
        className="flex h-2 w-full overflow-hidden rounded-full bg-muted"
        role="img"
        aria-label={ariaLabel || "暂无 Token 构成"}
      >
        {segments.map((s) => (
          <div
            key={s.key}
            className={cn("h-full", s.cls)}
            style={{ width: total > 0 ? `${(s.value / total) * 100}%` : "0%" }}
            title={`${s.label} ${formatTokenCompact(s.value)} · ${tokenPercentage(s.value, total)}`}
            aria-label={`${s.label} ${formatTokenExact(s.value)} Token，${tokenPercentage(s.value, total)}`}
          />
        ))}
      </div>
      <div className="mt-1 flex flex-wrap gap-x-2.5 gap-y-0.5 text-xs text-muted-foreground">
        {segments.map((s) => (
          <span key={s.key} className="inline-flex items-center gap-1">
            <span className={cn("size-1.5 shrink-0 rounded-full", s.cls)} aria-hidden="true" />
            {s.label} {tokenPercentage(s.value, total)}
          </span>
        ))}
      </div>
    </div>
  );
}
