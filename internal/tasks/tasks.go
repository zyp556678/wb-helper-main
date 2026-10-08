// Package tasks 是成长任务中心：扫描待办、自动完成、报名与领奖，以及连登 / 旅行 / 抽奖三块活动。
//
// 一个前提必须先说清，它决定了本模块能做到什么：
//
// 上游的任务进度**绝大多数是靠客户端行为事件点亮的**（在客户端里用某个模型对话、
// 召唤专家、用模板新建任务、读资料库文档……），这些事件只能在客户端产生。
// 我们唯一能自己造的、且已实测有效的事件是「对话活跃上报」（chat_request_send）——
// 它是客户端发对话时会发的一条埋点，也是「聊天 N 次」「体验某模型」「桌面端对话」
// 这几类任务真正的判据。
//
// 所以本模块的自动化能力分成三档，并在面板上如实标注：
//
//	可自动   —— 判据就是对话事件（chat_5 / first_buddy / Model_chat_* / RichMeow_Chat / black_cat）
//	可报名领奖 —— 中间行为不需要我们做，只要 accept 或 claim（上游已有进度但未领的）
//	需客户端 —— 判据是别的客户端行为，我们只能提示用户去做，不伪造事件
//
// 刻意**不做**的事情：不为「需客户端」那一档伪造事件链。伪造出来的进度一旦被上游
// 风控识别，代价是整个账号受影响，而收益只是几个积分——不划算。
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/eventlog"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/upstream"
)

// Category 是任务的待办分类（面板按此分组展示）。
const (
	CatClaimable = "claimable" // 进度达标未领奖 → 可领
	CatAccept    = "accept"    // 未报名 → 可报名
	CatAuto      = "auto"      // 有自动动作可做
	CatManual    = "manual"    // 需客户端操作
	CatDone      = "done"      // 已领取，无事可做
)

// 上报节奏。
//
// chatEventGap 是「对话活跃事件」（chat_request_send）的**专用**间隔，刻意与
// 队列项之间的通用间隔分开 —— 上游对这类事件有节奏反作弊：
//
//	数秒级连发的事件会**先被计入进度**（回读甚至短暂显示达标），随后被判定无效
//	整体回滚：进度回落、claim 返回 400 task not completed。
//	实测 2s 连发 4 条全灭；45s 间隔逐条上报则全部存活且领奖成功。
//
// 所以这里取 45s + 0~10s 抖动，且**首条也要等**：上一轮残留进度被回滚后立即重报
// 同样会被判无效。代价是 chat_5 这类任务要跑 4 分多钟，但队列在后台执行
// （context.WithoutCancel），用户看得到进度；宁可慢，不可白报 —— 白报不仅拿不到
// 奖励，还会让进度被回滚，比不做更糟。
const (
	chatEventGap    = 45 * time.Second
	chatEventJitter = 10 * time.Second

	// actionGap 是普通动作（accept / claim / 旅行等）之间的间隔：
	// 上游对连续写操作敏感，留一点间隔即可，不需要真人节奏。
	actionGap = 1050 * time.Millisecond
)

// chatEventPause 返回本条上报前的等待时长：固定间隔 + 0~jitter 抖动。
//
// 加抖动的理由：固定周期本身也是一种机器特征。上游的反作弊如果按「间隔是否恒定」
// 判定，恒定的 45s 依然可能被识别。
func chatEventPause() time.Duration {
	return chatEventGap + time.Duration(rand.Int64N(int64(chatEventJitter)))
}

// sleepCtx 可被取消的等待；返回 false 表示上下文已结束、调用方应停止。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// PreconditionError 表示「这个账号/站点本身不满足做这件事的前提」，
// 而不是上游故障。
//
// 为什么要把这一类单独拎出来：调用方此前把所有 error 一律转成 HTTP 502
// （Bad Gateway，语义是「上游网关坏了」）。于是用户点「猫咪出发」，
// 在国际站账号上收到一句 502 —— 它会让人以为是网络抖动，去重试、去查网络，
// 而真正的原因是「国际站没有成长中心活动，这个按钮对你没有意义」。
//
// 归类为「前提不满足」后，服务端可以返回 4xx（客户端侧原因，重试无用），
// 前端也能把文案原样展示出来。
type PreconditionError struct {
	Msg string
}

func (e *PreconditionError) Error() string { return e.Msg }

// precondition 构造一个前提不满足的错误。
func precondition(format string, args ...any) error {
	return &PreconditionError{Msg: fmt.Sprintf(format, args...)}
}

// IsPrecondition 判断 err 是否属于「前提不满足」。
func IsPrecondition(err error) bool {
	var pe *PreconditionError
	return errors.As(err, &pe)
}

// 自动动作的触发方式。
const (
	modeReport = "report" // 对话活跃上报
	// modeRealChat 上报前**先真的发一次对话**（Model_chat_GLM5.2 / black_cat）。
	//
	// 与 modeReport 的差别只有一点，但很关键：这类任务的判据里既有 chat 事件，
	// 也看这次对话本身。只发事件不对话时进度不动 —— 这不是伪造，对话是真的。
	modeRealChat = "realchat"
	// modeBuddy 领养链：上报（解锁前置）→ 同意协议 → 领取第一只 Buddy。
	modeBuddy = "buddy"
	// modeDesktopEvent 上报**客户端指纹事件链**（模板 / 灵感案例 / 设计画布 /
	// 资料库介绍 / 换肤 / 专家召唤 / 技能加载 / 桌面端对话）。
	//
	// 与 modeReport 的本质差别：它发的不是「对话活跃」，而是**伪造的客户端行为**。
	// 因此它在配置里默认关闭（tasks.desktop_events_enabled），
	// 只有显式打开才会进入可自动完成的分类 —— 见 TasksSection 的注释。
	modeDesktopEvent = "desktop"
	// modeMP 小程序口径（事件形状按小程序源码对齐，同样属于伪造客户端行为，
	// 与 modeDesktopEvent 共用同一个开关）。
	modeMP = "mp"
)

// gatedMode 报告该动作是否属于「伪造客户端行为」——这类动作默认关闭，
// 必须显式打开 tasks.desktop_events_enabled 才会进入可自动完成的分类。
func gatedMode(mode string) bool {
	return mode == modeDesktopEvent || mode == modeMP
}

// action 是一个自动动作。
type action struct {
	code string
	desc string
	mode string
	// model 是上报时携带的模型（空表示用默认）。
	model     string
	modelName string
	// times 计算还需上报几次；返回 0 表示这条无需动作。
	times func(t upstream.GrowthTask) int64
	// desktop 是 modeDesktopEvent 的子类型：template / playbook / canvas /
	// library / appearance。用字符串而不是回调：回调无法在单测里断言
	// 「这个 code 映射到哪条事件链」。
	desktop string
	// window 非空时校验时间窗口（夜猫子类任务只在特定时段计分）。
	window func(now time.Time) bool
	// windowHint 是窗口外提示。
	windowHint string
	// run 非空时执行自定义链路（专家召唤 / 真实对话 / 领养 / 小程序）。
	// 此时 mode 只用于分类与开关判定，times 不再参与。
	run func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error)
}

// autoActions 可自动完成的任务表。
//
// 只收「判据确实是对话事件」的任务，并且每一条都经过上游实测。表里没有的 code
// 一律归到「需客户端」，不做猜测性扩展——猜错会白白打一堆无效请求。
var autoActions = []action{
	{
		code: "chat_5", desc: "补报对话活跃事件",
		mode:  modeReport,
		times: func(t upstream.GrowthTask) int64 { return gap(t, 5) },
	},
	{
		code: "first_buddy", desc: "上报解锁 → 同意协议 → 领取第一只 Buddy",
		mode: modeBuddy,
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runFirstBuddy(ctx, m, tg, t)
		},
	},
	{
		code: "RichMeow_Chat", desc: "上报桌面端完整对话事件链（agent_task_created → chat_response）",
		mode: modeDesktopEvent, desktop: "richmeow",
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runRichMeow(ctx, m, tg, t)
		},
	},
	{
		code: "Model_chat_GLM5.2", desc: "用 GLM-5.2 真实对话一次并按模型上报",
		mode: modeRealChat, model: "glm-5.2", modelName: "GLM-5.2",
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runModelChat(ctx, m, tg, "Model_chat_GLM5.2", "glm-5.2", "GLM-5.2")
		},
	},
	{
		code: "black_cat", desc: "夜猫子：夜间 23:00-08:00 窗口内用 GLM-5.2 真实对话补足",
		mode: modeRealChat, model: "glm-5.2", modelName: "GLM-5.2",
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runBlackCat(ctx, m, tg, t)
		},
		window:     blackCatWindow,
		windowHint: "夜猫子任务只在 23:00-08:00 计分，当前不在窗口内（每天 1 次，需累计 3 天）",
	},

	// ---- 客户端指纹事件链（gatedMode，默认关闭）----
	//
	// 这几条的判据是**客户端行为事件**（模板使用 / 灵感案例 / 设计画布 /
	// 资料库点击 / 换肤 / 专家召唤 / 技能加载 / 小程序事件），参考实现按抓包样本
	// 复刻形状并声称三账号实测点亮。我们按同一形状实现，但**默认不启用** ——
	// 理由见 TasksSection 的注释。
	{
		code: "template_5", desc: "上报「使用模板创建任务」事件组",
		mode: modeDesktopEvent, desktop: "template",
		times: func(t upstream.GrowthTask) int64 { return gap(t, 5) },
	},
	{
		code: "playbook_prompt", desc: "上报「灵感案例做同款」事件组",
		mode: modeDesktopEvent, desktop: "playbook",
		times: func(t upstream.GrowthTask) int64 { return gap(t, 1) },
	},
	{
		code: "create_canvas", desc: "上报「设计创意画布创建」事件组",
		mode: modeDesktopEvent, desktop: "canvas",
		times: func(t upstream.GrowthTask) int64 { return gap(t, 1) },
	},
	{
		code: "Library_read", desc: "上报「读资料库介绍」Web 事件",
		mode: modeDesktopEvent, desktop: "library",
		times: func(t upstream.GrowthTask) int64 { return gap(t, 1) },
	},
	{
		code: "Hp_Appearance", desc: "设置主题 + 上报换肤生效事件",
		mode: modeDesktopEvent, desktop: "appearance",
		times: func(t upstream.GrowthTask) int64 { return gap(t, 1) },
	},
	{
		// 五连事件同时覆盖 Buddy_App 与 Buddy_App_QQ（判据应用是同一个）。
		code: "Buddy_App", desc: "上报「进入 Buddy 应用」五连事件",
		mode: modeDesktopEvent, desktop: "buddyapp",
		times: func(t upstream.GrowthTask) int64 { return gap(t, 1) },
	},
	{
		code: "Buddy_App_QQ", desc: "上报「进入 Buddy 应用」五连事件（企鹅教师助手）",
		mode: modeDesktopEvent, desktop: "buddyapp",
		times: func(t upstream.GrowthTask) int64 { return gap(t, 1) },
	},
	{
		code: "automation_1", desc: "上报「定时任务创建成功」事件",
		mode: modeDesktopEvent, desktop: "automation",
		times: func(t upstream.GrowthTask) int64 { return gap(t, 1) },
	},
	{
		code: "expert_5", desc: "真实专家召唤 + 使用链 ×5（市场真实 id + 真实对话 requestId）",
		mode: modeDesktopEvent, desktop: "expert",
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runExpertBatch(ctx, m, tg, t, "agent", gap(*t, 5))
		},
	},
	{
		code: "Expert_team_use_3", desc: "真实专家团召唤 + 使用链 ×3",
		mode: modeDesktopEvent, desktop: "expert",
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runExpertBatch(ctx, m, tg, t, "team", gap(*t, 3))
		},
	},
	{
		code: "Expert_lighthouse", desc: "腾讯轻量云专家召唤 + 使用链（has_expert + LOCAL 口径）",
		mode: modeDesktopEvent, desktop: "expert",
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runExpertLighthouse(ctx, m, tg, t)
		},
	},
	{
		code: "skill_1", desc: "真实对话 + skill_info 技能加载事件",
		mode: modeDesktopEvent, desktop: "skill",
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runSkillFresh(ctx, m, tg, t)
		},
	},

	// ---- 小程序口径（modeMP，同样默认关闭）----
	{
		code: "school_season", desc: "校园日：小程序对话 + activityId 上报 → 领奖",
		mode: modeMP,
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runMPMiniChatTask(ctx, m, tg, "school_season", true)
		},
	},
	{
		code: "Sequential_Tasks_1", desc: "小程序首对话：accept → 对话上报 → 领奖",
		mode: modeMP,
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runMPMiniChatTask(ctx, m, tg, "Sequential_Tasks_1", false)
		},
	},
	{
		code: "Sequential_Tasks_2", desc: "小程序选专家对话：市场真实专家 id → accept → expert_actual_use",
		mode: modeMP,
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runMPExpertTask(ctx, m, tg, "Sequential_Tasks_2")
		},
	},
	{
		code: "Sequential_Tasks_3", desc: "小程序五次对话：accept → 对话上报 ×5（真人节奏）→ 领奖",
		mode: modeMP,
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runMPMiniChatTask(ctx, m, tg, "Sequential_Tasks_3", false)
		},
	},
	{
		code: "Sequential_Tasks_4", desc: "小程序定时任务：accept → 定时任务创建事件 → 领奖",
		mode: modeMP,
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runMPEventTask(ctx, m, tg, "Sequential_Tasks_4", func() error {
				return m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick,
					upstream.DesktopAutomationCreateEvent("WorkBuddy 自动化"))
			}, nil)
		},
	},
	{
		code: "Sequential_Tasks_5", desc: "小程序使用 GLM5.2：带模型字段的对话上报 → 领奖",
		mode: modeMP,
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runMPEventTask(ctx, m, tg, "Sequential_Tasks_5", func() error {
				conv := fmt.Sprintf("wbgw-mp-glm-%d", time.Now().UnixMilli())
				return m.client.ReportMPEvent(ctx, tg.Cred, tg.Prof, tg.Nick,
					upstream.MiniChatModelEvent(conv, mpChatModelID, mpChatModelName))
			}, func() error {
				conv := fmt.Sprintf("wbgw-mp-glm-%d", time.Now().UnixMilli())
				return m.client.ReportChatActivity(ctx, tg.Cred, tg.Prof, conv, "", mpChatModelID, mpChatModelName)
			})
		},
	},
	{
		code: "Sequential_Tasks_6", desc: "小程序十次对话：按差额补足 → 领奖",
		mode: modeMP,
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runMPMiniChatTask(ctx, m, tg, "Sequential_Tasks_6", false)
		},
	},
	{
		code: "Sequential_Tasks_7", desc: "体验灵感功能：PC 灵感事件组 + mp 形态双上报",
		mode: modeMP,
		run: func(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask) (string, error) {
			return runMPEventTask(ctx, m, tg, "Sequential_Tasks_7", func() error {
				ms := time.Now().UnixMilli()
				conv := fmt.Sprintf("wbgw-pb-%d", ms)
				req := fmt.Sprintf("wbgw-pb-req-%d", ms)
				return m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick,
					upstream.DesktopPlaybookPromptSequence(conv, req, mpPlaybookCaseID, mpPlaybookCaseName)...)
			}, func() error {
				return m.client.ReportMPEvent(ctx, tg.Cred, tg.Prof, tg.Nick,
					upstream.MiniPlaybookEvents(mpPlaybookCaseID, mpPlaybookCaseName)...)
			})
		},
	},
}

// blackCatWindow 判断是否处于夜猫子计分时段。
func blackCatWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

func gap(t upstream.GrowthTask, fallback int64) int64 {
	target := t.Target
	if target <= 0 {
		target = fallback
	}
	if n := target - t.Current; n > 0 {
		return n
	}
	return 0
}

// actionFor 返回该任务码的自动动作。
//
// **伪造客户端行为的动作只在显式开启时才返回** —— 这是「默认不伪造客户端行为」
// 这条约定的**唯一执行点**：关着的时候这些 code 会自然落进「需客户端」分类，
// 界面提示与开关的状态因此永远一致。
func (m *Manager) actionFor(code string) *action {
	enabled := m.config().TaskDesktopEventsEnabled()
	for i := range autoActions {
		if autoActions[i].code != code {
			continue
		}
		if gatedMode(autoActions[i].mode) && !enabled {
			return nil
		}
		return &autoActions[i]
	}
	return nil
}

// gatedActionExists 报告该 code 是否有动作、但被「客户端事件上报」开关挡住了。
//
// 单独判一次的理由：这几条在关着的时候会落进「需客户端」，而界面上的文案如果
// 只说「需客户端操作」，用户就会真的去客户端手动做一遍 —— 明明打开开关就能自动。
func gatedActionExists(code string) bool {
	for i := range autoActions {
		if autoActions[i].code == code && gatedMode(autoActions[i].mode) {
			return true
		}
	}
	return false
}

// manualReason 给「需客户端」的项生成一句具体的原因，比统一的「不可自动化」有用得多。
func manualReason(code, desc string) string {
	c := strings.ToLower(code)
	switch {
	case code == "wb_wechat_oa_subscribe_task":
		return "需关注「腾讯 WorkBuddy」官方公众号满 24 小时"
	case code == "Expert_Philanthropy":
		return "需在公益专家内完成一次捐款"
	case gatedActionExists(code):
		return "已内置自动动作，但「客户端事件上报」开关未打开（设置 → 任务中心）"
	case strings.Contains(c, "expert"):
		return "需在客户端召唤专家/专家团并发起对话"
	case strings.Contains(c, "template"):
		return "需在客户端用模板新建任务并发起对话"
	case strings.Contains(c, "skill"):
		return "需在客户端安装并使用一个技能"
	case strings.Contains(c, "library"):
		return "需在客户端读完资料库文档"
	case strings.Contains(c, "automation"):
		return "需在客户端设置一个自动化任务"
	case strings.Contains(c, "canvas"):
		return "需在客户端创建一次设计创意画布"
	case strings.Contains(c, "appearance") || strings.HasPrefix(code, "Hp_"):
		return "需在客户端切换对应主题/外观"
	case strings.Contains(c, "playbook"):
		return "需在客户端体验灵感案例"
	case strings.Contains(c, "buddy_app") || strings.Contains(c, "buddyapp"):
		return "需在客户端进入「发现应用」"
	default:
		if desc != "" {
			return "需客户端操作：" + desc
		}
		return "需客户端操作（上游按客户端行为事件点亮进度）"
	}
}

// -----------------------------------------------------------------------------
// 视图类型
// -----------------------------------------------------------------------------

// TaskView 是任务在面板上的形态（多账号聚合同一 task_code）。
type TaskView struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title"`
	Condition    string `json:"condition"`
	Desc         string `json:"description,omitempty"`
	Credit       int64  `json:"credit"`
	Energy       int64  `json:"energy"`
	Category     string `json:"category"`
	Action       string `json:"action,omitempty"`
	ActionReason string `json:"action_reason,omitempty"`
	Locked       bool   `json:"locked"`
	MPOnly       bool   `json:"mp_only"`
	AcceptStatus string `json:"accept_status,omitempty"`
	Target       int64  `json:"target"`
	Current      int64  `json:"current"`
	// Accounts 是持有该任务的账号 ID 列表。
	Accounts []string `json:"accounts"`
	// AccountCount / ClaimableCount 便于面板直接展示。
	AccountCount   int `json:"account_count"`
	ClaimableCount int `json:"claimable_count"`
}

// AccountScan 是单账号的扫描摘要。
type AccountScan struct {
	UID            string  `json:"uid"`
	ID             string  `json:"id"`
	Nickname       string  `json:"nickname"`
	Site           string  `json:"site"`
	SiteLabel      string  `json:"site_label"`
	TaskCount      int     `json:"task_count"`
	PendingCount   int     `json:"pending_count"`
	ClaimableCount int     `json:"claimable_count"`
	AutoCount      int     `json:"auto_count"`
	ManualCount    int     `json:"manual_count"`
	Streak         int     `json:"streak"`
	TravelState    string  `json:"travel_state,omitempty"`
	TravelReward   float64 `json:"travel_reward,omitempty"`
	LotteryChances int     `json:"lottery_chances"`
	// Error 非空表示该账号读取任务**失败**。
	Error string `json:"error,omitempty"`
	// Note 是中性标注，不是故障：例如「国际站没有成长中心活动」。
	// 与 Error 分开是刻意的 —— 面板把 Error 渲染成红色，把「本就不存在的能力」
	// 说成故障会让用户去查网络。
	Note string `json:"note,omitempty"`
}

// ScanResult 是一次全量扫描的结果。
type ScanResult struct {
	ScannedAt      int64         `json:"scanned_at"`
	Accounts       []AccountScan `json:"accounts"`
	Tasks          []TaskView    `json:"tasks"`
	Totals         Totals        `json:"totals"`
	SupportWarning string        `json:"support_warning,omitempty"`
}

// Totals 是待办汇总。
type Totals struct {
	Tasks      int `json:"tasks"`
	Claimable  int `json:"claimable"`
	Acceptable int `json:"acceptable"`
	Auto       int `json:"auto"`
	Manual     int `json:"manual"`
	Done       int `json:"done"`
}

// -----------------------------------------------------------------------------
// Manager
// -----------------------------------------------------------------------------

// Manager 是任务中心。
type Manager struct {
	pool   *pool.Pool
	client *upstream.Client
	events *eventlog.Log
	Logf   func(format string, args ...any)

	// cfgPtr 用原子指针持有配置快照：面板保存配置是「克隆 → 改克隆 → 换指针」，
	// 固定持有构造时那一份会让勾选项（如 tasks.desktop_events_enabled、
	// schedule.include_disabled_in_tasks）永远停留在启动时的值。
	cfgPtr atomic.Pointer[config.Config]

	mu   sync.Mutex
	last *ScanResult
	// busy 记录正在被任务操作占用的账号，避免同一账号并发跑任务。
	busy map[string]bool
	// queue 是执行队列状态。
	queue *QueueState
	// queueFile 是队列状态的落盘路径（空表示不落盘）。
	queueFile string

	// adoptTried 记录「某账号当日已尝试领养且未过门槛」（key 为 uid，值为 CST 自然日）。
	// 与 mu 分开一把锁：领养判定在调度线程里跑，不该和任务队列的锁互相等。
	adoptMu    sync.Mutex
	adoptTried map[string]string

	// lastStreakBonus 是最近一次连登管家的逐账号结果（面板展示用）。
	streakMu        sync.Mutex
	lastStreakBonus []StreakBonusResult
}

// New 构造任务中心。
func New(p *pool.Pool, client *upstream.Client, cfg *config.Config, events *eventlog.Log) *Manager {
	m := &Manager{
		pool: p, client: client, events: events,
		busy:  map[string]bool{},
		queue: &QueueState{Items: []QueueItem{}, Phase: QueuePhaseIdle},
	}
	m.cfgPtr.Store(cfg)
	if cfg != nil && cfg.WorkDir != "" {
		m.queueFile = filepath.Join(cfg.WorkDir, queueFileName)
		m.loadQueue()
	}
	return m
}

// SetConfig 换入新的配置快照（面板保存配置后由 server 调用）。
//
// 任务中心的多个判据都来自配置（桌面事件开关、保号任务是否覆盖禁用账号），
// 不换入的话「设置页改了、任务照旧」。
func (m *Manager) SetConfig(cfg *config.Config) {
	if cfg != nil {
		m.cfgPtr.Store(cfg)
	}
}

// config 取当前生效的配置快照；未设置时返回零值配置（安全语义）。
func (m *Manager) config() *config.Config {
	if cfg := m.cfgPtr.Load(); cfg != nil {
		return cfg
	}
	return &config.Config{}
}

// queueFileName 是队列状态的落盘文件名。
//
// 落盘的理由很实际：面板一刷新、网关一重启，上一次执行的结果本该还能看见 ——
// 「我刚才点了执行到底成没成功、哪条失败了」这个问题在重启后依然要能回答。
// 只存在内存里的话，这两件事发生时任凭用户怎么翻都找不回来。
const queueFileName = "wb-tasks-queue.json"

// maxPersistedItems 限制落盘的条目数。
//
// 队列最多可能到「账号数 × 任务数」量级（几十到上百条），全存没问题；
// 但设一个上限可以防止将来任务种类暴增后把状态文件撑成几 MB。
const maxPersistedItems = 500

// loadQueue 从磁盘恢复上次的队列状态。
//
// 恢复时**强制把 running 归零**：进程重启后不可能还有 goroutine 在跑，
// 若照搬文件里的 running=true，界面会永远显示「执行中」且无法再触发新队列
// （因为 Run 会一直认为有队列在跑）。把中断的那次标成 done 并注明原因，
// 比让它伪装成「还在跑」诚实得多。
func (m *Manager) loadQueue() {
	raw, err := os.ReadFile(m.queueFile)
	if err != nil {
		return // 首次运行没有这个文件，属正常
	}
	var q QueueState
	if err := json.Unmarshal(raw, &q); err != nil {
		m.logf("[任务] 队列状态文件解析失败，已忽略: %v", err)
		return
	}
	if len(q.Items) > maxPersistedItems {
		q.Items = q.Items[:maxPersistedItems]
	}
	if q.Running {
		q.Running = false
		q.Phase = QueuePhaseDone
		q.PrepareHint = ""
		if q.FinishedAt == 0 {
			q.FinishedAt = time.Now().Unix()
		}
		q.Note = "网关重启，上次执行已中断"
	}
	if q.Phase == "" {
		q.Phase = QueuePhaseIdle
	}
	if q.Items == nil {
		q.Items = []QueueItem{}
	}
	m.mu.Lock()
	m.queue = &q
	m.mu.Unlock()
}

// persistQueue 把队列状态写盘（尽力而为，失败只记日志）。
func (m *Manager) persistQueue() {
	if m.queueFile == "" {
		return
	}
	m.mu.Lock()
	snap := *m.queue
	// 同 Queue()：用 copy 而不是 append(nil, ...)，避免空切片变成 null 落盘。
	snap.Items = make([]QueueItem, len(m.queue.Items))
	copy(snap.Items, m.queue.Items)
	m.mu.Unlock()
	if len(snap.Items) > maxPersistedItems {
		snap.Items = snap.Items[:maxPersistedItems]
	}
	// MPOnlyHint 是内部字段（json:"-"），不必也不该落盘。
	raw, err := json.Marshal(snap)
	if err != nil {
		m.logf("[任务] 序列化队列状态失败: %v", err)
		return
	}
	tmp := m.queueFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		m.logf("[任务] 写入队列状态失败: %v", err)
		return
	}
	// 先写临时文件再改名：中途崩溃时不会留下一个被截断的 JSON，
	// 否则下次启动解析失败、状态直接丢光。
	if err := os.Rename(tmp, m.queueFile); err != nil {
		m.logf("[任务] 替换队列状态文件失败: %v", err)
	}
}

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// target 是一个参与任务操作的账号快照。
type target struct {
	ID       string
	UID      string
	Nick     string
	Site     string
	SiteName string
	Cred     *upstream.CredentialView
	Prof     *upstream.Profile
}

// targets 返回当前可用（未禁用）的账号快照，可按 ID 过滤。
//
// 注意：这里直接做快照后就不再持有账号锁——成长域是控制面请求，
// 不需要和对话请求共用那把串行锁（共用反而会把两者互相拖住）。
func (m *Manager) targets(ids []string) []target {
	return m.targetsIncluding(ids, false)
}

// targetsIncluding 同 targets，includeDisabled 为真时连**已禁用**账号一起返回。
//
// 只给保号类任务用（活跃上报）：schedule.include_disabled_in_tasks 打开后，禁用号的
// 活跃上报照常发（连登与领养前置都靠它），但选号侧依旧不受影响。
func (m *Manager) targetsIncluding(ids []string, includeDisabled bool) []target {
	want := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			want[id] = true
		}
	}
	var out []target
	for _, a := range m.pool.Accounts() {
		if a.IsDisabled() && !includeDisabled {
			continue
		}
		id := a.Cred.AccountID()
		if len(want) > 0 && !want[id] {
			continue
		}
		view := a.View()
		if view == nil || view.AccessToken == "" {
			continue
		}
		site := a.Site()
		out = append(out, target{
			ID: id, UID: view.UID, Nick: a.Cred.Nickname,
			Site: site, SiteName: auth.SiteLabel(site),
			Cred: view, Prof: a.Profile(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// -----------------------------------------------------------------------------
// 扫描
// -----------------------------------------------------------------------------

// Scan 扫描全部账号的任务状态，并缓存结果供面板读取。
func (m *Manager) Scan(ctx context.Context) *ScanResult {
	res := &ScanResult{ScannedAt: time.Now().Unix(), Accounts: []AccountScan{}, Tasks: []TaskView{}}

	merged := map[string]*TaskView{}
	order := []string{}

	for _, tg := range m.targets(nil) {
		if ctx.Err() != nil {
			break
		}
		as := AccountScan{UID: tg.UID, ID: tg.ID, Nickname: tg.Nick, Site: tg.Site, SiteLabel: tg.SiteName}

		// 成长中心只有国内站有（Profile.SupportsGrowthActivity，与参考实现的
		// WbVariant::supports_travel 同口径），且**企业版账号没有个人成长体系**。
		// 两者都在这里标注并跳过。
		//
		// 为什么不直接发请求然后记 Error：那会拿到 404 / 403，然后被渲染成红色的
		// 「读取任务失败」—— 把「本就不存在的能力」说成了故障，用户会去查网络。
		// Note 是中性标注，与 Error 分开。
		if !upstream.GrowthAllowed(tg.Prof, tg.Cred) {
			// 两种原因必须分开说：企业号可以出现在国内站，此时说「国内站没有成长
			// 中心活动」是一句明显错误的话，用户会以为站点选错了。
			if tg.Cred.IsEnterprise() {
				as.Note = "企业版账号没有个人成长体系"
			} else {
				as.Note = tg.SiteName + "没有成长中心活动"
			}
			res.Accounts = append(res.Accounts, as)
			continue
		}

		tasks, err := m.listTasks(ctx, tg)
		if err != nil {
			as.Error = err.Error()
			res.Accounts = append(res.Accounts, as)
			continue
		}
		for _, t := range tasks {
			category, act, reason := m.classify(t)
			as.TaskCount++
			switch category {
			case CatClaimable:
				as.ClaimableCount++
			case CatAccept:
				as.PendingCount++
			case CatAuto:
				as.AutoCount++
			case CatManual:
				as.ManualCount++
			}
			if category != CatDone {
				as.PendingCount++
			}

			v, ok := merged[t.TaskCode]
			if !ok {
				v = &TaskView{
					TaskCode: t.TaskCode, Title: t.Title, Condition: t.TaskDesc,
					Desc: t.Description, Credit: t.Credit, Energy: t.Energy,
					Category: category, Action: act, ActionReason: reason,
					Locked: t.Locked, MPOnly: t.MPOnly,
					AcceptStatus: t.AcceptStatus, Target: t.Target, Current: t.Current,
					Accounts: []string{},
				}
				merged[t.TaskCode] = v
				order = append(order, t.TaskCode)
			}
			v.Accounts = append(v.Accounts, tg.ID)
			if category == CatClaimable {
				v.ClaimableCount++
			}
			// 取「最优」分类：claimable > auto > accept > manual > done，
			// 这样多账号里只要有一个能推进，面板就把它显示成可推进。
			if rank(category) < rank(v.Category) {
				v.Category = category
				v.Action = act
				v.ActionReason = reason
			}
		}

		// 活动状态（连登 / 旅行 / 抽奖）——失败不影响任务扫描
		if upstream.GrowthAllowed(tg.Prof, tg.Cred) {
			if st, err := m.client.GrowthStreak(ctx, tg.Cred, tg.Prof); err == nil && st != nil {
				as.Streak = st.Streak.Days
			}
			if tr, err := m.client.TravelStatus(ctx, tg.Cred, tg.Prof); err == nil && tr != nil {
				as.TravelState = tr.State
				as.TravelReward = tr.RewardCredit
			}
			if lo, err := m.client.LotteryChances(ctx, tg.Cred, tg.Prof); err == nil && lo != nil {
				as.LotteryChances = lo.Chances
			}
		}
		res.Accounts = append(res.Accounts, as)
	}

	for _, code := range order {
		v := merged[code]
		v.AccountCount = len(v.Accounts)
		res.Tasks = append(res.Tasks, *v)
		switch v.Category {
		case CatClaimable, CatAccept, CatAuto, CatManual:
			res.Totals.Tasks++
		}
		switch v.Category {
		case CatClaimable:
			res.Totals.Claimable++
		case CatAccept:
			res.Totals.Acceptable++
		case CatAuto:
			res.Totals.Auto++
		case CatManual:
			res.Totals.Manual++
		case CatDone:
			res.Totals.Done++
		}
	}
	// 排序：可领 → 可自动 → 可报名 → 需客户端 → 已完成；同级按奖励从高到低
	sort.SliceStable(res.Tasks, func(i, j int) bool {
		ri, rj := rank(res.Tasks[i].Category), rank(res.Tasks[j].Category)
		if ri != rj {
			return ri < rj
		}
		return res.Tasks[i].Credit > res.Tasks[j].Credit
	})

	if len(res.Accounts) > 0 {
		allUnsupported := true
		for _, a := range res.Accounts {
			if a.Error == "" {
				allUnsupported = false
				break
			}
		}
		if allUnsupported {
			res.SupportWarning = "所有账号都读取任务失败：成长域活动通常只有国内站可用"
		}
	}

	m.mu.Lock()
	m.last = res
	m.mu.Unlock()
	return res
}

// LastScan 返回上次扫描结果（可能为 nil）。
func (m *Manager) LastScan() *ScanResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// rank 越小越靠前。
func rank(category string) int {
	switch category {
	case CatClaimable:
		return 0
	case CatAuto:
		return 1
	case CatAccept:
		return 2
	case CatManual:
		return 3
	default:
		return 4
	}
}

// classify 判定任务的待办分类与可用动作。
func (m *Manager) classify(t upstream.GrowthTask) (category, act, reason string) {
	if t.Claimed {
		return CatDone, "", ""
	}
	if t.Claimable {
		return CatClaimable, "领取奖励", ""
	}
	if a := m.actionFor(t.TaskCode); a != nil {
		return CatAuto, a.desc, ""
	}
	if t.AcceptStatus != "accepted" && t.AcceptStatus != "claimed" {
		return CatAccept, "报名任务", ""
	}
	return CatManual, "", manualReason(t.TaskCode, t.TaskDesc)
}

// listTasks 拉取单个账号的任务列表：PC 口径 + 小程序口径合并去重。
//
// 合并方向有讲究：两份都以 PC 侧为准（PC 侧字段更全），小程序独有的才标 MPOnly 并追加。
func (m *Manager) listTasks(ctx context.Context, tg target) ([]upstream.GrowthTask, error) {
	pc, err := m.client.ListGrowthTasks(ctx, tg.Cred, tg.Prof, false)
	if err != nil {
		return nil, err
	}
	merged := make([]upstream.GrowthTask, 0, len(pc))
	seen := map[string]bool{}
	for _, t := range pc {
		seen[t.TaskCode] = true
		merged = append(merged, t)
	}
	// 小程序口径：失败静默忽略（PC 侧列表已是主体，缺了它只是少了小程序限定任务）
	if mp, e := m.client.ListGrowthTasks(ctx, tg.Cred, tg.Prof, true); e == nil {
		for _, t := range mp {
			if seen[t.TaskCode] {
				continue
			}
			t.MPOnly = true
			merged = append(merged, t)
		}
	}
	return merged, nil
}

// -----------------------------------------------------------------------------
// 执行
// -----------------------------------------------------------------------------

// RunOptions 是执行参数。
type RunOptions struct {
	// UIDs 限定账号（账号 ID），空表示全部可用账号。
	UIDs []string
	// TaskCodes 限定任务，空表示全部待办任务。
	TaskCodes []string
	// Actions 限定动作集合，可选值 accept / auto / claim；空表示三者都做。
	Actions []string
}

// allow 判断某个动作是否被本次请求允许。
func (o RunOptions) allow(name string) bool {
	if len(o.Actions) == 0 {
		return true
	}
	for _, a := range o.Actions {
		if a == name {
			return true
		}
	}
	return false
}

// Run 异步执行任务队列；返回本次计划执行的任务项数。
//
// # 为什么这里必须立刻返回（不要在里面做扫描）
//
// 早期实现是在这里**同步**跑完 `m.Scan(ctx)` 再返回的 —— 扫描要打上游，
// 实测约 2.5 秒，账号多或上游慢时能到几十秒。这带来两个可见的坏结果：
//
//  1. 用户点「立即执行全部待办」后，界面在这几秒里**完全没有任何反馈**，
//     看起来就像按钮没生效（面板里队列区块在页尾，不滚动根本看不到）。
//  2. 前端是在 `POST /tasks/run` **返回之后**才去读队列的。等它读到的时候，
//     小规模队列（几条）往往已经跑完了 —— 于是 `running` 已经是 false，
//     前端那条「running 才轮询」的 effect 从不启动，**整个执行过程一次都没被看到**。
//
// 现在改为：先把队列置为 `preparing` 并立刻返回，扫描与组装都放进后台 goroutine。
// 返回的 `planned` 这时还不知道确数，用 `-2` 表示「已受理，项数待定」，
// 由前端按 `preparing` 阶段展示。返回 `-1` 仍是「已有队列在跑」，`0` 仍是「无可用账号」。
func (m *Manager) Run(ctx context.Context, opts RunOptions) int {
	m.mu.Lock()
	if m.queue.Running {
		m.mu.Unlock()
		return -1
	}
	// 先把占位状态立起来，避免用户在准备阶段重复点击导致队列叠加。
	m.queue = &QueueState{
		Running: true, Phase: QueuePhasePreparing,
		StartedAt: time.Now().Unix(),
		Items:     []QueueItem{},
	}
	m.mu.Unlock()

	targets := m.targets(opts.UIDs)
	if len(targets) == 0 {
		m.mu.Lock()
		m.queue = &QueueState{
			Phase: QueuePhaseDone, StartedAt: time.Now().Unix(), FinishedAt: time.Now().Unix(),
			Items: []QueueItem{}, Note: "没有可用的账号",
		}
		m.mu.Unlock()
		return 0
	}

	m.setPrepareHint(fmt.Sprintf("正在扫描 %d 个账号的任务…", len(targets)))

	// 后台完成「扫描 → 组装 → 执行」。
	// 用 WithoutCancel 摘掉请求取消：HTTP 响应一返回，r.Context() 就会被取消，
	// 若不摘掉，队列会在第一步就因 ctx.Err() 自杀（表现为「启动后立刻完成，一项都没跑」）。
	go m.prepareAndRun(context.WithoutCancel(ctx), targets, opts)
	return -2
}

// prepareAndRun 在后台完成扫描、组装并串行执行。
func (m *Manager) prepareAndRun(ctx context.Context, targets []target, opts RunOptions) {
	// 先扫描一次拿到最新进度，避免按过期状态行动
	scan := m.Scan(ctx)
	byCode := map[string]TaskView{}
	for _, v := range scan.Tasks {
		byCode[v.TaskCode] = v
	}

	codes := opts.TaskCodes
	if len(codes) == 0 {
		for _, v := range scan.Tasks {
			if v.Category == CatClaimable || v.Category == CatAccept || v.Category == CatAuto {
				codes = append(codes, v.TaskCode)
			}
		}
	}
	wanted := map[string]bool{}
	for _, c := range codes {
		wanted[c] = true
	}

	// 组装执行项：每个 (账号, 任务) 一条
	var items []QueueItem
	for _, tg := range targets {
		taskList, err := m.listTasks(ctx, tg)
		if err != nil {
			m.logf("[任务] 账号 %s 读取任务失败: %v", tg.ID, err)
			continue
		}
		for _, t := range taskList {
			if !wanted[t.TaskCode] {
				continue
			}
			view, ok := byCode[t.TaskCode]
			if !ok {
				continue
			}
			kind := ""
			switch view.Category {
			case CatClaimable:
				kind = "claim"
			case CatAccept:
				kind = "accept"
			case CatAuto:
				kind = "auto"
			}
			if kind == "" || !opts.allow(kind) {
				continue
			}
			items = append(items, QueueItem{
				UID: tg.UID, Account: tg.ID, AccountName: tg.Nick,
				TaskCode: t.TaskCode, Title: t.Title, Kind: kind, Status: "pending",
				MPOnlyHint: t.MPOnly,
			})
		}
	}

	if len(items) == 0 {
		m.mu.Lock()
		m.queue = &QueueState{
			Phase: QueuePhaseDone, StartedAt: m.queue.StartedAt, FinishedAt: time.Now().Unix(),
			Items: []QueueItem{}, Note: "没有需要执行的待办任务",
		}
		m.mu.Unlock()
		m.logf("[任务] 队列组装完成：没有需要执行的待办任务")
		return
	}

	// 组装完成，正式进入执行阶段。已跑过的项数继续累加（这里必然是 0，保持语义清晰）。
	m.mu.Lock()
	m.queue.Phase = QueuePhaseRunning
	m.queue.PrepareHint = ""
	m.queue.Total = len(items)
	m.queue.Items = items
	m.mu.Unlock()
	m.logf("[任务] 队列组装完成：共 %d 项，开始执行", len(items))

	m.runQueue(ctx, items, wanted, scan)
}

// runQueue 串行执行队列。
//
// 为什么串行而不是并发：成长域的上报与领奖接口对同一账号有明显的频率敏感
// （短时间连发会被上游按风控处理），而任务量本身只有几十条，
// 串行的总耗时完全可接受——用并发换来的那点时间不值得冒风控风险。
func (m *Manager) runQueue(ctx context.Context, items []QueueItem, wanted map[string]bool, scan *ScanResult) {
	byID := map[string]target{}
	for _, tg := range m.targets(nil) {
		byID[tg.ID] = tg
	}

	for i := range items {
		if ctx.Err() != nil {
			m.finishQueue("已取消")
			return
		}
		it := &items[i]
		m.markItem(i, "running", "")
		tg, ok := byID[it.Account]
		if !ok {
			m.markItem(i, "skipped", "账号已失效或已禁用")
			continue
		}
		msg := m.runOne(ctx, tg, it)
		status := "ok"
		if strings.HasPrefix(msg, "失败") {
			status = "failed"
		}
		m.markItem(i, status, msg)
		if i < len(items)-1 {
			// 普通动作之间的间隔；对话事件有自己更长的真人节奏（见 chatEventGap）。
			if !sleepCtx(ctx, actionGap) {
				m.finishQueue("已取消")
				return
			}
		}
	}
	m.finishQueue("")
}

// runOne 执行单条队列项，返回可读结果。
func (m *Manager) runOne(ctx context.Context, tg target, it *QueueItem) string {
	switch it.Kind {
	case "claim":
		credit, energy, err := m.client.ClaimGrowthTask(ctx, tg.Cred, tg.Prof, it.TaskCode, it.MPOnlyHint)
		if err != nil {
			return "失败：" + err.Error()
		}
		if credit == 0 && energy == 0 {
			return "已领取过（无新增奖励）"
		}
		return fmt.Sprintf("领取成功 +%d 积分 +%d 能量", credit, energy)

	case "accept":
		// 未解锁的任务上游会返回成功但状态不变（报名是惰性的），
		// 这里先看一眼锁定状态，避免把「没生效」报成「报名成功」。
		if t, err := m.taskByCode(ctx, tg, it.TaskCode); err == nil && t != nil && t.Locked {
			return "任务尚未解锁（需先完成前序任务），已跳过"
		}
		if err := m.client.AcceptGrowthTasks(ctx, tg.Cred, tg.Prof, []string{it.TaskCode}, it.MPOnlyHint); err != nil {
			return "失败：" + err.Error()
		}
		return "报名成功"

	case "auto":
		a := m.actionFor(it.TaskCode)
		if a == nil {
			return "无自动动作"
		}
		if a.window != nil && !a.window(time.Now()) {
			return a.windowHint
		}
		t, err := m.taskByCode(ctx, tg, it.TaskCode)
		if err != nil {
			return "失败：" + err.Error()
		}
		if t == nil {
			return "任务不存在"
		}
		msg, err := m.runAction(ctx, tg, t, a)
		if err != nil {
			return "失败：" + err.Error()
		}
		return m.settleAfterAction(ctx, tg, it, msg)
	}
	return "未知动作"
}

// claimPollAttempts / claimPollGap 是「达标回读」的有界轮询参数。
//
// 上游计分是**异步**的：行为事件上报后进度要数秒才刷新（实测对话完成后立即回读
// 仍是 0/1，约 5-8 秒后才变 1/1）。一次性回读会把「还没算完」误判成「没达标」，
// 于是明明点亮了却不去领奖。这里最多轮询 N 次、每次隔 gap，总预算约 12 秒。
var (
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
)

// taskByCodeMP 以小程序口径拉取单个任务（mp 专属任务在默认列表里查不到）。
func (m *Manager) taskByCodeMP(ctx context.Context, tg target, code string) (*upstream.GrowthTask, error) {
	list, err := m.client.ListGrowthTasks(ctx, tg.Cred, tg.Prof, true)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].TaskCode == code {
			return &list[i], nil
		}
	}
	return nil, nil
}

// taskByCodeWaiting 回读任务；未达标时在有界预算内轮询等待（上游异步计分）。
//
// 已达标（claimable/claimed）立即返回；预算耗尽返回最后一次结果（可能仍未达标）。
func (m *Manager) taskByCodeWaiting(ctx context.Context, tg target, code string, mp bool) (*upstream.GrowthTask, error) {
	fetch := func() (*upstream.GrowthTask, error) {
		if mp {
			return m.taskByCodeMP(ctx, tg, code)
		}
		return m.taskByCode(ctx, tg, code)
	}
	t, err := fetch()
	if err != nil || t == nil {
		return t, err
	}
	if t.Claimable || t.Claimed {
		return t, nil
	}
	for i := 1; i < claimPollAttempts; i++ {
		if !sleepCtx(ctx, claimPollGap) {
			return t, nil
		}
		t2, err2 := fetch()
		if err2 != nil {
			return t, nil // 轮询期间的查询失败不覆盖已拿到的结果
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

// taskByCodeWaitingMP 是 mp 口径的达标回读（两轮各隔 3 秒，比 PC 侧紧凑）。
//
// 小程序接口对同一账号的连续读更敏感，而且 mp 任务的判据上报本身就带真人节奏，
// 到这一步时上游多半已经算完了。
func (m *Manager) taskByCodeWaitingMP(ctx context.Context, tg target, code string) (*upstream.GrowthTask, error) {
	t, err := m.taskByCodeMP(ctx, tg, code)
	if err != nil || t == nil {
		return t, err
	}
	for i := 0; i < 2; i++ {
		if t.Claimable || t.Claimed {
			return t, nil
		}
		if !sleepCtx(ctx, claimPollGap) {
			return t, nil
		}
		if t2, e := m.taskByCodeMP(ctx, tg, code); e == nil && t2 != nil {
			t = t2
		}
	}
	return t, nil
}

// settleAfterAction 在动作执行后回读进度，**达标就自动领奖**。
//
// 为什么要把「完成」与「领奖」收敛成一步：动作成功后进度往往已经达标，
// 但用户还得回列表再点一次「领取」—— 而那一步经常被忘掉，结果第二天进度被重置、
// 奖励白丢。这里自动接上，只在领奖失败时提示手动重试（不掩盖主流程结果）。
func (m *Manager) settleAfterAction(ctx context.Context, tg target, it *QueueItem, msg string) string {
	t, err := m.taskByCodeWaiting(ctx, tg, it.TaskCode, it.MPOnlyHint)
	if err != nil || t == nil {
		return msg
	}
	if !t.Claimable {
		return msg + "（进度 " + progressText(t) + "）"
	}
	credit, energy, cerr := m.client.ClaimGrowthTask(ctx, tg.Cred, tg.Prof, it.TaskCode, it.MPOnlyHint)
	if cerr != nil {
		return msg + "；已达标但领奖失败，可在任务列表手动点「领取」重试"
	}
	if credit == 0 && energy == 0 {
		return msg + "；奖励此前已领取"
	}
	return msg + fmt.Sprintf("；已自动领奖 +%d 积分 +%d 能量", credit, energy)
}

// progressText 是任务进度的可读表示（回读对比与提示用）。
func progressText(t *upstream.GrowthTask) string {
	if t == nil {
		return "?"
	}
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.Claimed {
		return "已领取"
	}
	if t.AcceptStatus != "" {
		return t.AcceptStatus
	}
	return "?"
}

// taskByCode 拉取单个任务的最新状态。
func (m *Manager) taskByCode(ctx context.Context, tg target, code string) (*upstream.GrowthTask, error) {
	list, err := m.listTasks(ctx, tg)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].TaskCode == code {
			return &list[i], nil
		}
	}
	return nil, nil
}

// -----------------------------------------------------------------------------
// 队列状态
// -----------------------------------------------------------------------------

// QueueItem 是队列里的一个执行项。
type QueueItem struct {
	UID         string `json:"uid"`
	Account     string `json:"account"`
	AccountName string `json:"account_name,omitempty"`
	TaskCode    string `json:"task_code"`
	Title       string `json:"title,omitempty"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	// MPOnlyHint 是内部字段：该任务是否需小程序口径（要一起传给 claim/accept）。
	MPOnlyHint bool  `json:"-"`
	StartedAt  int64 `json:"started_at,omitempty"`
	FinishedAt int64 `json:"finished_at,omitempty"`
}

// 队列阶段。
//
// 单独有一个 `preparing` 而不是复用 `running`，是因为**准备阶段本身要花时间**：
// 触发一次执行要先全量扫描（打上游、最坏几十秒）。若这段时间队列看起来是「什么都没发生」，
// 用户会以为按钮没生效而反复点，或者干脆去重启网关。
//
// 把它显式建模成一个阶段，界面就能说「正在扫描任务…（约 2-5 秒）」，
// 而不是给一个不会动的 0%。
const (
	// QueuePhaseIdle 表示还没有执行过任何队列。
	QueuePhaseIdle = "idle"
	// QueuePhasePreparing 表示正在扫描并组装执行项，还没开始真正执行。
	QueuePhasePreparing = "preparing"
	// QueuePhaseRunning 表示正在逐项执行。
	QueuePhaseRunning = "running"
	// QueuePhaseDone 表示已结束（成功或带失败）。
	QueuePhaseDone = "done"
)

// QueueState 是队列整体状态。
type QueueState struct {
	Running    bool  `json:"running"`
	StartedAt  int64 `json:"started_at"`
	FinishedAt int64 `json:"finished_at,omitempty"`
	// Phase 是机读阶段：idle / preparing / running / done。
	//
	// 与 Running 并存而不是取代它：Running 是「有没有在跑」的布尔判断（前端据此决定
	// 要不要轮询），Phase 是「跑到哪一步」的展示判断。用一个字段表达两件事迟早会打架。
	Phase string `json:"phase"`
	// PrepareHint 在 preparing 阶段给出可读说明（例如「正在扫描 2 个账号的任务」）。
	PrepareHint string      `json:"prepare_hint,omitempty"`
	Total       int         `json:"total"`
	Done        int         `json:"done"`
	Failed      int         `json:"failed"`
	Note        string      `json:"note,omitempty"`
	Items       []QueueItem `json:"items"`
}

// Phase 归一化：老数据没有 phase 字段时按 running/finished 推断，避免界面拿到空串。
func (q QueueState) NormalizedPhase() string {
	if q.Phase != "" {
		return q.Phase
	}
	switch {
	case q.Running:
		return QueuePhaseRunning
	case q.StartedAt == 0:
		return QueuePhaseIdle
	default:
		return QueuePhaseDone
	}
}

// Queue 返回队列状态快照。
func (m *Manager) Queue() QueueState {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *m.queue
	// 必须保证 Items 非 nil。
	//
	// `append([]QueueItem(nil), 空切片...)` 会返回 **nil**，而 nil 切片序列化成
	// `"items": null`。前端拿到它做 `items.length` 会直接抛
	// 「Cannot read properties of null (reading 'length')」，整个面板页崩成
	// 「这个页面出错了」——用户看到的是白屏加一个重试按钮，完全不像一个数据问题。
	//
	// 这条路径在「刚启动、还没跑过队列」时必然经过，所以不是理论风险。
	cp.Items = make([]QueueItem, len(m.queue.Items))
	copy(cp.Items, m.queue.Items)
	// 兜底归一化：老的状态文件可能没有 phase，不能让前端拿到空串去判断。
	cp.Phase = cp.NormalizedPhase()
	return cp
}

func (m *Manager) markItem(i int, status, msg string) {
	m.mu.Lock()
	if i < 0 || i >= len(m.queue.Items) {
		m.mu.Unlock()
		return
	}
	it := &m.queue.Items[i]
	it.Status = status
	if msg != "" {
		it.Message = msg
	}
	it.FinishedAt = time.Now().Unix()
	switch status {
	case "ok":
		m.queue.Done++
	case "failed":
		m.queue.Failed++
	}
	// 取一份快照用于写日志（不能在持锁时调 events，避免把日志缓冲的锁卷进来）。
	snap := *it
	phase := m.queue.Phase
	m.mu.Unlock()

	// 把每一条结果写进任务通道。
	//
	// 为什么必须落日志而不是只留在内存队列里：内存队列只在页面停留期间可见，
	// 一刷新、一重启就没了，而且用户排查「为什么这条失败」时习惯去日志页翻。
	// 只记 start/done 两条汇总的话，日志页只能说「2 项里有 1 项失败」，
	// 却永远说不出**是哪一项、为什么** —— 那正是用户唯一想看的信息。
	//
	// 只记终态（ok/failed/skipped），不记 running：running 是过程态，
	// 每项都记会产生两倍噪声且不携带新信息。
	if status == "running" || m.events == nil {
		return
	}
	if phase != QueuePhaseRunning {
		return
	}
	level := eventlog.LevelInfo
	if status == "failed" {
		level = eventlog.LevelWarn
	}
	title := snap.Title
	if title == "" {
		title = snap.TaskCode
	}
	detail := snap.Message
	if detail == "" {
		detail = status
	}
	m.events.Add(level, eventlog.ChannelTask, "task_item_"+status,
		fmt.Sprintf("[%s] %s —— %s", kindLabel(snap.Kind), title, detail),
		map[string]any{
			"task_code":    snap.TaskCode,
			"kind":         snap.Kind,
			"status":       status,
			"account":      snap.Account,
			"account_name": snap.AccountName,
			"message":      snap.Message,
		})
}

// kindLabel 把动作标识翻成中文，便于日志页直接读。
func kindLabel(kind string) string {
	switch kind {
	case "claim":
		return "领取奖励"
	case "accept":
		return "报名任务"
	case "auto":
		return "自动完成"
	default:
		return kind
	}
}

// setPrepareHint 更新准备阶段的提示文案（不改变其它字段）。
func (m *Manager) setPrepareHint(hint string) {
	m.mu.Lock()
	m.queue.PrepareHint = hint
	m.mu.Unlock()
}

func (m *Manager) finishQueue(note string) {
	m.mu.Lock()
	q := m.queue
	q.Running = false
	q.Phase = QueuePhaseDone
	q.PrepareHint = ""
	q.FinishedAt = time.Now().Unix()
	if note != "" {
		q.Note = note
	}
	total, failed := q.Total, q.Failed
	m.mu.Unlock()

	m.persistQueue()

	if m.events != nil {
		level := eventlog.LevelInfo
		if failed > 0 {
			level = eventlog.LevelWarn
		}
		m.events.Add(level, eventlog.ChannelTask, "task_queue_done",
			fmt.Sprintf("任务队列执行完成：%d 项，失败 %d 项", total, failed),
			map[string]any{"total": total, "failed": failed})
	}
	m.logf("[任务] 队列执行完成：共 %d 项，失败 %d 项", total, failed)
}

// -----------------------------------------------------------------------------
// 单独动作（面板按钮）
// -----------------------------------------------------------------------------

// ReportActivity 手动补一条对话活跃上报（account 为空时对全部可用账号各发一条）。
func (m *Manager) ReportActivity(ctx context.Context, accountID, model string) (string, error) {
	var list []target
	if strings.TrimSpace(accountID) == "" {
		// 全量上报：是否覆盖禁用账号由 schedule.include_disabled_in_tasks 决定
		//（缺省跳过，与选号过滤一致）。
		list = m.targetsIncluding(nil, m.config().IncludeDisabledInTasks())
	} else {
		// 指定账号是显式意图，不受开关影响。
		list = m.targets([]string{accountID})
	}
	if len(list) == 0 {
		return "", fmt.Errorf("没有可用的账号")
	}
	ok := 0
	gated := 0
	var firstErr error
	var suspects []string
	for _, tg := range list {
		// 活跃上报走的是成长域端点（ReportChatActivity 在 internal/upstream/growth.go）。
		// 两类账号注定失败，先跳过、别白发请求：
		//   - 企业版：没有个人成长体系（auth.Credential.IsEnterprise）
		//   - 国际站：没有成长中心（Profile.SupportsGrowthActivity）
		// 这是「面板点补活跃」与排程共用的入口，所以门控放在这里。
		if !upstream.GrowthAllowed(tg.Prof, tg.Cred) {
			gated++
			continue
		}
		cid := fmt.Sprintf("wbgw-manual-%d", time.Now().UnixMilli())
		if err := m.client.ReportChatActivity(ctx, tg.Cred, tg.Prof, cid, "", model, ""); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok++
		// 上报成功 ≠ 计分成功：实测存在「200 但静默丢弃」（缺 userId 时进度不动），
		// 所以回读一次连登天数做闭环校验（只读，失败不影响主流程）。
		if check := m.CheckActivityStreak(ctx, tg); check.Suspect {
			suspects = append(suspects, tg.Nick+"："+check.Note)
		}
	}
	if ok == 0 && firstErr != nil {
		return "", firstErr
	}
	detail := fmt.Sprintf("已上报 %d/%d 个账号的对话活跃事件", ok, len(list))
	// 跳过的原因写出来，否则「上报 0/3」看着像失败。
	if gated > 0 {
		detail += fmt.Sprintf("；跳过 %d 个（企业版或国际站，无成长体系）", gated)
	}
	if len(suspects) > 0 {
		detail += "；注意：" + strings.Join(suspects, "；")
	}
	return detail, nil
}

// TravelAction 执行一次旅行动作（depart / claim）。
func (m *Manager) TravelAction(ctx context.Context, uid, action string) (string, error) {
	tg, err := m.one(uid)
	if err != nil {
		return "", err
	}
	// 派猫猫旅行是成长中心的活动，只有国内站有（与参考实现的 WbVariant::supports_travel 同口径）。
	// 在发请求前就拒掉：否则用户会看到一个 404 文案，误以为是网络或上游故障。
	if !upstream.GrowthAllowed(tg.Prof, tg.Cred) {
		return "", precondition("%s 没有成长中心活动，无法执行旅行动作", tg.SiteName)
	}
	switch action {
	case "depart":
		st, err := m.client.TravelStatus(ctx, tg.Cred, tg.Prof)
		if err != nil {
			return "", err
		}
		if st != nil && st.DailyLimitReached {
			return "今日出发次数已用完", nil
		}
		if err := m.client.TravelDepart(ctx, tg.Cred, tg.Prof, 0); err != nil {
			return "", err
		}
		return "已让猫咪出发（到达后可领奖励）", nil
	case "claim":
		st, err := m.client.TravelStatus(ctx, tg.Cred, tg.Prof)
		if err != nil {
			return "", err
		}
		if st == nil {
			return "无法读取旅行状态", nil
		}
		if st.State != "arrived" && st.RewardCredit <= 0 {
			return fmt.Sprintf("猫咪还在路上（状态 %s），到达后才能领奖励", st.State), nil
		}
		credit, err := m.client.TravelClaim(ctx, tg.Cred, tg.Prof, st.RecordID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("领取成功 +%d 积分", credit), nil
	}
	return "", precondition("未知的旅行动作: %s", action)
}

// RedeemStreak 兑换连登奖励。
func (m *Manager) RedeemStreak(ctx context.Context, uid, tier string) (string, error) {
	tg, err := m.one(uid)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(tier) == "" {
		return "", precondition("缺少兑换档位")
	}
	if err := m.client.GrowthRedeemTier(ctx, tg.Cred, tg.Prof, tier); err != nil {
		return "", err
	}
	return "兑换成功（档位 " + tier + "）", nil
}

// DrawLottery 抽一次奖。
func (m *Manager) DrawLottery(ctx context.Context, uid string) (string, error) {
	tg, err := m.one(uid)
	if err != nil {
		return "", err
	}
	sum, err := m.client.LotteryChances(ctx, tg.Cred, tg.Prof)
	if err != nil {
		return "", err
	}
	if sum != nil && sum.Chances <= 0 {
		return "没有可用的抽奖机会（做任务或连登可获得）", nil
	}
	out, err := m.client.LotteryDraw(ctx, tg.Cred, tg.Prof)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(out))
	if text == "" || text == "null" {
		return "抽奖完成", nil
	}
	return "抽奖完成：" + text, nil
}

// one 取单个账号快照。
func (m *Manager) one(id string) (target, error) {
	list := m.targets([]string{id})
	if len(list) == 0 {
		return target{}, precondition("账号不存在或已禁用: %s", id)
	}
	return list[0], nil
}

// Overview 汇总当前待办数量（供面板摘要与定时任务判断是否需要跑）。
func (m *Manager) Overview() map[string]any {
	res := m.LastScan()
	if res == nil {
		return map[string]any{"scanned": false}
	}
	return map[string]any{
		"scanned":    true,
		"scanned_at": res.ScannedAt,
		"totals":     res.Totals,
		"accounts":   len(res.Accounts),
		"queue":      m.Queue(),
	}
}
