import { useEffect, useRef } from "react";

/**
 * 页面可见时按 intervalMs 周期性调用 callback；切到后台立刻暂停，恢复可见时立即补一次。
 *
 * callback 存在 ref 里，调用方不必为它写依赖，也不会因为每次渲染换函数而重排定时器。
 */
export function useVisibleInterval(callback: () => void, intervalMs: number, enabled = true) {
  const callbackRef = useRef(callback);

  useEffect(() => {
    callbackRef.current = callback;
  }, [callback]);

  useEffect(() => {
    if (!enabled || intervalMs <= 0) return;

    let timer: number | undefined;
    const stop = () => {
      if (timer !== undefined) {
        window.clearInterval(timer);
        timer = undefined;
      }
    };
    const start = () => {
      stop();
      timer = window.setInterval(() => callbackRef.current(), intervalMs);
    };
    const sync = () => {
      if (document.hidden) {
        stop();
        return;
      }
      // 恢复可见时先补一次，避免后台期间的数据断档。
      callbackRef.current();
      start();
    };

    if (!document.hidden) start();
    document.addEventListener("visibilitychange", sync);
    return () => {
      document.removeEventListener("visibilitychange", sync);
      stop();
    };
  }, [intervalMs, enabled]);
}
