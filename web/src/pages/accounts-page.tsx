import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  CalendarCheck,
  FileDown,
  FileUp,
  Loader2,
  Maximize2,
  Minimize2,
  QrCode,
  RefreshCw,
} from "lucide-react";
import {
  notifySuccess,
  notifyError,
  notifyWarning,
  notifyInfo,
} from "@/lib/notify";

import { AccountCard } from "@/components/account-card";
import { AccountInfoDialog } from "@/components/account-info-dialog";
import { AddAccountDialog } from "@/components/add-account-dialog";
import { AccountSortToggle } from "@/components/account-sort-toggle";
import { ApiKeyPanel } from "@/components/api-key-panel";
import { ExportAccountsDialog } from "@/components/export-accounts-dialog";
import { ExpiryReminderCard } from "@/components/expiry-reminder-card";
import { ImportAccountsDialog } from "@/components/import-accounts-dialog";
import { LocalAppsCard, SwitchDialog } from "@/components/local-apps-card";
import { LocalSessionsCard } from "@/components/local-sessions-card";
import { PillGroup } from "@/components/section";
import { StatsBar } from "@/components/stats-bar";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  checkinAccount,
  checkinAllAccounts,
  describeError,
  fetchCheckinStatus,
  fetchCredits,
  refreshAccountQuota,
  activateIntlAccount,
  identifyAccountPlan,
  refreshAccountToken,
  refreshAllQuota,
  removeAccount,
  reviveAccount,
  setAccountDisabled,
} from "@/lib/api";
import { nowSeconds, sortAccounts, toCreditPackageViews, type AccountSort } from "@/lib/format";
import { creditResourceName } from "@/lib/credit-package-names";
import { useSupportedTools } from "@/lib/supported-tools";
import { useVisibleInterval } from "@/lib/use-visible-interval";
import type {
  Account,
  AccountSite,
  CheckinStatusView,
  CreditsResponse,
  LocalAppTarget,
  PanelCreditAccount,
} from "@/lib/types";
import type { PageId } from "@/lib/navigation";
import type { PanelData } from "@/lib/use-panel-data";
import { cn } from "@/lib/utils";

/** 紧凑模式的 localStorage 键。 */
const COMPACT_KEY = "workbuddy-gateway.accounts_compact";

/** 读紧凑偏好。localStorage 在某些隐私模式下会抛，所以整段兜住。 */
function readCompactPref(): boolean {
  try {
    return localStorage.getItem(COMPACT_KEY) === "1";
  } catch {
    return false;
  }
}

function writeCompactPref(value: boolean) {
  try {
    localStorage.setItem(COMPACT_KEY, value ? "1" : "0");
  } catch {
    // 存不了就只在本次会话生效，不值得打扰用户。
  }
}

export interface AccountsPageProps {
  data: PanelData;
  /** 侧栏「配置访问密钥」打开的面板状态（本页不再自带入口，避免两处入口重复）。 */
  keyPanelOpen: boolean;
  onKeyPanelOpenChange: (open: boolean) => void;
  /** 跳转到其他页（卡片上的「查看全部积分包」用）。 */
  onNavigate?: (page: PageId) => void;
}

export function AccountsPage({
  data,
  keyPanelOpen,
  onKeyPanelOpenChange,
  onNavigate,
}: AccountsPageProps) {
  const { overview, accounts, loading, error, unauthorized, reload, refresh } = data;
  const now = nowSeconds();
  // 模型限额提醒的保鲜：卡片上的倒计时靠本地时钟递减，但**新出现**的限流
  // 只能靠重新拉账号列表。按 wb-switch 的节奏 5 分钟静默刷新一次
  //（不翻 loading，不闪骨架屏；页面隐藏时由 useVisibleInterval 暂停）。
  useVisibleInterval(() => refresh(), 5 * 60_000);
  // 「支持工具」：WorkBuddy 关闭时隐藏本机会话卡（它只服务 WorkBuddy 客户端）。
  const tools = useSupportedTools();
  const [sort, setSort] = useState<AccountSort>("expiry");
  const [addOpen, setAddOpen] = useState(false);
  const [importOpen, setImportOpen] = useState(false);
  const [exportOpen, setExportOpen] = useState(false);
  const [refreshingAll, setRefreshingAll] = useState(false);
  /** 批量「刷新全部账号积分并签到」进行中（与「刷新积分」互不阻塞，但各自防重入）。 */
  const [checkinAllRunning, setCheckinAllRunning] = useState(false);
  /** 档位筛选（全部 / 国内版 / 国际版）：纯前端过滤卡片，不改变任何请求。 */
  const [variant, setVariant] = useState<"all" | AccountSite>("all");
  /** 「账号信息」弹窗目标账号。 */
  const [infoTarget, setInfoTarget] = useState<Account | null>(null);
  /**
   * 本机各目标的探测结果（由 LocalAppsCard 上报）。
   *
   * 账号卡上的工具入口（当前 / 可切换）与「本机应用接入」卡片共用这**同一份数据**：
   * 页面不在这里再拉一次接口，避免两份可能不一致的「当前账号」。
   */
  const [localTargets, setLocalTargets] = useState<LocalAppTarget[]>([]);
  /** 账号卡发起的本机切换请求（复用本机页的 SwitchDialog，预选该卡账号）。 */
  const [switchRequest, setSwitchRequest] = useState<{
    target: LocalAppTarget;
    accountId: string;
  } | null>(null);
  /** 账号卡完成切换后自增，触发 LocalAppsCard 重新探测（「当前」标记要跟上）。 */
  const [localAppsRefreshKey, setLocalAppsRefreshKey] = useState(0);
  /**
   * 紧凑 / 宽松模式。纯展示偏好，存 localStorage（与主题同类的东西，
   * 不该占用后端配置——它不影响任何一次请求的行为）。
   */
  const [compact, setCompact] = useState(() => readCompactPref());

  /**
   * 逐包明细（账号卡片上的「近期到期」用）。
   *
   * 为什么单独拉一次而不是并进 usePanelData：这个接口要给每个账号各打一次上游，
   * 比账号列表慢得多（实测数秒）。并进主数据源会让整页被它拖住 ——
   * 积分明细缺失顶多是卡片少一块，而账号列表读不出来整页就是空的。
   * 所以它独立、非阻塞、失败静默（`creditsError` 只用于卡片内的降级提示）。
   */
  const [credits, setCredits] = useState<Map<string, PanelCreditAccount>>(new Map());
  const [creditsLoaded, setCreditsLoaded] = useState(false);
  /** 整轮明细拉取失败的原因；到期提醒卡用它显示「查询失败：…」（单账号失败在行内展示）。 */
  const [creditsError, setCreditsError] = useState<string | null>(null);
  // 并行拉取时后发先至会让旧结果覆盖新的，用序号丢弃过期响应。
  const creditsSeq = useRef(0);

  const loadCredits = useCallback(async (signal?: AbortSignal) => {
    const seq = ++creditsSeq.current;
    try {
      const res: CreditsResponse = await fetchCredits(signal);
      if (seq !== creditsSeq.current) return;
      const map = new Map<string, PanelCreditAccount>();
      for (const item of res.accounts ?? []) {
        // id 就是凭据文件名（后端 Credential.AccountID），与账号卡片的 account.id 同源。
        map.set(item.id, item);
      }
      setCredits(map);
      setCreditsError(null);
      setCreditsLoaded(true);
    } catch (err) {
      // 静默：明细是加分项，失败不该在账号页弹错误（积分统计页有完整的报错展示）。
      // 但把原因存下来给到期提醒卡显示 —— 那一块全是逐账号结果，整体失败要说清。
      if (seq === creditsSeq.current) {
        setCreditsError(describeError(err));
        setCreditsLoaded(true);
      }
    }
  }, []);

  // 账号集合变化时重新拉明细（新增 / 移除账号后键要对得上）。
  const accountIds = useMemo(() => accounts.map((a) => a.id).join("|"), [accounts]);
  useEffect(() => {
    if (accountIds === "") return;
    const controller = new AbortController();
    void loadCredits(controller.signal);
    return () => controller.abort();
  }, [accountIds, loadCredits]);

  /**
   * 今日签到状态（GET... 实为批量 POST /panel/api/accounts/checkin-status）。
   *
   * 与积分明细分开、且**不阻塞**：签到状态是「点签到之前看一眼」的辅助信息，
   * 拿不到时卡片少一行而已（见卡片里的 showCheckin 口径）。
   * 服务端有 5 分钟 TTL 缓存 + 并发限流，所以来回切页面不会反复打上游。
   */
  const [checkins, setCheckins] = useState<Map<string, CheckinStatusView>>(new Map());
  /**
   * 账号 ID → 它当前是本机哪些应用的登录账号。
   *
   * 由「本机应用接入」卡片探测后上报（它已经拿到了这份数据），
   * 页面只是转发给账号卡 —— 不在这里再拉一次接口，避免两份可能不一致的数据。
   */
  const [localRoles, setLocalRoles] = useState<Map<string, string[]>>(new Map());
  const checkinSeq = useRef(0);

  const loadCheckins = useCallback(async (signal?: AbortSignal) => {
    const seq = ++checkinSeq.current;
    try {
      const res = await fetchCheckinStatus(signal);
      if (seq !== checkinSeq.current) return;
      setCheckins(new Map(Object.entries(res.accounts ?? {})));
    } catch {
      // 静默：状态是加分项，查不到就不显示，不去打扰用户
      //（真正操作签到时后端会给出权威结论）。
    }
  }, []);

  useEffect(() => {
    if (accountIds === "") return;
    const controller = new AbortController();
    void loadCheckins(controller.signal);
    return () => controller.abort();
  }, [accountIds, loadCheckins]);

  /** 当前档位筛选下的账号（「全部」= 不过滤）。统计条与「建议优先」仍按全量口径。 */
  const visibleAccounts = useMemo(
    () => (variant === "all" ? accounts : accounts.filter((a) => a.site === variant)),
    [accounts, variant],
  );

  const sortedAccounts = useMemo(
    () => sortAccounts(visibleAccounts, sort, now),
    // now 每秒才变化一次，作为依赖足以在跨秒后重排。
    [visibleAccounts, sort, now],
  );

  /**
   * 到期提醒卡用的账号顺序：**全量账号**（不跟随档位筛选），排序跟随列表。
   *
   * 与统计条、「建议优先」同一约定 —— 档位筛选只过滤卡片，跨账号的风险提示
   * 不应因为筛到某一档就少看一半账号；排序则跟随列表，方便与下方卡片对号。
   */
  const sortedAllAccounts = useMemo(() => sortAccounts(accounts, sort, now), [accounts, sort, now]);

  /**
   * 「建议优先使用」：标出**积分最早到期**的那个账号。
   *
   * 判据必须看积分到期而不是 token 到期 —— token 到期只决定「还能不能用」，
   * 积分到期是「不用就没了」。这也是卡片上把「Token 有效期」换成
   * 「积分有效期区间」的同一条理由。
   *
   * 只在明细加载完成后才判定：明细没到就没有到期日可比较，
   * 这时标一个「建议优先」等于凭空指一个账号。同理，一个已用完所有积分的
   * 账号不该被标（没有东西会过期）。
   *
   * **遍历的是 sortedAccounts 而不是 accounts**：到期时间精确到分钟，实测本机就
   * 有两个账号并列在同一分钟。若按池内顺序（≈文件名序）取第一个，标记可能落在
   * 视觉上并不靠前的那张卡上 —— 用户看到「第二张被标了建议」只会困惑。
   * 按展示顺序遍历，并列时标记天然落在最靠前的那张卡。
   */
  const recommendedId = useMemo(() => {
    if (!creditsLoaded) return null;
    let best: { id: string; at: number } | null = null;
    for (const account of sortedAccounts) {
      const views = toCreditPackageViews(credits.get(account.id)?.resources, creditResourceName);
      // 与卡片同一口径：只看还有余额、且带明确到期日、且尚未过期的包。
      const live = views.filter((p) => p.remaining > 0 && p.expireAt > 0 && !p.expired);
      if (live.length === 0) continue;
      const at = Math.min(...live.map((p) => p.expireAt));
      if (!best || at < best.at) best = { id: account.id, at };
    }
    return best?.id ?? null;
  }, [sortedAccounts, credits, creditsLoaded]);

  /** 单账号写操作：成功 toast + 刷新，失败把后端 message 原样带出。 */
  const handleRefreshQuota = useCallback(
    async (id: string) => {
      try {
        await refreshAccountQuota(id);
        notifySuccess("积分已刷新");
        reload();
        // 同 handleRefreshAll：单账号刷新也会改余额，明细要跟着更新。
        void loadCredits();
      } catch (err) {
        notifyError(describeError(err));
      }
    },
    [reload, loadCredits],
  );

  const handleSetDisabled = useCallback(
    async (id: string, disabled: boolean) => {
      try {
        await setAccountDisabled(id, disabled);
        notifySuccess(disabled ? "账号已禁用" : "账号已启用");
        reload();
      } catch (err) {
        notifyError(describeError(err));
      }
    },
    [reload],
  );

  const handleRevive = useCallback(
    async (id: string) => {
      try {
        await reviveAccount(id);
        notifySuccess("账号已复活（已清除禁用、冷却、熔断与连败降权）");
        reload();
      } catch (err) {
        notifyError(describeError(err));
      }
    },
    [reload],
  );

  const handleRemove = useCallback(
    async (id: string, deleteFile: boolean) => {
      try {
        const result = await removeAccount(id, deleteFile);
        notifySuccess(
          deleteFile
            ? `已删除账号 ${result.removed}，凭据文件已彻底删除`
            : `已删除账号 ${result.removed}（凭据已归档，可随时恢复）`,
        );
        reload();
      } catch (err) {
        notifyError(describeError(err));
      }
    },
    [reload],
  );

  /** 强制刷新登录令牌（无条件刷，不判断是否临近过期）。 */
  /**
   * 国际站账号：注册激活（补注册地区）+ 领 trial 加油包。
   *
   * 失败也是「这次没做成」而不是请求异常 —— 按 ok 分流，把后端给的理由展示出来
   * （例如上游维护），而不是笼统地报「失败」。
   */
  const handleActivateIntl = useCallback(
    async (id: string) => {
      try {
        const res = await activateIntlAccount(id);
        if (res.ok === false) notifyError(res.detail || "注册激活失败");
        else notifySuccess(res.detail || "国际站账号已激活");
        reload();
      } catch (err) {
        notifyError(describeError(err));
      }
    },
    [reload],
  );

  /**
   * 完整识别套餐：查订阅权益列表，回答「哪一档、还有效吗」。
   *
   * 与「刷新额度」分开：额度刷新给的是摘要级套餐名（有编码、无有效期），
   * 这里多打一次分页查询，所以是显式的用户动作而不是自动行为。
   */
  const handleIdentifyPlan = useCallback(
    async (id: string) => {
      try {
        const res = await identifyAccountPlan(id);
        if (res.ok === false) notifyError(res.detail || "识别套餐失败");
        else notifySuccess(`套餐：${res.plan}${res.detail ? "（" + res.detail + "）" : ""}`);
        reload();
      } catch (err) {
        notifyError(describeError(err));
      }
    },
    [reload],
  );

  const handleRefreshToken = useCallback(
    async (id: string) => {
      try {
        await refreshAccountToken(id);
        notifySuccess("Token 已刷新");
        reload();
      } catch (err) {
        notifyError(describeError(err));
      }
    },
    [reload],
  );

  /**
   * 手动签到。
   *
   * 「今天已签到」走的是成功分支（后端把它识别为幂等成功），
   * 不要因为文案里带「已」就当失败 —— 上游每天第二次签到返回的就是这个。
   */
  const handleCheckin = useCallback(
    async (id: string) => {
      try {
        const result = await checkinAccount(id);
        notifySuccess(result.detail || (result.already ? "今天已签到" : "签到成功"));
        reload();
        // 签到会改余额，明细要跟着更新。
        void loadCredits();
        // 服务端已在签到成功时让状态缓存失效，这里重拉就会拿到新状态
        //（不重拉的话卡片会显示「今日未签到」直到下次进页面）。
        void loadCheckins();
      } catch (err) {
        notifyError(describeError(err));
      }
    },
    [reload, loadCredits, loadCheckins],
  );

  const handleRefreshAll = useCallback(async () => {
    setRefreshingAll(true);
    try {
      const result = await refreshAllQuota();
      if (result.failed > 0) {
        notifyWarning(`已刷新 ${result.refreshed} 个账号，${result.failed} 个失败`);
      } else {
        notifySuccess(`已刷新 ${result.refreshed} 个账号的积分`);
      }
      reload();
      // 额度和逐包明细是两个接口，「刷新积分」要一起更新，否则卡片上会出现
      // 「总数变了但明细还是旧的」的不一致观感。
      void loadCredits();
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setRefreshingAll(false);
    }
  }, [reload, loadCredits]);

  /**
   * 批量「刷新全部账号积分并签到」。
   *
   * 与「刷新积分」的区别：签到遵守签到时间段（窗口外整轮跳过，提示「本次仅刷新积分」）
   * 与自动签到排除名单（已关闭自动签到的账号不参与），结果按结构化计数分类展示。
   */
  const handleCheckinAll = useCallback(async () => {
    if (checkinAllRunning) return;
    setCheckinAllRunning(true);
    try {
      const res = await checkinAllAccounts();
      if (res.outside_window) {
        notifyInfo("当前不在签到时间段，本次仅刷新积分");
      } else {
        const parts: string[] = [];
        if (res.success > 0) parts.push(`${res.success} 个签到成功`);
        if (res.already > 0) parts.push(`${res.already} 个已签到`);
        if (res.failed > 0) parts.push(`${res.failed} 个失败`);
        if (res.skipped > 0) parts.push(`${res.skipped} 个跳过（已关闭自动签到或不支持签到）`);
        const summary = parts.length > 0 ? parts.join("，") : "没有账号需要签到";
        if (res.failed > 0) {
          notifyWarning("刷新积分并签到完成，但有失败", {
            description: res.first_error ? `${summary}；首个错误：${res.first_error}` : summary,
          });
        } else {
          notifySuccess("刷新积分并签到完成", { description: summary });
        }
      }
      reload();
      // 签到会改余额，逐包明细与今日签到状态都要跟着更新。
      void loadCredits();
      void loadCheckins();
    } catch (err) {
      notifyError("刷新积分并签到失败", { description: describeError(err) });
    } finally {
      setCheckinAllRunning(false);
    }
  }, [checkinAllRunning, reload, loadCredits, loadCheckins]);

  const showKeyPanel = keyPanelOpen || unauthorized;
  const firstLoad = loading && accounts.length === 0;
  // 统计口径以后端的 overview 为准（总数 = 可用 + 失效 + 冷却），前端不另算一套。
  const stats = overview?.accounts;

  return (
    <div className="mx-auto w-full max-w-[1180px] px-6 py-8 sm:px-8 sm:py-9">
      <header className="mb-6">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="min-w-0">
            <h1 className="text-[28px] font-semibold tracking-tight">账号管理</h1>
            {/*
              副标题按**事实**裁剪：wb-switch 的原文是「统一管理 WorkBuddy、CodeBuddy IDE、
              CodeBuddy CLI 与 VS Code CodeBuddy 插件账号…」；本项目对 IDE / VS Code
              只做探测、未实现写入（见 internal/localapps/targets.go），写进副标题
              等于宣传做不到的能力，因此裁到实际覆盖的范围。
            */}
            <p className="mt-2 text-sm leading-6 text-muted-foreground">
              统一管理 WorkBuddy 账号的凭据、积分与签到状态。
            </p>
            {/* 档位筛选（全部 / 国内版 / 国际版）：纯前端过滤卡片，不改变任何请求。 */}
            <PillGroup
              className="mt-4"
              value={variant}
              onChange={setVariant}
              ariaLabel="账号档位筛选"
              options={[
                { value: "all", label: "全部" },
                { value: "cn", label: "国内版" },
                { value: "intl", label: "国际版" },
              ]}
            />
          </div>
          <div className="flex shrink-0 flex-wrap items-center justify-end gap-1.5 pt-1">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => void handleRefreshAll()}
              disabled={refreshingAll || loading}
            >
              <RefreshCw
                className={cn("size-3.5", refreshingAll && "animate-spin")}
                aria-hidden="true"
              />
              刷新积分
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={reload}
              disabled={loading}
              aria-label="重新拉取账号列表"
            >
              <RefreshCw className={cn("size-3.5", loading && "animate-spin")} aria-hidden="true" />
              刷新
            </Button>
          </div>
        </div>
      </header>

      {/*
        添加与迁移账号：独立区块（对齐 wb-switch 的页面结构），
        原页头的添加 / 导入 / 导出按钮移入这里 —— 「拿到账号」是一个完整动作，
        与右侧「刷新状态」不是同一类操作。
      */}
      <Card className="mb-5 gap-0 overflow-hidden rounded-2xl border bg-muted/30 py-0 shadow-none">
        <div className="flex flex-wrap items-center gap-x-5 gap-y-3 px-5 py-4">
          <div className="min-w-[190px] flex-1">
            <h2 className="text-sm font-semibold text-foreground">添加与迁移账号</h2>
            <p className="mt-1 text-xs leading-5 text-muted-foreground">
              快速接入新账号，或从已有环境恢复
            </p>
          </div>
          <Button type="button" onClick={() => setAddOpen(true)}>
            <QrCode className="size-3.5" aria-hidden="true" />
            OAuth 扫码添加
          </Button>
          <div className="flex items-center gap-1">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              title="从备份文件导入账号"
              onClick={() => setImportOpen(true)}
            >
              <FileUp className="size-3.5" aria-hidden="true" />
              导入备份
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="sm"
              title="导出账号备份"
              disabled={accounts.length === 0}
              onClick={() => setExportOpen(true)}
            >
              <FileDown className="size-3.5" aria-hidden="true" />
              导出
            </Button>
          </div>
        </div>
      </Card>

      {showKeyPanel ? (
        <ApiKeyPanel
          unauthorized={unauthorized}
          onSaved={reload}
          onDismiss={() => onKeyPanelOpenChange(false)}
        />
      ) : null}

      {error && !unauthorized ? (
        <Card className="mb-5 gap-3 border-destructive/30 bg-destructive/5 p-4">
          <div className="text-sm font-medium text-destructive">读取失败</div>
          <p className="text-xs leading-5 text-muted-foreground">{error}</p>
          <div>
            <Button type="button" variant="outline" size="sm" onClick={reload} disabled={loading}>
              重试
            </Button>
          </div>
        </Card>
      ) : null}

      {/*
        积分到期提醒：放在统计条之前 —— 它回答的是「现在最该先处理哪个账号」，
        与逐包明细共用已加载的 credits，不新增网络请求。「检查」复用页面
        「刷新积分」同一条路径（refreshAllQuota + 重拉明细）。
        没有账号时整卡不显示（与页面其余空态风格一致）。
      */}
      {accounts.length > 0 ? (
        <ExpiryReminderCard
          accounts={sortedAllAccounts}
          credits={credits}
          creditsLoaded={creditsLoaded}
          creditsError={creditsError}
          checking={refreshingAll}
          onCheck={() => void handleRefreshAll()}
        />
      ) : null}

      <StatsBar
        total={stats?.total ?? 0}
        active={stats?.active ?? 0}
        disabled={stats?.disabled ?? 0}
        cooldown={stats?.cooldown ?? 0}
        creditsRemaining={stats?.credits_remaining ?? 0}
        quotaKnown={stats?.quota_known ?? 0}
        loading={firstLoad}
      />

      <div className="mt-5 flex items-center justify-between gap-3">
        <h2 className="text-sm font-medium text-muted-foreground">
          账号列表
          {visibleAccounts.length > 0 ? (
            <span className="ml-1.5 tabular-nums text-muted-foreground">
              {visibleAccounts.length}
            </span>
          ) : null}
        </h2>
        <div className="flex shrink-0 items-center gap-1.5">
          {/* 批量「刷新全部账号积分并签到」：遵守签到时间段与自动签到排除名单。 */}
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                aria-label="刷新全部账号积分并签到（仅在签到时间段内签到；忽略已关闭自动签到的账号）"
                disabled={checkinAllRunning || loading || accounts.length === 0}
                onClick={() => void handleCheckinAll()}
              >
                {checkinAllRunning ? (
                  <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                ) : (
                  <CalendarCheck className="size-3.5" aria-hidden="true" />
                )}
              </Button>
            </TooltipTrigger>
            <TooltipContent side="top">
              刷新全部账号积分并签到（仅在签到时间段内签到；忽略已关闭自动签到的账号）
            </TooltipContent>
          </Tooltip>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            aria-pressed={compact}
            aria-label={compact ? "切换为宽松模式" : "切换为紧凑模式"}
            title={compact ? "切换为宽松模式" : "切换为紧凑模式"}
            onClick={() => {
              const next = !compact;
              setCompact(next);
              writeCompactPref(next);
            }}
          >
            {compact ? (
              <Maximize2 className="size-3.5" aria-hidden="true" />
            ) : (
              <Minimize2 className="size-3.5" aria-hidden="true" />
            )}
            {compact ? "宽松" : "紧凑"}
          </Button>
          <AccountSortToggle value={sort} onChange={setSort} />
        </div>
      </div>

      <section className="mt-3" aria-label="账号列表">
        {firstLoad ? (
          <div className="flex items-center gap-2 py-16 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" aria-hidden="true" />
            加载账号…
          </div>
        ) : accounts.length === 0 ? (
          <Card className="items-center gap-3 py-14 text-center">
            <div className="text-sm font-medium">暂无账号</div>
            <p className="max-w-lg text-xs leading-5 text-muted-foreground">
              暂无账号。点击上方「OAuth 扫码添加」接入账号；本机已登录的账号也请一并添加，以便随时切回。
            </p>
            <Button type="button" size="sm" onClick={() => setAddOpen(true)}>
              <QrCode className="size-3.5" aria-hidden="true" />
              OAuth 扫码添加
            </Button>
          </Card>
        ) : visibleAccounts.length === 0 ? (
          <Card className="items-center gap-3 py-12 text-center">
            <div className="text-sm font-medium">当前档位下暂无账号</div>
            <p className="text-xs leading-5 text-muted-foreground">
              切回「全部」，或添加一个该档位的账号。
            </p>
          </Card>
        ) : (
          <div
              className={cn(
                "grid min-w-0 grid-cols-1 gap-4",
                compact ? "lg:grid-cols-2 xl:grid-cols-3" : "xl:grid-cols-2",
              )}
            >
            {sortedAccounts.map((account) => (
              <AccountCard
                key={account.id || `${account.site}:${account.file}:${account.uid}`}
                account={account}
                now={now}
                credits={credits.get(account.id) ?? null}
                creditsLoaded={creditsLoaded}
                checkin={checkins.get(account.id) ?? null}
                autoCheckinEnabled={account.auto_checkin_enabled}
                localTargets={localTargets}
                onSwitchLocalApp={(target, accountId) => setSwitchRequest({ target, accountId })}
                currentIn={localRoles.get(account.id)}
                recommended={recommendedId !== null && account.id === recommendedId}
                compact={compact}
                onRefreshQuota={handleRefreshQuota}
                onSetDisabled={handleSetDisabled}
                onRevive={handleRevive}
                onRemove={handleRemove}
                onRefreshToken={handleRefreshToken}
                onCheckin={handleCheckin}
                onActivateIntl={handleActivateIntl}
                onIdentifyPlan={handleIdentifyPlan}
                onShowInfo={setInfoTarget}
                onViewAllPackages={onNavigate ? () => onNavigate("credits") : undefined}
              />
            ))}
          </div>
        )}
      </section>

      {/*
        本机应用接入。放在账号列表**之后**：它是「拿到账号之后要干什么」，
        属于下游动作；放前面会让页面第一屏全是本机环境探测结果，
        而用户打开这一页多半是想看账号本身。
      */}
      <section className="mt-6">
        <LocalAppsCard
          accounts={accounts}
          onRolesChange={setLocalRoles}
          onTargetsChange={setLocalTargets}
          refreshKey={localAppsRefreshKey}
        />
      </section>

      {/* 本机会话：与「本机应用接入」同属「账号之外的本机数据」，放一起便于对照。
          WorkBuddy 工具关闭时整卡隐藏（它读的就是 WorkBuddy 客户端的会话库）。 */}
      {tools.workbuddy ? (
        <section className="mt-6">
          <LocalSessionsCard accounts={accounts} />
        </section>
      ) : null}

      <AddAccountDialog open={addOpen} onOpenChange={setAddOpen} onAdded={reload} />
      <ImportAccountsDialog
        open={importOpen}
        onOpenChange={setImportOpen}
        onImported={() => {
          reload();
          // 导入会带进余额完全未知的账号，明细要重新拉一次。
          void loadCredits();
        }}
      />
      <ExportAccountsDialog open={exportOpen} onOpenChange={setExportOpen} accounts={accounts} />
      {/* 账号信息：查看企业名 / UID、编辑备注、选择卡片显示字段（批次 4）。 */}
      <AccountInfoDialog
        open={infoTarget !== null}
        onOpenChange={(open) => {
          if (!open) setInfoTarget(null);
        }}
        account={infoTarget}
        onSaved={reload}
      />
      {/*
        账号卡发起的本机切换：复用「本机应用接入」卡片里的同一个 SwitchDialog
        （预选该卡账号）—— 不新造写本机文件的逻辑，也不复制一份对话框。
      */}
      <SwitchDialog
        target={switchRequest?.target ?? null}
        accounts={accounts}
        open={switchRequest !== null}
        initialAccountId={switchRequest?.accountId}
        onOpenChange={(open) => {
          if (!open) setSwitchRequest(null);
        }}
        onDone={() => setLocalAppsRefreshKey((key) => key + 1)}
      />
    </div>
  );
}
