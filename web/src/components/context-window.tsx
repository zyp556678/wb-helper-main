import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { formatCount, formatTokenCompact } from "@/lib/format";
import type { PanelModel } from "@/lib/types";

/**
 * 上下文窗口：默认档 + 可选档位 + 最大输入。
 *
 * 上游目录把三件事分开声明：
 *
 *   contextWindow.defaultLength  默认档（也是 context_length）
 *   contextWindow.supportedLengths  可选档位
 *   maxInputTokens               最大输入
 *
 * 只显示默认档会把模型说小 —— deepseek-v4.1-flash 默认 300K、可选
 * 300K/600K/1M、最大输入 1M，面板上却只看到 300K。所以有额外信息时加虚线下划线，
 * 悬停列全；没有额外信息时保持原来的纯数字，不制造多余的可交互元素。
 *
 * 文案为中英双语：值本身（300K / low / high）与语言无关，标签给两种语言。
 */
export function ContextWindow({
  model,
  prefix = "",
  exact = false,
}: {
  model: PanelModel;
  /** 表格里不带前缀（列头已说明），卡片里带「上下文 Context」。 */
  prefix?: string;
  /** 表格用精确千分位，卡片用紧凑单位（300K）。 */
  exact?: boolean;
}) {
  const value = model.context_length;
  const options = model.context_length_options ?? [];
  const maxInput = model.max_input_tokens ?? 0;
  const hasOptions = options.length > 1;
  const text = `${prefix}${exact ? formatCount(value) : formatTokenCompact(value)}`;

  if (!hasOptions && maxInput <= value) {
    return <span className="tabular-nums">{text}</span>;
  }

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="cursor-help border-b border-dashed border-muted-foreground/40 tabular-nums">
          {text}
        </span>
      </TooltipTrigger>
      <TooltipContent side="top" className="max-w-[300px] space-y-0.5 text-xs">
        <div>默认 Default：{formatTokenCompact(value)}</div>
        {hasOptions ? (
          <div>可选 Options：{options.map((n) => formatTokenCompact(n)).join(" · ")}</div>
        ) : null}
        {maxInput > 0 ? <div>最大输入 Max input：{formatTokenCompact(maxInput)}</div> : null}
        <div className="text-muted-foreground">
          由上游按请求自动选档 · Tier chosen by upstream
        </div>
      </TooltipContent>
    </Tooltip>
  );
}
