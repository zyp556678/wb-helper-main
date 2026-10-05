import { cn } from "@/lib/utils";

/**
 * 排行行：序号 + 名称（可带一行补充说明）+ 数值 + 占比，下挂一条占比进度条。
 *
 * 对齐 wb-switch 的分布/排行区块 —— 用「进度条 + 占比」而不是表格：
 * 表格适合逐列比对精确值，而这里的问题是「谁占大头」，横向长度比数字更快。
 */
export interface RankingRow {
  key: string;
  title: string;
  /** 名称下方的补充说明（请求数、失败数等）；保留表格里的次要列 */
  subtitle?: string;
  value: number;
  valueText: string;
  /**
   * 数值的精确文案（如「1,234 Token」），作为 title/aria-label 挂在数值上。
   * compact 显示（1.2K）会丢精度，悬浮能读到原值（对齐 switch 的排行行）。
   */
  valueExact?: string;
  /** 有失败/异常时把补充说明染红 */
  danger?: boolean;
}

/** 占比：分母为 0 时返回 null，让调用方显示「—」而不是 NaN%。 */
export function share(part: number, total: number): number | null {
  if (total <= 0) return null;
  return (part / total) * 100;
}

export function formatShare(percent: number | null): string {
  if (percent === null) return "—";
  // 非零但小于 0.05% 时显示「<0.1%」：显示「0.0%」会被读成「没有消耗」，
  // 而这一行确实存在且非零（与 switch 的百分比口径一致）。
  if (percent > 0 && percent < 0.05) return "<0.1%";
  return `${percent.toFixed(1)}%`;
}

export function RankingRows({ rows, total }: { rows: RankingRow[]; total: number }) {
  return (
    <div className="space-y-3">
      {rows.map((row, index) => {
        const percent = share(row.value, total);
        return (
          <div key={row.key}>
            <div className="mb-1.5 flex min-w-0 items-start gap-3 text-xs">
              <span className="mt-0.5 w-5 shrink-0 font-mono text-muted-foreground">
                {String(index + 1).padStart(2, "0")}
              </span>
              <div className="min-w-0 flex-1">
                <div className="truncate font-medium" title={row.title}>
                  {row.title}
                </div>
                {row.subtitle ? (
                  <div
                    className={cn(
                      "mt-0.5 truncate text-xs tabular-nums",
                      row.danger ? "text-destructive" : "text-muted-foreground",
                    )}
                    title={row.subtitle}
                  >
                    {row.subtitle}
                  </div>
                ) : null}
              </div>
              <span
                className="mt-0.5 shrink-0 tabular-nums"
                title={row.valueExact}
                aria-label={row.valueExact}
              >
                {row.valueText}
              </span>
              <span className="mt-0.5 w-12 shrink-0 text-right text-muted-foreground tabular-nums">
                {formatShare(percent)}
              </span>
            </div>
            <div className="ml-8 h-1.5 overflow-hidden rounded-full bg-muted">
              <div
                className="h-full rounded-full bg-primary"
                style={{ width: percent === null ? "0%" : `${Math.min(percent, 100)}%` }}
              />
            </div>
          </div>
        );
      })}
    </div>
  );
}

/** 构成行：名称在左，数值与占比在右（同一行），下挂一条占比进度条。 */
export interface ShareRow {
  key: string;
  /** 左侧名称。 */
  title: string;
  /** 右侧主数值文案，如「329.83 积分」。 */
  valueText: string;
  /** 占比进度条的长度依据；同时用于算占比。 */
  value: number;
  /** 数值不可得（如官方用量取不到）时置灰显示 valueText，且不计入分母。 */
  unavailable?: boolean;
}

/**
 * 构成行（`ShareRows`）与排行行（`RankingRows`）的**区别是有意的**：
 *
 * 排行行带序号与一行副标题，适合「消耗最高的会话」这类需要额外上下文的榜单；
 * 而积分页的「账号消耗构成 / 按模型分类」只有名称与数值两列 —— 加序号会引入
 * 一个无人使用的视觉层级，把「谁占大头」这个唯一要读的信息压下去。
 * wb-switch 的积分页用的是后者，这里照同一形态实现。
 */
export function ShareRows({ rows }: { rows: ShareRow[] }) {
  // 分母只算可得的行：把「取不到」当成 0 计进分母会让所有占比集体偏小。
  const measurable = rows.filter((row) => !row.unavailable);
  const total = measurable.reduce((sum, row) => sum + row.value, 0);

  return (
    <div className="space-y-3">
      {rows.map((row) => {
        const percent = row.unavailable ? null : share(row.value, total);
        return (
          <div key={row.key} className="min-w-0">
            <div className="flex min-w-0 items-center justify-between gap-3 text-xs">
              <span className="min-w-0 truncate font-medium" title={row.title}>
                {row.title}
              </span>
              <span className="shrink-0 text-muted-foreground tabular-nums">
                {row.valueText}
                {percent === null ? null : (
                  <span className="ml-1.5 font-medium text-foreground">{formatShare(percent)}</span>
                )}
              </span>
            </div>
            <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted" aria-hidden="true">
              <div
                className="h-full rounded-full bg-primary/75"
                style={{ width: percent === null ? "0%" : `${Math.min(percent, 100)}%` }}
              />
            </div>
          </div>
        );
      })}
    </div>
  );
}
