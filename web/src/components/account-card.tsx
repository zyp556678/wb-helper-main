import { useEffect, useRef, useState } from "react";
import {
  AlertTriangle,
  ArrowRight,
  Building2,
  CalendarCheck,
  CalendarCheck2,
  CalendarDays,
  CalendarOff,
  Check,
  CheckCircle2,
  Clock,
  Coins,
  Ellipsis,
  Gauge,
  Globe,
  HeartPulse,
  Inbox,
  Globe2,
  Info,
  KeyRound,
  Layers,
  MonitorSmartphone,
  Package,
  PauseCircle,
  Power,
  PowerOff,
  RefreshCw,
  Sparkles,
  Star,
  Tag,
  Trash2,
} from "lucide-react";
import type * as React from "react";

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
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  TARGET_ICON,
  localAppShortLabel,
  localAppTargetLabel,
  targetCurrentAccountIds,
} from "@/components/local-apps-card";
import { useNowSeconds } from "@/lib/use-now-seconds";
import { creditResourceName } from "@/lib/credit-package-names";
import {
  accountDisplayName,
  accountIdentityLine,
  formatCredits,
  formatDuration,
  formatExpiryFull,
  maskEmail,
  siteLabel,
  toAccountView,
  toCreditPackageViews,
  toQuotaView,
  type AccountView,
  type CreditPackageTone,
  type CreditPackageView,
} from "@/lib/format";
import {
  PACKAGE_SORT_LABELS,
  PACKAGE_SORT_MODES,
  isPackageSortMode,
  usePackageSortMode,
} from "@/lib/package-sort";
import type { Account, CheckinStatusView, LocalAppTarget, PanelCreditAccount } from "@/lib/types";
import { cn } from "@/lib/utils";

/** 卡片内某次写操作的 pending 标识。 */
type PendingAction = "quota" | "toggle" | "pause" | "revive" | "remove" | "token" | "checkin" | "intl" | "plan";

/** 卡片上「近期到期」最多展示几行 —— 再多就该去积分统计页看完整列表了。 */
const EXPIRING_ROWS = 2;

/** 进度条配色：已到期红、7 天内橙、充裕绿。与徽章同一套语义色。 */
const TONE_BAR: Record<CreditPackageTone, string> = {
  expired: "bg-destructive",
  soon: "bg-amber-500",
  plenty: "bg-primary",
};

const TONE_TEXT: Record<CreditPackageTone, string> = {
  expired: "text-destructive",
  soon: "text-amber-700 dark:text-amber-300",
  plenty: "text-muted-foreground",
};

/** 限流倒计时：`2h14m 后恢复`；不足 1 分钟按「即将恢复」，已过期由调用方过滤。 */
function formatRateLimitRemaining(untilSeconds: number, now: number): string {
  const remainingMs = (untilSeconds - now) * 1000;
  if (remainingMs <= 0) return "即将恢复";
  const totalMinutes = Math.max(1, Math.ceil(remainingMs / 60_000));
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (hours > 0 && minutes > 0) return `${hours}h${minutes}m 后恢复`;
  if (hours > 0) return `${hours}h 后恢复`;
  return `${minutes}m 后恢复`;
}

/** 恢复时刻：今天 `17:59`、明天 `明天 09:00`、更远 `9/18 09:00`。 */
function formatRateLimitClock(untilSeconds: number, now: number): string {
  const reset = new Date(untilSeconds * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  const time = `${pad(reset.getHours())}:${pad(reset.getMinutes())}`;
  const midnight = (date: Date) =>
    new Date(date.getFullYear(), date.getMonth(), date.getDate()).getTime();
  const days = Math.round((midnight(reset) - midnight(new Date(now * 1000))) / 86_400_000);
  if (days <= 0) return time;
  if (days === 1) return `明天 ${time}`;
  return `${reset.getMonth() + 1}/${reset.getDate()} ${time}`;
}

/**
 * 标题行上的语义图标（小圆底），全部带 tooltip。
 *
 * 这是参考图里那一排图标的落地方式：参考图本身没说明每个图标的含义，
 * 所以不照搬外观，而是把网关**已经有的状态**映射成图标 ——
 * 每个图标都真的能点/能读出一个事实，不做纯装饰。
 */
function StatusIcon({
  icon: Icon,
  label,
  tooltip,
  count,
  className,
}: {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  /** 自定义 tooltip 内容（多行列表等）；缺省显示 label 文本。 */
  tooltip?: React.ReactNode;
  /** >1 时在图标右上角显示数量角标。 */
  count?: number;
  className?: string;
}) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          tabIndex={0}
          aria-label={label}
          className={cn(
            "relative flex size-8 shrink-0 cursor-default items-center justify-center rounded-full transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
            className,
          )}
        >
          <Icon className="size-4" aria-hidden="true" />
          {count && count > 1 ? (
            // 数量角标：switch 用 10px 字号，本项目有「最小 12px」的可读性契约，
            // 按契约抬到 text-xs 并把角标略微放大（已在本文件注释声明该偏离）。
            <span className="absolute -right-0.5 -top-0.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-amber-500 px-0.5 text-xs font-semibold leading-none text-white">
              {count}
            </span>
          ) : null}
        </span>
      </TooltipTrigger>
      <TooltipContent side="bottom">{tooltip ?? label}</TooltipContent>
    </Tooltip>
  );
}

/** 状态徽章 + tooltip，与图标行配合使用。 */
function StateIcon({ account, view }: { account: Account; view: AccountView }) {
  const hint =
    view.state === "disabled"
      ? account.disabled_reason || "该账号已在配置中禁用"
      : view.state === "cooldown"
        ? account.cooldown_reason || `冷却剩余 ${formatDuration(view.cooldownRemaining)}`
        : "Token 有效，可正常参与调度";
  const tone =
    view.state === "active"
      ? "bg-primary/12 text-primary-ink"
      : view.state === "cooldown"
        ? "bg-amber-500/15 text-amber-800 dark:text-amber-300"
        : "bg-destructive/15 text-destructive";
  return (
    <StatusIcon
      icon={view.state === "active" ? CheckCircle2 : view.state === "cooldown" ? Clock : PowerOff}
      label={`${view.stateLabel} · ${hint}`}
      className={tone}
    />
  );
}

/** 「···」更多操作菜单。用轻量 popover 而不是引入 dropdown-menu 依赖（本项目没装）。 */
function MoreMenu({
  account,
  view,
  busy,
  pending,
  onToggleDisabled,
  onTogglePaused,
  onRevive,
  onRemoveRequest,
  onRefreshQuota,
  onRefreshToken,
  onCheckin,
  onActivateIntl,
  onIdentifyPlan,
  onShowInfo,
}: {
  account: Account;
  view: AccountView;
  busy: boolean;
  pending: PendingAction | null;
  onToggleDisabled: () => void;
  /** 暂停 / 恢复「选号」：只不派发，维护任务照常（与禁用是两种状态）。 */
  onTogglePaused: () => void;
  /** 人工复活：清除禁用 + 冷却 + 熔断 + 连败降权（余额类冷却不清）。 */
  onRevive: () => void;
  onRemoveRequest: () => void;
  onRefreshQuota: () => void;
  onRefreshToken: () => void;
  onCheckin: () => void;
  /** 国际站账号：完成注册激活（补地区）并领 trial 加油包。 */
  onActivateIntl: () => void;
  /** 完整识别套餐（查权益列表，回答「哪一档、还有效吗」）。 */
  onIdentifyPlan: () => void;
  /** 打开「账号信息」弹窗（查看企业名 / UID、编辑备注、选择显示字段）。 */
  onShowInfo: () => void;
}) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement | null>(null);

  // 点击外部 / Esc 关闭。没有用 Radix 的 popover，这两个行为得自己兜。
  useEffect(() => {
    if (!open) return;
    function onPointerDown(event: MouseEvent) {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    }
    function onKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("mousedown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, [open]);

  const itemClass =
    "flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-left text-xs transition-colors hover:bg-accent hover:text-accent-foreground disabled:pointer-events-none disabled:opacity-50";

  return (
    <div ref={rootRef} className="relative shrink-0">
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            aria-label="更多操作"
            aria-expanded={open}
            aria-haspopup="menu"
            disabled={busy}
            onClick={() => setOpen((v) => !v)}
            className={cn(
              "flex size-8 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-foreground/5 hover:text-foreground disabled:opacity-50",
              open && "bg-foreground/5 text-foreground",
            )}
          >
            <Ellipsis className="size-4" aria-hidden="true" />
          </button>
        </TooltipTrigger>
        <TooltipContent side="bottom">更多操作</TooltipContent>
      </Tooltip>

      {open ? (
        <div
          role="menu"
          className="absolute right-0 top-full z-30 mt-1.5 w-44 rounded-xl border border-border bg-popover p-1 shadow-lg"
        >
          {/*
            「刷新积分」同时存在于页脚按钮与本菜单：紧凑模式会隐藏页脚，
            若只有页脚有，紧凑模式下就再也刷不了单个账号的积分 ——
            紧凑模式是为了省空间，不是为了砍功能。
          */}
          <button
            type="button"
            role="menuitem"
            className={itemClass}
            disabled={busy}
            onClick={() => {
              setOpen(false);
              onRefreshQuota();
            }}
          >
            <Coins className="size-3.5" aria-hidden="true" />
            刷新积分
          </button>
          <button
            type="button"
            role="menuitem"
            className={itemClass}
            disabled={busy}
            onClick={() => {
              setOpen(false);
              onRefreshToken();
            }}
          >
            <KeyRound className="size-3.5" aria-hidden="true" />
            刷新 Token
          </button>
          {/* 国际站账号才需要注册激活：新号没补注册地区时对话会报 14017 trial not activated。 */}
          {account.site === "intl" ? (
            <button
              type="button"
              role="menuitem"
              className={itemClass}
              disabled={busy}
              title="补注册地区并激活账号，顺带领取 trial 加油包（已激活/已领过会自动跳过）"
              onClick={() => {
                setOpen(false);
                onActivateIntl();
              }}
            >
              <Globe2 className="size-3.5" aria-hidden="true" />
              国际站激活
            </button>
          ) : null}
          {/* 完整套餐识别：额度刷新只给摘要级套餐名，这条会查权益列表确认档位与有效期。 */}
          <button
            type="button"
            role="menuitem"
            className={itemClass}
            disabled={busy}
            title="查询订阅权益列表，确认套餐档位（国内站四档）与是否仍在有效期内"
            onClick={() => {
              setOpen(false);
              onIdentifyPlan();
            }}
          >
            <Layers className="size-3.5" aria-hidden="true" />
            识别套餐
          </button>
          {/* 账号信息（批次 4）：查看企业名 / UID、编辑本地备注、选卡片显示字段。 */}
          <button
            type="button"
            role="menuitem"
            className={itemClass}
            disabled={busy}
            onClick={() => {
              setOpen(false);
              onShowInfo();
            }}
          >
            <Info className="size-3.5" aria-hidden="true" />
            账号信息
          </button>
          {/*
            签到只在**做得到**的账号上出现，两条互斥条件：
              - 站点：国际站后端直接 400（没有签到接口）；
              - 账号类型：企业版没有个人成长体系，上游回 400 code 10001。
            与其让用户点了才发现，不如不显示 —— 但**不能用禁用态代替隐藏**：
            灰着一条「不支持」的菜单项，等于每条菜单都在提醒一个恒定不变的事实。
          */}
          {account.site !== "intl" && !account.is_enterprise ? (
            <button
              type="button"
              role="menuitem"
              className={itemClass}
              disabled={busy}
              onClick={() => {
                setOpen(false);
                onCheckin();
              }}
            >
              <CalendarCheck className="size-3.5" aria-hidden="true" />
              手动签到
            </button>
          ) : null}
          <button
            type="button"
            role="menuitem"
            className={itemClass}
            disabled={busy}
            onClick={() => {
              setOpen(false);
              onToggleDisabled();
            }}
          >
            {account.disabled ? (
              <Power className="size-3.5" aria-hidden="true" />
            ) : (
              <PowerOff className="size-3.5" aria-hidden="true" />
            )}
            {account.disabled ? "启用账号" : "禁用账号"}
          </button>
          {/*
            暂停选号：与"禁用账号"刻意并列而不是合并 —— 暂停只把号从派发里摘出来，
            签到 / 保活 / 旅行 / 成长任务照常跑。用在"这个号最近容易被风控，先让它
            歇一阵，但别让它掉队"。禁用的号连维护都停（除非开了 include_disabled_in_tasks），
            两者混成一个按钮，用户就没法表达"只想让它别发请求"这个意思了。
          */}
          <button
            type="button"
            role="menuitem"
            className={itemClass}
            disabled={busy}
            onClick={() => {
              setOpen(false);
              onTogglePaused();
            }}
          >
            <PauseCircle className="size-3.5" aria-hidden="true" />
            {account.paused ? "恢复选号" : "暂停选号"}
          </button>
          {/* 「复活」只在账号确实处于非正常状态时出现（禁用 / 冷却中）。
              恒显示的代价是每张卡都多一条用不到的菜单项，而它的语义
              （清熔断与降权）只对「被治理层挡住」的账号有意义。 */}
          {account.disabled || (account.cooldown_reason ?? "") !== "" ? (
            <button
              type="button"
              role="menuitem"
              className={itemClass}
              disabled={busy}
              onClick={() => {
                setOpen(false);
                onRevive();
              }}
              title="清除禁用、冷却、熔断与连败降权（余额类冷却需签到或额度恢复）"
            >
              <HeartPulse className="size-3.5" aria-hidden="true" />
              复活账号
            </button>
          ) : null}
          <button
            type="button"
            role="menuitem"
            className={cn(
              itemClass,
              "text-destructive hover:bg-destructive/10 hover:text-destructive",
            )}
            disabled={busy}
            onClick={() => {
              setOpen(false);
              onRemoveRequest();
            }}
          >
            <Trash2 className="size-3.5" aria-hidden="true" />
            {/* 文案与 wb-switch 对齐用「删除账号」；本项目默认语义是**归档**
                （可恢复），勾选彻底删除才不可撤销 —— 说明写在确认弹窗里。 */}
            删除账号
          </button>
          {pending !== null ? (
            <p className="px-2.5 py-1 text-xs text-muted-foreground">
              {pending === "checkin"
                ? "正在签到…"
                : pending === "token"
                  ? "正在刷新 Token…"
                  : pending === "quota"
                    ? "正在刷新积分…"
                    : pending === "revive"
                      ? "正在复活…"
                      : "正在提交…"}
            </p>
          ) : null}
          <p className="mt-0.5 border-t border-border/60 px-2.5 pb-0.5 pt-1.5 font-mono text-xs leading-4 text-muted-foreground">
            {view.stateLabel} · {siteLabel(account)}
          </p>
        </div>
      ) : null}
    </div>
  );
}

/** 单条「近期到期」明细：金额 + 类型 + 到期时间 + 细进度条。 */
function ExpiringRow({ pkg }: { pkg: CreditPackageView }) {
  return (
    <li className="min-w-0">
      <div className="flex min-w-0 items-center gap-2 text-xs">
        <span className="shrink-0 font-medium tabular-nums text-foreground">
          {formatCredits(pkg.remaining)}
          <span className="ml-1 font-normal text-muted-foreground">积分</span>
        </span>
        <span className="min-w-0 flex-1 truncate text-muted-foreground" title={pkg.name}>
          {pkg.name}
        </span>
        <span className={cn("shrink-0 tabular-nums", TONE_TEXT[pkg.tone])}>
          {pkg.expired ? "已到期" : pkg.expireLabel}
        </span>
      </div>
      <div className="mt-1.5 h-1.5 w-full overflow-hidden rounded-full bg-muted" aria-hidden="true">
        <div
          className={cn("h-full rounded-full transition-[width] duration-300", TONE_BAR[pkg.tone])}
          style={{ width: `${Math.round(pkg.ratio * 100)}%` }}
        />
      </div>
    </li>
  );
}

export interface AccountCardProps {
  account: Account;
  now: number;
  /** 该账号的逐包明细（GET /panel/api/credits）；null 表示无数据或查询失败。 */
  credits?: PanelCreditAccount | null;
  /** 明细接口是否已返回过（用于区分「加载中」与「确实没有」）。 */
  creditsLoaded?: boolean;
  /**
   * 今日签到状态。
   *
   * `undefined` = 还没查到（不显示）；`unsupported` = 该站点没有签到（不显示）；
   * `error` 非空 = 这次没查成（也不显示，见下方注释）。
   */
  checkin?: CheckinStatusView | null;
  /**
   * 是否参与自动签到（批次 4；后端 `auto_checkin_enabled`）。
   * `false` = 已关闭（在排除名单里），显示「自动签到已关闭」chip；
   * 缺省/undefined = 未知或旧后端，按参与处理（不显示该 chip）。
   */
  autoCheckinEnabled?: boolean;
  /**
   * 本机各目标（WorkBuddy 客户端 / CodeBuddy CLI / JetBrains 插件）的探测结果，
   * 由「本机应用接入」卡片上报 —— 与它共用同一份数据，不在这里再拉一次接口。
   */
  localTargets?: LocalAppTarget[];
  /** 把本卡账号设为某本机目标的当前登录（打开本机页同一个切换对话框）。 */
  onSwitchLocalApp?: (target: LocalAppTarget, accountId: string) => void;
  /**
   * 该账号当前是本机哪些应用的登录账号（由「本机应用接入」探测得出）。
   *
   * 空/未传表示它不是任何应用的当前账号 —— 这里**只显示正向信息**，
   * 不去显示「未接入 X」：那会让每张卡都多出一排否定式文案。
   */
  currentIn?: string[];
  /** 刷新单账号额度；由父层负责 toast 与刷新。 */
  onRefreshQuota: (id: string) => Promise<void>;
  /** 设置禁用 / 启用状态。 */
  onSetDisabled: (id: string, disabled: boolean) => Promise<void>;
  /** 暂停 / 恢复「选号」：只不派发，维护任务照常（见 pool.SetPaused）。 */
  onSetPaused: (id: string, paused: boolean) => Promise<void>;
  /**
   * 人工复活：清除禁用 + 冷却 + 熔断 + 连败降权。
   *
   * 与 onSetDisabled(id,false) 是**两个不同的动作**，不要合并：
   * 那一个只改「禁用」位，这一个才是运维口径的无条件恢复。
   */
  onRevive: (id: string) => Promise<void>;
  /** 移除账号；deleteFile=true 时同时删除凭据文件。 */
  onRemove: (id: string, deleteFile: boolean) => Promise<void>;
  /** 强制刷新登录令牌。 */
  onRefreshToken: (id: string) => Promise<void>;
  /** 国际站账号：注册激活 + 领 trial 加油包。 */
  onActivateIntl: (id: string) => Promise<void>;
  /** 完整识别套餐（多打一次分页权益查询）。 */
  onIdentifyPlan: (id: string) => Promise<void>;
  /** 手动签到（仅国内站账号会用到）。 */
  onCheckin: (id: string) => Promise<void>;
  /** 打开「账号信息」弹窗（查看信息、编辑备注、选择显示字段）。 */
  onShowInfo?: (account: Account) => void;
  /**
   * 是否建议优先使用该账号（按积分最早到期的那个）。
   *
   * 由父层统一判定而不是卡片自己算：这是**跨账号**的比较，
   * 单张卡片看不到别的账号，自己算只能拍脑袋。
   */
  recommended?: boolean;
  /**
   * 紧凑模式：去掉「近期到期」明细与页脚，只留标题 / 积分总额 / 有效期区间。
   *
   * 给「账号多、只想扫一眼谁快到期」的场景用。**不做成折叠**是有意的 ——
   * 折叠要一个展开动作，而紧凑模式的目的正是省掉逐个展开。
   */
  compact?: boolean;
  /** 跳到「积分统计」看完整积分包列表；未提供时该链接降级为纯文本。 */
  onViewAllPackages?: () => void;
}

export function AccountCard({
  account,
  now,
  credits = null,
  creditsLoaded = false,
  checkin = null,
  autoCheckinEnabled,
  localTargets,
  onSwitchLocalApp,
  currentIn,
  onRefreshQuota,
  onSetDisabled,
  onSetPaused,
  onRevive,
  onRemove,
  onRefreshToken,
  onCheckin,
  onActivateIntl,
  onIdentifyPlan,
  onShowInfo,
  recommended = false,
  compact = false,
  onViewAllPackages,
}: AccountCardProps) {
  const view = toAccountView(account, now);
  const quota = toQuotaView(account);
  // 模型级冷却（429/6004 模型限流）的倒计时：30s 粒度刷新即可
  //（文案显示到分钟；switch 用每秒刷新，这里降频减少整卡重渲染）。
  // 已过恢复时刻的条目按本地时钟即时过滤，不依赖后端再刷新。
  const rateLimitNow = useNowSeconds(30_000);
  const rateLimits = (account.model_cooldowns ?? [])
    .filter((item) => Number.isFinite(item.until) && item.until > rateLimitNow)
    .sort((left, right) => left.until - right.until);
  const rateLimitLines = rateLimits.map(
    (item) =>
      `${item.model || "未知模型"} · ${formatRateLimitRemaining(item.until, rateLimitNow)}（${formatRateLimitClock(item.until, rateLimitNow)}）`,
  );
  const [pending, setPending] = useState<PendingAction | null>(null);
  const [removeOpen, setRemoveOpen] = useState(false);
  const [deleteFile, setDeleteFile] = useState(false);
  const [packagesOpen, setPackagesOpen] = useState(false);
  const busy = pending !== null;
  // 逐包排序偏好：默认到期升序（近的在前），可切面额降序；持久化 + 同页各处同步。
  const [packageSortMode, setPackageSortMode] = usePackageSortMode();

  // 显示名按 display_field 选（备注 / 手机号回退 / 昵称），身份行用 uid
  //（本项目凭据没有 email 字段，uid 是等价的身份信息）。
  const name = accountDisplayName(account);
  const identity = accountIdentityLine(account);
  // 可切换的本机目标：只保留「我们实现了写入」的（VS Code / CodeBuddy IDE
  // 从未实现写入，writable=false，天然排除在入口之外）。
  const writableTargets = (localTargets ?? []).filter((t) => t.writable);

  // 逐包明细 → 视图：排序跟随弹窗顶部的选择（默认到期升序，近的在前）。
  // 卡片这一块「近期到期」与弹窗明细共用同一份视图，两处行序始终一致。
  //
  // **剩余为 0 的包一律不进这两个列表**。两条理由：
  //   - 「近期到期」那两行是留给「该优先处理哪个」的，用完的包不该占位置；
  //   - 它也不该参与下面的有效期区间 —— 一个已经没有余额的包，其到期日不约束
  //     任何东西，用户要看的是「还能用的那些什么时候作废」。
  const allPackages = toCreditPackageViews(
    credits?.resources,
    creditResourceName,
    packageSortMode,
  );
  const packages = allPackages.filter((p) => p.remaining > 0);
  const expiring = packages.slice(0, EXPIRING_ROWS);
  const hasPackages = packages.length > 0;
  /** 有明细但余额全为 0 —— 空态要说清是「用完了」而不是「没数据」，否则像是接口没返回。 */
  const allDepleted = !hasPackages && allPackages.length > 0;

  /**
   * 是否展示今日签到状态。
   *
   * 三种情况都不显示，且**理由各不相同**：
   *   - `undefined`：还没查到（首屏那一两秒），显示「未签到」是凭空断言；
   *   - `unsupported`：该站点（国际站）根本没有签到，显示出来是噪音；
   *   - `error`：这次没查成。**刻意不显示任何状态**，而不是显示「今日未签到」——
   *     后者会把「没查到」讲成「还没签」，用户照着去点，却拿到「今天已签到」。
   *     查询失败的原因由「手动签到」按钮在真正操作时给出，那里才是权威结论。
   */
  const showCheckin = checkin != null && !checkin.unsupported && !checkin.error;

  // 积分有效期区间：最先到期 → 最晚到期。
  //
  // 为什么要它取代「Token 有效期」：凭据 token 的到期时间与「积分什么时候作废」无关 ——
  // 实测同一张卡上 token 有效期是 2027-09-29，而积分 9/30 就到期。盯着前者会漏掉真正
  // 该处理的日期，用户看到的就是「明明写着还有 364 天，积分却清零了」。
  //
  // 口径两点：
  //   - 只看**未过期**的包。已过期的包 expireAt 在过去，会永远占据「最先到期」的位置，
  //     让这一栏长期显示一个已经发生的日期。
  //   - `expireAt === 0`（长期有效）不参与端点，否则「最晚到期」会落在永不到期的包上，
  //     区间失去意义 —— 全为长期有效时这一栏直接写「长期有效」。
  const livePackages = packages.filter((p) => p.expireAt > 0 && !p.expired);
  const datedPackages =
    livePackages.length > 0 ? livePackages : packages.filter((p) => p.expireAt > 0);
  // 端点是 min/max 而不是首尾下标：列表顺序跟随逐包排序模式（面额降序时
  // 首个元素未必是最早到期），按下标取会把区间算错。
  const earliestAt = datedPackages.reduce(
    (min, p) => (min === 0 || p.expireAt < min ? p.expireAt : min),
    0,
  );
  const latestAt = datedPackages.reduce((max, p) => (p.expireAt > max ? p.expireAt : max), 0);
  const expiryRange =
    earliestAt > 0 && latestAt > 0
      ? earliestAt === latestAt
        ? formatExpiryFull(earliestAt)
        : `${formatExpiryFull(earliestAt)} ~ ${formatExpiryFull(latestAt)}`
      : null;

  /** 统一处理 loading：无论成功失败都复位，toast 交给父层。 */
  async function run(kind: PendingAction, action: () => Promise<void>) {
    setPending(kind);
    try {
      await action();
    } finally {
      setPending(null);
    }
  }

  return (
    <Card className="flex min-w-0 flex-col gap-0 overflow-hidden rounded-2xl py-0 shadow-none transition-shadow hover:border-foreground/15">
      {/*
        标题行：账号名是识别一个账号的第一信息，所以占左；右侧一排图标把
        「这个账号是什么状态」压缩成可扫的形状，再右边是「···」把破坏性操作收起来。
        参考图里账号名前有个头像，这里去掉 —— 头像不承载任何信息，而图标承载状态。
      */}
      <header className="flex min-h-[64px] items-center gap-2.5 border-b border-border px-4 py-2.5">
        {/* 首字母头像 + 名称 + 身份行（对齐 wb-switch 的卡片头部）。
            邮箱形态的名称仍遮成 a***@domain：账号名最容易在截图里泄露个人信息。 */}
        <span
          aria-hidden="true"
          className="flex size-9 shrink-0 items-center justify-center rounded-full bg-primary/12 text-sm font-semibold text-primary-ink"
        >
          {name.charAt(0).toUpperCase()}
        </span>
        <div className="min-w-0 flex-1">
          <h3 className="min-w-0 truncate text-[15px] font-semibold leading-6" title={name}>
            {maskEmail(name)}
          </h3>
          {identity ? (
            <p className="truncate text-xs leading-5 text-muted-foreground" title={identity}>
              {identity}
            </p>
          ) : null}
        </div>

        <div className="flex shrink-0 items-center gap-1">
          {/* 紧凑模式的本机工具入口：图标按钮，当前账号显示勾选角标。
              非紧凑模式在页脚有同样入口（这里是给紧凑模式补的，因为页脚被隐藏）。 */}
          {compact
            ? writableTargets.map((target) => {
                const Icon = TARGET_ICON[target.id];
                // 「当前」按**全部站点**的当前登录判定：WorkBuddy 的国内站与国际站
                // 是两个独立客户端，国际站那份登录也要能把本卡标成当前。
                const current = targetCurrentAccountIds(target).includes(account.id);
                // 显示名按**账号站点**取：国际站账号旁边必须写 WorkBuddy AI，
                // 否则看起来像在写国内站那个隔离的文件（见 workBuddyClientLabel）。
                const appLabel = localAppTargetLabel(target.id, target.label, account.site);
                // WorkBuddy 客户端切换会先关掉客户端再写（不关会被它退出时的回写覆盖），
                // 悬停提示里先说清楚，免得用户以为面板擅自关了他的客户端。
                const restartHint =
                  target.id === "workbuddy-desktop" ? "（会先关闭客户端，写入后再打开）" : "";
                const label = current
                  ? `${appLabel} 当前账号`
                  : `设为 ${appLabel} 的当前登录${restartHint}`;
                return (
                  <Tooltip key={target.id}>
                    <TooltipTrigger asChild>
                      <button
                        type="button"
                        disabled={current || !onSwitchLocalApp}
                        aria-label={label}
                        aria-current={current ? "true" : undefined}
                        onClick={() => onSwitchLocalApp?.(target, account.id)}
                        className={cn(
                          "relative flex size-7 items-center justify-center rounded-lg border transition-colors",
                          current
                            ? "border-primary/25 bg-primary/10 text-primary-ink"
                            : "border-border text-muted-foreground hover:bg-accent hover:text-foreground",
                          "disabled:cursor-default disabled:opacity-100 disabled:hover:bg-transparent",
                        )}
                      >
                        <Icon className="size-3.5" aria-hidden="true" />
                        {current ? (
                          <span className="absolute -right-1 -top-1 flex size-3 items-center justify-center rounded-full bg-primary text-primary-foreground">
                            <Check className="size-2" strokeWidth={3} aria-hidden="true" />
                          </span>
                        ) : null}
                      </button>
                    </TooltipTrigger>
                    <TooltipContent side="bottom">{label}</TooltipContent>
                  </Tooltip>
                );
              })
            : null}
          {/*
            「建议优先」：只标一个账号，弱化但不隐去。
            用 Sparkles 而非彩色填充徽章 —— 卡片标题行已经有一排状态图标，
            再加一个高饱和色块会把「状态」和「建议」混成一类信息。
          */}
          {recommended ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <span
                  className="inline-flex shrink-0 items-center gap-1 rounded-full bg-primary/12 px-2 py-0.5 text-xs font-medium text-primary-ink"
                  aria-label="建议优先使用"
                >
                  <Sparkles className="size-3" aria-hidden="true" />
                  建议优先
                </span>
              </TooltipTrigger>
              <TooltipContent side="bottom">积分最早到期，建议优先使用</TooltipContent>
            </Tooltip>
          ) : null}
          {/*
            今日签到 / 自动签到状态（批次 4，与 wb-switch 的状态 chip 同语义）。
            两枚互斥：
              - 已关闭自动签到：CalendarOff + 说明（刷新时也会忽略，仍可手动签到）；
              - 否则显示今日签到状态（查询失败/不支持时不显示，理由见 showCheckin）。
          */}
          {autoCheckinEnabled === false ? (
            <StatusIcon
              icon={CalendarOff}
              label="该账号已关闭自动签到，刷新时也会忽略，仍可手动签到"
              className="bg-muted text-muted-foreground"
            />
          ) : showCheckin && checkin ? (
            <StatusIcon
              icon={checkin.today_checked_in ? CalendarCheck2 : CalendarDays}
              label={checkin.today_checked_in ? "今日已签到" : "今日未签到"}
              className={
                checkin.today_checked_in
                  ? "bg-primary/12 text-primary-ink"
                  : "bg-muted text-muted-foreground"
              }
            />
          ) : null}
          {/*
            模型限额（429/6004 模型级限流，对齐 wb-switch 的限额 chip）：
            只在该账号当前有受限模型时渲染；悬停按恢复时间升序列出
            「模型 · 倒计时（恢复时刻）」，受限模型 >1 时带数量角标。
            冷却只作用于「账号 × 模型」——切其他模型立即可用，所以这里
            是提醒而不是「账号不可用」的告警。
          */}
          {rateLimits.length > 0 ? (
            <StatusIcon
              icon={Gauge}
              label={`模型限额：${rateLimitLines.join("；")}`}
              tooltip={
                <span className="flex flex-col gap-0.5">
                  {rateLimitLines.map((line) => (
                    <span key={line}>{line}</span>
                  ))}
                </span>
              }
              count={rateLimits.length}
              className="bg-amber-500/15 text-amber-800 dark:text-amber-300"
            />
          ) : null}
          {/* 站点：国内站品牌绿 / 国际站冷蓝 */}
          <StatusIcon
            icon={account.site === "intl" ? Globe : Layers}
            label={`站点：${siteLabel(account)}`}
            className={
              account.site === "intl"
                ? "bg-sky-500/15 text-sky-600 dark:text-sky-300"
                : "bg-primary/12 text-primary-ink"
            }
          />
          {/* 套餐 / 付费 */}
          {quota.known && quota.plan ? (
            <StatusIcon
              icon={Star}
              label={`套餐：${quota.plan}${quota.paid ? "（付费）" : ""}`}
              className={
                quota.paid || quota.plan === "pro"
                  ? "bg-primary/12 text-primary-ink"
                  : "bg-muted text-muted-foreground"
              }
            />
          ) : null}
          {/* 运行状态：可用 / 冷却 / 禁用 */}
          <StateIcon account={account} view={view} />
          {/*
            企业账号：上游没有个人成长体系，签到 / 成长任务 / 连登 / 旅行 / 夜猫子
            这些入口**已经直接隐藏**（见上面的手动签到分支与 tasks 侧的门控）。
            标签的作用是给「为什么少了那些按钮」一个答案。
            判据用后端下发的 is_enterprise（= enterprise_id 非空），前端不再自己判一遍。
          */}
          {account.is_enterprise ? (
            <StatusIcon
              icon={Building2}
              label={`企业版账号 · ${account.enterprise_id}｜上游没有个人成长体系，签到与成长任务不可用`}
              className="bg-violet-500/15 text-violet-600 dark:text-violet-300"
            />
          ) : null}
          {/* Token 即将到期 / 已过期 */}
          {view.tokenExpired || view.tokenExpiringSoon ? (
            <StatusIcon
              icon={AlertTriangle}
              label={
                view.tokenExpired
                  ? `Token 已过期${view.tokenRemaining ? `（${view.tokenRemaining.replace(/^已过期 /, "")}）` : ""}`
                  : `Token 有效期：${view.tokenLabel}${view.tokenRemaining ? `（${view.tokenRemaining}）` : ""}`
              }
              className={
                view.tokenExpired
                  ? "bg-destructive/15 text-destructive"
                  : "bg-amber-500/15 text-amber-800 dark:text-amber-300"
              }
            />
          ) : null}

          <MoreMenu
            account={account}
            view={view}
            busy={busy}
            pending={pending}
            onTogglePaused={() =>
              void run("pause", () => onSetPaused(account.id, !account.paused))
            }
            onToggleDisabled={() =>
              void run("toggle", () => onSetDisabled(account.id, !account.disabled))
            }
            onRevive={() => void run("revive", () => onRevive(account.id))}
            onRemoveRequest={() => setRemoveOpen(true)}
            onRefreshQuota={() => void run("quota", () => onRefreshQuota(account.id))}
            onRefreshToken={() => void run("token", () => onRefreshToken(account.id))}
            onCheckin={() => void run("checkin", () => onCheckin(account.id))}
            onActivateIntl={() => void run("intl", () => onActivateIntl(account.id))}
            onIdentifyPlan={() => void run("plan", () => onIdentifyPlan(account.id))}
            onShowInfo={() => onShowInfo?.(account)}
          />
        </div>
      </header>

      {view.tokenExpired ? (
        <div className="flex items-center gap-2 border-b border-destructive/20 bg-destructive/8 px-4 py-2 text-xs font-medium text-destructive">
          <AlertTriangle className="size-3.5 shrink-0" aria-hidden="true" />
          <span>
            Token{" "}
            {view.tokenRemaining
              ? `已过期（${view.tokenRemaining.replace(/^已过期 /, "")}）`
              : "已失效"}
            ，请尽快重新授权
          </span>
        </div>
      ) : null}

      <section className="flex min-w-0 flex-1 flex-col px-4 pb-4 pt-3.5">
        {/* 主数字：积分总额 + 积分包数量 + 更新时间 */}
        <div className="flex items-baseline gap-x-2.5 gap-y-1">
          <Coins className="size-4 shrink-0 self-center text-muted-foreground" aria-hidden="true" />
          <strong className="font-display text-[26px] font-semibold leading-none tracking-[-0.025em] tabular-nums">
            {quota.known ? formatCredits(quota.remaining) : "—"}
          </strong>
          <span className="shrink-0 text-xs text-muted-foreground">
            {hasPackages ? `${packages.length} 个积分包` : quota.known ? "剩余积分" : "未查询"}
          </span>
          <div className="ml-auto flex shrink-0 items-center gap-1.5 self-center text-xs text-muted-foreground">
            <Clock className="size-3.5 shrink-0" aria-hidden="true" />
            <span className="whitespace-nowrap tabular-nums">
              {quota.updatedLabel ? `${quota.updatedLabel.slice(11)} 更新` : "—"}
            </span>
          </div>
        </div>

        {/* 「近期到期」明细：紧凑模式下整块省掉（它只服务「谁快到期」，而紧凑模式
            的用途正是快速扫过很多账号，不逐个看明细）。 */}
        {compact ? null : (
          <>
            <div className="mt-4 flex items-center justify-between gap-2">
              <span className="text-xs font-medium text-muted-foreground">近期到期</span>
              {hasPackages ? (
                <button
                  type="button"
                  onClick={() => setPackagesOpen(true)}
                  className="inline-flex shrink-0 items-center gap-0.5 rounded text-xs font-medium text-primary-ink transition-colors hover:text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                >
                  查看全部积分包
                  <ArrowRight className="size-3.5" aria-hidden="true" />
                </button>
              ) : null}
            </div>

            {hasPackages ? (
              <ul className="mt-2.5 flex flex-col gap-2.5">
                {expiring.map((pkg) => (
                  <ExpiringRow key={pkg.key} pkg={pkg} />
                ))}
              </ul>
            ) : !creditsLoaded ? (
              <p className="mt-2.5 text-xs text-muted-foreground">正在读取积分包明细…</p>
            ) : (
              <p className="mt-2.5 flex items-center gap-1.5 text-xs text-muted-foreground">
                <Inbox className="size-3.5 shrink-0" aria-hidden="true" />
                {credits && !credits.ok && credits.error
                  ? credits.error
                  : allDepleted
                    ? "积分包已全部用完"
                    : "暂无积分包明细"}
              </p>
            )}
          </>
        )}

        {/* 次要信息一行：积分有效期区间 + 冷却；不再用大进度条，参考图没有 */}
        <div className="mt-3.5 flex flex-wrap items-center gap-x-5 gap-y-1.5 border-t border-border/60 pt-3 text-xs">
          <span className="flex min-w-0 items-center gap-1.5">
            <span className="shrink-0 text-muted-foreground">
              {hasPackages ? "积分有效期" : "Token 有效期"}
            </span>
            {hasPackages ? (
              <span className="font-mono text-foreground">{expiryRange ?? "长期有效"}</span>
            ) : (
              <>
                <span
                  className={cn(
                    "font-mono",
                    view.tokenExpired
                      ? "font-medium text-destructive"
                      : view.tokenExpiringSoon
                        ? "font-medium text-amber-700 dark:text-amber-300"
                        : "text-foreground",
                  )}
                >
                  {view.tokenLabel}
                </span>
                {view.tokenRemaining ? (
                  <span
                    className={cn(
                      view.tokenExpired
                        ? "text-destructive"
                        : view.tokenExpiringSoon
                          ? "text-amber-700 dark:text-amber-300"
                          : "text-muted-foreground",
                    )}
                  >
                    （{view.tokenRemaining}）
                  </span>
                ) : null}
              </>
            )}
          </span>

          {view.cooldownRemaining > 0 ? (
            <span className="flex items-center gap-1 text-amber-700 dark:text-amber-300">
              <Clock className="size-3" aria-hidden="true" />
              冷却剩余 {formatDuration(view.cooldownRemaining)}
            </span>
          ) : null}

          {currentIn && currentIn.length > 0 ? (
            <span className="flex items-center gap-1 text-primary-ink">
              <MonitorSmartphone className="size-3" aria-hidden="true" />
              当前登录：{currentIn.join("、")}
            </span>
          ) : null}

          <span
            className="ml-auto min-w-0 truncate font-mono text-xs text-muted-foreground"
            title={account.file}
          >
            {account.file}
          </span>
        </div>
      </section>

      {/* 页脚：紧凑模式下整块省掉。它的内容是「刷新积分」按钮 + 站点/套餐 + 成功失败计数，
          都属于「决定了要处理这个账号之后才需要」的信息。 */}
      {compact ? null : (
        <footer className="flex flex-wrap items-center gap-1.5 border-t border-border/60 px-4 py-2.5">
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 rounded-full px-2.5"
            disabled={busy}
            onClick={() => void run("quota", () => onRefreshQuota(account.id))}
          >
            <RefreshCw
              className={cn("size-3.5", pending === "quota" && "animate-spin")}
              aria-hidden="true"
            />
            {quota.known ? "刷新积分" : "查询积分"}
          </Button>

          {/* 本机工具入口（批次 4）：把本卡账号设为该工具的当前登录。
              复用「本机应用接入」卡片里的同一个切换对话框（预选本账号），
              不在这里新造写本机文件的逻辑。 */}
          {writableTargets.map((target) => {
            const Icon = TARGET_ICON[target.id];
            // 同上：任一站点（国内站/国际站）登录了本账号，都算「当前」。
            const current = targetCurrentAccountIds(target).includes(account.id);
            // 同上：国际站写的是 WorkBuddy AI（独立应用/独立文件），名字要跟着站点走。
            const appLabel = localAppTargetLabel(target.id, target.label, account.site);
            const restartHint =
              target.id === "workbuddy-desktop" ? "（会先关闭客户端，写入后再打开）" : "";
            if (current) {
              return (
                <span
                  key={target.id}
                  title={`${appLabel} 当前账号`}
                  className="inline-flex h-7 items-center gap-1 rounded-full border border-primary/25 bg-primary/10 px-2.5 text-xs font-medium text-primary-ink"
                >
                  <Icon className="size-3.5" aria-hidden="true" />
                  <Check className="size-3" aria-hidden="true" />
                  {localAppShortLabel(target.id, account.site)}
                </span>
              );
            }
            return (
              <Button
                key={target.id}
                type="button"
                variant="outline"
                size="sm"
                className="h-7 rounded-full px-2.5"
                disabled={busy || !onSwitchLocalApp}
                title={`设为 ${appLabel} 的当前登录${restartHint}`}
                onClick={() => onSwitchLocalApp?.(target, account.id)}
              >
                <Icon className="size-3.5" aria-hidden="true" />
                {writableTargets.length === 1
                  ? "设为当前"
                  : `设为 ${localAppShortLabel(target.id, account.site)}`}
              </Button>
            );
          })}

          {/* 标签：占位读出站点与套餐，让这一排不只剩下按钮 */}
          <span className="ml-1 inline-flex min-w-0 items-center gap-1 text-xs text-muted-foreground">
            <Tag className="size-3.5 shrink-0" aria-hidden="true" />
            <span className="truncate">
              {siteLabel(account)}
              {quota.known && quota.plan ? ` · ${quota.plan}` : ""}
            </span>
          </span>

          <span className="ml-auto tabular-nums text-xs text-muted-foreground">
            成功 {account.success_count} · 失败 {account.failure_count}
          </span>
        </footer>
      )}

      {/*
        全部积分包弹窗。
        列表口径与卡片一致（只列还有余额的包），并在底部**显式说明**被过滤掉的数量 ——
        否则「共 3 个积分包」点开只看到 2 条时，用户会以为弹窗漏了数据。
        「已用完」的包在卡片口径里本来就不展示（见上面的 allPackages/packages）。
      */}
      <Dialog open={packagesOpen} onOpenChange={setPackagesOpen}>
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-2 pr-6">
              <DialogTitle className="flex items-center gap-2">
                <Package className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
                全部积分包
              </DialogTitle>
              {/*
                逐包排序控件：默认「到期升序 · 近的在前」，可切「面额降序」。
                偏好持久化（localStorage），切换只重排内存里的数据、不重新请求；
                行序同时作用于弹窗明细与卡片上的「近期到期」两行。
              */}
              <select
                aria-label="逐包明细排序"
                title="逐包明细的排序规则"
                value={packageSortMode}
                onChange={(event) => {
                  const value = event.target.value;
                  if (isPackageSortMode(value)) setPackageSortMode(value);
                }}
                className="h-7 shrink-0 cursor-pointer rounded-md border border-border bg-background px-2 text-xs text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
              >
                {PACKAGE_SORT_MODES.map((mode) => (
                  <option key={mode} value={mode}>
                    {PACKAGE_SORT_LABELS[mode]}
                  </option>
                ))}
              </select>
            </div>
            <DialogDescription>
              {name} · 共 {packages.length} 个可用积分包
            </DialogDescription>
          </DialogHeader>

          <ul className="max-h-[52vh] space-y-2 overflow-y-auto pr-1">
            {packages.map((pkg) => (
              <li key={pkg.key} className="rounded-lg border border-border px-3 py-2.5">
                <div className="flex items-baseline justify-between gap-3">
                  <span className="min-w-0 truncate text-sm font-medium" title={pkg.name}>
                    {pkg.name}
                  </span>
                  <span className="shrink-0 font-mono text-sm tabular-nums">
                    {formatCredits(pkg.remaining)}
                  </span>
                </div>
                <div className="mt-1.5 flex items-center justify-between gap-3 text-xs text-muted-foreground">
                  <span className="truncate">
                    {pkg.expireAt > 0
                      ? pkg.expired
                        ? "已到期"
                        : `到期 ${formatExpiryFull(pkg.expireAt)}`
                      : "长期有效"}
                  </span>
                  <span className="shrink-0 tabular-nums">
                    {formatCredits(pkg.remaining)} / {formatCredits(pkg.total)} · 已用{" "}
                    {formatCredits(pkg.used)}
                  </span>
                </div>
                <div className="mt-2 h-1 overflow-hidden rounded-full bg-muted">
                  <div
                    className="h-full rounded-full bg-primary"
                    style={{ width: `${Math.round(pkg.ratio * 100)}%` }}
                  />
                </div>
              </li>
            ))}
          </ul>

          {allPackages.length > packages.length ? (
            <p className="text-xs leading-5 text-muted-foreground">
              另有 {allPackages.length - packages.length} 个积分包已用完（剩余为 0），未列出。
            </p>
          ) : null}

          <DialogFooter className="sm:justify-between">
            {onViewAllPackages ? (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => {
                  setPackagesOpen(false);
                  onViewAllPackages();
                }}
              >
                在积分统计中查看
                <ArrowRight className="size-3.5" aria-hidden="true" />
              </Button>
            ) : (
              <span />
            )}
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => setPackagesOpen(false)}
            >
              关闭
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog
        open={removeOpen}
        onOpenChange={(open) => {
          setRemoveOpen(open);
          if (!open) setDeleteFile(false);
        }}
      >
        <AlertDialogTrigger asChild>
          {/* 触发器由 ··· 菜单驱动，这里放一个不可见的占位以满足 Dialog 的结构要求。 */}
          <span className="hidden" aria-hidden="true" />
        </AlertDialogTrigger>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除账号「{accountDisplayName(account)}」？</AlertDialogTitle>
            <AlertDialogDescription>
              默认把凭据文件归档到账号目录下的 .removed/：它会离开账号池且不再被自动加载，
              但文件仍在磁盘上，随时可以移回原目录恢复。勾选下方选项则彻底删除文件，
              此操作不可撤销。
            </AlertDialogDescription>
          </AlertDialogHeader>

          <label className="flex items-start gap-2.5 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-xs">
            <Checkbox
              checked={deleteFile}
              onCheckedChange={(checked) => setDeleteFile(checked === true)}
              disabled={pending === "remove"}
              aria-label="同时彻底删除凭据文件"
              className="mt-0.5"
            />
            <span className="min-w-0">
              <span className="font-medium text-destructive">同时彻底删除凭据文件</span>
              <span className="mt-0.5 block leading-5 text-muted-foreground">
                将永久删除 <span className="font-mono">{account.file}</span>
                ，此操作不可撤销，需要重新登录才能恢复此账号。
              </span>
            </span>
          </label>

          <AlertDialogFooter>
            <AlertDialogCancel disabled={pending === "remove"}>取消</AlertDialogCancel>
            <AlertDialogAction
              disabled={pending === "remove"}
              onClick={(event) => {
                event.preventDefault();
                void run("remove", async () => {
                  await onRemove(account.id, deleteFile);
                  setRemoveOpen(false);
                });
              }}
            >
              {pending === "remove" ? "删除中…" : deleteFile ? "彻底删除" : "删除（归档）"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Card>
  );
}
