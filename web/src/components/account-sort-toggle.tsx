import { ArrowDownAZ, Clock, type LucideIcon } from "lucide-react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { AccountSort } from "@/lib/format";
import { cn } from "@/lib/utils";

const OPTIONS: { value: AccountSort; label: string; hint: string; icon: LucideIcon }[] = [
  {
    value: "expiry",
    label: "按到期时间",
    hint: "Token 最快到期的排最前，便于及时换号或续期；未标注过期时间的排在最后",
    icon: Clock,
  },
  { value: "file", label: "按文件名", hint: "按凭据文件名排序", icon: ArrowDownAZ },
];

export interface AccountSortToggleProps {
  value: AccountSort;
  onChange: (value: AccountSort) => void;
}

/** 账号列表排序切换：按到期时间（默认）/ 按凭据文件名。 */
export function AccountSortToggle({ value, onChange }: AccountSortToggleProps) {
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
