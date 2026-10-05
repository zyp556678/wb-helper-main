import { useState } from "react";
import type * as React from "react";
import { Check, Copy, Eye, EyeOff, KeyRound } from "lucide-react";
import {
  notifySuccess,
  notifyError,
} from "@/lib/notify";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { copyToClipboard } from "@/lib/utils";

/** 复制反馈的持续时间：够看清对勾，又不至于一直亮着。 */
const COPIED_MS = 1600;

/** 中间遮挡时露出的首尾长度。 */
const MASK_HEAD = 6;
const MASK_TAIL = 4;
/** 短于这个长度就整体打码：露出首尾等于把整串都给了。 */
const MASK_MIN_LENGTH = MASK_HEAD + MASK_TAIL + 3;
/**
 * 打码点数上限。
 *
 * 不按真实长度无限打码，是因为这一格是 `truncate` 的 —— 密钥很长时
 * （几十位）超出宽度的部分会被**从尾部**截掉，而尾部恰恰是用来辨认的那几位。
 * 与其显示一串被切掉尾巴、看不出是什么的点，不如把点数封顶、保证首尾都在。
 */
const MASK_MAX_DOTS = 16;

/**
 * 中间遮挡：露出首尾便于辨认，中间打码。
 *
 * 默认用这一档而不是「全打码」或「全明文」，是因为这两种都不好用：
 * 全打码看不出手里这把钥匙是不是要的那把（多账号时尤其明显），
 * 全明文又会在截图、录屏、旁边有人时直接漏出去。
 */
export function maskMiddle(key: string): string {
  if (key.length < MASK_MIN_LENGTH) return "•".repeat(key.length);
  const dots = Math.min(key.length - MASK_HEAD - MASK_TAIL, MASK_MAX_DOTS);
  return key.slice(0, MASK_HEAD) + "•".repeat(dots) + key.slice(-MASK_TAIL);
}

export interface AccessInfoCardProps {
  /** 网关是否要求访问密钥（config.auth_check_enabled）。 */
  authCheckEnabled: boolean;
  /** 网关**实际配置**的访问密钥；未配置时是空串。 */
  configuredApiKey: string;
}

/**
 * 接入信息：Base URL 与 API Key 各带一键复制。
 *
 * ## 值从哪来
 *
 * - **Base URL** 由当前页面地址推导。面板与 OpenAI 兼容接口**同源**（都在网关
 *   这一个端口上），所以 `location.origin` 就是网关地址，接口在它下面的 `/v1`。
 *   这样写而不是让后端下发，是因为后端看到的 `listen` 可能是 `0.0.0.0` 或
 *   `127.0.0.1`，而用户实际是用某个具体地址打开面板的 —— 用用户看到的那个才对。
 * - **API Key** 优先取网关**实际配置**的那一份（`configuredApiKey`）——
 *   用户要拿去填进别的工具，就必须是这个值；网关没配时回落到本机浏览器里
 *   存的那份（用户手动在「访问密钥」面板填过的情况）。
 *
 * ## 密钥的两种显示状态
 *
 * 默认**中间遮挡**（露首尾各几位），点眼睛才完全显示 —— 见 `maskMiddle`。
 * 无论哪种状态，复制按钮给的都是真值。
 */
export function AccessInfoCard({ authCheckEnabled, configuredApiKey }: AccessInfoCardProps) {
  // 网关配了密钥就用它 —— 这正是要填进别的工具的那个值。
  //
  // 刻意**不**回落到 localStorage 里那份：那可能是上一次运行留下的旧密钥
  // （比如先前用 --api-key 跑过、后来又没配），展示出来会让人以为网关还在校验它。
  // 后端是权威来源，两种情况下都以它为准。
  const apiKey = configuredApiKey;

  /** false = 中间遮挡（默认），true = 完全显示。 */
  const [revealed, setRevealed] = useState(false);
  const [copied, setCopied] = useState<"base" | "key" | null>(null);

  const baseUrl = `${window.location.origin}/v1`;
  const masked = apiKey ? maskMiddle(apiKey) : "";

  const copy = async (kind: "base" | "key", value: string, label: string) => {
    // 注意：无论当前是遮挡还是完全显示，复制的都是**真值**。
    // 复制按钮不该受显示状态影响 —— 否则用户以为复制到了明文，粘出来却是一串点。
    const ok = await copyToClipboard(value);
    if (!ok) {
      notifyError(`复制${label}失败，请手动选中复制`);
      return;
    }
    setCopied(kind);
    window.setTimeout(() => setCopied((prev) => (prev === kind ? null : prev)), COPIED_MS);
    notifySuccess(`已复制${label}`);
  };

  return (
    <Card className="gap-0 px-4 py-3">
      <div className="flex flex-col gap-2.5 lg:flex-row lg:items-center lg:gap-6">
        <div className="flex shrink-0 items-center gap-2">
          <span
            aria-hidden="true"
            className="inline-flex size-6 items-center justify-center rounded-md bg-primary/12 text-primary-ink"
          >
            <KeyRound className="size-3.5" />
          </span>
          <span className="text-[13px] font-medium">接入信息</span>
        </div>

        <Field
          label="Base URL"
          value={baseUrl}
          copied={copied === "base"}
          onCopy={() => void copy("base", baseUrl, "Base URL")}
        />

        {apiKey ? (
          <Field
            label="API Key"
            value={revealed ? apiKey : masked}
            copied={copied === "key"}
            onCopy={() => void copy("key", apiKey, "API Key")}
            extra={
              <Tooltip>
                <TooltipTrigger asChild>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    aria-label={revealed ? "改为中间遮挡" : "完全显示密钥"}
                    aria-pressed={revealed}
                    onClick={() => setRevealed((prev) => !prev)}
                  >
                    {revealed ? (
                      <EyeOff className="size-3.5" aria-hidden="true" />
                    ) : (
                      <Eye className="size-3.5" aria-hidden="true" />
                    )}
                  </Button>
                </TooltipTrigger>
                <TooltipContent side="top" className="max-w-[260px]">
                  {revealed
                    ? "改为中间遮挡（露出首尾各几位）"
                    : "完全显示密钥明文。默认只露首尾，避免截图或录屏时整串外泄"}
                </TooltipContent>
              </Tooltip>
            }
          />
        ) : (
          <div className="flex min-w-0 flex-1 items-center gap-2">
            <span className="shrink-0 text-xs text-muted-foreground">API Key</span>
            <span className="min-w-0 flex-1 truncate rounded-md bg-muted px-2 py-1 text-xs text-muted-foreground">
              {authCheckEnabled
                ? "网关要求鉴权，但未能读到密钥，请刷新页面重试"
                : "网关未启用鉴权，客户端可留空"}
            </span>
          </div>
        )}
      </div>
    </Card>
  );
}

/** 一行可复制字段：标签 + 等宽值 + 复制按钮。 */
function Field({
  label,
  value,
  copied,
  onCopy,
  extra,
}: {
  label: string;
  value: string;
  copied: boolean;
  onCopy: () => void;
  extra?: React.ReactNode;
}) {
  return (
    <div className="flex min-w-0 flex-1 items-center gap-2">
      <span className="shrink-0 text-xs text-muted-foreground">{label}</span>
      <code className="min-w-0 flex-1 truncate rounded-md bg-muted px-2 py-1 font-mono text-xs text-foreground">
        {value}
      </code>
      {extra}
      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            aria-label={`复制 ${label}`}
            onClick={onCopy}
          >
            {copied ? (
              <Check className="size-3.5 text-primary-ink" aria-hidden="true" />
            ) : (
              <Copy className="size-3.5" aria-hidden="true" />
            )}
          </Button>
        </TooltipTrigger>
        <TooltipContent side="top">{copied ? "已复制" : `复制 ${label}`}</TooltipContent>
      </Tooltip>
    </div>
  );
}
