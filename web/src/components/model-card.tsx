import { Brain, FlaskConical, Image as ImageIcon, Loader2, Wrench } from "lucide-react";

import { CopyIconButton, useCopy } from "@/components/copy-button";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  formatMillis,
  formatRelativeTime,
  formatTokenCompact,
  formatUnixSeconds,
  REASONING_EFFORT_LABEL,
} from "@/lib/format";
import { SITE_KEYS, SITE_LABEL, SITE_PREFIX, effortList, type ModelView } from "@/lib/model-view";
import type { ModelSiteInfo, ModelSiteKey } from "@/lib/types";
import { cn } from "@/lib/utils";

/** 站点强调色：只用于卡片左上角那一个小方块，靠颜色区分「这是哪个站的价」。 */
const SITE_ACCENT: Record<ModelSiteKey, string> = {
  cn: "var(--data-series-emerald)",
  intl: "var(--data-series-indigo)",
};

/** 倍率文案按 verdict 上色：免费走品牌绿，收费走琥珀，未知走灰。 */
function multiplierTone(verdict: string): string {
  if (verdict === "free") return "text-primary-ink";
  if (verdict === "paid") return "text-amber-700 dark:text-amber-300";
  return "text-muted-foreground";
}

/** 价格确认状态的一句话口径；免费判定优先于收费。 */
function verdictText(info: ModelSiteInfo | undefined): string {
  if (!info) return "该站点无此模型";
  if (!info.known) return "未确认";
  if (info.verdict === "free") return "实测免费";
  if (info.verdict === "paid") return "实测收费";
  return "已确认";
}

/**
 * 站点价格行：站点标签 + 倍率 + 口径 + 右侧探测时间。悬停展开探测详情。
 *
 * `pinnedName` 是该站点的**锁定写法**（`CN-<模型 id>`），点右侧小图标即可复制。
 * 之所以把「锁定到本站」的复制入口放在这一行而不是模型名旁边：用户想锁定站点
 * 的时刻，正是眼睛落在这行倍率上、判断「这一侧便宜/免费」的时刻。
 * 放在名字旁边就得先想起「哦还有站点前缀这回事」，再回到站点行确认该用哪个前缀。
 */
function SitePriceRow({
  site,
  info,
  now,
  pinnedName,
  copied,
  onCopy,
}: {
  site: ModelSiteKey;
  info: ModelSiteInfo | undefined;
  now: number;
  /** 形如 `CN-deepseek-v4.1-flash`；由宿主编好传进来。 */
  pinnedName: string;
  copied: boolean;
  onCopy: () => void;
}) {
  const label = info?.multiplier_label || "-";
  const probe = info?.probe ?? null;
  const probedAt = probe ? formatUnixSeconds(probe.last_probe_at) : null;

  return (
    <div className="flex items-center gap-2 text-xs">
      <Tooltip>
        <TooltipTrigger asChild>
          <div className="flex min-w-0 flex-1 cursor-default items-center gap-2">
            <span
              className="inline-flex shrink-0 items-center gap-1.5 rounded-md bg-muted px-1.5 py-0.5 font-medium text-muted-foreground"
              aria-hidden="true"
            >
              <span
                className="size-1.5 shrink-0 rounded-[2px]"
                style={{ backgroundColor: SITE_ACCENT[site] }}
              />
              {SITE_LABEL[site]}
            </span>
            <span className={cn("shrink-0 tabular-nums font-medium", multiplierTone(info?.verdict ?? ""))}>
              {label}
            </span>
            <span className="min-w-0 truncate text-muted-foreground">{verdictText(info)}</span>
            {info?.promo_label ? (
              <Badge variant="warning" className="shrink-0">
                {info.promo_label}
              </Badge>
            ) : null}
          </div>
        </TooltipTrigger>
        <TooltipContent side="top" className="max-w-[320px] whitespace-pre-wrap break-words">
          <div className="font-medium">
            {SITE_LABEL[site]}：{label}
            {info?.credits ? `（上游原文 ${info.credits}）` : ""}
          </div>
          {probe ? (
            <>
              <div className="mt-1">{probe.detail || "后端未给出探测说明"}</div>
              <div className="mt-1 text-popover-foreground/70">
                探测于 {formatRelativeTime(probe.last_probe_at, now)}
                {probedAt ? `（${probedAt}）` : ""}
              </div>
            </>
          ) : (
            <div className="mt-1 text-popover-foreground/70">尚未探测该站点的价格</div>
          )}
          {info?.promo_label ? (
            <div className="mt-1 text-popover-foreground/70">
              促销：{info.promo_label}
              {info.promo_until ? ` · ${info.promo_until}` : ""}
            </div>
          ) : null}
          <div className="mt-1.5 border-t border-popover-foreground/15 pt-1.5 font-mono text-popover-foreground/70">
            {pinnedName}
          </div>
        </TooltipContent>
      </Tooltip>
      {probe ? (
        <span className="shrink-0 text-muted-foreground">
          实测 {formatRelativeTime(probe.last_probe_at, now)}
        </span>
      ) : null}
      <CopyIconButton
        label={`锁定${SITE_LABEL[site]}的模型名`}
        copied={copied}
        onCopy={onCopy}
        className="-mr-1.5 size-5 shrink-0 p-0 text-muted-foreground"
      />
    </div>
  );
}

export interface ModelCardProps {
  view: ModelView;
  /** 每秒刷新的当前 Unix 秒，用于探测时间的相对展示。 */
  now: number;
  /** 该模型在可探测站点里还没确认价格的站点（空数组 = 没有可探测的目标）。 */
  pendingSites: ModelSiteKey[];
  probing: boolean;
  /** 黑白名单是整体覆盖写入，保存期间禁用所有开关避免并发互相覆盖。 */
  blockBusy: boolean;
  /**
   * 当前筛选到的那一个站点；未筛选或同时选了多个站点时传 null。
   *
   * 传了就在模型名前面加上该站的前缀（`CN-` / `AI-`）。理由：筛选到单站时，
   * 用户看到的每一张卡都只关心那一个站，「这个模型该怎么写才能走这一站」
   * 是此刻最直接的问题 —— 前缀本来只藏在站点行的 tooltip 里，得先知道有这回事
   * 才会去悬停。
   *
   * 未筛选时**不加**：一张卡同时列了两个站的价格，标题带哪个前缀都是错的。
   */
  siteFilter?: ModelSiteKey | null;
  onProbe: () => void;
  onToggleBlocked: (blocked: boolean) => void;
}

/**
 * 模型卡片。
 *
 * 版式参考 sudocode 的模型广场卡片：**顶部一行是身份**（图标 + 名称 + 徽章 + 操作），
 * 中间是**价格行**（标签胶囊 + 数值 + 口径），底部一条**元信息带**。
 * 与参考页的差别在于内容 —— 这里放的是本项目的双站倍率、实测结论与推理档位，
 * 而不是单一的人民币单价。
 */
export function ModelCard({
  view,
  now,
  pendingSites,
  probing,
  blockBusy,
  siteFilter = null,
  onProbe,
  onToggleBlocked,
}: ModelCardProps) {
  const { model, blocked, badges, reasoning } = view;
  const { efforts, fallback } = effortList(model);
  const description = model.description || model.name || "上游未提供描述";
  const hasConfirmed = SITE_KEYS.some((site) => Boolean(model.sites?.[site]?.known));
  /** 没有可探测的目标：要么两个可探测站点都已确认，要么根本没有可用账号的站点。 */
  const noProbeTarget = pendingSites.length === 0;
  const { copiedKey, copy } = useCopy();

  // 筛选到单站时标题带前缀，复制出去的也就是可直接用的「锁定写法」。
  const displayName = siteFilter ? `${SITE_PREFIX[siteFilter]}${model.id}` : model.id;
  const displayNameLabel = siteFilter ? `锁定${SITE_LABEL[siteFilter]}的模型名` : "模型名";

  return (
    <article
      className={cn(
        // 禁用态用淡底而不是整卡 opacity：账号数、请求数这些恰恰是判断要不要
        // 重新启用它的依据，淡到看不清就失去了对照意义。
        "flex min-w-0 flex-col rounded-xl border bg-card transition-colors",
        blocked && "bg-muted/40",
      )}
      aria-label={`模型 ${displayName}`}
    >
      <div className="flex min-w-0 items-start gap-3 p-4 pb-3">
        <span
          className="mt-1 size-2.5 shrink-0 rounded-[3px]"
          style={{ backgroundColor: SITE_ACCENT[view.confirmedSites[0] ?? "cn"] }}
          aria-hidden="true"
        />
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 flex-wrap items-center gap-1.5">
            <span className="truncate font-mono text-[13px] font-medium">{displayName}</span>
            {/* 复制的是**标题上那个写法**：筛到单站时带前缀（可直接粘到调用里锁定该站），
                未筛选时是裸模型名（网关按自己的规则挑站点，默认倾向免费的一侧）。
                锁定具体站点还可以用下面每一行的图标，两者刻意分开 ——
                合成一个「复制」按钮会让用户分不清粘出来的是哪一种。 */}
            <CopyIconButton
              label={displayNameLabel}
              copied={copiedKey === "model"}
              onCopy={() => void copy(displayName, "model", displayNameLabel)}
              className="size-5 shrink-0 p-0 text-muted-foreground"
            />
            {model.is_default ? <Badge variant="default">默认模型</Badge> : null}
            {badges.map((badge) => (
              // 上游给的颜色只用来做**色点与淡底**，文字一律走主题前景色。
              //
              // 为什么不直接把 badge.color 当文字色：上游给的是运营色（红 #FF0000、
              // 蓝 #1E90FF），直接当文字在白底上只有 2.9–3.4:1，连 AA 都过不了
              // （实测 a11y 审计报了 3 处）。颜色信号由色点承载就够了，
              // 文字必须保证可读 —— 而运营改色时前端也不该跟着发版。
              <span
                key={badge.label}
                className="inline-flex shrink-0 items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs font-medium text-foreground"
                style={{ backgroundColor: `${badge.color}1f`, borderColor: `${badge.color}59` }}
                title={`上游运营标签：${badge.label}`}
              >
                <span
                  className="size-1.5 shrink-0 rounded-[2px]"
                  style={{ backgroundColor: badge.color }}
                  aria-hidden="true"
                />
                {badge.label}
              </span>
            ))}
            {blocked ? <Badge variant="danger">已禁用</Badge> : null}
          </div>
          <p className="mt-1 line-clamp-2 text-xs leading-5 text-muted-foreground">{description}</p>
        </div>
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="inline-flex shrink-0 items-center gap-1.5">
              <span className="text-xs text-muted-foreground">启用</span>
              <Switch
                checked={!blocked}
                disabled={blockBusy}
                aria-label={`启用模型 ${model.id}`}
                onCheckedChange={(checked) => onToggleBlocked(!checked)}
              />
            </span>
          </TooltipTrigger>
          <TooltipContent side="top" className="max-w-[300px]">
            {blocked
              ? "该模型当前在黑名单中（已禁用）。打开开关会把它移出黑名单"
              : "关闭开关会把该模型加入黑名单（禁用），网关不再调度它"}
          </TooltipContent>
        </Tooltip>
      </div>

      <div className="space-y-1.5 px-4 pb-3">
        {SITE_KEYS.map((site) => {
          const pinnedName = `${SITE_PREFIX[site]}${model.id}`;
          return (
            <SitePriceRow
              key={site}
              site={site}
              info={model.sites?.[site]}
              now={now}
              pinnedName={pinnedName}
              copied={copiedKey === pinnedName}
              onCopy={() => void copy(pinnedName, pinnedName, `${SITE_LABEL[site]}的模型名`)}
            />
          );
        })}
        {!hasConfirmed && pendingSites.length > 0 ? (
          <p className="text-xs text-muted-foreground">
            还没有确认价格的站点，可点右下角「探测价格」实测一次（会消耗账号额度）。
          </p>
        ) : null}
        {!hasConfirmed && pendingSites.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            还没有确认价格的站点；当前没有可用于探测的账号，先到「账号」页添加。
          </p>
        ) : null}
      </div>

      <div className="mt-auto flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-1 border-t px-4 py-2.5 text-xs text-muted-foreground">
        <span className="tabular-nums">上下文 {formatTokenCompact(model.context_length)}</span>
        <span className="tabular-nums">输出 {formatTokenCompact(model.max_output_tokens)}</span>
        <span className="tabular-nums">账号 {model.available_accounts}</span>
        {/* 运行指标：总量之外还要看「行不行」—— 失败计数与最近状态才是那个答案，
            平均首字则回答「慢不慢」。三者都没有时只显示请求数，不摆一堆 0。 */}
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="cursor-default tabular-nums">
              请求 {model.requests}
              {model.failed ? <span className="text-destructive">（失败 {model.failed}）</span> : null}
            </span>
          </TooltipTrigger>
          <TooltipContent side="top">
            <div className="space-y-0.5 text-xs">
              <div>请求 {model.requests} · 成功 {model.success ?? 0} · 失败 {model.failed ?? 0}</div>
              <div>累计 token {formatTokenCompact(model.tokens ?? 0)}（上游 usage）</div>
              <div>
                平均首字 {typeof model.avg_ttft_ms === "number" ? `${formatMillis(model.avg_ttft_ms)} ms` : "无样本"}
                {" · "}
                平均耗时 {typeof model.avg_total_ms === "number" ? `${formatMillis(model.avg_total_ms)} ms` : "无样本"}
              </div>
              <div>最近状态 {model.last_status || "本进程还没跑过"}</div>
            </div>
          </TooltipContent>
        </Tooltip>
        <span className="inline-flex items-center gap-1" title="推理档位">
          <Brain className="size-3.5" aria-hidden="true" />
          {reasoningLabel(reasoning, efforts, fallback)}
        </span>
        {model.supports_tool_call ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex cursor-default items-center" aria-label="支持工具调用">
                <Wrench className="size-3.5" aria-hidden="true" />
              </span>
            </TooltipTrigger>
            <TooltipContent side="top">支持工具调用（function calling）</TooltipContent>
          </Tooltip>
        ) : null}
        {model.supports_images ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex cursor-default items-center" aria-label="支持图片输入">
                <ImageIcon className="size-3.5" aria-hidden="true" />
              </span>
            </TooltipTrigger>
            <TooltipContent side="top">接受图片作为输入</TooltipContent>
          </Tooltip>
        ) : null}
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="ml-auto inline-flex">
              <Button
                type="button"
                variant="ghost"
                size="sm"
                disabled={noProbeTarget || probing}
                onClick={onProbe}
              >
                {probing ? (
                  <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                ) : (
                  <FlaskConical className="size-3.5" aria-hidden="true" />
                )}
                探测价格
              </Button>
            </span>
          </TooltipTrigger>
          <TooltipContent side="top" className="max-w-[320px]">
            {noProbeTarget
              ? "没有可探测的站点：要么价格已全部确认，要么该站点没有可用账号"
              : `对${pendingSites.map((site) => SITE_LABEL[site]).join("、")}各发一次极短请求，会真实消耗账号额度`}
          </TooltipContent>
        </Tooltip>
      </div>
    </article>
  );
}

/** 档位文案：有档位列表就列全，只有默认档就标「默认档」，完全不支持才写「—」。 */
function reasoningLabel(reasoning: ModelView["reasoning"], efforts: string[], fallback: string): string {
  if (reasoning === "none") return "—";
  if (efforts.length > 0) {
    return efforts.map((effort) => REASONING_EFFORT_LABEL[effort] ?? effort).join("/");
  }
  if (fallback) return `${REASONING_EFFORT_LABEL[fallback] ?? fallback}（默认档）`;
  return "默认档";
}
