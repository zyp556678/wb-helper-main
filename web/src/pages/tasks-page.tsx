import { useCallback, useEffect, useMemo, useState } from "react";
import {
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  HandCoins,
  Loader2,
  MousePointerClick,
  RefreshCw,
  Send,
  Sparkles,
  Ticket,
  XCircle,
} from "lucide-react";
import {
  notifySuccess,
  notifyError,
} from "@/lib/notify";

import { StatTile } from "@/components/stats-bar";
import { AccountTaskDialog } from "@/components/account-task-dialog";
import { VouchersDialog } from "@/components/vouchers-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { MetricRow, Section } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import {
  describeError,
  fetchTasks,
  fetchTasksQueue,
  growthAction,
  reportTaskActivity,
  runStreakBonus,
  runTasks,
} from "@/lib/api";
import { formatRelativeTime, formatUnixSecondsFull } from "@/lib/format";
import type {
  GrowthTaskView,
  TaskAccountScan,
  TaskCategory,
  TaskQueueKind,
  TaskQueueState,
  TaskScanResponse,
} from "@/lib/types";
import { cn } from "@/lib/utils";

/** 任务分类的展示元数据。 */
const CATEGORY_META: Record<TaskCategory, { label: string; tone: string; rank: number }> = {
  claimable: { label: "可领奖", tone: "bg-emerald-500/12 text-emerald-700 dark:text-emerald-300", rank: 0 },
  auto: { label: "可自动完成", tone: "bg-sky-500/12 text-sky-700 dark:text-sky-300", rank: 1 },
  accept: { label: "可报名", tone: "bg-violet-500/12 text-violet-700 dark:text-violet-300", rank: 2 },
  manual: { label: "需人工处理", tone: "bg-muted text-muted-foreground", rank: 3 },
  done: { label: "已完成", tone: "bg-muted text-muted-foreground", rank: 4 },
};

const CATEGORY_ORDER: TaskCategory[] = ["claimable", "auto", "accept", "manual", "done"];

const KIND_LABEL: Record<TaskQueueKind, string> = {
  claim: "领奖",
  accept: "报名",
  auto: "自动",
};

const TRAVEL_LABEL: Record<string, string> = {
  travelling: "旅行中",
  arrived: "已到达，可领奖励",
};

export function TasksPage() {
  const [scan, setScan] = useState<TaskScanResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [scanning, setScanning] = useState(false);
  const [running, setRunning] = useState(false);
  const [reporting, setReporting] = useState<string | null>(null);
  const [acting, setActing] = useState<string | null>(null);
  const [queue, setQueue] = useState<TaskQueueState | null>(null);
  const [expandedDone, setExpandedDone] = useState(false);
  // 账号级「一键完成」弹窗：存的是账号对象而不是 id —— 弹窗要展示昵称与站点，
  // 只存 id 的话扫描结果一变（比如重新扫描后该账号消失）就得去猜。
  const [taskDialog, setTaskDialog] = useState<TaskAccountScan | null>(null);
  const [vouchersOpen, setVouchersOpen] = useState(false);

  const load = useCallback(async (refresh: boolean) => {
    if (refresh) setScanning(true);
    try {
      const data = await fetchTasks(refresh);
      setScan(data);
      setError(null);
    } catch (err) {
      setError(describeError(err));
    } finally {
      setLoading(false);
      setScanning(false);
    }
  }, []);

  useEffect(() => {
    void load(false);
    // 进页面就拉一次队列：上次执行的结果（含刷新前跑完的那次）必须能看见。
    //
    // 早期实现只在「点了执行之后」才 setQueue，于是页面一刷新队列区块就整块消失，
    // 用户想问的「刚才到底哪条失败了」再也查不到。
    void (async () => {
      try {
        setQueue(await fetchTasksQueue());
      } catch {
        // 拉取失败不阻塞页面，留待后续轮询或下次操作补齐。
      }
    })();
  }, [load]);

  // 队列状态轮询。
  //
  // 只依赖 `queue?.running` 会漏掉一类关键情况：提交请求刚返回的那一刻队列可能
  // **已经是 running**（后端先进 preparing 再返回），若 effect 依赖的是「调用前的
  // queue 值」就永远起不来。这里改为依赖 `running` 这个**布尔值本身**，
  // 任何来源（首次拉取、提交后的刷新、上一轮轮询）把它变成 true 都会启动轮询，
  // 变成 false 就自动停下。
  const queueActive = queue?.running === true;
  useEffect(() => {
    if (!queueActive) return;
    let cancelled = false;
    const tick = async () => {
      try {
        const state = await fetchTasksQueue();
        if (cancelled) return;
        setQueue(state);
        if (!state.running) {
          notifySuccess(
            state.failed > 0
              ? `任务队列执行完成：${state.done} 项成功，${state.failed} 项失败`
              : `任务队列执行完成：共 ${state.done} 项`,
          );
          void load(false);
        }
      } catch {
        // 轮询失败不打断页面，下一轮再试。
      }
    };
    // 立刻先查一次，再进入定时间隔 —— 否则第一帧要等满 1 个间隔才更新，
    // 在「准备阶段」这种转瞬即逝的状态上会整段错过。
    void tick();
    const timer = window.setInterval(() => void tick(), 1200);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [queueActive, load]);

  const handleRun = useCallback(async (taskCodes?: string[], actions?: TaskQueueKind[]) => {
    setRunning(true);
    try {
      const result = await runTasks({ task_codes: taskCodes, actions });
      // 后端已改为「受理即返回」，planned 为 -1 时项数要等扫描完才知道。
      if (result.ok === false) {
        // 409「已有队列在执行中」等业务性拒绝：把后端给的理由原样展示。
        notifyError(result.detail || "提交执行失败");
      } else if (result.planned > 0) {
        notifySuccess(`已提交 ${result.planned} 个任务项到执行队列`);
      } else if (result.planned === 0) {
        notifySuccess(result.detail ?? "没有需要执行的待办任务");
      } else {
        notifySuccess("已提交执行，正在扫描待办任务…");
      }
      // 立刻把队列拉回来。此时后端必然已把队列置为 preparing/running，
      // 上面的轮询 effect 会因此启动，用户马上就能看到「正在扫描…」而不是空白。
      const state = await fetchTasksQueue();
      setQueue(state);
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setRunning(false);
    }
  }, []);

  const handleReport = useCallback(async (account: string) => {
    setReporting(account);
    try {
      const result = await reportTaskActivity(account);
      // 失败也是「这次动作没做成」而不是请求异常——按 ok 分流，
      // 别把后端给的理由吞掉（例如「账号没有成长中心活动」）。
      if (result.ok === false) notifyError(result.detail || "上报失败");
      else notifySuccess(result.detail || "上报成功");
      void load(false);
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setReporting(null);
    }
  }, []);

  const handleGrowth = useCallback(
    async (account: string, action: string, label: string) => {
      setActing(account + ":" + action);
      try {
        // 连登管家是「一串动作」（补签 → 礼包/补偿 → 兑换 → 抽奖），
        // 有自己的端点；其余仍是成长中心的单动作。
        const result =
          action === "streak_bonus"
            ? await runStreakBonus(account)
            : await growthAction(account, action);
        if (result.ok) notifySuccess(result.detail || label + "完成");
        else notifyError(result.detail || label + "失败");
        void load(false);
      } catch (err) {
        notifyError(describeError(err));
      } finally {
        setActing(null);
      }
    },
    [load],
  );

  const tasks = scan?.tasks ?? [];
  const totals = scan?.totals;
  const grouped = useMemo(() => {
    const map = new Map<TaskCategory, GrowthTaskView[]>();
    for (const t of tasks) {
      const list = map.get(t.category) ?? [];
      list.push(t);
      map.set(t.category, list);
    }
    return CATEGORY_ORDER.map((category) => ({
      category,
      items: map.get(category) ?? [],
    })).filter((g) => g.items.length > 0 && (g.category !== "done" || expandedDone));
  }, [tasks, expandedDone]);

  const firstLoad = loading && scan === null;

  return (
    <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
      <header className="mb-6 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">任务中心</h1>
          <p className="mt-2 text-sm leading-6 text-muted-foreground">
            成长任务待办与自动完成、连登、猫咪旅行与抽奖。
            {scan ? (
              <span className="ml-1 text-muted-foreground">
                扫描于{" "}
                <span title={formatUnixSecondsFull(scan.scanned_at) ?? ""}>
                  {formatRelativeTime(scan.scanned_at, Math.floor(Date.now() / 1000))}
                </span>
              </span>
            ) : null}
          </p>
        </div>
        <div className="flex shrink-0 flex-wrap items-center justify-end gap-1.5 pt-1">
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={() => setVouchersOpen(true)}
            title="开学季抽奖抽中的第三方券码（复制给门店核销）"
          >
            <Ticket className="size-3.5" aria-hidden="true" />
            券码
          </Button>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={scanning || running}
            onClick={() => void load(true)}
          >
            {scanning ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <RefreshCw className="size-3.5" aria-hidden="true" />
            )}
            重新扫描
          </Button>
          <Button type="button" size="sm" disabled={scanning || running} onClick={() => void handleRun()}>
            {running ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <Send className="size-3.5" aria-hidden="true" />
            )}
            立即执行全部待办
          </Button>        </div>
      </header>

      {scan?.support_warning ? (
        <div className="mb-4 rounded-lg border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-800 dark:text-amber-200">
          {scan.support_warning}
        </div>
      ) : null}

      {/* 执行队列放在最前。
          这一块是「我刚点了执行，现在怎么样了」的答案，必须**不用滚动就能看到**。
          原先放在页面最底部（在可领奖/可自动/可报名/需客户端四档之后），
          点完按钮界面纹丝不动、进度却在视线之外，用户会以为根本没生效。 */}
      {queue && (queue.running || queue.items.length > 0 || queue.note) ? (
        <div className="mb-6">
          <QueuePanel queue={queue} />
        </div>
      ) : null}

      {error ? (
        <Card>
          <CardContent className="p-6 text-sm text-destructive">{error}</CardContent>
        </Card>
      ) : firstLoad ? (
        <div className="space-y-12">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
            {Array.from({ length: 5 }).map((_, i) => (
              <Skeleton key={i} className="h-20" />
            ))}
          </div>
          <Skeleton className="h-64" />
        </div>
      ) : (
        <div className="space-y-12">
          {/* 汇总磁贴 */}
          {totals ? (
            <Card className="gap-0 overflow-hidden rounded-xl py-0 shadow-none">
              <MetricRow cols={5}>
              <StatTile
                label="可领奖"
                value={String(totals.claimable)}
                hint="进度达标待领取"
                icon={HandCoins}
                tone={totals.claimable > 0 ? "success" : "default"}
              />
              <StatTile
                label="可自动完成"
                value={String(totals.auto)}
                hint="判据为对话事件"
                divided
                icon={Sparkles}
                tone={totals.auto > 0 ? "warning" : "default"}
              />
              <StatTile
                label="可报名"
                value={String(totals.acceptable)}
                hint="尚未接取的任务"
                divided
                icon={MousePointerClick}
                tone="default"
              />
              <StatTile
                label="需人工处理"
                value={String(totals.manual)}
                hint="需客户端操作"
                divided
                icon={MousePointerClick}
                tone="default"
              />
              <StatTile
                label="已完成"
                value={String(totals.done)}
                hint="奖励已领取"
                icon={CheckCircle2}
                tone="default"
                divided
              />
              </MetricRow>
            </Card>
          ) : null}

          {/* 账号成长状态 */}
          <Section
            id="tasks-growth"
            title="账号成长状态"
            description="连登与活动进度按账号分别读取；成长域活动目前仅国内站可用。"
          >
            <CardContent className="space-y-3">
              {(scan?.accounts ?? []).map((account) => (
                <AccountGrowthRow
                  key={account.id}
                  account={account}
                  reporting={reporting === account.id}
                  acting={acting}
                  onReport={() => void handleReport(account.id)}
                  onGrowth={handleGrowth}
                  onOpenTasks={() => setTaskDialog(account)}
                />
              ))}
              {(scan?.accounts ?? []).length === 0 ? (
                <p className="text-sm text-muted-foreground">当前没有可用账号。</p>
              ) : null}
            </CardContent>
          </Section>

          {/* 待办任务（按分类分组） */}
          {grouped.map((group) => {
            const meta = CATEGORY_META[group.category];
            return (
              <Section
                key={group.category}
                id={`tasks-group-${group.category}`}
                title={
                  <>
                    <span className={cn("rounded px-1.5 py-0.5 text-xs font-medium", meta.tone)}>
                      {meta.label}
                    </span>
                    <span className="ml-2 text-xs font-normal tabular-nums text-muted-foreground">
                      {group.items.length} 项
                    </span>
                  </>
                }
                description={
                  group.category === "manual"
                    ? "这些任务的进度由客户端行为事件点亮（召唤专家、用模板建任务、读资料库等），网关不伪造事件，请在 WorkBuddy 客户端里完成对应操作。"
                    : undefined
                }
              >
                <CardContent className="divide-y divide-border/60">
                  {group.items.map((task) => (
                    <TaskRow
                      key={task.task_code}
                      task={task}
                      running={running}
                      onAction={() =>
                        void handleRun(
                          [task.task_code],
                          task.category === "claimable"
                            ? ["claim"]
                            : task.category === "accept"
                              ? ["accept"]
                              : ["auto"],
                        )
                      }
                    />
                  ))}
                </CardContent>
              </Section>
            );
          })}

          {/* 已完成折叠开关 */}
          {totals && totals.done > 0 ? (
            <button
              type="button"
              className="flex w-full items-center justify-center gap-1.5 rounded-lg border border-border/60 py-2 text-xs text-muted-foreground transition-colors hover:bg-muted/40"
              onClick={() => setExpandedDone((v) => !v)}
            >
              {expandedDone ? (
                <ChevronDown className="size-3.5" aria-hidden="true" />
              ) : (
                <ChevronRight className="size-3.5" aria-hidden="true" />
              )}
              {expandedDone ? "收起已完成任务" : `展开已完成任务（${totals.done} 项）`}
            </button>
          ) : null}

          {/* 执行队列已上移到页首（见上方），此处不再重复渲染 */}

          {/* 说明：尾部注记用正文段落而不是卡片 —— 一张带边框的卡会让它看起来像可操作的区块 */}
          <div className="space-y-2 px-1 text-xs leading-5 text-muted-foreground">
            <p>
              上游任务进度绝大多数靠<strong className="text-foreground">客户端行为事件</strong>
              点亮（召唤专家、用模板新建任务、读资料库文档等），这些事件只能在客户端产生。
              网关能自动做的是判据为「对话事件」的那几类（聊天 N 次、体验某模型、夜猫子），
              以及设置里打开「客户端事件上报」开关后按样本复刻的客户端事件链。
            </p>
            <p>
              「补活跃」会上报一次对话事件，用于点亮连登并解锁「领取一只 Buddy」的前置条件，
              不需要真的发一轮对话。任务列表里的「执行」走跨账号队列；
              账号行上的「一键完成」是**单账号**同步执行，结果当场逐项展示。
            </p>
          </div>
        </div>
      )}

      {/* 账号级一键完成弹窗：只列该账号名下的自动任务 */}
      <AccountTaskDialog
        account={taskDialog}
        tasks={
          taskDialog
            ? tasks.filter((t) => (t.accounts ?? []).includes(taskDialog.id))
            : []
        }
        open={taskDialog !== null}
        onOpenChange={(next) => {
          if (!next) setTaskDialog(null);
        }}
        onFinished={() => void load(false)}
      />

      <VouchersDialog open={vouchersOpen} onOpenChange={setVouchersOpen} />
    </div>
  );
}

// -----------------------------------------------------------------------------

function AccountGrowthRow({
  account,
  reporting,
  acting,
  onReport,
  onGrowth,
  onOpenTasks,
}: {
  account: TaskAccountScan;
  reporting: boolean;
  acting: string | null;
  onReport: () => void;
  onGrowth: (account: string, action: string, label: string) => Promise<void>;
  onOpenTasks: () => void;
}) {
  if (account.error) {
    return (
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="font-medium">{account.nickname || account.id}</span>
        <Badge variant="outline">{account.site_label}</Badge>
        <span className="text-xs text-destructive">{account.error}</span>
      </div>
    );
  }

  // 中性标注（如「国际站没有成长中心活动」）：不是故障，所以不渲染成红色，
  // 也不给连登 / 旅行 / 抽奖这些在该站点上本就不存在的操作入口。
  if (account.note) {
    return (
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="font-medium">{account.nickname || account.id}</span>
        <Badge variant="outline">{account.site_label}</Badge>
        <span className="text-xs text-muted-foreground">{account.note}</span>
      </div>
    );
  }

  const busy = reporting || acting !== null;
  const travel = TRAVEL_LABEL[account.travel_state ?? ""] ?? "未出发";
  const travelReady = account.travel_state === "arrived";

  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-sm">
      <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2">
        <span className="font-medium">{account.nickname || account.id}</span>
        <Badge variant="outline">{account.site_label}</Badge>
        <span className="text-xs text-muted-foreground">
          任务 {account.task_count} · 待办 {account.pending_count + account.claimable_count + account.auto_count + account.manual_count}
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
        <span>
          连登 <span className="font-medium tabular-nums text-foreground">{account.streak}</span> 天
        </span>
        <span>
          旅行 <span className={cn(travelReady && "font-medium text-emerald-600 dark:text-emerald-400")}>{travel}</span>
        </span>
        <span>
          抽奖 <span className="font-medium tabular-nums text-foreground">{account.lottery_chances ?? 0}</span> 次
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-1.5">
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={busy}
          title="打开该账号的自动任务列表，可单项执行或一键完成全部（结果当场展示）"
          onClick={onOpenTasks}
        >
          <Sparkles className="size-3.5" aria-hidden="true" />
          一键完成
        </Button>
        <Button type="button" size="sm" variant="outline" disabled={busy} onClick={onReport}>
          {reporting ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : <Sparkles className="size-3.5" aria-hidden="true" />}
          补活跃
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={() => void onGrowth(account.id, "travel_depart", "猫咪出发")}
        >
          {acting === account.id + ":travel_depart" ? (
            <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
          ) : null}
          猫咪出发
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={() => void onGrowth(account.id, "travel_claim", "领旅行奖励")}
        >
          {acting === account.id + ":travel_claim" ? (
            <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
          ) : null}
          领旅行奖励
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={() => void onGrowth(account.id, "lottery_draw", "抽奖")}
        >
          {acting === account.id + ":lottery_draw" ? (
            <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
          ) : null}
          抽奖
        </Button>
        {/* 连登管家：补签 → 礼包/补偿 → 兑换已解锁档位 → 抽完所有次数（幂等，可重复点） */}
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={busy}
          title="补签保连登、领礼包/补偿、兑换已解锁的连登档位，并把抽奖次数抽完（已领的会自动跳过）"
          onClick={() => void onGrowth(account.id, "streak_bonus", "连登管家")}
        >
          {acting === account.id + ":streak_bonus" ? (
            <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
          ) : null}
          连登管家
        </Button>
      </div>
    </div>
  );
}

function TaskRow({
  task,
  running,
  onAction,
}: {
  task: GrowthTaskView;
  running: boolean;
  onAction: () => void;
}) {
  const meta = CATEGORY_META[task.category];
  const actionable = task.category === "claimable" || task.category === "accept" || task.category === "auto";
  const actionLabel = task.category === "claimable" ? "领取" : task.category === "accept" ? "报名" : "执行";

  return (
    // 已完成任务用淡色底区分（原来是整块 opacity-60）：任务标题、条件、积分
    // 在「已完成」状态下仍然值得回看，不该被一起调淡。
    <div
      className={cn(
        "flex flex-wrap items-start gap-x-4 gap-y-1.5 rounded-md py-3",
        task.category === "done" && "bg-muted/30",
      )}
    >
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-medium text-foreground">{task.title || task.task_code}</span>
          <span className="font-mono text-xs text-muted-foreground">{task.task_code}</span>
          {task.mp_only ? <Badge variant="outline">小程序</Badge> : null}
          {task.locked ? <Badge variant="outline">未解锁</Badge> : null}
          {task.account_count > 1 ? (
            <span className="text-xs text-muted-foreground">{task.account_count} 个账号</span>
          ) : null}
        </div>
        {task.condition ? (
          <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{task.condition}</p>
        ) : null}
      </div>

      <div className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-1.5">
        {task.credit > 0 ? (
          <Badge variant="outline" className="border-amber-500/40 text-amber-700 dark:text-amber-300">
            +{task.credit} 积分
          </Badge>
        ) : null}
        {task.energy > 0 ? (
          <Badge variant="outline" className="border-violet-500/40 text-violet-700 dark:text-violet-300">
            +{task.energy} 能量
          </Badge>
        ) : null}
        {task.target > 0 ? (
          <span
            className={cn(
              "text-xs tabular-nums",
              task.current >= task.target ? "text-emerald-600 dark:text-emerald-400" : "text-muted-foreground",
            )}
          >
            {task.current}/{task.target}
          </span>
        ) : null}
        <span className={cn("rounded px-1.5 py-0.5 text-xs font-medium", meta.tone)}>{meta.label}</span>
        {actionable ? (
          <Button type="button" size="sm" variant="outline" disabled={running} onClick={onAction}>
            {running ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : null}
            {actionLabel}
          </Button>
        ) : task.category === "manual" && task.action_reason ? (
          <span className="max-w-[260px] text-right text-xs leading-4 text-muted-foreground">
            {task.action_reason}
          </span>
        ) : null}
      </div>
    </div>
  );
}

const QUEUE_STATUS_META: Record<string, { icon: typeof CheckCircle2; className: string }> = {
  pending: { icon: ChevronRight, className: "text-muted-foreground" },
  running: { icon: Loader2, className: "animate-spin text-sky-600 dark:text-sky-400" },
  ok: { icon: CheckCircle2, className: "text-emerald-600 dark:text-emerald-400" },
  failed: { icon: XCircle, className: "text-destructive" },
  skipped: { icon: ChevronRight, className: "text-muted-foreground" },
};

function QueuePanel({ queue }: { queue: TaskQueueState }) {
  // phase 缺省时按 running/finished 兜底，兼容老后端。
  const phase = queue.phase ?? (queue.running ? "running" : queue.total > 0 ? "done" : "idle");
  const preparing = phase === "preparing";
  const progress =
    queue.total > 0 ? Math.round(((queue.done + queue.failed) / queue.total) * 100) : 0;

  let summary: string;
  if (preparing) {
    // 准备阶段没有项数可报，就明确说「在扫描」并给出可读提示，
    // 而不是显示一个不动的 0% —— 那种界面会让人以为卡死了。
    summary = queue.prepare_hint || "正在扫描待办任务…";
  } else if (queue.running) {
    summary = `执行中 ${queue.done + queue.failed}/${queue.total}`;
  } else {
    summary = `已完成：成功 ${queue.done}，失败 ${queue.failed}`;
  }
  if (!preparing && queue.failed > 0) {
    summary += " · 失败原因见每行说明或日志页";
  }
  if (queue.note) {
    summary += ` · ${queue.note}`;
  }

  return (
    <Section
      id="tasks-queue"
      title={
        <>
          执行队列
          {queue.running ? (
            <Loader2
              className="ml-2 inline size-3.5 animate-spin align-[-2px] text-sky-600 dark:text-sky-400"
              aria-hidden="true"
            />
          ) : null}
        </>
      }
      description={summary}
    >
      <CardContent className="space-y-3">
        {preparing ? (
          // 准备阶段用一个**不确定进度**的滑动条：项数还不知道，
          // 任何百分比都是编的。动画本身就是在告诉用户「还在动，别急」。
          <div
            className="h-1.5 overflow-hidden rounded-full bg-muted"
            role="progressbar"
            aria-label="正在扫描待办任务"
          >
            <div className="h-full w-1/3 animate-pulse rounded-full bg-primary" />
          </div>
        ) : (
          <div
            className="h-1.5 overflow-hidden rounded-full bg-muted"
            role="progressbar"
            aria-valuenow={progress}
            aria-valuemin={0}
            aria-valuemax={100}
          >
            <div
              className="h-full rounded-full bg-primary transition-[width] duration-500"
              style={{ width: `${progress}%` }}
            />
          </div>
        )}
        {preparing && queue.items.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            正在读取各账号的任务清单，这一步要打上游，通常需要几秒；完成后会自动开始逐项执行。
          </p>
        ) : null}
        <div className="space-y-1.5">
          {queue.items.slice(0, 60).map((item, index) => {
            const meta = QUEUE_STATUS_META[item.status] ?? QUEUE_STATUS_META.pending;
            const Icon = meta.icon;
            return (
              <div key={`${item.account}-${item.task_code}-${index}`} className="flex items-start gap-2 text-xs">
                <Icon className={cn("mt-0.5 size-3.5 shrink-0", meta.className)} aria-hidden="true" />
                <span className="min-w-0 flex-1 truncate">
                  {item.title || item.task_code}
                  <span className="ml-1.5 text-muted-foreground">{item.account_name || item.account}</span>
                </span>
                <Badge variant="outline" className="shrink-0 text-xs">
                  {KIND_LABEL[item.kind] ?? item.kind}
                </Badge>
                <span
                  className={cn(
                    "max-w-[45%] shrink-0 truncate text-right",
                    item.status === "failed" ? "text-destructive" : "text-muted-foreground",
                  )}
                  title={item.message}
                >
                  {item.message || (item.status === "pending" ? "等待执行" : "")}
                </span>
              </div>
            );
          })}
          {queue.items.length > 60 ? (
            <p className="text-xs text-muted-foreground">仅显示前 60 项，共 {queue.items.length} 项。</p>
          ) : null}
        </div>
      </CardContent>
    </Section>
  );
}
