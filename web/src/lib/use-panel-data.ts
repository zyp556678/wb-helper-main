import { useCallback, useEffect, useState } from "react";

import { describeError, fetchAccounts, fetchOverview, isUnauthorized } from "@/lib/api";
import type { Account, Overview } from "@/lib/types";

export interface PanelData {
  overview: Overview | null;
  accounts: Account[];
  loading: boolean;
  error: string | null;
  /** 后端要求 api_key 而当前密钥缺失 / 不正确。 */
  unauthorized: boolean;
  reload: () => void;
  /**
   * 静默刷新：与 reload 拉同一份数据，但**不翻 loading**（不闪骨架屏）。
   * 供周期轮询使用（如账号卡片上的模型限额倒计时，5 分钟一次）。
   */
  refresh: () => void;
}

/** 面板数据源：overview 与 accounts 一起拉，共用一个 loading / error 状态。 */
export function usePanelData(): PanelData {
  const [overview, setOverview] = useState<Overview | null>(null);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [unauthorized, setUnauthorized] = useState(false);

  const load = useCallback(async (silent: boolean) => {
    if (!silent) setLoading(true);
    try {
      const [nextOverview, nextAccounts] = await Promise.all([fetchOverview(), fetchAccounts()]);
      setOverview(nextOverview);
      // Go 侧 nil slice 会序列化成 null，这里兜底成空数组。
      setAccounts(nextAccounts.accounts ?? []);
      setError(null);
      setUnauthorized(false);
    } catch (err) {
      setError(describeError(err));
      setUnauthorized(isUnauthorized(err));
    } finally {
      if (!silent) setLoading(false);
    }
  }, []);

  const reload = useCallback(() => void load(false), [load]);
  const refresh = useCallback(() => void load(true), [load]);

  useEffect(() => {
    void load(false);
  }, [load]);

  return { overview, accounts, loading, error, unauthorized, reload, refresh };
}
