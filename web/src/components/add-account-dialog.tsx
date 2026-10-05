import { useEffect, useState } from "react";
import {
  AlertCircle,
  Check,
  Copy,
  ExternalLink,
  Globe,
  Loader2,
  MessageCircle,
  RefreshCw,
} from "lucide-react";
import { QRCodeSVG } from "qrcode.react";
import {
  notifySuccess,
  notifyError,
} from "@/lib/notify";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Skeleton } from "@/components/ui/skeleton";
import { describeError, fetchLoginSites, isAbortError, openExternalUrl, pollLogin, startLogin } from "@/lib/api";
import type { AccountSite, LoginSite, LoginStartResponse } from "@/lib/types";
import { cn, copyToClipboard } from "@/lib/utils";

/** 轮询间隔：后端建议 1.5~2 秒。 */
const POLL_INTERVAL_MS = 2000;

type Phase = "sites" | "waiting" | "error";

const SITE_ICON: Record<AccountSite, typeof Globe> = {
  cn: MessageCircle,
  intl: Globe,
};

const STEPS = ["选择站点", "扫码授权", "完成"] as const;

export interface AddAccountDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** 登录成功、凭据已落盘并热加载后调用，用于刷新账号列表。 */
  onAdded: () => void;
}

/** 添加账号：选站点 → login/start 扫码 → 轮询 login/poll 直到成功或失败。 */
export function AddAccountDialog({ open, onOpenChange, onAdded }: AddAccountDialogProps) {
  const [phase, setPhase] = useState<Phase>("sites");
  const [sites, setSites] = useState<LoginSite[] | null>(null);
  const [sitesLoading, setSitesLoading] = useState(false);
  const [sitesError, setSitesError] = useState<string | null>(null);
  /** 站点拉取失败后的重试计数，仅用于强制重跑 effect。 */
  const [sitesReloadKey, setSitesReloadKey] = useState(0);
  const [starting, setStarting] = useState(false);
  const [login, setLogin] = useState<LoginStartResponse | null>(null);
  const [errorText, setErrorText] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  // 关闭时复位，保证下次打开从第一步开始。
  useEffect(() => {
    if (!open) {
      setPhase("sites");
      setLogin(null);
      setErrorText(null);
      setStarting(false);
      setCopied(false);
    }
  }, [open]);

  // 首次打开时拉取可选站点。
  useEffect(() => {
    if (!open || sites !== null) return;
    let cancelled = false;
    setSitesLoading(true);
    fetchLoginSites()
      .then((result) => {
        if (cancelled) return;
        setSites(result.sites ?? []);
        setSitesError(null);
      })
      .catch((error) => {
        if (!cancelled) setSitesError(describeError(error));
      })
      .finally(() => {
        if (!cancelled) setSitesLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, sites, sitesReloadKey]);

  // 轮询登录结果；关闭对话框 / 切换 phase 时清理定时器并中止在途请求。
  useEffect(() => {
    if (!open || phase !== "waiting" || !login) return;
    const controller = new AbortController();
    let cancelled = false;

    const tick = async () => {
      try {
        const result = await pollLogin(login.login_id, controller.signal);
        if (cancelled) return;

        if (result.status === "pending") return;
        if (result.status === "ok") {
          notifySuccess(`已添加账号：${result.account.nickname || result.account.file}`);
          onAdded();
          onOpenChange(false);
          return;
        }
        setErrorText(
          result.status === "expired"
            ? result.error || "登录已超时，请重新发起"
            : result.error || "登录失败，请重试",
        );
        setPhase("error");
      } catch (error) {
        if (cancelled || isAbortError(error)) return;
        setErrorText(describeError(error));
        setPhase("error");
      }
    };

    const timer = window.setInterval(() => void tick(), POLL_INTERVAL_MS);
    void tick();

    return () => {
      cancelled = true;
      window.clearInterval(timer);
      controller.abort();
    };
    // onAdded / onOpenChange 由父层提供，不参与依赖以免重启轮询。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, phase, login]);

  async function pickSite(site: AccountSite) {
    setStarting(true);
    setErrorText(null);
    try {
      const result = await startLogin(site);
      setLogin(result);
      setPhase("waiting");
    } catch (error) {
      setErrorText(describeError(error));
      setPhase("error");
    } finally {
      setStarting(false);
    }
  }

  async function copyAuthUrl() {
    if (!login) return;
    const ok = await copyToClipboard(login.auth_url);
    if (!ok) {
      notifyError("复制失败，请手动选择链接复制");
      return;
    }
    setCopied(true);
    notifySuccess("已复制授权链接");
    window.setTimeout(() => setCopied(false), 2000);
  }

  /**
   * 打开授权链接。
   *
   * 先试 `window.open`：普通浏览器里这是最自然的做法（新标签页，不打断面板）。
   * 但**桌面壳的 WebView 会静默拦掉它**（返回 null 而不报错）—— 那时退回网关，
   * 由网关在本机用系统默认浏览器打开。国际站的授权流程本来就必须在浏览器里完成，
   * 链接点不开等于这条路走不通。
   */
  async function openAuthUrl() {
    if (!login) return;
    try {
      const opened = window.open(login.auth_url, "_blank", "noopener,noreferrer");
      if (opened) return;
    } catch {
      // 被拦或抛错都落到下面的网关回退。
    }
    try {
      await openExternalUrl(login.auth_url);
    } catch (error) {
      notifyError(`无法自动打开浏览器，请点「复制」后手动打开：${describeError(error)}`);
    }
  }

  function restart() {
    setLogin(null);
    setErrorText(null);
    setPhase("sites");
  }

  const stepIndex = phase === "waiting" ? 1 : phase === "error" ? 1 : 0;
  const expiresMinutes = login ? Math.max(1, Math.round(login.expires_in / 60)) : 0;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          {/* 文案对齐 wb-switch 的「OAuth 扫码登录」；对端只做国内版，我们支持
              国内站 / 国际站两个入口，所以在副标题里说明，而不是在标题写死档位。 */}
          <DialogTitle>OAuth 扫码登录</DialogTitle>
          <DialogDescription>
            选择站点后扫码授权，凭据会写入网关并自动热加载进账号池。
          </DialogDescription>
        </DialogHeader>

        <ol className="flex items-center gap-2 text-xs" aria-label="添加账号步骤">
          {STEPS.map((step, index) => (
            <li key={step} className="flex min-w-0 items-center gap-2">
              <span
                className={cn(
                  "inline-flex size-5 shrink-0 items-center justify-center rounded-full text-xs font-medium",
                  index <= stepIndex
                    ? "bg-primary text-primary-foreground"
                    : "bg-muted text-muted-foreground",
                )}
              >
                {index < stepIndex ? <Check className="size-3" /> : index + 1}
              </span>
              <span
                className={cn(
                  "truncate",
                  index <= stepIndex ? "text-foreground" : "text-muted-foreground",
                )}
              >
                {step}
              </span>
              {index < STEPS.length - 1 ? (
                <span aria-hidden className="h-px w-4 shrink-0 bg-border" />
              ) : null}
            </li>
          ))}
        </ol>

        {phase === "sites" ? (
          <div className="space-y-2">
            {sitesLoading && sites === null ? (
              <>
                <Skeleton className="h-[68px] rounded-lg" />
                <Skeleton className="h-[68px] rounded-lg" />
              </>
            ) : sitesError && sites === null ? (
              <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-xs text-destructive">
                <div className="flex items-center gap-2 font-medium">
                  <AlertCircle className="size-3.5 shrink-0" />
                  读取站点失败
                </div>
                <p className="mt-1 leading-5 text-muted-foreground">{sitesError}</p>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="mt-2"
                  onClick={() => {
                    setSitesError(null);
                    setSites(null);
                    setSitesReloadKey((key) => key + 1);
                  }}
                >
                  <RefreshCw className="size-3.5" />
                  重试
                </Button>
              </div>
            ) : (sites ?? []).length === 0 ? (
              <p className="rounded-lg border border-border/60 bg-muted/40 p-3 text-xs text-muted-foreground">
                网关未提供任何可登录站点。
              </p>
            ) : (
              (sites ?? []).map((item) => {
                const Icon = SITE_ICON[item.site] ?? Globe;
                return (
                  <button
                    key={item.site}
                    type="button"
                    disabled={starting}
                    onClick={() => void pickSite(item.site)}
                    className={cn(
                      "flex w-full cursor-pointer items-center gap-3 rounded-lg border px-3.5 py-3 text-left outline-none transition-colors",
                      "hover:border-primary/50 hover:bg-accent/50 focus-visible:ring-2 focus-visible:ring-ring/50",
                      "disabled:cursor-not-allowed disabled:opacity-60",
                    )}
                  >
                    <span
                      aria-hidden
                      className="inline-flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/12 text-primary-ink"
                    >
                      {starting ? <Loader2 className="size-4 animate-spin" /> : <Icon className="size-4" />}
                    </span>
                    <span className="min-w-0 flex-1">
                      <span className="block text-sm font-medium">{item.label}</span>
                      <span className="mt-0.5 block truncate text-xs text-muted-foreground">
                        {item.hint}
                      </span>
                    </span>
                  </button>
                );
              })
            )}
          </div>
        ) : null}

        {phase === "waiting" && login ? (
          <div className="space-y-4">
            <div className="flex justify-center">
              <div className="rounded-xl border bg-white p-3 shadow-xs">
                <QRCodeSVG value={login.auth_url} size={176} level="M" marginSize={0} />
              </div>
            </div>

            <div className="flex items-center justify-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin text-primary-ink" aria-hidden="true" />
              等待授权中…
            </div>

            <div className="space-y-1.5">
              <div className="text-xs text-muted-foreground">
                {login.site === "intl"
                  ? // 国际站官方流程是「在浏览器里完成授权」（邮箱/验证码/SSO），不是 App 内扫码；
                    // 说成「用手机扫码」会让人以为扫完就自动加好，实际还要在页面里登录一次。
                    "国际站在浏览器中完成授权（手机扫码会打开手机浏览器，也可以点下面的按钮在本机浏览器打开）"
                  : "国内站用微信 / 企业微信扫码授权，或在浏览器中打开下面的链接"}
                {expiresMinutes > 0 ? `（约 ${expiresMinutes} 分钟内有效）` : ""}
              </div>
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  onClick={() => void openAuthUrl()}
                  className="min-w-0 flex-1 truncate text-left font-mono text-xs text-primary-ink underline-offset-4 hover:underline"
                  title={login.auth_url}
                >
                  {login.auth_url}
                </button>
                <Button type="button" variant="outline" size="sm" onClick={() => void copyAuthUrl()}>
                  {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
                  {copied ? "已复制" : "复制"}
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className="size-8"
                  onClick={() => void openAuthUrl()}
                  aria-label="在浏览器中打开"
                  title="在浏览器中打开"
                >
                  <ExternalLink className="size-3.5" />
                </Button>
              </div>
            </div>
          </div>
        ) : null}

        {phase === "error" ? (
          <div className="space-y-3">
            <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive">
              <div className="flex items-center gap-2 font-medium">
                <AlertCircle className="size-4 shrink-0" />
                未能完成登录
              </div>
              <p className="mt-1 text-xs leading-5 text-muted-foreground">
                {errorText || "未知错误"}
              </p>
            </div>
          </div>
        ) : null}

        <DialogFooter>
          {phase === "waiting" ? (
            <Button type="button" variant="outline" size="sm" onClick={() => onOpenChange(false)}>
              取消
            </Button>
          ) : null}
          {phase === "error" ? (
            <>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => onOpenChange(false)}
              >
                关闭
              </Button>
              <Button type="button" size="sm" onClick={restart}>
                <RefreshCw className="size-3.5" />
                重新发起
              </Button>
            </>
          ) : null}
          {phase === "sites" ? (
            <Button type="button" variant="ghost" size="sm" onClick={() => onOpenChange(false)}>
              取消
            </Button>
          ) : null}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
