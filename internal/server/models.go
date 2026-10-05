package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/catalog"
	"workbuddy-gateway/internal/metrics"
)

// 本文件是模型目录相关的面板接口与后台刷新（切片 3）。
//
// 目录数据本身由 internal/catalog 维护（双源合并 + 缓存 + 价格探测台账），
// 这里只负责把它组装成面板需要的形状。

// reloadLoop 周期扫描凭据来源，新增 / 更新 / 删除免重启生效。
func (s *Server) reloadLoop(ctx context.Context) {
	interval := time.Duration(s.config().ReloadInterval) * time.Second
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.pool.Reload() {
				s.logf("[凭据] 检测到变化，账号池已热加载（当前 %d 个账号）", s.pool.Len())
				// 账号变化后可能新增站点，目录需要跟着刷新
				go func() {
					if err := s.cat.RefreshOnce(context.Background()); err != nil {
						s.logf("[目录] 账号变化后刷新失败: %v", err)
					}
				}()
			}
		}
	}
}

// -----------------------------------------------------------------------------
// 面板接口
// -----------------------------------------------------------------------------

// handlePanelModels 返回模型目录 + 倍率 + 探测台账 + 运行指标。
func (s *Server) handlePanelModels(w http.ResponseWriter, r *http.Request) {
	source, fetchedAt := s.cat.Source()
	models := s.cat.Merged()
	now := time.Now()

	// 模型级运行指标（请求/成功/失败/token/首字与总耗时/最近状态）。
	// 与「可用账号数」「免费判定」并排展示才有诊断价值：
	// 只看请求数说不了「这个模型现在还行不行」，只看倍率也说不了「它实际跑得快不快」。
	statsByModel := map[string]metrics.ModelSnapshot{}
	for _, st := range s.metrics.Snapshot() {
		statsByModel[st.Model] = st
	}

	// 探测汇总
	probes := s.cat.Probes()
	summary := map[string]any{"free": 0, "paid": 0, "unknown": 0, "last_probe_at": int64(0)}
	for _, p := range probes {
		switch p.Verdict {
		case "free":
			summary["free"] = summary["free"].(int) + 1
		case "paid":
			summary["paid"] = summary["paid"].(int) + 1
		default:
			summary["unknown"] = summary["unknown"].(int) + 1
		}
		if p.LastProbeAt > summary["last_probe_at"].(int64) {
			summary["last_probe_at"] = p.LastProbeAt
		}
	}

	sites := []map[string]any{}
	for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
		sites = append(sites, map[string]any{
			"site":        site,
			"label":       auth.SiteLabel(site),
			"model_count": s.cat.SiteCount(site),
			"accounts":    s.pool.AccountsForSiteCount(site),
		})
	}

	rows := make([]map[string]any, 0, len(models))
	for _, m := range models {
		blocked, _ := s.config().ModelDisabled(m.ID)

		// 该模型当前可调度的账号数（已计入冷却、模型级冷却与在途）
		available := 0
		for _, a := range s.pool.Accounts() {
			if a.HealthyForModel(now, m.ID) {
				available++
			}
		}

		siteInfo := map[string]any{}
		for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
			entry, ok := s.cat.Entry(site, m.ID)
			if !ok {
				continue
			}
			label := s.cat.DisplayMultiplier(site, m.ID)
			info := map[string]any{
				"known":            entry.HasMultiplier || s.cat.KnownFree(site, m.ID) || s.cat.KnownPaid(site, m.ID),
				"multiplier_label": label,
				"verdict":          verdictOf(s.cat, site, m.ID),
				"promo_label":      entry.PromoLabel,
				"promo_until":      entry.PromoUntil,
				"credits":          entry.Credits,
			}
			if entry.HasMultiplier {
				info["multiplier"] = entry.Multiplier
			} else {
				info["multiplier"] = nil
			}
			if p, ok := s.cat.ProbeOf(site, m.ID); ok {
				info["probe"] = map[string]any{
					"last_probe_at": p.LastProbeAt,
					"cost":          p.Cost,
					"credit":        p.Credit,
					"tokens":        p.Tokens,
					"detail":        p.Detail,
				}
			} else {
				info["probe"] = nil
			}
			siteInfo[site] = info
		}
		// 两站点都没有该模型时补空，保证前端字段齐全
		for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
			if _, ok := siteInfo[site]; !ok {
				siteInfo[site] = map[string]any{
					"known": false, "multiplier": nil, "multiplier_label": "-",
					"verdict": "", "promo_label": "", "promo_until": "", "probe": nil,
				}
			}
		}

		row := map[string]any{
			"id":                 m.ID,
			"name":               m.Name,
			"description":        m.Description,
			"context_length":     m.ContextLength(),
			"max_output_tokens":  m.MaxOutputTokens,
			"max_allowed_size":   m.MaxAllowedSize,
			"available_accounts": available,
			"requests":           s.metrics.RequestsFor(m.ID),
			"blocked":            blocked,
			"sites":              siteInfo,
			"supports_images":    m.SupportsImages,
			"supports_tool_call": m.SupportsToolCall,
			"is_default":         m.IsDefault,
			"tags":               m.Tags,
			"vendor":             m.Vendor,
		}
		// 运行指标：没有请求记录时**不写 0**，让前端能区分「没跑过」与「跑了但全失败」。
		if st, ok := statsByModel[m.ID]; ok {
			row["requests"] = st.Requests
			row["success"] = st.Success
			row["failed"] = st.Failures
			row["tokens"] = st.TokensTotal
			row["avg_ttft_ms"] = st.AvgTTFTMs
			row["avg_total_ms"] = st.AvgTotalMs
			row["last_request_at"] = st.LastRequestAt
			row["last_status"] = st.LastStatus
		} else {
			row["success"] = int64(0)
			row["failed"] = int64(0)
			row["tokens"] = int64(0)
			row["avg_ttft_ms"] = nil
			row["avg_total_ms"] = nil
			row["last_request_at"] = int64(0)
			row["last_status"] = ""
		}
		if len(m.SupportedEfforts) > 0 {
			row["reasoning_supported_efforts"] = m.SupportedEfforts
		}
		if m.ReasoningEffort != "" {
			row["reasoning_default_effort"] = m.ReasoningEffort
		}
		if m.SupportsReasoning {
			row["reasoning_supported"] = true
		}
		if m.OnlyReasoning {
			row["reasoning_only"] = true
		}
		if m.CanDisableThinking {
			row["reasoning_can_disable"] = true
		}
		if m.PromoExpired {
			row["promo_expired"] = true
		}
		rows = append(rows, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"source":                 source,
		"fetched_at":             fetchedAt,
		"model_count":            len(models),
		"sites":                  sites,
		"models":                 rows,
		"probe_summary":          summary,
		"probe_interval_seconds": int(catalog.ProbeInterval.Seconds()),
	})
}

// verdictOf 返回某 (站点, 模型) 的价格结论。
func verdictOf(c *catalog.Catalog, site, model string) string {
	if p, ok := c.ProbeOf(site, model); ok {
		return p.Verdict
	}
	return ""
}

// handleModelsRefresh 手动刷新模型目录。
func (s *Server) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	ctx, cancel := contextWithTimeout(r, 90*time.Second)
	defer cancel()

	if err := s.cat.RefreshOnce(ctx); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"code": 502, "message": "刷新模型目录失败: " + err.Error()},
		})
		return
	}
	source, _ := s.cat.Source()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"source":      source,
		"model_count": len(s.cat.Merged()),
	})
}

// handleModelsProbe 手动触发价格探测。支持单条与批量两种请求体。
//
// 注意：探测会真实消耗账号额度（发一次极短对话并比对余额差分）。
func (s *Server) handleModelsProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Site   string `json:"site"`
		Model  string `json:"model"`
		Models []struct {
			Site  string `json:"site"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}

	targets := req.Models
	if len(targets) == 0 && req.Model != "" {
		site := req.Site
		if site == "" {
			site = auth.SiteCN
		}
		targets = append(targets, struct {
			Site  string `json:"site"`
			Model string `json:"model"`
		}{site, req.Model})
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody("请提供 site/model 或 models 数组"))
		return
	}
	// 限量：探测消耗额度，一次最多 10 个，避免误点把额度打光
	if len(targets) > 10 {
		targets = targets[:10]
	}

	// 组装成 catalog 需要的形状
	list := make([]struct{ Site, Model string }, 0, len(targets))
	for _, t := range targets {
		site := t.Site
		if site != auth.SiteINTL {
			site = auth.SiteCN
		}
		model := strings.TrimSpace(t.Model)
		if model == "" {
			continue
		}
		list = append(list, struct{ Site, Model string }{site, model})
	}

	ctx, cancel := contextWithTimeout(r, 5*time.Minute)
	defer cancel()

	results := s.cat.ProbeModels(ctx, list)
	okCount := 0
	for _, row := range results {
		if errFlag, _ := row["error"].(bool); !errFlag {
			okCount++
		}
	}
	s.logf("[价格探测] 手动触发 %d 个目标，成功 %d", len(list), okCount)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}
