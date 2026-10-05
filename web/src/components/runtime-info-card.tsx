import type * as React from "react";

import { CardContent } from "@/components/ui/card";
import { Section } from "@/components/section";
import type { PanelConfig } from "@/lib/types";

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-1 py-2.5">
      <span className="text-sm text-muted-foreground">{label}</span>
      <span className="min-w-0 max-w-[70%] break-all text-right font-mono text-xs text-foreground">
        {children}
      </span>
    </div>
  );
}

function BoolText({ value }: { value: boolean }) {
  return (
    <span className={value ? "text-primary-ink" : "text-muted-foreground"}>{value ? "是" : "否"}</span>
  );
}

/** 只读运行信息：保留账号页既有配置视图，避免这些信息在配置页缺失。 */
export function RuntimeInfoCard({ config }: { config: PanelConfig }) {
  const blocklist = config.models_filter.blocklist ?? [];
  const allowlist = config.models_filter.allowlist ?? [];

  return (
    <Section
      id="config-runtime"
      title="运行信息"
      description="只读，改动需编辑网关配置文件后重启。"
    >
      <CardContent className="divide-y divide-border/60">
        <Row label="监听地址">{config.listen || "(默认)"}</Row>
        <Row label="凭据来源">{config.credential_source || "未配置"}</Row>
        <Row label="访问鉴权">
          <BoolText value={config.auth_check_enabled} />
        </Row>
        <Row label="凭据热加载间隔">
          {config.reload_interval > 0 ? `${config.reload_interval} 秒` : "关闭"}
        </Row>
        <Row label="模型目录刷新间隔">
          {config.models_refresh > 0 ? `${config.models_refresh} 分钟` : "关闭"}
        </Row>
        <Row label="下游首字节超时">{config.upstream.header_timeout_seconds} 秒</Row>
        <Row label="下游空闲超时">{config.upstream.idle_timeout_seconds} 秒</Row>
        <Row label="出站代理">
          <BoolText value={config.upstream.proxy} />
        </Row>
        <Row label="调试日志">
          <BoolText value={config.debug_enabled} />
        </Row>
        <Row label="模型黑名单">
          {blocklist.length > 0 ? blocklist.join("、") : "未配置"}
        </Row>
        <Row label="模型白名单">
          {allowlist.length > 0 ? allowlist.join("、") : "未配置"}
        </Row>
      </CardContent>
    </Section>
  );
}
