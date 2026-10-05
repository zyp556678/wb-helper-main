import { useEffect, useRef, useState } from "react";
import { ChevronDown, ChevronRight, FileText } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import type { LocalSession } from "@/lib/types";
import { cn } from "@/lib/utils";

/**
 * 会话树（对齐 wb-switch 的 `session-tree.tsx`）。
 *
 * 分组口径与客户端侧栏一致：**任务（playground）平铺，空间按 cwd 最后一段分文件夹**。
 * 组头带三态复选框（全选 / 半选 / 未选），勾选集合由调用方持有 ——
 * 树自己不保存选择，避免「父组件换了数据、树的旧选择还在」。
 *
 * 刻意**不做搜索框**：与 switch 一致（它整棵树也没有搜索，只有三态勾选）。
 */

/** 三态复选框：全选 / 半选 / 未选。用原生 input + indeterminate（对齐 switch）。 */
function TreeCheckbox({
  checked,
  indeterminate,
  onChange,
  label,
}: {
  checked: boolean;
  indeterminate: boolean;
  onChange: (next: boolean) => void;
  label: string;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = indeterminate && !checked;
  }, [indeterminate, checked]);
  return (
    <input
      ref={ref}
      type="checkbox"
      className="size-3.5 shrink-0 accent-[var(--primary)]"
      checked={checked}
      aria-label={label}
      onChange={(e) => onChange(e.target.checked)}
      onClick={(e) => e.stopPropagation()}
    />
  );
}

/** 一个会话文件夹（空间下的分组）。 */
interface Folder {
  key: string;
  label: string;
  sessions: LocalSession[];
}

/** 按「任务 / 空间 → 文件夹」分组（与客户端侧栏一致）。 */
function groupSessions(sessions: LocalSession[]): { tasks: LocalSession[]; folders: Folder[] } {
  const tasks: LocalSession[] = [];
  const byFolder = new Map<string, Folder>();
  for (const s of sessions) {
    if (s.is_playground) {
      tasks.push(s);
      continue;
    }
    const label = folderLabel(s.cwd);
    const key = label;
    let f = byFolder.get(key);
    if (!f) {
      f = { key, label, sessions: [] };
      byFolder.set(key, f);
    }
    f.sessions.push(s);
  }
  // 文件夹顺序按会话出现顺序（列表已按更新时间倒序 → 最近活动的空间在前）。
  return { tasks, folders: [...byFolder.values()] };
}

/** 取 cwd 最后一段作为文件夹名（空 cwd → 「未分组」）。 */
function folderLabel(cwd: string): string {
  const trimmed = (cwd || "").trim().replace(/[\\/]+$/, "");
  if (!trimmed) return "未分组";
  const parts = trimmed.split(/[\\/]/);
  return parts[parts.length - 1] || "未分组";
}

export interface SessionTreeProps {
  sessions: LocalSession[];
  selected: Set<string>;
  onChange: (next: Set<string>) => void;
}

export function SessionTree({ sessions, selected, onChange }: SessionTreeProps) {
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const { tasks, folders } = groupSessions(sessions);

  const toggleCollapse = (key: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  /** 勾选/取消一组会话。 */
  const setMany = (items: LocalSession[], next: boolean) => {
    const out = new Set(selected);
    for (const s of items) {
      if (next) out.add(s.id);
      else out.delete(s.id);
    }
    onChange(out);
  };

  const countSelected = (items: LocalSession[]) => items.filter((s) => selected.has(s.id)).length;

  const renderRow = (s: LocalSession, indent: string) => (
    <li key={s.id}>
      <label
        className={cn(
          "flex cursor-pointer items-center gap-2 rounded-lg px-2 py-1.5 text-sm hover:bg-accent/50",
          indent,
        )}
      >
        <input
          type="checkbox"
          className="size-3.5 shrink-0 accent-[var(--primary)]"
          checked={selected.has(s.id)}
          onChange={(e) => {
            const out = new Set(selected);
            if (e.target.checked) out.add(s.id);
            else out.delete(s.id);
            onChange(out);
          }}
        />
        <span className="min-w-0 flex-1 truncate" title={s.title}>
          {s.title || "(无标题)"}
        </span>
        {s.has_body ? (
          <Badge variant="secondary" className="shrink-0 gap-1 text-xs font-normal">
            <FileText className="size-3" aria-hidden="true" />
            有内容
          </Badge>
        ) : (
          <Badge variant="outline" className="shrink-0 text-xs font-normal">
            无正文
          </Badge>
        )}
      </label>
    </li>
  );

  const renderGroupHeader = (key: string, label: string, items: LocalSession[], indent = "") => {
    const picked = countSelected(items);
    const isCollapsed = collapsed.has(key);
    return (
      <div className={cn("flex items-center gap-2 px-2 py-1.5", indent)}>
        <TreeCheckbox
          checked={picked === items.length && items.length > 0}
          indeterminate={picked > 0 && picked < items.length}
          onChange={(next) => setMany(items, next)}
          label={`选择${label}`}
        />
        <button
          type="button"
          onClick={() => toggleCollapse(key)}
          className="flex min-w-0 flex-1 items-center gap-1 text-left text-xs font-medium text-muted-foreground hover:text-foreground"
          aria-label={`${isCollapsed ? "展开" : "折叠"}${label}`}
        >
          {isCollapsed ? (
            <ChevronRight className="size-3.5 shrink-0" aria-hidden="true" />
          ) : (
            <ChevronDown className="size-3.5 shrink-0" aria-hidden="true" />
          )}
          <span className="truncate">{label}</span>
          <span className="shrink-0 tabular-nums">（{items.length}）</span>
        </button>
      </div>
    );
  };

  return (
    <div className="max-h-[38vh] space-y-1 overflow-y-auto pr-1" data-testid="session-tree">
      {tasks.length > 0 ? (
        <div>
          {renderGroupHeader("__tasks__", "任务", tasks)}
          {collapsed.has("__tasks__") ? null : (
            <ul className="space-y-0.5">{tasks.map((s) => renderRow(s, "pl-7"))}</ul>
          )}
        </div>
      ) : null}

      {folders.length > 0 ? (
        <div>
          {renderGroupHeader("__spaces__", "空间", folders.flatMap((f) => f.sessions))}
          {collapsed.has("__spaces__")
            ? null
            : folders.map((f) => (
                <div key={f.key}>
                  {renderGroupHeader(`folder:${f.key}`, f.label, f.sessions, "pl-7")}
                  {collapsed.has(`folder:${f.key}`) ? null : (
                    <ul className="space-y-0.5">{f.sessions.map((s) => renderRow(s, "pl-12"))}</ul>
                  )}
                </div>
              ))}
        </div>
      ) : null}
    </div>
  );
}
