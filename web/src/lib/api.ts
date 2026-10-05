import type {
  Account,
  AccountExportDoc,
  AccountMetaUpdateResponse,
  AccountsMetaResponse,
  AccountsResponse,
  AccountSite,
  AutostartStatus,
  CheckinAllSummary,
  CheckinLogsResponse,
  CheckinStatusResponse,
  ClearLogsResponse,
  ConfigPatch,
  CreditStatsResponse,
  CreditsResponse,
  DailySnapshot,
  DisplayField,
  ExtSessionClient,
  ExtSessionCopyResult,
  ExtSessionPreviewPair,
  ExtSessionUnifyResult,
  ExtSessionsResponse,
  ImportAccountsResult,
  ImportPreviewResponse,
  LocalAppBackupsResponse,
  LocalAppsActionResult,
  LocalAppsResponse,
  LocalAppTargetID,
  LocalSessionCopyResult,
  LocalSessionsResponse,
  LocalSessionSyncResult,
  LocalSessionGroupsResponse,
  LocalSessionGroupDetailResponse,
  RecoveryReportItem,
  GroupSyncReport,
  AddGroupMemberResult,
  LoginPollResponse,
  LoginSitesResponse,
  LocalCapabilities,
  LocalHooksResponse,
  LocalLogsResponse,
  LocalOverview,
  LocalPathsResponse,
  LoginStartResponse,
  LogPathsResponse,
  LogsResponse,
  MetricsResponse,
  ModelsProbeRequest,
  ModelsProbeResponse,
  ModelsResponse,
  OfficialUsageResponse,
  Overview,
  PanelConfig,
  QuotaAllResponse,
  RefreshModelsResponse,
  RequestLogsResponse,
  RequestMetricsResponse,
  RemoveAccountResponse,
  SaveConfigResponse,
  SchedulerRunResponse,
  SchedulerTask,
  SitesResponse,
  SiteRouteResponse,
  StatsPurgeResponse,
  StatsResponse,
  AccountTaskAutoAllResponse,
  AccountTaskAutoResponse,
  TaskActionResponse,
  TaskQueueState,
  TaskRunOptions,
  TaskRunResponse,
  TaskScanResponse,
  TaskVouchersResponse,
  TokenStatistics,
} from "@/lib/types";

/** 所有请求走相对路径，前缀与 Go 侧路由一致。 */
export const API_BASE = "/panel/api";

const API_KEY_STORAGE = "workbuddy-gateway.api_key";

/**
 * 后端配置了 api_key 时需带 Authorization: Bearer <key>；
 * 未配置时后端不校验，这里留空即可。
 */
export function getApiKey(): string {
  try {
    return localStorage.getItem(API_KEY_STORAGE) ?? "";
  } catch {
    return "";
  }
}

export function setApiKey(key: string) {
  try {
    if (key) localStorage.setItem(API_KEY_STORAGE, key);
    else localStorage.removeItem(API_KEY_STORAGE);
  } catch {
    // 存储不可用时本次会话仍按内存中的输入重试。
  }
}

export class ApiError extends Error {
  readonly status: number;
  /** 后端返回 401/403，需要用户补 api_key。 */
  readonly unauthorized: boolean;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.unauthorized = status === 401 || status === 403;
  }
}

function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  if (error instanceof TypeError) return "无法连接网关，请确认服务已启动";
  return error instanceof Error ? error.message : "未知错误";
}

interface RequestOptions {
  method?: "GET" | "POST" | "PATCH";
  /** 会序列化成 JSON；undefined 时请求不带 body。 */
  body?: unknown;
  /** 供轮询等场景在关闭时取消在途请求。 */
  signal?: AbortSignal;
}

/**
 * 从错误响应体里抽出可展示的文案。
 *
 * 后端存在两套错误信封，必须都认：
 *  ① `{"error":{"code":400,"message":"..."}}` —— 统一错误体（errBody）。
 *  ② `{"ok":false,"detail":"..."}` —— 任务/成长/本机代理这类**动作型**端点。
 *     它们把业务理由放在**平铺的 detail** 里。
 *
 * 漏认 ② 的后果不是「少一句话」，而是**把可解释的业务结果变成一句
 * 「请求失败（HTTP 502）」**：用户点「猫咪出发」，后端明明回了
 * 「国际站没有成长中心活动」，界面却只显示一个无从下手的 502。
 * 这类端点的 HTTP 状态码本身也不携带信息（业务拒绝与上游故障都是 502），
 * 所以 error 字段缺失时必须继续找 detail。
 */
async function readErrorMessage(response: Response): Promise<string> {
  let raw = "";
  try {
    raw = await response.text();
  } catch {
    // 错误响应体不可读时退回状态码文案。
  }
  if (!raw) return "";

  try {
    const parsed = JSON.parse(raw) as {
      error?: { message?: string } | string;
      message?: string;
      detail?: string;
    };
    const nested = typeof parsed.error === "object" ? parsed.error?.message : parsed.error;
    return (nested || parsed.message || parsed.detail || "").trim();
  } catch {
    return raw.trim().slice(0, 200);
  }
}

/** 统一出口：错误一律转成 ApiError，调用方只需读 message / unauthorized。 */
async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = "GET", body, signal } = options;
  const key = getApiKey();
  const hasBody = body !== undefined;
  let response: Response;

  try {
    response = await fetch(`${API_BASE}${path}`, {
      method,
      signal,
      headers: {
        Accept: "application/json",
        ...(hasBody ? { "Content-Type": "application/json" } : {}),
        ...(key ? { Authorization: `Bearer ${key}` } : {}),
      },
      ...(hasBody ? { body: JSON.stringify(body) } : {}),
    });
  } catch (error) {
    // 取消请求不该被当成失败展示，交由调用方按 signal.aborted 判断。
    if (signal?.aborted) throw error;
    throw new ApiError(0, errorMessage(error));
  }

  if (!response.ok) {
    const detail = await readErrorMessage(response);
    throw new ApiError(response.status, detail || `请求失败（HTTP ${response.status}）`);
  }

  return (await response.json()) as T;
}

/**
 * 动作型端点的出口：**4xx 不抛异常**，而是把响应体原样交回给调用方。
 *
 * 为什么需要它：任务 / 成长活动这类端点用扁平信封 `{ok:false, detail:"..."}`
 * 表达「这次动作没做成」，其中**相当一部分是正常业务结果**，而不是故障：
 * 「今日出发次数已用完」「国际站没有成长中心活动」「猫咪还在路上」……
 *
 * 这些响应用 request() 会被当成异常抛出，于是调用方精心写好的
 * `if (result.ok) toast.success(...) else toast.error(result.detail)`
 * **永远走不到 else 分支**，只能落到 catch 里显示一句「请求失败（HTTP 502）」，
 * 把「次数用完」讲成了「网关坏了」。
 *
 * 所以这里对 <500 的状态不发难：它们是调用方能解释的业务结果。
 * 只有 5xx（服务端真的出问题）仍然抛出，交由统一错误处理。
 */
async function actionRequest<T>(path: string, body: unknown): Promise<T> {
  const key = getApiKey();
  let response: Response;
  try {
    response = await fetch(`${API_BASE}${path}`, {
      method: "POST",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/json",
        ...(key ? { Authorization: `Bearer ${key}` } : {}),
      },
      body: JSON.stringify(body),
    });
  } catch (error) {
    throw new ApiError(0, errorMessage(error));
  }

  if (response.status >= 500) {
    const detail = await readErrorMessage(response);
    throw new ApiError(response.status, detail || `请求失败（HTTP ${response.status}）`);
  }

  // 4xx 也尝试解析 JSON（后端在 4xx 上同样返回 {ok:false,detail}）；
  // 解析不出来时兜一个 ok:false，保证调用方拿到的形状稳定。
  try {
    return (await response.json()) as T;
  } catch {
    const detail = await readErrorMessage(response);
    return { ok: false, detail: detail || `请求失败（HTTP ${response.status}）` } as T;
  }
}

/**
 * 本机代理走 /panel/local/*（反代到本机代理进程），与 /panel/api 前缀不同——
 * 它属于「另一个进程的接口」，只是借网关转发。这里单独一个出口，
 * 不把两套前缀混进同一个 request()。
 */
const LOCAL_BASE = "/panel/local";

async function localRequest<T>(path: string, signal?: AbortSignal): Promise<T> {
  const key = getApiKey();
  let response: Response;
  try {
    response = await fetch(LOCAL_BASE + path, {
      signal,
      headers: {
        Accept: "application/json",
        ...(key ? { Authorization: "Bearer " + key } : {}),
      },
    });
  } catch (error) {
    if (signal?.aborted) throw error;
    throw new ApiError(0, errorMessage(error));
  }
  if (!response.ok) {
    const detail = await readErrorMessage(response);
    throw new ApiError(response.status, detail || "请求失败（HTTP " + response.status + "）");
  }
  return (await response.json()) as T;
}

export function fetchOverview() {
  return request<Overview>("/overview");
}

export function fetchAccounts() {
  return request<AccountsResponse>("/accounts");
}

/** 刷新单账号额度，返回热加载后的账号对象。 */
export async function refreshAccountQuota(id: string) {
  const { account } = await request<{ account: Account }>(
    `/accounts/${encodeURIComponent(id)}/quota`,
    { method: "POST" },
  );
  return account;
}

/**
 * 完整识别单账号套餐（POST /accounts/{id}/plan）。
 *
 * 与 refreshAccountQuota 的差别：额度刷新只给摘要级套餐名（有订阅编码、无有效期），
 * 这里会再查一次分页权益列表，回答「这条订阅现在还有效吗、是哪一档」。
 * 多打一次上游请求，所以只做成按需触发。
 */
export function identifyAccountPlan(id: string) {
  return actionRequest<{ ok: boolean; plan: string; detail?: string; account?: Account }>(
    `/accounts/${encodeURIComponent(id)}/plan`,
    {},
  );
}

export async function setAccountDisabled(id: string, disabled: boolean) {
  const action = disabled ? "disable" : "enable";
  const { account } = await request<{ account: Account }>(
    `/accounts/${encodeURIComponent(id)}/${action}`,
    { method: "POST" },
  );
  return account;
}

/**
 * 人工复活账号：清除禁用 + 冷却 + 熔断 + 连败降权。
 *
 * 与 `setAccountDisabled(id, false)` 的区别是**刻意保留的**：那一项只改「禁用」位
 *（用在「上次禁用错了」的场景），而复活是运维口径的无条件恢复。
 * 余额类硬冷却不清 —— 那一条的依据是「余额为 0」这个客观事实，点按钮不会让余额回来。
 */
export async function reviveAccount(id: string) {
  const { account } = await request<{ account: Account }>(
    `/accounts/${encodeURIComponent(id)}/revive`,
    { method: "POST" },
  );
  return account;
}

/** delete_file=false 仅从账号池移除，true 时同时删除凭据文件（不可逆）。 */
export function removeAccount(id: string, deleteFile: boolean) {
  return request<RemoveAccountResponse>(`/accounts/${encodeURIComponent(id)}/remove`, {
    method: "POST",
    body: { delete_file: deleteFile },
  });
}

/** 刷新全部账号额度。 */
export function refreshAllQuota() {
  return request<QuotaAllResponse>("/quota_all", { method: "POST" });
}

// -----------------------------------------------------------------------------
// 账号导入 / 导出 / 刷新 Token / 签到
// -----------------------------------------------------------------------------

/**
 * 导出账号为 JSON。`ids` 为空数组表示导出全部（后端按此约定处理）。
 *
 * 只取数据、不落盘：文件由前端做 Blob 下载。服务端落盘要处理「选目录」
 * 这类跨平台问题，而浏览器下载已经解决了。
 */
export function exportAccounts(ids: string[] = []) {
  const query = ids.length > 0 ? "?ids=" + encodeURIComponent(ids.join(",")) : "";
  return request<AccountExportDoc>("/accounts/export" + query);
}

/**
 * 解析备份文件并返回预览。
 *
 * 入参是**文件原始内容**，不是已解析的对象 —— 这样 token 全程留在
 * 「文件 → 服务端」这条路径上，不必先经浏览器解析再回传一遍。
 */
export function previewImportAccounts(raw: string) {
  return request<ImportPreviewResponse>("/accounts/import/preview", {
    method: "POST",
    body: { raw },
  });
}

/**
 * 按勾选写入账号。`indexes` 是预览项在文件里的下标。
 *
 * overwrite 为假时，与现有账号冲突的项**跳过**而非覆盖（后端返回 skipped 说明原因）。
 */
export function importAccounts(raw: string, indexes: number[], overwrite: boolean) {
  return request<ImportAccountsResult>("/accounts/import", {
    method: "POST",
    body: { raw, indexes, overwrite },
  });
}

/** 强制刷新该账号的登录令牌（无条件刷新，不判断是否临近过期）。 */
export async function refreshAccountToken(id: string) {
  const { account } = await request<{ account: Account }>(
    `/accounts/${encodeURIComponent(id)}/refresh-token`,
    { method: "POST" },
  );
  return account;
}

/**
 * 国际站账号：完成注册激活（补注册地区）并领取 trial 加油包。
 *
 * 新注册的国际站账号没补地区时对话会直接报 `14017 trial not activated` ——
 * 登录与导入都会自动跑一次，这个按钮是那次失败后的「再试一次」。
 */
export function activateIntlAccount(id: string) {
  return actionRequest<{ ok: boolean; detail?: string; activated?: boolean; trial_claimed?: boolean }>(
    `/accounts/${encodeURIComponent(id)}/intl-activate`,
    {},
  );
}

/**
 * 手动为该账号签到。
 *
 * 「今天已签到」是成功而非失败（上游返回业务码 10001/14001），
 * 由返回的 `already` 区分，不要把它当异常处理。
 */
export function checkinAccount(id: string) {
  return request<{ ok: boolean; already: boolean; detail: string; account: Account }>(
    `/accounts/${encodeURIComponent(id)}/checkin`,
    { method: "POST" },
  );
}

/**
 * 批量查询各账号的「今日是否已签到」。
 *
 * 收拢成一个批量端点而不是每张卡各自请求：状态查询是「每账号一次上游请求」，
 * 让 N 张卡各发一次就是把 N 个并发请求从浏览器铺到上游。服务端做了 TTL 缓存
 * （见返回的 `ttl`）与并发限流，且对不支持签到的站点直接不发请求。
 */
export function fetchCheckinStatus(signal?: AbortSignal) {
  return request<CheckinStatusResponse>("/accounts/checkin-status", {
    method: "POST",
    body: {},
    signal,
  });
}

// -----------------------------------------------------------------------------
// 批次 4：账号备注 / 显示字段 / 批量刷新积分并签到
// -----------------------------------------------------------------------------

/** 全部账号的备注与显示字段（GET /panel/api/accounts/meta）。 */
export function fetchAccountsMeta(signal?: AbortSignal) {
  return request<AccountsMetaResponse>("/accounts/meta", { signal });
}

/**
 * 更新某账号的备注 / 显示字段（PATCH /panel/api/accounts/{id}/meta）。
 *
 * 字段缺席 = 不改；显式空串 = 清除。备注上限 24 字符（后端校验，超限 400）。
 */
export function updateAccountMeta(
  id: string,
  patch: { note?: string; display_field?: DisplayField | "" },
) {
  return request<AccountMetaUpdateResponse>(`/accounts/${encodeURIComponent(id)}/meta`, {
    method: "PATCH",
    body: patch,
  });
}

/**
 * 批量「刷新积分并签到」（POST /panel/api/accounts/checkin_all）。
 *
 * 遵守签到时间段：窗口外整轮跳过签到（outside_window=true），但积分仍会刷新。
 * 排除名单（已关闭自动签到的账号）不参与签到；单账号手动签到不受影响。
 */
export function checkinAllAccounts() {
  return request<CheckinAllSummary>("/accounts/checkin_all", { method: "POST" });
}

// -----------------------------------------------------------------------------
// 本机应用接入
// -----------------------------------------------------------------------------

/** 探测本机各目标应用的安装情况与当前登录账号。 */
export function fetchLocalApps(signal?: AbortSignal) {
  return request<LocalAppsResponse>("/local-apps", { signal });
}

/**
 * 把某个账号写入某个本机目标。
 *
 * `site` 只对 WorkBuddy 客户端有意义（国内站与国际站是两个独立文件）；
 * `allowPlaintext` 是「允许写入明文凭据」的显式确认 —— 目标客户端若用加密信封
 * 存凭据，而我们又拿不到它的密钥，就只能写明文，这一步必须由用户点头。
 */
export function switchLocalApp(params: {
  target: LocalAppTargetID;
  accountId: string;
  site?: string;
  allowPlaintext?: boolean;
}) {
  return request<LocalAppsActionResult>("/local-apps/switch", {
    method: "POST",
    body: {
      target: params.target,
      account_id: params.accountId,
      site: params.site ?? "",
      allow_plaintext: params.allowPlaintext ?? false,
    },
  });
}

/** 本机登录态的备份列表（每次写入前都会自动备份）。 */
export function fetchLocalAppBackups(signal?: AbortSignal) {
  return request<LocalAppBackupsResponse>("/local-apps/backups", { signal });
}

/** 列出本机 WorkBuddy 客户端的会话（`uid` 非空时只列该账号的）。 */
export function fetchLocalSessions(uid?: string, signal?: AbortSignal) {
  const q = uid && uid.trim() ? `?uid=${encodeURIComponent(uid.trim())}` : "";
  return request<LocalSessionsResponse>(`/local-sessions${q}`, { signal });
}

/**
 * 把一条会话复制给某个账号。
 *
 * `dryRun` 只做检查与备份、不写任何东西 —— 界面把它做成默认第一步，
 * 因为这一步会读 71MB 级的正文，先预演能让用户看清代价再决定。
 */
export function copyLocalSession(params: { sessionId: string; accountId: string; dryRun?: boolean }) {
  return request<LocalSessionCopyResult>("/local-sessions/copy", {
    method: "POST",
    body: { session_id: params.sessionId, account_id: params.accountId, dry_run: params.dryRun ?? false },
  });
}

/**
 * 把来源会话的内容同步到目标会话（两者应是同一段对话的两份副本）。
 *
 * `force` 允许覆盖分叉的目标 —— 那会替换目标账号的**完整内容**，
 * 可能是用户在另一个账号里继续写的东西，所以必须由用户显式确认。
 */
export function syncLocalSession(params: { sourceId: string; targetId: string; force?: boolean }) {
  return request<LocalSessionSyncResult>("/local-sessions/sync", {
    method: "POST",
    body: { source_id: params.sourceId, target_id: params.targetId, force: params.force ?? false },
  });
}

/** 关联会话分组（同一段对话的多份副本聚成一组）。 */
export function fetchSessionGroups(signal?: AbortSignal) {
  return request<LocalSessionGroupsResponse>("/local-sessions/groups", { signal });
}

/**
 * 恢复未完成的会话写入（POST /local-sessions/recover）。
 *
 * **必须客户端已退出**：会话写入会被运行中客户端的退出回写覆盖，所以运行中后端
 * 返回 409 并说明原因。不传 variants 时恢复全部有待恢复项的档位。
 */
export function recoverLocalSessions(variants?: string[]) {
  return request<{
    ok: boolean;
    reports: Record<string, RecoveryReportItem>;
    note?: string;
    error?: string;
  }>("/local-sessions/recover", {
    method: "POST",
    body: { variants: variants ?? [] },
  });
}

/**
 * 单个关联组的详情（含可关联账号列表）。
 *
 * 详情弹窗的「重新检查」与同步后的刷新都走它；组不存在时后端返回 404。
 */
export function fetchSessionGroup(groupId: string, signal?: AbortSignal) {
  return request<LocalSessionGroupDetailResponse>(
    `/local-sessions/groups/${encodeURIComponent(groupId)}`,
    { signal },
  );
}

/**
 * 预览一对成员的同步判定（POST /groups/{id}/preview-pair）。
 *
 * 返回 verdict / 可用写入模式 / 原因 / 两边独有记录数 —— 前端「以此为准」确认框
 * 据此列出每个目标会怎么处理，判定不允许写入的目标列为阻断项（对照 switch 的预览流程）。
 */
export function previewSessionGroupPair(
  groupId: string,
  params: { sourceMemberId: string; targetMemberId: string },
) {
  return request<{
    verdict: string;
    reason: string;
    source_only: number;
    target_only: number;
    available_modes: string[];
    /**
     * 服务端签发的预览凭据 id：执行同步时必须原样带回。
     *
     * 服务端把判定所依赖的全部版本信息（组指纹、双方身份与正文摘要、基线、结论）
     * 存在自己那边，只把 id 交给前端 —— 伪造/篡改参数不能扩大权限，执行时逐字段复核，
     * 任何版本变化都会跳过该项（reason_code=previewStale）。
     */
    preview_token: string;
  }>(`/local-sessions/groups/${encodeURIComponent(groupId)}/preview-pair`, {
    method: "POST",
    body: { source_member_id: params.sourceMemberId, target_member_id: params.targetMemberId },
  });
}

/**
 * 组级统一：以某成员为准，逐目标按**模式 + 预览凭据**写入
 * （POST /groups/{id}/unify，body {source_member_id, targets:[{member_id, mode, preview_token}]}）。
 *
 * 每个目标的模式与凭据都由预览结果给出；执行前后端用同一套算法重算绑定并逐字段复核，
 * 任何版本变化进 skipped（reason_code=previewStale），判定不再允许该模式则报错。
 */
export function syncSessionGroupUnify(
  groupId: string,
  params: {
    sourceMemberId: string;
    targets: { memberId: string; mode: string; previewToken: string }[];
  },
) {
  return request<GroupSyncReport>(`/local-sessions/groups/${encodeURIComponent(groupId)}/unify`, {
    method: "POST",
    body: {
      source_member_id: params.sourceMemberId,
      targets: params.targets.map((t) => ({
        member_id: t.memberId,
        mode: t.mode,
        preview_token: t.previewToken,
      })),
    },
  });
}

/**
 * 批量同步落后账号（POST /groups/{id}/safe-batch）。
 *
 * 后端重算安全源并对每个落后副本重新复核 fast-forward，不再属于快进范围的项进 skipped。
 */
export function syncSessionGroupSafeBatch(groupId: string) {
  return request<GroupSyncReport>(`/local-sessions/groups/${encodeURIComponent(groupId)}/safe-batch`, {
    method: "POST",
  });
}

/**
 * 组级同步（旧接口，保留兼容）。
 *
 * 不给 `sourceMemberId` 时后端自动挑「安全源」（内容最全且其余成员都是它有序前缀的那个），
 * 只做纯追加；不安全/不可读的成员进 skipped 而不是报错。给了来源则逐对同步，
 * `force` 允许覆盖分叉的目标（会替换目标完整内容）。
 */
export function syncSessionGroup(
  groupId: string,
  params: { sourceMemberId?: string; force?: boolean } = {},
) {
  return request<GroupSyncReport>(`/local-sessions/groups/${encodeURIComponent(groupId)}/sync`, {
    method: "POST",
    body: { source_member_id: params.sourceMemberId ?? "", force: params.force ?? false },
  });
}

/**
 * 复制并关联到新账号：把来源成员的副本复制给目标账号。
 *
 * 「关联」不需要登记 —— 副本内容与来源同源，下一次分组自动进组。
 * 组 ID 可能因新副本的 id 更小而变化，响应里的 `group_id` 是刷新后应定位的组。
 */
export function addSessionGroupMember(
  groupId: string,
  params: { sourceMemberId: string; targetUid: string },
) {
  return request<AddGroupMemberResult>(`/local-sessions/groups/${encodeURIComponent(groupId)}/add`, {
    method: "POST",
    body: { source_member_id: params.sourceMemberId, target_uid: params.targetUid },
  });
}

/**
 * 取消某个成员与组的关联（POST /local-sessions/groups/{id}/unlink）。
 *
 * **只解除分组关系，不碰会话内容**（对照 wb-switch：「账号里的会话内容不会被删除」）。
 * 组内只剩一份副本时，这个组在页面上自然消失。
 */
export function unlinkSessionGroupMember(groupId: string, memberId: string) {
  return request<{
    ok: boolean;
    member_id: string;
    group_removed: boolean;
    remaining: number;
    notes?: string[];
  }>(`/local-sessions/groups/${encodeURIComponent(groupId)}/unlink`, {
    method: "POST",
    body: { member_id: memberId },
  });
}

/**
 * 删除整个会话组（组内成员全部解除关联）。
 *
 * 语义与 wb-switch 的 delete_group 一致：**只解除关联，不删会话内容**。
 */
export function deleteSessionGroup(groupId: string) {
  return request<{ ok: boolean; removed: number; notes?: string[] }>(
    `/local-sessions/groups/${encodeURIComponent(groupId)}/delete`,
    { method: "POST" },
  );
}

// -----------------------------------------------------------------------------
// 扩展数据仓会话（VS Code 内 CodeBuddy 插件 / CodeBuddy IDE）
//
// 四个端点由网关直接读写扩展的数据仓（不经本机代理）。与 WorkBuddy 侧同一套
// 语义（复制 + 登记、预览判定、执行前复核），差别在会话形态与生命周期守卫：
// 写入前编辑器必须完全退出（运行中写入会被它的退出回写覆盖）。
// -----------------------------------------------------------------------------

/**
 * 扩展数据仓的状态、可复制会话与关联组
 * （GET /local-sessions/ext?client=vscode&uid=<源账号>）。
 *
 * `uid` 为空时 sessions 为空（后端不按无账号的请求枚举会话），groups 不受影响。
 */
export function fetchExtSessions(
  client: ExtSessionClient,
  uid?: string,
  signal?: AbortSignal,
) {
  const q = new URLSearchParams({ client });
  if (uid && uid.trim()) q.set("uid", uid.trim());
  return request<ExtSessionsResponse>(`/local-sessions/ext?${q.toString()}`, { signal });
}

/**
 * 复制并登记：把来源账号的会话写入目标账号的数据仓
 * （POST /local-sessions/ext/copy，body {client, source_uid, target_uid, items, restart}）。
 *
 * `restart` 只影响编辑器正在运行的场景：false 时后端**直接拒绝**（不假装写入），
 * true 时先关闭编辑器、写完再重开。无论哪种情况，目标工作区索引都会先备份到
 * `<工作目录>/backups/<kind>/<utc>/`（结果里的 report.backup 给出实际路径）。
 */
export function copyExtSessions(params: {
  client: ExtSessionClient;
  sourceUid: string;
  targetUid: string;
  /** 复制项 = (工作区 hash, 会话 id)，两者都是 32 位小写 hex。 */
  items: { workspaceHash: string; conversationId: string }[];
  restart?: boolean;
}) {
  return request<ExtSessionCopyResult>("/local-sessions/ext/copy", {
    method: "POST",
    body: {
      client: params.client,
      source_uid: params.sourceUid,
      target_uid: params.targetUid,
      restart: params.restart ?? false,
      items: params.items.map((it) => ({
        workspace_hash: it.workspaceHash,
        conversation_id: it.conversationId,
      })),
    },
  });
}

/**
 * 预览一对成员的同步判定（POST /ext/groups/{id}/preview-pair）。
 *
 * 只检查不写入：返回 verdict / 可用写入模式 / 原因 / 两边独有记录数。
 * 服务端同时签发预览凭据（预览与执行时都重算绑定），执行统一时由后端自行复核。
 */
export function previewExtSessionPair(
  groupId: string,
  params: { client: ExtSessionClient; sourceMemberId: string; targetMemberId: string },
) {
  return request<ExtSessionPreviewPair>(
    `/local-sessions/ext/groups/${encodeURIComponent(groupId)}/preview-pair`,
    {
      method: "POST",
      body: {
        client: params.client,
        source_member_id: params.sourceMemberId,
        target_member_id: params.targetMemberId,
      },
    },
  );
}

/**
 * 整组统一（POST /ext/groups/{id}/unify，body {client, source_member_id, targets, restart}）。
 *
 * `targets` 是「成员 id → 写入模式」；每个目标的模式都由预览给出。后端执行前会
 * 重新计算预览并逐项复核，内容已变化的项进 outcomes（不 applied）而不是静默覆盖；
 * 覆盖的目标会话会整目录备份到 `<工作目录>/backups/<kind>/overwrite/<utc>/…`。
 */
export function unifyExtSessionGroup(
  groupId: string,
  params: {
    client: ExtSessionClient;
    sourceMemberId: string;
    targets: Record<string, string>;
    restart?: boolean;
  },
) {
  return request<ExtSessionUnifyResult>(
    `/local-sessions/ext/groups/${encodeURIComponent(groupId)}/unify`,
    {
      method: "POST",
      body: {
        client: params.client,
        source_member_id: params.sourceMemberId,
        targets: params.targets,
        restart: params.restart ?? false,
      },
    },
  );
}

/** 用备份恢复本机登录态。 */
export function restoreLocalAppBackup(backupId: string) {
  return request<LocalAppsActionResult>("/local-apps/restore", {
    method: "POST",
    body: { backup_id: backupId },
  });
}

export function fetchLoginSites() {
  return request<LoginSitesResponse>("/login/sites");
}

export function startLogin(site: AccountSite) {
  return request<LoginStartResponse>("/login/start", { method: "POST", body: { site } });
}

export function pollLogin(loginId: string, signal?: AbortSignal) {
  return request<LoginPollResponse>(`/login/poll?id=${encodeURIComponent(loginId)}`, { signal });
}

/**
 * 让**网关所在机器**用系统默认浏览器打开链接（POST /panel/api/open-url）。
 *
 * 只在桌面壳里 `window.open` 被 WebView 拦掉时用（拦掉是静默的，返回值是 null）。
 * 网关只对来自本机（回环）的请求生效；远程访问面板时返回 403，
 * 这时应提示用户复制链接自己打开 —— 在服务器上开浏览器毫无意义。
 */
export function openExternalUrl(url: string) {
  return request<{ ok: boolean }>("/open-url", { method: "POST", body: { url } });
}

/** 运行指标快照，供监控页轮询（GET /panel/api/metrics）。 */
export function fetchMetrics(signal?: AbortSignal) {
  return request<MetricsResponse>("/metrics", { signal });
}

/** 可展示配置（GET /panel/api/config）。 */
export function fetchConfig() {
  return request<PanelConfig>("/config");
}

/** 保存配置子集（POST /panel/api/config），返回生效情况与最新配置。 */
export function saveConfig(patch: ConfigPatch) {
  return request<SaveConfigResponse>("/config", { method: "POST", body: patch });
}

/**
 * 手动触发定时任务（POST /panel/api/scheduler/run）。
 *
 * `respectWindow` 只对 checkin 有意义：传 true 时遵守配置的签到时间段，
 * 窗口外整轮跳过（响应里 skipped=true）。显式点击的「立即签到」不该传它 ——
 * 窗口是策略约束，不该否决一个明确的用户意图。
 */
export function runSchedulerTask(task: SchedulerTask, opts?: { respectWindow?: boolean }) {
  return request<SchedulerRunResponse>("/scheduler/run", {
    method: "POST",
    body: { task, respect_window: opts?.respectWindow ?? false },
  });
}

/** 模型目录快照（GET /panel/api/models）。 */
export function fetchModels() {
  return request<ModelsResponse>("/models");
}

/** 强制刷新模型目录（POST /panel/api/models/refresh）。 */
export function refreshModels() {
  return request<RefreshModelsResponse>("/models/refresh", { method: "POST" });
}

/**
 * 探测模型价格（POST /panel/api/models/probe）。
 * 注意：后端会向该模型发一次极短的对话请求，真实消耗账号额度。
 */
export function probeModels(body: ModelsProbeRequest) {
  return request<ModelsProbeResponse>("/models/probe", { method: "POST", body });
}

/** GET /panel/api/logs 的可选查询参数。 */
export interface LogsQuery {
  /** 频道过滤：request / outbound / governance / task / system。 */
  channel?: string;
  /** 级别过滤：info / warn / error。 */
  level?: string;
  /** 关键词，匹配 message / model / account。 */
  q?: string;
  /** 返回条数，默认 200、上限 500。 */
  limit?: number;
}

/** 事件日志快照（GET /panel/api/logs）。 */
export function fetchLogs(params: LogsQuery = {}, signal?: AbortSignal) {
  const search = new URLSearchParams();
  if (params.channel) search.set("channel", params.channel);
  if (params.level) search.set("level", params.level);
  const keyword = params.q?.trim();
  if (keyword) search.set("q", keyword);
  if (params.limit) search.set("limit", String(params.limit));
  const query = search.toString();
  return request<LogsResponse>(`/logs${query ? `?${query}` : ""}`, { signal });
}

/** 清空事件日志（POST /panel/api/logs/clear）。 */
export function clearLogs() {
  return request<ClearLogsResponse>("/logs/clear", { method: "POST" });
}

/** GET /panel/api/request-logs 的可选查询参数。 */
export interface RequestLogsQuery {
  /** 结局过滤：success / http_error / stream_error / interrupted。 */
  outcome?: string;
  /** 账号关键词（模糊匹配脱敏标签）。 */
  account?: string;
  /** 模型关键词（模糊匹配）。 */
  model?: string;
  /** 返回条数，默认 200、上限 1000。 */
  limit?: number;
}

/**
 * 进程内请求指标（GET /panel/api/request-metrics）。
 *
 * 与「日志」页的区别：那边是**事件流**（发生了什么），这里是**请求级明细**
 * （每次请求一条，含状态码/耗时/TTFB/token）。回答的是「刚才那次 502 是哪次请求」。
 */
export function fetchRequestMetrics(signal?: AbortSignal) {
  return request<RequestMetricsResponse>("/request-metrics", { signal });
}

/** 从 JSONL 归档读取最近请求（GET /panel/api/request-logs）。 */
export function fetchRequestLogs(params: RequestLogsQuery = {}, signal?: AbortSignal) {
  const search = new URLSearchParams();
  if (params.outcome) search.set("outcome", params.outcome);
  const account = params.account?.trim();
  if (account) search.set("account", account);
  const model = params.model?.trim();
  if (model) search.set("model", model);
  if (params.limit) search.set("limit", String(params.limit));
  const query = search.toString();
  return request<RequestLogsResponse>(`/request-logs${query ? `?${query}` : ""}`, { signal });
}

export function describeError(error: unknown): string {
  return errorMessage(error);
}

export function isUnauthorized(error: unknown): boolean {
  return error instanceof ApiError && error.unauthorized;
}

/** 请求是否因调用方取消而中止。 */
export function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === "AbortError";
}

// -----------------------------------------------------------------------------
// 任务中心（切片 5）
// -----------------------------------------------------------------------------

/** 任务中心数据（GET /panel/api/tasks）；refresh 为真时强制重扫。 */
export function fetchTasks(refresh = false) {
  return request<TaskScanResponse>("/tasks" + (refresh ? "?refresh=1" : ""));
}

/** 强制重新扫描全部账号的任务状态（POST /panel/api/tasks/scan）。 */
export function scanTasks() {
  return request<TaskScanResponse>("/tasks/scan", { method: "POST" });
}

/**
 * 触发任务队列执行（POST /panel/api/tasks/run），后端异步跑。
 *
 * 走 actionRequest 而不是 request：
 * 「已有队列在执行中」后端返回 409 + 一句可读的 detail，
 * 那是**当前状态不允许**，不是故障。用 request() 会把它抛成异常，
 * 调用方精心写的 planned/accepted 分流逻辑在失败时完全走不到，
 * 只剩一句笼统的「请求失败（HTTP 409）」。
 */
export function runTasks(options: TaskRunOptions = {}) {
  return actionRequest<TaskRunResponse>("/tasks/run", options);
}

/**
 * 队列执行状态（GET /panel/api/tasks/queue）。
 *
 * 这里做一次**防御性归一化**，把 `items` 补成数组。
 *
 * 起因是一个真实事故：Go 侧 `append([]T(nil), 空切片...)` 会得到 nil，
 * 序列化成 `"items": null`；前端 `items.length` 直接抛
 * 「Cannot read properties of null (reading 'length')」，
 * 整个任务中心页崩成「这个页面出错了」，而错误信息完全指不出是队列字段的问题。
 *
 * 后端已经修了根因，但这类「一个字段为 null 就让整页白屏」的脆弱性不该只靠
 * 后端守规矩 —— 在数据入口收口一次，比在每个用到的地方写可选链更不容易漏。
 */
export async function fetchTasksQueue(signal?: AbortSignal): Promise<TaskQueueState> {
  const state = await request<TaskQueueState>("/tasks/queue", { signal });
  return {
    ...state,
    running: state?.running === true,
    items: Array.isArray(state?.items) ? state.items : [],
    total: typeof state?.total === "number" ? state.total : 0,
    done: typeof state?.done === "number" ? state.done : 0,
    failed: typeof state?.failed === "number" ? state.failed : 0,
  };
}

/**
 * 手动补一条对话活跃上报（点亮连登 / 解锁领养前置）。
 *
 * 走 actionRequest：端点在「前提不满足」等业务结果上返回 4xx，
 * 那不是故障，应当把 detail 交给调用方展示，而不是抛成异常。
 */
export function reportTaskActivity(account: string, model = "") {
  return actionRequest<TaskActionResponse>("/tasks/report", { account, model });
}

/**
 * 连登管家（POST /tasks/streak-bonus）：补签 → 礼包/补偿 → 兑换已解锁档位 → 抽完所有次数。
 *
 * 幂等：已领的档位、无次数的抽奖、无卡的补签都会自动跳过，多点几次不会重复发奖。
 * account 为空表示对全部可用账号执行。
 */
export function runStreakBonus(account = "") {
  return actionRequest<{ ok: boolean; detail?: string; accounts?: unknown[] }>(
    "/tasks/streak-bonus",
    { account_id: account },
  );
}

/** 成长活动动作：猫咪出发 / 领旅行奖励 / 连登兑换 / 抽奖。 */
export function growthAction(account: string, action: string, tier = "") {
  return actionRequest<TaskActionResponse>("/growth/action", { account, action, tier });
}

/**
 * 账号级单任务动作（POST /accounts/{id}/tasks/auto）。
 *
 * 与队列执行的区别：这是**同步**返回逐项结果的动作，弹窗直接展示
 *（做了什么、进度从哪到哪、领了多少），不需要再轮询队列。
 * 「账号在忙」后端返回 409 + detail，属于当前状态不允许，不是故障 → actionRequest。
 */
export function runAccountTask(account: string, taskCode: string) {
  return actionRequest<AccountTaskAutoResponse>(
    `/accounts/${encodeURIComponent(account)}/tasks/auto`,
    { task_code: taskCode },
  );
}

/** 账号级一键完成全部可自动任务（POST /accounts/{id}/tasks/auto_all）。 */
export function runAccountTasksAll(account: string) {
  return actionRequest<AccountTaskAutoAllResponse>(
    `/accounts/${encodeURIComponent(account)}/tasks/auto_all`,
    {},
  );
}

/** 开学季券码（POST /panel/api/tasks/vouchers）；account 为空表示全部账号。 */
export function fetchTaskVouchers(account = "") {
  return actionRequest<TaskVouchersResponse>("/tasks/vouchers", { account_id: account });
}

/** 用量统计（GET /panel/api/stats）；range 支持 24h / 7d / 30d。 */
export function fetchStats(range: string, signal?: AbortSignal) {
  return request<StatsResponse>("/stats?range=" + encodeURIComponent(range), { signal });
}

/** 按天统计（GET /panel/api/stats/daily）；range 支持 today / 7d / 30d / 90d / 180d / 1y。 */
export function fetchStatsDaily(range: string, signal?: AbortSignal) {
  return request<DailySnapshot>("/stats/daily?range=" + encodeURIComponent(range), { signal });
}

/** Token 统计（GET /panel/api/token-stats）；days 为扫描窗口天数。 */
export function fetchTokenStats(days: number, signal?: AbortSignal) {
  return request<TokenStatistics>("/token-stats?days=" + String(days), { signal });
}

/** 清空统计数据（POST /panel/api/stats/purge）。 */
export function purgeStats() {
  return request<StatsPurgeResponse>("/stats/purge", { method: "POST" });
}

/** 官方计费口径的请求用量（GET /panel/api/stats/official）；days 为查询天数。 */
export function fetchOfficialUsage(days: number, signal?: AbortSignal) {
  return request<OfficialUsageResponse>("/stats/official?days=" + String(days), { signal });
}

/**
 * 积分资源包明细（GET /panel/api/credits）。
 *
 * 与 fetchOfficialUsage 是账本的两端：那个说「花了多少」，这个说「还剩多少、是什么包」。
 * 两条都打上游，所以并行发起、各自失败各自标注。
 */
export function fetchCredits(signal?: AbortSignal) {
  return request<CreditsResponse>("/credits", { signal });
}

/**
 * 本地观察口径的积分统计（GET /panel/api/credits/stats，切片 19）。
 *
 * 纯本地快照台账，不打上游 —— 官方用量接口不可用时页面用这里的
 * summary/daily 回退显示（「测不到」不等于「没发生」）。
 */
export function fetchCreditStatistics(signal?: AbortSignal) {
  return request<CreditStatsResponse>("/credits/stats", { signal });
}

// -----------------------------------------------------------------------------
// 本机代理（切片 7）
// -----------------------------------------------------------------------------

/** 本机代理能力与状态（GET /panel/api/local/capabilities）。 */
export function fetchLocalCapabilities(signal?: AbortSignal) {
  return request<LocalCapabilities>("/local/capabilities", { signal });
}

/**
 * 手动重启本机代理（POST /panel/api/local/restart）。
 *
 * 走 actionRequest：重启失败时后端返回的 4xx/5xx 带 detail，
 * 之前 request() 会把它抛成异常，调用方那行 `toast.success(res.detail)`
 * 在失败时永远走不到，只能显示一句笼统的「请求失败」。
 */
export function restartLocalAgent() {
  return actionRequest<{ ok: boolean; detail?: string }>("/local/restart", {});
}

/**
 * 本机数据总览。
 *
 * 注意路径：本机代理挂在 /panel/local/* 下（不在 /panel/api 里），
 * 因为它是反代到另一个进程的通道，不是网关自己的接口。
 */
export function fetchLocalOverview(signal?: AbortSignal) {
  return localRequest<LocalOverview>("/api/local/overview", signal);
}

export function fetchLocalPaths(signal?: AbortSignal) {
  return localRequest<LocalPathsResponse>("/api/local/paths", signal);
}

export function fetchLocalLogs(signal?: AbortSignal) {
  return localRequest<LocalLogsResponse>("/api/local/logs", signal);
}

export function fetchLocalLogTail(file: string, tail = 200, signal?: AbortSignal) {
  const qs = new URLSearchParams({ file, tail: String(tail) });
  return localRequest<LocalLogsResponse>("/api/local/logs?" + qs.toString(), signal);
}

export function fetchLocalHooks(signal?: AbortSignal) {
  return localRequest<LocalHooksResponse>("/api/local/hooks", signal);
}

// -----------------------------------------------------------------------------
// 双站视图（切片 8）
// -----------------------------------------------------------------------------

/** 双站对照视图（GET /panel/api/sites）。 */
export function fetchSites() {
  return request<SitesResponse>("/sites");
}

/**
 * 站点路由的用法说明（GET /panel/api/site-route）。
 *
 * 下发的词表就是后端**实际生效**的那一份，因此界面不会教出后端不认的写法。
 */
export function fetchSiteRoute(signal?: AbortSignal) {
  return request<SiteRouteResponse>("/site-route", { signal });
}

// -----------------------------------------------------------------------------
// 开机自启动（切片 9）
// -----------------------------------------------------------------------------

/**
 * 查询开机自启动状态（GET /panel/api/autostart）。
 *
 * 返回的是**系统里的真实状态**（注册表 / plist / .desktop），不是网关配置。
 * 因此用户绕过面板改动过自启项时，这里读到的也是最新的。
 */
export function fetchAutostart(signal?: AbortSignal) {
  return request<AutostartStatus>("/autostart", { signal });
}

/**
 * 开启 / 关闭开机自启动（POST /panel/api/autostart）。
 *
 * 冲突时后端返回 409（桌面版与命令行版共用 8317 端口），此处透传为 ApiError；
 * 调用方拿到后应重新 `fetchAutostart()` 取最新状态，并引导用户二选一。
 */
export function setAutostart(enabled: boolean) {
  return request<AutostartStatus>("/autostart", {
    method: "POST",
    body: { enabled },
  });
}

// -----------------------------------------------------------------------------
// 批次 5：设置页只读查询（签到日志 / 日志落点 / 最近请求日志下载）
// -----------------------------------------------------------------------------

/** 签到日志（GET /panel/api/checkin/logs，纯本地台账，不打上游）。 */
export function fetchCheckinLogs(days = 30, signal?: AbortSignal) {
  return request<CheckinLogsResponse>(`/checkin/logs?days=${days}`, { signal });
}

/** 日志落点（GET /panel/api/logs/paths）：请求归档目录 + 数据目录的真实路径。 */
export function fetchLogPaths(signal?: AbortSignal) {
  return request<LogPathsResponse>("/logs/paths", { signal });
}

/**
 * 下载最近一个请求日志归档（GET /panel/api/logs/download）。
 *
 * 面板可能配了 api_key，普通 `<a href>` 带不上 Authorization 头，
 * 所以这里用 fetch 取 blob、由调用方触发下载；name 为空表示「最近一个」。
 */
export async function downloadRequestLog(
  name = "",
): Promise<{ blob: Blob; filename: string }> {
  const key = getApiKey();
  const query = name ? `?name=${encodeURIComponent(name)}` : "";
  let response: Response;
  try {
    response = await fetch(`${API_BASE}/logs/download${query}`, {
      headers: { ...(key ? { Authorization: `Bearer ${key}` } : {}) },
    });
  } catch (error) {
    throw new ApiError(0, errorMessage(error));
  }
  if (!response.ok) {
    const detail = await readErrorMessage(response);
    throw new ApiError(response.status, detail || `下载失败（HTTP ${response.status}）`);
  }
  const blob = await response.blob();
  const disposition = response.headers.get("Content-Disposition") ?? "";
  const match = /filename="([^"]+)"/.exec(disposition);
  return { blob, filename: match?.[1] || "request-log.jsonl" };
}
