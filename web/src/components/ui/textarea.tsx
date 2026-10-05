import * as React from "react";

import { cn } from "@/lib/utils";

/** 多行输入：默认等宽字体、纵向可拉伸，用于提示词等长文本。 */
function Textarea({ className, ...props }: React.ComponentProps<"textarea">) {
  return (
    <textarea
      data-slot="textarea"
      className={cn(
        "border-input placeholder:text-muted-foreground flex max-h-[320px] min-h-[120px] w-full resize-y rounded-md border bg-transparent px-3 py-2 font-mono text-xs leading-5 shadow-xs outline-none transition-[color,box-shadow] disabled:cursor-not-allowed disabled:opacity-50",
        "focus-visible:border-ring focus-visible:ring-ring/50 focus-visible:ring-[3px]",
        "aria-invalid:border-destructive aria-invalid:ring-destructive/20",
        className,
      )}
      {...props}
    />
  );
}

export { Textarea };
