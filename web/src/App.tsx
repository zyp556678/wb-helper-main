import { Suspense, lazy, useState } from "react";

import { AppSidebar } from "@/components/app-sidebar";
import { ErrorBoundary } from "@/components/error-boundary";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { formatUptime } from "@/lib/format";
import type { PageId } from "@/lib/navigation";
import { usePanelData } from "@/lib/use-panel-data";
import { AccountsPage } from "@/pages/accounts-page";
import { ConfigPage } from "@/pages/config-page";
import { LogsPage } from "@/pages/logs-page";
import { RequestsPage } from "@/pages/requests-page";
import { ModelsPage } from "@/pages/models-page";
import { MonitoringPage } from "@/pages/monitoring-page";
import { SitesPage } from "@/pages/sites-page";
import { LocalPage } from "@/pages/local-page";
import { LinkedSessionsPage } from "@/pages/linked-sessions-page";
import { TasksPage } from "@/pages/tasks-page";
import { useLocalCapabilities } from "@/lib/use-local-capabilities";

/**
 * 用量统计页按需加载：它依赖 recharts（约 400 kB），而图表只在一个页面用得上。
 * 静态引入会把主包从 520 kB 抬到 950 kB，让「打开面板」这个最常发生的动作
 * 为一张可能永远不看的图买单。
 */
const StatsPage = lazy(() =>
  import("@/pages/stats-page").then((m) => ({ default: m.StatsPage })),
);

// 积分统计同样按需加载：它也依赖 recharts，与用量统计共享同一个图表 chunk，
// 不加载其中一页就不会为图表付首屏体积。
const CreditStatsPage = lazy(() =>
  import("@/pages/credit-stats-page").then((m) => ({ default: m.CreditStatsPage })),
);

export default function App() {
  const [page, setPage] = useState<PageId>("accounts");
  const [keyPanelOpen, setKeyPanelOpen] = useState(false);
  const data = usePanelData();
  // 本机能力可用性：决定侧栏是否显示「本机客户端」（服务端部署下自动隐藏）
  const localAvailable = useLocalCapabilities();

  const online = data.error === null && data.overview !== null;

  return (
    <TooltipProvider delayDuration={250}>
      <div className="flex h-screen min-h-0 overflow-hidden bg-background">
        <AppSidebar
          current={page}
          onNavigate={setPage}
          online={online}
          version={data.overview?.version}
          uptime={data.overview ? formatUptime(data.overview.uptime_seconds) : undefined}
          onOpenApiKey={() => setKeyPanelOpen(true)}
          showLocal={localAvailable}
        />
        <main className="min-w-0 flex-1 overflow-y-auto overscroll-contain bg-background">
          {/* key 用 page：切换导航即重建边界，避免上一页的错误状态粘住新页面 */}
          <ErrorBoundary key={page}>
          {page === "accounts" ? (
            <AccountsPage
              data={data}
              keyPanelOpen={keyPanelOpen}
              onKeyPanelOpenChange={setKeyPanelOpen}
              onNavigate={setPage}
            />
          ) : page === "monitoring" ? (
            <MonitoringPage />
          ) : page === "models" ? (
            <ModelsPage />
          ) : page === "sites" ? (
            <SitesPage />
          ) : page === "tasks" ? (
            <TasksPage />
          ) : page === "sessions" ? (
            <LinkedSessionsPage accounts={data.accounts ?? []} />
          ) : page === "local" ? (
            <LocalPage />
          ) : page === "stats" ? (
            <Suspense
              fallback={
                <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
                  <div className="h-8 w-40 animate-pulse rounded bg-muted" />
                  <div className="mt-6 h-72 animate-pulse rounded-lg bg-muted" />
                </div>
              }
            >
              <StatsPage />
            </Suspense>
          ) : page === "config" ? (
            <ConfigPage />
          ) : page === "credits" ? (
            <Suspense
              fallback={
                <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
                  <div className="h-8 w-40 animate-pulse rounded bg-muted" />
                  <div className="mt-6 h-72 animate-pulse rounded-lg bg-muted" />
                </div>
              }
            >
              <CreditStatsPage />
            </Suspense>
          ) : page === "logs" ? (
            <LogsPage />
          ) : page === "requests" ? (
            <RequestsPage />
          ) : null}
          </ErrorBoundary>
        </main>
      </div>
      <Toaster />
    </TooltipProvider>
  );
}
