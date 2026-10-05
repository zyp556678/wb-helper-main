import { useEffect, useState } from "react";

import { nowSeconds } from "@/lib/format";

/** 每秒返回当前 Unix 秒，用于冷却倒计时本地递减；页面隐藏时暂停刷新以省电。 */
export function useNowSeconds(intervalMs = 1000): number {
  const [now, setNow] = useState(nowSeconds);

  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) setNow(nowSeconds());
    }, intervalMs);
    return () => window.clearInterval(timer);
  }, [intervalMs]);

  return now;
}
