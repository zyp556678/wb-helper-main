import { useCallback, useEffect, useRef, useState } from "react";
import { Download, FileDown, Loader2, ShieldAlert } from "lucide-react";
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
import { describeError, exportAccounts } from "@/lib/api";
import type { Account } from "@/lib/types";
import { formatExpiryFull, siteLabel } from "@/lib/format";

export interface ExportAccountsDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  accounts: Account[];
}

/** 账号名兜底（与卡片同一口径）。 */
function accountName(account: Account): string {
  return account.nickname || account.uid || account.file || "未命名账号";
}

/**
 * 导出账号：勾选 → 下载 JSON。
 *
 * 两处刻意的做法：
 *  1. **默认全选**。与「导入」相反 —— 导入是往池子里加东西（默认保守），
 *     导出只是把已有数据读出来（默认方便）。
 *  2. **不落盘到服务端**。文件由浏览器下载，服务端只返回 JSON。服务端写文件
 *     要处理「让用户选目录」这类跨平台交互，而下载这件事浏览器已经解决了。
 */
export function ExportAccountsDialog({ open, onOpenChange, accounts }: ExportAccountsDialogProps) {
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);

  /**
   * 用 ref 读最新的账号列表，让下面的初始化 effect **只依赖 open**。
   *
   * 若把 accounts 也放进依赖，那么「面板空闲时刷新了一次账号列表」会把这个
   * effect 重跑一遍，把用户已经取消的勾选原样恢复成全选 —— 一个只在
   * 「弹窗开着的时候刚好有刷新」才复现的诡异性。
   */
  const accountsRef = useRef(accounts);
  accountsRef.current = accounts;

  // 每次打开都重置为全选（导出是「把已有数据读出来」，默认方便）。
  useEffect(() => {
    if (open) setSelected(new Set(accountsRef.current.map((a) => a.id)));
  }, [open]);

  const handleOpenChange = useCallback(
    (next: boolean) => {
      onOpenChange(next);
    },
    [onOpenChange],
  );

  const toggle = useCallback((id: string, next: boolean) => {
    setSelected((prev) => {
      const copy = new Set(prev);
      if (next) copy.add(id);
      else copy.delete(id);
      return copy;
    });
  }, []);

  const handleExport = useCallback(async () => {
    if (selected.size === 0) {
      notifyError("请先勾选要导出的账号");
      return;
    }
    setBusy(true);
    try {
      const doc = await exportAccounts(Array.from(selected));
      const blob = new Blob([JSON.stringify(doc, null, 2)], {
        type: "application/json",
      });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      const stamp = new Date().toISOString().slice(0, 10);
      a.href = url;
      a.download = `workbuddy-gateway-accounts-${stamp}.json`;
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      // 立刻 revoke 会让部分浏览器来不及取到内容，延后一拍。
      window.setTimeout(() => URL.revokeObjectURL(url), 1000);

      notifySuccess(
        `已导出 ${doc.accounts.length} 个账号。文件含登录 token，等同密码，请勿上传网盘或发送给他人。`,
      );
      handleOpenChange(false);
    } catch (err) {
      notifyError(describeError(err));
    } finally {
      setBusy(false);
    }
  }, [selected, handleOpenChange]);

  const allSelected = accounts.length > 0 && selected.size === accounts.length;

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <FileDown className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
            导出账号
          </DialogTitle>
          <DialogDescription>勾选要导出的账号，导出为 JSON 文件。</DialogDescription>
        </DialogHeader>

        <div className="flex items-start gap-2 rounded-lg border border-amber-500/40 bg-amber-500/8 px-3 py-2.5 text-xs">
          <ShieldAlert
            className="mt-0.5 size-3.5 shrink-0 text-amber-700 dark:text-amber-300"
            aria-hidden="true"
          />
          <span className="min-w-0 leading-5">
            <span className="font-medium">安全提示</span>
            <span className="mt-0.5 block text-muted-foreground">
              导出文件含登录 token，等同密码，请勿上传网盘或发送给他人。
            </span>
          </span>
        </div>

        {accounts.length === 0 ? (
          <p className="py-6 text-center text-sm text-muted-foreground">暂无账号可导出。</p>
        ) : (
          <>
            <div className="flex flex-wrap items-center justify-between gap-2">
              <p className="text-xs text-muted-foreground">
                共 {accounts.length} 个账号，已选{" "}
                <span className="font-medium text-foreground tabular-nums">{selected.size}</span> 个
              </p>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() =>
                  setSelected(allSelected ? new Set() : new Set(accounts.map((a) => a.id)))
                }
              >
                {allSelected ? "取消全选" : "全选"}
              </Button>
            </div>

            <ul className="max-h-[42vh] space-y-1.5 overflow-y-auto pr-1">
              {accounts.map((account) => (
                <li key={account.id}>
                  <label className="flex items-start gap-2.5 rounded-lg border border-border px-3 py-2">
                    <Checkbox
                      checked={selected.has(account.id)}
                      disabled={busy}
                      onCheckedChange={(next) => toggle(account.id, next === true)}
                      className="mt-0.5 data-[state=checked]:border-primary data-[state=checked]:bg-primary"
                      aria-label={`选择 ${accountName(account)}`}
                    />
                    <span className="min-w-0 flex-1">
                      <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="min-w-0 truncate text-sm font-medium">
                          {accountName(account)}
                        </span>
                        <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
                          {siteLabel(account)}
                        </span>
                      </span>
                      <span className="mt-0.5 block min-w-0 truncate font-mono text-xs text-muted-foreground">
                        {account.file}
                        {account.token_expires_at > 0
                          ? ` · Token 到期 ${formatExpiryFull(account.token_expires_at * 1000)}`
                          : ""}
                      </span>
                    </span>
                  </label>
                </li>
              ))}
            </ul>
          </>
        )}

        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={() => handleOpenChange(false)}
          >
            取消
          </Button>
          <Button
            type="button"
            size="sm"
            disabled={busy || selected.size === 0}
            onClick={() => void handleExport()}
          >
            {busy ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <Download className="size-3.5" aria-hidden="true" />
            )}
            {busy ? "导出中…" : "导出勾选账号"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
