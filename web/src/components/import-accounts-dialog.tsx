import { useCallback, useMemo, useRef, useState } from "react";
import { AlertTriangle, FileUp, Loader2, Upload } from "lucide-react";
import {
  notifySuccess,
  notifyError,
} from "@/lib/notify";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { describeError, importAccounts, previewImportAccounts } from "@/lib/api";
import type { ImportPreviewItem } from "@/lib/types";
import { cn } from "@/lib/utils";

export interface ImportAccountsDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** 导入成功（哪怕只成功一部分）后调用，用于刷新账号列表。 */
  onImported: () => void;
}

/** 预览项的可读标签：昵称优先，退回 uid，再退回下标。 */
function itemLabel(item: ImportPreviewItem, fallbackIndex: number): string {
  return item.nickname || item.uid || `第 ${fallbackIndex + 1} 项`;
}

/** 该项是否**可以**导入（有没有 token / uid）。 */
function importable(item: ImportPreviewItem): boolean {
  return item.reason === "";
}

/** 导入账号：选文件 → 预览并勾选 → 写入。
 *
 * 两处刻意的口径：
 *  1. **冲突项默认不勾选**。池里已有同 uid 时，默认把它排除，用户要主动勾上
 *     才覆盖 —— 静默覆盖一个正在用的账号凭据，代价可能是「某个账号突然要重新登录」
 *     而当事人根本不知道发生过什么。
 *  2. **文件内容原样留在这个组件里**，提交时回传给后端重新解析。后端**不回传 token**，
 *     所以不存在「预览响应里带着一堆 token 在浏览器里漂流」这件事。
 */
export function ImportAccountsDialog({
  open,
  onOpenChange,
  onImported,
}: ImportAccountsDialogProps) {
  const fileRef = useRef<HTMLInputElement | null>(null);
  /** 备份文件原文，提交时回传（不在预览响应里带 token，见文件头注释）。 */
  const [raw, setRaw] = useState("");
  const [fileName, setFileName] = useState("");
  const [items, setItems] = useState<ImportPreviewItem[] | null>(null);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const [overwrite, setOverwrite] = useState(false);
  const [parsing, setParsing] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reset = useCallback(() => {
    setRaw("");
    setFileName("");
    setItems(null);
    setSelected(new Set());
    setOverwrite(false);
    setError(null);
    setParsing(false);
    setSubmitting(false);
    if (fileRef.current) fileRef.current.value = "";
  }, []);

  const handleOpenChange = useCallback(
    (next: boolean) => {
      onOpenChange(next);
      if (!next) reset();
    },
    [onOpenChange, reset],
  );

  const handleFile = useCallback(async (file: File) => {
    setParsing(true);
    setError(null);
    setItems(null);
    setSelected(new Set());
    try {
      const text = await file.text();
      setRaw(text);
      setFileName(file.name);
      const res = await previewImportAccounts(text);
      const list = res.accounts ?? [];
      if (list.length === 0) {
        setError("文件里没有解析出任何账号");
        return;
      }
      setItems(list);
      // 默认勾选「可导入且不冲突」的那些；冲突项要用户主动勾。
      const preset = new Set<number>();
      for (const item of list) {
        if (importable(item) && !item.conflict) preset.add(item.index);
      }
      setSelected(preset);
    } catch (err) {
      setError(describeError(err));
    } finally {
      setParsing(false);
    }
  }, []);

  const counts = useMemo(() => {
    const list = items ?? [];
    return {
      total: list.length,
      selected: selected.size,
      conflicts: list.filter((i) => i.conflict).length,
      broken: list.filter((i) => !importable(i)).length,
    };
  }, [items, selected]);

  const toggle = useCallback((index: number, next: boolean) => {
    setSelected((prev) => {
      const copy = new Set(prev);
      if (next) copy.add(index);
      else copy.delete(index);
      return copy;
    });
  }, []);

  const selectAll = useCallback(() => {
    const list = items ?? [];
    setSelected(new Set(list.filter(importable).map((i) => i.index)));
  }, [items]);

  const clearAll = useCallback(() => setSelected(new Set()), []);

  const handleSubmit = useCallback(async () => {
    if (selected.size === 0) {
      notifyError("请先勾选要导入的账号");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const result = await importAccounts(
        raw,
        Array.from(selected).sort((a, b) => a - b),
        overwrite,
      );
      const added = result.imported?.length ?? 0;
      const over = result.overwritten?.length ?? 0;
      const skipped = result.skipped?.length ?? 0;

      if (added === 0 && over === 0) {
        // 一条都没进去：把后端给的原因原样带出来，不要只说「失败」。
        notifyError(skipped > 0 ? `没有导入任何账号：${result.skipped[0]}` : "没有导入任何账号");
      } else {
        const parts = [`已导入 ${added} 个`];
        if (over > 0) parts.push(`覆盖 ${over} 个`);
        if (skipped > 0) parts.push(`跳过 ${skipped} 个`);
        notifySuccess(parts.join("，") + "。token 可能已过期，首次调用时若报鉴权失败请重新登录。");
      }
      onImported();
      handleOpenChange(false);
    } catch (err) {
      setError(describeError(err));
    } finally {
      setSubmitting(false);
    }
  }, [selected, raw, overwrite, onImported, handleOpenChange]);

  const allSelected = counts.total > 0 && counts.selected === items?.filter(importable).length;

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <FileUp className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
            导入账号
          </DialogTitle>
          <DialogDescription>
            选择 JSON 文件，勾选要导入的账号。支持本工具导出的备份，也兼容 wb-switch
            导出的账号文件。
          </DialogDescription>
        </DialogHeader>

        <input
          ref={fileRef}
          type="file"
          accept=".json,application/json"
          className="hidden"
          onChange={(event) => {
            const file = event.target.files?.[0];
            if (file) void handleFile(file);
          }}
        />

        <div className="flex min-w-0 items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={parsing || submitting}
            onClick={() => fileRef.current?.click()}
          >
            {parsing ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <Upload className="size-3.5" aria-hidden="true" />
            )}
            {parsing ? "正在解析…" : "选择文件"}
          </Button>
          {fileName ? (
            <span
              className="min-w-0 truncate font-mono text-xs text-muted-foreground"
              title={fileName}
            >
              {fileName}
            </span>
          ) : (
            <span className="text-xs text-muted-foreground">尚未选择文件</span>
          )}
        </div>

        {error ? (
          <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2.5 text-xs text-destructive">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            <span className="min-w-0 leading-5">{error}</span>
          </div>
        ) : null}

        {items ? (
          <>
            <div className="flex flex-wrap items-center justify-between gap-2">
              <p className="text-xs text-muted-foreground">
                共 {counts.total} 个账号，已选{" "}
                <span className="font-medium text-foreground tabular-nums">{counts.selected}</span>{" "}
                个{counts.conflicts > 0 ? ` · ${counts.conflicts} 个已存在` : ""}
                {counts.broken > 0 ? ` · ${counts.broken} 个不可导入` : ""}
              </p>
              <div className="flex shrink-0 items-center gap-1">
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  onClick={allSelected ? clearAll : selectAll}
                >
                  {allSelected ? "取消全选" : "全选"}
                </Button>
              </div>
            </div>

            <ul className="max-h-[42vh] space-y-1.5 overflow-y-auto pr-1">
              {items.map((item) => {
                const ok = importable(item);
                const checked = selected.has(item.index);
                return (
                  <li key={item.index}>
                    <label
                      className={cn(
                        "flex items-start gap-2.5 rounded-lg border px-3 py-2",
                        ok ? "border-border" : "border-border/60 bg-muted/40",
                        !ok && "opacity-70",
                      )}
                    >
                      <Checkbox
                        checked={checked}
                        disabled={!ok || submitting}
                        onCheckedChange={(next) => toggle(item.index, next === true)}
                        className="mt-0.5 data-[state=checked]:border-primary data-[state=checked]:bg-primary"
                        aria-label={`选择 ${itemLabel(item, item.index)}`}
                      />
                      <span className="min-w-0 flex-1">
                        <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                          <span className="min-w-0 truncate text-sm font-medium">
                            {itemLabel(item, item.index)}
                          </span>
                          <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
                            {item.site_label || item.site}
                          </span>
                          {item.encrypted ? (
                            <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs font-medium text-muted-foreground">
                              加密凭据
                            </span>
                          ) : !item.has_token ? (
                            <span className="shrink-0 rounded bg-destructive/12 px-1.5 py-0.5 text-xs font-medium text-destructive">
                              缺少 token
                            </span>
                          ) : null}
                          {item.conflict ? (
                            <span className="shrink-0 rounded bg-amber-500/15 px-1.5 py-0.5 text-xs font-medium text-amber-800 dark:text-amber-300">
                              已存在
                            </span>
                          ) : null}
                        </span>
                        <span className="mt-0.5 block min-w-0 truncate font-mono text-xs text-muted-foreground">
                          {item.uid ? `UID ${item.uid}` : "无 UID"}
                          {item.domain ? ` · ${item.domain}` : ""}
                        </span>
                        {item.reason ? (
                          <span className="mt-0.5 block text-xs text-destructive">
                            {item.reason}
                          </span>
                        ) : null}
                      </span>
                    </label>
                  </li>
                );
              })}
            </ul>

            {counts.conflicts > 0 ? (
              <label className="flex items-start gap-2.5 rounded-lg border border-amber-500/40 bg-amber-500/8 px-3 py-2.5 text-xs">
                <Checkbox
                  checked={overwrite}
                  disabled={submitting}
                  onCheckedChange={(next) => setOverwrite(next === true)}
                  className="mt-0.5 data-[state=checked]:border-primary data-[state=checked]:bg-primary"
                  aria-label="覆盖已存在的同名账号"
                />
                <span className="min-w-0 leading-5">
                  <span className="font-medium">覆盖已存在的同名账号</span>
                  <span className="mt-0.5 block text-muted-foreground">
                    不勾选时，与账号池中 UID 相同的项会被跳过；勾选后会直接改写这些账号的凭据文件。
                    若这些账号正在被使用，改写后可能需要重新登录。
                  </span>
                </span>
              </label>
            ) : null}
          </>
        ) : null}

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={submitting}
            onClick={() => handleOpenChange(false)}
          >
            取消
          </Button>
          <Button
            type="button"
            size="sm"
            disabled={submitting || !items || selected.size === 0}
            onClick={() => void handleSubmit()}
          >
            {submitting ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : null}
            {submitting ? "导入中…" : "导入勾选账号"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
