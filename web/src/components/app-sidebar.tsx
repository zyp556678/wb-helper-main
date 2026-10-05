import { KeyRound, Waypoints } from "lucide-react";

import { StatusDot } from "@/components/status-dot";
import { ThemeSwitcher } from "@/components/theme-switcher";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { NAV_ITEMS, type PageId } from "@/lib/navigation";
import { useSessionsNavEnabled } from "@/lib/nav-prefs";
import { cn } from "@/lib/utils";

export interface AppSidebarProps {
  current: PageId;
  onNavigate: (id: PageId) => void;
  online: boolean;
  version?: string;
  uptime?: string;
  onOpenApiKey: () => void;
  /**
   * 本机相关页面是否显示。服务端部署下本机代理不存在，
   * 隐藏入口比让用户点进去看「不可用」更合理。
   */
  showLocal?: boolean;
}

export function AppSidebar({
  current,
  onNavigate,
  online,
  version,
  uptime,
  onOpenApiKey,
  showLocal,
}: AppSidebarProps) {
  // 「显示关联会话菜单」偏好（配置页外观分组可关）：关掉后这里不渲染该入口，
  // 页面本身与数据不受影响；重新打开即恢复。
  const sessionsNavEnabled = useSessionsNavEnabled();
  return (
    <aside className="flex min-h-0 w-[220px] shrink-0 flex-col border-r border-sidebar-border bg-sidebar px-3 pb-4 pt-4">
      <div className="flex items-center gap-2.5 px-1 pb-5">
        <span
          aria-hidden
          className="inline-flex size-9 shrink-0 items-center justify-center rounded-[26%] bg-primary text-primary-foreground shadow-sm"
        >
          <Waypoints className="size-5" strokeWidth={2.2} />
        </span>
        <div className="min-w-0">
          <div className="truncate text-[15px] font-semibold leading-5 tracking-[-0.02em] text-sidebar-foreground/90">
            WorkBuddy 网关
          </div>
          <div className="truncate text-xs leading-4 text-sidebar-foreground/70">控制台</div>
        </div>
      </div>

      <nav className="flex min-h-0 flex-1 flex-col gap-0.5" aria-label="主导航">
        {NAV_ITEMS.filter(
          (item) =>
            (item.id !== "local" || showLocal) && (item.id !== "sessions" || sessionsNavEnabled),
        ).map(({ id, label, icon: Icon }) => {
          const active = id === current;
          return (
            <button
              key={id}
              type="button"
              aria-current={active ? "page" : undefined}
              onClick={() => onNavigate(id)}
              className={cn(
                "flex cursor-pointer items-center gap-2.5 rounded-lg px-3 py-2.5 text-left text-sm outline-none transition-colors focus-visible:ring-2 focus-visible:ring-sidebar-ring/50",
                active
                  ? "bg-foreground/[0.06] font-medium text-foreground"
                  : "text-muted-foreground hover:bg-foreground/[0.04] hover:text-foreground",
              )}
            >
              <Icon className="size-4 shrink-0" aria-hidden="true" />
              <span className="truncate">{label}</span>
            </button>
          );
        })}
      </nav>

      <section className="mt-auto border-t border-sidebar-border px-2 pt-3 text-xs">
        <div className="flex items-center gap-2 text-[13px] text-sidebar-foreground">
          <StatusDot on={online} />
          <span className="min-w-0 flex-1 truncate">{online ? "网关在线" : "未连接"}</span>
          <span className="shrink-0 text-sidebar-foreground/70">v{version || "?"}</span>
        </div>
        {uptime ? (
          <div className="mt-1 truncate pl-3.5 text-sidebar-foreground/70">已运行 {uptime}</div>
        ) : null}
        <div className="mt-3 flex items-center justify-between gap-2">
          <ThemeSwitcher />
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                aria-label="配置访问密钥"
                onClick={onOpenApiKey}
                className="size-7 text-sidebar-foreground/70 hover:text-sidebar-foreground"
              >
                <KeyRound className="size-3.5" />
              </Button>
            </TooltipTrigger>
            <TooltipContent side="top">配置访问密钥</TooltipContent>
          </Tooltip>
        </div>
      </section>
    </aside>
  );
}
