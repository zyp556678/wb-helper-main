import { useCallback, useEffect, useMemo, useState } from "react";
import {
  AlertTriangle,
  Check,
  FileCode2,
  History,
  Loader2,
  MonitorSmartphone,
  Plug,
  RefreshCw,
  SquareCode,
  Terminal,
} from "lucide-react";
import {
  notifySuccess,
  notifyError,
} from "@/lib/notify";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  describeError,
  fetchLocalAppBackups,
  fetchLocalApps,
  restoreLocalAppBackup,
  switchLocalApp,
} from "@/lib/api";
import { allToolsDisabled, targetEnabled, useSupportedTools } from "@/lib/supported-tools";
import type {
  Account,
  LocalAppBackup,
  LocalAppTarget,
  LocalAppTargetID,
  LocalAppsResponse,
} from "@/lib/types";
import { cn } from "@/lib/utils";

/** 每个目标用哪个图标（纯装饰，不承载信息）。账号卡的工具入口复用同一份映射。 */
export const TARGET_ICON: Record<LocalAppTargetID, typeof Terminal> = {
  "codebuddy-cli": Terminal,
  "workbuddy-desktop": MonitorSmartphone,
  jetbrains: SquareCode,
  vscode: FileCode2,
  "codebuddy-ide": Plug,
};

/**
 * WorkBuddy 客户端的显示名 —— 按账号站点区分。
 *
 * 国内站与国际站是**两个独立的应用**：认证文件是 workbuddy-desktop.info 与
 * workbuddy-desktop-ai.info，数据目录是 ~/.workbuddy 与 ~/.workbuddy-ai，
 * 国际站客户端的正式名就是 WorkBuddy AI。面板上只写「WorkBuddy」会让
 * 国际站账号旁边的按钮看起来在写国内站的文件 —— 那是错的。
 */
export function workBuddyClientLabel(site?: string): string {
  return site === "intl" ? "WorkBuddy AI" : "WorkBuddy";
}

/** 账号卡工具入口用的短名（页脚按钮宽度紧，不能用完整 Label）。 */
export function localAppShortLabel(id: LocalAppTargetID, site?: string): string {
  switch (id) {
    case "codebuddy-cli":
      return "CLI";
    case "workbuddy-desktop":
      return workBuddyClientLabel(site);
    case "jetbrains":
      return "JetBrains";
    case "vscode":
      return "VS Code";
    case "codebuddy-ide":
      return "CodeBuddy IDE";
    default:
      return id;
  }
}

/**
 * 完整显示名（tooltip / 状态 chip 用）。
 *
 * 只有 WorkBuddy 客户端按站点换名；其余目标与站点无关，原样返回后端 Label。
 */
export function localAppTargetLabel(id: LocalAppTargetID, label: string, site?: string): string {
  return id === "workbuddy-desktop" ? `${workBuddyClientLabel(site)} 客户端` : label;
}

/**
 * 目标当前登录的账号 ID 列表。
 *
 * WorkBuddy 的国内站与国际站是两个独立文件，可各登录一个账号，
 * 后端在 `current_accounts` 里全部给出；旧后端只有单值的 `current_account`，
 * 这时退化为单元素列表（而不是什么都不显示）。
 */
export function targetCurrentAccountIds(t: LocalAppTarget): string[] {
  if (t.current_accounts && t.current_accounts.length > 0) return t.current_accounts;
  return t.current_account ? [t.current_account] : [];
}

/** 账号展示名（与面板同一口径）。 */
function accountName(a: Account): string {
  return a.nickname || a.uid || a.file || "未命名账号";
}

function accountSiteLabel(site: string): string {
  return site === "intl" ? "国际站" : "国内站";
}

function formatTime(unixSec: number): string {
  if (!unixSec) return "—";
  const d = new Date(unixSec * 1000);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/**
 * 「设为当前」对话框：选账号 →（WorkBuddy 还要选档位）→（必要时确认明文）→ 执行。
 *
 * **两个调用点共用**：本机应用卡片自己的「设为当前」，以及账号卡片上的
 * 工具入口（批次 4）。因此这里导出，并接受 `initialAccountId` 预选账号 ——
 * 账号卡上点某个工具的图标，意图就是「把**这张卡**的账号设为该工具的当前」。
 *
 * 执行完**把后端返回的逐条 notes 原样展示**而不是只说「成功」：
 * 用户需要看到「档位标记也改了」「哪些配置被保留」——
 * 这类信息在出问题时是唯一能自证的线索。
 */
export function SwitchDialog({
  target,
  accounts,
  open,
  onOpenChange,
  onDone,
  initialAccountId,
}: {
  target: LocalAppTarget | null;
  accounts: Account[];
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onDone: () => void;
  /** 打开时预选的账号（账号卡入口用）；缺省保持空选择。 */
  initialAccountId?: string;
}) {
  const [picked, setPicked] = useState<string>("");
  const [site, setSite] = useState<string>("");
  const [allowPlaintext, setAllowPlaintext] = useState(false);
  const [busy, setBusy] = useState(false);
  const [notes, setNotes] = useState<string[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) {
      setPicked("");
      setSite("");
      setAllowPlaintext(false);
      setNotes(null);
      setError(null);
      setBusy(false);
      return;
    }
    // 打开时把账号卡带来的预选值写进草稿；档位仍按账号自动带出（下面算）。
    setPicked(initialAccountId ?? "");
    setSite("");
    setAllowPlaintext(false);
    setNotes(null);
    setError(null);
    setBusy(false);
  }, [open, initialAccountId]);

  // WorkBuddy 的国内站与国际站是两个独立文件，必须明确写哪一个。
  const needsSite = target?.id === "workbuddy-desktop";
  const plaintextRisk = Boolean(target?.warning && target.warning.includes("明文"));

  const pickedAccount = accounts.find((a) => a.id === picked) ?? null;
  // 选账号后自动带出它的站点，减少一次选择；用户仍可改。
  const effectiveSite = site || pickedAccount?.site || "";

  // 「当前」标记：WorkBuddy 的国内站与国际站是两个独立登录态，
  // 只标**所选档位**那一个 —— 否则在国内站档位下把国际站账号标成「当前」会误导
  //（它当前登录的是另一个客户端，不是这次要写的这份文件）。
  const currentIds = target ? targetCurrentAccountIds(target) : [];
  const isCurrentAccount = (a: Account) =>
    currentIds.includes(a.id) &&
    (target?.id !== "workbuddy-desktop" || !effectiveSite || a.site === effectiveSite);

  const submit = useCallback(async () => {
    if (!target || !picked) {
      notifyError("请先选择一个账号");
      return;
    }
    if (needsSite && effectiveSite !== "cn" && effectiveSite !== "intl") {
      notifyError("请选择要写入的档位（国内站 / 国际站）");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const res = await switchLocalApp({
        target: target.id,
        accountId: picked,
        site: needsSite ? effectiveSite : "",
        allowPlaintext,
      });
      setNotes(res.notes ?? []);
      onDone();
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy(false);
    }
  }, [target, picked, needsSite, effectiveSite, allowPlaintext, onDone]);

  if (!target) return null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <span className="shrink-0 text-muted-foreground">
              {(() => {
                const Icon = TARGET_ICON[target.id];
                return <Icon className="size-4" aria-hidden="true" />;
              })()}
            </span>
            把账号设为{" "}
            {needsSite ? `${workBuddyClientLabel(effectiveSite)} 客户端` : target.label} 的当前登录
          </DialogTitle>
          <DialogDescription>
            {needsSite
              ? // WorkBuddy 客户端必须在写入前关闭：它退出时会把内存里的登录态回写认证文件，
                // 不关就写会被它覆盖（现象是「切了又变回去」）。这一点要提前说清楚，
                // 否则用户会以为面板擅自关掉了他的客户端。
                "会把所选账号的凭据写入该应用的登录态文件。切换时会先关闭客户端，写入后再重新打开（原本没运行则不会替你启动）；写入前会自动备份，随时可恢复。"
              : "会把所选账号的凭据写入该应用的登录态文件。写入前会自动备份，随时可恢复。"}
          </DialogDescription>
        </DialogHeader>

        {notes ? (
          <div className="space-y-2">
            <p className="flex items-center gap-1.5 text-sm font-medium text-foreground">
              <Check className="size-4 shrink-0 text-primary-ink" aria-hidden="true" />
              已完成
            </p>
            <ul className="space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2.5">
              {notes.map((n, i) => (
                <li key={i} className="text-xs leading-5 text-muted-foreground">
                  · {n}
                </li>
              ))}
            </ul>
          </div>
        ) : (
          <>
            {target.warning ? (
              <div className="flex items-start gap-2 rounded-lg border border-amber-500/40 bg-amber-500/8 px-3 py-2.5 text-xs">
                <AlertTriangle
                  className="mt-0.5 size-3.5 shrink-0 text-amber-700 dark:text-amber-300"
                  aria-hidden="true"
                />
                <span className="min-w-0 leading-5">{target.warning}</span>
              </div>
            ) : null}

            {needsSite ? (
              <div className="space-y-1.5">
                <p className="text-xs font-medium text-muted-foreground">写入哪个档位</p>
                <div className="flex gap-1.5">
                  {(["cn", "intl"] as const).map((s) => (
                    <button
                      key={s}
                      type="button"
                      onClick={() => setSite(s)}
                      aria-pressed={effectiveSite === s}
                      className={cn(
                        "rounded-full border px-3 py-1 text-xs transition-colors",
                        effectiveSite === s
                          ? "border-primary bg-primary/12 font-medium text-primary-ink"
                          : "border-border text-muted-foreground hover:bg-accent",
                      )}
                    >
                      {s === "intl" ? "国际站（WorkBuddy AI）" : "国内站（WorkBuddy）"}
                    </button>
                  ))}
                </div>
                <p className="text-xs text-muted-foreground">
                  国内站与国际站在客户端里是两个独立的应用（WorkBuddy / WorkBuddy AI），
                  登录态文件互不影响。
                </p>
              </div>
            ) : null}

            <div className="space-y-1.5">
              <p className="text-xs font-medium text-muted-foreground">选择账号</p>
              <ul className="max-h-[38vh] space-y-1.5 overflow-y-auto pr-1">
                {accounts.map((a) => (
                  <li key={a.id}>
                    <label
                      className={cn(
                        "flex cursor-pointer items-center gap-2.5 rounded-lg border px-3 py-2",
                        picked === a.id ? "border-primary bg-primary/6" : "border-border",
                      )}
                    >
                      <input
                        type="radio"
                        name="local-app-account"
                        className="size-3.5 shrink-0 accent-[var(--primary)]"
                        checked={picked === a.id}
                        onChange={() => setPicked(a.id)}
                      />
                      <span className="min-w-0 flex-1">
                        <span className="flex flex-wrap items-center gap-x-2">
                          <span className="min-w-0 truncate text-sm">{accountName(a)}</span>
                          <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
                            {accountSiteLabel(a.site)}
                          </span>
                          {isCurrentAccount(a) ? (
                            <span className="shrink-0 rounded bg-primary/12 px-1.5 py-0.5 text-xs font-medium text-primary-ink">
                              当前
                            </span>
                          ) : null}
                        </span>
                        <span className="mt-0.5 block truncate font-mono text-xs text-muted-foreground">
                          {a.file}
                        </span>
                      </span>
                    </label>
                  </li>
                ))}
              </ul>
            </div>

            {plaintextRisk ? (
              <label className="flex items-start gap-2.5 rounded-lg border border-amber-500/40 bg-amber-500/8 px-3 py-2.5 text-xs">
                <Checkbox
                  checked={allowPlaintext}
                  disabled={busy}
                  onCheckedChange={(v) => setAllowPlaintext(v === true)}
                  className="mt-0.5 data-[state=checked]:border-primary data-[state=checked]:bg-primary"
                  aria-label="允许写入明文凭据"
                />
                <span className="min-w-0 leading-5">
                  <span className="font-medium">允许写入明文凭据</span>
                  <span className="mt-0.5 block text-muted-foreground">
                    目标账号若不在客户端已有的账号列表里，我们只能写入明文凭据
                    （客户端自己用的是加密信封，而我们拿不到它的密钥）。
                    客户端能否接受明文无法离线确认，切换后需重启客户端验证；失败可用备份恢复。
                  </span>
                </span>
              </label>
            ) : null}

            {error ? (
              <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2.5 text-xs text-destructive">
                <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
                <span className="min-w-0 leading-5">{error}</span>
              </div>
            ) : null}
          </>
        )}

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={() => onOpenChange(false)}
          >
            {notes ? "关闭" : "取消"}
          </Button>
          {notes ? null : (
            <Button
              type="button"
              size="sm"
              disabled={busy || !picked || (needsSite && !effectiveSite)}
              onClick={() => void submit()}
            >
              {busy ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : null}
              {busy ? "写入中…" : "设为当前"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export interface LocalAppsCardProps {
  accounts: Account[];
  /**
   * 把「账号 ID → 它当前被哪些本机应用使用」上报给页面。
   *
   * 由本组件算而不是让页面各算一遍：当前账号的判定依据（targets[].current_account）
   * 就在这里，再拉一次接口会有两份可能不一致的数据。
   */
  onRolesChange?: (roles: Map<string, string[]>) => void;
  /** 上报各目标的探测结果（含当前账号），供账号卡上的工具入口复用同一份数据。 */
  onTargetsChange?: (targets: LocalAppTarget[]) => void;
  /** 父层在别处（账号卡）完成切换后自增，触发重新探测。 */
  refreshKey?: number;
}

/**
 * 本机应用接入。
 *
 * 为什么这件事能做：网关是**本机进程**，读写本机文件是它的本来能力
 *（凭据文件、本机代理、开机自启都已经在这么做）。面板拿不到 Tauri IPC，
 * 但只要能调 /panel/api/* 就够了 —— 真正的本机操作在网关侧完成。
 */
export function LocalAppsCard({ accounts, onRolesChange, onTargetsChange, refreshKey = 0 }: LocalAppsCardProps) {
  // 「支持工具」开关：关闭的工具不渲染对应行，也不参与「当前登录」标记与
  // 账号卡入口的分发；五个都关时连探测请求都不发（能省一次本机探测就省）。
  const tools = useSupportedTools();
  const allOff = allToolsDisabled(tools);
  // 后端已保证列表字段是 [] 而不是 null（Go 的 nil slice 会序列化成 null），
  // 这里仍然兜一层：一个字段是 null 就让**整页白屏**，代价与收益完全不成比例。
  const [data, setData] = useState<LocalAppsResponse | null>(null);
  const [backups, setBackups] = useState<LocalAppBackup[]>([]);
  const [loading, setLoading] = useState(true);
  const [dialogTarget, setDialogTarget] = useState<LocalAppTarget | null>(null);
  const [restoring, setRestoring] = useState<string | null>(null);

  /**
   * 「哪些账号是本机某应用的当前账号」——账号卡据此显示绿色「当前」标记。
   *
   * **刻意在 render 里派生（useMemo），而不是在 load() 里算一次**：
   * `accounts`（账号池）与 `data`（本机探测）是两个独立到达的数据源，谁先到都有可能。
   * 在 load() 里算会把「账号还没到」那一刻的空表固化下来，而且 load 不会因
   * accounts 变化重跑 —— 结果是国际站账号的「当前登录」显示成国内站客户端名
   *（站点查不到 → 退回默认名）。派生值则会在两者都就绪后自动重算。
   *
   * 只看启用中的工具：关掉的工具不该在账号卡上留标记。
   */
  const roles = useMemo(() => {
    const out = new Map<string, string[]>();
    const siteById = new Map(accounts.map((a) => [a.id, a.site]));
    const enabled = (data?.targets ?? []).filter((t) => targetEnabled(tools, t.id));
    for (const t of enabled) {
      // 一个目标可能对应多个当前账号（WorkBuddy 的国内站/国际站各一个），
      // 每个都按它自己的站点取显示名。
      for (const accountId of targetCurrentAccountIds(t)) {
        const list = out.get(accountId) ?? [];
        list.push(localAppTargetLabel(t.id, t.label, siteById.get(accountId)));
        out.set(accountId, list);
      }
    }
    return out;
  }, [accounts, data, tools]);

  useEffect(() => {
    onRolesChange?.(roles);
  }, [roles, onRolesChange]);

  const load = useCallback(async (signal?: AbortSignal) => {
    if (allOff) {
      // 全部关闭：不发请求，也不上报任何目标（账号卡因此不会出现工具入口）。
      setData(null);
      setBackups([]);
      onTargetsChange?.([]);
      setLoading(false);
      return;
    }
    try {
      const [apps, bk] = await Promise.all([fetchLocalApps(signal), fetchLocalAppBackups(signal)]);
      setData(apps);
      setBackups(bk.backups ?? []);
      const enabledTargets = (apps.targets ?? []).filter((t) => targetEnabled(tools, t.id));
      onTargetsChange?.(enabledTargets);
    } catch {
      // 静默：本机接入是加分项，失败不该在账号页弹错误。
    } finally {
      setLoading(false);
    }
  }, [onTargetsChange, allOff, tools]);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
    // refreshKey 由父层在账号卡完成切换后自增：必须重新探测，
    // 否则账号卡上的「当前」标记会停留在切换前。
  }, [load, refreshKey]);

  const handleRestore = useCallback(
    async (id: string) => {
      setRestoring(id);
      try {
        const res = await restoreLocalAppBackup(id);
        notifySuccess(`已恢复（${(res.notes ?? []).length} 个文件）`);
        void load();
      } catch (err) {
        notifyError(describeError(err));
      } finally {
        setRestoring(null);
      }
    },
    [load],
  );

  // 只渲染启用中的工具；关闭的条目不出现（账号卡入口同样按这份列表分发）。
  const targets = (data?.targets ?? []).filter((t) => targetEnabled(tools, t.id));
  const conflict = data?.switch_conflict;

  return (
    <Card className="gap-0 overflow-hidden rounded-2xl py-0 shadow-none">
      <header className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-3">
        <h2 className="text-sm font-semibold">本机应用接入</h2>
        <p className="min-w-0 flex-1 text-xs leading-5 text-muted-foreground">
          把账号池里的账号写入本机应用的登录态（CLI / 客户端 / IDE 插件）。每次写入前自动备份。
        </p>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          aria-label="重新探测"
          disabled={loading}
          onClick={() => void load()}
        >
          <RefreshCw className={cn("size-3.5", loading && "animate-spin")} aria-hidden="true" />
          重新探测
        </Button>
      </header>

      {conflict?.present ? (
        <div className="flex items-start gap-2 border-b border-amber-500/40 bg-amber-500/8 px-4 py-3 text-xs">
          <AlertTriangle
            className="mt-0.5 size-3.5 shrink-0 text-amber-700 dark:text-amber-300"
            aria-hidden="true"
          />
          <div className="min-w-0 leading-5">
            <p className="font-medium text-foreground">
              检测到 wb-switch 与本工具争抢同一批文件
              {conflict.hook_active ? "（它的 hook 仍在生效）" : ""}
            </p>
            <p className="mt-0.5 text-muted-foreground">{conflict.detail}</p>
            {conflict.contended.length > 0 ? (
              <ul className="mt-1 space-y-0.5">
                {conflict.contended.map((f) => (
                  <li key={f} className="truncate font-mono text-xs text-muted-foreground" title={f}>
                    {f}
                  </li>
                ))}
              </ul>
            ) : null}
          </div>
        </div>
      ) : null}

      <ul className="divide-y divide-border">
        {targets.map((t) => {
          const Icon = TARGET_ICON[t.id];
          const targetBackups = backups.filter((b) => b.target === t.id);
          return (
            <li key={t.id} className="flex flex-wrap items-start gap-3 px-4 py-3">
              <span className="mt-0.5 shrink-0 text-muted-foreground">
                <Icon className="size-4" aria-hidden="true" />
              </span>

              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                  <span className="text-sm font-medium">
                    {t.id === "workbuddy-desktop" ? "WorkBuddy 客户端 / WorkBuddy AI" : t.label}
                  </span>
                  {!t.installed ? (
                    <Badge variant="secondary">未检测到</Badge>
                  ) : t.writable ? (
                    <Badge variant="success">已接入</Badge>
                  ) : (
                    <Badge variant="warning">未实现写入</Badge>
                  )}
                  {(t.all_accounts ?? []).length > 0 ? (
                    <span className="text-xs text-muted-foreground">
                      客户端内有 {(t.all_accounts ?? []).length} 个账号
                    </span>
                  ) : null}
                </div>

                {t.installed ? (
                  <p className="mt-1 text-xs text-muted-foreground">
                    当前：
                    <span className="font-medium text-foreground">
                      {t.current_account ? t.current_label : t.current_label || "未识别"}
                    </span>
                    {t.current_uid && !t.current_account ? (
                      <span className="ml-1 font-mono">（{t.current_uid.slice(0, 8)}… 不在账号池中）</span>
                    ) : null}
                  </p>
                ) : null}

                {t.note ? (
                  <p className="mt-1 text-xs leading-5 text-muted-foreground">{t.note}</p>
                ) : null}
                {(t.blockers ?? []).map((b, i) => (
                  <p key={i} className="mt-1 text-xs leading-5 text-amber-700 dark:text-amber-300">
                    {b}
                  </p>
                ))}
              </div>

              <div className="flex shrink-0 flex-wrap items-center gap-1.5">
                {targetBackups.length > 0 ? (
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    disabled={restoring !== null}
                    onClick={() => void handleRestore(targetBackups[0].id)}
                    title={`恢复 ${formatTime(targetBackups[0].created_at)} 的备份`}
                  >
                    {restoring === targetBackups[0].id ? (
                      <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                    ) : (
                      <History className="size-3.5" aria-hidden="true" />
                    )}
                    恢复备份
                  </Button>
                ) : null}
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={!t.writable || accounts.length === 0}
                  title={
                    !t.writable
                      ? t.blockers[0] || "尚未实现写入"
                      : accounts.length === 0
                        ? "账号池为空"
                        : `把某个账号设为 ${t.label} 的当前登录`
                  }
                  onClick={() => setDialogTarget(t)}
                >
                  设为当前
                </Button>
              </div>
            </li>
          );
        })}
        {!loading && targets.length === 0 ? (
          <li className="px-4 py-6 text-center text-sm text-muted-foreground">
            {allOff
              ? "所有支持工具已在「配置 → 支持工具」关闭，本页不再探测本机应用。"
              : "未能探测本机应用（后端可能未就绪）"}
          </li>
        ) : null}
      </ul>

      <SwitchDialog
        target={dialogTarget}
        accounts={accounts}
        open={dialogTarget !== null}
        onOpenChange={(v) => {
          if (!v) setDialogTarget(null);
        }}
        onDone={() => void load()}
      />
    </Card>
  );
}
