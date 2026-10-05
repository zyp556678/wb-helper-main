import { Check } from "lucide-react";
import type { ReactNode } from "react";

import {
  CAPABILITY_OPTIONS,
  PRICE_TIERS,
  REASONING_OPTIONS,
  SITE_KEYS,
  SITE_LABEL,
  STATE_OPTIONS,
  activeFilterCount,
  type CapabilityKey,
  type ModelFilters,
  type ModelView,
  type PriceTierKey,
  type ReasoningKey,
  type StateKey,
  toggleValue,
} from "@/lib/model-view";
import type { ModelSiteKey } from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * 左侧筛选栏。
 *
 * 版式参考 sudocode 模型广场的筛选栏：**分组 + 组内计数 + 每组独立「重置」**，
 * 顶部一个「清除全部」。计数取**全量模型**而不是当前筛选结果 —— 那样会随每次勾选
 * 自我收缩，勾错一项后剩下的选项计数全变 0，反而看不出还有什么可选。
 */

interface OptionRowProps {
  label: ReactNode;
  count: number;
  checked: boolean;
  onToggle: () => void;
}

/** 单行选项：自绘方块 + 标签 + 右侧计数。
 *
 * 不用项目里那个共享 Checkbox：它的选中态是 `bg-destructive`（红），
 * 那是给账号页的多选删除场景用的；筛选器用红色填充会被读成危险操作。
 */
function OptionRow({ label, count, checked, onToggle }: OptionRowProps) {
  return (
    <label className="flex cursor-pointer items-center gap-2 rounded-md px-1.5 py-1 text-xs transition-colors hover:bg-muted/60">
      <input
        type="checkbox"
        checked={checked}
        onChange={onToggle}
        className="peer sr-only"
        aria-label={typeof label === "string" ? label : undefined}
      />
      <span
        aria-hidden="true"
        className={cn(
          "flex size-3.5 shrink-0 items-center justify-center rounded-[4px] border transition-colors",
          "peer-focus-visible:ring-2 peer-focus-visible:ring-ring/50",
          checked ? "border-primary bg-primary text-primary-foreground" : "border-input",
        )}
      >
        {checked ? <Check className="size-2.5" strokeWidth={3} /> : null}
      </span>
      <span className={cn("min-w-0 flex-1 truncate", checked ? "font-medium" : "text-foreground/90")}>
        {label}
      </span>
      <span className="shrink-0 tabular-nums text-muted-foreground">{count}</span>
    </label>
  );
}

interface GroupProps {
  title: string;
  hint?: string;
  dirty: boolean;
  onReset: () => void;
  children: ReactNode;
}

function FilterGroup({ title, hint, dirty, onReset, children }: GroupProps) {
  return (
    <div className="min-w-0 border-t px-2 py-3 first:border-t-0 first:pt-0">
      <div className="mb-1 flex items-center justify-between gap-2 px-1.5">
        <h3 className="text-xs font-medium" title={hint}>
          {title}
        </h3>
        {dirty ? (
          <button
            type="button"
            onClick={onReset}
            className="cursor-pointer rounded text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
          >
            重置
          </button>
        ) : null}
      </div>
      <div className="space-y-0.5">{children}</div>
    </div>
  );
}

export interface ModelFilterRailProps {
  /** 全量模型视图，用于算各组计数。 */
  all: ModelView[];
  filters: ModelFilters;
  onChange: (next: ModelFilters) => void;
  className?: string;
}

export function ModelFilterRail({ all, filters, onChange, className }: ModelFilterRailProps) {
  const count = (predicate: (view: ModelView) => boolean) => all.filter(predicate).length;
  const dirty = activeFilterCount(filters) > 0;

  const siteCount = (site: ModelSiteKey) => count((v) => v.confirmedSites.includes(site));
  const tierCount = (tier: PriceTierKey) => count((v) => v.tier === tier);
  const reasoningCount = (key: ReasoningKey) => count((v) => v.reasoning === key);
  const capCount = (key: CapabilityKey) =>
    count((v) => (key === "tool" ? v.model.supports_tool_call : v.model.supports_images));
  const stateCount = (key: StateKey) => count((v) => (key === "blocked" ? v.blocked : !v.blocked));

  return (
    <aside
      className={cn("min-w-0", className)}
      aria-label="模型筛选"
    >
      <div className="rounded-xl border bg-card">
        <div className="flex items-center justify-between gap-2 border-b px-3.5 py-3">
          <span className="text-[13px] font-medium">筛选</span>
          {dirty ? (
            <button
              type="button"
              onClick={() =>
                onChange({ sites: [], tiers: [], reasoning: [], caps: [], state: [] })
              }
              className="cursor-pointer rounded text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline"
            >
              清除全部（{activeFilterCount(filters)}）
            </button>
          ) : null}
        </div>

        <FilterGroup
          title="可用站点"
          hint="已确认价格的站点；两侧都未确认的模型不会出现在任何一侧"
          dirty={filters.sites.length > 0}
          onReset={() => onChange({ ...filters, sites: [] })}
        >
          {SITE_KEYS.map((site) => (
            <OptionRow
              key={site}
              label={SITE_LABEL[site]}
              count={siteCount(site)}
              checked={filters.sites.includes(site)}
              onToggle={() => onChange({ ...filters, sites: toggleValue(filters.sites, site) })}
            />
          ))}
        </FilterGroup>

        <FilterGroup
          title="计费档位"
          hint="按已确认站点中的最低倍率分档 —— 网关默认倾向便宜的那一侧，最低倍率才是实际会付的价"
          dirty={filters.tiers.length > 0}
          onReset={() => onChange({ ...filters, tiers: [] })}
        >
          {PRICE_TIERS.map((tier) => (
            <OptionRow
              key={tier.key}
              label={
                <span className="inline-flex items-baseline gap-1.5">
                  {tier.label}
                  <span className="text-xs text-muted-foreground">{tier.hint}</span>
                </span>
              }
              count={tierCount(tier.key)}
              checked={filters.tiers.includes(tier.key)}
              onToggle={() => onChange({ ...filters, tiers: toggleValue(filters.tiers, tier.key) })}
            />
          ))}
        </FilterGroup>

        <FilterGroup
          title="推理档位"
          hint="上游用精简目录时会漏发 reasoning_supported，因此「不支持推理」里含字段缺失的模型"
          dirty={filters.reasoning.length > 0}
          onReset={() => onChange({ ...filters, reasoning: [] })}
        >
          {REASONING_OPTIONS.map((option) => (
            <OptionRow
              key={option.key}
              label={option.label}
              count={reasoningCount(option.key)}
              checked={filters.reasoning.includes(option.key)}
              onToggle={() =>
                onChange({ ...filters, reasoning: toggleValue(filters.reasoning, option.key) })
              }
            />
          ))}
        </FilterGroup>

        <FilterGroup
          title="能力"
          hint="同时勾选多项时要求全部满足"
          dirty={filters.caps.length > 0}
          onReset={() => onChange({ ...filters, caps: [] })}
        >
          {CAPABILITY_OPTIONS.map((option) => (
            <OptionRow
              key={option.key}
              label={option.label}
              count={capCount(option.key)}
              checked={filters.caps.includes(option.key)}
              onToggle={() => onChange({ ...filters, caps: toggleValue(filters.caps, option.key) })}
            />
          ))}
        </FilterGroup>

        <FilterGroup
          title="启用状态"
          dirty={filters.state.length > 0}
          onReset={() => onChange({ ...filters, state: [] })}
        >
          {STATE_OPTIONS.map((option) => (
            <OptionRow
              key={option.key}
              label={option.label}
              count={stateCount(option.key)}
              checked={filters.state.includes(option.key)}
              onToggle={() => onChange({ ...filters, state: toggleValue(filters.state, option.key) })}
            />
          ))}
        </FilterGroup>
      </div>
    </aside>
  );
}
