import { useEffect, useState } from "react";
import { Check, Copy, Loader2, PencilLine, UserRound } from "lucide-react";
import {
  notifySuccess,
  notifyError,
} from "@/lib/notify";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { updateAccountMeta } from "@/lib/api";
import { accountDisplayName, siteLabel } from "@/lib/format";
import type { Account, DisplayField } from "@/lib/types";
import { cn, copyToClipboard } from "@/lib/utils";

/**
 * 备注长度上限：与后端 `internal/accountmeta.MaxNoteRunes` 保持一致，
 * 超限时后端会返回 400；这里先在前端拦住以获得即时反馈。
 */
const NOTE_MAX_LENGTH = 24;

const FIELD_OPTIONS: Array<{ value: DisplayField; label: string }> = [
  { value: "nickname", label: "账号名" },
  { value: "phone", label: "手机号" },
  { value: "note", label: "备注" },
];

export interface AccountInfoDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** 目标账号；为 null 时不渲染内容（保留上一次快照播完退出动画）。 */
  account: Account | null;
  /** 保存成功后回调（父层刷新账号列表）。 */
  onSaved?: () => void;
}

/**
 * 账号信息弹窗（批次 4，对齐 wb-switch 的 account-info-dialog）。
 *
 * 可以做的事：查看企业名 / UID（UID 一键复制）、编辑本地备注（24 字上限）、
 * 选择卡片显示字段（账号名 / 手机号 / 备注，带实时预览）。
 *
 * 两处与本项目事实对齐的裁剪（见注释内说明）：
 *  - 没有手机号数据源：`phone` 选项禁用并注明；此前存过 phone 的账号回退昵称；
 *  - 凭据模型里没有企业名：有 enterprise_name 就展示，缺省回退展示企业 ID。
 */
export function AccountInfoDialog({ open, onOpenChange, account, onSaved }: AccountInfoDialogProps) {
  const [note, setNote] = useState("");
  const [field, setField] = useState<DisplayField>("nickname");
  const [busy, setBusy] = useState(false);
  const [uidCopied, setUidCopied] = useState(false);
  // 关闭时父层会把 account 置空：保留最后一次快照，让退出动画播完再卸载。
  const [snapshot, setSnapshot] = useState<Account | null>(null);

  useEffect(() => {
    if (open && account) {
      setNote(account.note ?? "");
      setField(account.display_field === "phone" || account.display_field === "note"
        ? account.display_field
        : "nickname");
      setBusy(false);
      setUidCopied(false);
    }
  }, [open, account]);

  useEffect(() => {
    if (account) setSnapshot(account);
  }, [account]);

  const current = account ?? snapshot;
  if (!current) return null;

  const accountId = current.id;
  // 本项目没有手机号数据源：phone 选项恒不可选；已存 phone 的账号回退昵称。
  const effectiveField: DisplayField = field === "phone" ? "nickname" : field;

  /** 实时预览：备注格跟随输入框草稿，改完即可见。 */
  function previewValue(value: DisplayField): string {
    if (value === "nickname") return current?.nickname || current?.uid || "—";
    if (value === "phone") return "无手机号";
    return note.trim() || "未填写备注";
  }

  async function copyUid(value: string) {
    const ok = await copyToClipboard(value);
    if (!ok) {
      notifyError("复制失败，请手动选择复制");
      return;
    }
    setUidCopied(true);
    notifySuccess("已复制 UID");
    window.setTimeout(() => setUidCopied(false), 1500);
  }

  async function save() {
    setBusy(true);
    try {
      await updateAccountMeta(accountId, {
        note: note.trim(),
        display_field: effectiveField,
      });
      notifySuccess("账号信息已保存");
      onSaved?.();
      onOpenChange(false);
    } catch (err) {
      notifyError("保存失败", { description: err instanceof Error ? err.message : "未知错误" });
    } finally {
      setBusy(false);
    }
  }

  const name = accountDisplayName(current);
  const identity = current.uid ? `UID ${current.uid}` : current.file;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-md">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <UserRound className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
            账号信息
          </DialogTitle>
          <DialogDescription>备注与显示字段仅保存在本机，不影响官方登录数据。</DialogDescription>
        </DialogHeader>

        {/* 身份区：与账号卡片同一视觉语言（首字母头像 + 显示名 + 站点徽标）。 */}
        <div className="flex items-center gap-3">
          <div
            aria-hidden="true"
            className="flex size-11 shrink-0 items-center justify-center rounded-full bg-primary/12 text-base font-semibold text-primary-ink"
          >
            {name.charAt(0).toUpperCase()}
          </div>
          <div className="min-w-0 flex-1">
            <div className="flex min-w-0 items-center gap-2">
              <span className="truncate text-sm font-semibold" title={name}>
                {name}
              </span>
              <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
                {siteLabel(current)}
              </span>
            </div>
            <p className="mt-0.5 truncate text-xs text-muted-foreground" title={identity}>
              {identity}
            </p>
          </div>
        </div>

        {/* 次级字段：企业名与 UID（UID 等宽 + 一键复制）。 */}
        <dl className="divide-y divide-border/70 rounded-lg border text-xs">
          <div className="flex items-center gap-3 px-3 py-2.5">
            <dt className="w-14 shrink-0 text-muted-foreground">企业名</dt>
            <dd
              className="min-w-0 flex-1 truncate font-medium"
              title={current.enterprise_name ?? current.enterprise_id ?? ""}
            >
              {/* 本项目凭据里没有企业名，用企业 ID 兜底展示（有值就是事实）。 */}
              {current.enterprise_name || current.enterprise_id || "—"}
            </dd>
          </div>
          <div className="flex items-center gap-3 px-3 py-2.5">
            <dt className="w-14 shrink-0 text-muted-foreground">UID</dt>
            <dd
              className="min-w-0 flex-1 truncate font-mono text-[11px] text-muted-foreground"
              title={current.uid ?? ""}
            >
              {current.uid || "—"}
            </dd>
            {current.uid ? (
              <button
                type="button"
                className="shrink-0 cursor-pointer rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                onClick={() => void copyUid(current.uid)}
                aria-label="复制 UID"
                title="复制 UID"
              >
                {uidCopied ? (
                  <Check className="size-3.5 text-primary-ink" aria-hidden="true" />
                ) : (
                  <Copy className="size-3.5" aria-hidden="true" />
                )}
              </button>
            ) : null}
          </div>
        </dl>

        <div className="space-y-1.5">
          <div className="flex items-baseline justify-between">
            <label htmlFor="account-note" className="text-xs font-medium text-muted-foreground">
              备注
            </label>
            <span className="text-xs tabular-nums text-muted-foreground">
              {note.length}/{NOTE_MAX_LENGTH}
            </span>
          </div>
          <Input
            id="account-note"
            value={note}
            maxLength={NOTE_MAX_LENGTH}
            placeholder={`最多 ${NOTE_MAX_LENGTH} 个字符`}
            onChange={(event) => setNote(event.target.value)}
          />
        </div>

        <div className="space-y-1.5">
          <p className="text-xs font-medium text-muted-foreground">卡片显示</p>
          <div className="grid grid-cols-3 gap-2" role="radiogroup" aria-label="卡片显示字段">
            {FIELD_OPTIONS.map((opt) => {
              const id = `display-field-${opt.value}`;
              const disabled = opt.value === "phone"; // 无手机号数据源
              const selected = effectiveField === opt.value;
              return (
                <label
                  key={opt.value}
                  htmlFor={id}
                  className={cn(
                    "flex cursor-pointer flex-col gap-1 rounded-lg border px-2.5 py-2 transition-colors",
                    selected ? "border-primary/60 bg-primary/5" : "border-border hover:bg-muted/40",
                    disabled && "cursor-not-allowed opacity-50 hover:bg-transparent",
                  )}
                  title={disabled ? "本项目没有手机号数据源，无法按手机号显示" : undefined}
                >
                  <span className="flex items-center justify-between gap-1">
                    <span className="text-xs font-medium">{opt.label}</span>
                    <input
                      id={id}
                      type="radio"
                      name="account-display-field"
                      className="size-3.5 shrink-0 accent-[var(--primary)]"
                      checked={selected}
                      disabled={disabled}
                      onChange={() => setField(opt.value)}
                    />
                  </span>
                  <span className="truncate text-xs text-muted-foreground">
                    {previewValue(opt.value)}
                  </span>
                </label>
              );
            })}
          </div>
          <p className="flex items-start gap-1.5 text-xs leading-5 text-muted-foreground">
            <PencilLine className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
            本项目没有手机号数据源，无法按手机号显示；此前选过手机号的账号会回退到账号名。
          </p>
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" disabled={busy} onClick={() => onOpenChange(false)}>
            取消
          </Button>
          <Button type="button" disabled={busy} onClick={() => void save()}>
            {busy ? <Loader2 className="size-3.5 animate-spin" aria-hidden="true" /> : null}
            保存
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
