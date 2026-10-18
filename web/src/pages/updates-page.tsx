import * as React from "react";
import {
  AlertTriangle,
  CheckCircle2,
  Download,
  ExternalLink,
  Globe,
  HardDriveDownload,
  Info,
  Loader2,
  RefreshCw,
  Save,
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
import { Input } from "@/components/ui/input";
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
  setUpdateAutoInstall,
  setUpdateMirror,
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
 * 检测、下载、安装都可以全自动（三个开关默认全开），而且安装过程本身**不需要用户在
 * 安装包里做任何选择**（Windows 上也不再弹向导）。分界画在「要不要自动装」这个开关上：
 * 安装会覆盖已装好的程序、结束正在跑的进程、必要时提权，失败时可能留下一个既起不来
 * 也没有回退版本的现场 —— 关掉这个开关就退回「只下不装」。手动那条路（面板上的
 * 「安装并重启」按钮）在确认框里列出将要执行的命令，用户知道自己在同意什么。
 */
export function UpdatesPage() {
  const [status, setStatus] = React.useState<UpdateStatus | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [busy, setBusy] = React.useState<"" | "check" | "download" | "install" | "switch" | "mirror">("");
  const [error, setError] = React.useState("");
  /**
   * 点了安装之后网关的去向：`restart`（Unix：换上新二进制继续服务）或
   * `exit`（Windows：退出以让出文件锁，装完由安装包把应用重新拉起来）。
   * 两者都会让本页暂时连不上网关，所以统一进入探活模式。
   */
  const [outcome, setOutcome] = React.useState<"" | "restart" | "exit">("");
  const [installOutput, setInstallOutput] = React.useState("");
  const [confirmOpen, setConfirmOpen] = React.useState(false);
  /** 「自动安装」是更新链路里唯一动系统的自动化，打开前必须再确认一次。 */
  const [confirmAutoInstall, setConfirmAutoInstall] = React.useState(false);

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
    // 下载进度条跟着这个节奏走：间隔太长条会一顿一顿的，太短又刷得太凶。
    status?.downloading ? 800 : CHECK_POLL_MS,
    Boolean(status?.checking) ||
      Boolean(status?.downloading) ||
      Boolean(status?.installing) ||
      waitingForDownload,
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
    async (kind: "check" | "download" | "install", next: boolean) => {
      setBusy("switch");
      setError("");
      try {
        // 后端返回保存后的真实状态（含它顺手触发的那次检查/下载），不做乐观更新：
        // 写 config.json 可能失败，乐观更新会让开关停在用户点的那一下。
        const next_status =
          kind === "check"
            ? await setUpdateAutoCheck(next)
            : kind === "download"
              ? await setUpdateAutoDownload(next)
              : await setUpdateAutoInstall(next);
        setStatus(next_status);
      } catch (err) {
        setError(describeError(err));
        await load();
      } finally {
        setBusy("");
      }
    },
    [load],
  );

  // 下载加速前缀：文本输入，失焦/回车才提交（每个字符都发请求太吵）。
  const [mirrorInput, setMirrorInput] = React.useState<string | null>(null);
  const mirrorDraft = mirrorInput ?? (status?.download_mirror ?? "");
  const handleMirrorSave = React.useCallback(async () => {
    setBusy("mirror");
    setError("");
    try {
      const res = await setUpdateMirror(mirrorDraft.trim());
      setStatus((prev) => (prev ? { ...prev, download_mirror: res.mirror } : prev));
      notifySuccess(res.detail || "下载加速设置已保存");
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy("");
      setMirrorInput(null);
    }
  }, [mirrorDraft]);

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
  const busyAll = busy !== "" || outcome !== "" || Boolean(status?.installing);

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
              正在自动安装（可能弹系统授权框，装完网关会重启或退出）…
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
                          {plan?.needs_auth ? "，并弹系统授权框临时提权" : ""}），装的过程不需要任何选择。
                          安装期间这个页面会短暂断连，恢复后自动刷新。
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

              {/* 没有匹配本机形态的安装包时不显示「下载」：后端已经把原因写进
                  detail（npm 安装要用 npm 升级、缺平台产物要去 Release 页）。
                  留一个点了只会报错的按钮，比不显示更让人困惑。 */}
              {status?.update_available && !downloaded && status.asset_name ? (
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
            </CardHint>
          ) : null}
        </CardContent>
      </Section>

      <Section
        id="update-auto"
        title="自动更新"
        description="检测、下载、安装都可以全自动；安装会覆盖已装好的程序、结束正在跑的进程，所以默认开启但随时可关。"
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

          <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-4">
            <div className="min-w-0 space-y-0.5">
              <div className="flex items-center gap-2 text-sm font-medium">
                <Wand2 className="size-4 text-muted-foreground" aria-hidden="true" />
                自动安装
              </div>
              <div className="text-xs text-muted-foreground">
                安装包下好之后直接装并重启，全程不需要任何选择
                （Windows 上也是：后台静默安装，不弹向导、不用点任何按钮）。
                默认开启；它会覆盖程序、结束正在跑的进程，可随时关掉改成「只下不装」。
              </div>
            </div>
            {loading ? (
              <Loader2 className="size-4 animate-spin text-muted-foreground" aria-hidden="true" />
            ) : (
              <Switch
                checked={status?.auto_install ?? false}
                disabled={busyAll || !status}
                onCheckedChange={(checked) => {
                  // 默认已开；关掉是恢复安全状态，直接生效、不拦。
                  // 重新打开会覆盖程序、结束进程、重启网关，先确认。
                  if (checked) setConfirmAutoInstall(true);
                  else void handleSwitch("install", false);
                }}
                aria-label="自动安装更新"
              />
            )}
          </div>

          <div className="space-y-1.5 border-t pt-4">
            <div className="flex items-center gap-2 text-sm font-medium">
              <Globe className="size-4 text-muted-foreground" aria-hidden="true" />
              下载加速
            </div>
            <p className="text-xs leading-5 text-muted-foreground">
              <strong className="font-medium text-foreground">默认开启</strong>
              ：直连 github.com 下安装包慢，实际请求会变成 前缀 + 原地址。只影响安装包下载，
              不影响版本检查。
              <strong className="font-medium text-foreground">清空并保存 = 退回直连</strong>
              （安装包将不经第三方转发）。
            </p>
            <div className="flex flex-wrap items-center gap-2">
              <Input
                value={mirrorDraft}
                onChange={(e) => setMirrorInput(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void handleMirrorSave();
                }}
                placeholder="https://gh-proxy.org（清空 = 直连）"
                spellCheck={false}
                className="h-8 max-w-md font-mono text-xs"
                aria-label="下载加速前缀"
              />
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={busy === "mirror" || mirrorDraft.trim() === (status?.download_mirror ?? "")}
                onClick={() => void handleMirrorSave()}
              >
                {busy === "mirror" ? (
                  <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                ) : (
                  <Save className="size-3.5" aria-hidden="true" />
                )}
                保存
              </Button>
            </div>
          </div>

          <AlertDialog open={confirmAutoInstall} onOpenChange={setConfirmAutoInstall}>
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>打开「自动安装」？</AlertDialogTitle>
                <AlertDialogDescription className="space-y-3">
                  <span className="block">
                    打开之后：发现新版本会**自动**下载并安装，全程不需要任何选择
                    （Windows 上也不会弹向导），网关随即回到新版本。
                  </span>
                  <span className="block">
                    安装会覆盖已装好的程序、结束正在跑的进程，必要时弹系统授权框；
                    失败时可能留下一个既起不来、也没有回退版本的现场。
                    装在跑着重要会话的机器上时，请确认你能接受这次重启。
                  </span>
                </AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel>取消</AlertDialogCancel>
                <AlertDialogAction
                  onClick={() => {
                    setConfirmAutoInstall(false);
                    void handleSwitch("install", true);
                  }}
                >
                  打开自动安装
                </AlertDialogAction>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>

          <CardHint>
            自动检查、自动下载、自动安装三项默认全开：发现新版本即下载，下好即静默安装并重启，
            全程不需要任何选择。安装会覆盖程序、结束正在跑的进程；不想让它自动装的话，
            关掉上面的开关即可（那就只下不装，安装那一下由你自己动手）。
          </CardHint>
        </CardContent>
      </Section>
    </div>
  );
}
