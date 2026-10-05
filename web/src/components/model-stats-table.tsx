import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatMillis, formatTokens } from "@/lib/format";
import type { ModelMetric } from "@/lib/types";
import { cn } from "@/lib/utils";

export interface ModelStatsTableProps {
  models: ModelMetric[];
}

/** 模型统计附表：每行一个模型。 */
export function ModelStatsTable({ models }: ModelStatsTableProps) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>模型</TableHead>
          <TableHead className="text-right">请求数</TableHead>
          <TableHead className="text-right">失败数</TableHead>
          <TableHead className="text-right">平均首字</TableHead>
          <TableHead className="text-right">平均总耗时</TableHead>
          <TableHead className="text-right">累计输出 token</TableHead>
          <TableHead className="text-right">可用账号</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {models.map((metric) => (
          <TableRow key={metric.model}>
            <TableCell className="font-mono text-foreground">{metric.model}</TableCell>
            <TableCell className="text-right tabular-nums">{metric.requests}</TableCell>
            <TableCell
              className={cn(
                "text-right tabular-nums",
                metric.failures > 0 ? "text-destructive" : "text-muted-foreground",
              )}
            >
              {metric.failures}
            </TableCell>
            <TableCell className="text-right tabular-nums text-foreground">
              {metric.avg_ttft_ms === null ? "-" : `${formatMillis(metric.avg_ttft_ms)} ms`}
            </TableCell>
            <TableCell className="text-right tabular-nums text-foreground">
              {metric.avg_total_ms === null ? "-" : `${formatMillis(metric.avg_total_ms)} ms`}
            </TableCell>
            <TableCell className="text-right tabular-nums text-foreground">
              {formatTokens(metric.tokens_total)}
            </TableCell>
            <TableCell className="text-right tabular-nums">{metric.available_accounts}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
