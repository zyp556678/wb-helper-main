import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";

import { Card, CardDescription, CardHeader } from "@/components/ui/card";
import { cn } from "@/lib/utils";

/**
 * 区块排版原语，对齐 wb-switch 的三个约定：
 *
 * 1. **标题在卡片外、上方**，`text-[13px] font-medium`。层级靠字号表达而不是字重 ——
 *    标题比卡片内容还抢眼时，扫读时会先撞上标题而不是数据。
 * 2. **卡片自身的标题位留给说明文字**（`CardDescription`，12px muted），
 *    真正的区块名交给上面那个 h2，避免同一层级出现两个"标题"。
 * 3. 操作控件（范围切换、筛选）放卡片**内右上**，与说明同行两端对齐。
 *
 * 这套结构在 switch 的 Token 统计 / 积分统计 / 设置三页完全一致，
 * 因此抽成原语而不是各页手写 —— 否则每次加区块都要重新决定一遍间距与字号。
 */

export function SectionTitle({
  id,
  children,
  action,
}: {
  id: string;
  children: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-wrap items-center justify-between gap-3 px-1">
      <h2 id={id} className="text-[13px] font-medium leading-5">
        {children}
      </h2>
      {action}
    </div>
  );
}

export interface SectionProps {
  /** 作为 h2 的 id，同时用于 section 的 aria-labelledby */
  id: string;
  title: ReactNode;
  /** 卡片左上角的说明文字；不用它承担"区块标题"职责 */
  description?: ReactNode;
  /** 卡片右上角的操作区 */
  action?: ReactNode;
  children: ReactNode;
  className?: string;
  /** 少数区块需要不同圆角/内边距时覆盖卡片类名 */
  cardClassName?: string;
  /** 无说明也无操作时不渲染 CardHeader（省掉一段空 padding） */
  bare?: boolean;
}

export function Section({
  id,
  title,
  description,
  action,
  children,
  className,
  cardClassName,
  bare = false,
}: SectionProps) {
  const hasHeader = !bare && (description != null || action != null);
  return (
    <section className={cn("min-w-0 space-y-2.5", className)} aria-labelledby={id}>
      <SectionTitle id={id}>{title}</SectionTitle>
      <Card
        className={cn("min-w-0 gap-0 overflow-hidden rounded-xl py-0 shadow-none", cardClassName)}
      >
        {hasHeader ? (
          <CardHeader className="gap-0 px-4 pt-3 pb-0 sm:px-5">
            <div className="flex min-w-0 flex-wrap items-center justify-between gap-3">
              {description != null ? (
                <CardDescription className="min-w-0 text-xs">{description}</CardDescription>
              ) : (
                <span />
              )}
              {action != null ? (
                <div className="flex min-w-0 flex-wrap items-center gap-2">{action}</div>
              ) : null}
            </div>
          </CardHeader>
        ) : null}
        {children}
      </Card>
    </section>
  );
}

export interface StatMetricProps {
  icon: LucideIcon;
  label: string;
  value: string;
  /** 数值下方的一行补充说明（switch 的指标格没有，用于承载"口径/来源"信息） */
  hint?: string;
  /**
   * 说明文字允许换行。
   *
   * 默认 `truncate`（一行截断）是为了让四格指标高度一致；但像「差异」那种
   * 「官方记的更多：可能有网关外的消耗或漏记」的**解释性文案**被截掉就失去意义了，
   * 那类格子必须换行显示。
   */
  wrapHint?: boolean;
  /** 除第一格外都传 true：靠竖线分列，而不是每格各画一个盒子 */
  divided?: boolean;
  /** 数值颜色覆盖（如红色告警） */
  valueClassName?: string;
}

/**
 * 统计指标格：图标+标签在上、数值在下、整体居中。
 *
 * 与 switch 一致地**不要盒中盒** —— 一条带边框的卡片切成四份（`MetricRow`），
 * 每格只靠一条竖线分隔。每格各画一个圆角小盒会让这条指标带显得零碎。
 */
export function StatMetric({
  icon: Icon,
  label,
  value,
  hint,
  wrapHint = false,
  divided = false,
  valueClassName,
}: StatMetricProps) {
  return (
    <div
      className={cn(
        "flex min-w-0 flex-col items-center justify-center px-4 py-5 text-center sm:py-3",
        divided && "sm:border-l sm:border-border/60",
      )}
    >
      <div className="flex max-w-full items-center justify-center gap-2 text-[13px] font-medium leading-5 text-muted-foreground">
        <Icon className="size-4 shrink-0 stroke-[1.75]" aria-hidden="true" />
        <span className="truncate">{label}</span>
      </div>
      <div
        className={cn(
          "font-display mt-3 max-w-full truncate text-[26px] font-semibold leading-8 tracking-[-0.025em] tabular-nums",
          valueClassName ?? "text-foreground",
        )}
      >
        {value}
      </div>
      {hint ? (
        <div
          className={cn(
            "mt-1 max-w-full text-xs text-muted-foreground",
            wrapHint ? "text-pretty" : "truncate",
          )}
        >
          {hint}
        </div>
      ) : null}
    </div>
  );
}

/**
 * 指标带的列数 → 类名映射。
 *
 * Tailwind 需要静态类名，所以不能写 `sm:grid-cols-${n}`；而且 `sm:grid-cols-4`
 * 与 `sm:grid-cols-5` 同时出现时谁生效取决于样式表顺序而不是类名顺序，必须二选一。
 */
const METRIC_COLS: Record<number, string> = {
  3: "sm:grid-cols-3",
  4: "sm:grid-cols-4",
  5: "sm:grid-cols-5",
  6: "sm:grid-cols-6",
};

/** 指标带容器：一条卡片横向切 N 份（窄屏竖排并恢复分隔线）。 */
export function MetricRow({
  children,
  cols = 4,
  className,
}: {
  children: ReactNode;
  cols?: number;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "grid min-w-0 grid-cols-1 divide-y divide-border/60 p-0 sm:divide-y-0 sm:py-5",
        METRIC_COLS[cols] ?? METRIC_COLS[4],
        className,
      )}
    >
      {children}
    </div>
  );
}

/**
 * 范围 / 视图切换的药丸组。
 *
 * switch 全站有三种实现（Radix Tabs、手写 aria-pressed、手写 role=tablist），
 * 视觉相同而代码三份；这里统一成一个原语，避免以后新增一处又抄一遍。
 */
export function PillGroup<T extends string>({
  value,
  options,
  onChange,
  ariaLabel,
  className,
}: {
  value: T;
  /** `disabled` 用于「这一项当前没有数据」：置灰但仍可见，让人知道有这条来源。 */
  options: readonly { value: T; label: string; disabled?: boolean }[];
  onChange: (next: T) => void;
  ariaLabel: string;
  className?: string;
}) {
  return (
    <div
      className={cn("flex max-w-full flex-wrap gap-1 rounded-lg bg-muted p-1", className)}
      aria-label={ariaLabel}
    >
      {options.map((option) => (
        <button
          key={option.value}
          type="button"
          aria-pressed={value === option.value}
          disabled={option.disabled}
          className={cn(
            "cursor-pointer rounded-md px-2.5 py-1.5 text-xs transition-colors",
            option.disabled && "cursor-not-allowed opacity-50",
            value === option.value
              ? "bg-background font-medium text-foreground shadow-sm"
              : "text-muted-foreground hover:text-foreground",
          )}
          onClick={() => onChange(option.value)}
        >
          {option.label}
        </button>
      ))}
    </div>
  );
}

/** 统一的空状态块：不用留白，明确说明「为什么没有数据」。 */
export function EmptyHint({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-lg border border-dashed px-4 py-10 text-center text-sm text-muted-foreground">
      {children}
    </div>
  );
}

/** 占位说明：与空状态同色系但更轻，用于卡片内的一句话提示。 */
export function CardHint({ children }: { children: ReactNode }) {
  return <p className="px-1 text-xs leading-5 text-muted-foreground">{children}</p>;
}

