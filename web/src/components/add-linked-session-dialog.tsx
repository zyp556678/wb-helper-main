import { useCallback, useEffect, useMemo, useState } from "react";
import { AlertCircle, Loader2, RefreshCw } from "lucide-react";

import { notifyError, notifySuccess, notifyWarning } from "@/lib/notify";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { SessionTree } from "@/components/session-tree";
import { addSessionGroupMember, describeError, fetchLocalSessions } from "@/lib/api";
import { accountDisplayName, siteLabel } from "@/lib/format";
import type { Account, LocalSession } from "@/lib/types";

/**
 * 「新增关联会话」对话框（对齐 wb-switch 的 AddLinkedSessionDialog）。
 *
 * 流程：**来源账号 → 会话树勾选 → 目标账号 → 复制并关联**。
 *
 * 语义说明（与本项目的数据模型有关，必须说清）：这里没有「先建组再入组」这一步 ——
 * 本项目的组是按内容推导的（见 localsessions/groups.go），复制出的副本与来源同源，
 * 下一次分组自然同组。所以「关联」= 复制，逐条会话各调一次 `/groups/{id}/add`。
 *
 * 会话树里**过滤掉 claw 工作区的会话**：那是账号绑定的 IM 渠道工作区，
 * 复制会话行不够，目标账号也用不了（对照 wb-switch 的 `is_claw_workspace`）。
 */

export interface AddLinkedSessionDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  accounts: Account[];
  /** 完成复制后调用（刷新列表）。 */
  onAdded: () => void;
}

/** claw 工作区：cwd 最后一段是 claw（大小写不敏感）。 */
function isClawWorkspace(cwd: string): boolean {
  const trimmed = (cwd || "").trim().replace(/[\\/]+$/, "");
  if (!trimmed) return false;
  const parts = trimmed.split(/[\\/]/);
  return (parts[parts.length - 1] || "").toLowerCase() === "claw";
}

export function AddLinkedSessionDialog({
  open,
  onOpenChange,
  accounts,
  onAdded,
}: AddLinkedSessionDialogProps) {
  const [sourceId, setSourceId] = useState("");
  const [targetId, setTargetId] = useState("");
  const [sessions, setSessions] = useState<LocalSession[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);

  // 关闭时复位：下次打开从「选来源账号」开始。
  useEffect(() => {
    if (!open) {
      setSourceId("");
      setTargetId("");
      setSessions(null);
      setError(null);
      setSelected(new Set());
      setBusy(false);
    }
  }, [open]);

  const sourceAccount = accounts.find((a) => a.id === sourceId) ?? null;
  const targetAccount = accounts.find((a) => a.id === targetId) ?? null;

  // 选来源账号后拉它的会话（后端按 uid 过滤）。
  useEffect(() => {
    if (!open || !sourceAccount) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    fetchLocalSessions(sourceAccount.uid)
      .then((res) => {
        if (cancelled) return;
        if (!res.available) {
          setError(res.note || "未找到本机会话库（客户端可能还没用过）");
          setSessions([]);
          return;
        }
        setSessions((res.sessions ?? []).filter((s) => !isClawWorkspace(s.cwd)));
      })
      .catch((err) => {
        if (!cancelled) setError(describeError(err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, sourceAccount, reloadKey]);

  /** 换来源账号：清空勾选与目标（与 switch 的 changeSource 一致）。 */
  const changeSource = (id: string) => {
    setSourceId(id);
    setTargetId("");
    setSelected(new Set());
    setSessions(null);
  };

  const submit = useCallback(async () => {
    if (!sourceAccount || !targetAccount) return;
    const ids = [...selected];
    if (ids.length === 0) {
      notifyError("请先在会话树里勾选要复制的会话");
      return;
    }
    setBusy(true);
    const copied: string[] = [];
    const skipped: string[] = [];
    const failed: string[] = [];
    for (const sessionId of ids) {
      try {
        // 组 ID 直接用这条会话的 id：登记表按「源身份」找组，
        // 无论它当前是单份还是会话组的一员都能定位到正确的组。
        const res = await addSessionGroupMember(sessionId, {
          sourceMemberId: sessionId,
          targetUid: targetAccount.uid,
        });
        // alreadyLinked 是幂等结果（switch 同样按成功返回），不计入「已复制」。
        if (res.status === "alreadyLinked") skipped.push(sessionId);
        else copied.push(sessionId);
      } catch (err) {
        failed.push(describeError(err));
      }
    }
    setBusy(false);

    if (copied.length > 0) {
      notifySuccess(`已复制 ${copied.length} 个会话到「${accountDisplayName(targetAccount)}」并建立关联`);
    }
    if (skipped.length > 0) {
      notifyWarning(`「${accountDisplayName(targetAccount)}」已有 ${skipped.length} 个会话的副本，未重复复制`);
    }
    if (failed.length > 0) {
      notifyError(`部分会话复制失败：${failed.join("；")}`);
    }
    if (copied.length === 0 && skipped.length === 0 && failed.length === 0) {
      notifyError("没有可复制的会话");
    }

    if (failed.length === 0) {
      onAdded();
      onOpenChange(false);
    }
  }, [sourceAccount, targetAccount, selected, onAdded, onOpenChange]);

  const footerHint = useMemo(() => {
    if (!sourceAccount) return "先选择来源账号";
    if (!targetAccount) return "再选择目标账号";
    if (selected.size === 0) return `勾选要复制到「${accountDisplayName(targetAccount)}」的会话`;
    return `复制并关联到「${accountDisplayName(targetAccount)}」`;
  }, [sourceAccount, targetAccount, selected.size]);

  return (
    <Dialog open={open} onOpenChange={(next) => (!busy ? onOpenChange(next) : undefined)}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>新增关联会话</DialogTitle>
          <DialogDescription>
            选择一个来源账号的会话，复制到目标账号并建立关联。
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          {/* 来源账号 */}
          <div className="space-y-1.5">
            <label className="text-xs font-medium text-muted-foreground" htmlFor="add-link-source">
              来源账号
            </label>
            <select
              id="add-link-source"
              className="w-full rounded-lg border border-border bg-background px-3 py-2 text-sm"
              value={sourceId}
              disabled={busy}
              onChange={(e) => changeSource(e.target.value)}
            >
              <option value="">选择 WorkBuddy 账号</option>
              {accounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {accountDisplayName(a)} · {siteLabel(a)}
                </option>
              ))}
            </select>
            {accounts.length === 0 ? (
              <p className="text-xs text-muted-foreground">账号库里还没有 WorkBuddy 账号。</p>
            ) : null}
          </div>

          {/* 来源会话（会话树） */}
          <div className="space-y-1.5">
            <p className="text-xs font-medium text-muted-foreground">来源会话</p>
            {!sourceAccount ? (
              <p className="rounded-lg border border-border/60 bg-muted/40 p-3 text-xs text-muted-foreground">
                先选择来源账号，再从会话树里勾选要复制的会话。
              </p>
            ) : loading ? (
              <div className="space-y-2" aria-label="正在读取会话">
                <Skeleton className="h-6 w-full" />
                <Skeleton className="h-6 w-4/5" />
                <Skeleton className="h-6 w-3/5" />
              </div>
            ) : error ? (
              <div className="flex items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-xs">
                <AlertCircle className="size-3.5 shrink-0 text-destructive" aria-hidden="true" />
                <span className="min-w-0 flex-1">{error}</span>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => setReloadKey((k) => k + 1)}
                >
                  <RefreshCw className="size-3.5" />
                  重试
                </Button>
              </div>
            ) : (sessions ?? []).length === 0 ? (
              <p className="rounded-lg border border-border/60 bg-muted/40 p-3 text-xs text-muted-foreground">
                该账号没有可复制的会话。
              </p>
            ) : (
              <SessionTree sessions={sessions ?? []} selected={selected} onChange={setSelected} />
            )}
          </div>

          {/* 目标账号 */}
          <div className="space-y-1.5">
            <label className="text-xs font-medium text-muted-foreground" htmlFor="add-link-target">
              目标账号
            </label>
            <select
              id="add-link-target"
              className="w-full rounded-lg border border-border bg-background px-3 py-2 text-sm"
              value={targetId}
              disabled={busy || !sourceAccount}
              onChange={(e) => setTargetId(e.target.value)}
            >
              <option value="">{sourceAccount ? "选择目标账号" : "先选择来源账号"}</option>
              {accounts
                .filter((a) => a.id !== sourceId)
                .map((a) => (
                  <option key={a.id} value={a.id}>
                    {accountDisplayName(a)} · {siteLabel(a)}
                  </option>
                ))}
            </select>
          </div>
        </div>

        <DialogFooter className="items-center gap-3 sm:justify-between">
          <div className="min-w-0 text-xs text-muted-foreground">
            <span className="tabular-nums">已选 {selected.size} 个会话</span>
            <span className="ml-2">{footerHint}</span>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <Button type="button" variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>
              取消
            </Button>
            <Button
              type="button"
              disabled={busy || !sourceAccount || !targetAccount || selected.size === 0}
              onClick={() => void submit()}
            >
              {busy ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : null}
              {busy ? "处理中…" : "复制并关联"}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
