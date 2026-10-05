import { cn } from "@/lib/utils";

/** 底部状态点：照搬 wb-switch 的 StatusDot。 */
export function StatusDot({ on, className }: { on: boolean; className?: string }) {
  return (
    <span
      aria-hidden
      className={cn("size-1.5 shrink-0 rounded-full", on ? "bg-primary" : "bg-muted-foreground/35", className)}
    />
  );
}
