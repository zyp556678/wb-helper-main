package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"workbuddy-gateway/internal/eventlog"
	"workbuddy-gateway/internal/tasks"
)

// handlePanelTasks 返回任务中心数据。
//
// 默认返回上次扫描的缓存（扫描会打上游，不适合每次进页面都触发）；
// 带 ?refresh=1 时强制重新扫描。
func (s *Server) handlePanelTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	if r.URL.Query().Get("refresh") == "1" || s.tasks.LastScan() == nil {
		ctx, cancel := contextWithTimeout(r, 90*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, s.tasks.Scan(ctx))
		return
	}
	writeJSON(w, http.StatusOK, s.tasks.LastScan())
}

// handleTasksScan 强制重新扫描全部账号的任务状态。
func (s *Server) handleTasksScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	ctx, cancel := contextWithTimeout(r, 90*time.Second)
	defer cancel()
	res := s.tasks.Scan(ctx)
	s.logf("[任务] 扫描完成：账号 %d 个，待办 %d 项（可领 %d / 可自动 %d / 可报名 %d / 需客户端 %d）",
		len(res.Accounts), res.Totals.Tasks, res.Totals.Claimable,
		res.Totals.Auto, res.Totals.Acceptable, res.Totals.Manual)
	writeJSON(w, http.StatusOK, res)
}

// handleTasksRun 触发任务队列执行（异步）。
func (s *Server) handleTasksRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Accounts  []string `json:"accounts"`
		TaskCodes []string `json:"task_codes"`
		Actions   []string `json:"actions"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	for _, a := range req.Actions {
		switch a {
		case "accept", "auto", "claim":
		default:
			writeJSON(w, http.StatusBadRequest, errBody("未知动作: "+a+"（可选 accept / auto / claim）"))
			return
		}
	}

	n := s.tasks.Run(r.Context(), tasks.RunOptions{
		UIDs: req.Accounts, TaskCodes: req.TaskCodes, Actions: req.Actions,
	})
	if n == -1 {
		// 409 + 动作型信封（ok/detail）：这不是故障，是「当前状态不允许」。
		// 用 errBody 的话前端两个分支都读不到（它查 error.message，且没有 ok 字段），
		// 只能落进 catch，把「队列在跑」讲成一句无从下手的失败。
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "planned": 0, "accepted": false, "running": true,
			"detail": "已有任务队列在执行中，等它跑完再点（队列进度见上方执行队列）",
		})
		return
	}
	if n == 0 {
		// 0 表示没有可用账号；此时队列已置为 done 并带 note，交给面板照常展示。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "planned": 0, "accepted": true,
			"detail": "没有可执行的账号或待办任务，详情见队列状态",
		})
		return
	}
	// n == -2：已受理，项数要等后台扫描完才知道。
	// 刻意**不再阻塞在这里等扫描结果** —— 扫描要打上游（实测 2.5s，最坏几十秒），
	// 阻塞期间界面没有任何反馈，且前端拿到响应时小队列可能已经跑完，
	// 导致整个执行过程一次都看不到。项数由面板轮询 /tasks/queue 得到。
	if s.events != nil {
		s.events.Info(eventlog.ChannelTask, "task_queue_start", "任务队列已启动（正在扫描待办）",
			map[string]any{"planned": -1})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "planned": -1, "accepted": true})
}

// handleTasksQueue 返回队列执行状态（面板轮询用）。
func (s *Server) handleTasksQueue(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.tasks.Queue())
}

// handleTasksOverview 返回任务摘要（供配置页/监控页展示与定时任务判断）。
func (s *Server) handleTasksOverview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.tasks.Overview())
}

// handleTasksReport 手动补一条对话活跃上报。
//
// 它的用途有两个，都很实在：一是「连登」和「领养前置」就靠这个事件点亮，
// 用户不必真的去发一轮对话；二是上游对上报的形状（字段、uid）有隐性要求，
// 出问题时用这个接口单发一条，比看日志猜快得多。
func (s *Server) handleTasksReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Account string `json:"account"`
		Model   string `json:"model"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	detail, err := s.tasks.ReportActivity(ctx, req.Account, req.Model)
	if err != nil {
		writeActionError(w, err)
		return
	}
	s.logf("[任务] 手动活跃上报：账号=%s 模型=%s → %s", req.Account, req.Model, detail)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": detail})
}

// handleGrowthAction 执行一次成长活动动作（旅行 / 连登兑换 / 抽奖）。
func (s *Server) handleGrowthAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Account string `json:"account"`
		Action  string `json:"action"`
		Tier    string `json:"tier"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	if strings.TrimSpace(req.Account) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少 account（账号 ID）"))
		return
	}

	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	var (
		detail string
		err    error
	)
	switch req.Action {
	case "travel_depart":
		detail, err = s.tasks.TravelAction(ctx, req.Account, "depart")
	case "travel_claim":
		detail, err = s.tasks.TravelAction(ctx, req.Account, "claim")
	case "redeem":
		detail, err = s.tasks.RedeemStreak(ctx, req.Account, req.Tier)
	case "lottery_draw":
		detail, err = s.tasks.DrawLottery(ctx, req.Account)
	default:
		writeJSON(w, http.StatusBadRequest, errBody("未知动作: "+req.Action+
			"（可选 travel_depart / travel_claim / redeem / lottery_draw）"))
		return
	}
	if err != nil {
		writeActionError(w, err)
		return
	}
	s.logf("[成长活动] 账号=%s 动作=%s → %s", req.Account, req.Action, detail)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": req.Action, "detail": detail})
}

// handleTasksStreakBonus 连登管家（POST /panel/api/tasks/streak-bonus）。
//
// body 可选 {account_id}：给了就只做那一个账号（面板单号按钮），否则做全部可用账号。
// 语义是**幂等**的：已领的档位、无次数的抽奖、无卡的补签都会自动跳过，
// 所以「多点几次」不会重复发奖。
func (s *Server) handleTasksStreakBonus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	ctx, cancel := contextWithTimeout(r, 120*time.Second)
	defer cancel()
	detail, err := s.tasks.RunStreakBonus(ctx, req.AccountID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "detail": err.Error(),
		})
		return
	}
	s.logf("[连登] %s", detail)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "detail": detail, "accounts": s.tasks.StreakBonusResults(),
	})
}

// handleAccountTaskAuto 对单个账号执行单个任务的自动动作
// （POST /panel/api/accounts/{id}/tasks/auto，body {task_code}）。
//
// 与队列执行的区别：这是**同步**返回逐项结果的动作，面板弹窗直接展示
// （做了什么、进度从哪到哪、领了多少），不需要再去轮询队列状态。
func (s *Server) handleAccountTaskAuto(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	id := r.PathValue("id")
	var req struct {
		TaskCode string `json:"task_code"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	if strings.TrimSpace(req.TaskCode) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少 task_code"))
		return
	}
	// 单项动作可能含真实对话（专家/模型/技能），给足时间；前端另有自己的超时。
	ctx, cancel := contextWithTimeout(r, 5*time.Minute)
	defer cancel()

	res, err := s.tasks.RunAccountTask(ctx, id, req.TaskCode)
	if err != nil {
		writeAccountTaskError(w, err)
		return
	}
	s.logf("[任务] 单任务动作 账号=%s 任务=%s → %s", id, req.TaskCode, res.Message)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": res.Message, "result": res,
		"progress_before": res.ProgressBefore, "progress_after": res.ProgressAfter,
		"claimable": res.Claimable, "claimed": res.Claimed,
		"credit": res.Credit, "energy": res.Energy, "attempt": res.Attempt,
	})
}

// handleAccountTaskAutoAll 对该账号一键完成全部可自动任务
// （POST /panel/api/accounts/{id}/tasks/auto_all）。
//
// 与队列执行不同，这是**同步**返回逐项结果的路径：面板弹窗要当场展示
// 「哪几项做了、进度从哪到哪、领了多少」，所以这里等流水线跑完再回。
// 上下文挂在请求上，客户端断开即中止；账号锁由 RunAccountAll 的 defer 释放，
// 不会因为提前返回而泄漏 —— 泄漏的后果是该账号此后一直 409。
func (s *Server) handleAccountTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	id := r.PathValue("id")
	ctx, cancel := contextWithTimeout(r, 5*time.Minute)
	defer cancel()

	results, err := s.tasks.RunAccountAll(ctx, id)
	if err != nil {
		writeAccountTaskError(w, err)
		return
	}
	done, failed := 0, 0
	for _, it := range results {
		switch it.Status {
		case "done":
			done++
		case "error":
			failed++
		}
	}
	s.logf("[任务] 一键完成 账号=%s 共 %d 项（完成 %d / 失败 %d）", id, len(results), done, failed)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// writeAccountTaskError 把账号级任务动作的错误映射成合适的状态码。
//
// 三类分开：账号在忙 → 409（等一轮再来，不是故障）；前提不满足（任务不可自动化、
// 开关没开、任务不存在）→ 400（重试无用，文案就是答案）；其余 → 502。
func writeAccountTaskError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tasks.ErrAccountBusy):
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "busy": true,
			"detail": "该账号有任务动作正在执行中，等本轮结束后再试",
		})
	case tasks.IsPrecondition(err):
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "detail": err.Error()})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "detail": err.Error()})
	}
}

// handleTasksVouchers 查询开学季券码（只读）。
//
// body 可选 {account_id}：给了就只查那一个账号，否则查全部国内站可用账号。
// 券码是**用户自己要复制去核销**的东西，所以按账号分组返回，而不是拍平成一个列表 ——
// 拍平之后用户分不清哪张券属于哪个号。
func (s *Server) handleTasksVouchers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()
	groups, err := s.tasks.SchoolVouchers(ctx, req.AccountID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "detail": err.Error()})
		return
	}
	total := 0
	for _, g := range groups {
		total += len(g.Vouchers)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "groups": groups, "total": total})
}
