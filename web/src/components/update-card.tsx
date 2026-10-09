import * as React from "react";
import {
  AlertTriangle,
  Download,
  ExternalLink,
  Loader2,
  RefreshCw,
  Sparkles,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CardContent } from "@/components/ui/card";
import { Switch } from "@/components/ui/switch";
import { CardHint, Section } from "@/components/section";
import {
  checkUpdateNow,
  describeError,
  downloadUpdate,
  fetchUpdateStatus,
  openExternalUrl,
  setUpdateAutoCheck,
} from "@/lib/api";
import { formatBytes, formatRelativeTime, nowSeconds } from "@/lib/format";
import { notifyError, notifyInfo, notifySuccess } from "@/lib/notify";
import type { UpdateStatus } from "@/lib/types";
import { useVisibleInterval } from "@/lib/use-visible-interval";

/** 后端正在检查时的轮询间隔；比日志页慢，因为一次真实检查要打 GitHub。 */
const CHECK_POLL_MS = 2000;

/**
 * 检查更新卡片。
 *
 * 与「开机自启动」同类：数据来自网关的**运行期状态**（不是 config.json 里的
 * 某个字段），因此自成一块状态，不参与配置页底部的「保存配置」链路 ——
 * 只有「自动检查」那个开关会写配置，而且它写的是后端自己的端点（立即生效）。
 *
 * 为什么前端不自己请求 GitHub：面板的 CSP 是 `connect-src 'self'`，跨域请求会被
 * 浏览器直接拦掉；私有仓库的只读令牌也只该由网关持有。所以这里所有请求都只是
 * 同源地问网关，由网关代发。
 */
export function UpdateCard() {
  const [status, setStatus] = React.useState<UpdateStatus | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [busy, setBusy] = React.useState<"" | "check" | "download" | "auto">("");
  const [error, setError] = React.useState("");
  /** 下载完成后的本地路径（后端返回，展示给用户去双击安装）。 */
  const [saved, setSaved] = React.useState("");

  const load = React.useCallback(async (signal?: AbortSignal) => {
    try {
      const next = await fetchUpdateStatus(signal);
      setStatus(next);
      setError("");
    } catch (err) {
      if (signal?.aborted) return;
      setError(describeError(err));
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  // 网关正在检查时轮询：包括「打开面板时后端自动补的那一次」。
  // checking 转 false 之后自动停，不会长期占着定时器。
  useVisibleInterval(() => void load(), CHECK_POLL_MS, Boolean(status?.checking));

  const handleCheck = React.useCallback(async () => {
    setBusy("check");
    setError("");
    setSaved("");
    try {
      const next = await checkUpdateNow();
      setStatus(next);
      if (next.error) {
        // 检查失败**不是**异常（网络不通、缺令牌都会走到这里），
        // 如实转述后端给的说明，别包装成「操作失败」。
        notifyError(next.error);
      } else if (next.update_available) {
        notifyInfo(`发现新版本 ${next.latest ?? ""}`, {
          description: "可在「检查更新」区块下载安装包。",
        });
      } else {
        notifySuccess("已是最新版本");
      }
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy("");
    }
  }, []);

  const handleAuto = React.useCallback(
    async (next: boolean) => {
      setBusy("auto");
      setError("");
      try {
        // 后端返回保存后的真实状态（含它顺手触发的那次检查），不做乐观更新：
        // 写 config.json 可能失败，乐观更新会让开关停在用户点的那一下。
        setStatus(await setUpdateAutoCheck(next));
      } catch (err) {
        setError(describeError(err));
        await load();
      } finally {
        setBusy("");
      }
    },
    [load],
  );

  const handleDownload = React.useCallback(async () => {
    setBusy("download");
    setError("");
    setSaved("");
    try {
      const res = await downloadUpdate();
      if (!res.ok) {
        setError(res.detail || "下载安装包失败。");
        return;
      }
      setSaved(res.path);
      notifySuccess(res.skipped ? "安装包已存在" : "安装包已下载", {
        description: res.detail,
      });
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy("");
    }
  }, []);

  const handleOpenRelease = React.useCallback(async () => {
    const url = status?.release_url;
    if (!url) return;
    try {
      // 走网关代开：桌面壳的 WebView 会静默拦掉 window.open。
      await openExternalUrl(url);
    } catch (err) {
      notifyError(describeError(err));
    }
  }, [status?.release_url]);

  const checking = busy === "check" || Boolean(status?.checking);
  const latest = status?.latest ?? "";

  return (
    <Section
      id="config-update"
      title="检查更新"
      description="向发布仓库查询最新版本；发现新版本时可把匹配本机平台的安装包下到数据目录。"
      action={
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={checking || busy !== ""}
          onClick={() => void handleCheck()}
        >
          {checking ? (
            <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
          ) : (
            <RefreshCw className="size-3.5" aria-hidden="true" />
          )}
          {checking ? "正在检查…" : "检查更新"}
        </Button>
      }
    >
      <CardContent className="space-y-3">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 py-1 text-sm">
          <span className="font-medium">当前版本</span>
          <span className="font-mono">v{status?.current ?? "…"}</span>
          <span className="text-muted-foreground">·</span>
          <span className="font-medium">最新版本</span>
          <span className="font-mono">{latest ? `v${latest}` : "—"}</span>
          {status && !status.error && status.checked_at ? (
            status.update_available ? (
              <Badge variant="warning">有新版本</Badge>
            ) : (
              <Badge variant="success">已是最新</Badge>
            )
          ) : null}
        </div>

        {loading ? (
          <CardHint>正在读取更新状态…</CardHint>
        ) : status?.update_available ? (
          <p className="px-1 text-xs leading-5 text-muted-foreground">
            {status.release_name ? `${status.release_name} · ` : ""}
            {status.published_at
              ? `发布于 ${formatRelativeTime(status.published_at, nowSeconds())}`
              : ""}
            {status.asset_name
              ? ` · 本机可下载 ${status.asset_name}${
                  status.asset_size ? `（${formatBytes(status.asset_size)}）` : ""
                }`
              : ""}
          </p>
        ) : null}

        {status?.notes ? (
          <div className="max-h-56 overflow-auto rounded-md border bg-muted/40 px-3 py-2">
            <p className="whitespace-pre-wrap text-xs leading-5">{status.notes}</p>
          </div>
        ) : null}

        {status?.update_available || saved ? (
          <div className="flex flex-wrap items-center gap-2">
            {status?.update_available ? (
              <Button
                type="button"
                size="sm"
                disabled={busy !== ""}
                onClick={() => void handleDownload()}
              >
                {busy === "download" ? (
                  <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                ) : (
                  <Download className="size-3.5" aria-hidden="true" />
                )}
                {busy === "download" ? "正在下载…" : "下载安装包"}
              </Button>
            ) : null}
            {status?.release_url ? (
              <Button type="button" variant="outline" size="sm" onClick={() => void handleOpenRelease()}>
                <ExternalLink className="size-3.5" aria-hidden="true" />
                打开 Release 页
              </Button>
            ) : null}
          </div>
        ) : null}

        {saved ? (
          <CardHint>
            安装包已保存到 <span className="font-mono break-all">{saved}</span>。
            双击安装即可完成升级（安装包未做代码签名，macOS 首次打开需右键 →「打开」）。
          </CardHint>
        ) : null}

        <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-3">
          <div className="min-w-0 space-y-0.5">
            <div className="flex items-center gap-2 text-sm font-medium">
              <Sparkles className="size-4 text-muted-foreground" aria-hidden="true" />
              自动检查
            </div>
            <div className="text-xs text-muted-foreground">
              {status
                ? `每 ${status.interval_hours} 小时在后台查一次，发现新版本会写进日志并在这里提示`
                : "在后台周期查询最新版本"}
            </div>
          </div>
          {loading ? (
            <Loader2 className="size-4 animate-spin text-muted-foreground" aria-hidden="true" />
          ) : (
            <Switch
              checked={status?.auto_check ?? false}
              disabled={busy !== "" || !status}
              onCheckedChange={(checked) => void handleAuto(checked)}
              aria-label="自动检查更新"
            />
          )}
        </div>

        {status?.error ? (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-xs text-destructive">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <span className="break-all">{status.error}</span>
          </div>
        ) : null}

        {error ? (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-xs text-destructive">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <span className="break-all">{error}</span>
          </div>
        ) : null}

        {status && !status.error ? (
          <CardHint>
            更新源 <span className="font-mono break-all">{status.repo}</span>
            {status.checked_at
              ? ` · 上次检查 ${formatRelativeTime(status.checked_at, nowSeconds())}`
              : " · 尚未检查"}
            {/* 私有仓库未认证访问一律 404，所以这里明说令牌的状态，用户才知道该去哪配。 */}
            {status.token_set ? " · 已配置访问令牌" : " · 未配置访问令牌（私有仓库需要）"}
          </CardHint>
        ) : null}
      </CardContent>
    </Section>
  );
}
