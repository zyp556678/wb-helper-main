import { useCallback, useState } from "react";
import { Loader2, Ticket } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CopyIconButton, useCopy } from "@/components/copy-button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { describeError, fetchTaskVouchers } from "@/lib/api";
import { notifyError } from "@/lib/notify";
import type { TaskVoucherGroup } from "@/lib/types";

/**
 * 开学季券码弹窗。
 *
 * 券码是**要复制去核销**的东西，所以这里按账号分组、每条券码配一个复制按钮，
 * 而不是拍平成一个列表：拍平之后用户分不清哪张券属于哪个号，
 * 而核销时给错号是要出事的。
 */
export function VouchersDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [groups, setGroups] = useState<TaskVoucherGroup[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const { copiedKey, copy } = useCopy();

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const res = await fetchTaskVouchers();
      if (res.ok === false) {
        setError(res.detail || "读取券码失败");
        setGroups([]);
        return;
      }
      setGroups(res.groups ?? []);
      setError(null);
    } catch (err) {
      const msg = describeError(err);
      setError(msg);
      notifyError(msg);
    } finally {
      setLoading(false);
    }
  }, []);

  // 打开时才拉取：券码变动很少，没必要每次进页面都打一遍上游。
  const handleOpenChange = useCallback(
    (next: boolean) => {
      if (next && groups === null && !loading) void load();
      onOpenChange(next);
    },
    [groups, loading, load, onOpenChange],
  );

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>我的券码</DialogTitle>
          <DialogDescription>
            开学季抽奖抽中的第三方券码（KFC / 瑞幸 / 酷狗等）。积分奖励不在这里，
            券码请复制给门店核销。
          </DialogDescription>
        </DialogHeader>

        {loading ? (
          <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
            <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            正在读取…
          </div>
        ) : error ? (
          <p className="py-6 text-sm text-destructive">{error}</p>
        ) : (
          <div className="max-h-[52vh] space-y-3 overflow-y-auto pr-1">
            {(groups ?? []).length === 0 ? (
              <p className="py-6 text-center text-sm text-muted-foreground">还没有查询到券码。</p>
            ) : (
              (groups ?? []).map((g) => (
                <div key={g.account} className="rounded-lg border border-border/60 px-3 py-2.5">
                  <div className="flex flex-wrap items-center gap-2 text-sm">
                    <span className="font-medium">{g.nickname || g.account}</span>
                    <Badge variant="outline">{g.site_label}</Badge>
                    {g.note ? (
                      <span className="text-xs text-muted-foreground">{g.note}</span>
                    ) : null}
                    {g.error ? <span className="text-xs text-destructive">{g.error}</span> : null}
                    {!g.note && !g.error ? (
                      <span className="text-xs text-muted-foreground">{g.vouchers.length} 张</span>
                    ) : null}
                  </div>
                  {g.vouchers.length > 0 ? (
                    <div className="mt-2 space-y-1.5">
                      {g.vouchers.map((v) => (
                        <div
                          key={`${v.grant_id}-${v.code}`}
                          className="flex flex-wrap items-center gap-2 text-xs"
                        >
                          <span className="min-w-[8rem] font-medium">
                            {v.prize_name || v.sku_code || "券"}
                          </span>
                          <code className="rounded bg-muted px-1.5 py-0.5 font-mono">{v.code}</code>
                          <CopyIconButton
                            label="券码"
                            copied={copiedKey === v.code}
                            onCopy={() => void copy(v.code, v.code, "券码")}
                          />
                          {v.valid_to ? (
                            <span className="text-muted-foreground">有效期至 {v.valid_to}</span>
                          ) : null}
                        </div>
                      ))}
                    </div>
                  ) : null}
                </div>
              ))
            )}
          </div>
        )}

        <div className="flex justify-end gap-2">
          <Button type="button" size="sm" variant="outline" disabled={loading} onClick={() => void load()}>
            {loading ? (
              <Loader2 className="size-3.5 animate-spin" aria-hidden="true" />
            ) : (
              <Ticket className="size-3.5" aria-hidden="true" />
            )}
            重新读取
          </Button>
        </div>
      </DialogContent>
    </Dialog>
  );
}
