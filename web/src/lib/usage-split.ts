/**
 * 用量来源拆分：把请求量拆成「经过网关的（反代）」与「不经网关的（官方直连）」。
 *
 * ## 这个拆分的口径（改之前先读完）
 *
 * - **反代侧是测出来的**：网关每次请求完成即累加，见 `internal/stats.Record`。
 *   它只覆盖**经过网关**的流量。
 * - **官方账本是全部流量**：WorkBuddy 自己记的请求用量，含反代与直连两路。
 *   （网关向上游发请求时，上游同样会记一笔，所以官方账本是反代的超集。）
 * - **官方直连只能减出来**：`官方账本 − 反代计数`。它**不是独立测量值**，
 *   页面上必须如实标注，不能让用户以为这是两条各自量出来的曲线。
 *
 * ## 为什么积分不能一起拆
 *
 * 官方账本有逐请求的 `credit`，但**反代侧没有对应的量**：
 * `RecordInput` 里只有 token，没有 credit；网关也从不读取账本的 `request_id`
 * （`internal/upstream/client.go` 用自己生成的 id），出站请求不带任何可识别标记。
 * 因此没有任何依据把「这 742 积分里有 300 是反代的」算出来 —— 编一个出来
 * 比空着更糟。积分只给官方合计，并说明它含两路。
 *
 * ## 窗口口径
 *
 * 两侧都必须按**本地日历日**切；反代的天桶原先按 UTC 日切，在 GMT+8 下
 * 「今天」从本地 08:00 才起算，与官方账本错开 8 小时，减出来的直连量会
 * 系统性偏大。已在 `internal/stats` 把天桶改成本地日（schema 3）。
 *

 */
import type { OfficialUsageResponse } from "@/lib/types";

export type UsageSplitState =
  /** 两侧都有数据，可以拆。 */
  | "ready"
  /** 官方账本不可用：未登录 / 接口失败 / 该站点不提供该端点 / 无可用账号。 */
  | "no-official"
  /** 官方账本记录数少于反代计数，减不出正数（账本延迟或窗口错位）。 */
  | "ledger-below"
  /** 官方命中上游分页上限，账本本身不完整，差值不可信。 */
  | "truncated";

export interface UsageSplit {
  state: UsageSplitState;
  /** 反代请求数（精确计数）。 */
  reverseRequests: number;
  /** 官方账本合计（反代 + 直连）。不可用时为 null。 */
  officialRequests: number | null;
  /** 官方账本的积分消耗合计。**无法拆分到反代侧**，只作整体参照。 */
  officialCredit: number | null;
  /** 官方直连请求数（差分推算）。不可拆时为 null，绝不填 0 冒充数据。 */
  directRequests: number | null;
  /** 官方侧不可用/不完整的原因，用于如实标注。 */
  reason?: string;
}

/** 累加官方账本的按天聚合。字段缺失按 0 处理（接口契约是数组，但不做信任假设）。 */
export function sumOfficialDays(official: OfficialUsageResponse | null | undefined): {
  requests: number;
  credit: number;
  days: number;
} {
  const days = official?.days ?? [];
  let requests = 0;
  let credit = 0;
  for (const d of days) {
    requests += d?.requests ?? 0;
    credit += d?.credit ?? 0;
  }
  return { requests, credit, days: days.length };
}

/**
 * 官方账本是否被上游分页截断。
 *
 * 上游按 `pageSize` 返回，超过部分不会翻页（见 `upstream.OfficialUsage`），
 * 而每个账号的 `total` 是上游自报的**真实条数**。两者不等即说明账本缺行，
 * 此时差值不可信 —— 宁可标注不完整，也不要给一个偏小的官方数。
 */
export function officialTruncated(official: OfficialUsageResponse | null | undefined): boolean {
  const accounts = official?.accounts ?? [];
  const reported = accounts.reduce((sum, a) => sum + (a?.total ?? 0), 0);
  return reported > 0 && reported > sumOfficialDays(official).requests;
}

/** 官方侧失败原因（取第一个非空 error），用于把「为什么不可用」说清楚。 */
function officialReason(official: OfficialUsageResponse | null | undefined): string | undefined {
  const failed = (official?.accounts ?? []).filter((a) => a?.error);
  if (failed.length > 0) {
    const first = failed[0];
    return `${failed.length} 个账号读取失败（如 ${first.nickname || first.id}：${first.error}）`;
  }
  return undefined;
}

/**
 * 计算拆分。`reverseRequests` 取当前范围的反代口径合计。
 *
 * 注意「官方账本记录数少于反代计数」这个分支：它**不是异常流量**，而是
 * 账本有延迟或窗口错位。这时唯一诚实的做法是把直连标成不可得，
 * 而不是显示一个负的直连数或用 0 掩盖。
 */
export function splitUsage(
  reverseRequests: number,
  official: OfficialUsageResponse | null | undefined,
): UsageSplit {
  const base: UsageSplit = {
    state: "no-official",
    reverseRequests,
    officialRequests: null,
    officialCredit: null,
    directRequests: null,
  };

  if (!official) {
    return { ...base, reason: "官方账本未取到数据" };
  }

  const { requests, credit, days } = sumOfficialDays(official);
  if (days === 0) {
    const reason = officialReason(official);
    return {
      ...base,
      reason:
        reason ??
        (official.ok_count === 0
          ? "没有可用的账号，或该站点不提供官方用量接口"
          : "官方账本在所选范围内没有记录"),
    };
  }

  const partial: UsageSplit = {
    ...base,
    officialRequests: requests,
    officialCredit: credit,
  };

  if (officialTruncated(official)) {
    return {
      ...partial,
      state: "truncated",
      reason: "官方账本命中上游分页上限，本次取到的明细不完整",
    };
  }

  if (requests < reverseRequests) {
    return {
      ...partial,
      state: "ledger-below",
      reason: "官方账本记录数少于反代计数，可能是账本延迟或统计窗口尚未对齐",
    };
  }

  return {
    state: "ready",
    reverseRequests,
    officialRequests: requests,
    officialCredit: credit,
    directRequests: requests - reverseRequests,
  };
}
