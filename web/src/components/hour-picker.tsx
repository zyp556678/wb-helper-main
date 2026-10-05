import { cn } from "@/lib/utils";

const HOURS = Array.from({ length: 24 }, (_, hour) => hour);

export interface HourPickerProps {
  /** 已选小时（0-23）。 */
  value: number[];
  onChange: (next: number[]) => void;
  disabled?: boolean;
  id?: string;
}

/** 0-23 小时多选：toggle chip 组，选中项填色，右下角同步显示已选摘要。 */
export function HourPicker({ value, onChange, disabled, id }: HourPickerProps) {
  const selected = new Set(value);

  const toggle = (hour: number) => {
    const next = new Set(selected);
    if (next.has(hour)) next.delete(hour);
    else next.add(hour);
    onChange([...next].sort((a, b) => a - b));
  };

  return (
    <div id={id} className="w-[336px] max-w-full">
      <div className="grid grid-cols-8 gap-1 sm:grid-cols-12">
        {HOURS.map((hour) => {
          const active = selected.has(hour);
          return (
            <button
              key={hour}
              type="button"
              aria-pressed={active}
              disabled={disabled}
              onClick={() => toggle(hour)}
              className={cn(
                "h-7 cursor-pointer rounded-md border text-xs tabular-nums outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50",
                active
                  ? "border-primary bg-primary/15 font-medium text-primary-ink"
                  : "border-border text-muted-foreground hover:border-foreground/25 hover:text-foreground",
              )}
            >
              {hour}
            </button>
          );
        })}
      </div>
      <div className="mt-1.5 text-xs text-muted-foreground">
        {value.length > 0 ? (
          <>
            已选 <span className="tabular-nums text-foreground">{value.join("、")}</span> 时
          </>
        ) : (
          "未选择任何小时"
        )}
      </div>
    </div>
  );
}
