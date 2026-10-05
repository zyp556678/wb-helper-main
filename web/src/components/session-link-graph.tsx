import { FileText, Link2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import type { LocalSessionGroup, LocalSessionGroupMember, MemberVersionStatus } from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * 会话关联图（对齐 wb-switch 的 `relationship-canvas`）。
 *
 * 画的是一件事：**同一段对话在哪些账号里各有一份、内容是什么关系**。
 * 中间是「同一会话」节点（分叉时是「共同旧版」），左右两列挂各账号的副本，
 * 连线表示它们同源；分叉时不同分支用不同颜色，一眼能看出「谁和谁各自往下写了」。
 *
 * **布局不测量 DOM**：坐标由本组件按固定的行高算出来（节点等高铁定高度），
 * 再用 `viewBox` + `preserveAspectRatio="none"` 映射到容器 ——
 * 这样不需要 ResizeObserver，窗口缩放也不会出现连线错位（switch 用测量，是因为
 * 它的卡片高度可变；我们这里不需要那种自由度）。
 */

const ROW_H = 56;
const NODE_H = 44;
const PAD_Y = 8;

/** 分支配色（chart-* 令牌，与 switch 的 chart-3/2/1/5/4 顺序一致）。 */
const BRANCH_COLORS = ["var(--chart-3)", "var(--chart-2)", "var(--chart-1)", "var(--chart-5)", "var(--chart-4)"];
const COMMON_COLOR = "var(--chart-4)";

/** 成员状态徽标文案（与详情页同一口径）。 */
const STATUS_LABEL: Record<MemberVersionStatus, string> = {
  latest: "内容最新",
  behind: "内容落后",
  diverge: "存在分歧",
  missing: "内容缺失",
  unknown: "无法确认",
  stale: "已失效",
  superseded: "已替代",
};

export interface SessionLinkGraphProps {
  group: LocalSessionGroup;
  /** 成员显示名（账号昵称/备注）。 */
  memberLabel: (m: LocalSessionGroupMember) => string;
}

export function SessionLinkGraph({ group, memberLabel }: SessionLinkGraphProps) {
  const members = group.members ?? [];
  if (members.length === 0) return null;

  const divergence = group.divergence ?? null;
  const commonIds = new Set(divergence?.common_member_ids ?? []);
  const branchList = divergence?.branch_member_ids ?? [];
  const hasBranches = Boolean(divergence && branchList.length > 0);

  // ---- 分列 ----
  //
  // 有分支形状时：共同旧版在左、各分支在右（与 switch 一致）。
  // 普通组：前半在左、后半在右（让连线不交叉得太乱）。
  let left: LocalSessionGroupMember[] = [];
  let right: LocalSessionGroupMember[] = [];
  /** 成员 → 分支序号（-1 表示非分支成员）。 */
  const branchOf = new Map<string, number>();

  if (hasBranches) {
    left = members.filter((m) => commonIds.has(m.session_id));
    branchList.forEach((ids, bi) => {
      for (const id of ids) branchOf.set(id, bi);
    });
    right = members.filter((m) => branchOf.has(m.session_id));
    // 兜底：既不在共同旧版也不在分支里的成员（理论上不该有），补到右侧。
    for (const m of members) {
      if (!commonIds.has(m.session_id) && !branchOf.has(m.session_id)) right.push(m);
    }
  } else {
    const half = Math.ceil(members.length / 2);
    left = members.slice(0, half);
    right = members.slice(half);
  }

  const rows = Math.max(left.length, right.length, 1);
  const height = rows * ROW_H + PAD_Y * 2;
  const hubY = height / 2;

  const lineColor = (m: LocalSessionGroupMember): string => {
    if (hasBranches) {
      const bi = branchOf.get(m.session_id);
      if (bi !== undefined) return BRANCH_COLORS[bi % BRANCH_COLORS.length];
      if (commonIds.has(m.session_id)) return COMMON_COLOR;
    }
    return "var(--primary)";
  };

  const nodeY = (index: number) => PAD_Y + index * ROW_H + (ROW_H - NODE_H) / 2;
  const nodeCenterY = (index: number) => nodeY(index) + NODE_H / 2;

  const renderNode = (m: LocalSessionGroupMember, index: number, side: "left" | "right") => (
    <div
      key={m.session_id}
      className={cn(
        "absolute flex flex-col justify-center gap-1 rounded-xl border border-border bg-card px-3 shadow-xs",
        side === "left" ? "left-0 w-[38%]" : "right-0 w-[38%]",
      )}
      style={{ top: nodeY(index), height: NODE_H }}
      title={m.reason || undefined}
    >
      <div className="flex min-w-0 items-center gap-1.5">
        <span className="min-w-0 truncate text-xs font-medium">{memberLabel(m)}</span>
        <span className="shrink-0 rounded bg-muted px-1 text-[10px] text-muted-foreground">
          {m.variant === "intl" ? "国际站" : "国内站"}
        </span>
        {hasBranches && commonIds.has(m.session_id) ? (
          <Badge variant="warning" className="shrink-0 text-[10px]">
            共同旧版
          </Badge>
        ) : null}
        {hasBranches && branchOf.has(m.session_id) ? (
          <Badge variant="outline" className="shrink-0 text-[10px]">
            分支 {(branchOf.get(m.session_id) ?? 0) + 1}
          </Badge>
        ) : null}
      </div>
      <div className="flex min-w-0 items-center gap-1.5 text-[11px] text-muted-foreground">
        <span className="truncate">{STATUS_LABEL[m.version_status] ?? m.version_status}</span>
        <span className="shrink-0 tabular-nums">
          {m.readable ? `${m.record_count} 条内容` : "内容条数无法确认"}
        </span>
      </div>
    </div>
  );

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <p className="text-sm font-medium">
          {hasBranches ? "内容分支图" : "会话关联图"}
        </p>
        <p className="text-xs text-muted-foreground">
          {hasBranches
            ? `共同旧版上的 ${branchList.length} 条独立更新`
            : "同一会话，在不同账号中各有一份"}
        </p>
      </div>

      <div
        className="relative overflow-hidden rounded-xl border border-border bg-muted/30 p-2"
        style={{ height }}
      >
        {/* 连线层（先画线，节点盖在上面） */}
        <svg
          className="pointer-events-none absolute inset-0 size-full"
          viewBox={`0 0 1000 ${height}`}
          preserveAspectRatio="none"
          aria-hidden="true"
        >
          {left.map((m, i) => (
            <path
              key={`l-${m.session_id}`}
              d={`M 380 ${nodeCenterY(i)} C 440 ${nodeCenterY(i)}, 440 ${hubY}, 500 ${hubY}`}
              fill="none"
              stroke={lineColor(m)}
              strokeWidth={hasBranches ? 2 : 1.5}
              vectorEffect="non-scaling-stroke"
            />
          ))}
          {right.map((m, i) => (
            <path
              key={`r-${m.session_id}`}
              d={`M 620 ${nodeCenterY(i)} C 560 ${nodeCenterY(i)}, 560 ${hubY}, 500 ${hubY}`}
              fill="none"
              stroke={lineColor(m)}
              strokeWidth={hasBranches ? 2 : 1.5}
              vectorEffect="non-scaling-stroke"
            />
          ))}
        </svg>

        {/* 中心节点 */}
        <div
          className="absolute left-1/2 flex w-[22%] -translate-x-1/2 flex-col items-center justify-center gap-0.5 rounded-xl border border-primary/30 bg-primary/10 px-2 text-center"
          style={{ top: hubY - NODE_H / 2, height: NODE_H }}
        >
          <div className="flex items-center gap-1 text-xs font-medium text-primary-ink">
            {hasBranches ? (
              <Link2 className="size-3" aria-hidden="true" />
            ) : (
              <FileText className="size-3" aria-hidden="true" />
            )}
            {hasBranches ? "共同旧版" : "同一会话"}
          </div>
          <div className="text-[10px] text-muted-foreground">
            {hasBranches
              ? `${commonIds.size} 个账号内容相同`
              : `${members.length} 个关联账号`}
          </div>
        </div>

        {left.map((m, i) => renderNode(m, i, "left"))}
        {right.map((m, i) => renderNode(m, i, "right"))}
      </div>

      <p className="text-xs leading-5 text-muted-foreground">
        {hasBranches
          ? "线条表示已验证的内容继承与分叉；不同颜色的分支互有新增，无法自动合并。"
          : group.status === "diverge"
            ? "连线只表示这些账号属于同一会话；不会自动选择要保留的内容。"
            : group.status === "latest"
              ? "账号更新后，可重新检查内容状态。"
              : "连线表示账号关联；仅在确认可安全补齐时提供批量同步。"}
      </p>
    </div>
  );
}
