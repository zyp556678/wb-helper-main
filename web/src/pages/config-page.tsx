import { useCallback, useEffect, useMemo, useState } from "react";
import {
  CalendarCheck,
  Download,
  Loader2,
  RefreshCw,
  RotateCcw,
  Save,
  Trash2,
} from "lucide-react";
import {
  notifySuccess,
  notifyError,
  notifyWarning,
  notifyInfo,
  notifyMessage,
} from "@/lib/notify";

import { ConfigRow, DraftNumberField, NumberField } from "@/components/config-field";
import { CopyIconButton, useCopy } from "@/components/copy-button";
import { HourPicker } from "@/components/hour-picker";
import { PromptConfigCard, type PromptFormValue } from "@/components/prompt-config-card";
import { RuntimeInfoCard } from "@/components/runtime-info-card";
import { AutostartCard } from "@/components/autostart-card";
import { UpdateCard } from "@/components/update-card";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { PillGroup, Section } from "@/components/section";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  describeError,
  downloadRequestLog,
  fetchAccounts,
  fetchCheckinLogs,
  fetchConfig,
  fetchLogPaths,
  runSchedulerTask,
  saveConfig,
} from "@/lib/api";
import {
  clearNotificationHistory,
  NOTIFY_HISTORY_LIMIT,
  readNotificationHistory,
  subscribeNotificationHistory,
  type NotificationRecord,
} from "@/lib/notify";
import { setSessionsNavEnabled, useSessionsNavEnabled } from "@/lib/nav-prefs";
import { setToolEnabled, SUPPORTED_TOOLS, useSupportedTools } from "@/lib/supported-tools";
import { useThemePreference, type ThemePreference } from "@/lib/theme";
import type {
  Account,
  CheckinLogsResponse,
  ConfigPatch,
  LogPathsResponse,
  PanelConfig,
} from "@/lib/types";
import { cn } from "@/lib/utils";

type NumKind = "int" | "float";

interface NumFieldSpec<K extends string> {
  key: K;
  label: string;
  hint: string;
  kind: NumKind;
  unit?: string;
}

type CooldownNumKey =
  | "soft_rate_seconds"
  | "soft_rate_max_seconds"
  | "degrade_threshold"
  | "degrade_cooldown_seconds"
  | "degrade_cooldown_max_seconds";

const COOLDOWN_FIELDS: NumFieldSpec<CooldownNumKey>[] = [
  {
    key: "soft_rate_seconds",
    label: "软限流冷却基数",
    hint: "账号触发上游软限流后的基础冷却时长",
    kind: "int",
    unit: "秒",
  },
  {
    key: "soft_rate_max_seconds",
    label: "软限流冷却上限",
    hint: "指数退避后的冷却封顶",
    kind: "int",
    unit: "秒",
  },
  {
    key: "degrade_threshold",
    label: "连败降权阈值",
    hint: "连续失败达到该次数后临时出池",
    kind: "int",
    unit: "次",
  },
  {
    key: "degrade_cooldown_seconds",
    label: "降权冷却时长",
    hint: "连败降权的固定冷却时长",
    kind: "int",
    unit: "秒",
  },
  {
    key: "degrade_cooldown_max_seconds",
    label: "降权冷却上限",
    hint: "降权冷却时长的钳制上限",
    kind: "int",
    unit: "秒",
  },
];

type PoolNumKey =
  | "max_in_flight"
  | "max_in_flight_global"
  | "breaker_threshold"
  | "breaker_cooldown_seconds"
  | "breaker_cooldown_max_seconds"
  | "idle_weight_per_hour"
  | "idle_weight_max"
  | "credit_floor";

const POOL_FIELDS: NumFieldSpec<PoolNumKey>[] = [
  {
    key: "max_in_flight",
    label: "单账号在途上限",
    hint: "单个账号同时处理的上游请求数，0 = 不限",
    kind: "int",
    unit: "个",
  },
  {
    key: "max_in_flight_global",
    label: "国际站在途上限",
    hint: "国际站风控更紧，单独压低并发",
    kind: "int",
    unit: "个",
  },
  {
    key: "breaker_threshold",
    label: "熔断阈值",
    hint: "连续失败达到该次数后触发熔断",
    kind: "int",
    unit: "次",
  },
  {
    key: "breaker_cooldown_seconds",
    label: "熔断冷却时长",
    hint: "熔断后的基础冷却时长",
    kind: "int",
    unit: "秒",
  },
  {
    key: "breaker_cooldown_max_seconds",
    label: "熔断冷却上限",
    hint: "指数退避后的冷却封顶",
    kind: "int",
    unit: "秒",
  },
  {
    key: "idle_weight_per_hour",
    label: "闲置补偿权重",
    hint: "每闲置一小时增加的调度权重",
    kind: "float",
  },
  {
    key: "idle_weight_max",
    label: "闲置补偿封顶",
    hint: "闲置补偿权重的上限",
    kind: "float",
  },
  {
    key: "credit_floor",
    label: "积分保底",
    hint: "余额低于该值时不再承接实测收费的模型（保护余额不被收费请求打穿）；0 = 关闭",
    kind: "int",
    unit: "分",
  },
];

type StickyNumKey = "ttl_seconds" | "gc_interval_seconds";

const STICKY_FIELDS: NumFieldSpec<StickyNumKey>[] = [
  {
    key: "ttl_seconds",
    label: "绑定 TTL",
    hint: "同一会话绑定到同一账号的有效时长",
    kind: "int",
    unit: "秒",
  },
  { key: "gc_interval_seconds", label: "回收周期", hint: "过期绑定的清理间隔", kind: "int", unit: "秒" },
];

interface ConfigForm {
  cooldown: Record<CooldownNumKey, string>;
  pool: Record<PoolNumKey, string> & { prefer_free_site: boolean };
  session_sticky: { enabled: boolean; ttl_seconds: string; gc_interval_seconds: string };
  schedule: {
    checkin_enabled: boolean;
    checkin_hours: number[];
    checkin_start: string;
    checkin_end: string;
    /** 不参与自动签到的账号 ID（本地草稿；随「保存配置」提交）。 */
    checkin_excluded_accounts: string[];
    keepalive_enabled: boolean;
    keepalive_hours: number[];
    /** 保活阈值（天）与惰性窗口（小时）的草稿文本；失焦提交到草稿。 */
    keepalive_days: string;
    lazy_refresh_hours: string;
    balance_refresh_enabled: boolean;
    balance_refresh_minutes: string;
    tasks_enabled: boolean;
    tasks_hours: number[];
    blackcat_enabled: boolean;
    blackcat_hours: number[];
    travel_enabled: boolean;
    travel_hours: number[];
    activity_enabled: boolean;
    activity_hours: number[];
    /** 保号类四任务是否覆盖已禁用账号（禁用只关选号、不停保号）。 */
    include_disabled_in_tasks: boolean;
  };
  /** 入站 HTTP 参数；保存后需重启进程才生效。 */
  server: { read_timeout: string };
  tasks: { desktop_events_enabled: boolean };
  prompt: PromptFormValue;
}

/** 提示词段的兜底值：后端尚未下发 prompt 时避免整页崩掉。 */
const DEFAULT_PROMPT: PromptFormValue = {
  mode: "append",
  text: "",
  sanitize: true,
  degraded_retry: true,
  strict_first_system: true,
};

function sortedHours(hours: number[]): number[] {
  return [...hours].sort((a, b) => a - b);
}

function toForm(config: PanelConfig): ConfigForm {
  return {
    cooldown: {
      soft_rate_seconds: String(config.cooldown.soft_rate_seconds),
      soft_rate_max_seconds: String(config.cooldown.soft_rate_max_seconds),
      degrade_threshold: String(config.cooldown.degrade_threshold),
      degrade_cooldown_seconds: String(config.cooldown.degrade_cooldown_seconds),
      degrade_cooldown_max_seconds: String(config.cooldown.degrade_cooldown_max_seconds),
    },
    pool: {
      max_in_flight: String(config.pool.max_in_flight),
      max_in_flight_global: String(config.pool.max_in_flight_global),
      breaker_threshold: String(config.pool.breaker_threshold),
      breaker_cooldown_seconds: String(config.pool.breaker_cooldown_seconds),
      breaker_cooldown_max_seconds: String(config.pool.breaker_cooldown_max_seconds),
      idle_weight_per_hour: String(config.pool.idle_weight_per_hour),
      idle_weight_max: String(config.pool.idle_weight_max),
      credit_floor: String(config.pool.credit_floor ?? 0),
      // 后端未下发时按默认开处理（与后端 ApplyDefaults 口径一致）
      prefer_free_site: config.pool.prefer_free_site ?? true,
    },
    session_sticky: {
      enabled: config.session_sticky.enabled,
      ttl_seconds: String(config.session_sticky.ttl_seconds),
      gc_interval_seconds: String(config.session_sticky.gc_interval_seconds),
    },
    schedule: {
      checkin_enabled: config.schedule.checkin_enabled,
      checkin_hours: sortedHours(config.schedule.checkin_hours),
      checkin_start: config.schedule.checkin_start ?? "",
      checkin_end: config.schedule.checkin_end ?? "",
      checkin_excluded_accounts: [...(config.schedule.checkin_excluded_accounts ?? [])],
      keepalive_enabled: config.schedule.keepalive_enabled,
      keepalive_hours: sortedHours(config.schedule.keepalive_hours),
      // 后端未下发时按默认值口径兜底（keepalive_days=0 无条件 / lazy=24）。
      keepalive_days: String(config.schedule.keepalive_days ?? 0),
      lazy_refresh_hours: String(config.schedule.lazy_refresh_hours ?? 24),
      balance_refresh_enabled: config.schedule.balance_refresh_enabled,
      balance_refresh_minutes: String(config.schedule.balance_refresh_minutes),
      tasks_enabled: config.schedule.tasks_enabled ?? true,
      tasks_hours: sortedHours(config.schedule.tasks_hours ?? [9]),
      blackcat_enabled: config.schedule.blackcat_enabled ?? true,
      blackcat_hours: sortedHours(config.schedule.blackcat_hours ?? [23]),
      travel_enabled: config.schedule.travel_enabled ?? true,
      travel_hours: sortedHours(config.schedule.travel_hours ?? [9, 21]),
      activity_enabled: config.schedule.activity_enabled ?? true,
      activity_hours: sortedHours(config.schedule.activity_hours ?? [10]),
      // 默认关：后端未下发时按「禁用即跳过」处理（与 config 的 nil=false 口径一致）。
      include_disabled_in_tasks: config.schedule.include_disabled_in_tasks ?? false,
    },
    server: {
      // 后端未下发时按缺省 300s 兜底（与 config.DefaultServerReadTimeout 同口径）。
      read_timeout: config.server?.read_timeout ?? "300s",
    },
    tasks: {
      // 默认关：后端未下发时按「关闭」处理（与 config 的 nil=false 口径一致）。
      desktop_events_enabled: config.tasks?.desktop_events_enabled ?? false,
    },
    prompt: config.prompt
      ? {
          mode: config.prompt.mode,
          text: config.prompt.text,
          sanitize: config.prompt.sanitize,
          degraded_retry: config.prompt.degraded_retry,
          strict_first_system: config.prompt.strict_first_system,
        }
      : DEFAULT_PROMPT,
  };
}

/** 非负数字校验：整数型不允许小数点；返回错误文案或 null。 */
function validateNumber(raw: string, kind: NumKind): string | null {
  const trimmed = raw.trim();
  if (!trimmed) return "不能为空";
  if (!/^\d+(\.\d+)?$/.test(trimmed)) return "请输入非负数字";
  if (kind === "int" && !/^\d+$/.test(trimmed)) return "必须是整数";
  return null;
}

function validateAll(form: ConfigForm): Record<string, string> {
  const errors: Record<string, string> = {};
  for (const field of COOLDOWN_FIELDS) {
    const message = validateNumber(form.cooldown[field.key], field.kind);
    if (message) errors[`cooldown.${field.key}`] = message;
  }
  for (const field of POOL_FIELDS) {
    const message = validateNumber(form.pool[field.key], field.kind);
    if (message) errors[`pool.${field.key}`] = message;
  }
  for (const field of STICKY_FIELDS) {
    const message = validateNumber(form.session_sticky[field.key], field.kind);
    if (message) errors[`session_sticky.${field.key}`] = message;
  }
  const refreshMessage = validateNumber(form.schedule.balance_refresh_minutes, "int");
  if (refreshMessage) errors["schedule.balance_refresh_minutes"] = refreshMessage;

  // 入站读取上限：与后端 time.ParseDuration 同口径的轻量校验（后端仍会再校验一次）。
  // 负值在后端是 fail fast（静默钳 0 等于把保护悄悄关掉），前端也不放行。
  const readTimeout = form.server.read_timeout.trim();
  if (readTimeout === "") {
    errors["server.read_timeout"] = "不能为空；留空表示按默认 300s，请直接填 300s";
  } else if (readTimeout.startsWith("-")) {
    errors["server.read_timeout"] = "不支持负值（0 表示不限制）";
  } else if (readTimeout !== "0" && !/^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/.test(readTimeout)) {
    errors["server.read_timeout"] = "格式形如 300s / 5m / 1h30m；0 表示不限制";
  }
  // 保活参数：阈值 0-90（0 = 无条件刷新），惰性窗口 1-72。
  {
    const daysMessage = validateNumber(form.schedule.keepalive_days, "int");
    if (daysMessage) {
      errors["schedule.keepalive_days"] = daysMessage;
    } else if (Number(form.schedule.keepalive_days) > 90) {
      errors["schedule.keepalive_days"] = "应为 0-90 之间的整数";
    }
    const lazyMessage = validateNumber(form.schedule.lazy_refresh_hours, "int");
    if (lazyMessage) {
      errors["schedule.lazy_refresh_hours"] = lazyMessage;
    } else {
      const hours = Number(form.schedule.lazy_refresh_hours);
      if (hours < 1 || hours > 72) errors["schedule.lazy_refresh_hours"] = "应为 1-72 之间的整数";
    }
  }
  if (form.schedule.checkin_enabled && form.schedule.checkin_hours.length === 0) {
    errors["schedule.checkin_hours"] = "至少选择一个小时；如需禁用请关闭上方开关";
  }
  // 签到时间段：要么两个都留空（不限制），要么都填且合法。
  // 只填一个会被后端拒绝启动，这里先说清楚，省得用户以为保存成功了。
  {
    const cs = form.schedule.checkin_start.trim();
    const ce = form.schedule.checkin_end.trim();
    if (cs === "" && ce !== "") {
      errors["schedule.checkin_start"] = "要与结束时间一起填写（或两个都留空表示不限制）";
    } else if (cs !== "" && ce === "") {
      errors["schedule.checkin_end"] = "要与开始时间一起填写（或两个都留空表示不限制）";
    } else if (cs !== "" && ce !== "") {
      const okClock = /^\d{1,2}:\d{2}$/;
      if (!okClock.test(cs)) errors["schedule.checkin_start"] = "格式应为 HH:MM";
      if (!okClock.test(ce)) errors["schedule.checkin_end"] = "格式应为 HH:MM";
      if (!errors["schedule.checkin_start"] && !errors["schedule.checkin_end"]) {
        const toMin = (v: string) => {
          const [h, m] = v.split(":").map(Number);
          return h * 60 + m;
        };
        if (toMin(cs) >= toMin(ce)) {
          errors["schedule.checkin_end"] = "结束时间必须晚于开始时间（不支持跨午夜）";
        }
      }
    }
  }
  if (form.schedule.keepalive_enabled && form.schedule.keepalive_hours.length === 0) {
    errors["schedule.keepalive_hours"] = "至少选择一个小时；如需禁用请关闭上方开关";
  }
  if (form.schedule.tasks_enabled && form.schedule.tasks_hours.length === 0) {
    errors["schedule.tasks_hours"] = "至少选择一个小时；如需禁用请关闭上方开关";
  }
  if (form.schedule.travel_enabled && form.schedule.travel_hours.length === 0) {
    errors["schedule.travel_hours"] = "至少选择一个小时；如需禁用请关闭上方开关";
  }
  if (form.schedule.activity_enabled && form.schedule.activity_hours.length === 0) {
    errors["schedule.activity_hours"] = "至少选择一个小时；如需禁用请关闭上方开关";
  }
  if (form.schedule.blackcat_enabled && form.schedule.blackcat_hours.length === 0) {
    errors["schedule.blackcat_hours"] = "至少选择一个小时；如需禁用请关闭上方开关";
  }
  return errors;
}

function sameHours(a: number[], b: number[]): boolean {
  const x = sortedHours(a);
  const y = sortedHours(b);
  return x.length === y.length && x.every((hour, index) => hour === y[index]);
}

/** 与已保存配置逐字段对比，只产出发生变化的字段路径。 */
function buildPatch(form: ConfigForm, base: PanelConfig): ConfigPatch {
  const patch: ConfigPatch = {};

  const cooldown: NonNullable<ConfigPatch["cooldown"]> = {};
  for (const field of COOLDOWN_FIELDS) {
    const next = Number(form.cooldown[field.key]);
    if (next !== base.cooldown[field.key]) cooldown[field.key] = next;
  }
  if (Object.keys(cooldown).length > 0) patch.cooldown = cooldown;

  const pool: NonNullable<ConfigPatch["pool"]> = {};
  for (const field of POOL_FIELDS) {
    const next = Number(form.pool[field.key]);
    if (next !== base.pool[field.key]) pool[field.key] = next;
  }
  if (form.pool.prefer_free_site !== base.pool.prefer_free_site) {
    pool.prefer_free_site = form.pool.prefer_free_site;
  }
  if (Object.keys(pool).length > 0) patch.pool = pool;

  const sticky: NonNullable<ConfigPatch["session_sticky"]> = {};
  if (form.session_sticky.enabled !== base.session_sticky.enabled) {
    sticky.enabled = form.session_sticky.enabled;
  }
  for (const field of STICKY_FIELDS) {
    const next = Number(form.session_sticky[field.key]);
    if (next !== base.session_sticky[field.key]) sticky[field.key] = next;
  }
  if (Object.keys(sticky).length > 0) patch.session_sticky = sticky;

  const schedule: NonNullable<ConfigPatch["schedule"]> = {};
  if (form.schedule.checkin_enabled !== base.schedule.checkin_enabled) {
    schedule.checkin_enabled = form.schedule.checkin_enabled;
  }
  if (!sameHours(form.schedule.checkin_hours, base.schedule.checkin_hours)) {
    schedule.checkin_hours = sortedHours(form.schedule.checkin_hours);
  }
  if (form.schedule.keepalive_enabled !== base.schedule.keepalive_enabled) {
    schedule.keepalive_enabled = form.schedule.keepalive_enabled;
  }
  if (!sameHours(form.schedule.keepalive_hours, base.schedule.keepalive_hours)) {
    schedule.keepalive_hours = sortedHours(form.schedule.keepalive_hours);
  }
  // 保活阈值 / 惰性窗口（批次 5）：草稿文本 → 数字，与已保存值不同才提交。
  const keepaliveDays = Number(form.schedule.keepalive_days);
  if (keepaliveDays !== base.schedule.keepalive_days) {
    schedule.keepalive_days = keepaliveDays;
  }
  const lazyRefreshHours = Number(form.schedule.lazy_refresh_hours);
  if (lazyRefreshHours !== base.schedule.lazy_refresh_hours) {
    schedule.lazy_refresh_hours = lazyRefreshHours;
  }
  // 排除名单按集合比较（顺序无关）：提交排序去重后的数组，避免只是顺序变化也报「有改动」。
  const excludedNext = [...new Set(form.schedule.checkin_excluded_accounts)].sort();
  const excludedBase = [...new Set(base.schedule.checkin_excluded_accounts ?? [])].sort();
  if (excludedNext.join("\n") !== excludedBase.join("\n")) {
    schedule.checkin_excluded_accounts = excludedNext;
  }
  if (form.schedule.balance_refresh_enabled !== base.schedule.balance_refresh_enabled) {
    schedule.balance_refresh_enabled = form.schedule.balance_refresh_enabled;
  }
  const refreshMinutes = Number(form.schedule.balance_refresh_minutes);
  if (refreshMinutes !== base.schedule.balance_refresh_minutes) {
    schedule.balance_refresh_minutes = refreshMinutes;
  }
  if (form.schedule.tasks_enabled !== base.schedule.tasks_enabled) {
    schedule.tasks_enabled = form.schedule.tasks_enabled;
  }
  if (!sameHours(form.schedule.tasks_hours, base.schedule.tasks_hours)) {
    schedule.tasks_hours = sortedHours(form.schedule.tasks_hours);
  }
  if (form.schedule.blackcat_enabled !== base.schedule.blackcat_enabled) {
    schedule.blackcat_enabled = form.schedule.blackcat_enabled;
  }
  if (!sameHours(form.schedule.blackcat_hours, base.schedule.blackcat_hours)) {
    schedule.blackcat_hours = sortedHours(form.schedule.blackcat_hours);
  }
  if (form.schedule.checkin_start.trim() !== base.schedule.checkin_start) {
    schedule.checkin_start = form.schedule.checkin_start.trim();
  }
  if (form.schedule.checkin_end.trim() !== base.schedule.checkin_end) {
    schedule.checkin_end = form.schedule.checkin_end.trim();
  }
  if (form.schedule.travel_enabled !== base.schedule.travel_enabled) {
    schedule.travel_enabled = form.schedule.travel_enabled;
  }
  if (!sameHours(form.schedule.travel_hours, base.schedule.travel_hours)) {
    schedule.travel_hours = sortedHours(form.schedule.travel_hours);
  }
  if (form.schedule.activity_enabled !== base.schedule.activity_enabled) {
    schedule.activity_enabled = form.schedule.activity_enabled;
  }
  if (!sameHours(form.schedule.activity_hours, base.schedule.activity_hours)) {
    schedule.activity_hours = sortedHours(form.schedule.activity_hours);
  }
  if (
    form.schedule.include_disabled_in_tasks !==
    (base.schedule.include_disabled_in_tasks ?? false)
  ) {
    schedule.include_disabled_in_tasks = form.schedule.include_disabled_in_tasks;
  }
  if (Object.keys(schedule).length > 0) patch.schedule = schedule;

  const baseReadTimeout = base.server?.read_timeout ?? "300s";
  if (form.server.read_timeout.trim() !== baseReadTimeout) {
    patch.server = { read_timeout: form.server.read_timeout.trim() };
  }

  if (form.tasks.desktop_events_enabled !== (base.tasks?.desktop_events_enabled ?? false)) {
    patch.tasks = { desktop_events_enabled: form.tasks.desktop_events_enabled };
  }

  const basePrompt = base.prompt ?? DEFAULT_PROMPT;
  const prompt: NonNullable<ConfigPatch["prompt"]> = {};
  if (form.prompt.mode !== basePrompt.mode) prompt.mode = form.prompt.mode;
  if (form.prompt.text !== basePrompt.text) prompt.text = form.prompt.text;
  if (form.prompt.sanitize !== basePrompt.sanitize) prompt.sanitize = form.prompt.sanitize;
  if (form.prompt.degraded_retry !== basePrompt.degraded_retry) {
    prompt.degraded_retry = form.prompt.degraded_retry;
  }
  if (form.prompt.strict_first_system !== basePrompt.strict_first_system) {
    prompt.strict_first_system = form.prompt.strict_first_system;
  }
  if (Object.keys(prompt).length > 0) patch.prompt = prompt;

  return patch;
}

/** 签到日志时间：当天只显示时分秒，跨天显示 MM-DD HH:mm:ss。 */
function formatLogTime(ms: number): string {
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  const sameDay = d.toDateString() === new Date().toDateString();
  const clock = `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  return sameDay ? clock : `${p(d.getMonth() + 1)}-${p(d.getDate())} ${clock}`;
}

/** 签到结果 → 文案与着色（成功绿 / 已签琥珀 / 失败红，与参考实现同口径）。 */
function checkinResultView(result: string): { text: string; className: string } {
  switch (result) {
    case "success":
      return { text: "签到成功", className: "text-emerald-600 dark:text-emerald-400" };
    case "already":
      return { text: "已签到", className: "text-amber-600 dark:text-amber-400" };
    default:
      return { text: "失败", className: "text-destructive" };
  }
}

const NOTIFY_LEVEL_LABEL: Record<NotificationRecord["level"], string> = {
  success: "成功",
  error: "错误",
  warning: "警告",
  info: "提示",
  message: "提示",
};

const NOTIFY_LEVEL_DOT: Record<NotificationRecord["level"], string> = {
  success: "bg-primary",
  error: "bg-destructive",
  warning: "bg-amber-500",
  info: "bg-muted-foreground/60",
  message: "bg-muted-foreground/60",
};

/** 通知时间：当天只显示时分秒，更早显示完整时间（与参考实现一致）。 */
function formatNotificationTime(at: number): string {
  const date = new Date(at);
  const sameDay = date.toDateString() === new Date().toDateString();
  return sameDay
    ? date.toLocaleTimeString("zh-CN", { hour12: false })
    : date.toLocaleString("zh-CN", { hour12: false });
}

export function ConfigPage() {
  const [config, setConfig] = useState<PanelConfig | null>(null);
  const [form, setForm] = useState<ConfigForm | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  /** 「按时间段签到一次」的独立忙碌态：不该与保存按钮互相禁用。 */
  const [windowCheckinBusy, setWindowCheckinBusy] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [errors, setErrors] = useState<Record<string, string>>({});

  // ---- 批次 5：本机偏好（localStorage 即时生效，不跟随「保存配置」）----
  /** 主题三态：与侧栏切换器共用同一 hook，两处状态必须同步。 */
  const [themePreference, selectTheme] = useThemePreference();
  /** 「显示关联会话菜单」偏好：关掉后左侧导航不渲染「关联会话」入口。 */
  const sessionsNav = useSessionsNavEnabled();
  /** 「支持工具」开关：控制本机应用条目 / 账号卡入口与对应探测。 */
  const supportedTools = useSupportedTools();
  /** 路径复制反馈（请求日志 / 数据目录）。 */
  const { copiedKey, copy } = useCopy();

  // ---- 批次 5：只读数据（签到日志 / 日志落点 / 排除名单要用的账号列表）----
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [checkinLogs, setCheckinLogs] = useState<CheckinLogsResponse | null>(null);
  const [checkinLogsError, setCheckinLogsError] = useState<string | null>(null);
  const [logPaths, setLogPaths] = useState<LogPathsResponse | null>(null);
  const [downloadingLog, setDownloadingLog] = useState(false);
  /** 通知历史存档（localStorage；订阅变更，清空后列表立即刷新）。 */
  const [notifications, setNotifications] = useState<NotificationRecord[]>(() =>
    readNotificationHistory(),
  );

  useEffect(
    () => subscribeNotificationHistory(() => setNotifications(readNotificationHistory())),
    [],
  );

  const loadCheckinLogs = useCallback(async () => {
    try {
      setCheckinLogs(await fetchCheckinLogs(30));
      setCheckinLogsError(null);
    } catch (err) {
      setCheckinLogs(null);
      setCheckinLogsError(describeError(err));
    }
  }, []);

  useEffect(() => {
    void loadCheckinLogs();
    fetchLogPaths()
      .then(setLogPaths)
      .catch(() => setLogPaths(null));
    fetchAccounts()
      .then((res) => setAccounts(res.accounts ?? []))
      .catch(() => setAccounts([]));
  }, [loadCheckinLogs]);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const next = await fetchConfig();
      setConfig(next);
      setForm(toForm(next));
      setErrors({});
      setLoadError(null);
    } catch (err) {
      setLoadError(describeError(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const clearError = useCallback((path: string) => {
    setErrors((prev) => {
      if (!(path in prev)) return prev;
      const next = { ...prev };
      delete next[path];
      return next;
    });
  }, []);

  const setCooldown = (key: CooldownNumKey, value: string) => {
    setForm((prev) =>
      prev
        ? { ...prev, cooldown: { ...prev.cooldown, [key]: value } as Record<CooldownNumKey, string> }
        : prev,
    );
    clearError(`cooldown.${key}`);
  };

  const setPool = (key: PoolNumKey, value: string) => {
    setForm((prev) =>
      prev
        ? {
            ...prev,
            pool: { ...prev.pool, [key]: value } as Record<PoolNumKey, string> & {
              prefer_free_site: boolean;
            },
          }
        : prev,
    );
    clearError(`pool.${key}`);
  };

  const setPreferFreeSite = (checked: boolean) => {
    setForm((prev) => (prev ? { ...prev, pool: { ...prev.pool, prefer_free_site: checked } } : prev));
  };

  const setSticky = (patch: Partial<ConfigForm["session_sticky"]>) => {
    setForm((prev) => (prev ? { ...prev, session_sticky: { ...prev.session_sticky, ...patch } } : prev));
  };

  const setSchedule = (patch: Partial<ConfigForm["schedule"]>) => {
    setForm((prev) => (prev ? { ...prev, schedule: { ...prev.schedule, ...patch } } : prev));
  };

  const setServer = (patch: Partial<ConfigForm["server"]>) => {
    setForm((prev) => (prev ? { ...prev, server: { ...prev.server, ...patch } } : prev));
  };
  const setTasks = (patch: Partial<ConfigForm["tasks"]>) => {
    setForm((prev) => (prev ? { ...prev, tasks: { ...prev.tasks, ...patch } } : prev));
  };

  const setPrompt = (patch: Partial<PromptFormValue>) => {
    setForm((prev) => (prev ? { ...prev, prompt: { ...prev.prompt, ...patch } } : prev));
  };

  const dirty = useMemo(
    () => (config && form ? Object.keys(buildPatch(form, config)).length > 0 : false),
    [config, form],
  );

  const handleReset = () => {
    if (!config) return;
    setForm(toForm(config));
    setErrors({});
    notifyMessage("已重置为上次保存的配置");
  };

  const handleSave = async () => {
    if (!form || !config) return;

    const nextErrors = validateAll(form);
    setErrors(nextErrors);
    if (Object.keys(nextErrors).length > 0) {
      notifyError("请先修正标红的字段");
      return;
    }

    const patch = buildPatch(form, config);
    if (Object.keys(patch).length === 0) {
      notifyMessage("配置没有变化");
      return;
    }

    setSaving(true);
    try {
      const result = await saveConfig(patch);
      setConfig(result.config);
      setForm(toForm(result.config));
      setErrors({});
      if (result.applied.length > 0) {
        notifySuccess("配置已保存", { description: `已热生效：${result.applied.join("、")}` });
      }
      if (result.need_restart.length > 0) {
        notifyWarning("部分配置需重启进程生效", { description: result.need_restart.join("、") });
      }
      if (result.applied.length === 0 && result.need_restart.length === 0) {
        notifyMessage("配置已保存（无生效项）");
      }
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setSaving(false);
    }
  };

  /**
   * 逐账号「不参与自动签到」开关：只改**本地草稿**，随底部「保存配置」提交。
   *
   * 与 wb-switch 的即时落盘不同（那是它的统一交互），本项目配置页是
   * 草稿 + 底部保存：这里保持一致，避免同一页面两种提交时机。
   */
  const toggleExcludedAccount = (accountId: string, allowed: boolean) => {
    setForm((prev) => {
      if (!prev) return prev;
      const excluded = new Set(prev.schedule.checkin_excluded_accounts);
      if (allowed) excluded.delete(accountId);
      else excluded.add(accountId);
      return {
        ...prev,
        schedule: { ...prev.schedule, checkin_excluded_accounts: [...excluded] },
      };
    });
  };

  /** 下载最近一个请求日志归档（面板可能配了 api_key，走 fetch + blob）。 */
  const handleDownloadRequestLog = async () => {
    setDownloadingLog(true);
    try {
      const { blob, filename } = await downloadRequestLog();
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = filename;
      link.click();
      URL.revokeObjectURL(url);
      notifySuccess("最近请求日志已开始下载", { description: filename });
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setDownloadingLog(false);
    }
  };

  const firstLoad = loading && !form;

  return (
    <div className="mx-auto w-full max-w-3xl px-6 py-8 sm:px-8 sm:py-9">
      <header className="mb-6 flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-[28px] font-semibold tracking-tight">配置</h1>
          <p className="mt-2 text-sm leading-6 text-muted-foreground">
            调度与治理参数。能热生效的改动立即应用，需重启的会在保存后提示。
          </p>
        </div>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => void load()}
          disabled={loading}
          className="mt-1"
          aria-label="重新拉取配置"
        >
          <RefreshCw className={loading ? "size-3.5 animate-spin" : "size-3.5"} aria-hidden="true" />
          刷新
        </Button>
      </header>

      {loadError ? (
        <Card className="mb-5 gap-3 border-destructive/30 bg-destructive/5 p-4">
          <div className="text-sm font-medium text-destructive">读取配置失败</div>
          <p className="text-xs leading-5 text-muted-foreground">{loadError}</p>
          <div>
            <Button type="button" variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
              重试
            </Button>
          </div>
        </Card>
      ) : null}

      {firstLoad ? (
        <div className="space-y-12">
          {[0, 1, 2, 3].map((index) => (
            <Skeleton key={index} className="h-[240px] rounded-xl" />
          ))}
        </div>
      ) : form && config ? (
        <div className="space-y-4">
          <Section id="config-cooldown" title="冷却与熔断"
            description="控制软限流与连败降权的冷却时长，生效后对后续请求立即应用。">
            <CardContent className="divide-y divide-border/60">
              {COOLDOWN_FIELDS.map((field) => {
                const path = `cooldown.${field.key}`;
                return (
                  <ConfigRow
                    key={field.key}
                    label={field.label}
                    hint={field.hint}
                    htmlFor={path}
                    error={errors[path]}
                  >
                    <NumberField
                      id={path}
                      value={form.cooldown[field.key]}
                      unit={field.unit}
                      allowDecimal={field.kind === "float"}
                      invalid={Boolean(errors[path])}
                      disabled={saving}
                      onChange={(value) => setCooldown(field.key, value)}
                    />
                  </ConfigRow>
                );
              })}
            </CardContent>
          </Section>

          <Section id="config-pool" title="账号池"
            description="并发上限、熔断、闲置补偿权重与站点价格倾斜，影响选号与调度。">
            <CardContent className="divide-y divide-border/60">
              {POOL_FIELDS.map((field) => {
                const path = `pool.${field.key}`;
                return (
                  <ConfigRow
                    key={field.key}
                    label={field.label}
                    hint={field.hint}
                    htmlFor={path}
                    error={errors[path]}
                  >
                    <NumberField
                      id={path}
                      value={form.pool[field.key]}
                      unit={field.unit}
                      allowDecimal={field.kind === "float"}
                      invalid={Boolean(errors[path])}
                      disabled={saving}
                      onChange={(value) => setPool(field.key, value)}
                    />
                  </ConfigRow>
                );
              })}
              <ConfigRow
                label="免费站点优先"
                hint="同一模型只有「一侧免费、另一侧收费」时倾斜到免费侧；优先站点不可用时自动回落，两站结论相同或未知时不倾斜"
              >
                <Switch
                  checked={form.pool.prefer_free_site}
                  disabled={saving}
                  aria-label="免费站点优先"
                  onCheckedChange={setPreferFreeSite}
                />
              </ConfigRow>
            </CardContent>
          </Section>

          <Section id="config-session-sticky" title="会话粘性"
            description="同一会话尽量命中同一账号，提升缓存命中率。">
            <CardContent className="divide-y divide-border/60">
              <ConfigRow
                label="启用会话粘性"
                hint="关闭后同一会话不再固定账号"
              >
                <Switch
                  checked={form.session_sticky.enabled}
                  disabled={saving}
                  aria-label="启用会话粘性"
                  onCheckedChange={(checked) =>
                    setSticky({ enabled: checked })
                  }
                />
              </ConfigRow>
              {STICKY_FIELDS.map((field) => {
                const path = `session_sticky.${field.key}`;
                return (
                  <ConfigRow
                    key={field.key}
                    label={field.label}
                    hint={field.hint}
                    htmlFor={path}
                    error={errors[path]}
                  >
                    <NumberField
                      id={path}
                      value={form.session_sticky[field.key]}
                      unit={field.unit}
                      allowDecimal={field.kind === "float"}
                      invalid={Boolean(errors[path])}
                      disabled={saving}
                      onChange={(value) => {
                        setSticky(
                          field.key === "ttl_seconds"
                            ? { ttl_seconds: value }
                            : { gc_interval_seconds: value },
                        );
                        clearError(path);
                      }}
                    />
                  </ConfigRow>
                );
              })}
            </CardContent>
          </Section>

          <Section id="config-schedule" title="定时任务"
            description="按整点触发签到、保活与余额刷新；下方小时为该任务的执行时点。">
            <CardContent className="divide-y divide-border/60">
              <ConfigRow
                label="自动签到"
                hint="到点后自动为账号完成签到"
              >
                <Switch
                  checked={form.schedule.checkin_enabled}
                  disabled={saving}
                  aria-label="自动签到"
                  onCheckedChange={(checked) => {
                    setSchedule({ checkin_enabled: checked });
                    clearError("schedule.checkin_hours");
                  }}
                />
              </ConfigRow>
              <ConfigRow
                label="签到小时"
                hint="选择每天执行签到的整点，可多选"
                htmlFor="schedule-checkin-hours"
                error={errors["schedule.checkin_hours"]}
              >
                <HourPicker
                  id="schedule-checkin-hours"
                  value={form.schedule.checkin_hours}
                  disabled={saving}
                  onChange={(hours) => {
                    setSchedule({ checkin_hours: hours });
                    clearError("schedule.checkin_hours");
                  }}
                />
              </ConfigRow>

              {/* 签到时间段：左闭右开。只影响排程签到与「刷新并签到」，
                  单账号的立即签到不受影响 —— 那句提示必须写在界面上，
                  否则用户会以为窗口能拦住手动点击。 */}
              <ConfigRow
                label="签到时间段"
                hint="格式 HH:MM，左闭右开；留空表示不限制。仅约束排程签到与批量签到，手动单账号签到不受限"
                htmlFor="schedule-checkin-start"
                error={errors["schedule.checkin_start"] ?? errors["schedule.checkin_end"]}
              >
                <div className="flex items-center gap-1.5">
                  <Input
                    id="schedule-checkin-start"
                    className="w-20"
                    placeholder="09:00"
                    value={form.schedule.checkin_start}
                    disabled={saving}
                    onChange={(e) => {
                      setSchedule({ checkin_start: e.target.value });
                      clearError("schedule.checkin_start");
                      clearError("schedule.checkin_end");
                    }}
                  />
                  <span className="text-xs text-muted-foreground">至</span>
                  <Input
                    className="w-20"
                    placeholder="11:00"
                    value={form.schedule.checkin_end}
                    disabled={saving}
                    aria-label="签到结束时间"
                    onChange={(e) => {
                      setSchedule({ checkin_end: e.target.value });
                      clearError("schedule.checkin_start");
                      clearError("schedule.checkin_end");
                    }}
                  />
                </div>
              </ConfigRow>

              {/* 「按时间段签到一次」= 遵守窗口的批量签到。与监控页的「立即签到」
                  刻意做成两个入口：前者是策略语义（窗口外跳过），后者是立即语义。
                  合成一个会让「为什么点了没反应」变成日常疑问。 */}
              <ConfigRow
                label="按时间段签到一次"
                hint="遵守上面的时间段立即跑一轮；不在窗口内会整轮跳过，不发任何上游请求"
              >
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={saving || windowCheckinBusy}
                  onClick={async () => {
                    setWindowCheckinBusy(true);
                    try {
                      const res = await runSchedulerTask("checkin", { respectWindow: true });
                      if (res.skipped) {
                        notifyInfo(res.detail);
                      } else {
                        notifySuccess(res.detail);
                      }
                    } catch (err) {
                      notifyError(describeError(err));
                    } finally {
                      setWindowCheckinBusy(false);
                    }
                  }}
                >
                  {windowCheckinBusy ? (
                    <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                  ) : (
                    <CalendarCheck className="size-3.5" aria-hidden="true" />
                  )}
                  立即跑一轮
                </Button>
              </ConfigRow>

              <ConfigRow
                label="Token 保活"
                hint="定期刷新账号 Token 避免过期"
              >
                <Switch
                  checked={form.schedule.keepalive_enabled}
                  disabled={saving}
                  aria-label="Token 保活"
                  onCheckedChange={(checked) => {
                    setSchedule({ keepalive_enabled: checked });
                    clearError("schedule.keepalive_hours");
                  }}
                />
              </ConfigRow>
              <ConfigRow
                label="保活小时"
                hint="选择每天执行保活的整点，可多选"
                htmlFor="schedule-keepalive-hours"
                error={errors["schedule.keepalive_hours"]}
              >
                <HourPicker
                  id="schedule-keepalive-hours"
                  value={form.schedule.keepalive_hours}
                  disabled={saving}
                  onChange={(hours) => {
                    setSchedule({ keepalive_hours: hours });
                    clearError("schedule.keepalive_hours");
                  }}
                />
              </ConfigRow>

              {/* 保活阈值 / 惰性窗口（批次 5，对齐 wb-switch 的同名参数）。
                  只作用于**排程**保活；面板上的手动「令牌保活」保持立即语义
                  （显式意图不该被阈值/窗口否决），这句话要写在界面上。 */}
              <ConfigRow
                label="保活阈值"
                hint="天；0 = 每天无条件刷新全部账号，大于 0 时只刷新剩余有效期不足该天数的账号。仅约束排程保活，手动刷新不受限"
                htmlFor="schedule-keepalive-days"
                error={errors["schedule.keepalive_days"]}
              >
                <DraftNumberField
                  id="schedule-keepalive-days"
                  value={form.schedule.keepalive_days}
                  unit="天"
                  invalid={Boolean(errors["schedule.keepalive_days"])}
                  disabled={saving}
                  onCommit={(value) => {
                    setSchedule({ keepalive_days: value });
                    clearError("schedule.keepalive_days");
                  }}
                />
              </ConfigRow>
              <ConfigRow
                label="惰性刷新窗口"
                hint="小时；距上次成功刷新不足该小时数的账号跳过，避免同一天多个保活时点重复刷新"
                htmlFor="schedule-lazy-refresh-hours"
                error={errors["schedule.lazy_refresh_hours"]}
              >
                <DraftNumberField
                  id="schedule-lazy-refresh-hours"
                  value={form.schedule.lazy_refresh_hours}
                  unit="时"
                  invalid={Boolean(errors["schedule.lazy_refresh_hours"])}
                  disabled={saving}
                  onCommit={(value) => {
                    setSchedule({ lazy_refresh_hours: value });
                    clearError("schedule.lazy_refresh_hours");
                  }}
                />
              </ConfigRow>

              {/* 逐账号「不参与自动签到」：只列国内站（国际站没有签到接口）。
                  名单内的账号排程与批量签到都会跳过，单账号手动签到不受影响。 */}
              <ConfigRow
                label="不参与自动签到的账号"
                hint="关闭后，排程签到与批量「刷新积分并签到」都会跳过该账号（仅国内站有签到接口，故只列国内站）；仍可在账号卡进行单账号手动签到"
              >
                <span className="text-xs text-muted-foreground">
                  {form.schedule.checkin_excluded_accounts.length > 0
                    ? `已排除 ${form.schedule.checkin_excluded_accounts.length} 个`
                    : "全部参与"}
                </span>
              </ConfigRow>
              <div className="divide-y divide-border/40">
                {accounts.length === 0 ? (
                  <p className="px-1 py-2 text-xs text-muted-foreground">账号列表加载中…</p>
                ) : (
                  accounts
                    .filter((account) => account.site === "cn")
                    .map((account) => {
                      const name = account.nickname || account.uid || account.file;
                      const allowed = !form.schedule.checkin_excluded_accounts.includes(account.id);
                      return (
                        <div
                          key={account.id}
                          className="flex min-h-[44px] items-center justify-between gap-3 py-1.5"
                        >
                          <span className="min-w-0 flex-1 truncate text-xs" title={name}>
                            {name}
                          </span>
                          <Switch
                            checked={allowed}
                            disabled={saving}
                            aria-label={`${name} 参与自动签到`}
                            onCheckedChange={(checked) => toggleExcludedAccount(account.id, checked)}
                          />
                        </div>
                      );
                    })
                )}
                {accounts.length > 0 && accounts.every((account) => account.site !== "cn") ? (
                  <p className="px-1 py-2 text-xs text-muted-foreground">暂无可签到的国内站账号</p>
                ) : null}
              </div>

              <ConfigRow label="余额后台刷新" hint="周期性刷新账号剩余积分">
                <Switch
                  checked={form.schedule.balance_refresh_enabled}
                  disabled={saving}
                  aria-label="余额后台刷新"
                  onCheckedChange={(checked) => setSchedule({ balance_refresh_enabled: checked })}
                />
              </ConfigRow>
              <ConfigRow
                label="余额刷新间隔"
                hint="两次刷新之间的间隔"
                htmlFor="schedule.balance-refresh-minutes"
                error={errors["schedule.balance_refresh_minutes"]}
              >
                <NumberField
                  id="schedule.balance-refresh-minutes"
                  value={form.schedule.balance_refresh_minutes}
                  unit="分"
                  invalid={Boolean(errors["schedule.balance_refresh_minutes"])}
                  disabled={saving}
                  onChange={(value) => {
                    setSchedule({ balance_refresh_minutes: value });
                    clearError("schedule.balance_refresh_minutes");
                  }}
                />
              </ConfigRow>

              <ConfigRow
                label="任务中心每日执行"
                hint="到点自动跑一次成长任务（报名 → 可自动完成 → 领奖）"
              >
                <Switch
                  checked={form.schedule.tasks_enabled}
                  disabled={saving}
                  aria-label="任务中心每日执行"
                  onCheckedChange={(checked) => {
                    setSchedule({ tasks_enabled: checked });
                    clearError("schedule.tasks_hours");
                  }}
                />
              </ConfigRow>
              <ConfigRow
                label="任务中心小时"
                hint="选择每天执行任务中心的整点，可多选"
                htmlFor="schedule-tasks-hours"
                error={errors["schedule.tasks_hours"]}
              >
                <HourPicker
                  id="schedule-tasks-hours"
                  value={form.schedule.tasks_hours}
                  disabled={saving}
                  onChange={(hours) => {
                    setSchedule({ tasks_hours: hours });
                    clearError("schedule.tasks_hours");
                  }}
                />
              </ConfigRow>

              <ConfigRow
                label="夜猫子任务"
                hint="夜间折扣活动只在 23:00-08:00 计分，需连续 3 天"
              >
                <Switch
                  checked={form.schedule.blackcat_enabled}
                  disabled={saving}
                  aria-label="夜猫子任务"
                  onCheckedChange={(checked) => {
                    setSchedule({ blackcat_enabled: checked });
                    clearError("schedule.blackcat_hours");
                  }}
                />
              </ConfigRow>
              <ConfigRow
                label="夜猫子小时"
                hint="选择执行夜猫子任务的整点（应在 23:00-08:00 窗口内），可多选"
                htmlFor="schedule-blackcat-hours"
                error={errors["schedule.blackcat_hours"]}
              >
                <HourPicker
                  id="schedule-blackcat-hours"
                  value={form.schedule.blackcat_hours}
                  disabled={saving}
                  onChange={(hours) => {
                    setSchedule({ blackcat_hours: hours });
                    clearError("schedule.blackcat_hours");
                  }}
                />
              </ConfigRow>

              <ConfigRow
                label="旅行巡检"
                hint="对每个账号推进一趟猫猫旅行：无猫则领养，有猫则出发或领奖"
              >
                <Switch
                  checked={form.schedule.travel_enabled}
                  disabled={saving}
                  aria-label="旅行巡检"
                  onCheckedChange={(checked) => {
                    setSchedule({ travel_enabled: checked });
                    clearError("schedule.travel_hours");
                  }}
                />
              </ConfigRow>
              <ConfigRow
                label="旅行小时"
                hint="一轮需要「出发 + 到站领奖」两次推进，建议保留两个时点"
                htmlFor="schedule-travel-hours"
                error={errors["schedule.travel_hours"]}
              >
                <HourPicker
                  id="schedule-travel-hours"
                  value={form.schedule.travel_hours}
                  disabled={saving}
                  onChange={(hours) => {
                    setSchedule({ travel_hours: hours });
                    clearError("schedule.travel_hours");
                  }}
                />
              </ConfigRow>

              <ConfigRow
                label="活跃上报"
                hint="每个账号补一条对话活跃事件，用于点亮连登与领养前置"
              >
                <Switch
                  checked={form.schedule.activity_enabled}
                  disabled={saving}
                  aria-label="活跃上报"
                  onCheckedChange={(checked) => {
                    setSchedule({ activity_enabled: checked });
                    clearError("schedule.activity_hours");
                  }}
                />
              </ConfigRow>
              <ConfigRow
                label="活跃上报小时"
                hint="每天一次即可；建议放在签到之后"
                htmlFor="schedule-activity-hours"
                error={errors["schedule.activity_hours"]}
              >
                <HourPicker
                  id="schedule-activity-hours"
                  value={form.schedule.activity_hours}
                  disabled={saving}
                  onChange={(hours) => {
                    setSchedule({ activity_hours: hours });
                    clearError("schedule.activity_hours");
                  }}
                />
              </ConfigRow>

              {/* 保号类四任务是否覆盖已禁用账号。
                  「禁用」的语义是「不参与选号」，不是「停止一切上游保号行为」——
                  轮换养号（一次只放开一个号）时，闲置待命的号恰恰最需要签到与续期。 */}
              <ConfigRow
                label="保号任务覆盖已禁用账号"
                hint="默认关闭。打开后已禁用账号仍会签到 / 活跃上报 / 保活 / 刷新余额（依旧不参与选号）。适合「一次只放开一个账号、用禁用做流量开关」的轮换养号用法；猫猫旅行、夜猫子、连登管家与成长任务仍跳过禁用号"
              >
                <Switch
                  checked={form.schedule.include_disabled_in_tasks}
                  disabled={saving}
                  aria-label="保号任务覆盖已禁用账号"
                  onCheckedChange={(checked) =>
                    setSchedule({ include_disabled_in_tasks: checked })
                  }
                />
              </ConfigRow>

              {/* 客户端事件链开关。默认关闭，且说明必须写在界面上 ——
                  打开它意味着开始伪装客户端行为，那是个需要知情的决定。 */}
              <ConfigRow
                label="客户端事件链"
                hint="为「需电脑端 / 小程序」类任务上报客户端指纹事件（模板 / 灵感 / 画布 / 资料库 / 换肤 / 专家召唤 / 技能加载 / 校园日与 Sequential 链）。默认关闭：事件形状按实测样本复刻，但上游是否接受无法离线验证，且伪造事件有被风控识别的风险。专家系只伪造「点了召唤」这一个动作——专家 id 与对话都是真实的"
              >
                <Switch
                  checked={form.tasks.desktop_events_enabled}
                  disabled={saving}
                  aria-label="客户端事件链"
                  onCheckedChange={(checked) =>
                    setTasks({ desktop_events_enabled: checked })
                  }
                />
              </ConfigRow>
            </CardContent>
          </Section>

          {/* 入站 HTTP：目前只有请求读取上限。属装配期字段，改了要重启 —— 
              提示必须写在界面上，否则用户会以为保存后立即生效。 */}
          <Section
            id="config-server"
            title="入站 HTTP"
            description="网关自己监听的服务参数。这里改动的字段需要重启进程才生效。"
          >
            <CardContent>
              <ConfigRow
                label="请求读取上限"
                hint="入站请求读取（含 body 上传）总时长上限，形如 300s / 5m；0 = 不限制。大上下文或文件块经反代链上传较慢，调小会被掐成 400 read body i/o timeout；改后需重启进程"
                htmlFor="server-read-timeout"
                error={errors["server.read_timeout"]}
              >
                <Input
                  id="server-read-timeout"
                  className="w-28"
                  placeholder="300s"
                  value={form.server.read_timeout}
                  disabled={saving}
                  onChange={(e) => {
                    setServer({ read_timeout: e.target.value });
                    clearError("server.read_timeout");
                  }}
                />
              </ConfigRow>
            </CardContent>
          </Section>

          {/* 签到日志（批次 5）：读本地台账，纯只读、不打上游。 */}
          <Section
            id="config-checkin-logs"
            title="签到日志"
            description={
              checkinLogs
                ? `保留最近 ${checkinLogs.retention_days} 天、最多 ${checkinLogs.max_records} 条；本机明文保存，可能含账号昵称。`
                : "保留最近 30 天；本机明文保存，可能含账号昵称。"
            }
            action={
              <Button type="button" variant="ghost" size="sm" onClick={() => void loadCheckinLogs()}>
                <RefreshCw className="size-3.5" aria-hidden="true" />
                刷新
              </Button>
            }
          >
            <CardContent>
              {checkinLogsError ? (
                <p className="py-2 text-xs text-destructive">{checkinLogsError}</p>
              ) : !checkinLogs ? (
                <p className="py-2 text-xs text-muted-foreground">正在读取…</p>
              ) : checkinLogs.logs.length === 0 ? (
                <p className="py-2 text-xs text-muted-foreground">暂无签到记录</p>
              ) : (
                <ul className="max-h-72 divide-y divide-border/40 overflow-y-auto">
                  {checkinLogs.logs.map((item, index) => {
                    const tone = checkinResultView(item.result);
                    return (
                      <li
                        key={`${item.ts}-${index}`}
                        className="flex items-center justify-between gap-3 py-2 text-xs"
                      >
                        <div className="min-w-0 flex-1 truncate">
                          <span className="font-medium">
                            {item.account_name || item.account_id || "未知账号"}
                          </span>
                          <span className="ml-1.5 text-muted-foreground">
                            {item.site === "intl" ? "国际站" : "国内站"}
                          </span>
                          {item.error ? (
                            <span className="ml-1.5 text-destructive">（{item.error}）</span>
                          ) : null}
                        </div>
                        <div className="flex shrink-0 items-center gap-2">
                          <span className={tone.className}>{tone.text}</span>
                          <span className="tabular-nums text-muted-foreground">
                            {formatLogTime(item.ts)}
                          </span>
                        </div>
                      </li>
                    );
                  })}
                </ul>
              )}
            </CardContent>
          </Section>

          <PromptConfigCard
            value={form.prompt}
            effectivePreview={config.prompt?.effective_text_preview ?? ""}
            file={config.prompt?.file ?? ""}
            saving={saving}
            onChange={setPrompt}
          />

          {/* 外观（批次 5）：纯前端偏好，点选即刻生效并持久化，不跟随底部「保存配置」。
              主题与侧栏切换器共用同一 hook，两处状态同步。 */}
          <Section
            id="config-appearance"
            title="外观"
            description="主题与导航显示偏好；点选即刻生效并持久化在本机，不跟随底部「保存配置」。"
          >
            <CardContent className="divide-y divide-border/60">
              <ConfigRow label="主题" hint="选择浅色、深色，或跟随系统外观自动切换">
                <PillGroup<ThemePreference>
                  value={themePreference}
                  onChange={(next) => selectTheme(next)}
                  ariaLabel="主题"
                  options={[
                    { value: "system", label: "跟随系统" },
                    { value: "light", label: "浅色" },
                    { value: "dark", label: "深色" },
                  ]}
                />
              </ConfigRow>
              <ConfigRow
                label="显示关联会话菜单"
                hint="关闭后左侧导航不再显示「关联会话」入口，会话数据与关联关系不受影响"
              >
                <Switch
                  checked={sessionsNav}
                  aria-label="显示关联会话菜单"
                  onCheckedChange={(checked) => setSessionsNavEnabled(checked)}
                />
              </ConfigRow>
            </CardContent>
          </Section>

          {/* 支持工具（批次 5）：对齐 wb-switch，但只列本项目真实覆盖的入口；
              没有写能力的端（VS Code / CodeBuddy IDE）置灰并注明原因。 */}
          <Section
            id="config-tools"
            title="支持工具"
            description="控制各客户端入口是否在界面上出现；关闭只隐藏入口并跳过对应本机探测，不影响账号库与其它工具。"
          >
            <CardContent className="divide-y divide-border/60">
              {SUPPORTED_TOOLS.map((tool) => (
                <ConfigRow
                  key={tool.id}
                  label={tool.label}
                  hint={
                    tool.writable
                      ? tool.description
                      : `${tool.description}。${tool.unwritableNote ?? "本机暂不支持写入。"}`
                  }
                >
                  <Switch
                    checked={supportedTools[tool.id]}
                    disabled={!tool.writable}
                    aria-label={tool.label}
                    onCheckedChange={(checked) => {
                      setToolEnabled(tool.id, checked);
                      notifySuccess(`${checked ? "已开启" : "已关闭"} ${tool.label}`);
                    }}
                  />
                </ConfigRow>
              ))}
            </CardContent>
          </Section>

          {/* 通知历史（批次 5）：toast 的本地存档（lib/notify 写入），只读列表 + 清空。 */}
          <Section
            id="config-notifications"
            title="通知历史"
            description={`最近 ${NOTIFY_HISTORY_LIMIT} 条应用内提示，存于浏览器本机（localStorage），便于事后核对；只记录文字标题与描述。`}
            action={
              <AlertDialog>
                <AlertDialogTrigger asChild>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    disabled={notifications.length === 0}
                  >
                    <Trash2 className="size-3.5" aria-hidden="true" />
                    清空
                  </Button>
                </AlertDialogTrigger>
                <AlertDialogContent>
                  <AlertDialogHeader>
                    <AlertDialogTitle>清空通知历史？</AlertDialogTitle>
                    <AlertDialogDescription>
                      将删除本机保存的全部提示存档（仅当前浏览器），无法恢复。
                    </AlertDialogDescription>
                  </AlertDialogHeader>
                  <AlertDialogFooter>
                    <AlertDialogCancel>取消</AlertDialogCancel>
                    <AlertDialogAction
                      onClick={() => {
                        clearNotificationHistory();
                        notifySuccess("通知历史已清空");
                      }}
                    >
                      清空
                    </AlertDialogAction>
                  </AlertDialogFooter>
                </AlertDialogContent>
              </AlertDialog>
            }
          >
            <CardContent>
              {notifications.length === 0 ? (
                <p className="py-2 text-xs text-muted-foreground">还没有记录到任何提示。</p>
              ) : (
                <ul className="max-h-72 divide-y divide-border/40 overflow-auto">
                  {notifications.map((item, index) => (
                    <li key={`${item.time}-${index}`} className="py-1.5">
                      <div className="flex items-center gap-1.5 text-xs leading-4 text-muted-foreground">
                        <span
                          className={cn(
                            "size-1.5 shrink-0 rounded-full",
                            NOTIFY_LEVEL_DOT[item.level],
                          )}
                          aria-hidden
                        />
                        <span>{NOTIFY_LEVEL_LABEL[item.level]}</span>
                        <span aria-hidden>·</span>
                        <span>{formatNotificationTime(item.time)}</span>
                      </div>
                      <div className="mt-0.5 text-xs leading-5">{item.title}</div>
                      {item.description ? (
                        <div className="mt-0.5 break-all text-xs leading-5 text-muted-foreground">
                          {item.description}
                        </div>
                      ) : null}
                    </li>
                  ))}
                </ul>
              )}
            </CardContent>
          </Section>

          {/* 错误日志（批次 5 的 Web 降级形态）：展示真实落点 + 复制 + 下载最近归档。
              「打开系统文件管理器」只在 Tauri 壳内可行，浏览器场景无法保证，故不提供。 */}
          <Section
            id="config-error-log"
            title="错误日志"
            description="日志落点与下载入口。本面板是 Web 形态，不提供「打开系统文件管理器」——浏览器无法保证打开本机目录。"
          >
            <CardContent className="divide-y divide-border/60">
              <ConfigRow
                label="请求日志"
                hint="脱敏后的请求元数据按天写 JSONL；排障时最有用的一份落点"
              >
                <span className="flex min-w-0 max-w-[320px] items-center gap-1">
                  <span
                    className="truncate font-mono text-xs text-muted-foreground"
                    title={logPaths?.request_log_dir ?? ""}
                  >
                    {logPaths?.request_log_dir ?? "读取中…"}
                  </span>
                  <CopyIconButton
                    label="请求日志路径"
                    copied={copiedKey === "request_log_dir"}
                    onCopy={() => {
                      if (logPaths) void copy(logPaths.request_log_dir, "request_log_dir", "请求日志路径");
                    }}
                    className="size-7 shrink-0"
                  />
                </span>
              </ConfigRow>
              <ConfigRow
                label="事件日志"
                hint="出站改写 / 冷却熔断 / 任务等运行事件：只在内存保留最近 500 条（见「日志」页），进程重启即清空，不落盘"
              >
                <span className="text-xs text-muted-foreground">内存</span>
              </ConfigRow>
              <ConfigRow label="数据目录" hint="config.json、台账与凭据自动发现的落点">
                <span className="flex min-w-0 max-w-[320px] items-center gap-1">
                  <span
                    className="truncate font-mono text-xs text-muted-foreground"
                    title={logPaths?.data_dir ?? ""}
                  >
                    {logPaths?.data_dir ?? "读取中…"}
                  </span>
                  <CopyIconButton
                    label="数据目录路径"
                    copied={copiedKey === "data_dir"}
                    onCopy={() => {
                      if (logPaths) void copy(logPaths.data_dir, "data_dir", "数据目录路径");
                    }}
                    className="size-7 shrink-0"
                  />
                </span>
              </ConfigRow>
              <ConfigRow
                label="最近请求日志"
                hint={
                  logPaths?.latest_request_log
                    ? logPaths.latest_request_log
                    : logPaths?.request_log_dir_exists
                      ? "目录已生成，但还没有归档文件"
                      : "请求日志目录尚未生成（有请求后自动创建）"
                }
              >
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={!logPaths?.latest_request_log || downloadingLog}
                  onClick={() => void handleDownloadRequestLog()}
                >
                  {downloadingLog ? (
                    <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                  ) : (
                    <Download className="size-3.5" aria-hidden="true" />
                  )}
                  下载最近日志
                </Button>
                {logPaths?.latest_request_log ? (
                  <CopyIconButton
                    label="日志文件路径"
                    copied={copiedKey === "latest_request_log"}
                    onCopy={() => {
                      if (logPaths) void copy(logPaths.latest_request_log, "latest_request_log", "日志文件路径");
                    }}
                    className="size-7 shrink-0"
                  />
                ) : null}
              </ConfigRow>
            </CardContent>
          </Section>

          <RuntimeInfoCard config={config} />

          {/* 自启自成一块：它不在 config.json 里（是系统注册表 / plist / .desktop），
              点开关即刻生效，不跟随底部的「保存配置」。 */}
          <AutostartCard />

          {/* 检查更新同样自成一块：状态是网关的运行期状态（不是配置字段），
              只有里面的「自动检查」开关会写 config.json，而且它写的是自己的端点，
              不跟随底部的「保存配置」—— 免得用户以为不点保存就不会去查。 */}
          <UpdateCard />

          <div className="sticky bottom-0 z-10 -mx-6 border-t border-border bg-background/95 px-6 py-3 backdrop-blur sm:-mx-8 sm:px-8">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <span className="text-xs text-muted-foreground">
                {dirty ? "有未保存的改动" : "配置与已保存值一致"}
              </span>
              <div className="flex items-center gap-2">
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={handleReset}
                  disabled={!dirty || saving}
                >
                  <RotateCcw className="size-3.5" aria-hidden="true" />
                  重置为已保存值
                </Button>
                <Button
                  type="button"
                  size="sm"
                  onClick={() => void handleSave()}
                  disabled={!dirty || saving}
                >
                  {saving ? (
                    <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
                  ) : (
                    <Save className="size-3.5" aria-hidden="true" />
                  )}
                  保存配置
                </Button>
              </div>
            </div>
          </div>
        </div>
      ) : null}
    </div>
  );
}
