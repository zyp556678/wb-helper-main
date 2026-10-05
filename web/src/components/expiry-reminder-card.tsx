import { CalendarDays, Loader2, RefreshCw } from "lucide-react";
import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { creditResourceName } from "@/lib/credit-package-names";
import {
  accountDisplayName,
  formatCredits,
  maskEmail,
  toCreditPackageViews,
  type CreditPackageView,
} from "@/lib/format";
import type { Account, PanelCreditAccount } from "@/lib/types";
import { cn } from "@/lib/utils";

/* ────────────────────────────────────────────────────────────────────────
 * 积分到期提醒（账号页顶部卡片）—— 移植自参考实现 wb2api-panel 29e2749
 * 「积分到期提醒卡片」。
 *
 * 积分不是永久的：只看「剩余积分 ÷ 日消耗」会系统性偏乐观 —— 用不完的部分
 * 到期直接蒸发。这里把「最近要过期的是哪批、有多少、到期前每天至少要消耗
 * 多少」顶到页面顶部。数据源就是账号页已经加载的 credits（与「逐包明细」
 * 同一份 `GET /panel/api/credits` 响应），**不新增任何网络请求**。
 * ──────────────────────────────────────────────────────────────────────── */

/** 本地时区的今天零点（毫秒）。所有日期口径都以它为基准，避免 UTC 解析偏移。 */
export function startOfLocalDay(now: Date = new Date()): number {
  return new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
}

function pad2(value: number): string {
  return String(value).padStart(2, "0");
}

export interface ExpiryBatch {
  /** 本地日期 `YYYY-MM-DD`（展示用）。 */
  date: string;
  /** 该日期本地零点的毫秒时间戳（天数计算与排序用）。 */
  dayStart: number;
  /** 当天作废的剩余积分合计。 */
  remaining: number;
}

/**
 * 某账号的包 →「到期日 → 该日作废积分」升序列表。
 *
 * 只统计「剩余 > 0 且带到期时间」的包：长期有效（expireAt=0）不会作废，
 * 已用完（剩余=0）的包到期也没有任何东西会消失。已过期的批次不在这里剔除
 *（调用方按 `days >= 0` 过滤），这样本函数只关心聚合、不掺进「今天」的概念。
 */
export function aggregateExpiryBatches(packages: readonly CreditPackageView[]): ExpiryBatch[] {
  const byDay = new Map<number, ExpiryBatch>();
  for (const pkg of packages) {
    if (!(pkg.remaining > 0) || !(pkg.expireAt > 0)) continue;
    const at = new Date(pkg.expireAt);
    // 按**本地日期**归组：与「今天零点」同一时区基准，不用 UTC 日期。
    const dayStart = new Date(at.getFullYear(), at.getMonth(), at.getDate()).getTime();
    const existing = byDay.get(dayStart);
    if (existing) {
      existing.remaining += pkg.remaining;
    } else {
      byDay.set(dayStart, {
        date: `${at.getFullYear()}-${pad2(at.getMonth() + 1)}-${pad2(at.getDate())}`,
        dayStart,
        remaining: pkg.remaining,
      });
    }
  }
  return [...byDay.values()].sort((a, b) => a.dayStart - b.dayStart);
}

/** 距今天数：今天 0、明天 1、已经过去为负。基准是本地零点。 */
export function expiryDaysLeft(batchDayStart: number, todayStart: number): number {
  return Math.round((batchDayStart - todayStart) / 86_400_000);
}

/** 色点：≤3 天红（不抓紧就真没了）、≤7 天琥珀、更远绿；灰 = 查询失败。 */
const DOT_CLASS = {
  danger: "bg-destructive",
  warning: "bg-amber-500",
  ok: "bg-primary",
  muted: "bg-muted-foreground/40",
} as const;

/** 每账号一行：色点 + 账号名 + 正文（与参考实现 .exp-row 同结构）。 */
function ExpiryRow({ dot, name, children }: { dot: string; name: string; children: ReactNode }) {
  return (
    <div className="flex items-start gap-2.5 py-2 text-xs">
      <span className={cn("mt-[5px] size-2.5 shrink-0 rounded-full", dot)} aria-hidden="true" />
      <span
        className="min-w-[9em] max-w-[15em] shrink-0 truncate font-semibold text-foreground"
        title={name}
      >
        {name}
      </span>
      <span className="min-w-0 flex-1 leading-5 text-muted-foreground">{children}</span>
    </div>
  );
}

/** 正文里的关键数字：等宽 + 等宽数字（与页面其它明细同一写法）。 */
function Num({ children }: { children: ReactNode }) {
  return <b className="font-mono font-semibold tabular-nums text-foreground">{children}</b>;
}

export interface ExpiryReminderCardProps {
  /** 账号列表（页面展示顺序；全量账号，不跟随档位筛选，与统计条同口径）。 */
  accounts: Account[];
  /** 账号页已加载的逐包明细，与「逐包明细」共用同一份数据。 */
  credits: Map<string, PanelCreditAccount>;
  /** 明细接口是否已返回过（区分「加载中」与「确实查不到」）。 */
  creditsLoaded: boolean;
  /** 整轮明细拉取失败的原因（单账号失败在行内展示）。 */
  creditsError?: string | null;
  /** 「检查」进行中；按钮与数据新鲜度提示同步展示加载态。 */
  checking: boolean;
  /** 检查：复用账号页「刷新积分」同一条路径（refreshAllQuota + 重拉明细）。 */
  onCheck: () => void;
}

export function ExpiryReminderCard({
  accounts,
  credits,
  creditsLoaded,
  creditsError = null,
  checking,
  onCheck,
}: ExpiryReminderCardProps) {
  // 每秒（页面 now 时钟）都会重算一次，跨过本地零点后日期口径自动跟上。
  const todayStart = startOfLocalDay();
  const note =
    checking || !creditsLoaded
      ? "查询中…"
      : creditsError
        ? `查询失败：${creditsError}`
        : `${accounts.length} 个账号 · 实时查询上游`;

  return (
    <Card className="mb-5 gap-0 overflow-hidden rounded-2xl py-0 shadow-none">
      <header className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2 border-b border-border/60 px-5 py-4">
        <div className="min-w-0">
          <h2 className="flex items-center gap-1.5 text-sm font-semibold text-foreground">
            <CalendarDays className="size-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />
            积分到期提醒
          </h2>
          <p className="mt-1 text-xs leading-5 text-muted-foreground">
            按最近到期批次估算 · 日均需耗 = 批次剩余 ÷ 距到期天数 ·
            上游按失效时刻先后自动优先扣减（FEFO）
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <span className="text-xs tabular-nums text-muted-foreground">{note}</span>
          <Button type="button" variant="outline" size="sm" disabled={checking} onClick={onCheck}>
            {checking ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <RefreshCw className="size-3.5" aria-hidden="true" />
            )}
            检查
          </Button>
        </div>
      </header>
      {/* 行多时给列表一个最大高度 + 滚动，避免大量账号把页面撑破。 */}
      <div className="max-h-[340px] divide-y divide-border/60 overflow-y-auto px-5 py-1">
        {!creditsLoaded ? (
          <p className="py-6 text-center text-xs text-muted-foreground">
            查询中…（逐账号向上游实时查询）
          </p>
        ) : (
          accounts.map((account) => (
            <ExpiryReminderRow
              key={account.id || account.file || account.uid}
              account={account}
              entry={credits.get(account.id)}
              todayStart={todayStart}
            />
          ))
        )}
      </div>
    </Card>
  );
}

/** 单账号的提醒行；拆出来让「查询失败 / 无到期 / 有批次」三条分支一目了然。 */
function ExpiryReminderRow({
  account,
  entry,
  todayStart,
}: {
  account: Account;
  entry: PanelCreditAccount | undefined;
  todayStart: number;
}) {
  // 账号名与账号卡片同口径（display_field + 邮箱遮罩），两处对得上号。
  const name = maskEmail(accountDisplayName(account));

  if (!entry || entry.ok === false) {
    const reason = entry ? entry.error?.trim() || "上游未返回明细" : "未返回该账号的积分明细";
    return (
      <ExpiryRow dot={DOT_CLASS.muted} name={name}>
        <span className="text-destructive">查询失败：{reason}</span>
      </ExpiryRow>
    );
  }

  const packages = toCreditPackageViews(entry.resources, creditResourceName);
  const batches = aggregateExpiryBatches(packages).filter(
    (batch) => expiryDaysLeft(batch.dayStart, todayStart) >= 0,
  );
  const first = batches[0];
  if (!first) {
    return (
      <ExpiryRow dot={DOT_CLASS.ok} name={name}>
        7 天内无到期积分
      </ExpiryRow>
    );
  }

  const days = expiryDaysLeft(first.dayStart, todayStart);
  // 天数在 UI 上最小是 1（今天到期也按「还有 1 天可耗」算），与参考实现一致。
  const daily = Math.ceil(first.remaining / Math.max(1, days));
  const weekTotal = batches
    .filter((batch) => expiryDaysLeft(batch.dayStart, todayStart) <= 7)
    .reduce((sum, batch) => sum + batch.remaining, 0);
  const dot = days <= 3 ? DOT_CLASS.danger : days <= 7 ? DOT_CLASS.warning : DOT_CLASS.ok;
  const dayWord = days === 0 ? "今天到期" : days === 1 ? "明天到期" : `${days} 天后到期`;
  const laterBatches = batches.slice(1, 4);
  const laterText =
    laterBatches
      .map((batch) => `随后 ${batch.date.slice(5)} · ${formatCredits(batch.remaining)}`)
      .join("　") + (batches.length > 4 ? `　等 ${batches.length} 批` : "");

  return (
    <ExpiryRow dot={dot} name={name}>
      最近到期 <Num>{first.date}</Num>（{dayWord}）· 该批{" "}
      <Num>{formatCredits(first.remaining)}</Num> 积分 · 到期前日均需耗 ≥
      <Num>{formatCredits(daily)}</Num>
      {weekTotal > first.remaining ? (
        <>
          {" "}
          · 7 天内合计 <Num>{formatCredits(weekTotal)}</Num>
        </>
      ) : null}
      {laterText ? <div className="mt-0.5 text-muted-foreground/80">{laterText}</div> : null}
    </ExpiryRow>
  );
}
