import { useState } from "react";
import { KeyRound } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { getApiKey, setApiKey } from "@/lib/api";

export interface ApiKeyPanelProps {
  /** 后端主动要求鉴权（401/403）时提示语更直接。 */
  unauthorized: boolean;
  onSaved: () => void;
  onDismiss: () => void;
}

/**
 * 访问密钥输入：密钥只存在 localStorage，随请求以 Authorization: Bearer 发出。
 * 后端未配置 api_key 时留空即可，无需填写。
 */
export function ApiKeyPanel({ unauthorized, onSaved, onDismiss }: ApiKeyPanelProps) {
  const [value, setValue] = useState(() => getApiKey());
  const [saved, setSaved] = useState(false);

  const save = () => {
    setApiKey(value.trim());
    setSaved(true);
    onSaved();
  };

  const clear = () => {
    setValue("");
    setApiKey("");
    setSaved(false);
    onSaved();
  };

  return (
    <Card className="mb-5 gap-0 p-5">
      <div className="flex items-start gap-3">
        <span
          aria-hidden
          className="inline-flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/12 text-primary-ink"
        >
          <KeyRound className="size-4" />
        </span>
        <div className="min-w-0 flex-1">
          <div className="text-sm font-medium">访问密钥</div>
          <p className="mt-1 text-xs leading-5 text-muted-foreground">
            {unauthorized
              ? "网关要求鉴权：请输入后端配置的 api_key，密钥保存在本机浏览器，不会上传到别处。"
              : "仅当网关配置了 api_key 时需要填写；未配置时可留空。"}
          </p>
          <div className="mt-3 flex flex-wrap items-center gap-2">
            <Input
              type="password"
              value={value}
              autoComplete="off"
              spellCheck={false}
              aria-label="访问密钥"
              placeholder="粘贴 api_key"
              onChange={(event) => {
                setValue(event.target.value);
                setSaved(false);
              }}
              onKeyDown={(event) => {
                if (event.key === "Enter") save();
              }}
              className="h-8 w-full max-w-[320px] font-mono text-xs"
            />
            <Button type="button" size="sm" onClick={save}>
              保存并重试
            </Button>
            <Button type="button" size="sm" variant="ghost" onClick={clear}>
              清除
            </Button>
            <Button type="button" size="sm" variant="ghost" onClick={onDismiss}>
              收起
            </Button>
          </div>
          {saved ? <p className="mt-2 text-xs text-primary-ink">已保存，正在重新拉取账号…</p> : null}
        </div>
      </div>
    </Card>
  );
}
