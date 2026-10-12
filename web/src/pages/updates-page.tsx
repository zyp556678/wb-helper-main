import * as React from "react";
import {
  AlertTriangle,
  CheckCircle2,
  Download,
  ExternalLink,
  HardDriveDownload,
  Info,
  Loader2,
  RefreshCw,
  Sparkles,
  Wand2,
} from "lucide-react";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Switch } from "@/components/ui/switch";
import { CardHint, Section } from "@/components/section";
import {
  checkUpdateNow,
  describeError,
  downloadUpdate,
  fetchUpdateStatus,
  installUpdate,
  openExternalUrl,
  setUpdateAutoCheck,
  setUpdateAutoDownload,
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
 * # 为什么从「配置」页搬出来单独一页
 *
 * 检查更新与配置页的其它内容有个根本差别：配置页整页是「偏好」，改完保存即可；
 * 这里是**动作**（检查、下载几十 MB、安装并重启），有过程、有失败态、有「网关正在
 * 重启」这种跨请求的状态。混在一堆开关中间时，用户既不容易发现它，也不容易看懂
 * 点下去会发生什么。
 *
 * # 自动到哪一步为止
 *
 * 检测与下载是只读的（打一次 GitHub、往数据目录放一个文件），所以都能自动；
 * **安装必须是人的一次明确动作**：它会覆盖已装好的程序、结束正在跑的进程、
 * 必要时提权，失败时可能留下一个既起不来也没有回退版本的现场。确认框里会列出
 * 将要执行的命令（由后端按平台给出），用户知道自己在同意什么。
 */
export function UpdatesPage() {
  const [status, setStatus] = React.useState<UpdateStatus | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [busy, setBusy] = React.useState<"" | "check" | "download" | "install" | "switch">("");
  const [error, setError] = React.useState("");
  /** 点了安装、网关正在重启：此时所有请求都会失败，页面进入探活模式。 */
  const [restarting, setRestarting] = React.useState(false);
  const [installOutput, setInstallOutput] = React.useState("");
  const [confirmOpen, setConfirmOpen] = React.useState(false);

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
  // 自动下载也在后台跑，所以本地还没有安装包时也继续轮询 —— 否则用户要手动刷新
  // 才能看到「已下载」，会以为开关没生效。
  const waitingForDownload =
    Boolean(status?.update_available) && status?.auto_download === true && !status?.local_path;
  useVisibleInterval(
    () => void load(),
    CHECK_POLL_MS,
    Boolean(status?.checking) || waitingForDownload,
  );

  // 网关重启期间的探活：能拿到状态就说明新版本已经起来了，整页刷新让所有页面
  // （含侧栏的版本号）都用新数据重建。
  useVisibleInterval(
    () => {
      void (async () => {
        try {
          await fetchUpdateStatus();
          notifySuccess("网关已重启完成");
          window.location.reload();
        } catch {
          // 还没起来，下一次探活继续。
        }
      })();
    },
    RESTART_POLL_MS,
    restarting,
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
        notifyInfo(`发现新版本 ${next.latest ?? ""}`, {
          description: next.auto_download ? "正在后台下载安装包。" : "可在本页下载安装包。",
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

  const handleSwitch = React.useCallback(
    async (kind: "check" | "download", next: boolean) => {
      setBusy("switch");
      setError("");
      try {
        // 后端返回保存后的真实状态（含它顺手触发的那次检查/下载），不做乐观更新：
        // 写 config.json 可能失败，乐观更新会让开关停在用户点的那一下。
        setStatus(
          kind === "check" ? await setUpdateAutoCheck(next) : await setUpdateAutoDownload(next),
        );
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
    try {
      const res = await downloadUpdate();
      if (!res.ok) {
        setError(res.detail || "下载安装包失败。");
        return;
      }
      notifySuccess(res.skipped ? "安装包已存在" : "安装包已下载", { description: res.detail });
      await load();
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy("");
    }
  }, [load]);

  const handleInstall = React.useCallback(async () => {
    setBusy("install");
    setError("");
    setInstallOutput("");
    try {
      const res = await installUpdate();
      if (!res.ok) {
        setError(res.detail || "安装失败。");
        setInstallOutput(res.output ?? "");
        return;
      }
      setInstallOutput(res.output ?? "");
      if (res.state) setStatus(res.state);
      if (res.restart) {
        // 页面接下来会连不上网关（它在重启），进入探活模式而不是报错。
        setRestarting(true);
        notifyInfo("安装完成，网关正在重启…", { description: res.detail });
        return;
      }
      notifySuccess("安装完成", { description: res.detail });
      await load();
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy("");
    }
  }, [load]);

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
  const canInstall = Boolean(status?.update_available && downloaded && plan?.supported);
  const busyAll = busy !== "" || restarting;

  return (
    <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
      <header className="mb-6 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">更新</h1>
          <p className="mt-2 text-sm leading-6 text-muted-foreground">
            查询发布仓库的最新版本，自动把匹配本机平台的安装包下好；确认安装后，网关会重启到新版本。
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

      {restarting ? (
        <Card className="mb-5 border-primary/40 bg-primary/5">
          <CardContent className="flex items-start gap-3 py-4">
            <Loader2 className="mt-0.5 size-4 shrink-0 animate-spin" aria-hidden="true" />
            <div className="min-w-0 space-y-1">
              <p className="text-sm font-medium">网关正在重启，页面会自动恢复</p>
              <p className="text-xs leading-5 text-muted-foreground">
                已经装上{latest ? ` v${latest}` : "新版本"}。重启期间这个页面连不上网关是正常的，
                起来了会自动刷新。
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

          {loading ? (
            <CardHint>正在读取更新状态…</CardHint>
          ) : status?.update_available ? (
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
            <div className="flex flex-wrap items-center gap-2">
              {canInstall && plan?.restart ? (
                <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
                  <AlertDialogTrigger asChild>
                    <Button type="button" size="sm" disabled={busyAll}>
                      {busy === "install" ? (
                        <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                      ) : (
                        <Wand2 className="size-3.5" aria-hidden="true" />
                      )}
                      {busy === "install" ? "正在安装…" : "安装并重启"}
                    </Button>
                  </AlertDialogTrigger>
                  <AlertDialogContent>
                    <AlertDialogHeader>
                      <AlertDialogTitle>安装 v{latest} 并重启网关？</AlertDialogTitle>
                      <AlertDialogDescription className="space-y-3">
                        <span className="block">
                          安装会覆盖已装好的程序（网关
                          {plan?.needs_auth ? "，并弹系统授权框临时提权" : ""}），随后网关自行重启到新版本。
                          重启期间面板会短暂断连，几秒后自动恢复。
                        </span>
                        {plan?.command ? (
                          <span className="block">
                            <span className="text-muted-foreground">将要执行：</span>
                            <code className="mt-1 block whitespace-pre-wrap break-all rounded-md border bg-muted/50 px-2 py-1.5 font-mono text-[11px] leading-5">
                              {plan.command}
                            </code>
                          </span>
                        ) : null}
                        {plan?.note ? (
                          <span className="block text-muted-foreground">{plan.note}</span>
                        ) : null}
                      </AlertDialogDescription>
                    </AlertDialogHeader>
                    <AlertDialogFooter>
                      <AlertDialogCancel>取消</AlertDialogCancel>
                      <AlertDialogAction
                        onClick={() => {
                          setConfirmOpen(false);
                          void handleInstall();
                        }}
                      >
                        安装并重启
                      </AlertDialogAction>
                    </AlertDialogFooter>
                  </AlertDialogContent>
                </AlertDialog>
              ) : null}

              {status?.update_available && !downloaded ? (
                <Button type="button" size="sm" disabled={busyAll} onClick={() => void handleDownload()}>
                  {busy === "download" ? (
                    <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                  ) : (
                    <Download className="size-3.5" aria-hidden="true" />
                  )}
                  {busy === "download" ? "正在下载…" : "下载安装包"}
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
          ) : null}

          {downloaded && status?.local_path ? (
            <CardHint>
              安装包已就绪：<span className="font-mono break-all">{status.local_path}</span>
              {status.local_bytes ? `（${formatBytes(status.local_bytes)}）` : ""}
              {plan?.supported
                ? "。点上面的「安装并重启」直接升级。"
                : "。本平台不支持面板内安装，请手动安装这个文件。"}
            </CardHint>
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
              {/* 私有仓库未认证访问一律 404，所以这里明说令牌的状态，用户才知道该去哪配。 */}
              {status.token_set ? " · 已配置访问令牌" : " · 未配置访问令牌（私有仓库需要）"}
            </CardHint>
          ) : null}
        </CardContent>
      </Section>

      <Section
        id="update-auto"
        title="自动更新"
        description="检测与下载可以全自动；安装会覆盖已装好的程序，所以留给你点一下。"
      >
        <CardContent className="space-y-4">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="min-w-0 space-y-0.5">
              <div className="flex items-center gap-2 text-sm font-medium">
                <Sparkles className="size-4 text-muted-foreground" aria-hidden="true" />
                自动检查
              </div>
              <div className="text-xs text-muted-foreground">
                {status
                  ? `每 ${status.interval_hours} 小时在后台查一次，发现新版本会写进日志并在本页提示`
                  : "在后台周期查询最新版本"}
              </div>
            </div>
            {loading ? (
              <Loader2 className="size-4 animate-spin text-muted-foreground" aria-hidden="true" />
            ) : (
              <Switch
                checked={status?.auto_check ?? false}
                disabled={busyAll || !status}
                onCheckedChange={(checked) => void handleSwitch("check", checked)}
                aria-label="自动检查更新"
              />
            )}
          </div>

          <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-4">
            <div className="min-w-0 space-y-0.5">
              <div className="flex items-center gap-2 text-sm font-medium">
                <HardDriveDownload className="size-4 text-muted-foreground" aria-hidden="true" />
                自动下载安装包
              </div>
              <div className="text-xs text-muted-foreground">
                发现新版本后直接下到数据目录，打开本页就能装 ——
                只放一个文件，不改动已安装的程序、也不重启网关。
              </div>
            </div>
            {loading ? (
              <Loader2 className="size-4 animate-spin text-muted-foreground" aria-hidden="true" />
            ) : (
              <Switch
                checked={status?.auto_download ?? false}
                disabled={busyAll || !status}
                onCheckedChange={(checked) => void handleSwitch("download", checked)}
                aria-label="自动下载安装包"
              />
            )}
          </div>

          <CardHint>
            安装**不**自动执行：它会覆盖程序、结束正在跑的进程，必要时还要提权，
            失败时可能留下一个既起不来也没有回退版本的现场。这一下由你点。
          </CardHint>
        </CardContent>
      </Section>
    </div>
  );
}
