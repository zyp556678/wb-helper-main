package server

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy-gateway/internal/pool"
)

// 本文件实现「账号健康告警」（对照 wb-gateway v1.14.7 / v1.14.17）。
//
// 告警回答的是「现在该先处理哪个账号」：账号失效、令牌过期、额度用尽、
// 冷却中、被暂停 —— 这五种状态各自有不同的处置动作，但散在卡片各处时
// 用户只能逐个卡片看。这里把它们抽成一份平铺列表，按时间倒序。
//
// 与上游的差别（有意为之）：
//   - 上游把标记写进状态快照、并记日志文件字节偏移供「跳转到日志行」用；
//     本项目**没有日志文件**（事件日志是内存环形缓冲，不落盘），跳转无处可去，
//     因此只保留「状态 + 首次出现时间」，不做日志定位。v1.14.18 上游自己
//     也把跳转撤销了。
//   - 首次出现时间由本进程内存维护，重启后从当前时刻重新计时 —— 告警不是
//     审计数据，重启后「这个号已经坏了多久」从头算不会误导任何人。

// AccountAlert 是一条账号告警。
type AccountAlert struct {
	Account string `json:"account"`
	// State 是告警类别：disabled / expired / exhausted / cooldown / paused。
	State string `json:"state"`
	// Label 是短标签（卡片与列表上显示的那几个字）。
	Label string `json:"label"`
	// Severity 只用于配色与排序：urgent > important > warning > notice。
	Severity string `json:"severity"`
	Message  string `json:"message"`
	// Since 是**首次出现**的 Unix 秒（同一次故障期间不刷新）。
	Since int64 `json:"since"`
}

type alertRule struct {
	Label    string
	Severity string
	Message  string
}

// 顺序即优先级：urgent 最先。
var alertRules = map[string]alertRule{
	"disabled": {"账号失效", "urgent", "登录授权已失效，账号已停止调度，请重新登录。"},
	"expired":  {"账号过期", "important", "访问令牌已过期，请检查自动续期结果或重新登录。"},
	"exhausted": {"积分用尽", "important",
		"已确认账号额度耗尽，收费模型暂不可用；等待额度恢复或签到。"},
	"cooldown": {"账号冷却", "warning", "账号触发频率限制，冷却期间暂不调度，等待自动恢复。"},
	"paused":   {"账号暂停", "notice", "该账号被暂停选号；这不是授权失效，可在账号卡片上恢复。"},
}

var alertSeverityOrder = map[string]int{
	"urgent": 0, "important": 1, "warning": 2, "notice": 3,
}

// accountAlertKinds 返回账号当前命中的告警类别。
//
// 判据都取**已有的运行状态**，不新增探测：告警只是换个视角重排事实，
// 不应因为「面板打开了」而多打一次上游。
func accountAlertKinds(st pool.AccountState, now time.Time) []string {
	var kinds []string
	if st.Disabled {
		kinds = append(kinds, "disabled")
	}
	if !st.TokenValid {
		kinds = append(kinds, "expired")
	}
	if st.Quota != nil && st.Quota.Exhausted && !st.Quota.Unlimited {
		kinds = append(kinds, "exhausted")
	}
	if !st.Disabled && (st.CooldownUntil > now.Unix() || st.BreakerUntil > now.Unix()) {
		kinds = append(kinds, "cooldown")
	}
	if st.Paused && !st.Disabled {
		kinds = append(kinds, "paused")
	}
	return kinds
}

// alertTracker 记住每条告警的首次出现时刻。
//
// 用「账号 + 类别」作键：同一个账号从「冷却」转成「失效」是两件不同的事，
// 各自计时比共用一条时间线更能说明「它已经坏了一上午」。
type alertTracker struct {
	mu    sync.Mutex
	since map[string]int64
}

func newAlertTracker() *alertTracker {
	return &alertTracker{since: map[string]int64{}}
}

// observe 用当前快照更新首次出现时刻，并返回告警列表。
func (t *alertTracker) observe(states []pool.AccountState, now time.Time) []AccountAlert {
	out := t.collect(states, now)
	// 排序放在锁外：轮询频繁（监控页每 5 秒），锁内排序会串行化并发面板请求。
	sortAlerts(out)
	return out
}

// collect 在持锁状态下更新台账并收集告警（未排序）。
func (t *alertTracker) collect(states []pool.AccountState, now time.Time) []AccountAlert {
	t.mu.Lock()
	defer t.mu.Unlock()

	seen := make(map[string]bool, len(t.since))
	out := make([]AccountAlert, 0, len(states))
	for _, st := range states {
		for _, kind := range accountAlertKinds(st, now) {
			key := st.ID + "\x00" + kind
			seen[key] = true
			if _, ok := t.since[key]; !ok {
				t.since[key] = now.Unix()
			}
			rule := alertRules[kind]
			out = append(out, AccountAlert{
				Account: st.ID, State: kind,
				Label: rule.Label, Severity: rule.Severity, Message: rule.Message,
				Since: t.since[key],
			})
		}
	}
	// 已经恢复的告警要立刻从台账里消失：留着会让「恢复后首次再现」
	// 显示成很久以前，而这正是用户最容易被误导的地方。
	//
	// 注意这里**只删本快照确实覆盖到的账号**的旧 key：调用方各自取快照
	// （账号页 / 监控 / 概览），若按「本次没看到就删」，一个稍旧的快照就能
	// 把另一个请求刚登记的告警删掉，since 会被重置成「刚刚」。
	covered := make(map[string]bool, len(states))
	for _, st := range states {
		covered[st.ID] = true
	}
	for key := range t.since {
		id := key
		if i := strings.IndexByte(key, 0); i >= 0 {
			id = key[:i]
		}
		if !seen[key] && covered[id] {
			delete(t.since, key)
		}
	}
	return out
}

// sortAlerts 统一排序：严重度优先，同级按出现时间倒序，再按账号名稳定排序。
//
// 为什么不纯按时间：一条三天前的「账号失效」比一分钟前的「暂停」更该被先看到 ——
// 前者是号死了，后者只是一个开关。
func sortAlerts(out []AccountAlert) {
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := alertSeverityOrder[out[i].Severity], alertSeverityOrder[out[j].Severity]
		if ri != rj {
			return ri < rj
		}
		if out[i].Since != out[j].Since {
			return out[i].Since > out[j].Since
		}
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].State < out[j].State
	})
}

// alertSummary 是告警列表的汇总（面板顶栏与总览用）。
type alertSummary struct {
	Total int `json:"total"`
	// Urgent 是「必须马上处理」的条数（账号失效 / 令牌过期 / 额度用尽）。
	Urgent int `json:"urgent"`
	// AffectedAccounts 是涉及的账号数 —— 一个号同时失效又过期只算一个。
	AffectedAccounts int `json:"affected_accounts"`
}

func summarizeAlerts(list []AccountAlert) alertSummary {
	sum := alertSummary{Total: len(list)}
	accounts := map[string]bool{}
	for _, a := range list {
		accounts[a.Account] = true
		if a.Severity == "urgent" || a.Severity == "important" {
			sum.Urgent++
		}
	}
	sum.AffectedAccounts = len(accounts)
	return sum
}

// alertList 返回全池告警（按严重度排序），并顺带维护首次出现时刻。
//
// 所有对外出口（/panel/api/alerts、账号列表、监控页）都走这里，
// 保证同一个请求里看到的 Since 一致、且台账只被推进一次。
func (s *Server) alertListAt(now time.Time) []AccountAlert {
	return s.alertList(s.pool.Snapshot(), now)
}

// AlertList 是全池告警的对外入口（监控页与账号页之外的调用点用）。
func (s *Server) AlertList() []AccountAlert {
	return s.alertListAt(time.Now())
}

func (s *Server) alertList(states []pool.AccountState, now time.Time) []AccountAlert {
	if s.alerts == nil {
		return nil
	}
	return s.alerts.observe(states, now)
}

// handlePanelAlerts 返回全池账号告警（GET /panel/api/alerts）。
//
// 单独一个端点而不是只塞进 overview：告警要能按需刷新（账号页 5 分钟轮询），
// 而 overview 带的是账号汇总，轮询它会连带重算模型目录等无关数据。
func (s *Server) handlePanelAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	list := s.AlertList()
	writeJSON(w, http.StatusOK, map[string]any{
		"alerts":  list,
		"summary": summarizeAlerts(list),
	})
}
