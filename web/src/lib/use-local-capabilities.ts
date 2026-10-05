import { useEffect, useState } from "react";

import { fetchLocalCapabilities } from "@/lib/api";

/**
 * 探测本机能力是否可用（用于隐藏侧栏入口）。
 *
 * 只探一次、失败即当作不可用：本机代理在服务端部署下本来就不存在，
 * 反复重试只会刷日志。用户手工装好二进制后重启本机代理，刷新页面即可。
 */
export function useLocalCapabilities(): boolean {
  const [available, setAvailable] = useState(false);

  useEffect(() => {
    let cancelled = false;
    fetchLocalCapabilities()
      .then((caps) => {
        if (!cancelled) setAvailable(Boolean(caps.available));
      })
      .catch(() => {
        if (!cancelled) setAvailable(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return available;
}
