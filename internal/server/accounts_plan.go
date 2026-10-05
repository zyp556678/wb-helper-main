package server

import (
	"net/http"
	"time"

	"workbuddy-gateway/internal/upstream"
)

// handleAccountPlan 完整识别单账号套餐（POST /panel/api/accounts/{id}/plan）。
//
// 与「刷新额度」分开：额度刷新给的是**摘要级**套餐名（只有订阅编码，没有有效期），
// 而这里会再查一次分页权益列表，回答的是「这条订阅现在还有效吗、是哪一档」。
// 多打一次上游请求，所以只做成按需触发（面板上一个按钮），不挂在周期刷新上。
//
// 识别不出来时返回 200 + plan="待确认" + 依据，而不是 4xx：
// 「查不清是哪一档」是**上游数据的问题**，不是请求失败，把原因展示出来才有用。
func (s *Server) handleAccountPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	acc, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, 40*time.Second)
	defer cancel()

	view := acc.View()
	prof := acc.Profile()
	plan, detail, err := s.client.IdentifyPlan(ctx, view, prof)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":     false,
			"plan":   upstream.PlanUnknown,
			"detail": "识别失败: " + err.Error(),
		})
		return
	}
	if plan != upstream.PlanUnknown {
		s.pool.SetPlan(acc, plan)
	}
	s.logf("[套餐] 账号=%s → %s（%s）", acc.ID(), plan, detail)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "plan": plan, "detail": detail,
		"account": s.pool.StateOf(acc),
	})
}
