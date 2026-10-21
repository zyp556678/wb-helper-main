import { useState } from "react";

import { CardContent } from "@/components/ui/card";
import { Section } from "@/components/section";
import { Switch } from "@/components/ui/switch";
import { notifyError, notifySuccess } from "@/lib/notify";
import { describeError, saveConfig } from "@/lib/api";

export interface DebugLogCardProps {
  /** 当前 debug.enabled（需重启）。 */
  enabled: boolean;
  /** 当前 debug.full_requests（热生效）。 */
  fullRequests: boolean;
  /** 保存成功后由父层重新拉取配置。 */
  onSaved: () => void;
}

/**
 * 调试日志开关。
 *
 * 两个开关的生效方式不同，界面必须说清，否则「打开了却没日志」会被当成 bug：
 *   - enabled：事件流 sink 在启动时建，改完要重启进程；
 *   - full_requests：请求期读取，保存即生效（约 1 秒热加载）。
 *
 * full_requests 之所以值得单独暴露，是因为它记录的是**正文**：
 * 排障时临时打开、复现完立刻关掉，重启一次都嫌慢。
 */
export function DebugLogCard({ enabled, fullRequests, onSaved }: DebugLogCardProps) {
  const [saving, setSaving] = useState<"enabled" | "full_requests" | null>(null);

  async function toggle(field: "enabled" | "full_requests", next: boolean) {
    setSaving(field);
    try {
      await saveConfig({ debug: { [field]: next } });
      notifySuccess(next ? "调试开关已开启" : "调试开关已关闭");
      onSaved();
    } catch (error) {
      notifyError(`保存失败：${describeError(error)}`);
    } finally {
      setSaving(null);
    }
  }

  return (
    <Section
      id="config-debug"
      title="调试日志"
      description="排障用。事件流写到工作目录的 debug-YYYY-MM-DD.jsonl；默认关闭。"
    >
      <CardContent className="divide-y divide-border/60">
        <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 py-2.5">
          <div className="min-w-0">
            <div className="text-sm text-foreground">调试事件流</div>
            <p className="mt-1 text-xs leading-5 text-muted-foreground">
              记录请求进入/返回、策略拦截、上游重试等事件（不记提示词正文）。保存后约 1 秒内热生效。
            </p>
          </div>
          <Switch
            checked={enabled}
            disabled={saving !== null}
            aria-label="调试事件流"
            onCheckedChange={(checked) => void toggle("enabled", checked)}
          />
        </div>

        <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 py-2.5">
          <div className="min-w-0">
            <div className="text-sm text-foreground">完整请求正文</div>
            <p className="mt-1 text-xs leading-5 text-muted-foreground">
              在事件流基础上额外记录模型 API 的入站 / 出站正文与上游错误正文；已知凭据会脱敏，
              但提示词、代码与图片仍属敏感数据。保存即生效，排查完请关闭。需先打开「调试事件流」。
            </p>
          </div>
          <Switch
            checked={fullRequests}
            disabled={saving !== null || !enabled}
            aria-label="完整请求正文"
            onCheckedChange={(checked) => void toggle("full_requests", checked)}
          />
        </div>
      </CardContent>
    </Section>
  );
}
