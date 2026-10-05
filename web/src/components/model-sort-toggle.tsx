import { ArrowDownAZ, CircleHelp, TrendingDown, type LucideIcon } from "lucide-react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { ModelSort } from "@/lib/model-view";
import { cn } from "@/lib/utils";

const OPTIONS: { value: ModelSort; label: string; hint: string; icon: LucideIcon }[] = [
  {
    value: "unknown-first",
    label: "未确认优先",
    hint: "倍率未确认的最前 —— 它们是唯一需要人工探测的一批，埋在后面会找不到；其次按请求数，再按 id",
    icon: CircleHelp,
  },
  {
    value: "cheapest",
    label: "低价优先",
    hint: "按已确认站点中的最低倍率升序，未确认的垫底。网关默认倾向便宜的站点，最低倍率就是实际会付的价",
    icon: TrendingDown,
  },
  { value: "name", label: "按模型名", hint: "按模型名排序，中文按拼音", icon: ArrowDownAZ },
];

export interface ModelSortToggleProps {
  value: ModelSort;
  onChange: (value: ModelSort) => void;
}

/** 模型列表排序切换：未知价格优先（默认）/ 按模型名。 */
export function ModelSortToggle({ value, onChange }: ModelSortToggleProps) {
  return (
    <div
      role="group"
      aria-label="排序方式"
      className="flex items-center gap-0.5 rounded-lg bg-foreground/[0.05] p-0.5"
    >
      {OPTIONS.map(({ value: option, label, hint, icon: Icon }) => {
        const active = option === value;
        return (
          <Tooltip key={option}>
            <TooltipTrigger asChild>
              <button
                type="button"
                aria-pressed={active}
                onClick={() => onChange(option)}
                className={cn(
                  "inline-flex cursor-pointer items-center gap-1.5 rounded-md px-2.5 py-1 text-xs outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50",
                  active
                    ? "bg-background font-medium text-foreground shadow-xs"
                    : "text-muted-foreground hover:text-foreground",
                )}
              >
                <Icon className="size-3.5" aria-hidden="true" />
                {label}
              </button>
            </TooltipTrigger>
            <TooltipContent side="top">{hint}</TooltipContent>
          </Tooltip>
        );
      })}
    </div>
  );
}
