import { toast } from "sonner";
import type { ReactNode } from "react";

/**
 * 应用内通知存档（对齐 wb-switch 的 `lib/notify.ts` 思路）。
 *
 * 为什么需要它：toast 只存活几秒，事后无法回看；把每条提示同步写一份到
 * localStorage（最近 100 条），供用户与排障者核对「当时到底提示了什么」。
 *
 * 与参考实现的差异：wb-switch 把存档写进后端文件（跨重启保留，并由
 * 「通知历史」页做查询 / 清空接口）。本项目是 Web 面板：
 *   1. 不新增后端写盘通道（本批明确不为通知造写接口）；
 *   2. 面板提示本就是**当前浏览器会话**的 UI 信息，localStorage 足够，
 *      且清空 / 读取都是纯前端，不依赖网关状态。
 *
 * 用法：`web/src` 内不要直接 `toast.success(...)`，改用这里的 notify* 包装 ——
 * 显示行为完全一致（内部就是 sonner），只是多一份存档。
 */
export type NotifyLevel = "success" | "error" | "warning" | "info" | "message";

export interface NotificationRecord {
  level: NotifyLevel;
  /** Unix 毫秒。 */
  time: number;
  title: string;
  description?: string;
}

export const NOTIFY_STORAGE_KEY = "workbuddy-gateway.notifications";

/** 存档上限（与 wb-switch 的最近 100 条一致）。 */
export const NOTIFY_HISTORY_LIMIT = 100;

/** 同页面内的变更通知（`storage` 事件只在其它标签页触发）。 */
const NOTIFY_CHANGED_EVENT = "workbuddy-gateway:notifications-changed";

function isLevel(value: unknown): value is NotifyLevel {
  return (
    value === "success" ||
    value === "error" ||
    value === "warning" ||
    value === "info" ||
    value === "message"
  );
}

/** 读取存档（最新在前）。localStorage 不可用 / 内容损坏时返回空数组。 */
export function readNotificationHistory(): NotificationRecord[] {
  try {
    const raw = localStorage.getItem(NOTIFY_STORAGE_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    const out: NotificationRecord[] = [];
    for (const item of parsed) {
      if (!item || typeof item !== "object") continue;
      const rec = item as Partial<NotificationRecord>;
      if (!isLevel(rec.level)) continue;
      if (typeof rec.time !== "number" || typeof rec.title !== "string") continue;
      out.push({
        level: rec.level,
        time: rec.time,
        title: rec.title,
        ...(typeof rec.description === "string" && rec.description
          ? { description: rec.description }
          : {}),
      });
    }
    return out.slice(0, NOTIFY_HISTORY_LIMIT);
  } catch {
    return [];
  }
}

/** 清空存档。 */
export function clearNotificationHistory(): void {
  try {
    localStorage.removeItem(NOTIFY_STORAGE_KEY);
  } catch {
    // 存储不可用时没有可清的存档。
  }
  window.dispatchEvent(new Event(NOTIFY_CHANGED_EVENT));
}

/** 订阅存档变更（配置页的「通知历史」列表要实时刷新）。 */
export function subscribeNotificationHistory(listener: () => void): () => void {
  window.addEventListener("storage", listener);
  window.addEventListener(NOTIFY_CHANGED_EVENT, listener);
  return () => {
    window.removeEventListener("storage", listener);
    window.removeEventListener(NOTIFY_CHANGED_EVENT, listener);
  };
}

/** 只存字符串形式的标题 / 描述；ReactNode 提示不落盘，避免无意义序列化。 */
function textOf(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value : undefined;
}

function record(level: NotifyLevel, title: string | undefined, description: string | undefined) {
  if (!title) return;
  try {
    const next = [{ level, time: Date.now(), title, ...(description ? { description } : {}) }];
    for (const item of readNotificationHistory()) {
      next.push(item);
      if (next.length >= NOTIFY_HISTORY_LIMIT) break;
    }
    localStorage.setItem(NOTIFY_STORAGE_KEY, JSON.stringify(next));
    window.dispatchEvent(new Event(NOTIFY_CHANGED_EVENT));
  } catch {
    // 存档失败静默：提示本身照常显示，不能因为存不下就不提示。
  }
}

interface NotifyOptions {
  description?: ReactNode;
}

/** 与 sonner 的 toast.success 同签名；额外写一份存档。 */
export function notifySuccess(message: ReactNode, options?: NotifyOptions) {
  toast.success(message, options);
  record("success", textOf(message), textOf(options?.description));
}

export function notifyError(message: ReactNode, options?: NotifyOptions) {
  toast.error(message, options);
  record("error", textOf(message), textOf(options?.description));
}

export function notifyWarning(message: ReactNode, options?: NotifyOptions) {
  toast.warning(message, options);
  record("warning", textOf(message), textOf(options?.description));
}

export function notifyInfo(message: ReactNode, options?: NotifyOptions) {
  toast.info(message, options);
  record("info", textOf(message), textOf(options?.description));
}

/** 中性提示（sonner 的 toast.message）。 */
export function notifyMessage(message: ReactNode, options?: NotifyOptions) {
  toast.message(message, options);
  record("message", textOf(message), textOf(options?.description));
}
