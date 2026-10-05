import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Check,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Copy,
  Folder,
  Layers,
  Link2,
  Loader2,
  MessageSquare,
  MoreHorizontal,
  RefreshCw,
  SlidersHorizontal,
  Unlink,
} from "lucide-react";
import {
  notifySuccess,
  notifyError,
  notifyWarning,
  notifyInfo,
} from "@/lib/notify";

import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { AddLinkedSessionDialog } from "@/components/add-linked-session-dialog";
import { SessionLinkGraph } from "@/components/session-link-graph";
import {
  addSessionGroupMember,
  deleteSessionGroup,
  describeError,
  fetchSessionGroup,
  fetchSessionGroups,
  previewSessionGroupPair,
  recoverLocalSessions,
  syncSessionGroupSafeBatch,
  syncSessionGroupUnify,
  unlinkSessionGroupMember,
} from "@/lib/api";
import { relativeTime } from "@/lib/format";
import type {
  Account,
  GroupCounts,
  GroupStatus,
  GroupSyncReport,
  LocalSessionGroup,
  LocalSessionGroupDetailResponse,
  LocalSessionGroupMember,
  MemberVersionStatus,
  SessionAddTarget,
} from "@/lib/types";
import { cn } from "@/lib/utils";

// -----------------------------------------------------------------------------
// 常量与展示映射
// -----------------------------------------------------------------------------

const PAGE_SIZE = 8;
/** 列表不超过一页时隐藏底部分页条：一页装得下，去掉更清爽（与 switch 一致）。 */
const PAGINATION_HIDE_MAX = PAGE_SIZE;

/** 组状态 → 文案/圆点/文字色（与 switch 同口径：待同步=琥珀、内容一致=主色、有分歧=红）。 */
const GROUP_STATUS_STYLE: Record<GroupStatus, { label: string; dot: string; text: string }> = {
  latest: { label: "内容一致", dot: "bg-primary", text: "text-muted-foreground" },
  behind: { label: "待同步", dot: "bg-amber-500", text: "text-amber-700 dark:text-amber-300" },
  diverge: { label: "有分歧", dot: "bg-destructive", text: "text-destructive" },
  missing: { label: "内容缺失", dot: "bg-muted-foreground", text: "text-muted-foreground" },
  unknown: { label: "无法确认", dot: "bg-muted-foreground", text: "text-muted-foreground" },
};

/** 状态筛选项；后两项仅当计数 > 0（或当前选中）时出现。 */
type FilterID = "all" | GroupStatus;

const FILTERS: { id: FilterID; label: string }[] = [
  { id: "all", label: "全部" },
  { id: "behind", label: "待同步" },
  { id: "diverge", label: "有分歧" },
  { id: "latest", label: "内容一致" },
  { id: "missing", label: "内容缺失" },
  { id: "unknown", label: "无法确认" },
];

/** 条件展示的筛选项（计数为 0 时隐藏）。 */
const CONDITIONAL_FILTERS = new Set<FilterID>(["missing", "unknown"]);

function filterCount(counts: GroupCounts, id: FilterID): number {
  if (id === "all") return counts.all;
  if (id === "behind") return counts.behind;
  if (id === "diverge") return counts.diverge;
  if (id === "latest") return counts.latest;
  if (id === "missing") return counts.missing;
  return counts.unknown;
}

/** 成员状态徽标（latest 在整组一致时显示「内容一致」，否则「内容最新」）。 */
const MEMBER_STATUS_META: Record<
  MemberVersionStatus,
  { label: string; variant: "success" | "warning" | "outline" }
> = {
  latest: { label: "内容最新", variant: "success" },
  behind: { label: "内容落后", variant: "warning" },
  diverge: { label: "存在分歧", variant: "warning" },
  missing: { label: "内容缺失", variant: "warning" },
  unknown: { label: "无法确认", variant: "outline" },
  stale: { label: "已失效", variant: "outline" },
  superseded: { label: "已替代", variant: "outline" },
};

/** 同步写入模式的中文标签（对照 switch 的 modes 文案）。 */
const MODE_LABEL: Record<string, string> = {
  fastForward: "可安全快进（纯追加）",
  overwrite: "双方都有更新，将覆盖目标",
  unifyOverwrite: "目标有更新，将覆盖目标",
};

/** 「以此为准」的预览计划：来源 + 逐目标判定。 */
interface UnifyPlanItem {
  member: LocalSessionGroupMember;
  preview: {
    verdict: string;
    reason: string;
    source_only: number;
    target_only: number;
    available_modes: string[];
    /** 服务端签发的预览凭据 id（执行时原样带回，见 api.ts 的说明）。 */
    preview_token: string;
  } | null;
  /** 选定的写入模式；空串表示判定不允许写入（阻断项）。 */
  mode: string;
  error?: string;
}

interface UnifyPlan {
  source: LocalSessionGroupMember;
  items: UnifyPlanItem[];
}

/** 平台卡：与 switch 一致的三张（VS Code / IDE 依赖平台能力，本轮入口保持禁用）。 */
const PLATFORMS = [
  {
    id: "workbuddy",
    title: "WorkBuddy",
    subtitle: "国内版 · 国际版",
    enabled: true,
  },
  {
    id: "codebuddy-vscode",
    title: "CodeBuddy 插件",
    subtitle: "VS Code",
    enabled: false,
    disabledNote: "本机未安装 VS Code，无法读取会话",
  },
  {
    id: "codebuddy-ide",
    title: "CodeBuddy IDE",
    subtitle: "国内版 / 国际版",
    enabled: false,
    disabledNote: "本机未安装 CodeBuddy IDE，无法读取会话",
  },
] as const;

/**
 * 平台图标。
 *
 * 品牌字形无法从截图逐像素还原，这里用「同色圆角方块 + 近似字形」——
 * 尺寸、圆角、色相与截图对齐，不冒用别人的商标。
 */
function PlatformLogo({ id }: { id: string }) {
  const base = "flex size-12 shrink-0 items-center justify-center rounded-xl";
  if (id === "workbuddy") {
    return (
      <div className={cn(base, "bg-primary/12")}>
        <svg viewBox="0 0 24 24" className="size-6 text-primary-ink" aria-hidden="true">
          <path
            fill="currentColor"
            d="M4 5.5 7.2 17.5a1 1 0 0 0 1.93.02L12 9.6l2.87 7.92a1 1 0 0 0 1.93-.02L20 5.5h-2.3l-1.86 6.4-2.6-7.2h-2.48l-2.6 7.2L6.3 5.5H4Z"
          />
        </svg>
      </div>
    );
  }
  if (id === "codebuddy-vscode") {
    return (
      <div className={cn(base, "bg-foreground")}>
        <svg viewBox="0 0 24 24" className="size-6 text-background" aria-hidden="true">
          <path fill="currentColor" d="M12 2 4 6.2v11.6L12 22l8-4.2V6.2L12 2Zm0 3.4 4.6 2.4v8.4L12 18.6l-4.6-2.4V7.8L12 5.4Z" />
        </svg>
      </div>
    );
  }
  return (
    <div className={cn(base, "bg-[oklch(0.55_0.19_275)]")}>
      <svg viewBox="0 0 24 24" className="size-6 text-white" aria-hidden="true">
        <path fill="currentColor" d="M5 4h4v4H5V4Zm10 0h4v4h-4V4ZM5 16h4v4H5v-4Zm10 0h4v4h-4v-4ZM11 11h2v2h-2v-2Z" />
      </svg>
    </div>
  );
}

// -----------------------------------------------------------------------------
// 工具
// -----------------------------------------------------------------------------

/** 绝对时间 MM/DD HH:MM（会话库时间戳是毫秒）。 */
function formatAbsolute(ms: number): string {
  if (!ms) return "—";
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}/${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/**
 * 组同步反馈（文案对齐 switch 的 notifyResult）：
 * 「已同步 N 个会话」「有 N 项跳过」「部分会话同步失败」，都没有时说「当前没有需要同步的副本」。
 */
function notifyGroupSync(report: GroupSyncReport, labelOf?: (memberId: string) => string) {
  const synced = report.synced?.length ?? 0;
  const skipped = report.skipped?.length ?? 0;
  const errors = report.errors ?? [];
  const name = (id: string) => (labelOf ? labelOf(id) : id);
  /** 逐项列表：最多列 3 条，其余折叠成「等 N 项」—— 完整列表在组详情里能看到。 */
  const detailOf = (items: { member: string; text: string }[]) => {
    const head = items.slice(0, 3).map((it) => `${name(it.member)}：${it.text}`).join("；");
    return items.length > 3 ? `${head}；等 ${items.length} 项` : head;
  };
  if (synced > 0) notifySuccess(`已同步 ${synced} 个会话`, { description: `完成 ${synced} 项` });
  if (skipped > 0) {
    // **带上是谁、为什么**：只报「有 N 项跳过」的话，用户无法判断是自己选错了来源，
    // 还是某个账号的正文坏了（这两件事的处置完全不同）。
    notifyWarning(`有 ${skipped} 项跳过`, {
      description: detailOf((report.skipped ?? []).map((item) => ({ member: item.member_id, text: item.reason }))),
    });
  }
  if (errors.length > 0) {
    notifyError("部分会话同步失败", {
      description: detailOf(errors.map((item) => ({ member: item.member_id, text: item.error }))),
    });
  }
  if (report.needs_recovery) {
    // 留下需要人工处理的现场时，客户端会**暂停重开** —— 必须让用户知道
    // （否则他会以为「同步完了但客户端没打开」是另一个 bug）。
    notifyWarning("会话写入待恢复，客户端已暂停重开", {
      description: "请在「关联会话」页点「立即恢复」，并先退出对应客户端。",
    });
  }
  if (synced === 0 && skipped === 0 && errors.length === 0 && !report.needs_recovery) {
    notifyInfo("当前没有需要同步的副本");
  }
}

// -----------------------------------------------------------------------------
// 列表卡片
// -----------------------------------------------------------------------------

function GroupCard({
  group,
  selected,
  onOpen,
  onActions,
}: {
  group: LocalSessionGroup;
  selected: boolean;
  onOpen: () => void;
  /** 打开「更多操作」（查看成员与同步 / 删除关联）。 */
  onActions: () => void;
}) {
  const style = GROUP_STATUS_STYLE[group.status] ?? GROUP_STATUS_STYLE.unknown;
  const memberNames = (group.members ?? []).map((m) => m.label).join("、");
  return (
    <div className="relative min-w-0">
      <button
        type="button"
        aria-pressed={selected}
        onClick={onOpen}
        className={cn(
          "w-full min-w-0 cursor-pointer rounded-xl border bg-card px-4 py-3.5 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          selected ? "border-primary shadow-sm" : "border-border hover:border-primary/50",
        )}
      >
        <div className="flex min-w-0 items-center justify-between gap-2 pr-7 text-xs text-muted-foreground">
          <span className="flex min-w-0 items-center gap-1.5">
            <Folder className="size-3.5 shrink-0" aria-hidden="true" />
            <span className="truncate" title={group.project}>
              {group.project || "未标记项目"}
            </span>
          </span>
          <span className="shrink-0" title={formatAbsolute(group.updated_at)}>
            {relativeTime(group.updated_at)}
          </span>
        </div>
        <p className="my-3 truncate text-[15px] font-semibold leading-6" title={group.title}>
          {group.title || "（无标题）"}
        </p>
        <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
          <span className="flex min-w-0 items-center gap-1.5" title={memberNames}>
            <MessageSquare className="size-3.5 shrink-0" aria-hidden="true" />
            {group.members?.length ?? 0} 个账号
          </span>
          <span className="flex min-w-0 items-center gap-1.5">
            <span className={cn("size-1.5 shrink-0 rounded-full", style.dot)} aria-hidden="true" />
            <span className={cn("truncate", style.text)} title={group.reason}>
              {style.label}
            </span>
          </span>
        </div>
      </button>
      {/* 「…」不能嵌在整卡按钮里（button 不能嵌套），所以绝对定位到右上角。
          面板没有 dropdown 原语，用一个小对话框承载「查看成员与同步 / 删除关联」
        （switch 是 DropdownMenu；这里保持本仓库的原语约束）。 */}
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="absolute right-2 top-2.5 size-6 text-muted-foreground hover:text-foreground"
        aria-label={`${group.title || "会话组"} 的操作`}
        title="更多操作"
        onClick={(e) => {
          e.stopPropagation();
          onActions();
        }}
      >
        <MoreHorizontal className="size-4" aria-hidden="true" />
      </Button>
    </div>
  );
}

/** 加载骨架屏：4 张卡片（与 switch 的 GroupSkeleton 对齐）。 */
function GroupSkeleton() {
  return (
    <div className="grid min-w-0 grid-cols-1 gap-3 sm:grid-cols-2" aria-label="正在加载会话组" aria-busy="true">
      {Array.from({ length: 4 }, (_, index) => (
        <Card key={index} className="gap-3 rounded-xl py-3 shadow-none">
          <div className="space-y-3 px-4">
            <Skeleton className="h-4 w-2/3" />
            <Skeleton className="h-3 w-1/2" />
            <Skeleton className="h-7 w-full" />
          </div>
        </Card>
      ))}
    </div>
  );
}

// -----------------------------------------------------------------------------
// 成员卡的「副本详情」折叠区
// -----------------------------------------------------------------------------

/**
 * 副本详情：展开后显示「最近 N 条可读内容」预览 + 工作区 + 判定原因。
 *
 * 对照 switch 的 Collapsible：预览的用途是**让人确认这是哪段对话**再决定以谁为准，
 * 所以只给最近 6 条、每条截断到 240 字符（后端已截断），不在这里读全文。
 * 面板没有 collapsible 原语，用一个受控的按钮 + 条件渲染实现同样语义。
 */
function MemberDetailDisclosure({
  member,
  project,
}: {
  member: LocalSessionGroupMember;
  project: string;
}) {
  const [open, setOpen] = useState(false);
  const preview = member.content_preview ?? [];
  return (
    <div className="min-w-0">
      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="h-5 gap-0.5 px-0 text-[11px]"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        {member.readable && preview.length > 0 ? "查看内容" : "副本详情"}
        <ChevronDown className={cn("size-3 transition-transform", open && "rotate-180")} aria-hidden="true" />
      </Button>
      {open ? (
        <div className="mt-1.5 space-y-1.5 rounded-md bg-muted/60 p-2 text-[11px] leading-4 text-muted-foreground">
          {preview.length > 0 ? (
            <>
              <p>最近 {preview.length} 条可读内容：</p>
              {preview.map((item, index) => (
                <p key={index} className="break-words">
                  <span className="font-medium text-foreground">{item.speaker}：</span>
                  {item.text}
                </p>
              ))}
            </>
          ) : (
            <p>此副本暂无可展示的内容预览，请在对应客户端查看完整会话。</p>
          )}
          <p className="break-words">{project || "未标记工作区"}</p>
        </div>
      ) : null}
    </div>
  );
}

// -----------------------------------------------------------------------------
// 详情弹窗
// -----------------------------------------------------------------------------

interface GroupDetailDialogProps {
  groupId: string | null;
  onClose: () => void;
  /** 组内容变化后通知父层刷新列表；组 ID 变化（新副本 id 更小）时一并回传。 */
  onGroupChanged: (nextGroupId?: string) => void;
}

/**
 * 组详情：状态摘要 + 会话关联图 + 成员清单 + 「以此为准」/批量同步/「关联新账号」/「取消关联」。
 *
 * 可见行为对齐 wb-switch 的 `session-group-detail.tsx`。
 * 「取消关联」在本项目里的实现是**记一条排除项**（见 localsessions/unlink.go）：
 * 组是按内容推导的、没有登记项可删，所以解除 = 这条会话不再参与分组。
 * 对用户可见的效果与 switch 一致：该账号不再出现在组里、不再参与同步，会话内容不动。
 */
function GroupDetailDialog({ groupId, onClose, onGroupChanged }: GroupDetailDialogProps) {
  const [detail, setDetail] = useState<LocalSessionGroupDetailResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  /** 执行中的操作：`batch` / `add` / 某个成员的 session_id（以此为准）。 */
  const [busy, setBusy] = useState<string | null>(null);
  /** 「以此为准」的预览计划（逐目标的判定/模式/阻断项）。 */
  const [unifyPlan, setUnifyPlan] = useState<UnifyPlan | null>(null);
  const [unlinkTarget, setUnlinkTarget] = useState<LocalSessionGroupMember | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [addSourceId, setAddSourceId] = useState("");
  const [addTargetUid, setAddTargetUid] = useState("");

  const load = useCallback(async () => {
    if (!groupId) return;
    setLoading(true);
    setError(null);
    try {
      const res = await fetchSessionGroup(groupId);
      setDetail(res);
      // 默认来源 = 安全源（若有），否则第一个可读成员；目标默认第一个可添加账号。
      const safe = res.group.safe_source_member_id;
      const firstReadable = res.group.members.find((m) => m.readable)?.session_id ?? "";
      setAddSourceId(safe || firstReadable);
      setAddTargetUid(res.add_targets[0]?.uid ?? "");
    } catch (err) {
      setDetail(null);
      setError(describeError(err));
    } finally {
      setLoading(false);
    }
  }, [groupId]);

  useEffect(() => {
    void load();
  }, [load]);

  // 关闭时清空一次性状态，下次打开从干净状态开始。
  useEffect(() => {
    if (!groupId) {
      setDetail(null);
      setError(null);
      setLoading(false);
      setBusy(null);
      setUnifyPlan(null);
      setUnlinkTarget(null);
      setAddOpen(false);
      setAddSourceId("");
      setAddTargetUid("");
    }
  }, [groupId]);

  const group = detail?.group ?? null;

  /** 成员显示名（toast 里要能指名道姓）。 */
  const memberLabelOf = (memberId: string) =>
    (detail?.group.members ?? []).find((m) => m.session_id === memberId)?.label ?? memberId;

  /**
   * 同步/复制后「跟着组走」。
   *
   * 组 ID 是**组内最小的 sessionId**（见后端 groups.go），复制出新副本时可能变小、
   * 组内成员被同步成另一种内容时还可能分裂 —— 按旧 ID 重新拉取会静默显示成另一个组
   *（成员「凭空消失」）。这里用「旧成员 id 还在不在某个组里」把它找回来。
   */
  async function followGroup(knownMemberIds: string[]): Promise<{ ok: boolean; found?: string }> {
    if (knownMemberIds.length === 0) return { ok: false };
    try {
      const res = await fetchSessionGroups();
      for (const g of res.groups ?? []) {
        if ((g.members ?? []).some((m) => knownMemberIds.includes(m.session_id))) {
          return { ok: true, found: g.id };
        }
      }
      return { ok: true };
    } catch {
      return { ok: false };
    }
  }

  /** 统一执行（report 的逐项反馈由 notifyGroupSync 负责）。 */
  async function runSync(
    params: {
      sourceMemberId: string;
      targets: { memberId: string; mode: string; previewToken: string }[];
    },
    busyKey: string,
  ) {
    if (!groupId || busy) return;
    setBusy(busyKey);
    try {
      const known = members.map((m) => m.session_id);
      const report = await syncSessionGroupUnify(groupId, params);
      notifyGroupSync(report, memberLabelOf);
      setUnifyPlan(null);
      const followed = await followGroup(known);
      if (followed.ok && !followed.found) {
        // 组已经不存在（成员被同步成互不相干的内容而各自成组，或都被解除关联）。
        notifyInfo("这个会话组已经不存在了（成员内容已不再同源）");
        onClose();
        return;
      }
      if (followed.found && followed.found !== groupId) {
        onGroupChanged(followed.found);
        return;
      }
      await load();
      onGroupChanged();
    } catch (err) {
      notifyError("同步失败", { description: describeError(err) });
    } finally {
      setBusy(null);
    }
  }

  /** 取消某个成员的关联（只解除分组关系，不碰会话内容）。 */
  async function runUnlink() {
    if (!group || !unlinkTarget || busy) return;
    setBusy("unlink");
    try {
      // 登记表按 **member_id** 解除（session_id 只是会话本身，成员是它在组里的登记项）。
      const res = await unlinkSessionGroupMember(group.id, unlinkTarget.member_id);
      notifySuccess(`已取消「${unlinkTarget.label}」的关联`, {
        description: res.group_removed
          ? "该会话组已不足两份副本，不再显示在关联会话列表中；账号里的会话内容未改动。"
          : "账号里的会话内容未改动。",
      });
      setUnlinkTarget(null);
      if (res.group_removed) {
        onGroupChanged();
        onClose();
        return;
      }
      await load();
      onGroupChanged();
    } catch (err) {
      notifyError("取消关联失败", { description: describeError(err) });
    } finally {
      setBusy(null);
    }
  }

  /** 批量同步落后账号：后端重算安全源并逐对复核 fast-forward。 */
  async function runSafeBatch() {
    if (!groupId || busy) return;
    setBusy("batch");
    try {
      const known = members.map((m) => m.session_id);
      const report = await syncSessionGroupSafeBatch(groupId);
      notifyGroupSync(report, memberLabelOf);
      const followed = await followGroup(known);
      if (followed.ok && !followed.found) {
        notifyInfo("这个会话组已经不存在了");
        onClose();
        return;
      }
      if (followed.found && followed.found !== groupId) {
        onGroupChanged(followed.found);
        return;
      }
      await load();
      onGroupChanged();
    } catch (err) {
      notifyError("批量同步失败", { description: describeError(err) });
    } finally {
      setBusy(null);
    }
  }

  /**
   * 预览「以此为准」的每个目标（对照 switch 的 prepareUnify）：
   * 逐对调 preview-pair，按可用模式决定写入模式；判定不允许写入的目标列为**阻断项**，
   * 确认框里显式列出，不静默执行。
   */
  async function prepareUnify(source: LocalSessionGroupMember) {
    if (!groupId || !group || busy) return;
    setBusy("preview");
    try {
      const others = members.filter((m) => m.member_id !== source.member_id);
      const results = await Promise.all(
        others.map(async (m) => {
          try {
            const p = await previewSessionGroupPair(groupId, {
              sourceMemberId: source.member_id,
              targetMemberId: m.member_id,
            });
            const mode = p.available_modes?.[0] ?? "";
            return { member: m, preview: p, mode };
          } catch (err) {
            return { member: m, preview: null, mode: "", error: describeError(err) };
          }
        }),
      );
      setUnifyPlan({ source, items: results });
    } catch (err) {
      notifyError("无法检查各副本状态", { description: describeError(err) });
    } finally {
      setBusy(null);
    }
  }

  async function runAdd() {
    if (!group || !addSourceId || !addTargetUid || busy) return;
    setBusy("add");
    try {
      const res = await addSessionGroupMember(group.id, {
        sourceMemberId: addSourceId,
        targetUid: addTargetUid,
      });
      const target = detail?.add_targets.find((t) => t.uid === addTargetUid);
      notifySuccess(`已复制并关联到「${target?.label ?? addTargetUid}」`);
      if (res.group_id && res.group_id !== group.id) {
        // 组 ID 变了（新副本的 sessionId 更小）：把定位切到新 ID，由父层/effect 重新拉取。
        onGroupChanged(res.group_id);
      } else {
        await load();
        onGroupChanged();
      }
    } catch (err) {
      notifyError("复制并关联失败", { description: describeError(err) });
    } finally {
      setBusy(null);
    }
  }

  // ---- 派生状态（摘要文案与可执行动作）----
  const members = group?.members ?? [];
  const readable = members.filter((m) => m.readable);
  const behindMembers = readable.filter((m) => m.version_status === "behind");
  const divergence = group?.divergence ?? null;
  const equal = group?.status === "latest" && readable.length > 0;
  const canBatch = Boolean(group?.safe_source_member_id) && behindMembers.length > 0;
  const batchSourceLabel =
    members.find((m) => m.session_id === group?.safe_source_member_id)?.label ?? "安全源";
  // 摘要文案由后端给出（与 switch 的 summaryText 逐字一致），前端不再自己拼。
  const summary =
    group?.summary_text ||
    (equal
      ? `${readable.length} 个账号内容一致`
      : canBatch
        ? `有 ${behindMembers.length} 个账号待同步`
        : divergence
          ? `${divergence.common_member_ids.length} 个账号在共同旧版，另有 ${divergence.branches} 条独立更新`
          : group?.status === "missing"
            ? "部分账号内容缺失"
            : group?.status === "diverge"
              ? `${members.length} 个账号的会话内容不一致`
              : "部分内容状态尚无法确认");
  const summaryTone =
    group?.status === "diverge"
      ? "border-destructive/35 bg-destructive/10 text-destructive"
      : canBatch
        ? "border-amber-500/30 bg-amber-500/10 text-amber-800 dark:text-amber-300"
        : equal
          ? "border-primary/25 bg-primary/8 text-primary-ink"
          : "border-border bg-muted/60 text-muted-foreground";

  // 预览计划派生量：会覆盖的目标数（overwrite / unifyOverwrite）与阻断项。
  const planItems = unifyPlan?.items ?? [];
  const planBlocked = planItems.filter((it) => it.error || !it.mode);
  const planWrites = planItems.filter((it) => it.mode);
  const planOverwrites = planWrites.filter((it) => it.mode !== "fastForward");
  const confirmOverwrite = planOverwrites.reduce(
    (sum, it) => sum + (it.preview?.target_only ?? 0),
    0,
  );
  const addTargets: SessionAddTarget[] = detail?.add_targets ?? [];

  return (
    <Dialog
      open={groupId !== null}
      onOpenChange={(open) => {
        if (!open && !busy) onClose();
      }}
    >
      {/* 宽模态框：对照 switch 的 .session-detail-modal
          （width: min(980px, 100vw-96px)；max-height: min(760px, 100dvh-112px)）。
          关联图是左右两列的布局，窄弹窗里连线会挤成一团。 */}
      <DialogContent className="flex max-h-[min(760px,calc(100dvh-112px))] w-[min(980px,calc(100vw-96px))] max-w-none flex-col overflow-hidden">
        <DialogHeader className="shrink-0">
          <DialogTitle className="truncate">{group?.title || "会话详情"}</DialogTitle>
          <DialogDescription>
            {group ? `${group.project || "未标记项目"} · ${members.length} 个账号` : "正在读取会话组…"}
            {group?.reason ? <span className="mt-1 block">{group.reason}</span> : null}
          </DialogDescription>
        </DialogHeader>

        {error ? (
          <div role="alert" className="space-y-2 rounded-lg border border-destructive/30 p-3 text-sm text-destructive">
            <p className="break-words">{error}</p>
            <Button type="button" variant="outline" size="sm" disabled={loading} onClick={() => void load()}>
              重试
            </Button>
          </div>
        ) : null}

        {!group && !error && loading ? (
          <div className="space-y-3" aria-label="正在加载会话详情" aria-busy="true">
            <Skeleton className="h-6 w-3/4" />
            <Skeleton className="h-12 w-full" />
            <Skeleton className="h-40 w-full" />
          </div>
        ) : null}

        {group ? (
          <div className="min-h-0 flex-1 space-y-3 overflow-y-auto pr-0.5">
            {/* 状态摘要 + 重新检查 */}
            <div className={cn("flex items-start justify-between gap-2 rounded-lg border p-3", summaryTone)}>
              <div className="min-w-0">
                <p className="text-sm font-medium leading-5">{summary}</p>
                {equal && readable[0] ? (
                  <p className="mt-0.5 text-xs opacity-80">
                    {readable[0].record_count} 条内容 · 无需同步
                  </p>
                ) : null}
              </div>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="size-6 shrink-0"
                aria-label="重新检查会话状态"
                title="重新检查"
                disabled={loading || busy !== null}
                onClick={() => {
                  void load();
                  // 检查出的新状态/新计数要同步到背后的列表，否则关掉弹窗看到的还是旧值。
                  onGroupChanged();
                }}
              >
                <RefreshCw className={cn("size-3.5", loading && "animate-spin")} aria-hidden="true" />
              </Button>
            </div>

            {/* 会话关联图：一眼看清「谁和谁同源、谁各自往下写了」 */}
            <SessionLinkGraph group={group} memberLabel={(m) => m.label} />

            {/* 成员清单 */}
            <div className="flex items-center justify-between gap-2">
              <h3 className="text-sm font-semibold">关联账号副本</h3>
              <span className="rounded-full bg-muted px-2 py-0.5 text-xs tabular-nums text-muted-foreground">
                {members.length} 个账号
              </span>
            </div>
            <ul className="space-y-1.5">
              {members.map((m) => {
                const meta = MEMBER_STATUS_META[m.version_status] ?? MEMBER_STATUS_META.unknown;
                const label = m.version_status === "latest" && equal ? "内容一致" : meta.label;
                return (
                  <li key={m.session_id} className="rounded-lg border border-border px-3 py-2.5">
                    <div className="flex min-w-0 items-start justify-between gap-2">
                      <p className="flex min-w-0 flex-wrap items-center gap-1.5">
                        <span className="min-w-0 break-words text-sm font-medium">{m.label}</span>
                        {/* 跨库分组后成员来自两个客户端，档位要标出来（对照 switch 的 INTL 徽标）。 */}
                        <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
                          {m.variant === "intl" ? "国际站" : "国内站"}
                        </span>
                      </p>
                      <Badge variant={meta.variant} className="shrink-0">
                        {label}
                      </Badge>
                    </div>
                    <p className="mt-0.5 text-xs tabular-nums text-muted-foreground">
                      {m.readable ? `${m.record_count} 条内容` : "内容条数无法确认"}
                    </p>
                    <div className="mt-0.5 flex flex-wrap items-center justify-between gap-x-2">
                      <span className="text-[11px] text-muted-foreground" title={formatAbsolute(m.updated_at)}>
                        {formatAbsolute(m.updated_at) === "—" ? "更新时间未知" : `更新于 ${formatAbsolute(m.updated_at)}`}
                      </span>
                      <MemberDetailDisclosure member={m} project={group.project} />
                    </div>
                    <div className="mt-1.5 flex flex-wrap items-center justify-between gap-2">
                      <span className="text-[11px] text-muted-foreground">{m.reason}</span>
                      <span className="flex items-center gap-1.5">
                        {m.version_status !== "latest" ? (
                          <Button
                            type="button"
                            variant="outline"
                            size="sm"
                            className="h-7 px-2 text-xs"
                            disabled={busy !== null || !m.readable}
                            title={m.readable ? "以这份内容为准统一其他账号" : "正文不可读，无法作为来源"}
                            onClick={() => void prepareUnify(m)}
                          >
                            {m.version_status === "behind" ? "以旧版为准" : "以此为准"}
                          </Button>
                        ) : null}
                        {/* 取消关联收在「…」菜单里（对照 switch 的 DropdownMenu）。 */}
                        <DropdownMenu>
                          <DropdownMenuTrigger asChild>
                            <Button
                              type="button"
                              variant="ghost"
                              size="icon"
                              className="size-6 text-muted-foreground hover:text-foreground"
                              aria-label={`${m.label} 的操作`}
                              title="更多操作"
                              disabled={busy !== null}
                            >
                              <MoreHorizontal className="size-3.5" aria-hidden="true" />
                            </Button>
                          </DropdownMenuTrigger>
                          <DropdownMenuContent align="end" className="w-36">
                            <DropdownMenuItem
                              disabled={busy !== null || members.length < 2}
                              title={
                                members.length < 2
                                  ? "组内只剩这一份副本，无需解除"
                                  : "该账号不再属于这个会话组（会话内容不会被删除）"
                              }
                              onSelect={() => setUnlinkTarget(m)}
                            >
                              <Unlink />
                              取消关联
                            </DropdownMenuItem>
                          </DropdownMenuContent>
                        </DropdownMenu>
                      </span>
                    </div>
                  </li>
                );
              })}
            </ul>

            {/* 批量同步（有安全源时显示） */}
            {canBatch ? (
              <div className="space-y-1.5 border-t border-border/70 pt-3">
                <Button
                  type="button"
                  size="sm"
                  className="w-full"
                  disabled={busy !== null}
                  onClick={() => void runSafeBatch()}
                >
                  <RefreshCw className={cn("size-3.5", busy === "batch" && "animate-spin")} aria-hidden="true" />
                  同步到 {behindMembers.length} 个落后账号
                </Button>
                <p
                  className="truncate text-center text-[11px] text-muted-foreground"
                  title={`${batchSourceLabel} → ${behindMembers.map((m) => m.label).join("、")}`}
                >
                  {batchSourceLabel} → {behindMembers.length} 个账号
                </p>
              </div>
            ) : null}

            {/* 关联新账号：Popover（对照 switch 的 footer Popover） */}
            <div className="border-t border-border/70 pt-3">
              <Popover open={addOpen} onOpenChange={setAddOpen}>
                <PopoverTrigger asChild>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="w-full"
                    disabled={busy !== null || addTargets.length === 0}
                    title={addTargets.length === 0 ? "没有可添加的兼容账号" : "把这份会话复制到另一个账号并建立关联"}
                  >
                    <Link2 className="size-3.5" aria-hidden="true" />
                    关联新账号
                  </Button>
                </PopoverTrigger>
                <PopoverContent side="top" align="center" aria-label="关联新账号" className="w-[min(304px,calc(100vw-32px))] space-y-2.5 p-3">
                  <p className="text-[11px] leading-4 text-muted-foreground">
                    {equal
                      ? "选择任一可读取的副本，复制到新账号并建立关联。"
                      : "将所选账号的当前副本复制到新账号，并建立关联。"}
                  </p>
                  <label className="block space-y-1">
                    <span className="text-[11px] font-medium">复制来源</span>
                    <select
                      aria-label="选择复制来源"
                      className="w-full rounded-md border border-border bg-background px-2 py-1.5 text-xs"
                      value={addSourceId}
                      disabled={busy !== null}
                      onChange={(e) => setAddSourceId(e.target.value)}
                    >
                      {readable.map((m) => (
                        <option key={m.session_id} value={m.session_id}>
                          {m.label}
                        </option>
                      ))}
                    </select>
                  </label>
                  <label className="block space-y-1">
                    <span className="text-[11px] font-medium">目标账号</span>
                    <select
                      aria-label="目标关联账号"
                      className="w-full rounded-md border border-border bg-background px-2 py-1.5 text-xs"
                      value={addTargetUid}
                      disabled={busy !== null || addTargets.length === 0}
                      onChange={(e) => setAddTargetUid(e.target.value)}
                    >
                      {addTargets.length === 0 ? <option value="">没有可添加的兼容账号。</option> : null}
                      {addTargets.map((t) => (
                        <option key={t.uid} value={t.uid}>
                          {t.label}
                          {t.site === "intl" ? " · 国际站" : " · 国内站"}
                        </option>
                      ))}
                    </select>
                  </label>
                  {addTargets.length === 0 ? (
                    <p className="text-[11px] text-muted-foreground">没有可添加的兼容账号。</p>
                  ) : null}
                  <Button
                    type="button"
                    size="sm"
                    className="w-full"
                    disabled={busy !== null || !addSourceId || !addTargetUid}
                    onClick={() => void runAdd()}
                  >
                    {busy === "add" ? (
                      <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                    ) : (
                      <Copy className="size-3.5" aria-hidden="true" />
                    )}
                    {busy === "add" ? "处理中…" : "复制并关联"}
                  </Button>
                </PopoverContent>
              </Popover>
            </div>
          </div>
        ) : null}

        {/* 「以此为准」确认框：**先预览再确认**（对照 switch 的 prepareUnify/confirmUnify） */}
        <AlertDialog
          open={unifyPlan !== null}
          onOpenChange={(open) => {
            if (!open && !busy) setUnifyPlan(null);
          }}
        >
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>以「{unifyPlan?.source.label}」的内容为准？</AlertDialogTitle>
              <AlertDialogDescription>
                将这份会话内容统一到其他 {planItems.length} 个账号。内容相同的账号会跳过。
              </AlertDialogDescription>
            </AlertDialogHeader>

            {/* 逐目标预览：会怎么处理 / 为什么不能处理 */}
            <ul className="max-h-56 space-y-1.5 overflow-y-auto">
              {planItems.map((it) => (
                <li
                  key={it.member.member_id}
                  className="rounded-lg border border-border px-3 py-2 text-xs"
                >
                  <div className="flex items-center justify-between gap-2">
                    <span className="min-w-0 truncate font-medium">{it.member.label}</span>
                    <span
                      className={cn(
                        "shrink-0",
                        it.error || !it.mode ? "text-destructive" : "text-muted-foreground",
                      )}
                    >
                      {it.error
                        ? "无法检查内容"
                        : !it.mode
                          ? "无法安全更新此副本"
                          : MODE_LABEL[it.mode] ?? it.mode}
                    </span>
                  </div>
                  <p className="mt-0.5 leading-4 text-muted-foreground">
                    {it.error ?? it.preview?.reason}
                  </p>
                </li>
              ))}
            </ul>

            {planBlocked.length > 0 ? (
              <p className="text-sm text-destructive">
                目前无法统一全部账号：{planBlocked.map((it) => it.member.label).join("、")} 会被跳过。
              </p>
            ) : confirmOverwrite > 0 ? (
              <p className="text-sm text-destructive">
                其中 {planOverwrites.length} 个账号有独有内容（{confirmOverwrite} 条）；确认后，
                它们现有的会话内容会被完整替换。
              </p>
            ) : (
              <p className="text-xs text-muted-foreground">
                执行前会重新校验各副本内容；内容已变化的项会停止并报告结果。
              </p>
            )}

            <AlertDialogFooter>
              <AlertDialogCancel disabled={busy !== null}>取消</AlertDialogCancel>
              <Button
                type="button"
                variant={confirmOverwrite > 0 ? "destructive" : "default"}
                disabled={busy !== null || !unifyPlan || planWrites.length === 0}
                onClick={() =>
                  void runSync(
                    {
                      sourceMemberId: unifyPlan?.source.member_id ?? "",
                      targets: planWrites.map((it) => ({
                        memberId: it.member.member_id,
                        mode: it.mode,
                        previewToken: it.preview?.preview_token ?? "",
                      })),
                    },
                    "unify",
                  )
                }
              >
                {busy !== null ? "处理中…" : confirmOverwrite > 0 ? "确认统一" : "确认同步"}
              </Button>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>

        {/* 「取消关联」确认框 */}
        <AlertDialog
          open={unlinkTarget !== null}
          onOpenChange={(open) => {
            if (!open && !busy) setUnlinkTarget(null);
          }}
        >
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>取消「{unlinkTarget?.label}」的关联？</AlertDialogTitle>
              <AlertDialogDescription>
                该账号不再属于这个会话组，不再参与同步；账号里的会话内容不会被删除。
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={busy !== null}>取消</AlertDialogCancel>
              <Button
                type="button"
                variant="destructive"
                disabled={busy !== null || !unlinkTarget}
                onClick={() => void runUnlink()}
              >
                {busy === "unlink" ? "处理中…" : "确认取消关联"}
              </Button>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </DialogContent>
    </Dialog>
  );
}

// -----------------------------------------------------------------------------
// 页面
// -----------------------------------------------------------------------------

export interface LinkedSessionsPageProps {
  accounts: Account[];
}

/**
 * 关联会话页（对齐 switch 的同名页面）。
 *
 * 数据来自 `/panel/api/local-sessions/groups`：按**内容**把同源副本聚成组，
 * 状态（内容一致 / 待同步 / 有分歧 / 内容缺失）与同步判定共用一套口径。
 * **只展示存在于两个及以上账号的副本** —— 单份会话不是「关联」，
 * 它们仍在「本机会话库」里可见（否则这一页会把每个普通会话都列成一个组）。
 *
 * 与 switch 的布局一一对应：成员卡的「…」是 DropdownMenu、「关联新账号」是 footer 里的
 * Popover、详情是宽模态框（980px）且带「最近 N 条可读内容」预览。
 */
export function LinkedSessionsPage({ accounts }: LinkedSessionsPageProps) {
  const [data, setData] = useState<Awaited<ReturnType<typeof fetchSessionGroups>> | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [platform, setPlatform] = useState<string>("workbuddy");
  const [filter, setFilter] = useState<FilterID>("all");
  const [sortOrder, setSortOrder] = useState<"recent" | "oldest">("recent");
  const [page, setPage] = useState(1);
  const [detailId, setDetailId] = useState<string | null>(null);
  /** 卡片的「更多操作」对话框（查看成员与同步 / 删除关联）。 */
  const [actionsTarget, setActionsTarget] = useState<LocalSessionGroup | null>(null);
  /** 待确认删除的会话组。 */
  const [deleteTarget, setDeleteTarget] = useState<LocalSessionGroup | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [addDialogOpen, setAddDialogOpen] = useState(false);

  const load = useCallback(async (signal?: AbortSignal) => {
    // 首屏之后的每次刷新也要有反馈：不置 true 的话按钮永远不转、也不禁用，
    // 连点会并发发请求（早期版本的 `disabled={loading}` 因此形同虚设）。
    setLoading(true);
    try {
      const res = await fetchSessionGroups(signal);
      setData(res);
      setLoadError(null);
    } catch (err) {
      // 卸载/切换造成的 abort 不是失败，不要弹红色横幅。
      if ((err as { name?: string })?.name === "AbortError") return;
      // 网络/鉴权这类真失败才走这里；会话库读不出来由后端下发 store_status/store_error，
      // 页面显示琥珀横幅而不是整页错误态。
      setLoadError(describeError(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    const c = new AbortController();
    void load(c.signal);
    return () => c.abort();
  }, [load]);

  const [recovering, setRecovering] = useState(false);

  /**
   * 恢复未完成的会话写入。
   *
   * 后端会先确认对应客户端**没有在运行**（运行中返回 409 并说明），
   * 所以这里不需要前端自己判断进程状态。
   */
  async function runRecover() {
    if (recovering) return;
    setRecovering(true);
    try {
      const res = await recoverLocalSessions(pendingRecovery.variants);
      const reports = Object.entries(res.reports ?? {});
      const recovered = reports.reduce((n, [, r]) => n + (r.recovered?.length ?? 0), 0);
      const abandoned = reports.reduce((n, [, r]) => n + (r.abandoned?.length ?? 0), 0);
      const needs = reports.flatMap(([, r]) => r.needs_recovery ?? []);
      if (needs.length > 0) {
        notifyError("有会话写入需要人工处理", {
          description: needs.map((n) => n.reason).slice(0, 3).join("；"),
        });
      } else if (recovered + abandoned > 0) {
        notifySuccess(`已恢复 ${recovered} 项会话写入`, {
          description: abandoned > 0 ? `另有 ${abandoned} 项确认未写入，已安全放弃` : undefined,
        });
      } else {
        notifyInfo(res.note ?? "没有需要恢复的内容");
      }
      await load();
    } catch (err) {
      notifyError("恢复失败", { description: describeError(err) });
    } finally {
      setRecovering(false);
    }
  }

  const groups = data?.groups ?? [];
  const counts = useMemo<GroupCounts>(
    () => data?.counts ?? { all: 0, behind: 0, diverge: 0, latest: 0, missing: 0, unknown: 0 },
    [data],
  );
  const storeStatus = data?.store_status ?? "ok";
  const storeError = data?.store_error ?? "";
  const pendingRecovery = data?.pending_recovery ?? { count: 0, variants: [], unparseable: false, details: [] };

  // 筛选 + 排序（含标题 tie-break，保证同时间戳下顺序稳定）。
  const filtered = useMemo(() => {
    const list = filter === "all" ? groups : groups.filter((g) => g.status === filter);
    return [...list].sort((a, b) => {
      const diff = sortOrder === "oldest" ? a.updated_at - b.updated_at : b.updated_at - a.updated_at;
      if (diff !== 0) return diff;
      return (a.title || "").localeCompare(b.title || "");
    });
  }, [groups, filter, sortOrder]);

  const pageCount = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const currentPage = Math.min(page, pageCount);
  const visible = filtered.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE);
  useEffect(() => {
    setPage(1);
  }, [filter, sortOrder, platform]);

  // 分页页码集合：首尾 + 当前页 ±1（与 switch 同款省略号）。
  const pages = useMemo(
    () =>
      Array.from({ length: pageCount }, (_, i) => i + 1).filter(
        (value) => value === 1 || value === pageCount || Math.abs(value - currentPage) <= 1,
      ),
    [pageCount, currentPage],
  );

  const visibleFilters = FILTERS.filter(
    (f) => !CONDITIONAL_FILTERS.has(f.id) || filterCount(counts, f.id) > 0 || filter === f.id,
  );
  const activePlatform = PLATFORMS.find((p) => p.id === platform) ?? PLATFORMS[0];

  function closeDetail() {
    setDetailId(null);
  }

  function handleGroupChanged(nextGroupId?: string) {
    void load();
    if (nextGroupId && nextGroupId !== detailId) setDetailId(nextGroupId);
  }

  /** 删除关联：解除组内全部成员的关联（**不删会话内容**）。 */
  async function handleDeleteGroup() {
    if (!deleteTarget || deleting) return;
    setDeleting(true);
    try {
      const res = await deleteSessionGroup(deleteTarget.id);
      notifySuccess(`已删除「${deleteTarget.title || "该会话组"}」的关联`, {
        description:
          (res.notes ?? []).join("；") ||
          `共解除 ${res.removed ?? 0} 个账号的关联，账号内的会话内容不受影响`,
      });
      if (detailId === deleteTarget.id) setDetailId(null);
      setDeleteTarget(null);
      void load();
    } catch (err) {
      notifyError("删除关联失败", { description: describeError(err) });
    } finally {
      setDeleting(false);
    }
  }

  return (
    <div className="p-6 lg:p-8">
      {/* ── 页头 ── */}
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-3xl font-bold tracking-tight">关联会话</h1>
          <p className="mt-2 text-sm text-muted-foreground">统一管理各账号下的会话副本，按需复制或同步。</p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button type="button" variant="outline" size="sm" disabled={loading} onClick={() => void load()}>
            <RefreshCw className={cn("size-4", loading && "animate-spin")} aria-hidden="true" />
            刷新
          </Button>
          <Button
            type="button"
            size="sm"
            disabled={accounts.length === 0}
            title={accounts.length === 0 ? "账号库里还没有账号" : "把某个账号的会话复制到另一个账号并建立关联"}
            onClick={() => setAddDialogOpen(true)}
          >
            <Link2 className="size-4" aria-hidden="true" />
            新增关联会话
          </Button>
        </div>
      </div>

      {/* ── 平台卡 ── */}
      <div className="mt-6 grid grid-cols-1 gap-4 sm:grid-cols-3">
        {PLATFORMS.map((p) => {
          const selected = platform === p.id;
          const clickable = p.enabled;
          return (
            <button
              key={p.id}
              type="button"
              disabled={!clickable}
              aria-pressed={selected}
              title={clickable ? undefined : p.disabledNote}
              onClick={() => clickable && setPlatform(p.id)}
              className={cn(
                "relative flex items-center gap-3 rounded-2xl border bg-card p-4 text-left transition-colors",
                selected ? "border-primary bg-primary/4" : "border-border",
                clickable ? "hover:bg-accent/60" : "cursor-not-allowed",
              )}
            >
              {/* **满色显示**，不做灰化：参考截图里未选中的平台也是满色的，
                  把它们置灰等于改变了设计。未实现的状态靠
                  「不可点击 + hover 提示」表达。 */}
              <PlatformLogo id={p.id} />
              <span className="min-w-0 flex-1">
                <span className="block truncate text-[15px] font-semibold">{p.title}</span>
                <span className="mt-0.5 block truncate text-xs text-muted-foreground">{p.subtitle}</span>
              </span>
              {selected ? (
                <span className="absolute right-3 top-3 flex size-5 items-center justify-center rounded-full bg-primary text-primary-foreground">
                  <Check className="size-3" aria-hidden="true" strokeWidth={3} />
                </span>
              ) : null}
            </button>
          );
        })}
      </div>

      {/* ── 分组头 ── */}
      <div className="mt-8 flex items-center gap-2">
        <h2 className="text-base font-semibold">{activePlatform.title} 会话</h2>
        <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
          {counts.all}
        </span>
      </div>

      {/* ── 筛选段 + 排序 ── */}
      <div className="mt-4 flex flex-wrap items-center justify-between gap-3">
        <div className="inline-flex max-w-full flex-wrap items-center gap-1 rounded-xl bg-muted p-1" role="tablist" aria-label="按状态筛选">
          {visibleFilters.map((f) => {
            const active = filter === f.id;
            return (
              <button
                key={f.id}
                type="button"
                role="tab"
                aria-selected={active}
                onClick={() => setFilter(f.id)}
                className={cn(
                  "rounded-lg px-3 py-1.5 text-sm transition-colors",
                  active ? "bg-card font-medium shadow-sm" : "text-muted-foreground hover:text-foreground",
                )}
              >
                {f.label}
                <span className={cn("ml-1.5 text-xs", active ? "text-muted-foreground" : "text-muted-foreground/70")}>
                  {filterCount(counts, f.id)}
                </span>
              </button>
            );
          })}
        </div>

        {/* 排序：面板没有 Radix Select，用原生 select（切换语义与开关一致）。 */}
        <label className="inline-flex cursor-pointer items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-sm text-muted-foreground hover:bg-accent hover:text-foreground">
          <SlidersHorizontal className="size-3.5" aria-hidden="true" />
          <select
            aria-label="排序方式"
            className="cursor-pointer bg-transparent text-sm focus-visible:outline-none"
            value={sortOrder}
            onChange={(e) => setSortOrder(e.target.value === "oldest" ? "oldest" : "recent")}
          >
            <option value="recent">最近更新</option>
            <option value="oldest">最早更新</option>
          </select>
        </label>
      </div>

      {/* ── 存储状态横幅（会话库目录不可读）── */}
      {storeStatus === "unavailable" && storeError ? (
        <div
          role="status"
          className="mt-3 flex flex-wrap items-center gap-2 rounded-lg border border-amber-500/30 bg-amber-500/8 p-3 text-sm text-muted-foreground"
        >
          <span className="min-w-0 flex-1">{storeError}</span>
          <Button type="button" variant="outline" size="sm" disabled={loading} onClick={() => void load()}>
            重试
          </Button>
        </div>
      ) : null}

      {/* ── 待恢复横幅（上次复制/同步中途被打断）── */}
      {pendingRecovery.count > 0 ? (
        <div
          role="status"
          className="mt-3 flex flex-wrap items-center gap-2 rounded-lg border border-amber-500/30 bg-amber-500/8 p-3 text-sm text-muted-foreground"
        >
          <span className="min-w-0 flex-1">
            有 {pendingRecovery.count} 项会话写入没有完成
            {pendingRecovery.unparseable ? "（其中存在无法解析的操作记录，需要人工处理）" : ""}
            。恢复前请先退出对应客户端：写入会被运行中客户端的退出回写覆盖。
          </span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={loading || recovering}
            onClick={() => void runRecover()}
          >
            {recovering ? "恢复中…" : "立即恢复"}
          </Button>
        </div>
      ) : null}

      {/* ── 请求失败横幅（网络/鉴权）── */}
      {loadError ? (
        <div
          role="alert"
          className="mt-3 flex flex-wrap items-center gap-2 rounded-lg border border-destructive/30 p-3 text-sm text-destructive"
        >
          <span className="min-w-0 flex-1">{loadError}</span>
          <Button type="button" variant="outline" size="sm" disabled={loading} onClick={() => void load()}>
            重试
          </Button>
        </div>
      ) : null}

      {/* ── 卡片网格 ── */}
      {loading && groups.length === 0 ? (
        <div className="mt-4">
          <GroupSkeleton />
        </div>
      ) : filtered.length === 0 ? (
        <Card className="mt-4 items-center gap-2 rounded-2xl border-dashed py-12 shadow-none">
          <Layers className="size-6 text-muted-foreground" aria-hidden="true" />
          <p className="text-sm font-medium">{groups.length === 0 ? "还没有关联会话组" : "没有符合条件的会话"}</p>
          <p className="max-w-md px-6 text-center text-sm text-muted-foreground">
            {groups.length === 0
              ? storeStatus === "unavailable"
                ? // 读不出来时说清是「读不到」，而不是「还没复制过」——
                  // 后者会让用户去找一个根本不存在的操作。
                  (storeError || "会话库暂时读不出来，请稍后重试。")
                : data && !data.available
                  ? (data.note ?? "未找到本机会话库")
                  : "从账号切换时复制会话后，关联组会显示在这里。"
              : "试试其它筛选条件。"}
          </p>
        </Card>
      ) : (
        <div className="mt-4 grid grid-cols-1 gap-3 lg:grid-cols-2">
          {visible.map((g) => (
            <GroupCard
              key={g.id}
              group={g}
              selected={detailId === g.id}
              onOpen={() => setDetailId(g.id)}
              onActions={() => setActionsTarget(g)}
            />
          ))}
        </div>
      )}

      {/* ── 分页 ── */}
      {!loading && filtered.length > PAGINATION_HIDE_MAX ? (
        <div className="mt-6 flex flex-wrap items-center justify-between gap-3 text-xs text-muted-foreground">
          <span>
            显示 {(currentPage - 1) * PAGE_SIZE + 1}–{Math.min(currentPage * PAGE_SIZE, filtered.length)}，共{" "}
            {filtered.length} 个会话
          </span>
          <nav aria-label="会话分页" className="flex flex-wrap items-center gap-1.5">
            <Button
              type="button"
              variant="outline"
              size="icon"
              className="size-8"
              aria-label="上一页"
              disabled={currentPage <= 1}
              onClick={() => setPage(currentPage - 1)}
            >
              <ChevronLeft className="size-4" aria-hidden="true" />
            </Button>
            {pages.map((value, index) => (
              <span key={value} className="flex items-center gap-1.5">
                {index > 0 && value - pages[index - 1] > 1 ? <span>…</span> : null}
                <Button
                  type="button"
                  variant={value === currentPage ? "default" : "outline"}
                  className="size-8 p-0"
                  aria-label={`第 ${value} 页`}
                  aria-current={value === currentPage ? "page" : undefined}
                  onClick={() => setPage(value)}
                >
                  {value}
                </Button>
              </span>
            ))}
            <Button
              type="button"
              variant="outline"
              size="icon"
              className="size-8"
              aria-label="下一页"
              disabled={currentPage >= pageCount}
              onClick={() => setPage(currentPage + 1)}
            >
              <ChevronRight className="size-4" aria-hidden="true" />
            </Button>
          </nav>
        </div>
      ) : null}

      {data?.note ? <p className="mt-4 text-xs text-muted-foreground">{data.note}</p> : null}

      <GroupDetailDialog groupId={detailId} onClose={closeDetail} onGroupChanged={handleGroupChanged} />

      {/* 卡片的「更多操作」（面板没有 dropdown 原语，用对话框承载） */}
      <Dialog open={actionsTarget !== null} onOpenChange={(open) => (!open ? setActionsTarget(null) : undefined)}>
        <DialogContent className="max-w-sm">
          <DialogHeader>
            <DialogTitle className="truncate">{actionsTarget?.title || "会话组"}</DialogTitle>
            <DialogDescription>
              {actionsTarget
                ? `${actionsTarget.project || "未标记项目"} · ${actionsTarget.members?.length ?? 0} 个账号`
                : ""}
            </DialogDescription>
          </DialogHeader>
          <div className="flex flex-col gap-2">
            <Button
              type="button"
              variant="outline"
              onClick={() => {
                const id = actionsTarget?.id;
                setActionsTarget(null);
                if (id) setDetailId(id);
              }}
            >
              查看成员与同步
            </Button>
            <Button
              type="button"
              variant="destructive"
              onClick={() => {
                const target = actionsTarget;
                setActionsTarget(null);
                if (target) setDeleteTarget(target);
              }}
            >
              删除关联
            </Button>
          </div>
        </DialogContent>
      </Dialog>

      {/* 「删除关联」确认框（文案对齐 switch） */}
      <AlertDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => (!open && !deleting ? setDeleteTarget(null) : undefined)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除「{deleteTarget?.title || "该会话组"}」的关联？</AlertDialogTitle>
            <AlertDialogDescription>
              组内 {deleteTarget?.members?.length ?? 0} 个账号会一起解除关联，不再参与同步；
              账号里的会话内容不会被删除。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleting}>取消</AlertDialogCancel>
            <Button type="button" variant="destructive" disabled={deleting} onClick={() => void handleDeleteGroup()}>
              {deleting ? "处理中…" : "确认删除关联"}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* 新增关联会话（来源账号 → 会话树勾选 → 目标账号 → 复制并关联） */}
      <AddLinkedSessionDialog
        open={addDialogOpen}
        onOpenChange={setAddDialogOpen}
        accounts={accounts}
        onAdded={() => void load()}
      />
    </div>
  );
}
