import * as React from "react";
import {
  AlertTriangle,
  CheckCircle2,
  Download,
  ExternalLink,
  Info,
  Loader2,
  RefreshCw,
  Wand2,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { CardHint, Section } from "@/components/section";
import {
  checkUpdateNow,
  describeError,
  downloadUpdate,
  fetchUpdateStatus,
  installUpdate,
  openExternalUrl,
} from "@/lib/api";
import { formatBytes, formatRelativeTime, nowSeconds } from "@/lib/format";
import { notifyError, notifyInfo, notifySuccess } from "@/lib/notify";
import type { UpdateStatus } from "@/lib/types";
import { useVisibleInterval } from "@/lib/use-visible-interval";

/** 后端正在检查时的轮询间隔；比日志页慢，因为一次真实检查要打 GitHub。 */
const CHECK_POLL_MS = 2000;
/** 点击安装之后的探活间隔：网关重启通常几秒，探到了就整页刷新。 */
const RESTART_POLL_MS = 1500;

/**
 * 更新页。
 *
 * # 只有两件事：看状态、点更新
 *
 * 检测是后台自动的（每 6 小时一次只读 GET，代价低到不需要开关）；发现新版本之后
 * 本页显示「有新版本」与一个按钮。用户点那一下就是全部的选择：**还没下载就先下，
 * 下载完成立刻装**，装完网关回到新版本。所以这里没有「自动下载 / 自动安装」开关 ——
 * 该不该装就是点不点按钮的问题。
 *
 * # 为什么安装不需要再做一次确认
 *
 * 安装会覆盖已装好的程序、结束正在跑的进程，但**安装过程本身不需要任何选择**
 * （Windows 上跑 `/S /R`，不弹向导）。刚才那一次点击就是确认；再补一个「确定要装吗」
 * 只是把同一个问题问两遍。真正需要知情的是「会短暂断连」这件事，写在按钮下面。
 */
export function UpdatesPage() {
  const [status, setStatus] = React.useState<UpdateStatus | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [busy, setBusy] = React.useState<"" | "check" | "update">("");
  const [error, setError] = React.useState("");
  /**
   * 点了更新之后网关的去向：`restart`（Unix：换上新二进制继续服务）或
   * `exit`（Windows：退出以让出文件锁，装完由安装包把应用重新拉起来）。
   * 两者都会让本页暂时连不上网关，所以统一进入探活模式。
   */
  const [outcome, setOutcome] = React.useState<"" | "restart" | "exit">("");
  const [installOutput, setInstallOutput] = React.useState("");

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
  // 下载与安装期间也要轮询 —— 进度条与「正在安装」都靠它。
  useVisibleInterval(
    () => void load(),
    // 下载进度条跟着这个节奏走：间隔太长条会一顿一顿的，太短又刷得太凶。
    status?.downloading ? 800 : CHECK_POLL_MS,
    Boolean(status?.checking) || Boolean(status?.downloading) || Boolean(status?.installing),
  );

  // 网关重启/退出期间的探活：能拿到状态就说明新版本已经起来了，整页刷新让所有页面
  //（含侧栏的版本号）都用新数据重建。退出那条路上这一步要等安装包把应用重新拉起来
  //（Windows：退出、覆盖文件、重开应用），所以探活不设总时长上限 ——
  // 页面不可见时 useVisibleInterval 自己会停。
  useVisibleInterval(
    () => {
      void (async () => {
        try {
          await fetchUpdateStatus();
          notifySuccess(outcome === "exit" ? "网关已重新启动" : "网关已重启完成");
          window.location.reload();
        } catch {
          // 还没起来，下一次探活继续。
        }
      })();
    },
    RESTART_POLL_MS,
    outcome !== "",
  );

  const handleCheck = React.useCallback(async () => {
    setBusy("check");
    setError("");
    try {
      const next = await checkUpdateNow();
      setStatus(next);
      if (next.error) {
        // 检查失败**不是**异常（网络不通、缺令牌都会走到这里），
        // 如实转述后端给的说明，别包装成「操作失败」。
        notifyError(next.error);
      } else if (next.update_available) {
        notifyInfo(`发现新版本 ${next.latest ?? ""}`, { description: "点「更新」即可下载并安装。" });
      } else {
        notifySuccess("已是最新版本");
      }
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy("");
    }
  }, []);

  /**
   * 点一下「更新」：没下过就先下载，下载完成**接着自动安装**。
   *
   * 下载与安装分成两个请求（而不是后端一次做完）是因为下载要几百 MB 的进度可看、
   * 失败要能单独重试；而安装一旦开始就不该被网络中断打断 —— 两段的失败语义不同。
   * 顺序固定在这里，用户只需要点一次。
   */
  const handleUpdate = React.useCallback(async () => {
    setBusy("update");
    setError("");
    setInstallOutput("");
    try {
      if (!status?.local_path) {
        const dl = await downloadUpdate();
        if (!dl.ok) {
          setError(dl.detail || "下载安装包失败。");
          return;
        }
        notifySuccess(dl.skipped ? "安装包已就绪" : "安装包已下载", { description: "接着开始安装…" });
        await load();
      }
      const res = await installUpdate();
      if (!res.ok) {
        setError(res.detail || "安装失败。");
        setInstallOutput(res.output ?? "");
        return;
      }
      setInstallOutput(res.output ?? "");
      if (res.state) setStatus(res.state);
      if (res.restart || res.exit) {
        // 页面接下来会连不上网关（它在重启或退出），进入探活模式而不是报错。
        setOutcome(res.restart ? "restart" : "exit");
        notifyInfo(res.restart ? "安装完成，网关正在重启…" : "安装完成，网关即将退出", {
          description: res.detail,
        });
        return;
      }
      notifySuccess("安装完成", { description: res.detail });
      await load();
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy("");
    }
  }, [load, status?.local_path]);

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
  const plan = status?.install;
  const downloaded = Boolean(status?.local_path);
  const busyAll = busy !== "" || outcome !== "" || Boolean(status?.installing);

  return (
    <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
      <header className="mb-6 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">更新</h1>
          <p className="mt-2 text-sm leading-6 text-muted-foreground">
            查询发布仓库的最新版本；点「更新」把匹配本机平台的安装包下好，下载完成会自动安装并重启网关。
          </p>
        </div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={checking || busyAll}
          onClick={() => void handleCheck()}
        >
          {checking ? (
            <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
          ) : (
            <RefreshCw className="size-3.5" aria-hidden="true" />
          )}
          {checking ? "正在检查…" : "检查更新"}
        </Button>
      </header>

      {outcome !== "" ? (
        <Card className="mb-5 border-primary/40 bg-primary/5">
          <CardContent className="flex items-start gap-3 py-4">
            <Loader2 className="mt-0.5 size-4 shrink-0 animate-spin" aria-hidden="true" />
            <div className="min-w-0 space-y-1">
              <p className="text-sm font-medium">
                {outcome === "exit" ? "网关已退出，安装完成后应用会自动启动" : "网关正在重启，页面会自动恢复"}
              </p>
              <p className="text-xs leading-5 text-muted-foreground">
                已经装上{latest ? ` v${latest}` : "新版本"}。
                {outcome === "exit"
                  ? "Windows 上必须先让出文件锁，所以应用会停一会儿：安装包正在后台静默完成（不需要任何选择），装完会自动把应用重新拉起来，页面也会自己接回来。"
                  : "重启期间这个页面连不上网关是正常的，起来了会自动刷新。"}
              </p>
            </div>
          </CardContent>
        </Card>
      ) : null}

      <Section
        id="update-version"
        title="版本状态"
        description="向发布仓库查询最新版本；安装包由网关代下（面板在浏览器里发不出跨域请求，令牌也只该由网关持有）。"
      >
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm">
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
            {status?.update_available && downloaded ? (
              <Badge variant="success">
                <CheckCircle2 className="mr-1 size-3" aria-hidden="true" />
                安装包已就绪
              </Badge>
            ) : null}
          </div>

          {status?.downloading ? (
            <div className="space-y-1.5" aria-live="polite">
              <div className="flex items-center justify-between text-xs text-muted-foreground">
                <span className="inline-flex items-center gap-1.5">
                  <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                  正在下载安装包…
                </span>
                {status.download_total_bytes ? (
                  <span className="font-mono">
                    {formatBytes(status.downloaded_bytes ?? 0)} / {formatBytes(status.download_total_bytes)}
                    {status.download_total_bytes > 0 && status.downloaded_bytes !== undefined
                      ? `（${Math.min(100, Math.round((status.downloaded_bytes / status.download_total_bytes) * 100))}%）`
                      : ""}
                  </span>
                ) : null}
              </div>
              {status.download_total_bytes ? (
                <div
                  role="progressbar"
                  aria-valuemin={0}
                  aria-valuemax={100}
                  aria-valuenow={Math.min(100, Math.round(((status.downloaded_bytes ?? 0) / status.download_total_bytes) * 100))}
                  className="h-1.5 overflow-hidden rounded-full bg-muted"
                >
                  <div
                    className="h-full rounded-full bg-primary transition-[width] duration-300"
                    style={{
                      width: `${Math.min(100, Math.round(((status.downloaded_bytes ?? 0) / status.download_total_bytes) * 100))}%`,
                    }}
                  />
                </div>
              ) : null}
            </div>
          ) : null}

          {status?.installing ? (
            <p className="inline-flex items-center gap-1.5 rounded-md border bg-muted/40 px-3 py-2 text-xs text-muted-foreground" aria-live="polite">
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
              正在安装（可能弹系统授权框；装完网关会重启或退出）…
            </p>
          ) : null}

          {loading ? (
            <CardHint>正在读取更新状态…</CardHint>
          ) : !status?.downloading && status?.update_available ? (
            <p className="text-xs leading-5 text-muted-foreground">
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

          {status?.detail ? (
            <div className="flex items-start gap-2 rounded-md border bg-muted/40 px-3 py-2 text-xs">
              <Info className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />
              <span>{status.detail}</span>
            </div>
          ) : null}

          {status?.notes ? (
            <div className="max-h-56 overflow-auto rounded-md border bg-muted/40 px-3 py-2">
              <p className="whitespace-pre-wrap text-xs leading-5">{status.notes}</p>
            </div>
          ) : null}

          {status?.update_available || downloaded ? (
            <div className="space-y-2">
              <div className="flex flex-wrap items-center gap-2">
                {/* 没匹配到本机形态的安装包时不显示「更新」：后端已经把原因写进
                    detail（npm 安装要用 npm 升级、缺平台产物要去 Release 页）。
                    留一个点了只会报错的按钮，比不显示更让人困惑。 */}
                {status?.update_available && status.asset_name ? (
                  <Button type="button" size="sm" disabled={busyAll} onClick={() => void handleUpdate()}>
                    {busy === "update" ? (
                      <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                    ) : downloaded ? (
                      <Wand2 className="size-3.5" aria-hidden="true" />
                    ) : (
                      <Download className="size-3.5" aria-hidden="true" />
                    )}
                    {busy === "update"
                      ? downloaded
                        ? "正在安装…"
                        : "正在下载…"
                      : downloaded
                        ? "安装更新"
                        : "下载更新"}
                  </Button>
                ) : null}

                {status?.release_url ? (
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled={busyAll}
                    onClick={() => void handleOpenRelease()}
                  >
                    <ExternalLink className="size-3.5" aria-hidden="true" />
                    打开 Release 页
                  </Button>
                ) : null}
              </div>

              {/* 点完之后会发生什么写在这里 —— 按钮旁边是用户唯一的知情入口。 */}
              {status?.update_available && status.asset_name ? (
                <CardHint>
                  {downloaded
                    ? "安装包已就绪，点「安装更新」直接装。"
                    : "点「下载更新」：安装包下好之后会自动开始安装。"}
                  安装过程不需要任何选择（Windows 上也不弹向导）；网关会重启到新版本，
                  本页会短暂断连，恢复后自动刷新。
                </CardHint>
              ) : null}

              {downloaded && status?.local_path ? (
                <CardHint>
                  安装包在：<span className="font-mono break-all">{status.local_path}</span>
                  {status.local_bytes ? `（${formatBytes(status.local_bytes)}）` : ""}
                </CardHint>
              ) : null}
            </div>
          ) : null}

          {plan && status?.update_available && !plan.supported && plan.reason ? (
            <div className="flex items-start gap-2 rounded-md border bg-muted/40 px-3 py-2 text-xs">
              <Info className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />
              <span>{plan.reason}</span>
            </div>
          ) : null}

          {installOutput ? (
            <div className="max-h-40 overflow-auto rounded-md border bg-muted/40 px-3 py-2">
              <p className="whitespace-pre-wrap font-mono text-[11px] leading-5">{installOutput}</p>
            </div>
          ) : null}

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
            </CardHint>
          ) : null}
        </CardContent>
      </Section>
    </div>
  );
}
