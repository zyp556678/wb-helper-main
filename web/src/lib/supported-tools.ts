import { useEffect, useState } from "react";

import type { LocalAppTargetID } from "@/lib/types";

/**
 * 「支持工具」开关：控制各客户端入口是否在界面上出现（对齐 wb-switch 的
 * `src/lib/supported-tools.ts`，按本项目实际能力裁剪）。
 *
 * 语义：
 * - 关闭某工具 = 该工具的入口（「本机应用接入」行、账号卡上的工具按钮）不渲染，
 *   相关本机探测也尽量跳过；**不影响账号库与其它工具**，重新打开即恢复。
 * - 默认值：现有的四个端默认开启；JetBrains 端默认关闭（与参考实现一致的灰度策略）。
 * - VS Code / CodeBuddy IDE 在本项目**没有写能力**（见 internal/localapps/targets.go
 *   的 Writable=false）：开关条目仍展示，但置灰并注明「本机暂不支持写入」——
 *   不给用户一个打开后什么都不会发生的开关，也不假造能力。
 *
 * 持久化在 localStorage（与本项目其它前端偏好同风格），键名
 * `workbuddy-gateway.tools.<id>`，值 `"1"` / `"0"`；缺省时取 defaultEnabled。
 */
export type ToolId = "workbuddy" | "codebuddyIde" | "codebuddyCli" | "vscodeExt" | "jetbrains";

export interface ToolDef {
  id: ToolId;
  /** 设置页开关标题。 */
  label: string;
  /** 设置页说明文案（按本项目真实能力裁剪，不照抄做不到的描述）。 */
  description: string;
  /** 未显式配置时的默认状态。 */
  defaultEnabled: boolean;
  /** 对应的本机应用目标（「本机应用接入」与账号卡入口按它过滤）。 */
  target: LocalAppTargetID;
  /** 本项目是否实现了该端的写入；false 时设置页开关置灰。 */
  writable: boolean;
  /** 无法写入时的说明（置灰开关下方展示）。 */
  unwritableNote?: string;
}

export const SUPPORTED_TOOLS: ToolDef[] = [
  {
    id: "workbuddy",
    label: "WorkBuddy",
    // 国内站与国际站是两个隔离的客户端（WorkBuddy / WorkBuddy AI），同一个开关管两者。
    description: "WorkBuddy / WorkBuddy AI 桌面客户端账号切换与会话复制",
    defaultEnabled: true,
    target: "workbuddy-desktop",
    writable: true,
  },
  {
    id: "codebuddyIde",
    label: "CodeBuddy IDE",
    description: "CodeBuddy IDE 桌面客户端（国内版 / 国际版）账号探测",
    defaultEnabled: true,
    target: "codebuddy-ide",
    writable: false,
    unwritableNote: "本机暂不支持写入：拿不到该客户端的凭据加密密钥，只做探测。",
  },
  {
    id: "codebuddyCli",
    label: "CodeBuddy CLI",
    description: "CodeBuddy CLI 默认账号切换",
    defaultEnabled: true,
    target: "codebuddy-cli",
    writable: true,
  },
  {
    id: "vscodeExt",
    label: "VS Code CodeBuddy 插件",
    description: "VS Code 内 CodeBuddy 插件账号探测",
    defaultEnabled: true,
    target: "vscode",
    writable: false,
    unwritableNote: "本机暂不支持写入：插件登录态用 Safe Storage v10 加密，无法离线解开，只做探测。",
  },
  {
    id: "jetbrains",
    label: "JetBrains IDE 插件（IDEA / PyCharm）",
    description: "IntelliJ IDEA / PyCharm 内 CodeBuddy 插件账号切换",
    defaultEnabled: false,
    target: "jetbrains",
    writable: true,
  },
];

/** 本机目标 → 工具：账号卡与「本机应用接入」用它反查开关。 */
export const TOOL_FOR_TARGET: Record<LocalAppTargetID, ToolId> = {
  "workbuddy-desktop": "workbuddy",
  "codebuddy-ide": "codebuddyIde",
  "codebuddy-cli": "codebuddyCli",
  vscode: "vscodeExt",
  jetbrains: "jetbrains",
};

function storageKey(id: ToolId): string {
  return `workbuddy-gateway.tools.${id}`;
}

/** 同页面内的变更通知（`storage` 事件只在其它标签页触发）。 */
const TOOLS_CHANGED_EVENT = "workbuddy-gateway:tools-changed";

export function isToolEnabled(id: ToolId): boolean {
  const def = SUPPORTED_TOOLS.find((t) => t.id === id);
  const fallback = def?.defaultEnabled ?? true;
  try {
    const stored = localStorage.getItem(storageKey(id));
    if (stored === null) return fallback;
    return stored === "1";
  } catch {
    return fallback;
  }
}

export function setToolEnabled(id: ToolId, enabled: boolean): void {
  try {
    localStorage.setItem(storageKey(id), enabled ? "1" : "0");
  } catch {
    /* 隐私模式等场景下写入失败：开关本次会话内仍可见（由调用方 state 驱动） */
  }
  window.dispatchEvent(new Event(TOOLS_CHANGED_EVENT));
}

function readAll(): Record<ToolId, boolean> {
  return SUPPORTED_TOOLS.reduce(
    (acc, tool) => {
      acc[tool.id] = isToolEnabled(tool.id);
      return acc;
    },
    {} as Record<ToolId, boolean>,
  );
}

/** 订阅「支持工具」开关；设置页改动后（同一标签页）其余页面立即同步。 */
export function useSupportedTools(): Record<ToolId, boolean> {
  const [state, setState] = useState<Record<ToolId, boolean>>(readAll);
  useEffect(() => {
    const sync = () => setState(readAll());
    window.addEventListener("storage", sync);
    window.addEventListener(TOOLS_CHANGED_EVENT, sync);
    return () => {
      window.removeEventListener("storage", sync);
      window.removeEventListener(TOOLS_CHANGED_EVENT, sync);
    };
  }, []);
  return state;
}

/** 某工具是否启用（传已订阅的开关表，避免每行各订阅一次）。 */
export function targetEnabled(enabled: Record<ToolId, boolean>, target: LocalAppTargetID): boolean {
  return enabled[TOOL_FOR_TARGET[target]];
}

/** 五个工具是否全部关闭：全关时本机探测整轮跳过（能省一次探测就省）。 */
export function allToolsDisabled(enabled: Record<ToolId, boolean>): boolean {
  return SUPPORTED_TOOLS.every((tool) => !enabled[tool.id]);
}
