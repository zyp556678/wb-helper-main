import { useCallback, useEffect, useState } from "react";
import { AlertTriangle, ArrowLeftRight, Copy, HardDrive, Loader2, RefreshCw } from "lucide-react";
import { notifyError } from "@/lib/notify";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  describeError,
  fetchLocalSessions,
  copyLocalSession,
  syncLocalSession,
} from "@/lib/api";
import type {
  Account,
  LocalSessionsResponse,
  LocalSession,
  LocalSessionSyncResult,
} from "@/lib/types";
import { cn } from "@/lib/utils";

/** 结论 → 展示文案与色调。 */
const VERDICT_STYLE: Record<string, { label: string; tone: string }> = {
  identical: { label: "两边一致", tone: "text-muted-foreground" },
  fast_forward: { label: "可直接同步（纯追加）", tone: "text-primary-ink" },
  ahead: { label: "目标已领先", tone: "text-muted-foreground" },
  diverge: { label: "两边都有改动", tone: "text-amber-700 dark:text-amber-300" },
  unknown: { label: "无法判定", tone: "text-amber-700 dark:text-amber-300" },
};

/**
 * 同步对话框：选「另一份副本」→ 先判定（不写）→ 按结论决定能否直接执行。
 *
 * 判定与执行分两步是刻意的：分叉时覆盖会替换目标账号的**完整内容**，
 * 那可能是用户在另一个账号里继续写的东西 —— 必须先让用户看到结论再决定。
 */
function SyncSessionDialog({
  session,
  sessions,
  open,
  onOpenChange,
  onDone,
}: {
  session: LocalSession | null;
  sessions: LocalSession[];
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onDone: () => void;
}) {
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<LocalSessionSyncResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [force, setForce] = useState(false);

  useEffect(() => {
    if (!open) {
      setTarget("");
      setResult(null);
      setError(null);
      setForce(false);
      setBusy(false);
    }
  }, [open]);

  const run = useCallback(
    async (forceFlag: boolean) => {
      if (!session || !target) {
        notifyError("请先选择要同步到哪条会话");
        return;
      }
      setBusy(true);
      setError(null);
      try {
        const res = await syncLocalSession({ sourceId: session.id, targetId: target, force: forceFlag });
        setResult(res);
        if (res.applied) onDone();
      } catch (err) {
        setError(describeError(err));
      } finally {
        setBusy(false);
      }
    },
    [session, target, onDone],
  );

  if (!session) return null;

  const candidates = sessions.filter((s) => s.id !== session.id && s.has_body);
  const style = result ? VERDICT_STYLE[result.decision.verdict] : null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>同步到另一份副本</DialogTitle>
          <DialogDescription>
            把这条会话的内容同步到同一段对话的另一份副本（通常是复制给另一个账号后产生的那条）。
            <span className="mt-1 block font-medium text-foreground">
              只有「目标没有独有改动」时才会直接执行，否则需要你确认。
            </span>
          </DialogDescription>
        </DialogHeader>

        {result ? (
          <div className="space-y-2">
            <div className="rounded-lg border border-border bg-muted/40 px-3 py-2.5">
              <p className={cn("text-sm font-medium", style?.tone)}>{style?.label}</p>
              <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{result.decision.reason}</p>
              {result.decision.source_only || result.decision.target_only ? (
                <p className="mt-1 text-xs text-muted-foreground">
                  独有记录：来源 {result.decision.source_only} 条 · 目标 {result.decision.target_only} 条
                </p>
              ) : null}
            </div>
            <ul className="space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2.5">
              {result.notes.map((n, i) => (
                <li key={i} className="text-xs leading-5 text-muted-foreground">
                  · {n}
                </li>
              ))}
            </ul>
          </div>
        ) : (
          <>
            <div className="space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2">
              <p className="text-xs text-muted-foreground">来源（当前这条）</p>
              <p className="truncate text-sm">{session.title || "（无标题）"}</p>
              <p className="truncate font-mono text-xs text-muted-foreground">{session.id}</p>
            </div>

            <div className="space-y-1.5">
              <p className="text-xs font-medium text-muted-foreground">同步到</p>
              {candidates.length === 0 ? (
                <p className="text-xs text-amber-700 dark:text-amber-300">
                  没有其它带正文的会话可选（同步需要两边都有正文）
                </p>
              ) : (
                <ul className="max-h-[32vh] space-y-1.5 overflow-y-auto pr-1">
                  {candidates.map((s) => (
                    <li key={s.id}>
                      <label
                        className={cn(
                          "flex cursor-pointer items-center gap-2.5 rounded-lg border px-3 py-2",
                          target === s.id ? "border-primary bg-primary/6" : "border-border",
                        )}
                      >
                        <input
                          type="radio"
                          name="sync-target"
                          className="size-3.5 shrink-0 accent-[var(--primary)]"
                          checked={target === s.id}
                          onChange={() => setTarget(s.id)}
                        />
                        <span className="min-w-0 flex-1">
                          <span className="block truncate text-sm">{s.title || "（无标题）"}</span>
                          <span className="block truncate font-mono text-xs text-muted-foreground">
                            {s.id.slice(0, 8)}… · {humanSize(s.body_bytes)}
                          </span>
                        </span>
                      </label>
                    </li>
                  ))}
                </ul>
              )}
            </div>

            <label className="flex items-start gap-2.5 rounded-lg border border-border px-3 py-2.5 text-xs">
              <Checkbox
                checked={force}
                disabled={busy}
                onCheckedChange={(v) => setForce(v === true)}
                className="mt-0.5 data-[state=checked]:border-primary data-[state=checked]:bg-primary"
                aria-label="允许覆盖分叉的目标"
              />
              <span className="min-w-0 leading-5">
                <span className="font-medium">允许覆盖两边都有改动的情况</span>
                <span className="mt-0.5 block text-muted-foreground">
                  不勾选时，若两边各自都改过，同步会被拒绝（默认行为）。
                  勾选后会用来源**完整替换**目标内容 —— 目标账号独有的改动会丢失。
                </span>
              </span>
            </label>

            {error ? (
              <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2.5 text-xs text-destructive">
                <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
                <span className="min-w-0 leading-5">{error}</span>
              </div>
            ) : null}
          </>
        )}

        <DialogFooter>
          <Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => onOpenChange(false)}>
            {result ? "关闭" : "取消"}
          </Button>
          {result ? null : (
            <Button type="button" size="sm" disabled={busy || !target} onClick={() => void run(force)}>
              {busy ? (
                <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
              ) : (
                <ArrowLeftRight className="size-3.5" aria-hidden="true" />
              )}
              同步
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function humanSize(n: number): string {
  if (!n) return "无正文";
  if (n > 1048576) return `${(n / 1048576).toFixed(1)} MB`;
  if (n > 1024) return `${(n / 1024).toFixed(0)} KB`;
  return `${n} B`;
}

function formatTime(ms: number): string {
  if (!ms) return "—";
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** 复制对话框：选目标账号 → 预演（默认）→ 确认执行。 */
function CopySessionDialog({
  session,
  accounts,
  owners,
  open,
  onOpenChange,
  onDone,
}: {
  session: LocalSession | null;
  accounts: Account[];
  owners: LocalSessionsResponse["owners"];
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onDone: () => void;
}) {
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);
  const [notes, setNotes] = useState<string[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) {
      setTarget("");
      setNotes(null);
      setError(null);
      setBusy(false);
    }
  }, [open]);

  const run = useCallback(
    async (dryRun: boolean) => {
      if (!session || !target) {
        notifyError("请先选择目标账号");
        return;
      }
      setBusy(true);
      setError(null);
      try {
        const res = await copyLocalSession({ sessionId: session.id, accountId: target, dryRun });
        setNotes(res.notes ?? []);
        if (!dryRun) onDone();
      } catch (err) {
        setError(describeError(err));
      } finally {
        setBusy(false);
      }
    },
    [session, target, onDone],
  );

  if (!session) return null;

  // 源会话的归属账号，用于把它从候选里排除（复制给自己没有意义）。
  const srcOwner = Object.entries(owners ?? {}).find(([uid]) => uid === session.user_id)?.[0] ?? "";
  const candidates = accounts.filter((a) => a.uid !== srcOwner);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>把会话复制给另一个账号</DialogTitle>
          <DialogDescription>
            会新增一条会话并把归属改成目标账号，正文照抄（只重写正文里的会话 id）。
            <span className="mt-1 block font-medium text-foreground">
              不会改动或删除原会话。
            </span>
          </DialogDescription>
        </DialogHeader>

        {notes ? (
          <ul className="space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2.5">
            {notes.map((n, i) => (
              <li key={i} className="text-xs leading-5 text-muted-foreground">
                · {n}
              </li>
            ))}
          </ul>
        ) : (
          <>
            <div className="space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2">
              <p className="text-xs text-muted-foreground">源会话</p>
              <p className="truncate text-sm">{session.title || "（无标题）"}</p>
              <p className="truncate font-mono text-xs text-muted-foreground">{session.id}</p>
              <p className="text-xs text-muted-foreground">
                {humanSize(session.body_bytes)} · 更新于 {formatTime(session.updated_at)}
              </p>
            </div>

            <div className="space-y-1.5">
              <p className="text-xs font-medium text-muted-foreground">复制给</p>
              {candidates.length === 0 ? (
                <p className="text-xs text-amber-700 dark:text-amber-300">
                  账号池里没有其它账号可选
                </p>
              ) : (
                <ul className="max-h-[32vh] space-y-1.5 overflow-y-auto pr-1">
                  {candidates.map((a) => (
                    <li key={a.id}>
                      <label
                        className={cn(
                          "flex cursor-pointer items-center gap-2.5 rounded-lg border px-3 py-2",
                          target === a.id ? "border-primary bg-primary/6" : "border-border",
                        )}
                      >
                        <input
                          type="radio"
                          name="copy-target"
                          className="size-3.5 shrink-0 accent-[var(--primary)]"
                          checked={target === a.id}
                          onChange={() => setTarget(a.id)}
                        />
                        <span className="min-w-0 flex-1 truncate text-sm">
                          {a.nickname || a.uid || a.file}
                        </span>
                        <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
                          {a.site === "intl" ? "国际站" : "国内站"}
                        </span>
                      </label>
                    </li>
                  ))}
                </ul>
              )}
            </div>

            {error ? (
              <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2.5 text-xs text-destructive">
                <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
                <span className="min-w-0 leading-5">{error}</span>
              </div>
            ) : null}
          </>
        )}

        <DialogFooter>
          <Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => onOpenChange(false)}>
            {notes ? "关闭" : "取消"}
          </Button>
          {notes ? null : (
            <>
              <Button type="button" variant="outline" size="sm" disabled={busy || !target} onClick={() => void run(true)}>
                预演
              </Button>
              <Button type="button" size="sm" disabled={busy || !target} onClick={() => void run(false)}>
                {busy ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : <Copy className="size-3.5" aria-hidden="true" />}
                确认复制
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export interface LocalSessionsCardProps {
  accounts: Account[];
}

/**
 * 本机会话库（WorkBuddy 客户端的会话）。
 *
 * **只读 + 只新增**：本工具不改动、不删除任何已有会话 ——
 * 删用户的历史是不可逆的，没有理由碰它。所有写操作都先备份。
 */
export function LocalSessionsCard({ accounts }: LocalSessionsCardProps) {
  const [data, setData] = useState<LocalSessionsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [target, setTarget] = useState<LocalSession | null>(null);
  const [syncTarget, setSyncTarget] = useState<LocalSession | null>(null);

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      setData(await fetchLocalSessions(undefined, signal));
    } catch {
      // 静默：会话库是加分项，失败不该在账号页弹错误。
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    const c = new AbortController();
    void load(c.signal);
    return () => c.abort();
  }, [load]);

  const sessions = data?.sessions ?? [];
  const owners = data?.owners ?? {};

  return (
    <Card className="gap-0 overflow-hidden rounded-2xl py-0 shadow-none">
      <header className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-3">
        <h2 className="flex items-center gap-1.5 text-sm font-semibold">
          <HardDrive className="size-3.5 text-muted-foreground" aria-hidden="true" />
          本机会话
        </h2>
        <p className="min-w-0 flex-1 text-xs leading-5 text-muted-foreground">
          WorkBuddy 客户端在本机保存的会话。可把某条会话复制给另一个账号（只新增，不改不删原会话）。
        </p>
        <Button type="button" variant="ghost" size="sm" disabled={loading} onClick={() => void load()}>
          <RefreshCw className={cn("size-3.5", loading && "animate-spin")} aria-hidden="true" />
          刷新
        </Button>
      </header>

      {data && !data.available ? (
        <p className="px-4 py-6 text-center text-sm text-muted-foreground">
          {data.note || "未找到本机会话库"}
        </p>
      ) : (
        <ul className="divide-y divide-border">
          {sessions.map((s) => {
            const owner = owners[s.user_id];
            return (
              <li key={s.id} className="flex flex-wrap items-start gap-3 px-4 py-3">
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm">{s.title || "（无标题）"}</p>
                  <p className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground">
                    <span className="font-mono">{s.id.slice(0, 8)}…</span>
                    <span>{humanSize(s.body_bytes)}</span>
                    <span>{formatTime(s.updated_at)}</span>
                    {owner ? (
                      <Badge variant={owner.known ? "secondary" : "outline"}>
                        {owner.label}
                        {owner.known ? "" : "（不在账号池）"}
                      </Badge>
                    ) : null}
                  </p>
                </div>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  disabled={sessions.length < 2 || !s.has_body}
                  title={!s.has_body ? "没有正文，无法同步" : "同步到同一段对话的另一份副本"}
                  onClick={() => setSyncTarget(s)}
                >
                  <ArrowLeftRight className="size-3.5" aria-hidden="true" />
                  同步
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={accounts.length === 0}
                  title={accounts.length === 0 ? "账号池为空" : "把这条会话复制给另一个账号"}
                  onClick={() => setTarget(s)}
                >
                  <Copy className="size-3.5" aria-hidden="true" />
                  复制给账号
                </Button>
              </li>
            );
          })}
          {!loading && sessions.length === 0 ? (
            <li className="px-4 py-6 text-center text-sm text-muted-foreground">没有会话</li>
          ) : null}
        </ul>
      )}

      <SyncSessionDialog
        session={syncTarget}
        sessions={sessions}
        open={syncTarget !== null}
        onOpenChange={(v) => {
          if (!v) setSyncTarget(null);
        }}
        onDone={() => void load()}
      />

      <CopySessionDialog
        session={target}
        accounts={accounts}
        owners={owners}
        open={target !== null}
        onOpenChange={(v) => {
          if (!v) setTarget(null);
        }}
        onDone={() => void load()}
      />
    </Card>
  );
}
