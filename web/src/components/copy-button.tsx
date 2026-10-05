import * as React from "react";
import { Check, Copy } from "lucide-react";
import {
  notifySuccess,
  notifyError,
} from "@/lib/notify";

import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { copyToClipboard } from "@/lib/utils";

/** 复制反馈的持续时间：够看清对勾，又不至于一直亮着。 */
export const COPIED_MS = 1600;

/**
 * 复制按钮的通用交互：点一下 → 写剪贴板 → 显示对勾 → 自动复原。
 *
 * 抽出来是因为「复制 + 反馈」这套逻辑在项目里已有多处（接入信息卡、账号弹窗、
 * 现在的模型名），每处各写一遍必然出现「有的地方给 toast、有的地方没反馈」
 * 或「对勾不消失」这类不一致。
 *
 * 失败时**必须给出反馈**：剪贴板 API 在非安全上下文（http 下的局域网面板）
 * 会被浏览器拒绝，静默失败会让用户以为复制成功了，粘出来是上一次的内容。
 */
export function useCopy(resetMs: number = COPIED_MS) {
  const [copiedKey, setCopiedKey] = React.useState<string | null>(null);
  const timer = React.useRef<number | null>(null);

  // 组件卸载时清掉待触发的定时器：否则会在已卸载的组件上 setState。
  React.useEffect(() => {
    return () => {
      if (timer.current !== null) window.clearTimeout(timer.current);
    };
  }, []);

  const copy = React.useCallback(
    async (text: string, key: string, label: string) => {
      const ok = await copyToClipboard(text);
      if (!ok) {
        notifyError(`复制${label}失败，请手动选中复制`);
        return false;
      }
      setCopiedKey(key);
      if (timer.current !== null) window.clearTimeout(timer.current);
      timer.current = window.setTimeout(() => {
        setCopiedKey((prev) => (prev === key ? null : prev));
      }, resetMs);
      notifySuccess(`已复制${label}`);
      return true;
    },
    [resetMs],
  );

  return { copiedKey, copy };
}

/** 只显示图标的小复制按钮（用于列表/卡片这类紧凑位置）。 */
export function CopyIconButton({
  label,
  copied,
  onCopy,
  className,
  iconClassName,
}: {
  /** 复制对象的名称，用于无障碍标签与提示文案（如「模型名」）。 */
  label: string;
  copied: boolean;
  onCopy: () => void;
  className?: string;
  iconClassName?: string;
}) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className={className}
          aria-label={copied ? `已复制${label}` : `复制${label}`}
          onClick={onCopy}
        >
          {copied ? (
            <Check className={cn("size-3.5 text-primary-ink", iconClassName)} aria-hidden="true" />
          ) : (
            <Copy className={cn("size-3.5", iconClassName)} aria-hidden="true" />
          )}
        </Button>
      </TooltipTrigger>
      <TooltipContent side="top">{copied ? "已复制" : `复制 ${label}`}</TooltipContent>
    </Tooltip>
  );
}
