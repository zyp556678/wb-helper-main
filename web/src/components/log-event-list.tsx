import { Fragment, useState } from "react";
import { ChevronDown } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  formatRelativeTime,
  formatUnixSecondsFull,
  logChannelLabel,
  logLevelLabel,
} from "@/lib/format";
import type { LogEvent, LogFieldValue } from "@/lib/types";
import { cn } from "@/lib/utils";

/** 级别色点：info 中性、warn 琥珀、error destructive，与工具条筛选 chips 一致。 */
function levelDotClass(level: string): string {
  if (level === "warn") return "bg-amber-500";
  if (level === "error") return "bg-destructive";
  return "bg-muted-foreground/55";
}

/**
 * 左侧导轨：一条竖线 + 每行一个圆角肘部，把「一整列事件」连成一条线。
 *
 * 取 BoardUI agent-log 的形态。原来的写法是每行一个孤立的级别圆点，
 * 左侧这一列只有"点"没有"线" —— 事件之间的先后关系、以及"这是在同一条时间轴上"
 * 完全看不出来；事件一多，左边缘就成了一串散落的点。
 *
 * 结构上是两段（一条线不可能分叉），都用 1px：
 *   - **肘部**：`h-3.5 w-3` 的盒子只画左、下两条边并给左下 6px 圆角，
 *     于是从行顶下来后拐向右侧的内容。拐点固定在距行顶 14px，**不是 50%** ——
 *     消息换行时行会变高，按 50% 走拐点会飘到两三行之间去。
 *   - **树干**：从拐点（8px）继续向下，接到下一行的肘部。**最后一行不画**，
 *     否则这条线会一直垂到列尾，看起来像"后面还有内容没加载出来"。
 *
 * 级别色点落在肘部末端，于是左侧这一列同时承担「结构」和「严重程度」，
 * 不再需要额外一个游离的点。
 *
 * 线的颜色用 `--muted-foreground` 而不是 `--border`：`--border` 在浅色主题下是
 * `oklch(0.91 …)`，铺在近白卡片上基本看不见（这条导轨于是等于没画）。
 * 取与级别点同源的 `--muted-foreground`，线上比点更淡一档，形成「点压在线上」的层次。
 */
function EventRail({ level, last }: { level: string; last: boolean }) {
  return (
    <span aria-hidden className="pointer-events-none absolute inset-y-0 left-0 w-3">
      <span className="absolute left-0 top-0 h-3.5 w-3 rounded-bl-[6px] border-b border-l border-muted-foreground/40" />
      {last ? null : (
        <span className="absolute bottom-0 left-0 top-2 w-px bg-muted-foreground/40" />
      )}
      <span
        className={cn(
          "absolute left-[9px] top-[11px] size-1.5 rounded-full",
          levelDotClass(level),
        )}
      />
    </span>
  );
}

function levelBadgeVariant(level: string): "outline" | "warning" | "danger" {
  if (level === "warn") return "warning";
  if (level === "error") return "danger";
  return "outline";
}

/** 从 fields 里取请求级 trace（没有则返回空串）。 */
function traceOf(event: LogEvent): string {
  const v = event.fields?.trace_id;
  return typeof v === "string" ? v : "";
}

/** fields 的值类型不固定，统一转成可展示文本。 */
function formatFieldValue(value: LogFieldValue | null | undefined): string {
  if (value === null || value === undefined) return "-";
  if (typeof value === "boolean") return value ? "true" : "false";
  return String(value);
}

/** 展开后的字段详情：后端给的是任意键值对象，不假设固定键。 */
function FieldDetails({ fields }: { fields: LogEvent["fields"] }) {
  const entries = Object.entries(fields ?? {});
  if (entries.length === 0) {
    return (
      <div className="border-t border-border/60 bg-muted/30 py-2 pl-6 pr-4 text-xs text-muted-foreground">
        该事件没有附加字段
      </div>
    );
  }

  return (
    <div className="border-t border-border/60 bg-muted/30 py-2.5 pl-6 pr-4">
      <div className="grid grid-cols-[minmax(0,180px)_1fr] gap-x-4 gap-y-1">
        <span className="text-xs font-medium text-muted-foreground">字段</span>
        <span className="text-xs font-medium text-muted-foreground">值</span>
        {entries.map(([key, value]) => (
          <Fragment key={key}>
            <span className="min-w-0 break-words font-mono text-xs text-muted-foreground">
              {key}
            </span>
            <span className="min-w-0 whitespace-pre-wrap break-words font-mono text-xs text-foreground">
              {formatFieldValue(value)}
            </span>
          </Fragment>
        ))}
      </div>
    </div>
  );
}

export interface LogEventListProps {
  events: LogEvent[];
  /** 每秒刷新的当前 Unix 秒，用于相对时间展示。 */
  now: number;
  /** 点击事件里的 trace 时回调，用于按整条请求链路过滤日志。 */
  onTrace?: (trace: string) => void;
}

/** 事件列表：每条一行，点击展开 fields 键值详情。 */
export function LogEventList({ events, now, onTrace }: LogEventListProps) {
  const [expanded, setExpanded] = useState<Set<number>>(new Set());

  const toggle = (id: number) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  return (
    <ul className="divide-y divide-border">
      {events.map((event, index) => {
        const open = expanded.has(event.id);
        const full = formatUnixSecondsFull(event.ts);

        return (
          <li key={event.id} className="relative">
            <EventRail level={event.level} last={index === events.length - 1} />
            <div
              role="button"
              tabIndex={0}
              aria-expanded={open}
              onClick={() => toggle(event.id)}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  toggle(event.id);
                }
              }}
              className="flex w-full cursor-pointer items-start gap-3 py-2.5 pl-6 pr-4 text-left outline-none transition-colors hover:bg-muted/40 focus-visible:bg-muted/50"
            >
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <span className="cursor-default whitespace-nowrap text-xs tabular-nums text-muted-foreground">
                        {formatRelativeTime(event.ts, now)}
                      </span>
                    </TooltipTrigger>
                    <TooltipContent side="top">{full ?? "后端未给出时间"}</TooltipContent>
                  </Tooltip>
                  <Badge variant={levelBadgeVariant(event.level)}>{logLevelLabel(event.level)}</Badge>
                  <Badge variant="outline" className="text-muted-foreground">
                    {logChannelLabel(event.channel)}
                  </Badge>
                  {event.model ? (
                    <span
                      className="max-w-[220px] truncate font-mono text-xs text-foreground"
                      title={event.model}
                    >
                      {event.model}
                    </span>
                  ) : null}
                  {event.account ? (
                    <span
                      className="max-w-[160px] truncate text-xs text-muted-foreground"
                      title={event.account}
                    >
                      {event.account}
                    </span>
                  ) : null}
                  <span className="font-mono text-xs text-muted-foreground">{event.event}</span>
                  {traceOf(event) ? (
                    <button
                      type="button"
                      title="按该 trace 过滤整条请求链路"
                      onClick={(e) => {
                        e.stopPropagation();
                        onTrace?.(traceOf(event)!);
                      }}
                      /* 字号原本写死 10px，低于本项目的可读性下限（中文会糊，且等宽
                         hex 在 10px 下 0/8、1/l 几乎分不出），统一到 text-xs。
                         之前日志缓冲为空时这块根本不会渲染，所以历次审计都没量到它。 */
                      className="cursor-pointer rounded border border-border px-1.5 py-px font-mono text-xs text-muted-foreground transition-colors hover:border-primary/50 hover:text-foreground"
                    >
                      {traceOf(event)}
                    </button>
                  ) : null}
                </div>
                {/* 消息是这一行的主信息，抬到 13px 与上一行的 12px 元信息拉开层级 ——
                    元信息（时间/级别/频道/事件码）都齐平时，扫读时不知道先看哪。 */}
                {event.message ? (
                  <p className="mt-1 break-words text-[13px] leading-5 text-foreground">
                    {event.message}
                  </p>
                ) : null}
              </div>
              <ChevronDown
                aria-hidden="true"
                className={cn(
                  "mt-1 size-3.5 shrink-0 text-muted-foreground transition-transform",
                  open && "rotate-180",
                )}
              />
            </div>
            {open ? <FieldDetails fields={event.fields} /> : null}
          </li>
        );
      })}
    </ul>
  );
}
