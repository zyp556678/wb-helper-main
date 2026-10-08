package tasks

// 本文件是「复杂动作」的实现：专家召唤链、真实对话、领养链、小程序口径任务。
//
// 与 tasks.go 里那批「只发一条上报」的动作相比，这些动作都是**多步链路**，
// 每步都有前置依赖（真实 chat 拿服务端 requestId、accept 登记生效后才上报……），
// 所以单独放一个文件，逐条写清依据与失败语义。
//
// 所有动作的共同约定：
//   - 幂等：已 claimed / 已达标的任务直接跳过，不重复消耗上游配额；
//   - 失败不掩盖：中途失败返回可读原因，由调用方决定重试；
//   - 不猜：上游没证实的形状不写进来（宁可不做，也不白打一堆无效请求）。

import (
	"context"
	"fmt"
	"time"

	"workbuddy-gateway/internal/upstream"
)

// expertSummonGap 是专家召唤链之间的间隔（真实使用节奏）。
const expertSummonGap = 6 * time.Second

// mpActionGap 是小程序任务写动作之间的间隔（accept / 上报 / 领奖，防频控）。
const mpActionGap = 2 * time.Second

// -----------------------------------------------------------------------------
// 专家系：expert_5 / Expert_team_use_3 / Expert_lighthouse
// -----------------------------------------------------------------------------

// runExpertBatch 对 count 位**市场真实专家**完成「召唤 + 真实对话 + 使用」链。
//
// 判据是 expert_actual_use，它要求 id 真实存在、requestId 是真实 chat 的服务端 id，
// 所以链路固定：市场列表 → 召唤链 → 真实 chat 取 requestId → 使用事件。
// 单个专家失败不影响后续（逐个继续），最后汇报成功数与失败数。
func runExpertBatch(ctx context.Context, m *Manager, tg target, t *upstream.GrowthTask, expertType string, count int64) (string, error) {
	if count <= 0 {
		return "进度已达标，无需召唤", nil
	}
	experts, err := m.client.MarketExpertList(ctx, tg.Cred, tg.Prof, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家市场列表失败: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空（类型 %s）", expertType)
	}
	ok, fail := int64(0), 0
	for i, e := range experts {
		if ok >= count {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick,
			upstream.DesktopExpertSummonSequence(e)...); err != nil {
			fail++
			m.logf("[任务] 专家召唤链上报失败（%s）: %v", e.ExpertID, err)
			continue
		}
		conv, req, cerr := m.client.DesktopChatWithExpert(ctx, tg.Cred, tg.Prof, e.ExpertID)
		if cerr != nil {
			fail++
			m.logf("[任务] 专家真实对话失败（%s）: %v", e.ExpertID, cerr)
			continue
		}
		events := append(
			upstream.DesktopChatSequence(conv, req, "msg-"+tail(req, 8), "fast-model", "fast-model"),
			upstream.DesktopExpertActualUseEvent(e, conv, req))
		if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick, events...); err != nil {
			fail++
			m.logf("[任务] 专家使用事件上报失败（%s）: %v", e.ExpertID, err)
			continue
		}
		ok++
		if ok < count && i < len(experts)-1 {
			if !sleepCtx(ctx, expertSummonGap) {
				break
			}
		}
	}
	msg := fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s）", ok, expertType)
	if fail > 0 {
		msg += fmt.Sprintf("，%d 位失败", fail)
	}
	return msg, nil
}

// lighthouseExpertID 是「腾讯轻量云专家」（Expert_lighthouse 的固定判据专家）。
const lighthouseExpertID = "ex_2cvvUZQhDyeJ"

// runExpertLighthouse 完成 Expert_lighthouse：轻量云专家的召唤 + 使用链。
//
// 与 expert_5 同构，三处差异（对齐真实样本）：
//   - 专家 id 固定为轻量云专家；
//   - chat 链的 agent_task_created 要带 has_expert=true + expert_id（默认是 false）；
//   - expert_actual_use 的 mode=LOCAL、type 为空、cost=0。
func runExpertLighthouse(ctx context.Context, m *Manager, tg target, _ *upstream.GrowthTask) (string, error) {
	lh := upstream.MarketExpert{
		ExpertID: lighthouseExpertID, ExpertType: "agent",
		DisplayNameZH: "腾讯轻量云专家", ProfessionZH: "腾讯轻量云专家", Version: "1.0.2",
	}
	// 市场列表命中真实条目时以服务端信息为准（版本等）。
	if experts, err := m.client.MarketExpertList(ctx, tg.Cred, tg.Prof, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == lighthouseExpertID {
				lh = e
				break
			}
		}
	}
	if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick,
		upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链上报失败: %w", err)
	}
	conv, req, err := m.client.DesktopChatWithExpert(ctx, tg.Cred, tg.Prof, lh.ExpertID)
	if err != nil {
		return "", fmt.Errorf("真实对话失败: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+tail(req, 8), "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	use := upstream.DesktopExpertActualUseLocal(lh, conv, req)
	// 对齐真实样本：轻量云专家 actual_use 的 type 为空、cost=0。
	use["type"] = ""
	use["cost"] = 0
	events = append(events, use)
	if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick, events...); err != nil {
		return "", fmt.Errorf("使用事件上报失败: %w", err)
	}
	return "已上报轻量云专家召唤+使用链（真实对话 requestId）", nil
}

// skill_1 的判据载体（技能名/id/版本取自真实抓包样本；上游不校验技能是否真被使用，
// 但要求 skill_info 事件 JOIN 一次真实会话）。
const (
	skillProbeID      = "skill_2097350077599879168"
	skillProbeName    = "润泽小馆·日报撰写"
	skillProbeVersion = "1.0.0"
)

// runSkillFresh 完成 skill_1：真实对话 + skill_info 技能加载事件。
func runSkillFresh(ctx context.Context, m *Manager, tg target, _ *upstream.GrowthTask) (string, error) {
	conv, req, err := m.client.DesktopChatWithExpert(ctx, tg.Cred, tg.Prof, "")
	if err != nil {
		return "", fmt.Errorf("真实对话失败: %w", err)
	}
	events := upstream.DesktopSkillUseSequence(conv, req, skillProbeID, skillProbeName, skillProbeVersion)
	if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件上报失败: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// -----------------------------------------------------------------------------
// 真实对话类：Model_chat_GLM5.2 / RichMeow_Chat / black_cat
// -----------------------------------------------------------------------------

// runModelChat 完成「用指定模型真实对话一次 + 按该模型上报」。
//
// 与纯上报的差别是**先真的发一次对话**：这类任务的判据里既有 chat 事件，
// 也看这次对话本身。accept 失败不阻塞（行为事件才是进度判据）。
func runModelChat(ctx context.Context, m *Manager, tg target, code, modelID, modelName string) (string, error) {
	if err := m.client.AcceptGrowthTasks(ctx, tg.Cred, tg.Prof, []string{code}, false); err != nil {
		m.logf("[任务] %s 报名失败（继续走行为链路）: %v", code, err)
	}
	if !sleepCtx(ctx, actionGap) {
		return "已取消", nil
	}
	if err := m.client.RunModelChat(ctx, tg.Cred, tg.Prof, modelID, modelName); err != nil {
		return "对话或上报失败：" + err.Error(), nil
	}
	return fmt.Sprintf("已完成 %s 真实对话并按模型上报", modelName), nil
}

// runBlackCat 完成夜猫子：夜间窗口内补足 glm-5.2 真实对话次数。
//
// 窗口外不做（行为不计分），返回提示由排程在 23 点后自动补足。
func runBlackCat(ctx context.Context, m *Manager, tg target, _ *upstream.GrowthTask) (string, error) {
	// 暂停选号的账号不出对话流量 —— 而夜猫子是全部任务里**唯一**"整任务都是真实
	// 模型对话"的（RunNightChats 逐条发 glm-5.2 短对话），与"让位防风控"正面冲突。
	// 其余 RPC 类任务（签到 / 活跃上报 / 旅行 / 成长）对暂停号照常执行，
	// 所以门控只加在这里，不加在 targets() 上。
	if tg.Paused {
		return "该账号已暂停选号，夜猫子任务跳过（它会产生真实对话流量）；其余维护任务照常", nil
	}
	if !upstream.InNightWindow(time.Now()) {
		return "当前不在 23:00-08:00 计数窗口，行为不计分；网关会在每日 23 点后自动补足", nil
	}
	need, err := m.client.BlackcatNeed(ctx, tg.Cred, tg.Prof)
	if err != nil {
		return "", err
	}
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	ok, err := m.client.RunNightChats(ctx, tg.Cred, tg.Prof, int(need))
	if err != nil {
		return fmt.Sprintf("已完成 %d/%d 次夜间对话后中断：%v", ok, need, err), nil
	}
	return fmt.Sprintf("已完成 %d 次夜间真实对话并上报", ok), nil
}

// runRichMeow 完成 RichMeow_Chat（桌面端对话 1 次）。
//
// 判据是**桌面指纹的完整对话事件链**（agent_task_created → chat_message_response
// 等 6 条），参考实现三账号实测纯 API 可点亮。与 modeReport 那条「只发一条活跃上报」
// 不是同一个东西 —— 这条链才是桌面端任务认的形状。
func runRichMeow(ctx context.Context, m *Manager, tg target, _ *upstream.GrowthTask) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wbgw-rm-%d", ms)
	req := fmt.Sprintf("wbgw-rm-req-%d", ms)
	events := upstream.DesktopChatSequence(conv, req, fmt.Sprintf("req-%d-user", ms), "fast-model", "fast-model")
	if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick, events...); err != nil {
		return "", err
	}
	return "已按桌面端指纹上报完整对话事件链", nil
}

// -----------------------------------------------------------------------------
// 领养链：first_buddy
// -----------------------------------------------------------------------------

// runFirstBuddy 完成领养：上报（解锁前置）→ 同意协议 → 领取第一只 Buddy。
//
// 顺序是硬要求：未上报时 buddy/first 直接 400「first_buddy task not completed yet」。
// 门槛未过是**预期结果**（上游要求当日活跃），返回可读提示而不是错误。
func runFirstBuddy(ctx context.Context, m *Manager, tg target, _ *upstream.GrowthTask) (string, error) {
	conv := fmt.Sprintf("wbgw-adopt-%d", time.Now().UnixMilli())
	if err := m.client.ReportChatActivity(ctx, tg.Cred, tg.Prof, conv, "", "", ""); err != nil {
		return "", fmt.Errorf("领养前置上报失败: %w", err)
	}
	// 给上游事件处理留时间（参考实现脚本实测口径）。
	if !sleepCtx(ctx, actionGap) {
		return "已取消", nil
	}
	if err := m.client.BuddyAgreement(ctx, tg.Cred, tg.Prof); err != nil {
		return "", fmt.Errorf("同意领养协议失败: %w", err)
	}
	if err := m.client.BuddyFirst(ctx, tg.Cred, tg.Prof); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），稍后会再试", nil
		}
		return "", fmt.Errorf("领取 Buddy 失败: %w", err)
	}
	return "已领取第一只 Buddy（+300 积分 +8 能量）", nil
}

// -----------------------------------------------------------------------------
// 小程序口径任务
// -----------------------------------------------------------------------------

// mpTaskCodes 是**小程序口径专属**下发的成长任务：默认（无 mp 头）列表不出现，
// accept / claim 也要求 X-Client-Platform: miniprogram。
//
// 新任务出现时在此登记。Sequential_Tasks_4..7 的存在性已实测（accept 返回
// 链式依赖错误），判据形状按小程序源码预置，解锁后逐个校正。
var mpTaskCodes = map[string]bool{
	"school_season":      true, // 校园日（mini chat + activityId）
	"Sequential_Tasks_1": true, // 小程序首对话
	"Sequential_Tasks_2": true, // 小程序选中专家并完成有效对话
	"Sequential_Tasks_3": true, // 小程序完成 5 次对话
	"Sequential_Tasks_4": true, // 小程序创建定时任务
	"Sequential_Tasks_5": true, // 小程序使用 GLM5.2
	"Sequential_Tasks_6": true, // 小程序完成 10 次对话
	"Sequential_Tasks_7": true, // 体验灵感功能
}

// IsMPTaskCode 报告任务是否小程序口径专属（决定回读/接受/领奖走 mp 变体）。
func IsMPTaskCode(code string) bool { return mpTaskCodes[code] }

// acceptWithVerifyMP 接受小程序任务并**回读验证登记生效**。
//
// 上游存在「200 + OK 但 accept 没真正落账」的形态：此时后续上报的事件全部不归账，
// 任务永远点不亮。所以判定以回读 accept_status 为准，未生效重试一次。
func acceptWithVerifyMP(ctx context.Context, m *Manager, tg target, code string) bool {
	for attempt := 1; attempt <= 2; attempt++ {
		if err := m.client.AcceptGrowthTasks(ctx, tg.Cred, tg.Prof, []string{code}, true); err != nil {
			m.logf("[任务] %s 小程序报名失败（第 %d 次）: %v", code, attempt, err)
			continue
		}
		if !sleepCtx(ctx, mpActionGap) {
			return false
		}
		t, err := m.taskByCodeMP(ctx, tg, code)
		if err == nil && t != nil && t.AcceptStatus != "" && t.AcceptStatus != "not_accepted" {
			return true
		}
		m.logf("[任务] %s 小程序报名第 %d 次未登记生效（回读=%s）", code, attempt, acceptStatusOf(t))
	}
	return false
}

// acceptStatusOf 安全读取任务接受状态（nil 任务返回 "?"）。
func acceptStatusOf(t *upstream.GrowthTask) string {
	if t == nil || t.AcceptStatus == "" {
		return "?"
	}
	return t.AcceptStatus
}

// runMPMiniChatTask 是小程序「对话类」任务的通用闭环：
// mp 查询 → accept（带登记回读验证）→ 按差额补 mini chat 事件 → 回读 → 达标领奖。
//
// withActivityId 决定事件是否带开学季 activityId：school_season 必带，
// Sequential_Tasks_1/3/6 不带（服务端按 source=mini_program 指纹关联）。
func runMPMiniChatTask(ctx context.Context, m *Manager, tg target, code string, withActivityID bool) (string, error) {
	t, err := m.taskByCodeMP(ctx, tg, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "小程序口径未下发该任务（活动可能已结束或前置未完成）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := m.client.ClaimGrowthTask(ctx, tg.Cred, tg.Prof, code, true)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%d 积分 +%d 能量）", credit, energy), nil
	}
	if t.AcceptStatus == "" || t.AcceptStatus == "not_accepted" {
		if !acceptWithVerifyMP(ctx, m, tg, code) {
			return "报名未登记生效（上游返回成功但未落账），待下次重试", nil
		}
		// accept 前的任务进度为 null（target 下发 0），兜底 target=1 会少报 ——
		// 接受后必须回读一次拿真实 target/current，否则会「补 1 条就去领奖」。
		if t2, e := m.taskByCodeMP(ctx, tg, code); e == nil && t2 != nil {
			t = t2
			if t.Target > 0 {
				target = t.Target
			}
		}
	}
	need := target - t.Current
	if need <= 0 {
		need = 1
	}
	// 每条（**含首条**）都先等真人节奏：连发会被上游反作弊判无效并回滚进度。
	for i := int64(0); i < need; i++ {
		if !sleepCtx(ctx, chatEventPause()) {
			return fmt.Sprintf("已上报 %d/%d 条后被取消", i, need), nil
		}
		conv := fmt.Sprintf("wbgw-mp-%d-%d", time.Now().UnixMilli(), i)
		var ev map[string]any
		if withActivityID {
			ev = upstream.SchoolSeasonChatEvent(conv)
		} else {
			ev = upstream.SchoolChatTimesEvents(conv)
		}
		if err := m.client.ReportMPEvent(ctx, tg.Cred, tg.Prof, tg.Nick, ev); err != nil {
			return fmt.Sprintf("已上报 %d/%d 条后中断：%v", i, need, err), nil
		}
	}
	// 回读（异步计分，两轮各隔 3 秒）。
	if t2, e := m.taskByCodeWaitingMP(ctx, tg, code); e == nil && t2 != nil {
		t = t2
	}
	if t.Claimed {
		return "本轮已入账（已领取）", nil
	}
	if t.Current < target {
		return fmt.Sprintf("已上报 %d 次但进度未达 %d/%d（异步计分未归账，下次重试）", need, t.Current, target), nil
	}
	credit, energy, err := m.client.ClaimGrowthTask(ctx, tg.Cred, tg.Prof, code, true)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%d 积分 +%d 能量）", credit, energy), nil
}

// runMPEventTask 是 Sequential 链里「事件类」任务的通用骨架：
// mp 查询 → accept（带验证）→ 判据事件上报（primary；未点亮且有 fallback 时补一轮）
// → 回读 → 达标领奖。
//
// 每日零点解锁一环：锁定期间 accept 不落账，返回「等下次调度」而不是错误。
func runMPEventTask(ctx context.Context, m *Manager, tg target, code string,
	primary, fallback func() error) (string, error) {
	t, err := m.taskByCodeMP(ctx, tg, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "小程序口径未下发该任务（前置任务未完成或活动未开始）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := m.client.ClaimGrowthTask(ctx, tg.Cred, tg.Prof, code, true)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%d 积分 +%d 能量）", credit, energy), nil
	}
	if t.AcceptStatus == "" || t.AcceptStatus == "not_accepted" {
		if !acceptWithVerifyMP(ctx, m, tg, code) {
			return "报名未登记生效（任务可能处于每日锁定窗口，解锁后会自动重试）", nil
		}
	}
	if err := primary(); err != nil {
		return fmt.Sprintf("判据上报失败：%v", err), nil
	}
	for round := 0; round < 2; round++ {
		if !sleepCtx(ctx, claimPollGap) {
			return "已取消", nil
		}
		t2, err2 := m.taskByCodeMP(ctx, tg, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
		if round == 0 && fallback != nil {
			if err := fallback(); err != nil {
				return fmt.Sprintf("备选判据上报失败：%v", err), nil
			}
		}
	}
	if t.Claimed {
		return "本轮已入账（已领取）", nil
	}
	if t.Current < target {
		return "已上报但进度未点亮（判据形态待解锁后校正，下次重试）", nil
	}
	credit, energy, err := m.client.ClaimGrowthTask(ctx, tg.Cred, tg.Prof, code, true)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%d 积分 +%d 能量）", credit, energy), nil
}

// runMPExpertTask 完成 Sequential_Tasks_2「在小程序内选中专家并完成有效对话」。
//
// 判据是 mp 指纹的 expert_actual_use，专家 id 必须是市场真实 ex_ id。
// **专家列表在 accept 之前解析**：拉不到就整条任务不动作，
// 避免留下「已登记但没上报」的半程态（上游的 ids 前置判定同款）。
func runMPExpertTask(ctx context.Context, m *Manager, tg target, code string) (string, error) {
	t, err := m.taskByCodeMP(ctx, tg, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "小程序口径未下发该任务（活动可能已结束）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := m.client.ClaimGrowthTask(ctx, tg.Cred, tg.Prof, code, true)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%d 积分 +%d 能量）", credit, energy), nil
	}
	experts, merr := m.client.MarketExpertList(ctx, tg.Cred, tg.Prof, "")
	if merr != nil || len(experts) == 0 {
		return fmt.Sprintf("专家市场不可用（%v），本次跳过以免留下半程状态", merr), nil
	}
	e := experts[0]
	if t.AcceptStatus == "" || t.AcceptStatus == "not_accepted" {
		if !acceptWithVerifyMP(ctx, m, tg, code) {
			return "报名未登记生效（上游返回成功但未落账），待下次重试", nil
		}
	}
	if err := m.client.ReportMPEvent(ctx, tg.Cred, tg.Prof, tg.Nick,
		upstream.MiniExpertUseEvent(e.ExpertID, e.DisplayNameZH, e.ExpertType)); err != nil {
		return fmt.Sprintf("上报 expert_actual_use 失败：%v", err), nil
	}
	if t2, e2 := m.taskByCodeWaitingMP(ctx, tg, code); e2 == nil && t2 != nil {
		t = t2
	}
	if t.Claimed {
		return "本轮已入账（已领取）", nil
	}
	if t.Current < target {
		return "已上报但进度未归账（异步计分，下次重试）", nil
	}
	credit, energy, err := m.client.ClaimGrowthTask(ctx, tg.Cred, tg.Prof, code, true)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%d 积分 +%d 能量）", credit, energy), nil
}

// tail 取字符串末尾 n 个字符（构造 messageId 用）。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// mpChatModelID / mpChatModelName 是 Sequential_Tasks_5「使用 GLM5.2」的模型字段。
const (
	mpChatModelID   = "glm-5.2"
	mpChatModelName = "GLM-5.2"
)

// mpPlaybookCaseID / Name 是 Sequential_Tasks_7 用的灵感案例（形状取自真实样本）。
const (
	mpPlaybookCaseID   = "pm-gtm-launch-plan"
	mpPlaybookCaseName = "新产品上市 GTM 发布计划一页纸"
)
