import { Monitor, Moon, Sun, type LucideIcon } from "lucide-react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useThemePreference, type ThemePreference } from "@/lib/theme";
import { cn } from "@/lib/utils";

const OPTIONS: { value: ThemePreference; label: string; icon: LucideIcon }[] = [
  { value: "system", label: "跟随系统", icon: Monitor },
  { value: "light", label: "浅色", icon: Sun },
  { value: "dark", label: "深色", icon: Moon },
];

/**
 * 三态主题切换：system / light / dark。
 *
 * 偏好经 `useThemePreference` 订阅：配置页「外观」分组里也有同一个三态选择，
 * 两处状态必须同步（否则一处的激活态会停在旧值）。
 */
export function ThemeSwitcher() {
  const [preference, select] = useThemePreference();

  return (
    <div
      role="group"
      aria-label="主题"
      className="flex items-center gap-0.5 rounded-lg bg-foreground/[0.05] p-0.5"
    >
      {OPTIONS.map(({ value, label, icon: Icon }) => {
        const active = preference === value;
        return (
          <Tooltip key={value}>
            <TooltipTrigger asChild>
              <button
                type="button"
                aria-label={label}
                aria-pressed={active}
                onClick={() => select(value)}
                className={cn(
                  "inline-flex size-7 cursor-pointer items-center justify-center rounded-md outline-none transition-colors",
                  "focus-visible:ring-2 focus-visible:ring-sidebar-ring/50",
                  active
                    ? "bg-background text-foreground shadow-xs"
                    : "text-sidebar-foreground/70 hover:text-sidebar-foreground",
                )}
              >
                <Icon className="size-3.5" aria-hidden="true" />
              </button>
            </TooltipTrigger>
            <TooltipContent side="top">{label}</TooltipContent>
          </Tooltip>
        );
      })}
    </div>
  );
}
