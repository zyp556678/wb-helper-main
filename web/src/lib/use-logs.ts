import { useCallback, useEffect, useRef, useState } from "react";

import { describeError, fetchLogs, isAbortError, isUnauthorized } from "@/lib/api";
import type { LogsResponse } from "@/lib/types";
import { useVisibleInterval } from "@/lib/use-visible-interval";

/** 日志页轮询间隔。 */
export const LOGS_POLL_MS = 2000;

/** 单次拉取的事件上限（后端上限 500）。 */
export const LOGS_FETCH_LIMIT = 500;

/** 日志筛选条件；level / channel 用空串表示「全部」。 */
export interface LogsFilter {
  level: string;
  channel: string;
  q: string;
}

export interface LogsData {
  logs: LogsResponse | null;
  loading: boolean;
  error: string | null;
  /** 后端要求 api_key 而当前密钥缺失 / 不正确。 */
  unauthorized: boolean;
  /** 带 loading 的重新拉取（首屏 / 失败重试 / 清空后回读）。 */
  reload: () => void;
}

/**
 * 日志数据源：筛选条件变化立即拉取，其后按 autoRefresh 每 2 秒静默刷新
 * （页面不可见时暂停）。搜索词延迟 300ms 再发请求，避免逐字触发。
 */
export function useLogs(filter: LogsFilter, autoRefresh: boolean): LogsData {
  const { level, channel, q } = filter;
  const [logs, setLogs] = useState<LogsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [unauthorized, setUnauthorized] = useState(false);
  const [debouncedQ, setDebouncedQ] = useState(q);
  const controllerRef = useRef<AbortController | null>(null);
  // 在途请求的筛选条件：同一条件重复触发（轮询撞上手动刷新）直接复用，不互相取消。
  const inFlightKeyRef = useRef<string | null>(null);

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedQ(q), 300);
    return () => window.clearTimeout(timer);
  }, [q]);

  const load = useCallback(
    async (silent: boolean, keyword: string) => {
      const key = `${level}\u0000${channel}\u0000${keyword}`;
      if (inFlightKeyRef.current === key) return;

      // 筛选条件变了：作废在途请求，避免旧响应盖住新结果。
      controllerRef.current?.abort();
      const controller = new AbortController();
      controllerRef.current = controller;
      inFlightKeyRef.current = key;
      if (!silent) setLoading(true);

      try {
        const next = await fetchLogs(
          {
            level: level || undefined,
            channel: channel || undefined,
            q: keyword || undefined,
            limit: LOGS_FETCH_LIMIT,
          },
          controller.signal,
        );
        setLogs(next);
        setError(null);
        setUnauthorized(false);
      } catch (err) {
        if (isAbortError(err)) return;
        setError(describeError(err));
        setUnauthorized(isUnauthorized(err));
      } finally {
        // 被更新条件的请求顶掉时不再收尾，交给新请求。
        if (controllerRef.current === controller) {
          controllerRef.current = null;
          inFlightKeyRef.current = null;
          if (!silent) setLoading(false);
        }
      }
    },
    [level, channel],
  );

  const reload = useCallback(() => {
    void load(false, debouncedQ);
  }, [load, debouncedQ]);

  // 筛选变化（含搜索词防抖后）立即重新拉取。
  useEffect(() => {
    void load(false, debouncedQ);
  }, [load, debouncedQ]);

  useVisibleInterval(
    () => {
      void load(true, debouncedQ);
    },
    LOGS_POLL_MS,
    autoRefresh,
  );

  // 卸载时取消在途请求。
  useEffect(() => () => controllerRef.current?.abort(), []);

  return { logs, loading, error, unauthorized, reload };
}
