import { useCallback, useMemo, useState } from "react";
import { Inbox, RefreshCw, SearchX } from "lucide-react";
import {
  notifySuccess,
  notifyError,
} from "@/lib/notify";

import { LogEventList } from "@/components/log-event-list";
import { LogToolbar } from "@/components/log-toolbar";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { clearLogs, describeError } from "@/lib/api";
import { useLogs } from "@/lib/use-logs";
import { useNowSeconds } from "@/lib/use-now-seconds";
import { cn } from "@/lib/utils";

/** 单页最多渲染的事件数，超出部分靠筛选缩小范围。 */
const RENDER_LIMIT = 200;

export function LogsPage() {
  const [level, setLevel] = useState("");
  const [channel, setChannel] = useState("");
  const [q, setQ] = useState("");
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [clearing, setClearing] = useState(false);

  const { logs, loading, error, unauthorized, reload } = useLogs(
    { level, channel, q },
    autoRefresh,
  );
  // 每秒本地推进，保证两次轮询之间相对时间也在走。
  const now = useNowSeconds();

  const events = useMemo(() => logs?.events ?? [], [logs]);
  const visible = useMemo(() => events.slice(0, RENDER_LIMIT), [events]);
  const truncated = events.length > RENDER_LIMIT;
  const filterActive = level !== "" || channel !== "" || q.trim() !== "";
  const firstLoad = loading && logs === null;

  const handleClear = useCallback(async () => {
    setClearing(true);
    try {
      const result = await clearLogs();
      notifySuccess(`已清空 ${result.cleared} 条日志`);
      reload();
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setClearing(false);
    }
  }, [reload]);

  return (
    <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
      <header className="mb-6 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">日志</h1>
          <p className="mt-2 text-sm leading-6 text-muted-foreground">
            网关事件日志：出站改写、拦截重试、账号治理等。环形缓冲最多保留{" "}
            {logs?.capacity ?? 500} 条，新的在前。
          </p>
        </div>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={reload}
          disabled={loading}
          className="mt-1"
          aria-label="刷新日志"
        >
          <RefreshCw className={cn("size-3.5", loading && "animate-spin")} aria-hidden="true" />
          刷新
        </Button>
      </header>

      {error ? (
        <Card className="mb-5 gap-3 border-destructive/30 bg-destructive/5 p-4">
          <div className="text-sm font-medium text-destructive">读取日志失败</div>
          <p className="text-xs leading-5 text-muted-foreground">{error}</p>
          {unauthorized ? (
            <p className="text-xs leading-5 text-muted-foreground">
              网关要求鉴权：请到「账号」页右上角配置访问密钥后重试。
            </p>
          ) : null}
          <div>
            <Button type="button" variant="outline" size="sm" onClick={reload} disabled={loading}>
              重试
            </Button>
          </div>
        </Card>
      ) : null}

      <LogToolbar
        level={level}
        channel={channel}
        q={q}
        channels={logs?.channels ?? []}
        levels={logs?.levels ?? {}}
        total={logs?.total ?? 0}
        autoRefresh={autoRefresh}
        clearing={clearing}
        onLevelChange={setLevel}
        onChannelChange={setChannel}
        onQueryChange={setQ}
        onAutoRefreshChange={setAutoRefresh}
        onClear={() => void handleClear()}
      />

      <Card className="mt-3 gap-0 overflow-hidden py-0">
        {firstLoad ? (
          <div className="space-y-2 p-4">
            {[0, 1, 2, 3, 4].map((index) => (
              <Skeleton key={index} className="h-9 rounded-md" />
            ))}
          </div>
        ) : visible.length === 0 ? (
          <div className="flex flex-col items-center gap-2 py-14 text-center">
            {filterActive ? (
              <>
                <SearchX className="size-6 text-muted-foreground" aria-hidden="true" />
                <div className="text-sm font-medium">没有匹配的事件</div>
                <p className="text-xs text-muted-foreground">
                  换个关键词或调整筛选条件后再试。
                </p>
              </>
            ) : (
              <>
                <Inbox className="size-6 text-muted-foreground" aria-hidden="true" />
                <div className="text-sm font-medium">暂无事件</div>
                <p className="text-xs text-muted-foreground">
                  发起一次对话请求后，这里会显示出站改写、拦截重试、账号治理等事件。
                </p>
              </>
            )}
          </div>
        ) : (
          <LogEventList events={visible} now={now} onTrace={(trace) => setQ(trace)} />
        )}

        {truncated ? (
          <div className="border-t border-border bg-muted/40 px-4 py-2 text-center text-xs text-muted-foreground">
            仅显示最近 {RENDER_LIMIT} 条，可用筛选缩小范围。
          </div>
        ) : null}
      </Card>
    </div>
  );
}
