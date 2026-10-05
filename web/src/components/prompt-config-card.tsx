import { CheckCircle2, Circle } from "lucide-react";

import { ConfigRow } from "@/components/config-field";
import { CardContent } from "@/components/ui/card";
import { Section } from "@/components/section";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import type { PromptMode } from "@/lib/types";
import { cn } from "@/lib/utils";

/** 配置页里可编辑的提示词字段（effective_text_preview 只读、不在其中）。 */
export interface PromptFormValue {
  mode: PromptMode;
  text: string;
  sanitize: boolean;
  degraded_retry: boolean;
  strict_first_system: boolean;
}

interface ModeOption {
  value: PromptMode;
  label: string;
  description: string;
}

const MODE_OPTIONS: ModeOption[] = [
  {
    value: "passthrough",
    label: "完全透传",
    description:
      "客户端 system 原样发上游。行为最忠实，但客户端模板句可能被上游逐字匹配拦截，适合排障。",
  },
  {
    value: "append",
    label: "追加（默认）",
    description:
      "客户端消息逐字不动，只在开头连续 system 块之后插入一条网关提示词。保留客户端项目规范与工具约定。",
  },
  {
    value: "custom",
    label: "替换",
    description:
      "删除全部客户端 system，只发网关提示词。拦截率最低、行为最可预期，但会丢掉客户端自己的规范。",
  },
];

export interface PromptConfigCardProps {
  value: PromptFormValue;
  /** 只读：后端返回的当前实际生效提示词（前 200 字）。 */
  effectivePreview: string;
  /** 后端读取的提示词文件路径；非空时上方正文不会生效。 */
  file: string;
  saving: boolean;
  onChange: (patch: Partial<PromptFormValue>) => void;
}

/** 「提示词与出站改写」卡片：模式三选一 + 提示词正文 / 生效预览 + 三个开关。 */
export function PromptConfigCard({
  value,
  effectivePreview,
  file,
  saving,
  onChange,
}: PromptConfigCardProps) {
  const preview = effectivePreview.trim();

  return (
    <Section
      id="config-prompt"
      title="提示词与出站改写"
      description="网关发往上游前如何处理 system 提示词，以及被内容策略拦截后的兜底行为；全部改动热生效。"
    >
      <CardContent className="space-y-5">
        <div>
          <div className="text-sm font-medium text-foreground">提示词模式</div>
          <p className="mt-0.5 text-xs leading-5 text-muted-foreground">
            决定客户端的 system 消息与网关提示词如何合成一条出站请求。
          </p>
          <div
            role="radiogroup"
            aria-label="提示词模式"
            className="mt-2.5 grid gap-2 sm:grid-cols-3"
          >
            {MODE_OPTIONS.map((option) => {
              const active = option.value === value.mode;
              return (
                <button
                  key={option.value}
                  type="button"
                  role="radio"
                  aria-checked={active}
                  disabled={saving}
                  onClick={() => onChange({ mode: option.value })}
                  className={cn(
                    "flex h-full cursor-pointer flex-col gap-1.5 rounded-lg border p-3 text-left outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-60",
                    active ? "border-primary/50 bg-primary/[0.07]" : "border-border hover:bg-accent/40",
                  )}
                >
                  <span className="flex items-center gap-1.5 text-sm font-medium">
                    {active ? (
                      <CheckCircle2 className="size-3.5 shrink-0 text-primary-ink" aria-hidden="true" />
                    ) : (
                      <Circle className="size-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />
                    )}
                    {option.label}
                  </span>
                  <span className="text-xs leading-5 text-muted-foreground">
                    {option.description}
                  </span>
                </button>
              );
            })}
          </div>
        </div>

        <div className="border-t border-border pt-4">
          <label htmlFor="prompt-text" className="text-sm font-medium text-foreground">
            网关提示词
          </label>
          <p className="mt-0.5 text-xs leading-5 text-muted-foreground">
            append 模式下插入在开头连续 system 块之后；custom 模式下作为唯一的 system 内容。
          </p>
          <Textarea
            id="prompt-text"
            value={value.text}
            disabled={saving}
            rows={6}
            spellCheck={false}
            placeholder="留空则使用内置默认提示词"
            onChange={(event) => onChange({ text: event.target.value })}
            className="mt-2"
          />
          {file ? (
            <p className="mt-1.5 text-xs leading-5 text-amber-700 dark:text-amber-300">
              已配置提示词文件 <span className="break-all font-mono">{file}</span>
              ，后端优先读取该文件，上方正文不会生效。
            </p>
          ) : null}

          <div className="mt-3">
            <div className="flex flex-wrap items-baseline gap-x-1.5 gap-y-0.5">
              <span className="text-xs font-medium text-muted-foreground">当前实际生效</span>
              <span className="text-xs text-muted-foreground">（后端返回的前 200 字）</span>
            </div>
            <pre className="mt-1.5 max-h-[160px] overflow-auto rounded-md border border-border bg-muted/40 px-3 py-2 font-mono text-xs leading-5 whitespace-pre-wrap break-words text-foreground">
              {preview || "（空）"}
            </pre>
          </div>
        </div>

        <div className="divide-y divide-border border-t border-border">
          <ConfigRow label="出站指纹脱敏" hint="剥离客户端模板句与反探测串，降低被内容策略误拦的概率">
            <Switch
              checked={value.sanitize}
              disabled={saving}
              aria-label="出站指纹脱敏"
              onCheckedChange={(checked) => onChange({ sanitize: checked })}
            />
          </ConfigRow>
          <ConfigRow label="拦截后降级重试" hint="被拦截时换中性提示词再试一次，避免整单失败">
            <Switch
              checked={value.degraded_retry}
              disabled={saving}
              aria-label="拦截后降级重试"
              onCheckedChange={(checked) => onChange({ degraded_retry: checked })}
            />
          </ConfigRow>
          <ConfigRow label="首条消息保底 system" hint="首条不是 system 时自动补一条，避免上游参数校验报错">
            <Switch
              checked={value.strict_first_system}
              disabled={saving}
              aria-label="首条消息保底 system"
              onCheckedChange={(checked) => onChange({ strict_first_system: checked })}
            />
          </ConfigRow>
        </div>

        <p className="text-xs leading-5 text-muted-foreground">
          「更忠实」与「更安全」是冲突的：完全透传最忠实，替换最不容易撞内容策略拦截，默认的追加是两者的折中。若日志里频繁出现出站改写相关的拦截重试，可以切到替换。
        </p>
      </CardContent>
    </Section>
  );
}
