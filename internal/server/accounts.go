package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"workbuddy-gateway/internal/accountmeta"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/scheduler"
)

// -----------------------------------------------------------------------------
// 单账号与批量操作
//
// 路径参数 {id} 是凭据文件名（AccountState.ID），不是 uid —— 部分凭据没有 uid，
// 而文件名恒唯一。id 经 pool.ValidAccountID 校验（禁路径成分），再在池内按名字匹配，
// 双保险防路径穿越。
// -----------------------------------------------------------------------------

// accountFromPath 从路径参数解析账号，失败时已写回错误响应。
func (s *Server) accountFromPath(w http.ResponseWriter, r *http.Request) (*pool.Account, bool) {
	id := r.PathValue("id")
	acc, err := s.pool.Find(id)
	if err != nil {
		status := http.StatusNotFound
		if errors.Is(err, pool.ErrAccountNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{
			"error": map[string]any{"code": status, "message": err.Error()},
		})
		return nil, false
	}
	return acc, true
}

// handleAccountQuota 刷新单账号额度。
func (s *Server) handleAccountQuota(w http.ResponseWriter, r *http.Request) {
	acc, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()

	if err := s.pool.RefreshQuota(ctx, acc); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"code": 502, "message": "额度查询失败: " + err.Error()},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": s.pool.StateOf(acc)})
}

// handleAccountDisable 手工禁用账号。
func (s *Server) handleAccountDisable(w http.ResponseWriter, r *http.Request) {
	s.setAccountDisabled(w, r, true)
}

// handleAccountEnable 启用（复活）账号。
func (s *Server) handleAccountEnable(w http.ResponseWriter, r *http.Request) {
	s.setAccountDisabled(w, r, false)
}

// handleAccountPause 暂停选号：只把它从派发里摘出来，维护任务照常跑。
func (s *Server) handleAccountPause(w http.ResponseWriter, r *http.Request) {
	s.setAccountPaused(w, r, true)
}

// handleAccountResume 恢复选号。
func (s *Server) handleAccountResume(w http.ResponseWriter, r *http.Request) {
	s.setAccountPaused(w, r, false)
}

// setAccountPaused 切换「暂停选号」并返回最新账号状态。
//
// 为什么不复用 /disable：两者只关掉不同的东西 —— 禁用是完全不用，暂停只是不派发。
// 用同一个端点会逼着用户二选一，而"这个号先别发请求、但签到保活别停"恰恰是
// 让位防风控时最需要的那个中间态。
func (s *Server) setAccountPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	acc, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	if err := s.pool.SetPaused(acc, paused); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"code": 500, "message": err.Error()},
		})
		return
	}
	action := "暂停选号"
	if !paused {
		action = "恢复选号"
	}
	s.logf("[账号] %s：账号 %s（%s）", action, acc.Cred.AccountID(), acc.SiteLabel())
	writeJSON(w, http.StatusOK, map[string]any{"account": s.pool.StateOf(acc)})
}

// handleAccountRevive 人工复活：清禁用 + 清全部失败状态（冷却 / 熔断 / 连败降权）。
//
// 与 `/enable` 分开是刻意的：`/enable` 只改「禁用」这一个位（用户上次禁用错了），
// 而 `/revive` 是运维口径的无条件恢复 —— 用在「账号在别处被证明可用了」的场景。
// 合成一个按钮会让「只想取消禁用」的用户意外把熔断计数也抹掉，
// 从而**掩盖真实的持续故障**（熔断本来是在提醒你上游一直失败）。
func (s *Server) handleAccountRevive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	acc, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	if err := s.pool.Revive(acc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"code": 500, "message": err.Error()},
		})
		return
	}
	s.logf("[账号] 已复活账号 %s（清除禁用/冷却/熔断/降权；余额类硬冷却需签到或额度恢复）",
		acc.Cred.AccountID())
	writeJSON(w, http.StatusOK, map[string]any{"account": s.pool.StateOf(acc)})
}

func (s *Server) setAccountDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	acc, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	reason := "已通过面板手工禁用"
	if disabled {
		var req struct {
			Reason string `json:"reason"`
		}
		_ = decodeBody(r, &req)
		if req.Reason != "" {
			reason = req.Reason
		}
	}
	if err := s.pool.SetDisabled(acc, disabled, reason); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"code": 500, "message": err.Error()},
		})
		return
	}
	action := "禁用"
	if !disabled {
		action = "启用"
	}
	s.logf("[账号] %s 账号 %s（%s）", action, acc.Cred.AccountID(), acc.SiteLabel())
	writeJSON(w, http.StatusOK, map[string]any{"account": s.pool.StateOf(acc)})
}

// handleAccountRemove 从池中移除账号；可选同时删除凭据文件。
func (s *Server) handleAccountRemove(w http.ResponseWriter, r *http.Request) {
	acc, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		DeleteFile bool `json:"delete_file"`
	}
	_ = decodeBody(r, &req)

	id := acc.Cred.AccountID()

	// 删除凭据文件是**不可逆**的：删掉一份其实还能用的凭据，用户只能重新登录。
	// 所以删之前先做一次只读校验（切片 18）——「无法判定」时保守保留。
	// 只在真要删文件时校验：仅从池中移除（归档）是可恢复的，不必多花一次请求。
	if req.DeleteFile {
		ctx, cancel := contextWithTimeout(r, pool.CredentialVerifyTimeout+2*time.Second)
		keep, note := s.pool.CredentialStillUsableForDelete(ctx, acc, acc.Profile())
		cancel()
		if keep {
			s.logf("[账号] 未删除账号 %s 的凭据文件：%s", id, note)
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": map[string]any{
					"code": 409,
					"message": "该凭据仍可用，未删除凭据文件（避免销毁还能用的登录态）：" + note +
						"。如确实要删，请先在别处确认它已失效，或改用「仅从池中移除」（可恢复）",
				},
			})
			return
		}
		s.logf("[账号] 删除前校验：%s", note)
	}

	archived, err := s.pool.Remove(acc, req.DeleteFile)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"code": 500, "message": err.Error()},
		})
		return
	}
	payload := map[string]any{"ok": true, "removed": id}
	if req.DeleteFile {
		s.logf("[账号] 已移除账号 %s 并删除凭据文件", id)
		payload["delete_file"] = true
	} else {
		s.logf("[账号] 已移除账号 %s，凭据已归档到 %s（可持续恢复）", id, archived)
		payload["archived"] = archived
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleQuotaAll 刷新全部账号额度。
func (s *Server) handleQuotaAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	okCount, failed := s.pool.RefreshAllQuotas(ctx)
	s.logf("[额度] 批量刷新完成：成功 %d，失败 %d", okCount, failed)
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": okCount, "failed": failed})
}

// -----------------------------------------------------------------------------
// 账号备注与显示字段（批次 4，对齐 wb-switch 的 account-info-dialog）
// -----------------------------------------------------------------------------

// modelCooldownView 是「账号 × 模型」级冷却的一条记录（429/6004 模型限流）。
//
// Until 用 Unix 秒：面板要按本地时钟做倒计时与「已过期即不显示」，
// 传绝对时刻而不是剩余秒数 —— 剩余秒数在客户端会随时间失真。
type modelCooldownView struct {
	Model string `json:"model"`
	Until int64  `json:"until"`
	// Source 标注观测来源：gateway=网关代理请求时看到的 429；
	// client=WorkBuddy 客户端日志里的限流（客户端直连官方，网关看不到）。
	Source string `json:"source,omitempty"`
}

// panelAccountView 是账号列表的对外视图：账号池状态 + 本地展示元数据。
//
// 用**内嵌**而不是往 pool.AccountState 里加字段：备注与显示字段只在面板层
// 有意义，pool 包不该知道「显示字段」这种展示概念。内嵌结构体的字段会被
// encoding/json 提升到外层，因此契约与原来的 AccountState 逐字兼容（只做加法）。
type panelAccountView struct {
	pool.AccountState
	// Note 是本地备注（空串 = 未填写）。
	Note string `json:"note"`
	// DisplayField 是卡片显示字段：nickname / phone / note；空串 = 未设置（按昵称显示）。
	DisplayField string `json:"display_field"`
	// AutoCheckinEnabled 表示该账号是否参与自动签到（false = 在排除名单里）。
	// 单账号手动签到不受它影响。
	AutoCheckinEnabled bool `json:"auto_checkin_enabled"`
	// EnterpriseName 预留字段：本项目凭据模型里没有企业名数据源，
	// 有值才下发（omitempty），没有就不出现 —— 对齐差距清单的「没有就不带」。
	EnterpriseName string `json:"enterprise_name,omitempty"`
	// ModelCooldowns 是该账号当前仍在冷却的模型（按恢复时间升序）。
	// 没有冷却时不下发（omitempty）—— 卡片据此决定是否渲染「模型限额」提醒。
	ModelCooldowns []modelCooldownView `json:"model_cooldowns,omitempty"`
}

// panelAccounts 组装账号列表视图（池状态 + 本地元数据 + 自动签到开关 + 模型级冷却）。
func (s *Server) panelAccounts() []panelAccountView {
	states := s.pool.Snapshot()
	cfg := s.config()

	// 模型级冷却不在 AccountState 快照里（它是 *Account 上的台账），
	// 这里按账号 ID 单独收集一次；已过期的条目由面板口径过滤掉。
	now := time.Now()
	cooldowns := map[string][]modelCooldownView{}
	for _, a := range s.pool.Accounts() {
		mc := a.ModelCooldowns()
		if len(mc) == 0 {
			continue
		}
		list := make([]modelCooldownView, 0, len(mc))
		for model, until := range mc {
			if until.After(now) {
				list = append(list, modelCooldownView{Model: model, Until: until.Unix(), Source: "gateway"})
			}
		}
		if len(list) == 0 {
			continue
		}
		cooldowns[a.Cred.AccountID()] = list
	}

	// 客户端日志观测到的限流（internal/clientlimits）：
	// WorkBuddy 桌面客户端直连官方，它的 429 不会经过网关，只有客户端日志知道。
	// 这里按 uid（昵称唯一时兜底）归因到池内账号后合并展示。
	// **只用于展示，不进选号层** —— 归因来自日志文本，误判不该影响调度决策。
	if s.clientLimits != nil {
		byUID := map[string]string{}
		byName := map[string][]string{}
		for _, a := range s.pool.Accounts() {
			id := a.Cred.AccountID()
			if uid := strings.TrimSpace(a.Cred.UID); uid != "" {
				byUID[uid] = id
			}
			if name := strings.TrimSpace(a.Cred.Nickname); name != "" {
				byName[name] = append(byName[name], id)
			}
		}
		for _, limit := range s.clientLimits.Limits(now) {
			id := byUID[limit.UID]
			if id == "" && limit.Nickname != "" && len(byName[limit.Nickname]) == 1 {
				id = byName[limit.Nickname][0]
			}
			if id == "" {
				continue // 归因不到账号：不显示（宁可少显示，不显示错账号）
			}
			cooldowns[id] = mergeModelCooldown(cooldowns[id], modelCooldownView{
				Model:  limit.Model,
				Until:  limit.Until.Unix(),
				Source: "client",
			})
		}
	}

	out := make([]panelAccountView, 0, len(states))
	for _, st := range states {
		// s.accountMeta 可能为 nil（未接线）：Get/All 都是 nil 安全的。
		entry := s.accountMeta.Get(st.ID)
		list := cooldowns[st.ID]
		sortModelCooldowns(list)
		out = append(out, panelAccountView{
			AccountState:       st,
			Note:               entry.Note,
			DisplayField:       entry.DisplayField,
			AutoCheckinEnabled: !cfg.IsCheckinExcluded(st.ID),
			ModelCooldowns:     list,
		})
	}
	return out
}

// mergeModelCooldown 合并同模型的冷却条目：保留恢复时刻**更晚**的那条。
//
// 客户端日志给的是官方原文的重置时刻（如「明天 13:49」），而网关自己的冷却
// 有 2 小时封顶 —— 同一模型两边都有时，展示更晚的那个才符合「什么时候真能用」。
func mergeModelCooldown(list []modelCooldownView, item modelCooldownView) []modelCooldownView {
	for i, existing := range list {
		if existing.Model != item.Model {
			continue
		}
		if item.Until > existing.Until {
			list[i] = item
		}
		return list
	}
	return append(list, item)
}

// sortModelCooldowns 按恢复时间升序（同刻按模型名），保证展示顺序稳定。
func sortModelCooldowns(list []modelCooldownView) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Until != list[j].Until {
			return list[i].Until < list[j].Until
		}
		return list[i].Model < list[j].Model
	})
}

// handleAccountsMeta 返回全部账号的备注与显示字段
// （GET /panel/api/accounts/meta）。
func (s *Server) handleAccountsMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": s.accountMeta.All(),
		// 前端输入框的 maxLength 与计数直接用它，避免两边各写一份 24 而漂移。
		"note_max_length": accountmeta.MaxNoteRunes,
	})
}

// handleAccountMetaUpdate 更新某账号的备注 / 显示字段
// （PATCH /panel/api/accounts/{id}/meta，body {note?, display_field?}）。
//
// 两个字段都是可选的：字段缺席 = 不改；显式空串 = 清除。
// 校验失败（备注超 24 字符 / 显示字段非法）返回 400 且不改动任何状态。
func (s *Server) handleAccountMetaUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 PATCH"))
		return
	}
	acc, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Note         *string `json:"note"`
		DisplayField *string `json:"display_field"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	entry, err := s.accountMeta.Update(acc.Cred.AccountID(), req.Note, req.DisplayField)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	s.logf("[账号] 已更新账号信息：%s（备注 %d 字，显示字段 %q）",
		acc.Cred.AccountID(), utf8.RuneCountInString(entry.Note), entry.DisplayField)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "meta": entry})
}

// handleAccountsCheckinAll 批量「刷新积分并签到」
// （POST /panel/api/accounts/checkin_all）。
//
// 语义（对齐 wb-switch 的「刷新全部账号积分并签到」）：
//   - 遵守签到时间段：窗口外整轮跳过签到（outside_window=true），
//     但**积分照刷** —— 按钮承诺的是「刷新积分并签到」，窗口只约束签到那半，
//     面板据此提示「当前不在签到时间段，本次仅刷新积分」；
//   - 排除名单（config.schedule.checkin_excluded_accounts）内的账号不参与签到，
//     由 scheduler 统一处置；单账号手动签到不经这里，因此不受名单限制；
//   - 返回结构化计数，面板按计数拼 toast（成功 / 已签 / 失败 / 跳过）。
func (s *Server) handleAccountsCheckinAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	ctx, cancel := contextWithTimeout(r, 3*time.Minute)
	defer cancel()

	if !s.config().InCheckinWindow(time.Now()) {
		refreshed, failed := s.pool.RefreshAllQuotas(ctx)
		s.logf("[签到] 批量刷新积分并签到：当前不在签到时间段，已跳过签到（积分刷新 %d 成功 / %d 失败）",
			refreshed, failed)
		writeJSON(w, http.StatusOK, scheduler.CheckinSummary{OutsideWindow: true})
		return
	}

	sum, err := s.sched.RunCheckinNowSummary(ctx)
	if err != nil {
		writeActionErrorExtra(w, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}
