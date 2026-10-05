import { useEffect, useState } from "react";

/**
 * 逐包明细的排序模式（对齐参考实现 wb2api-panel 29e2749 的 pkSortMode）。
 *
 *   - `end_asc`   到期升序（默认）—— 快过期的包排最前，提醒优先消耗；
 *                 无到期时间（长期有效）的包统一垫底，不掺进日期序里。
 *   - `size_desc` 面额降序 —— 同面额再按到期升序，看「钱从哪来」。
 *
 * 偏好持久化在 localStorage（键名沿用本项目 `workbuddy-gateway.*` 风格），
 * 读写整段兜住异常（隐私模式 / 禁用存储时回落默认值，不打扰用户）。
 * 同页多处（每张账号卡的弹窗）通过自定义事件同步，跨标签页靠 `storage` 事件 ——
 * 与 lib/supported-tools.ts 同一套写法。
 */
export const PACKAGE_SORT_MODES = ["end_asc", "size_desc"] as const;
export type PackageSortMode = (typeof PACKAGE_SORT_MODES)[number];

/** 选择器与说明文案（与参考实现 PK_SORT_LABELS 同口径）。 */
export const PACKAGE_SORT_LABELS: Record<PackageSortMode, string> = {
  end_asc: "按到期升序 · 近的在前",
  size_desc: "按面额降序",
};

/** localStorage 键：值非法 / 缺失一律回落 end_asc。 */
export const PACKAGE_SORT_KEY = "workbuddy-gateway.package_sort";

/** 同页面内的变更通知（`storage` 事件只在其它标签页触发）。 */
const PACKAGE_SORT_CHANGED_EVENT = "workbuddy-gateway:package-sort-changed";

export function isPackageSortMode(value: string | null | undefined): value is PackageSortMode {
  return (PACKAGE_SORT_MODES as readonly string[]).includes(value ?? "");
}

/** 读排序偏好；localStorage 抛异常或值非法时回落默认 `end_asc`。 */
export function readPackageSortMode(): PackageSortMode {
  try {
    const stored = localStorage.getItem(PACKAGE_SORT_KEY);
    return isPackageSortMode(stored) ? stored : "end_asc";
  } catch {
    return "end_asc";
  }
}

/** 写排序偏好并广播变更；写失败（隐私模式等）时本次会话内仍由调用方 state 生效。 */
export function setPackageSortMode(mode: PackageSortMode): void {
  try {
    localStorage.setItem(PACKAGE_SORT_KEY, mode);
  } catch {
    /* 存不了就只在本次会话生效，不值得打扰用户。 */
  }
  window.dispatchEvent(new Event(PACKAGE_SORT_CHANGED_EVENT));
}

/**
 * 订阅逐包排序模式：[当前模式, 设置函数]。
 *
 * 设置函数走模块级的事件广播，因此同一个页面里多个订阅者（每张账号卡一个）
 * 在任一处切换后立即同步；数据已在内存，切换只重排、不重新请求。
 */
export function usePackageSortMode(): [PackageSortMode, (mode: PackageSortMode) => void] {
  const [mode, setMode] = useState<PackageSortMode>(readPackageSortMode);
  useEffect(() => {
    const sync = () => setMode(readPackageSortMode());
    window.addEventListener("storage", sync);
    window.addEventListener(PACKAGE_SORT_CHANGED_EVENT, sync);
    return () => {
      window.removeEventListener("storage", sync);
      window.removeEventListener(PACKAGE_SORT_CHANGED_EVENT, sync);
    };
  }, []);
  return [mode, setPackageSortMode];
}
