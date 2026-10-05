import * as React from "react";
import { AlertTriangle, Loader2, Power } from "lucide-react";

import { CardContent } from "@/components/ui/card";
import { Switch } from "@/components/ui/switch";
import { Section, CardHint } from "@/components/section";
import { fetchAutostart, setAutostart } from "@/lib/api";
import type { AutostartStatus } from "@/lib/types";

/** 宿主类型的中文名。 */
const KIND_LABEL: Record<string, string> = {
  desktop: "桌面版",
  cli: "命令行版",
  unknown: "未能识别",
};

/**
 * 开机自启动卡片。
 *
 * 自成一块独立状态：自启项不在 `config.json` 里（它是注册表 / plist / .desktop），
 * 所以不参与配置页的「未保存改动 / 保存配置」链路 —— 点开关即刻生效，
 * 不跟随底部那个保存按钮。
 *
 * 状态一律以**后端返回的真实状态**为准，不在前端做乐观更新：
 * 写注册表 / plist 可能因权限失败，乐观更新会让 UI 显示成已开启而实际没有。
 */
export function AutostartCard() {
  const [status, setStatus] = React.useState<AutostartStatus | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");

  const load = React.useCallback(async (signal?: AbortSignal) => {
    try {
      const next = await fetchAutostart(signal);
      setStatus(next);
      setError("");
    } catch (err) {
      if (signal?.aborted) return;
      setError(errorText(err));
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const handleToggle = React.useCallback(
    async (next: boolean) => {
      setBusy(true);
      setError("");
      try {
        const result = await setAutostart(next);
        setStatus(result);
      } catch (err) {
        setError(errorText(err));
        // 失败后回读真实状态：可能已经写进去了（例如写成功但装载失败），
        // 也可能被冲突拦住。不回读的话界面会停在用户点的那一下，与事实不符。
        await load();
      } finally {
        setBusy(false);
      }
    },
    [load],
  );

  const conflict = status?.conflict;
  // 不支持时整块隐藏 —— 显示一个永远点不动的开关比不显示更让人困惑。
  if (!loading && status && !status.supported) return null;

  return (
    <Section
      id="config-autostart"
      title="开机自启动"
      description="登录系统后自动在后台启动，供 CLI / IDE 持续调用。"
    >
      <CardContent className="space-y-3">
        <div className="flex flex-wrap items-center justify-between gap-3 py-1">
          <div className="min-w-0 space-y-0.5">
            <div className="flex items-center gap-2 text-sm font-medium">
              <Power className="size-4 text-muted-foreground" aria-hidden="true" />
              登录时自动启动
            </div>
            <div className="text-xs text-muted-foreground">
              {loading
                ? "正在读取系统自启状态…"
                : status?.enabled
                  ? `已开启${status.kind ? `（由${KIND_LABEL[status.kind] ?? status.kind}托管）` : ""}`
                  : "未开启，重启电脑后需要手动启动"}
            </div>
          </div>
          {loading ? (
            <Loader2 className="size-4 animate-spin text-muted-foreground" aria-hidden="true" />
          ) : (
            <Switch
              checked={status?.enabled ?? false}
              disabled={busy || !status?.supported}
              onCheckedChange={(checked) => void handleToggle(checked)}
              aria-label="开机自启动"
            />
          )}
        </div>

        {conflict ? (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-xs text-destructive">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <span>{conflict.message}</span>
          </div>
        ) : null}

        {error ? (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-xs text-destructive">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <span className="break-all">{error}</span>
          </div>
        ) : null}

        {status?.enabled && status.location ? (
          <CardHint>
            自启项位于 <span className="font-mono break-all">{status.location}</span>
            {status.host ? (
              <>
                {" "}
                ，指向 <span className="font-mono break-all">{status.host}</span>
              </>
            ) : null}
            。
          </CardHint>
        ) : null}
      </CardContent>
    </Section>
  );
}

function errorText(err: unknown): string {
  if (err instanceof Error && err.message) return err.message;
  return "操作失败，请重试。";
}
