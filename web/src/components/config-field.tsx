import * as React from "react";

import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

export interface ConfigRowProps {
  label: string;
  hint: string;
  htmlFor?: string;
  /** 校验失败时的红字提示。 */
  error?: string;
  children: React.ReactNode;
}

/** 紧凑的行式配置项：左侧标签 + 说明，右侧控件；错误信息排在说明下方。
 *
 * 行高与对齐取 BoardUI settings 行的配方：`min-h-[52px]` + `items-center`。
 * 用 `items-start` 时控件是贴着标签那一行的顶端对齐的，而标签/说明是两行文字块，
 * 视觉重心落在两行之间 —— 于是控件看起来比它标注的那一项「高半行」。
 * 走 min-h 而不是固定 h：说明长到换行时行要能自己长高。
 *
 * narrow 屏幕仍保留 `flex-wrap`：控件窄到放不下时换行到说明下方，
 * 而不是把标签压成细长条。
 */
export function ConfigRow({ label, hint, htmlFor, error, children }: ConfigRowProps) {
  return (
    <div className="flex min-h-[52px] flex-wrap items-center justify-between gap-x-6 gap-y-2 py-2.5">
      <div className="min-w-0 flex-1">
        <label htmlFor={htmlFor} className="text-[13px] font-medium leading-4 text-foreground">
          {label}
        </label>
        <p className="mt-0.5 text-xs leading-4 text-muted-foreground">{hint}</p>
        {error ? <p className="mt-1 text-xs font-medium text-destructive">{error}</p> : null}
      </div>
      <div className="flex shrink-0 items-center gap-1.5">{children}</div>
    </div>
  );
}

export interface NumberFieldProps {
  id?: string;
  value: string;
  /** 单位后缀，如「秒」「次」；小数型传 undefined。 */
  unit?: string;
  invalid?: boolean;
  disabled?: boolean;
  /** 小数型允许输入小数点，整数型仅数字。 */
  allowDecimal?: boolean;
  onChange: (value: string) => void;
}

/** 数字输入：文本型 + 输入法数字键盘，配合 config-page 的整数 / 小数校验。 */
export function NumberField({
  id,
  value,
  unit,
  invalid,
  disabled,
  allowDecimal,
  onChange,
}: NumberFieldProps) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <Input
        id={id}
        type="text"
        inputMode={allowDecimal ? "decimal" : "numeric"}
        value={value}
        disabled={disabled}
        spellCheck={false}
        autoComplete="off"
        aria-invalid={invalid || undefined}
        onChange={(event) => onChange(event.target.value)}
        className={cn(
          "h-8 w-28 text-right font-mono text-xs tabular-nums",
          invalid && "border-destructive focus-visible:ring-destructive/25",
        )}
      />
      {unit ? <span className="w-5 text-xs text-muted-foreground">{unit}</span> : null}
    </span>
  );
}

export interface DraftNumberFieldProps extends Omit<NumberFieldProps, "onChange"> {
  /** 失焦 / 回车时把整段草稿文本提交给父层（父层写入表单草稿）。 */
  onCommit: (value: string) => void;
}

/**
 * 草稿模式数字输入：输入期间只改本地文本，**失焦 / 回车才提交**。
 *
 * 为什么单独一个组件而不是给 NumberField 加开关：两种提交时机对应两类字段的
 * 语义 —— 普通参数边打边进草稿（随「保存配置」一起提交），而批次 5 的保活参数
 * 要求「失焦即存草稿」（半截输入 `1` 不该被当成 `12` 参与 dirty 判定）。
 * 混在一个组件里靠布尔分支，读的人要先想清楚当前是哪种模式。
 */
export function DraftNumberField({
  id,
  value,
  unit,
  invalid,
  disabled,
  allowDecimal,
  onCommit,
}: DraftNumberFieldProps) {
  const [text, setText] = React.useState(value);
  const [editing, setEditing] = React.useState(false);

  // 外部值变化（保存成功后的回读、重置）只在**没有正在编辑**时覆盖草稿，
  // 否则会把用户正在输入的内容顶掉。
  React.useEffect(() => {
    if (!editing) setText(value);
  }, [value, editing]);

  return (
    <span className="inline-flex items-center gap-1.5">
      <Input
        id={id}
        type="text"
        inputMode={allowDecimal ? "decimal" : "numeric"}
        value={text}
        disabled={disabled}
        spellCheck={false}
        autoComplete="off"
        aria-invalid={invalid || undefined}
        onFocus={() => setEditing(true)}
        onChange={(event) => setText(event.target.value)}
        onBlur={() => {
          setEditing(false);
          onCommit(text);
        }}
        onKeyDown={(event) => {
          if (event.key === "Enter") event.currentTarget.blur();
        }}
        className={cn(
          "h-8 w-28 text-right font-mono text-xs tabular-nums",
          invalid && "border-destructive focus-visible:ring-destructive/25",
        )}
      />
      {unit ? <span className="w-5 text-xs text-muted-foreground">{unit}</span> : null}
    </span>
  );
}
