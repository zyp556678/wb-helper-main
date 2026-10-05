// 三态主题（system / light / dark）：逻辑照搬 wb-switch 的 src/lib/theme.ts。
import { useCallback, useEffect, useState } from "react";

export type ThemePreference = "system" | "light" | "dark";

const THEME_STORAGE_KEY = "workbuddy-gateway.theme";

const DARK_QUERY = "(prefers-color-scheme: dark)";

/** 同页面内的变更通知（`storage` 事件只在其它标签页触发）。 */
const THEME_CHANGED_EVENT = "workbuddy-gateway:theme-changed";

function isThemePreference(value: string | null): value is ThemePreference {
  return value === "system" || value === "light" || value === "dark";
}

export function getThemePreference(): ThemePreference {
  try {
    const stored = localStorage.getItem(THEME_STORAGE_KEY);
    if (isThemePreference(stored)) return stored;
  } catch {
    // 存储不可用时仍可跟随系统主题。
  }

  return "system";
}

export function applyTheme(preference: ThemePreference) {
  const dark =
    preference === "dark" ||
    (preference === "system" && window.matchMedia(DARK_QUERY).matches);
  document.documentElement.classList.toggle("dark", dark);
}

export function setThemePreference(preference: ThemePreference) {
  try {
    localStorage.setItem(THEME_STORAGE_KEY, preference);
  } catch {
    // 存储不可用时仍立即应用当前选择。
  }
  applyTheme(preference);
  window.dispatchEvent(new Event(THEME_CHANGED_EVENT));
}

/**
 * 订阅主题偏好的 hook：侧栏切换器与配置页的「外观」分组共用。
 *
 * 两处入口同时修改一个 localStorage 键，靠这个 hook 的事件通知保持同步 ——
 * 否则在配置页选了「深色」，侧栏切换器的激活态还停在旧值，
 * 用户会以为设置没生效（实际已生效，只是那一处没同步）。
 */
export function useThemePreference(): [ThemePreference, (next: ThemePreference) => void] {
  const [preference, setPreference] = useState<ThemePreference>(getThemePreference);

  useEffect(() => {
    const sync = () => setPreference(getThemePreference());
    window.addEventListener("storage", sync);
    window.addEventListener(THEME_CHANGED_EVENT, sync);
    return () => {
      window.removeEventListener("storage", sync);
      window.removeEventListener(THEME_CHANGED_EVENT, sync);
    };
  }, []);

  const select = useCallback((next: ThemePreference) => setThemePreference(next), []);
  return [preference, select];
}

/** 订阅系统主题变化；仅在偏好为 system 时生效。返回取消订阅函数。 */
export function watchSystemTheme() {
  const media = window.matchMedia(DARK_QUERY);
  const onChange = () => {
    if (getThemePreference() === "system") applyTheme("system");
  };

  media.addEventListener("change", onChange);
  return () => media.removeEventListener("change", onChange);
}
