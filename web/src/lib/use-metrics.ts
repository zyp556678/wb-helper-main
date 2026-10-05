import { useCallback, useEffect, useRef, useState } from "react";

import { describeError, fetchMetrics, isUnauthorized } from "@/lib/api";
import { useVisibleInterval } from "@/lib/use-visible-interval";
import type { MetricsResponse } from "@/lib/types";

/** 监控页轮询间隔。 */
export const METRICS_POLL_MS = 5000;

export interface MetricsData {
  metrics: MetricsResponse | null;
  loading: boolean;
  error: string | null;
  /** 后端要求 api_key 而当前密钥缺失 / 不正确。 */
  unauthorized: boolean;
  /** 手动刷新（带 loading）；轮询走静默刷新，不闪骨架。 */
  reload: () => void;
}

/** 运行指标数据源：挂载即拉取，之后每 5 秒静默刷新一次（页面不可见时暂停）。 */
export function useMetrics(): MetricsData {
  const [metrics, setMetrics] = useState<MetricsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [unauthorized, setUnauthorized] = useState(false);
  // 轮询可能比 interval 慢，用 ref 防止请求叠加。
  const inFlight = useRef(false);

  const load = useCallback(async (silent: boolean) => {
    if (inFlight.current) return;
    inFlight.current = true;
    if (!silent) setLoading(true);
    try {
      const next = await fetchMetrics();
      setMetrics(next);
      setError(null);
      setUnauthorized(false);
    } catch (err) {
      setError(describeError(err));
      setUnauthorized(isUnauthorized(err));
    } finally {
      inFlight.current = false;
      if (!silent) setLoading(false);
    }
  }, []);

  const reload = useCallback(() => {
    void load(false);
  }, [load]);

  useEffect(() => {
    void load(false);
  }, [load]);

  useVisibleInterval(() => {
    void load(true);
  }, METRICS_POLL_MS);

  return { metrics, loading, error, unauthorized, reload };
}
