import { type ReactNode, useCallback, useEffect, useMemo, useState } from "react";
import {
  AlertTriangle,
  ArrowLeftRight,
  Copy,
  Database,
  FileText,
  FolderOpen,
  HardDrive,
  Info,
  Layers,
  MessagesSquare,
  Puzzle,
  Loader2,
  RefreshCw,
  RotateCw,
  Webhook,
} from "lucide-react";
import {
  notifyInfo,
  notifySuccess,
  notifyError,
  notifyWarning,
} from "@/lib/notify";

import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { MetricRow, PillGroup, Section, StatMetric } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import {
  copyExtSessions,
  describeError,
  fetchAccounts,
  fetchExtSessions,
  fetchLocalCapabilities,
  fetchLocalHooks,
  fetchLocalLogs,
  fetchLocalLogTail,
  fetchLocalOverview,
  fetchLocalPaths,
  isAbortError,
  previewExtSessionPair,
  restartLocalAgent,
  unifyExtSessionGroup,
} from "@/lib/api";
import { formatBytes, formatRelativeTime } from "@/lib/format";
import { useSupportedTools } from "@/lib/supported-tools";
import type {
  Account,
  ExtSession,
  ExtSessionClient,
  ExtSessionCopyResult,
  ExtSessionGroup,
  ExtSessionGroupMember,
  ExtSessionPreviewPair,
  ExtSessionsResponse,
  LocalCapabilities,
  LocalHooksResponse,
  LocalLogsResponse,
  LocalOverview,
  LocalPathsResponse,
} from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * 本机客户端页。
 *
 * 数据来源是**另一个进程**（本机代理），经网关 /panel/local/* 反代过来。
 * 所以这一页的失败模式比其它页多：网关在跑但本机代理没起来时，
 * 需要把「为什么没起来」讲清楚（配置关了 / 没装二进制 / 崩了），而不是笼统报错。
 */
export function LocalPage() {
  // 「支持工具」：关掉 WorkBuddy 后，这一页的客户端数据（会话/日志/配置钩子）
  // 不再展示也不再探测 —— 它们全部来自 WorkBuddy 客户端。
  // 代理状态卡与能力边界保留：那是「本机代理」本身的状态，不属于某个工具。
  const tools = useSupportedTools();
  const workbuddyOn = tools.workbuddy;
  const [caps, setCaps] = useState<LocalCapabilities | null>(null);
  const [overview, setOverview] = useState<LocalOverview | null>(null);
  const [paths, setPaths] = useState<LocalPathsResponse | null>(null);
  const [logs, setLogs] = useState<LocalLogsResponse | null>(null);
  const [tail, setTail] = useState<{ file: string; lines: string[] } | null>(null);
  const [hooks, setHooks] = useState<LocalHooksResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [restarting, setRestarting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const c = await fetchLocalCapabilities();
      setCaps(c);
      if (!c.available) {
        setError(null);
        setLoading(false);
        return;
      }
      if (!workbuddyOn) {
        // 跳过客户端数据探测：能省一次本机探测就省。
        setOverview(null);
        setPaths(null);
        setLogs(null);
        setHooks(null);
        setTail(null);
        setError(null);
        setLoading(false);
        return;
      }
      const [ov, pa, lg, hk] = await Promise.all([
        fetchLocalOverview().catch(() => null),
        fetchLocalPaths().catch(() => null),
        fetchLocalLogs().catch(() => null),
        fetchLocalHooks().catch(() => null),
      ]);
      setOverview(ov);
      setPaths(pa);
      setLogs(lg);
      setHooks(hk);
      setError(null);
    } catch (err) {
      setError(describeError(err));
    } finally {
      setLoading(false);
    }
  }, [workbuddyOn]);

  useEffect(() => {
    void load();
  }, [load]);

  const handleRestart = useCallback(async () => {
    setRestarting(true);
    try {
      const res = await restartLocalAgent();
      if (res.ok === false) {
        // 重启失败是「这次动作没做成」，不是请求失败——把后端给的理由原样展示。
        notifyError(res.detail || "本机代理重启失败");
      } else {
        notifySuccess(res.detail || "本机代理已重启");
      }
      await load();
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setRestarting(false);
    }
  }, [load]);

  const handleTail = useCallback(async (file: string) => {
    try {
      const res = await fetchLocalLogTail(file, 200);
      setTail({ file, lines: res.lines ?? [] });
    } catch (err) {
      notifyError(describeError(err));
    }
  }, []);

  if (loading && caps === null) {
    return (
      <Shell>
        <Skeleton className="h-24" />
        <Skeleton className="h-64" />
      </Shell>
    );
  }

  // 本机能力不可用：服务端部署的正常形态，不该当成错误大呼小叫
  if (caps && !caps.available) {
    return (
      <Shell>
        <Section
          id="local-unavailable"
          title={
            <>
              <AlertTriangle
                className="mr-1.5 inline size-3.5 align-[-2px] text-amber-500"
                aria-hidden="true"
              />
              本机客户端功能不可用
            </>
          }
          description={caps.reason || "本机代理未运行"}
        >
          <CardContent className="space-y-3 text-xs leading-5 text-muted-foreground">
            <p>
              本机相关功能（读取客户端会话、日志、配置）需要在本机额外运行一个小进程
              <span className="mx-1 font-mono">wb-local-agent</span>
              ，由网关自动拉起。服务端部署时没有它是**正常**的，相关能力会自动隐藏。
            </p>
            {caps.searched_paths && caps.searched_paths.length > 0 ? (
              <div>
                <p className="mb-1 text-foreground">已查找过以下位置：</p>
                <ul className="space-y-0.5 font-mono text-xs">
                  {caps.searched_paths.map((p) => (
                    <li key={p} className="truncate" title={p}>
                      {p}
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
            <div className="flex flex-wrap items-center gap-2 pt-1">
              <Button type="button" size="sm" variant="outline" onClick={() => void load()}>
                <RefreshCw className="size-3.5" aria-hidden="true" />
                重新检测
              </Button>
              <span className="text-xs text-muted-foreground">
                状态：{caps.state}
                {caps.enabled ? "" : "（已在配置中关闭）"}
              </span>
            </div>
          </CardContent>
        </Section>
      </Shell>
    );
  }

  const storage = overview?.storage;
  const client = overview?.client;

  return (
    <Shell>
      <header className="mb-6 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">本机客户端</h1>
          <p className="mt-2 text-sm leading-6 text-muted-foreground">
            本机 WorkBuddy 客户端的数据概览（会话、项目、日志、配置钩子）。
            这页的数据由本机代理进程提供，经网关反代，全部是**只读**的。
          </p>
        </div>
        <div className="flex shrink-0 flex-wrap items-center justify-end gap-1.5 pt-1">
          <Button type="button" size="sm" variant="outline" disabled={loading} onClick={() => void load()}>
            {loading ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <RefreshCw className="size-3.5" aria-hidden="true" />
            )}
            刷新
          </Button>
          <Button type="button" size="sm" variant="outline" disabled={restarting} onClick={() => void handleRestart()}>
            {restarting ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <RotateCw className="size-3.5" aria-hidden="true" />
            )}
            重启本机代理
          </Button>
        </div>
      </header>

      {error ? (
        <Card>
          <CardContent className="p-6 text-sm text-destructive">{error}</CardContent>
        </Card>
      ) : (
        <div className="space-y-12">
          {/* 代理状态 */}
          <Card>
            <CardContent className="flex flex-wrap items-center gap-x-6 gap-y-2 p-4 text-xs">
              <span className="flex items-center gap-1.5">
                <span className="size-1.5 rounded-full bg-emerald-500" aria-hidden="true" />
                本机代理运行中
              </span>
              {caps?.agent_version ? <span className="text-muted-foreground">版本 {caps.agent_version}</span> : null}
              {caps?.agent_pid ? (
                <span className="text-muted-foreground">pid {caps.agent_pid}</span>
              ) : null}
              {caps?.port ? <span className="text-muted-foreground">端口 {caps.port}</span> : null}
              {caps?.mode ? (
                <Badge variant="outline" className="text-xs">
                  {caps.mode === "readonly" ? "只读模式" : caps.mode}
                </Badge>
              ) : null}
              {caps?.restarts ? (
                <span className="text-muted-foreground">已自动重启 {caps.restarts} 次</span>
              ) : null}
              {caps?.bin ? (
                <span className="truncate font-mono text-xs text-muted-foreground" title={caps.bin}>
                  {caps.bin}
                </span>
              ) : null}
            </CardContent>
          </Card>

          {/* WorkBuddy 工具关闭时：不展示也不探测其客户端数据，但如实说明原因。 */}
          {!workbuddyOn ? (
            <Card>
              <CardContent className="flex items-start gap-2 p-4 text-xs leading-5 text-muted-foreground">
                <Info className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
                <span>
                  WorkBuddy 工具已在「配置 → 支持工具」关闭：本机客户端数据
                  （会话、数据库、关键路径、日志、配置钩子）不再展示，
                  也不再发起对应探测。重新打开即恢复。
                </span>
              </CardContent>
            </Card>
          ) : null}

          {/* 概览磁贴 */}
          {workbuddyOn ? (
            <>
              <Section id="local-overview" title="概览">
                <MetricRow>
                  <StatMetric
                    icon={MessagesSquare}
                    label="会话"
                    value={String(overview?.counts?.sessions ?? 0)}
                    hint="本地会话状态文件"
                  />
                  <StatMetric
                    icon={FolderOpen}
                    label="项目"
                    value={String(overview?.counts?.projects ?? 0)}
                    hint="工作区历史"
                    divided
                  />
                  <StatMetric
                    icon={Puzzle}
                    label="技能"
                    value={String(overview?.counts?.skills ?? 0)}
                    hint="已安装技能"
                    divided
                  />
                  <StatMetric
                    icon={HardDrive}
                    label="数据占用"
                    value={storage ? formatBytes(storage.size_bytes) : "—"}
                    hint={storage?.cached ? "缓存值（5 分钟内）" : storage ? "本次扫描" : "未统计"}
                    divided
                  />
                </MetricRow>
              </Section>

              {/* 客户端信息 */}
              <Section id="local-client" title="客户端">
                <CardContent className="space-y-1.5 text-xs">
                  <Row label="数据目录" value={overview?.data_dir ?? "-"} mono />
                  <Row
                    label="settings.json"
                    value={client?.settings_present ? "存在" : "不存在"}
                  />
                  <Row
                    label="客户端版本"
                    value={
                      describeVersion(client?.from_log) ||
                      describeVersion(client?.app_config) ||
                      "未从日志/配置中取到"
                    }
                  />
                  {overview?.logs ? (
                    <Row
                      label="日志"
                      value={`${overview.logs.file_count} 个文件 · 最新 ${
                        overview.logs.latest_date || "未知"
                      }`}
                    />
                  ) : null}
                  {overview?.scanned_at ? (
                    <Row
                      label="扫描时间"
                      value={formatRelativeTime(overview.scanned_at, Math.floor(Date.now() / 1000))}
                    />
                  ) : null}
                </CardContent>
              </Section>

              {/* 数据库（只看存在性与大小） */}
              {(overview?.databases ?? []).length > 0 ? (
                <Section
                  id="local-databases"
                  title={
                    <>
                      <Database className="mr-1.5 inline size-3.5 align-[-2px]" aria-hidden="true" />
                      本地数据库
                    </>
                  }
                  description="只读展示大小与修改时间；不打开数据库文件（打开会加锁，可能干扰正在运行的客户端）"
                >
                  <CardContent className="divide-y divide-border/60">
                    {(overview?.databases ?? []).map((db) => (
                      <div key={db.name} className="flex flex-wrap items-center justify-between gap-2 py-2 text-xs">
                        <span className="font-mono">{db.name}</span>
                        <span className="text-muted-foreground">
                          {formatBytes(db.size)} · {formatRelativeTime(db.mtime, Math.floor(Date.now() / 1000))}
                        </span>
                      </div>
                    ))}
                  </CardContent>
                </Section>
              ) : null}

              {/* 关键路径 */}
              {(paths?.paths ?? []).length > 0 ? (
                <Section
                  id="local-paths"
                  title={
                    <>
                      <FolderOpen className="mr-1.5 inline size-3.5 align-[-2px]" aria-hidden="true" />
                      关键路径
                    </>
                  }
                >
                  <CardContent className="divide-y divide-border/60">
                    {(paths?.paths ?? []).map((p) => (
                      <div key={p.rel} className="flex flex-wrap items-center justify-between gap-2 py-1.5 text-xs">
                        <span className="font-mono text-xs text-muted-foreground" title={p.path}>
                          {p.rel}
                        </span>
                        <span className={cn("shrink-0", p.exists ? "text-muted-foreground" : "text-muted-foreground")}>
                          {p.exists
                            ? p.is_dir
                              ? "目录"
                              : formatBytes(p.size ?? 0)
                            : "不存在"}
                        </span>
                      </div>
                    ))}
                  </CardContent>
                </Section>
              ) : null}

              {/* 配置钩子（只读） */}
              <Section
                id="local-hooks"
                title={
                  <>
                    <Webhook className="mr-1.5 inline size-3.5 align-[-2px]" aria-hidden="true" />
                    客户端配置钩子
                  </>
                }
                description="只读展示 settings.json 里已注册的 hooks。本机代理不会写入客户端配置 —— 改动用户客户端文件需要完整的备份与还原保护。"
              >
                <CardContent className="space-y-2 text-xs">
                  {hooks?.present === false ? (
                    <p className="text-muted-foreground">{hooks.message || "未找到 settings.json"}</p>
                  ) : hooks?.error ? (
                    <p className="text-destructive">{hooks.error}</p>
                  ) : (hooks?.hooks ?? []).length === 0 ? (
                    <p className="text-muted-foreground">未注册任何 hook。</p>
                  ) : (
                    (hooks?.hooks ?? []).map((h) => (
                      <div key={h.event} className="rounded-md border border-border/60 p-2">
                        <div className="flex items-center gap-2">
                          <Badge variant="outline" className="text-xs">
                            {h.event}
                          </Badge>
                          <span className="text-muted-foreground">{h.entries} 条</span>
                        </div>
                        {(h.preview ?? []).map((pv, i) => (
                          <div key={i} className="mt-1 space-y-0.5 font-mono text-xs text-muted-foreground">
                            {pv.matcher ? <div>matcher: {pv.matcher}</div> : null}
                            {(pv.commands ?? []).map((c, j) => (
                              <div key={j} className="truncate" title={c}>
                                {c}
                              </div>
                            ))}
                          </div>
                        ))}
                      </div>
                    ))
                  )}
                </CardContent>
              </Section>

              {/* 日志 */}
              <Section
                id="local-logs"
                title={
                  <>
                    <FileText className="mr-1.5 inline size-3.5 align-[-2px]" aria-hidden="true" />
                    客户端日志
                  </>
                }
                description="点击文件名查看尾部 200 行（读取上限 256KB，只取尾部）"
              >
                <CardContent className="space-y-2">
                  <div className="flex flex-wrap gap-1.5">
                    {(logs?.files ?? []).slice(0, 20).map((f) => (
                      <button
                        key={f.name}
                        type="button"
                        onClick={() => void handleTail(f.name)}
                        className={cn(
                          "cursor-pointer rounded border px-2 py-1 text-xs transition-colors",
                          tail?.file === f.name
                            ? "border-primary/60 bg-muted text-foreground"
                            : "border-border text-muted-foreground hover:text-foreground",
                        )}
                      >
                        {f.name}
                        <span className="ml-1 text-muted-foreground">{formatBytes(f.size)}</span>
                      </button>
                    ))}
                    {(logs?.files ?? []).length === 0 ? (
                      <span className="text-xs text-muted-foreground">日志目录为空或不可读。</span>
                    ) : null}
                  </div>
                  {tail ? (
                    <div className="max-h-72 overflow-auto rounded-md border border-border/60 bg-muted/30 p-2">
                      <pre className="whitespace-pre-wrap break-all font-mono text-xs leading-4 text-muted-foreground">
                        {tail.lines.join("\n")}
                      </pre>
                    </div>
                  ) : null}
                </CardContent>
              </Section>
            </>
          ) : null}

          {/* 扩展数据仓会话：网关直接读写扩展的数据仓，与 WorkBuddy 工具开关无关，
              因此放在 workbuddyOn 条件之外单独渲染。 */}
          <ExtSessionsSection />

          {/* 能力边界说明 */}
          {/* 能力边界说明：尾部注记用正文段落而不是卡片 */}
          <div className="space-y-2 px-1 text-xs leading-5 text-muted-foreground">
              <p className="flex items-center gap-1.5 text-foreground">
                <Info className="size-3.5" aria-hidden="true" />
                能力边界（如实说明）
              </p>
              <p>
                当前本机代理实现的能力：
                <span className="ml-1 font-mono">
                  {(caps?.agent_capabilities ?? []).join(" / ") || "-"}
                </span>
              </p>
              {(caps?.unimplemented ?? []).length > 0 ? (
                <p>
                  契约里有但**尚未实现**的能力：
                  <span className="ml-1 font-mono">{(caps?.unimplemented ?? []).join(" / ")}</span>
                  。这些都是需要**写本机**的操作（会话复制、进程扫描、数据库读写、编辑器配置注入、
                  钩子安装），在没有完整的「写前备份 + 幂等安装 + 逐字节还原」保护之前不做——
                  做薄了比没有更危险。
                </p>
              ) : null}
              <p>
                安全边界：本机代理只绑 127.0.0.1，所有请求校验一次性令牌（由网关注入并在反代时替换），
                外部凭据不会透传给本机代理。
              </p>
          </div>
        </div>
      )}
    </Shell>
  );
}

// -----------------------------------------------------------------------------
// 扩展数据仓会话（VS Code 内 CodeBuddy 插件 / CodeBuddy IDE）
//
// 与 WorkBuddy 侧的「本机会话」不同：这里读写的是扩展自己的会话树
// （`CodeBuddyExtension/Data/<uid>/<VSCode|CodeBuddyIDE>/<uid>/history`），
// 由网关直接服务（不经过本机代理）。会话 = 一个目录，复制项是
// (工作区 hash, 会话 id)；写入前编辑器必须完全退出，否则会被编辑器退出时的
// 内存回写覆盖 —— 这是这块 UI 最需要讲清楚的一条。
// -----------------------------------------------------------------------------

/** 客户端档位：value 是后端 extStore 接受的标识，backupKind 用于交代备份路径。 */
const EXT_CLIENTS: { value: ExtSessionClient; label: string; backupKind: string }[] = [
  { value: "vscode", label: "VS Code 插件", backupKind: "vscode-sessions" },
  { value: "codebuddy-ide", label: "CodeBuddy IDE", backupKind: "codebuddy-ide-sessions" },
];

/**
 * 预览判定 → 文案与色调。
 *
 * 结论值以后端 localsessions.Verdict* 为准（camelCase，如 fastForward）——
 * 与 WorkBuddy 组件里写的 fast_forward 不同，那是另一套渲染分支的历史遗留。
 */
const EXT_VERDICT_STYLE: Record<string, { label: string; tone: string }> = {
  identical: { label: "两边一致", tone: "text-muted-foreground" },
  fastForward: { label: "可直接同步（纯追加）", tone: "text-primary-ink" },
  ahead: { label: "目标已领先", tone: "text-amber-700 dark:text-amber-300" },
  diverge: { label: "两边都有改动", tone: "text-amber-700 dark:text-amber-300" },
  unknown: { label: "无法判定", tone: "text-amber-700 dark:text-amber-300" },
};

/** 写入模式（后端 Mode*，camelCase）→ 中文说明。 */
const EXT_MODE_LABEL: Record<string, string> = {
  fastForward: "可安全快进（纯追加，不覆盖）",
  overwrite: "双方都有改动，将覆盖目标",
  unifyOverwrite: "目标有改动，将覆盖目标",
};

/** 组状态 → 文案与色调（与后端 buildGroupView 的 status 取值一致）。 */
const EXT_GROUP_STATUS: Record<string, { label: string; tone: string }> = {
  latest: { label: "内容一致", tone: "text-muted-foreground" },
  behind: { label: "待同步", tone: "text-amber-700 dark:text-amber-300" },
  diverge: { label: "有分歧", tone: "text-destructive" },
  missing: { label: "内容缺失", tone: "text-muted-foreground" },
  unknown: { label: "无法确认", tone: "text-muted-foreground" },
};

/** 成员内容状态 → 徽标（与后端 buildGroupView 的 version_status 取值一致）。 */
const EXT_MEMBER_STATUS: Record<
  string,
  { label: string; variant: "success" | "warning" | "outline" }
> = {
  latest: { label: "内容最新", variant: "success" },
  behind: { label: "内容落后", variant: "warning" },
  diverge: { label: "存在分歧", variant: "warning" },
  missing: { label: "内容缺失", variant: "warning" },
  unknown: { label: "无法确认", variant: "outline" },
};

/** 来源 / 目标账号的可选项（账号池标签优先，组内成员 uid 兜底）。 */
interface ExtUidOption {
  uid: string;
  label: string;
}

/** 「以此为准统一」的预览计划：来源 + 逐目标判定。 */
interface ExtUnifyPlanItem {
  member: ExtSessionGroupMember;
  preview: ExtSessionPreviewPair | null;
  /** 选定的写入模式；空串 = 判定不允许写入（阻断项）。 */
  mode: string;
  error?: string;
}

interface ExtUnifyPlan {
  group: ExtSessionGroup;
  source: ExtSessionGroupMember;
  items: ExtUnifyPlanItem[];
  /** 编辑器运行中时，是否同意「关闭 → 写入 → 完成后重开」。 */
  restart: boolean;
}

/** 毫秒时间戳 → 本地时间（0 / 缺失显示 —）。 */
function formatExtTime(ms: number): string {
  if (!ms) return "—";
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/**
 * 扩展会话区块：客户端切换 + 来源账号 + 数据仓状态 + 会话/分组列表与操作入口。
 *
 * 交互与 WorkBuddy 的会话卡片保持一致：破坏性操作（复制 / 统一）一律先确认再执行，
 * 确认文案写明会备份到 `<工作目录>/backups/...`；动作失败走 notifyError，
 * 结果与提示原样展示后端返回的 notes，不做任何自动重试。
 */
function ExtSessionsSection() {
  const [client, setClient] = useState<ExtSessionClient>("vscode");
  const [uid, setUid] = useState("");
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [data, setData] = useState<ExtSessionsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [copyItem, setCopyItem] = useState<ExtSession | null>(null);
  const [previewGroup, setPreviewGroup] = useState<ExtSessionGroup | null>(null);
  const [unifyPlan, setUnifyPlan] = useState<ExtUnifyPlan | null>(null);

  const clientMeta = EXT_CLIENTS.find((c) => c.value === client) ?? EXT_CLIENTS[0];

  useEffect(() => {
    let cancelled = false;
    // 账号池只用于给「来源 / 目标账号」下拉提供标签；读不到时仍可用组内成员 uid。
    fetchAccounts()
      .then((res) => {
        if (!cancelled) setAccounts(res.accounts ?? []);
      })
      .catch(() => {
        // 静默：账号池读不到不影响会话组本身的浏览与预览。
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      try {
        const res = await fetchExtSessions(client, uid, signal);
        setData(res);
        setError(null);
      } catch (err) {
        if (isAbortError(err)) return;
        setError(describeError(err));
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [client, uid],
  );

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  // 可选的来源账号 = 账号池 ∪ 会话组里的成员 uid（去重；账号池标签优先）。
  const uidOptions = useMemo(() => {
    const map = new Map<string, string>();
    for (const a of accounts) {
      const id = a.uid.trim();
      if (id && !map.has(id)) map.set(id, a.nickname || id);
    }
    for (const g of data?.groups ?? []) {
      for (const m of g.members) {
        const id = (m.uid || "").trim();
        if (id && !map.has(id)) map.set(id, m.label || id);
      }
    }
    return [...map.entries()].map(([id, label]) => ({ uid: id, label }));
  }, [accounts, data]);

  // 默认选中第一个可用账号：不选账号时后端不会枚举会话。
  useEffect(() => {
    if (!uid && uidOptions.length > 0) setUid(uidOptions[0].uid);
  }, [uid, uidOptions]);

  const sessions = data?.sessions ?? [];
  const groups = data?.groups ?? [];
  const targets = uidOptions.filter((o) => o.uid !== uid);
  const sourceLabel = uidOptions.find((o) => o.uid === uid)?.label ?? uid;

  const planItems = unifyPlan?.items ?? [];
  const planBlocked = planItems.filter((it) => it.error || !it.mode);
  const planWrites = planItems.filter((it) => it.mode && it.preview);

  /**
   * 逐对预览「以此为准统一」：按 available_modes 决定写入模式，
   * 判定不允许写入的目标列为阻断项显式列出，不静默执行（对照 WorkBuddy 的 prepareUnify）。
   */
  const prepareUnify = useCallback(
    async (group: ExtSessionGroup, source: ExtSessionGroupMember) => {
      const others = group.members.filter((m) => m.member_id !== source.member_id);
      if (others.length === 0) {
        notifyError("该会话组只有一个成员，无需统一");
        return;
      }
      setBusy(true);
      try {
        const items = await Promise.all(
          others.map(async (m): Promise<ExtUnifyPlanItem> => {
            try {
              const p = await previewExtSessionPair(group.id, {
                client,
                sourceMemberId: source.member_id,
                targetMemberId: m.member_id,
              });
              return { member: m, preview: p, mode: p.available_modes?.[0] ?? "" };
            } catch (err) {
              return { member: m, preview: null, mode: "", error: describeError(err) };
            }
          }),
        );
        setUnifyPlan({ group, source, items, restart: false });
      } catch (err) {
        notifyError("无法检查各副本状态", { description: describeError(err) });
      } finally {
        setBusy(false);
      }
    },
    [client],
  );

  /** 执行整组统一；结果按「写入 / 跳过 / 失败」如实汇报。 */
  const runUnify = useCallback(async () => {
    if (!unifyPlan) return;
    const writes = unifyPlan.items.filter((it) => it.mode && it.preview);
    if (writes.length === 0) return;
    const labelOf = (memberId: string) =>
      unifyPlan.items.find((it) => it.member.member_id === memberId)?.member.label ??
      memberId.slice(0, 8);
    setBusy(true);
    try {
      const res = await unifyExtSessionGroup(unifyPlan.group.id, {
        client,
        sourceMemberId: unifyPlan.source.member_id,
        targets: Object.fromEntries(writes.map((it) => [it.member.member_id, it.mode])),
        restart: unifyPlan.restart,
      });
      const failed = res.outcomes.filter((o) => o.error);
      const skipped = res.outcomes.filter((o) => !o.applied && !o.error);
      if (failed.length > 0) {
        notifyError(`统一完成，但有 ${failed.length} 项失败`, {
          description: failed.map((o) => `${labelOf(o.member_id)}：${o.error}`).join("；"),
        });
      } else if (skipped.length > 0) {
        notifyWarning(`统一完成：${res.synced} 项写入，${skipped.length} 项跳过`, {
          description: skipped
            .map((o) => `${labelOf(o.member_id)}：${o.reason || "已跳过"}`)
            .join("；"),
        });
      } else {
        notifySuccess(`统一完成：${res.synced} 项写入`);
      }
      const notes = res.notes ?? [];
      if (notes.length > 0) notifyInfo(notes.join("；"));
      setUnifyPlan(null);
      await load();
    } catch (err) {
      notifyError("统一失败", { description: describeError(err) });
    } finally {
      setBusy(false);
    }
  }, [unifyPlan, client, load]);

  return (
    <Section
      id="local-ext-sessions"
      title={
        <>
          <Puzzle className="mr-1.5 inline size-3.5 align-[-2px]" aria-hidden="true" />
          扩展会话（VS Code 插件 / CodeBuddy IDE）
        </>
      }
      description="CodeBuddy 扩展自己的会话数据仓（与 WorkBuddy 客户端相互独立）：可把某账号的会话复制给同客户端的其他账号，并按内容关联组预览、统一。写入前编辑器必须完全退出。"
      action={
        <div className="flex flex-wrap items-center gap-2">
          <PillGroup
            value={client}
            options={EXT_CLIENTS.map((c) => ({ value: c.value, label: c.label }))}
            onChange={setClient}
            ariaLabel="选择扩展数据仓客户端"
          />
          <Button type="button" variant="ghost" size="sm" disabled={loading} onClick={() => void load()}>
            <RefreshCw className={cn("size-3.5", loading && "animate-spin")} aria-hidden="true" />
            刷新
          </Button>
        </div>
      }
    >
      <CardContent className="space-y-4 p-4 sm:p-5">
        {/* 来源账号：会话列表按它过滤，复制时作为来源 */}
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-xs">
          <span className="font-medium text-muted-foreground">来源账号</span>
          <select
            aria-label="选择来源账号"
            className="max-w-full min-w-[200px] cursor-pointer rounded-md border border-border bg-background px-2 py-1.5 text-xs"
            value={uid}
            disabled={loading && !data}
            onChange={(e) => setUid(e.target.value)}
          >
            {uidOptions.length === 0 ? <option value="">（账号池与会话组里都没有可用账号）</option> : null}
            {uidOptions.map((o) => (
              <option key={o.uid} value={o.uid}>
                {o.label}
              </option>
            ))}
          </select>
          {data ? (
            <span className="text-muted-foreground">
              {sessions.length} 条会话 · {groups.length} 个关联组
            </span>
          ) : null}
        </div>

        {error ? (
          <div
            role="alert"
            className="flex flex-wrap items-center gap-2 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2.5 text-xs text-destructive"
          >
            <AlertTriangle className="size-3.5 shrink-0" aria-hidden="true" />
            <span className="min-w-0 flex-1 leading-5">{error}</span>
            <Button type="button" variant="outline" size="sm" disabled={loading} onClick={() => void load()}>
              重试
            </Button>
          </div>
        ) : null}

        {loading && !data ? (
          <div className="space-y-2" aria-busy="true" aria-label="正在读取扩展数据仓">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-24 w-full" />
          </div>
        ) : null}

        {data && !data.available ? (
          <p className="rounded-lg border border-border bg-muted/40 px-3 py-6 text-center text-sm text-muted-foreground">
            {data.note || `未找到 ${clientMeta.label} 的数据目录（可能未安装或从未登录过）`}
          </p>
        ) : null}

        {data && data.available ? (
          <>
            {/* 数据仓状态 */}
            <div className="space-y-2">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                <span className="flex items-center gap-1.5">
                  <span className="size-1.5 rounded-full bg-emerald-500" aria-hidden="true" />
                  数据目录已就绪
                </span>
                <span className="max-w-full truncate font-mono" title={data.data_root}>
                  {data.data_root}
                </span>
                {typeof data.skipped === "number" && data.skipped > 0 ? (
                  <span className="text-amber-700 dark:text-amber-300">
                    有 {data.skipped} 个工作区索引无法解析
                  </span>
                ) : null}
              </div>
              {data.running ? (
                <div
                  role="status"
                  className="flex items-start gap-2 rounded-lg border border-amber-500/30 bg-amber-500/8 px-3 py-2.5 text-xs leading-5 text-amber-800 dark:text-amber-300"
                >
                  <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
                  <span className="min-w-0">
                    检测到 {clientMeta.label} 正在运行：编辑器退出时会把内存里的索引与消息回写磁盘，
                    覆盖本工具的写入。请先
                    <span className="font-medium">完全退出编辑器</span>
                    （含后台进程）再复制或统一；也可在确认框里勾选「关闭编辑器，写入完成后自动重开」。
                  </span>
                </div>
              ) : null}
              {data.store_status === "unavailable" && data.store_error ? (
                <div
                  role="status"
                  className="flex flex-wrap items-center gap-2 rounded-lg border border-amber-500/30 bg-amber-500/8 px-3 py-2.5 text-xs text-muted-foreground"
                >
                  <span className="min-w-0 flex-1 leading-5">{data.store_error}</span>
                  <Button type="button" variant="outline" size="sm" disabled={loading} onClick={() => void load()}>
                    重试
                  </Button>
                </div>
              ) : null}
            </div>

            {/* 可复制会话 */}
            <div className="space-y-2">
              <div className="flex items-center justify-between gap-2">
                <h3 className="text-xs font-medium text-muted-foreground">
                  可复制会话{uid ? `（来源：${sourceLabel}）` : ""}
                </h3>
                <span className="rounded-full bg-muted px-2 py-0.5 text-xs tabular-nums text-muted-foreground">
                  {sessions.length} 条
                </span>
              </div>
              {sessions.length === 0 ? (
                <p className="rounded-lg border border-border bg-muted/40 px-3 py-4 text-center text-xs text-muted-foreground">
                  {uid ? "该来源账号下没有可复制的会话。" : "先选择来源账号，再列出它的会话。"}
                </p>
              ) : (
                <ul className="divide-y divide-border/60 rounded-lg border border-border">
                  {sessions.map((s) => (
                    <li
                      key={`${s.workspace_hash}/${s.id}`}
                      className="flex flex-wrap items-start gap-3 px-3 py-2.5"
                    >
                      <div className="min-w-0 flex-1">
                        <p className="truncate text-sm">{s.title || "（无标题）"}</p>
                        <p className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground">
                          <span className="font-mono" title={s.id}>
                            {s.id.slice(0, 8)}…
                          </span>
                          <span>{formatExtTime(s.updated_at)}</span>
                          {s.has_history ? null : (
                            <Badge variant="outline" className="text-[10px]">
                              无正文
                            </Badge>
                          )}
                        </p>
                      </div>
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        disabled={targets.length === 0}
                        title={
                          targets.length === 0
                            ? "没有其它可复制的目标账号"
                            : `把这条会话复制到 ${clientMeta.label} 数据仓里的另一个账号`
                        }
                        onClick={() => setCopyItem(s)}
                      >
                        <Copy className="size-3.5" aria-hidden="true" />
                        复制到该客户端
                      </Button>
                    </li>
                  ))}
                </ul>
              )}
            </div>

            {/* 关联会话组 */}
            <div className="space-y-2">
              <div className="flex items-center justify-between gap-2">
                <h3 className="text-xs font-medium text-muted-foreground">关联会话组</h3>
                <span className="rounded-full bg-muted px-2 py-0.5 text-xs tabular-nums text-muted-foreground">
                  {groups.length} 个
                </span>
              </div>
              {groups.length === 0 ? (
                <p className="rounded-lg border border-border bg-muted/40 px-3 py-4 text-center text-xs text-muted-foreground">
                  暂无关联组 —— 复制过一次后，同源副本会自动聚成一组。
                </p>
              ) : (
                <ul className="space-y-2">
                  {groups.map((g) => {
                    const status = EXT_GROUP_STATUS[g.status] ?? EXT_GROUP_STATUS.unknown;
                    const safeSource = g.safe_source_member_id
                      ? g.members.find((m) => m.member_id === g.safe_source_member_id)
                      : undefined;
                    return (
                      <li key={g.id} className="rounded-lg border border-border px-3 py-2.5">
                        <div className="flex flex-wrap items-start justify-between gap-2">
                          <div className="min-w-0">
                            <p className="truncate text-sm font-medium">{g.title || "（无标题）"}</p>
                            <p className="mt-0.5 text-xs text-muted-foreground">
                              {g.summary_text || "—"}
                            </p>
                          </div>
                          <span className={cn("shrink-0 text-xs", status.tone)}>{status.label}</span>
                        </div>
                        <ul className="mt-2 space-y-1">
                          {g.members.map((m) => {
                            const meta = EXT_MEMBER_STATUS[m.version_status] ?? EXT_MEMBER_STATUS.unknown;
                            return (
                              <li
                                key={m.member_id}
                                className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs"
                              >
                                <span className="max-w-[220px] min-w-0 truncate font-medium" title={m.label}>
                                  {m.label}
                                </span>
                                <Badge variant={meta.variant} className="text-[10px]">
                                  {meta.label}
                                </Badge>
                                {m.readable ? (
                                  <span className="text-muted-foreground">{m.record_count} 条</span>
                                ) : null}
                                <span
                                  className="min-w-0 flex-1 truncate text-muted-foreground"
                                  title={m.reason}
                                >
                                  {m.reason}
                                </span>
                                <Button
                                  type="button"
                                  variant="ghost"
                                  size="sm"
                                  className="h-7 px-2 text-xs"
                                  disabled={busy || !m.readable || g.members.length < 2}
                                  title={
                                    !m.readable
                                      ? "正文不可读，无法作为来源"
                                      : g.members.length < 2
                                        ? "组内只有一个成员"
                                        : "以这份内容为准统一其他账号"
                                  }
                                  onClick={() => void prepareUnify(g, m)}
                                >
                                  <ArrowLeftRight className="size-3.5" aria-hidden="true" />
                                  以此为准统一
                                </Button>
                              </li>
                            );
                          })}
                        </ul>
                        <div className="mt-2 flex flex-wrap items-center gap-2">
                          <Button
                            type="button"
                            variant="outline"
                            size="sm"
                            disabled={g.members.length < 2}
                            title={
                              g.members.length < 2
                                ? "组内只有一个成员"
                                : "选择一对成员，预览同步判定（只检查，不写入）"
                            }
                            onClick={() => setPreviewGroup(g)}
                          >
                            <Layers className="size-3.5" aria-hidden="true" />
                            预览配对
                          </Button>
                          <Button
                            type="button"
                            variant="outline"
                            size="sm"
                            disabled={busy || !safeSource}
                            title={
                              safeSource
                                ? `以「${safeSource.label}」为来源统一整组（自动挑选安全源）`
                                : "组内没有可安全同步的来源成员"
                            }
                            onClick={() => safeSource && void prepareUnify(g, safeSource)}
                          >
                            <ArrowLeftRight className="size-3.5" aria-hidden="true" />
                            统一
                          </Button>
                        </div>
                      </li>
                    );
                  })}
                </ul>
              )}
            </div>
          </>
        ) : null}
      </CardContent>

      {/* 复制确认（写入目标账号；先确认再执行） */}
      <ExtCopyDialog
        item={copyItem}
        client={client}
        sourceUid={uid}
        sourceLabel={sourceLabel}
        targets={targets}
        running={data?.running ?? false}
        open={copyItem !== null}
        onOpenChange={(v) => {
          if (!v) setCopyItem(null);
        }}
        onDone={() => void load()}
      />

      {/* 预览配对（只检查，不写入） */}
      <ExtPreviewPairDialog
        group={previewGroup}
        client={client}
        open={previewGroup !== null}
        onOpenChange={(v) => {
          if (!v) setPreviewGroup(null);
        }}
      />

      {/* 「以此为准统一」确认框：先预览再确认（对照 WorkBuddy 的 prepareUnify / confirmUnify） */}
      <AlertDialog
        open={unifyPlan !== null}
        onOpenChange={(open) => {
          if (!open && !busy) setUnifyPlan(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>以「{unifyPlan?.source.label}」的内容为准统一？</AlertDialogTitle>
            <AlertDialogDescription>
              {clientMeta.label} 数据仓内，把这份会话统一到组内其他 {unifyPlan?.items.length ?? 0} 个账号。
              内容相同的账号会跳过。写入前会备份到{" "}
              <span className="font-mono">
                {`<工作目录>/backups/${clientMeta.backupKind}/overwrite/<UTC>/`}
              </span>
              ，被覆盖的目标会话整目录备份；不会改动来源账号。
            </AlertDialogDescription>
          </AlertDialogHeader>

          {/* 逐目标预览：会怎么处理 / 为什么不能处理 */}
          <ul className="max-h-56 space-y-1.5 overflow-y-auto">
            {planItems.map((it) => (
              <li
                key={it.member.member_id}
                className="rounded-lg border border-border px-3 py-2 text-xs"
              >
                <div className="flex items-center justify-between gap-2">
                  <span className="min-w-0 truncate font-medium">{it.member.label}</span>
                  <span
                    className={cn(
                      "shrink-0",
                      it.error || !it.mode ? "text-destructive" : "text-muted-foreground",
                    )}
                  >
                    {it.error
                      ? "无法检查内容"
                      : !it.mode
                        ? "无法安全更新此副本"
                        : (EXT_MODE_LABEL[it.mode] ?? it.mode)}
                  </span>
                </div>
                <p className="mt-0.5 leading-4 text-muted-foreground">{it.error ?? it.preview?.reason}</p>
              </li>
            ))}
          </ul>

          {planBlocked.length > 0 ? (
            <p className="text-sm text-destructive">
              目前无法统一全部账号：{planBlocked.map((it) => it.member.label).join("、")} 会被跳过。
            </p>
          ) : (
            <p className="text-xs text-muted-foreground">
              执行前后端会重新计算预览并逐项复核；内容已变化的项会被跳过并报告，不会静默覆盖。
            </p>
          )}

          {data?.running ? (
            <label className="flex items-start gap-2.5 rounded-lg border border-border px-3 py-2.5 text-xs">
              <Checkbox
                checked={unifyPlan?.restart ?? false}
                disabled={busy}
                onCheckedChange={(v) =>
                  setUnifyPlan((prev) => (prev ? { ...prev, restart: v === true } : prev))
                }
                className="mt-0.5 data-[state=checked]:border-primary data-[state=checked]:bg-primary"
                aria-label="关闭编辑器，写入完成后自动重开"
              />
              <span className="min-w-0 leading-5">
                <span className="font-medium">
                  关闭 {clientMeta.label} 再写入，完成后自动重开
                </span>
                <span className="mt-0.5 block text-muted-foreground">
                  不勾选时后端会直接拒绝 —— 编辑器运行中的写入会被它的退出回写覆盖。
                </span>
              </span>
            </label>
          ) : null}

          <AlertDialogFooter>
            <AlertDialogCancel disabled={busy}>取消</AlertDialogCancel>
            <Button
              type="button"
              disabled={busy || !unifyPlan || planWrites.length === 0}
              onClick={() => void runUnify()}
            >
              {busy ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : null}
              {busy ? "处理中…" : "确认统一"}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Section>
  );
}

/**
 * 复制对话框：选目标账号 → 确认（写明备份路径）→ 执行 → 展示报告。
 *
 * 与 WorkBuddy 的复制对话框同一套写法（来源信息卡、候选账号、错误框、
 * 结果里原样列出 notes），差别是本接口没有 dry-run，确认步骤用 AlertDialog 完成。
 */
function ExtCopyDialog({
  item,
  client,
  sourceUid,
  sourceLabel,
  targets,
  running,
  open,
  onOpenChange,
  onDone,
}: {
  item: ExtSession | null;
  client: ExtSessionClient;
  sourceUid: string;
  sourceLabel: string;
  targets: ExtUidOption[];
  /** 编辑器是否在运行（决定是否给出「关闭并重开」选项）。 */
  running: boolean;
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onDone: () => void;
}) {
  const [targetUid, setTargetUid] = useState("");
  const [restart, setRestart] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<ExtSessionCopyResult | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) {
      setTargetUid("");
      setRestart(false);
      setConfirming(false);
      setBusy(false);
      setResult(null);
      setError(null);
    }
  }, [open]);

  if (!item) return null;

  const meta = EXT_CLIENTS.find((c) => c.value === client) ?? EXT_CLIENTS[0];
  const targetLabel = targets.find((t) => t.uid === targetUid)?.label ?? targetUid;

  const run = async () => {
    setBusy(true);
    setError(null);
    try {
      const res = await copyExtSessions({
        client,
        sourceUid,
        targetUid,
        items: [{ workspaceHash: item.workspace_hash, conversationId: item.id }],
        restart,
      });
      setResult(res);
      setConfirming(false);
      onDone();
    } catch (err) {
      setError(describeError(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>复制到 {meta.label}</DialogTitle>
          <DialogDescription>
            在 {meta.label} 的数据仓内，把来源账号「{sourceLabel}」的这条会话复制给目标账号。
            <span className="mt-1 block font-medium text-foreground">
              不会改动或删除来源账号的会话。
            </span>
          </DialogDescription>
        </DialogHeader>

        {result ? (
          <div className="space-y-2">
            <div className="rounded-lg border border-border bg-muted/40 px-3 py-2.5 text-xs leading-5">
              <p className="text-sm font-medium">
                {result.report.copied.length > 0
                  ? `已复制 ${result.report.copied.length} 个会话`
                  : "没有会话被复制"}
              </p>
              {(result.report.errors ?? []).map((e, i) => (
                <p key={i} className="mt-0.5 text-destructive">
                  {e.error}
                </p>
              ))}
              <p className="mt-1 text-muted-foreground">
                备份目录：<span className="font-mono">{result.report.backup || "—"}</span>
              </p>
            </div>
            {(result.notes ?? []).length > 0 ? (
              <ul className="space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2.5">
                {(result.notes ?? []).map((n, i) => (
                  <li key={i} className="text-xs leading-5 text-muted-foreground">
                    · {n}
                  </li>
                ))}
              </ul>
            ) : null}
            {(result.link_errors ?? []).length > 0 ? (
              <ul className="space-y-1 rounded-lg border border-amber-500/30 bg-amber-500/8 px-3 py-2.5">
                {(result.link_errors ?? []).map((e, i) => (
                  <li key={i} className="text-xs leading-5 text-muted-foreground">
                    关联登记失败（会话内容不受影响）：{e.error}
                  </li>
                ))}
              </ul>
            ) : null}
          </div>
        ) : (
          <>
            <div className="space-y-1 rounded-lg border border-border bg-muted/40 px-3 py-2">
              <p className="text-xs text-muted-foreground">源会话（{sourceLabel}）</p>
              <p className="truncate text-sm">{item.title || "（无标题）"}</p>
              <p className="truncate font-mono text-xs text-muted-foreground">{item.id}</p>
              <p className="text-xs text-muted-foreground">
                {formatExtTime(item.updated_at)}
                {item.has_history ? "" : " · 无正文"}
              </p>
            </div>

            <div className="space-y-1.5">
              <p className="text-xs font-medium text-muted-foreground">复制给</p>
              {targets.length === 0 ? (
                <p className="text-xs text-amber-700 dark:text-amber-300">没有其它账号可选。</p>
              ) : (
                <select
                  aria-label="目标账号"
                  className="w-full cursor-pointer rounded-md border border-border bg-background px-2 py-1.5 text-xs"
                  value={targetUid}
                  disabled={busy}
                  onChange={(e) => setTargetUid(e.target.value)}
                >
                  <option value="">选择目标账号…</option>
                  {targets.map((t) => (
                    <option key={t.uid} value={t.uid}>
                      {t.label}
                    </option>
                  ))}
                </select>
              )}
            </div>

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
            {result ? "关闭" : "取消"}
          </Button>
          {result ? null : (
            <Button
              type="button"
              size="sm"
              disabled={busy || !targetUid}
              onClick={() => setConfirming(true)}
            >
              <Copy className="size-3.5" aria-hidden="true" />
              复制到该客户端…
            </Button>
          )}
        </DialogFooter>

        {/* 写入前确认：备份路径与生命周期守卫讲清楚（不确认不写入） */}
        <AlertDialog
          open={confirming}
          onOpenChange={(v) => {
            if (!busy) setConfirming(v);
          }}
        >
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>确认复制这条会话？</AlertDialogTitle>
              <AlertDialogDescription>
                将把「{item.title || "（无标题）"}」复制给 {meta.label} 的目标账号「{targetLabel}」。
                写入前会把目标工作区索引备份到{" "}
                <span className="font-mono">
                  {`<工作目录>/backups/${meta.backupKind}/<UTC>/`}
                </span>
                （结果里会给出实际路径）；复制按该客户端的 id 策略处理副本 id（插件侧取新 id，
                IDE 侧仅在冲突时取新 id），不会改动来源账号的会话。
                {running
                  ? " 当前检测到编辑器正在运行：不勾选下方选项时，后端会直接拒绝这次写入。"
                  : ""}
              </AlertDialogDescription>
            </AlertDialogHeader>

            {running ? (
              <label className="flex items-start gap-2.5 rounded-lg border border-border px-3 py-2.5 text-xs">
                <Checkbox
                  checked={restart}
                  disabled={busy}
                  onCheckedChange={(v) => setRestart(v === true)}
                  className="mt-0.5 data-[state=checked]:border-primary data-[state=checked]:bg-primary"
                  aria-label="关闭编辑器，写入完成后自动重开"
                />
                <span className="min-w-0 leading-5">
                  <span className="font-medium">
                    关闭 {meta.label} 再写入，完成后自动重开
                  </span>
                  <span className="mt-0.5 block text-muted-foreground">
                    编辑器退出时会把内存里的索引与消息回写磁盘，覆盖本次写入；关不掉时后端会放弃这次操作。
                  </span>
                </span>
              </label>
            ) : null}

            <AlertDialogFooter>
              <AlertDialogCancel disabled={busy}>取消</AlertDialogCancel>
              <Button type="button" disabled={busy} onClick={() => void run()}>
                {busy ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : null}
                {busy ? "处理中…" : "确认复制"}
              </Button>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </DialogContent>
    </Dialog>
  );
}

/** 预览配对对话框：选来源与目标成员 → 调 preview-pair → 展示判定（不写入）。 */
function ExtPreviewPairDialog({
  group,
  client,
  open,
  onOpenChange,
}: {
  group: ExtSessionGroup | null;
  client: ExtSessionClient;
  open: boolean;
  onOpenChange: (v: boolean) => void;
}) {
  const [sourceId, setSourceId] = useState("");
  const [targetId, setTargetId] = useState("");
  const [busy, setBusy] = useState(false);
  const [preview, setPreview] = useState<ExtSessionPreviewPair | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) {
      setPreview(null);
      setError(null);
      setBusy(false);
      return;
    }
    if (group) {
      // 默认来源：后端给出的安全源；没有就退到第一个可读成员。
      const preferred =
        group.safe_source_member_id ||
        group.members.find((m) => m.readable)?.member_id ||
        group.members[0]?.member_id ||
        "";
      setSourceId(preferred);
      setTargetId(group.members.find((m) => m.member_id !== preferred)?.member_id ?? "");
      setPreview(null);
      setError(null);
    }
  }, [open, group]);

  if (!group) return null;

  const verdict = preview
    ? (EXT_VERDICT_STYLE[preview.verdict] ?? EXT_VERDICT_STYLE.unknown)
    : null;

  const run = async () => {
    if (!sourceId || !targetId) return;
    setBusy(true);
    setError(null);
    try {
      setPreview(
        await previewExtSessionPair(group.id, {
          client,
          sourceMemberId: sourceId,
          targetMemberId: targetId,
        }),
      );
    } catch (err) {
      setPreview(null);
      setError(describeError(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>预览配对判定</DialogTitle>
          <DialogDescription>
            选择一对成员，检查「以来源为准写入目标」会怎么处理。
            <span className="mt-1 block font-medium text-foreground">这一步只检查，不写入。</span>
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-2 sm:grid-cols-2">
          <label className="block space-y-1">
            <span className="text-[11px] font-medium">来源（以此为准）</span>
            <select
              aria-label="来源成员"
              className="w-full cursor-pointer rounded-md border border-border bg-background px-2 py-1.5 text-xs"
              value={sourceId}
              disabled={busy}
              onChange={(e) => {
                setSourceId(e.target.value);
                setPreview(null);
              }}
            >
              {group.members.map((m) => (
                <option key={m.member_id} value={m.member_id}>
                  {m.label}
                  {m.readable ? "" : "（不可读）"}
                </option>
              ))}
            </select>
          </label>
          <label className="block space-y-1">
            <span className="text-[11px] font-medium">目标（将被写入）</span>
            <select
              aria-label="目标成员"
              className="w-full cursor-pointer rounded-md border border-border bg-background px-2 py-1.5 text-xs"
              value={targetId}
              disabled={busy}
              onChange={(e) => {
                setTargetId(e.target.value);
                setPreview(null);
              }}
            >
              {group.members.map((m) => (
                <option key={m.member_id} value={m.member_id}>
                  {m.label}
                  {m.readable ? "" : "（不可读）"}
                </option>
              ))}
            </select>
          </label>
        </div>

        {preview ? (
          <div className="space-y-2">
            <div className="rounded-lg border border-border bg-muted/40 px-3 py-2.5">
              <p className={cn("text-sm font-medium", verdict?.tone)}>{verdict?.label}</p>
              <p className="mt-0.5 text-xs leading-5 text-muted-foreground">{preview.reason}</p>
              <p className="mt-1 text-xs text-muted-foreground">
                独有记录：来源 {preview.source_only} 条 · 目标 {preview.target_only} 条
              </p>
              <p className="mt-1 text-xs text-muted-foreground">
                可用的写入模式：
                {preview.available_modes.length > 0
                  ? preview.available_modes.map((m) => EXT_MODE_LABEL[m] ?? m).join("；")
                  : "无（判定不允许写入）"}
              </p>
            </div>
            <p className="text-[11px] leading-4 text-muted-foreground">
              预览凭据由服务端签发并在执行时逐字段复核；内容之后再变化，该项执行会被跳过并报告。
            </p>
          </div>
        ) : null}

        {error ? (
          <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2.5 text-xs text-destructive">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <span className="min-w-0 leading-5">{error}</span>
          </div>
        ) : null}

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={() => onOpenChange(false)}
          >
            关闭
          </Button>
          <Button
            type="button"
            size="sm"
            disabled={busy || !sourceId || !targetId}
            onClick={() => void run()}
          >
            {busy ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <Layers className="size-3.5" aria-hidden="true" />
            )}
            预览
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function Shell({ children }: { children: ReactNode }) {
  return <div className="mx-auto w-full max-w-[1180px] space-y-12 px-6 py-8 sm:px-8 sm:py-9">{children}</div>;
}

function Row({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex flex-wrap items-baseline justify-between gap-2">
      <span className="shrink-0 text-muted-foreground">{label}</span>
      <span className={cn("min-w-0 truncate", mono && "font-mono text-xs")} title={value}>
        {value}
      </span>
    </div>
  );
}

/** 从客户端版本信息对象里拼一句可读文本。 */
function describeVersion(info?: Record<string, unknown>): string {
  if (!info || info.present === false) return "";
  const name = typeof info.appName === "string" ? info.appName : "";
  const ver = typeof info.appVersion === "string" ? info.appVersion : "";
  const build = typeof info.build === "string" ? info.build.slice(0, 8) : "";
  const parts = [name, ver, build ? "build " + build : ""].filter(Boolean);
  return parts.join(" ");
}
