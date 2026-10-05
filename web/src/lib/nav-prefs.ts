import { useEffect, useState } from "react";

/**
 * 左侧导航入口的显示偏好（对齐 wb-switch 的 `src/lib/nav-prefs.ts`）。
 *
 * 语义：
 * - 关闭「关联会话」后，左侧导航不再渲染该入口；会话数据、关联关系与账号库
 *   都不受影响，重新打开即恢复。
 * - 默认值：开启。
 *
 * 持久化在 localStorage（与 `workbuddy-gateway.tools.<id>` / `.theme` 同风格），
 * 键名 `workbuddy-gateway.nav.sessions`，值 `"1"` / `"0"`；缺省时为开启。
 */
const SESSIONS_NAV_STORAGE_KEY = "workbuddy-gateway.nav.sessions";

/** 同页面内的变更通知（`storage` 事件只在其它标签页触发）。 */
const NAV_PREFS_CHANGED_EVENT = "workbuddy-gateway:nav-prefs-changed";

export function isSessionsNavEnabled(): boolean {
  try {
    return localStorage.getItem(SESSIONS_NAV_STORAGE_KEY) !== "0";
  } catch {
    return true;
  }
}

export function setSessionsNavEnabled(enabled: boolean): void {
  try {
    localStorage.setItem(SESSIONS_NAV_STORAGE_KEY, enabled ? "1" : "0");
  } catch {
    /* 隐私模式等场景下写入失败：开关本次会话内仍可见（由调用方 state 驱动） */
  }
  window.dispatchEvent(new Event(NAV_PREFS_CHANGED_EVENT));
}

/** 订阅「关联会话」入口的显示偏好；设置页改动后（同一标签页）侧边栏立即同步。 */
export function useSessionsNavEnabled(): boolean {
  const [enabled, setEnabled] = useState(isSessionsNavEnabled);
  useEffect(() => {
    const sync = () => setEnabled(isSessionsNavEnabled());
    window.addEventListener("storage", sync);
    window.addEventListener(NAV_PREFS_CHANGED_EVENT, sync);
    return () => {
      window.removeEventListener("storage", sync);
      window.removeEventListener(NAV_PREFS_CHANGED_EVENT, sync);
    };
  }, []);
  return enabled;
}
