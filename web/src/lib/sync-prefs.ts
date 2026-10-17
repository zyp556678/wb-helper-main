import { useEffect, useState } from "react";

/**
 * 切换账号 / 统一会话时，「仅同步归档状态」这个可选项的默认勾选偏好
 * （对照 wb-switch 0.1.61 的「切换账号时默认勾选归档同步」#165）。
 *
 * 语义：
 * - 关闭（默认）：每次都要手动勾选「仅同步归档状态」—— 这与本项目原有的
 *   「默认不勾、防误操作」一致，是安全默认值。
 * - 开启：**只有**「正文已一致、且后端给出 archive_action」的目标会被预勾选；
 *   任何有正文写入的项都不受影响。勾选归档会写目标会话的 status 列，
 *   虽然可逆，但仍是一次写操作，所以默认值必须是关。
 *
 * 持久化在 localStorage（与 `workbuddy-gateway.nav.sessions` 同风格），
 * 键名 `workbuddy-gateway.sync.default-archive`，值 `"1"` / `"0"`。
 */
const ARCHIVE_SYNC_STORAGE_KEY = "workbuddy-gateway.sync.default-archive";

const SYNC_PREFS_CHANGED_EVENT = "workbuddy-gateway:sync-prefs-changed";

/**
 * 写入失败（隐私模式 / 配额满）时的内存兜底。
 *
 * 只靠 localStorage 会让开关「点开就弹回」：写入失败 → 派发事件 → 监听方重读
 * 存储 → 还是旧值。用一个模块级变量记住「本会话内用户选了什么」。
 */
let memoryFallback: boolean | null = null;

export function isDefaultArchiveSyncEnabled(): boolean {
  if (memoryFallback !== null) return memoryFallback;
  try {
    return localStorage.getItem(ARCHIVE_SYNC_STORAGE_KEY) === "1";
  } catch {
    return false;
  }
}

export function setDefaultArchiveSyncEnabled(enabled: boolean): void {
  try {
    localStorage.setItem(ARCHIVE_SYNC_STORAGE_KEY, enabled ? "1" : "0");
    memoryFallback = null;
  } catch {
    // 写不进去时用内存态保证「本次选择」立即生效，而不是弹回旧值。
    memoryFallback = enabled;
  }
  window.dispatchEvent(new Event(SYNC_PREFS_CHANGED_EVENT));
}

export function useDefaultArchiveSync(): boolean {
  const [enabled, setEnabled] = useState(isDefaultArchiveSyncEnabled);
  useEffect(() => {
    const sync = () => setEnabled(isDefaultArchiveSyncEnabled());
    window.addEventListener("storage", sync);
    window.addEventListener(SYNC_PREFS_CHANGED_EVENT, sync);
    return () => {
      window.removeEventListener("storage", sync);
      window.removeEventListener(SYNC_PREFS_CHANGED_EVENT, sync);
    };
  }, []);
  return enabled;
}

/**
 * 判定某个预览目标是否应**默认勾选**归档同步。
 *
 * 安全边界与 wb-switch 一致：只对「纯归档」目标成立 ——
 * 正文已一致（无写入模式）且后端明确给出 archive_action。
 * 「内容有分歧、需要覆盖」的目标永远不自动勾选，哪怕偏好已打开。
 */
export function shouldDefaultSelectArchive(options: {
  mode: string;
  archiveAction: string | undefined;
  defaultEnabled: boolean;
}): boolean {
  if (!options.defaultEnabled) return false;
  if (options.mode) return false;
  return Boolean(options.archiveAction);
}
