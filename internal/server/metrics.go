package server

import (
	"net/http"
	"time"

	"workbuddy-gateway/internal/auth"
)

// handlePanelMetrics 汇总监控页需要的三块数据：
// 账号运行状态（含冷却/熔断/在途/连败）、模型统计（请求/失败/首字/耗时/token）、以及全局汇总。
func (s *Server) handlePanelMetrics(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	states := s.pool.Snapshot()

	accounts := make([]map[string]any, 0, len(states))
	for i := range states {
		st := states[i]
		acc, err := s.pool.Find(st.ID)
		state := "available"
		switch {
		case st.Disabled:
			state = "disabled"
		case st.CooldownUntil > now.Unix() || st.BreakerUntil > now.Unix():
			state = "cooldown"
		}

		var credits any
		if st.Quota != nil {
			credits = st.Quota.Remaining
		}

		row := map[string]any{
			"id":                st.ID,
			"site":              st.Site,
			"site_label":        st.SiteLabel,
			"nickname":          st.Nickname,
			"uid":               st.UID,
			"state":             state,
			"cooldown_kind":     st.CooldownKind,
			"cooldown_until":    st.CooldownUntil,
			"cooldown_reason":   st.CooldownReason,
			"breaker_until":     st.BreakerUntil,
			"fails":             st.Fails,
			"in_flight":         st.InFlight,
			"success_count":     st.SuccessCount,
			"failure_count":     st.FailureCount,
			"credits_remaining": credits,
			"last_used_at":      st.LastUsedAt,
			"last_error":        st.LastError,
			"max_in_flight":     0,
		}
		if err == nil && acc != nil {
			row["max_in_flight"] = s.pool.InFlightLimit(acc)
			row["base_url"] = acc.Profile().Base
			// 模型级冷却台账（6004 / 11102）
			mc := acc.ModelCooldowns()
			if len(mc) > 0 {
				models := make([]map[string]any, 0, len(mc))
				for m, until := range mc {
					if until.After(now) {
						models = append(models, map[string]any{"model": m, "until": until.Unix()})
					}
				}
				if len(models) > 0 {
					row["model_cooldowns"] = models
				}
			}
		}
		accounts = append(accounts, row)
	}

	// 模型统计：补上「当前可调度该模型的账号数」
	models := s.metrics.Snapshot()
	modelRows := make([]map[string]any, 0, len(models))
	for _, m := range models {
		available := 0
		for _, a := range s.pool.Accounts() {
			if a.HealthyForModel(now, m.Model) {
				available++
			}
		}
		modelRows = append(modelRows, map[string]any{
			"model":              m.Model,
			"requests":           m.Requests,
			"failures":           m.Failures,
			"avg_ttft_ms":        m.AvgTTFTMs,
			"avg_total_ms":       m.AvgTotalMs,
			"tokens_total":       m.TokensTotal,
			"available_accounts": available,
			"last_request_at":    m.LastRequestAt,
		})
	}

	enabled, ttl, _ := s.sticky.Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds": int64(time.Since(s.start).Seconds()),
		"summary":        s.pool.Summary(),
		"accounts":       accounts,
		"models":         modelRows,
		"sticky": map[string]any{
			"enabled":     enabled,
			"bindings":    s.sticky.Count(),
			"ttl_seconds": int(ttl.Seconds()),
		},
		"totals": s.metrics.Totals(),
		"governance": map[string]any{
			"soft_rate_seconds":  int(s.pool.Government().SoftRate.Seconds()),
			"max_in_flight":      s.pool.Government().MaxInFlight,
			"breaker_threshold":  s.pool.Government().BreakerThreshold,
			"degrade_threshold":  s.pool.Government().DegradeThreshold,
			"selection_strategy": s.effectiveStrategy(),
		},
		"sites": []map[string]any{
			{"site": auth.SiteCN, "label": auth.SiteLabel(auth.SiteCN), "accounts": countSite(accounts, auth.SiteCN)},
			{"site": auth.SiteINTL, "label": auth.SiteLabel(auth.SiteINTL), "accounts": countSite(accounts, auth.SiteINTL)},
		},
		"last_scan_unix": s.pool.LastScan().Unix(),
	})
}

// effectiveStrategy 返回实际的选号策略名（空值即默认加权）。
func (s *Server) effectiveStrategy() string {
	if s.pool.Strategy == "" {
		return "weighted"
	}
	return s.pool.Strategy
}

func countSite(rows []map[string]any, site string) int {
	n := 0
	for _, row := range rows {
		if row["site"] == site {
			n++
		}
	}
	return n
}
