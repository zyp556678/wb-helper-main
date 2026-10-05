import { useCallback, useEffect, useRef, useState } from "react";

import { describeError, fetchConfig, fetchModels, isUnauthorized } from "@/lib/api";
import type { ConfigModelsFilter, ModelsResponse } from "@/lib/types";

export interface ModelsData {
  /** 模型目录快照（GET /panel/api/models）。 */
  catalog: ModelsResponse | null;
  /** 权威黑白名单（GET /panel/api/config 的 models_filter）。 */
  filter: ConfigModelsFilter | null;
  loading: boolean;
  error: string | null;
  /** 后端要求 api_key 而当前密钥缺失 / 不正确。 */
  unauthorized: boolean;
  /** 带 loading 的重新拉取（首屏 / 失败重试）。 */
  reload: () => void;
  /** 静默刷新：用于写操作后的回读，不闪骨架。 */
  refreshSilently: () => void;
  /** 保存黑白名单成功后，用返回体里的权威值覆盖本地副本。 */
  applyFilter: (next: ConfigModelsFilter) => void;
  /**
   * 网关是否要求访问密钥。
   *
   * 顺带从同一个 config 响应里带出来（`useModels` 本来就要拉 config），
   * 这样「接入信息」卡片不必再发一次请求就能知道密钥该不该填。
   */
  authCheckEnabled: boolean;
  /** 网关**实际配置**的访问密钥；未配置时是空串（详见 PanelConfig.api_key）。 */
  configuredApiKey: string;
}

/** 模型页数据源：目录与黑白名单一起拉，共用一个 loading / error 状态。 */
export function useModels(): ModelsData {
  const [catalog, setCatalog] = useState<ModelsResponse | null>(null);
  const [filter, setFilter] = useState<ConfigModelsFilter | null>(null);
  const [authCheckEnabled, setAuthCheckEnabled] = useState(false);
  const [configuredApiKey, setConfiguredApiKey] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [unauthorized, setUnauthorized] = useState(false);
  // 批量探测后的静默刷新可能连续触发，用 ref 防止请求叠加。
  const inFlight = useRef(false);

  const load = useCallback(async (silent: boolean) => {
    if (inFlight.current) return;
    inFlight.current = true;
    if (!silent) setLoading(true);
    try {
      const [nextCatalog, nextConfig] = await Promise.all([fetchModels(), fetchConfig()]);
      setCatalog(nextCatalog);
      // Go 侧 nil slice 会序列化成 null，这里兜底成空数组。
      setFilter({
        blocklist: nextConfig.models_filter?.blocklist ?? [],
        allowlist: nextConfig.models_filter?.allowlist ?? [],
      });
      setAuthCheckEnabled(Boolean(nextConfig.auth_check_enabled));
      setConfiguredApiKey(nextConfig.api_key ?? "");
      setError(null);
      setUnauthorized(false);
    } catch (err) {
      setError(describeError(err));
      setUnauthorized(isUnauthorized(err));
    } finally {
      inFlight.current = false;
      if (!silent) setLoading(false);
    }
  }, []);

  const reload = useCallback(() => {
    void load(false);
  }, [load]);

  const refreshSilently = useCallback(() => {
    void load(true);
  }, [load]);

  const applyFilter = useCallback((next: ConfigModelsFilter) => {
    setFilter({
      blocklist: next?.blocklist ?? [],
      allowlist: next?.allowlist ?? [],
    });
  }, []);

  useEffect(() => {
    void load(false);
  }, [load]);

  return {
    catalog,
    filter,
    loading,
    error,
    unauthorized,
    reload,
    refreshSilently,
    applyFilter,
    authCheckEnabled,
    configuredApiKey,
  };
}
