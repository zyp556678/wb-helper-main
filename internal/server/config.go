package server

import (
	"io"
	"net/http"
	"time"

	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/pool"
)

// handlePanelConfigRoute 按方法分发：GET 读配置，POST 保存配置补丁。
func (s *Server) handlePanelConfigRoute(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.configView())
	case http.MethodPost:
		s.saveConfig(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET / POST"))
	}
}

// configView 返回可安全展示的配置（**不含任何凭据**；api_key 见下方说明）。
func (s *Server) configView() map[string]any {
	c := s.config()
	enabled, ttl, gc := s.sticky.Config()
	return map[string]any{
		// ---- 运行信息（只读）----
		"listen":             c.ListenAddr(),
		"auth_check_enabled": c.APIKey != "",
		// api_key 在**已配置**时下发，未配置时是空串。
		//
		// 这里破例下发密钥，是因为「模型与档位」页要能把 Base URL + 真实密钥
		// 一起复制出去接别的工具（Claude Code / Cursor 之类）。不下发的话，
		// 用户设了 --api-key 之后在面板里反而看不到自己设的是什么。
		//
		// 风险面没有变大：这个接口本身就在 withPanelAuth 后面 —— 配了密钥就必须
		// 带对才能进来，调用方**本来就在 Authorization 头里带着这个值**，
		// 回给它不是新的泄露面。未配置时接口是公开的，但那时也没有密钥可泄。
		"api_key":           c.APIKey,
		"credential_source": s.credentialSource(),
		"reload_interval":   c.ReloadInterval,
		"models_refresh":    c.ModelsRefresh,
		"upstream": map[string]any{
			"header_timeout_seconds": int(c.HeaderTimeout.Seconds()),
			"idle_timeout_seconds":   int(c.IdleTimeout.Seconds()),
			"proxy":                  c.ProxyURL != "",
		},
		"debug_enabled": c.DebugEnabled,
		"models_filter": map[string]any{
			"blocklist": c.ModelBlock,
			"allowlist": c.ModelAllow,
		},

		// ---- 可热改的治理与调度（切片 2）----
		"cooldown": c.Cooldown,
		"pool":     c.Pool,
		"session_sticky": map[string]any{
			"enabled":             enabled,
			"ttl_seconds":         int(ttl.Seconds()),
			"gc_interval_seconds": int(gc.Seconds()),
		},
		"schedule": map[string]any{
			"checkin_enabled":           c.CheckinEnabled(),
			"checkin_hours":             c.Schedule.CheckinHours,
			"checkin_start":             c.Schedule.CheckinStart,
			"checkin_end":               c.Schedule.CheckinEnd,
			"checkin_excluded_accounts": c.CheckinExcludedAccountIDs(),
			"keepalive_enabled":         c.KeepaliveEnabled(),
			"keepalive_hours":           c.Schedule.KeepaliveHours,
			"keepalive_days":            c.KeepaliveDays(),
			"lazy_refresh_hours":        c.LazyRefreshHours(),
			"balance_refresh_enabled":   c.BalanceRefreshEnabled(),
			"balance_refresh_minutes":   c.Schedule.BalanceRefreshMinutes,
			"tasks_enabled":             c.TasksEnabled(),
			"tasks_hours":               c.Schedule.TasksHours,
			"blackcat_enabled":          c.BlackCatEnabled(),
			"blackcat_hours":            c.Schedule.BlackCatHours,
			"travel_enabled":            c.TravelEnabled(),
			"travel_hours":              c.Schedule.TravelHours,
			"activity_enabled":          c.ActivityEnabled(),
			"activity_hours":            c.Schedule.ActivityHours,
			// 保号类四任务是否覆盖已禁用账号（缺省 false = 禁用即跳过）。
			"include_disabled_in_tasks": c.IncludeDisabledInTasks(),
		},
		// 入站 HTTP 参数：**只读展示 + 可保存**，但保存后需重启才生效
		//（http.Server 只在启动时构造一次，见 SavePatch 的 need_restart）。
		"server": map[string]any{
			"read_timeout": c.Server.ReadTimeout,
		},
		"selection_strategy": s.pool.Strategy,
		"hot_reloadable":     config.HotFields(),

		// ---- 任务中心策略（切片 18）----
		"tasks": map[string]any{
			"desktop_events_enabled": c.TaskDesktopEventsEnabled(),
		},

		// ---- 本机代理（切片 7）：只暴露配置项，运行状态走 /panel/api/local/capabilities
		"local": map[string]any{
			"enabled":  c.LocalAgentEnabled(),
			"bin":      c.Local.Bin,
			"data_dir": c.Local.DataDir,
		},

		// ---- 提示词与出站改写（切片 4）----
		"prompt": map[string]any{
			"mode":                c.Prompt.Mode,
			"text":                c.Prompt.Text,
			"file":                c.Prompt.File,
			"sanitize":            c.PromptSanitize(),
			"degraded_retry":      c.PromptDegradedRetry(),
			"strict_first_system": c.PromptStrictFirstSystem(),
			// effective_text_preview 是**当前实际生效**的提示词前 200 字，
			// 让用户能确认自己到底在用什么（尤其 file 非空时 text 是无效的）。
			"effective_text_preview": preview(s.EffectivePrompt(), 200),
		},
	}
}

// preview 截断长文本用于面板展示（按 rune 截，避免把多字节字符切坏）。
func preview(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// saveConfig 保存配置补丁并热生效。
func (s *Server) saveConfig(w http.ResponseWriter, r *http.Request) {
	// 故意传原始 JSON 而不是解好的结构体：结构体分不清「字段没提交」与「提交了零值」，
	// 配置层需要原始键来判断该覆盖哪些字段（详见 config.SavePatch 的注释）。
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("读取请求体失败: "+err.Error()))
		return
	}
	_ = r.Body.Close()

	// 写时复制：**先克隆**，在克隆上合并补丁，成功了再整体换指针。
	//
	// 为什么不直接在 s.config() 上改：chat 热路径正在并发读那份配置，
	// 原地改写会让它读到撕裂的中间态（slice header / string 都不是原子读）。
	// 换指针后，仍在处理中的请求继续用它们已经拿到的那份旧配置，
	// 新请求拿到新配置 —— 两边都是完整版本，只是版本不同，这是可接受的。
	next := s.config().Clone()
	applied, needRestart, err := next.SavePatch(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("配置保存失败: "+err.Error()))
		return
	}
	s.cfgPtr.Store(next)

	// 把新配置推给运行时组件（这一步才是「热生效」的实质）
	s.applyRuntimeConfig()

	s.logf("[配置] 已保存（热生效 %v，需重启 %v）", applied, needRestart)
	writeJSON(w, http.StatusOK, map[string]any{
		"applied":      applied,
		"need_restart": needRestart,
		"config":       s.configView(),
	})
}

// applyRuntimeConfig 把配置同步到治理层与粘性表。
// 上游超时属于装配期字段，无法在此热改，需重启进程。
func (s *Server) applyRuntimeConfig() {
	c := s.config()
	s.pool.SetGov(pool.GovFromConfig(c))
	s.pool.SetCreditFloor(c.CreditFloor())
	s.sticky.Configure(c.StickyEnabled(), c.StickyTTL(), c.StickyGCInterval())
	// 调度器持有的是配置快照指针：不在这里换入新快照，面板改的签到时间段 /
	// 排除名单 / 各任务时点对排程循环永远不可见（设置页看着改了、定时任务照旧）。
	s.sched.SetConfig(c)
	// 任务中心同样持有配置快照：桌面事件开关、保号任务是否覆盖禁用账号这些判据
	// 都来自配置，不换入一样会「设置页改了、任务照旧」。
	if s.tasks != nil {
		s.tasks.SetConfig(c)
	}
}

// handleStickyInfo 返回会话粘性的当前状态（面板展示用）。
func (s *Server) handleStickyInfo(w http.ResponseWriter, r *http.Request) {
	enabled, ttl, gc := s.sticky.Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":             enabled,
		"bindings":            s.sticky.Count(),
		"ttl_seconds":         int(ttl.Seconds()),
		"gc_interval_seconds": int(gc.Seconds()),
	})
}

// handleSchedulerRun 手动触发一个定时任务。
func (s *Server) handleSchedulerRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Task string `json:"task"`
		// RespectWindow 让「签到」遵守配置的签到时间段（窗口外整轮跳过）。
		// 面板的「刷新并签到」传 true；单账号的立即签到不传。
		RespectWindow bool `json:"respect_window"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}

	ctx, cancel := contextWithTimeout(r, 3*time.Minute)
	defer cancel()

	var (
		detail  string
		err     error
		skipped bool
	)
	switch req.Task {
	case "checkin":
		if req.RespectWindow {
			detail, skipped, err = s.sched.RunCheckinNowIfInWindow(ctx)
		} else {
			detail, err = s.sched.RunCheckinNow(ctx)
		}
	case "keepalive":
		detail, err = s.sched.RunKeepaliveNow(ctx)
	case "balance":
		detail, err = s.sched.RunBalanceNow(ctx)
	case "tasks":
		detail, err = s.sched.RunTasksNow(ctx)
	case "blackcat":
		detail, err = s.sched.RunBlackCatNow(ctx)
	case "travel":
		detail, err = s.sched.RunTravelNow(ctx)
	case "activity":
		detail, err = s.sched.RunActivityNow(ctx)
	default:
		writeJSON(w, http.StatusBadRequest, errBody("未知任务: "+req.Task+
			"（可选 checkin / keepalive / balance / tasks / blackcat / travel / activity）"))
		return
	}

	if err != nil {
		writeActionErrorExtra(w, err, map[string]any{"task": req.Task})
		return
	}
	payload := map[string]any{"ok": true, "task": req.Task, "detail": detail}
	if skipped {
		// 明确区分「跳过了」与「做了」：前端据此显示中性提示而不是成功勾。
		payload["skipped"] = true
		payload["reason"] = "outside_checkin_window"
	}
	writeJSON(w, http.StatusOK, payload)
}
