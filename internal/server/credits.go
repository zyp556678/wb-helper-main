package server

import (
	"net/http"
	"time"

	"workbuddy-gateway/internal/creditwatch"
	"workbuddy-gateway/internal/upstream"
)

// 本文件是「积分资源包」的面板接口（积分统计页的「积分明细」与「剩余积分」用）。
//
// 与 /panel/api/stats/official 的分工：
//   - official 回答「**花了多少**」——上游计费账本里的请求行，按天/模型/账号拆。
//   - credits 回答「**还剩多少、是什么包**」——当前资源包明细（含到期时间）。
//
// 两者是账本的两端，缺一个都读不出「消耗是否正常」：只有消耗看不出余额够不够，
// 只有余额看不出消耗速率。
//
// 为什么不复用监控页的额度：那里的额度是池子在调度时顺手缓存的**一个合计数**
// （quotaRemaining），只用于选号与「余额耗尽」判定。面板要逐包看明细，
// 合计数拆不回来。

// panelCreditAccount 是单账号的积分资源结果。
type panelCreditAccount struct {
	ID        string `json:"id"`
	Nickname  string `json:"nickname"`
	Site      string `json:"site"`
	SiteLabel string `json:"site_label"`
	// OK 为 false 时 Error 一定有值，Resources 为空数组。
	OK        bool                      `json:"ok"`
	Error     string                    `json:"error,omitempty"`
	Resources []upstream.CreditResource `json:"resources"`
}

// handlePanelCredits 返回各账号的积分资源包明细。
//
// GET /panel/api/credits
func (s *Server) handlePanelCredits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	// 必须是非 nil 空切片：nil 会被序列化成 null，前端按数组取值会直接抛错。
	accounts := make([]panelCreditAccount, 0)

	var currentRemaining, currentCapacity float64
	okCount, failCount := 0, 0

	for _, a := range s.pool.Accounts() {
		if a.IsDisabled() {
			continue
		}
		view := a.View()
		if view == nil || view.AccessToken == "" {
			continue
		}
		rec := panelCreditAccount{
			ID: a.Cred.AccountID(), Nickname: a.Cred.Nickname,
			Site: a.Site(), SiteLabel: a.SiteLabel(),
			Resources: []upstream.CreditResource{},
		}

		expiry, err := s.client.FetchCreditResources(ctx, view, a.Profile())
		if err != nil {
			rec.OK = false
			rec.Error = err.Error()
			failCount++
			// 取不到明细时退回池子里缓存的额度：宁可给一个稍旧的总数，
			// 也好过让「剩余积分」整格变成 0 —— 那会被读成「余额花光了」。
			if st := s.pool.StateOf(a); st.Quota != nil {
				currentRemaining += st.Quota.Remaining
				currentCapacity += st.Quota.Total
			}
			accounts = append(accounts, rec)
			continue
		}

		okCount++
		rec.OK = true
		rec.Resources = expiry.Resources
		var accountRemaining, accountCapacity float64
		for _, res := range expiry.Resources {
			currentRemaining += res.Remaining
			currentCapacity += res.Total
			accountRemaining += res.Remaining
			accountCapacity += res.Total
		}
		// 本地观察台账：记下此刻该账号的余额。官方用量接口挂掉时，
		// 页面用「连续快照余额下降」回退估算消耗（对齐 wb-switch 的口径）。
		s.creditWatch.RecordSnapshot(rec.ID, rec.Nickname, a.Site(), accountCapacity, accountRemaining)
		// 顺手把到期快照写给选号层：这次请求本来就已经把 paid/free 明细取回来了，
		// 不写的话选号层的「快过期优先」就得另开一轮请求。
		s.pool.SetExpiringSnapshotOf(a, expiry.ExpiringRemaining, expiry.SoonestExpireAt)
		accounts = append(accounts, rec)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"fetched_at":   time.Now().Unix(),
		"collected_at": time.Now().UnixMilli(),
		"summary": map[string]any{
			"current_remaining": currentRemaining,
			"current_capacity":  currentCapacity,
		},
		"accounts":   accounts,
		"ok_count":   okCount,
		"fail_count": failCount,
	})
}

// handleCreditStatistics 返回本地观察口径的积分统计
// （GET /panel/api/credits/stats，字段对齐 wb-switch 的 get_credit_statistics）。
//
// 与 /panel/api/credits 的分工：credits 打上游取实时明细（慢、会失败），
// stats 读本地快照台账（纯本地、永远可用）。官方用量接口不可用时，
// 积分统计页用这里的 summary/daily 回退显示 —— 「测不到」不等于「没发生」。
func (s *Server) handleCreditStatistics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	accounts := make([]creditwatch.AccountInfo, 0, s.pool.Len())
	for _, a := range s.pool.Accounts() {
		accounts = append(accounts, creditwatch.AccountInfo{
			ID:   a.Cred.AccountID(),
			Name: a.Cred.Nickname,
			Site: a.Site(),
		})
	}
	writeJSON(w, http.StatusOK, s.creditWatch.Statistics(accounts, time.Now()))
}
