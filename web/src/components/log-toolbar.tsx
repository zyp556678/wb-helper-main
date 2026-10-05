import * as React from "react";
import { Loader2, Search, Trash2 } from "lucide-react";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { logChannelLabel, logLevelLabel } from "@/lib/format";
import type { LogChannelCount, LogLevel } from "@/lib/types";
import { cn } from "@/lib/utils";

/** 级别筛选的顺序与色点：与列表里的级别色点保持一致。 */
const LEVEL_FILTERS: { value: LogLevel; dot: string }[] = [
  { value: "info", dot: "bg-muted-foreground/55" },
  { value: "warn", dot: "bg-amber-500" },
  { value: "error", dot: "bg-destructive" },
];

interface FilterChipProps {
  active: boolean;
  onClick: () => void;
  dot?: string;
  children: React.ReactNode;
}

function FilterChip({ active, onClick, dot, children }: FilterChipProps) {
  return (
    <button
      type="button"
      aria-pressed={active}
      onClick={onClick}
      className={cn(
        "inline-flex cursor-pointer items-center gap-1.5 rounded-md border px-2.5 py-1 text-xs outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50",
        active
          ? "border-primary/40 bg-primary/10 font-medium text-foreground"
          : "border-border text-muted-foreground hover:bg-accent/50 hover:text-foreground",
      )}
    >
      {dot ? <span aria-hidden className={cn("size-1.5 shrink-0 rounded-full", dot)} /> : null}
      {children}
    </button>
  );
}

export interface LogToolbarProps {
  /** 级别筛选，空串 = 全部。 */
  level: string;
  /** 频道筛选，空串 = 全部。 */
  channel: string;
  q: string;
  /** logs.channels：各频道条数。 */
  channels: LogChannelCount[];
  /** logs.levels：各级别条数。 */
  levels: Partial<Record<LogLevel, number>>;
  /** 缓冲区中的事件总数，用于「全部」计数。 */
  total: number;
  autoRefresh: boolean;
  clearing: boolean;
  onLevelChange: (level: string) => void;
  onChannelChange: (channel: string) => void;
  onQueryChange: (q: string) => void;
  onAutoRefreshChange: (enabled: boolean) => void;
  onClear: () => void;
}

/** 日志页工具条：级别 / 频道筛选、关键词搜索、自动刷新与清空。 */
export function LogToolbar({
  level,
  channel,
  q,
  channels,
  levels,
  total,
  autoRefresh,
  clearing,
  onLevelChange,
  onChannelChange,
  onQueryChange,
  onAutoRefreshChange,
  onClear,
}: LogToolbarProps) {
  return (
    <Card className="gap-3 px-4 py-3">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="mr-0.5 text-xs font-medium text-muted-foreground">级别</span>
          <FilterChip active={level === ""} onClick={() => onLevelChange("")}>
            全部
            <span className="tabular-nums text-muted-foreground">{total}</span>
          </FilterChip>
          {LEVEL_FILTERS.map((item) => (
            <FilterChip
              key={item.value}
              active={level === item.value}
              dot={item.dot}
              onClick={() => onLevelChange(item.value)}
            >
              {logLevelLabel(item.value)}
              <span className="tabular-nums text-muted-foreground">{levels[item.value] ?? 0}</span>
            </FilterChip>
          ))}
        </div>

        <Separator orientation="vertical" className="hidden h-6 sm:block" />

        <div className="flex flex-wrap items-center gap-1.5">
          <span className="mr-0.5 text-xs font-medium text-muted-foreground">频道</span>
          <FilterChip active={channel === ""} onClick={() => onChannelChange("")}>
            全部
          </FilterChip>
          {channels.map((item) => (
            <FilterChip
              key={item.channel}
              active={channel === item.channel}
              onClick={() => onChannelChange(item.channel)}
            >
              {logChannelLabel(item.channel)}
              <span className="tabular-nums text-muted-foreground">{item.count}</span>
            </FilterChip>
          ))}
        </div>
      </div>

      <div className="flex flex-wrap items-center justify-between gap-3 border-t border-border pt-3">
        <div className="relative">
          <Search
            className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden="true"
          />
          <Input
            type="search"
            value={q}
            spellCheck={false}
            autoComplete="off"
            aria-label="搜索日志"
            placeholder="搜索 message / model / account / trace"
            onChange={(event) => onQueryChange(event.target.value)}
            className="h-8 w-56 pl-8 text-xs sm:w-72"
          />
        </div>

        <div className="flex flex-wrap items-center gap-3">
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex cursor-default items-center gap-2 text-xs text-muted-foreground">
                自动刷新
                <Switch
                  checked={autoRefresh}
                  aria-label="自动刷新日志"
                  onCheckedChange={onAutoRefreshChange}
                />
              </span>
            </TooltipTrigger>
            <TooltipContent side="top" className="max-w-[260px]">
              每 2 秒自动刷新一次；页面切到后台时暂停，恢复可见时立即补一次。
            </TooltipContent>
          </Tooltip>

          <AlertDialog>
            <AlertDialogTrigger asChild>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={clearing || total === 0}
                aria-label="清空日志"
              >
                {clearing ? (
                  <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                ) : (
                  <Trash2 className="size-3.5" aria-hidden="true" />
                )}
                清空日志
              </Button>
            </AlertDialogTrigger>
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>清空日志？</AlertDialogTitle>
                <AlertDialogDescription>
                  将丢弃缓冲区中的全部事件，且不可恢复。清空后网关仍会继续记录新事件。
                </AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel>取消</AlertDialogCancel>
                <AlertDialogAction onClick={onClear}>清空</AlertDialogAction>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>
        </div>
      </div>
    </Card>
  );
}
