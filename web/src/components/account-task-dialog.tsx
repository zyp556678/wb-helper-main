import { useCallback, useMemo, useState } from "react";
import { CheckCircle2, CircleSlash, Loader2, Play, Sparkles, XCircle } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { describeError, runAccountTask, runAccountTasksAll } from "@/lib/api";
import { notifyError, notifySuccess } from "@/lib/notify";
import type {
  AccountTaskResultItem,
  GrowthTaskView,
  TaskAccountScan,
} from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * 账号级「一键完成」弹窗。
 *
 * 为什么要有这个弹窗，而不是复用页首那条跨账号队列：两者的**问题不同**。
 * 队列回答的是「全部账号里哪些待办能推进」，跑完只给一个项数汇总；
 * 而这个弹窗回答的是「**这个账号**现在能自动做什么、刚才那一步到底成了没有」——
 * 需要逐项、同步、当场看到进度从哪变到哪（含自动领奖的到账数额）。
 *
 * 所以这里调的是同步端点（/tasks/auto 与 /tasks/auto_all），结果直接渲染，
 * 不再去轮询队列状态。
 */
export function AccountTaskDialog({
  account,
  tasks,
  open,
  onOpenChange,
  onFinished,
}: {
  account: TaskAccountScan | null;
  tasks: GrowthTaskView[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onFinished: () => void;
}) {
  // 结果按 task_code 归档：单项执行与全量执行都写进同一张表，
  // 于是「点过单项再点全量」不会丢掉先前那次的结论。
  const [results, setResults] = useState<Record<string, AccountTaskResultItem>>({});
  const [busy, setBusy] = useState<string | null>(null);
  const [runningAll, setRunningAll] = useState(false);

  // 只列「有自动动作」的项：其余分类（需人工 / 已完成）在这个弹窗里点了也没用。
  const automatable = useMemo(
    () => tasks.filter((t) => t.category === "auto" || t.category === "claimable"),
    [tasks],
  );

  const merge = useCallback((items: AccountTaskResultItem[]) => {
    setResults((prev) => {
      const next = { ...prev };
      for (const it of items) next[it.task_code] = it;
      return next;
    });
  }, []);

  const handleOne = useCallback(
    async (taskCode: string) => {
      if (!account) return;
      setBusy(taskCode);
      try {
        const res = await runAccountTask(account.id, taskCode);
        if (res.ok === false) {
          notifyError(res.detail || "执行失败");
          return;
        }
        if (res.result) merge([res.result]);
        notifySuccess(res.message || "已执行");
        onFinished();
      } catch (err) {
        notifyError(describeError(err));
      } finally {
        setBusy(null);
      }
    },
    [account, merge, onFinished],
  );

  const handleAll = useCallback(async () => {
    if (!account) return;
    setRunningAll(true);
    try {
      const res = await runAccountTasksAll(account.id);
      if (res.ok === false) {
        notifyError(res.detail || "执行失败");
        return;
      }
      const items = res.results ?? [];
      merge(items);
      const done = items.filter((i) => i.status === "done").length;
      const failed = items.filter((i) => i.status === "error").length;
      notifySuccess(
        failed > 0
          ? `一键完成结束：${done} 项完成，${failed} 项失败`
          : `一键完成结束：${done} 项完成`,
      );
      onFinished();
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setRunningAll(false);
    }
  }, [account, merge, onFinished]);

  const claimedTotal = useMemo(
    () =>
      Object.values(results).reduce(
        (acc, r) => ({ credit: acc.credit + (r.credit ?? 0), energy: acc.energy + (r.energy ?? 0) }),
        { credit: 0, energy: 0 },
      ),
    [results],
  );

  const busyAny = busy !== null || runningAll;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-3xl">
        <DialogHeader>
          <DialogTitle>一键完成 · {account?.nickname || account?.id || "账号"}</DialogTitle>
          <DialogDescription>
            逐项执行该账号的自动任务；动作完成后会回读进度，达标即自动领奖。
            专家召唤、真实对话类动作会消耗上游额度，单项失败不影响后续项。
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-wrap items-center gap-2">
          <Button type="button" size="sm" disabled={busyAny || automatable.length === 0} onClick={() => void handleAll()}>
            {runningAll ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <Sparkles className="size-3.5" aria-hidden="true" />
            )}
            一键完成全部
          </Button>
          {claimedTotal.credit > 0 || claimedTotal.energy > 0 ? (
            <span className="text-xs text-muted-foreground">
              本轮已领{" "}
              <span className="font-medium tabular-nums text-emerald-600 dark:text-emerald-400">
                +{claimedTotal.credit} 积分 +{claimedTotal.energy} 能量
              </span>
            </span>
          ) : null}
        </div>

        <div className="max-h-[52vh] space-y-2 overflow-y-auto pr-1">
          {automatable.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">
              该账号当前没有可自动完成的任务。
            </p>
          ) : (
            automatable.map((task) => {
              const res = results[task.task_code];
              const rowBusy = busy === task.task_code;
              return (
                <div
                  key={task.task_code}
                  className="rounded-lg border border-border/60 px-3 py-2.5 text-sm"
                >
                  <div className="flex flex-wrap items-start justify-between gap-2">
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="font-medium">{task.title || task.task_code}</span>
                        {task.category === "claimable" ? (
                          <Badge variant="outline" className="text-emerald-600 dark:text-emerald-400">
                            可领奖
                          </Badge>
                        ) : null}
                        {task.mp_only ? <Badge variant="outline">小程序</Badge> : null}
                        {task.credit > 0 || task.energy > 0 ? (
                          <span className="text-xs text-muted-foreground tabular-nums">
                            +{task.credit} 积分 +{task.energy} 能量
                          </span>
                        ) : null}
                      </div>
                      <p className="mt-0.5 text-xs text-muted-foreground">
                        {task.action || task.action_reason || task.condition}
                      </p>
                    </div>
                    <Button
                      type="button"
                      size="sm"
                      variant="outline"
                      disabled={busyAny}
                      onClick={() => void handleOne(task.task_code)}
                    >
                      {rowBusy ? (
                        <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                      ) : (
                        <Play className="size-3.5" aria-hidden="true" />
                      )}
                      执行
                    </Button>
                  </div>
                  {res ? <TaskResultLine result={res} /> : null}
                </div>
              );
            })
          )}
        </div>

        <DialogFooter>
          <Button type="button" size="sm" variant="outline" onClick={() => onOpenChange(false)}>
            关闭
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** TaskResultLine 渲染单条结果：状态图标 + 文案 + 进度变化。 */
function TaskResultLine({ result }: { result: AccountTaskResultItem }) {
  const icon =
    result.status === "error" ? (
      <XCircle className="size-3.5 shrink-0 text-destructive" aria-hidden="true" />
    ) : result.status === "skipped" ? (
      <CircleSlash className="size-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />
    ) : (
      <CheckCircle2 className="size-3.5 shrink-0 text-emerald-600 dark:text-emerald-400" aria-hidden="true" />
    );
  return (
    <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
      {icon}
      <span
        className={cn(
          result.status === "error" ? "text-destructive" : "text-muted-foreground",
        )}
      >
        {result.message}
      </span>
      {result.progress_after ? (
        <span className="tabular-nums text-muted-foreground">
          {result.progress_before ? `${result.progress_before} → ` : ""}
          {result.progress_after}
        </span>
      ) : null}
      {result.claimed ? (
        <Badge variant="outline" className="text-emerald-600 dark:text-emerald-400">
          已领奖
        </Badge>
      ) : null}
    </div>
  );
}
