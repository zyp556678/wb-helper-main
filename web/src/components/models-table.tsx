import { FlaskConical, Loader2 } from "lucide-react";

import { CopyIconButton, useCopy } from "@/components/copy-button";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  formatCount,
  formatRelativeTime,
  formatUnixSeconds,
  REASONING_EFFORT_LABEL,
} from "@/lib/format";
import { SITE_LABEL, SITE_PREFIX, pendingProbeSites } from "@/lib/model-view";
import type { ModelSiteInfo, ModelSiteKey, PanelModel } from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * 列宽表。取 BoardUI data-table 的做法：**列宽写死而不是让浏览器按内容分配**。
 *
 * 为什么必须写死：这两列的内容天生不均衡 —— 倍率列大多数时候只有一个 "-"，
 * 而操作列要塞下「探测价格」按钮 + 启用开关。不写死时浏览器会把富余宽度分给
 * 内容少的那两列（于是倍率列大段留白），同时把操作列压到卡片右边缘上。
 *
 * 各列宽度之和（236+84+84+116+116+76+76+152+196 = 1136）与表格的
 * min-w-[1136px] 一致，因此**不会因为写死列宽而多出一次横向滚动**。
 * 倍率列从 96 加到 116、模型列从 220 加到 236，是为了给新增的复制按钮腾位子 ——
 * 按钮不挤进单元格时表格不会重排，但压到临界宽度会开始截断倍率文案。
 */
const COL = {
  model: "w-[236px]",
  number: "w-[84px]",
  multiplier: "w-[116px]",
  count: "w-[76px]",
  reasoning: "w-[152px]",
  actions: "w-[196px]",
} as const;

/** 倍率文案按 verdict 上色：免费走品牌绿，收费走琥珀，未知走灰。 */
function multiplierTone(verdict: string): string {
  if (verdict === "free") return "text-primary-ink";
  if (verdict === "paid") return "text-amber-700 dark:text-amber-300";
  return "text-muted-foreground";
}

/** 倍率单元格：直接展示后端算好的 multiplier_label，悬停给出探测详情。 */
function MultiplierCell({
  site,
  info,
  now,
}: {
  site: ModelSiteKey;
  info: ModelSiteInfo | undefined;
  now: number;
}) {
  const label = info?.multiplier_label || "-";
  const probe = info?.probe ?? null;
  const probedAt = probe ? formatUnixSeconds(probe.last_probe_at) : null;

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-flex cursor-default items-center gap-1.5">
          <span className={cn("tabular-nums", multiplierTone(info?.verdict ?? ""))}>{label}</span>
          {info?.promo_label ? <Badge variant="warning">{info.promo_label}</Badge> : null}
        </span>
      </TooltipTrigger>
      <TooltipContent side="top" className="max-w-[320px] whitespace-pre-wrap break-words">
        <div className="font-medium">
          {SITE_LABEL[site]}：{label}
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
      </TooltipContent>
    </Tooltip>
  );
}

/** 推理档位：默认档用实心 badge 高亮，其余为描边。
 *
 * 上游通常只下发「默认档」（reasoning.effort），可选档位列表（supportedEfforts）
 * 要特定客户端指纹才会返回。因此这里分两种呈现：
 *   - 有档位列表 → 全部列出，默认档高亮
 *   - 只有默认档 → 显示单个「默认档」badge + 一行等宽说明，未下发可选档位的原因走 tooltip
 * 完全不支持推理的模型才显示 "-"。
 *
 * 「默认档」那行刻意**横排**而不是竖排：竖排会让这一格比其他格高一行，
 * 全表行高随之参差（36 行的表里对齐一断就很难扫）。行高统一交给「模型」列
 * （id + 名称两行）决定，各列都只占一行。
 */
function ReasoningCell({ model }: { model: PanelModel }) {
  const efforts = model.reasoning_supported_efforts ?? [];
  const fallback = model.reasoning_default_effort || "";

  if (efforts.length > 0) {
    return (
      <span className="inline-flex flex-wrap items-center gap-1">
        {efforts.map((effort) => {
          const isDefault = effort === fallback;
          return (
            <Badge
              key={effort}
              variant={isDefault ? "default" : "outline"}
              className={cn(!isDefault && "text-muted-foreground")}
            >
              {REASONING_EFFORT_LABEL[effort] ?? effort}
            </Badge>
          );
        })}
      </span>
    );
  }

  if (fallback) {
    return (
      <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
        <Badge variant="default">{REASONING_EFFORT_LABEL[fallback] ?? fallback}</Badge>
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="cursor-default text-xs text-muted-foreground">默认档</span>
          </TooltipTrigger>
          <TooltipContent side="top" className="max-w-[300px]">
            上游只下发了默认档（reasoning.effort）。可选档位列表要特定客户端指纹才会返回，
            因此这里列不出全部档位，不代表该模型只支持这一档。
          </TooltipContent>
        </Tooltip>
      </span>
    );
  }

  return <span className="text-muted-foreground">-</span>;
}

export interface ModelsTableProps {
  models: PanelModel[];
  /** 权威黑名单 id 集合，作为开关的选中态（写入成功后立即同步）。 */
  blockedIds: Set<string>;
  /** 可探测的站点（目录里报了有账号的站点）；没账号的站点探不了，不该出现在待探测里。 */
  probeable: ModelSiteKey[];
  /** 每秒刷新的当前 Unix 秒，用于探测时间的相对展示。 */
  now: number;
  /** 正在探测价格的模型 id。 */
  probingId: string | null;
  /** 正在保存黑白名单的模型 id。 */
  blockingId: string | null;
  /**
   * 当前筛选到的那一个站点；未筛选或同时选了多个站点时传 null。
   * 传了就在模型名前加该站前缀（`CN-` / `AI-`），与卡片视图保持一致。
   */
  siteFilter?: ModelSiteKey | null;
  onProbe: (model: PanelModel) => void;
  onToggleBlocked: (model: PanelModel, blocked: boolean) => void;
}

/** 模型目录表：每行一个模型，含各站点倍率、可用账号与黑白名单开关。 */
export function ModelsTable({
  models,
  blockedIds,
  probeable,
  now,
  probingId,
  blockingId,
  siteFilter = null,
  onProbe,
  onToggleBlocked,
}: ModelsTableProps) {
  // 复制状态提到表级而不是每行一个 hook：`.map()` 里不能调 hook，而为一行
  // 单独抽组件会把这张表的列宽/行高约定拆散。用「哪一行+哪一种」做 key 即可。
  const { copiedKey, copy } = useCopy();

  return (
    <Table className="min-w-[1136px]">
      <TableHeader>
        <TableRow>
          <TableHead className={COL.model}>模型</TableHead>
          <TableHead className={cn(COL.number, "text-right")}>上下文</TableHead>
          <TableHead className={cn(COL.number, "text-right")}>最大输出</TableHead>
          <TableHead className={COL.multiplier}>国内站倍率</TableHead>
          <TableHead className={COL.multiplier}>国际站倍率</TableHead>
          <TableHead className={cn(COL.count, "text-right")}>可用账号</TableHead>
          <TableHead className={cn(COL.count, "text-right")}>请求数</TableHead>
          <TableHead className={COL.reasoning}>档位</TableHead>
          <TableHead className={cn(COL.actions, "text-right")}>操作</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {models.map((model) => {
          const pendingSites = pendingProbeSites(model, probeable);
          const probing = probingId === model.id;
          // 筛到单站时名字带前缀，复制出去的也就是可直接用的「锁定写法」。
          const displayName = siteFilter ? `${SITE_PREFIX[siteFilter]}${model.id}` : model.id;
          const displayNameLabel = siteFilter ? `${SITE_LABEL[siteFilter]}的模型名` : "模型名";
          // 开关选中态看权威黑名单，行样式沿用后端给的 blocked。
          const blocklisted = blockedIds.has(model.id);
          const disabled = model.blocked || blocklisted;
          // 黑白名单是整体覆盖写入，保存期间禁用所有开关避免并发互相覆盖。
          const blockBusy = blockingId !== null;

          return (
            // 禁用行用「淡色底」而不是 opacity：整行调透明度会把文字一起拉淡，
            // 而可用账号数、请求数这些恰恰是判断要不要重新启用它的依据，
            // 淡到看不清就失去了对照意义。状态已由「已禁用」badge 明确表达。
            <TableRow key={model.id} className={cn(disabled && "bg-muted/40")}>
              <TableCell>
                {/* max-w 必须给具体值而不是 max-w-full：表格是 auto 布局，
                    `truncate`（nowrap）会让 min-content 等于整串 id 的宽度，
                    不设上限时这一列会被最长的那个 id 撑开，列宽锚定就失效了。 */}
                <div className="flex max-w-[188px] flex-col">
                  <div className="flex items-center gap-1.5">
                    <span className="truncate font-mono text-foreground">{displayName}</span>
                    <CopyIconButton
                      label={displayNameLabel}
                      copied={copiedKey === `model:${model.id}`}
                      onCopy={() =>
                        void copy(displayName, `model:${model.id}`, displayNameLabel)
                      }
                      className="size-5 shrink-0 p-0 text-muted-foreground"
                    />
                    {disabled ? <Badge variant="danger">已禁用</Badge> : null}
                  </div>
                  <span className="truncate text-xs text-muted-foreground">
                    {model.name || "未命名模型"}
                  </span>
                </div>
              </TableCell>
              <TableCell className="text-right tabular-nums text-foreground">
                {formatCount(model.context_length)}
              </TableCell>
              <TableCell className="text-right tabular-nums text-foreground">
                {formatCount(model.max_output_tokens)}
              </TableCell>
              <TableCell>
                <div className="flex items-center gap-1">
                  <MultiplierCell site="cn" info={model.sites?.cn} now={now} />
                  <CopyIconButton
                    label="锁定国内站的模型名"
                    copied={copiedKey === `cn:${model.id}`}
                    onCopy={() =>
                      void copy(`${SITE_PREFIX.cn}${model.id}`, `cn:${model.id}`, "国内站的模型名")
                    }
                    className="size-5 shrink-0 p-0 text-muted-foreground"
                  />
                </div>
              </TableCell>
              <TableCell>
                <div className="flex items-center gap-1">
                  <MultiplierCell site="intl" info={model.sites?.intl} now={now} />
                  <CopyIconButton
                    label="锁定国际站的模型名"
                    copied={copiedKey === `intl:${model.id}`}
                    onCopy={() =>
                      void copy(`${SITE_PREFIX.intl}${model.id}`, `intl:${model.id}`, "国际站的模型名")
                    }
                    className="size-5 shrink-0 p-0 text-muted-foreground"
                  />
                </div>
              </TableCell>
              <TableCell
                className={cn(
                  "text-right tabular-nums",
                  model.available_accounts === 0
                    ? "font-medium text-amber-700 dark:text-amber-300"
                    : "text-foreground",
                )}
              >
                {model.available_accounts}
              </TableCell>
              <TableCell className="text-right tabular-nums text-foreground">
                {model.requests}
              </TableCell>
              <TableCell>
                <ReasoningCell model={model} />
              </TableCell>
              <TableCell>
                <div className="flex items-center justify-end gap-2">
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <span className="inline-flex">
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          disabled={pendingSites.length === 0 || probing}
                          onClick={() => onProbe(model)}
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
                    <TooltipContent side="top" className="max-w-[300px]">
                      {pendingSites.length > 0
                        ? `对${pendingSites.map((site) => SITE_LABEL[site]).join("、")}各发一次极短请求，会真实消耗账号额度`
                        : "没有可探测的站点：要么价格已全部确认，要么该站点没有可用账号"}
                    </TooltipContent>
                  </Tooltip>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <span className="inline-flex items-center gap-1.5">
                        <span className="text-xs text-muted-foreground">启用</span>
                        <Switch
                          checked={!blocklisted}
                          disabled={blockBusy}
                          aria-label={`启用模型 ${model.id}`}
                          onCheckedChange={(checked) => onToggleBlocked(model, !checked)}
                        />
                      </span>
                    </TooltipTrigger>
                    <TooltipContent side="top" className="max-w-[300px]">
                      {blocklisted
                        ? "该模型当前在黑名单中（已禁用）。打开开关会把它移出黑名单"
                        : "关闭开关会把该模型加入黑名单（禁用），网关不再调度它"}
                    </TooltipContent>
                  </Tooltip>
                </div>
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
