import { useState } from "react";
import { ChevronLeft, ChevronRight } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { formatTokenCompact, formatTokenExact, formatUnixSecondsFull, tokenPercentage } from "@/lib/format";
import type { TokenRequestRow } from "@/lib/types";

/**
 * 请求明细：每次模型调用一行，按时间倒序。
 *
 * 「用量」这一格不是单一数字 —— 输入里有多少是缓存命中、输出里有多少是思考，
 * 决定了这次调用到底贵在哪。悬停展开明细，避免表格被 7 个数字撑爆。
 */
const PAGE_SIZE = 50;

function UsageCell({ row }: { row: TokenRequestRow }) {
  const reply = Math.max(0, row.output - row.thinking);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <div tabIndex={0} className="cursor-default rounded-md text-right outline-none">
          <div className="text-[13px] font-medium tabular-nums">{formatTokenCompact(row.total)}</div>
          <div className="mt-0.5 text-xs text-muted-foreground tabular-nums">
            ↑{formatTokenCompact(row.input)} ↓{formatTokenCompact(row.output)}{" "}
            {tokenPercentage(row.cacheRead, row.input)}
          </div>
        </div>
      </TooltipTrigger>
      <TooltipContent side="left" align="center" className="w-[264px] space-y-1.5 text-xs">
        <div className="flex items-center justify-between font-medium">
          <span>Token 消耗明细</span>
          <span className="tabular-nums">总计 {formatTokenExact(row.total)}</span>
        </div>
        <Row label="输入" value={row.input} color="var(--data-series-sky)" />
        <Row label="缓存命中" value={row.cacheRead} color="var(--data-series-emerald)" />
        <Row label="缓存未命中" value={row.uncachedInput} color="var(--data-series-rose)" />
        <Row label="输出" value={row.output} color="var(--data-series-violet)" />
        <Row label="思考过程" value={row.thinking} color="var(--data-series-indigo)" />
        <Row label="回复内容" value={reply} color="var(--data-series-slate)" />
        <Row label="缓存写入" value={row.cacheWrite} color="var(--data-series-amber)" />
        <div className="flex items-center justify-between border-t border-border/60 pt-1.5">
          <span className="text-muted-foreground">缓存命中率</span>
          <span className="font-medium tabular-nums" style={{ color: "var(--data-series-emerald)" }}>
            {tokenPercentage(row.cacheRead, row.input)}
          </span>
        </div>
      </TooltipContent>
    </Tooltip>
  );
}

function Row({ label, value, color }: { label: string; value: number; color: string }) {
  return (
    <div className="flex items-center gap-2">
      <span aria-hidden="true" className="size-2.5 shrink-0 rounded-[2px]" style={{ background: color }} />
      <span className="flex-1 text-muted-foreground">{label}</span>
      <span className="font-medium tabular-nums">{formatTokenExact(value)}</span>
    </div>
  );
}

export function TokenRequestDialog({
  open,
  onOpenChange,
  rows,
  records,
  label,
}: {
  open: boolean;
  onOpenChange: (next: boolean) => void;
  rows: TokenRequestRow[];
  records: number;
  label: string;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="grid-cols-1 max-h-[85vh] sm:max-w-5xl">
        <DialogHeader>
          <DialogTitle>请求明细</DialogTitle>
          <DialogDescription>
            {label}：每次模型调用一行，按时间倒序。悬停「用量」可看到输入/缓存/输出的拆分。
          </DialogDescription>
        </DialogHeader>
        {rows.length === 0 ? (
          <div className="rounded-lg border border-dashed px-4 py-10 text-center text-sm text-muted-foreground">
            该来源暂无可展示的请求明细。
          </div>
        ) : (
          <RequestTable rows={rows} records={records} />
        )}
      </DialogContent>
    </Dialog>
  );
}

function RequestTable({ rows, records }: { rows: TokenRequestRow[]; records: number }) {
  const [page, setPage] = useState(0);
  const pages = Math.max(1, Math.ceil(rows.length / PAGE_SIZE));
  const start = page * PAGE_SIZE;
  const shown = rows.slice(start, start + PAGE_SIZE);
  const shownEnd = start + shown.length;

  return (
    <div className="flex min-h-0 flex-col gap-3">
      <div className="min-h-0 flex-1 overflow-auto rounded-lg border">
        <Table containerClassName="overflow-visible" className="min-w-[720px]">
          <TableHeader className="sticky top-0 z-10 bg-background [&_th]:bg-background">
            <TableRow>
              <TableHead>时间</TableHead>
              <TableHead>模型</TableHead>
              <TableHead>会话</TableHead>
              <TableHead>项目</TableHead>
              <TableHead className="text-right">用量</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.map((r, i) => (
              <TableRow key={`${r.timestamp}-${r.sessionId}-${i}`}>
                <TableCell className="tabular-nums text-muted-foreground">
                  {formatUnixSecondsFull(r.timestamp / 1000) ?? "—"}
                </TableCell>
                <TableCell className="max-w-[200px] truncate font-mono">{r.model}</TableCell>
                <TableCell className="max-w-[220px] truncate" title={r.title ?? r.sessionId}>
                  {r.title?.trim() || `${r.sessionId.slice(0, 8)}…`}
                </TableCell>
                <TableCell className="max-w-[160px] truncate">{r.project}</TableCell>
                <TableCell className="w-[150px]">
                  <UsageCell row={r} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <div className="flex flex-wrap items-center justify-between gap-3 text-xs text-muted-foreground">
        <span className="tabular-nums">
          {records > rows.length
            ? `仅展示最近 ${rows.length} 条（共 ${records} 次调用）`
            : `共 ${rows.length} 次调用`}
        </span>
        <div className="flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={page === 0}
            onClick={() => setPage((p) => Math.max(0, p - 1))}
          >
            <ChevronLeft className="size-3.5" aria-hidden="true" />
            上一页
          </Button>
          <span className="tabular-nums">
            第 {start + 1}–{shownEnd} 条 · 共 {rows.length} 条
          </span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={page >= pages - 1}
            onClick={() => setPage((p) => Math.min(pages - 1, p + 1))}
          >
            下一页
            <ChevronRight className="size-3.5" aria-hidden="true" />
          </Button>
        </div>
      </div>
    </div>
  );
}
