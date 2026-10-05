import * as React from "react";

import { cn } from "@/lib/utils";

export interface SwitchProps
  extends Omit<React.ComponentProps<"button">, "onChange" | "value" | "type"> {
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
}

/**
 * 轻量开关：语义等同 checkbox role="switch"，因项目未引入 @radix-ui/react-switch
 * 而自实现，避免为一个控件新增依赖。
 */
export function Switch({ checked, onCheckedChange, className, disabled, ...props }: SwitchProps) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      disabled={disabled}
      onClick={() => onCheckedChange(!checked)}
      className={cn(
        "relative inline-flex h-5 w-9 shrink-0 cursor-pointer items-center rounded-full outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:ring-offset-1 focus-visible:ring-offset-background disabled:cursor-not-allowed disabled:opacity-50",
        checked ? "bg-primary" : "bg-foreground/25",
        className,
      )}
      {...props}
    >
      <span
        aria-hidden
        className={cn(
          "pointer-events-none block size-4 rounded-full shadow-sm transition-transform",
          checked
            ? "translate-x-[18px] bg-background dark:bg-primary-foreground"
            : "translate-x-0.5 bg-background dark:bg-foreground",
        )}
      />
    </button>
  );
}
