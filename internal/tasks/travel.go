// 猫猫旅行巡检（切片 18）。
//
// 这里放的是**一趟状态机**，不是「排程」——排程只管什么时候调用它，
// 怎么判断该派出还是该领奖归这里管，避免两处各写一套语义。
//
// 单账号单趟的判定顺序（与 wb2api-panel scheduler/travel.go 同口径）：
//
//	无猫          → 领养：report → agreement → buddy/first
//	状态 arrived  → 领奖（必须带 record_id）
//	状态 idle     → 派出（当日未达上限时）
//	状态 traveling→ 跳过（在途，等到站的那一趟再领）
//
// 为什么必须是「最多一个动作，不轮询不等待」：旅行一趟最短也有小时级时长，
// 原地等到达只会把排程线程占死；而每轮只推进一个状态，配合每天两个时点
// （默认 09 / 21）正好走完「出发 → 到站领奖」一轮。
package tasks

import (
	"context"
	"fmt"
	"time"

	"workbuddy-gateway/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：四个地点的收益/时长区间
	// 完全相同，没有最优解，随机选只会让日志更难比对。
	travelLocationID = 4

	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// 两处限速都是**包级变量**（不是常量）：测试要能把它置 0，
// 否则每跑一个用例都要真等几十秒。
var (
	// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。
	travelAccountDelay = 800 * time.Millisecond
	// adoptReportGap 领养前置上报后的等待：给上游事件处理留时间再发 buddy/first。
	// 1.05s 对齐参考实现实测的间隔口径；早了会拿到「任务未完成」的假失败。
	adoptReportGap = 1050 * time.Millisecond
)

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。
// 中国无夏令时，固定 +8 即可，不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// TravelInspect 对全部可用账号推进一趟旅行巡检，返回人类可读的汇总。
//
// 站点门控：旅行是成长域活动，只有国内站有。国际站账号**在发请求前**就跳过，
// 否则会拿到一串 404 把日志刷满，让人误以为是上游故障。
// 单个账号失败只跳过该账号本轮，不影响其余账号（也**不强刷 token** ——
// 令牌问题交给 22:00 的保活，在这里顺手刷新会让一次巡检打出两倍的请求）。
func (m *Manager) TravelInspect(ctx context.Context) (string, error) {
	list := m.targets(nil)
	if len(list) == 0 {
		return "", fmt.Errorf("没有可用的账号")
	}

	var adopt, depart, claim, skip, failed, gated int
	var firstErr string
	first := true

	for _, tg := range list {
		if ctx.Err() != nil {
			break
		}
		if !tg.Prof.SupportsGrowthActivity() {
			gated++
			continue
		}
		if !first {
			if !sleepCtx(ctx, travelAccountDelay) {
				break
			}
		}
		first = false

		action, err := m.travelOne(ctx, tg)
		if err != nil {
			failed++
			if firstErr == "" {
				firstErr = err.Error()
			}
			m.logf("[旅行] 账号 %s: %v", tg.Nick, err)
			continue
		}
		switch action {
		case travelAdoptAction:
			adopt++
		case travelDepartAction:
			depart++
		case travelClaimAction:
			claim++
		default:
			skip++
		}
	}

	detail := fmt.Sprintf("旅行巡检完成：领养 %d，出发 %d，领奖 %d，跳过 %d，失败 %d",
		adopt, depart, claim, skip, failed)
	if gated > 0 {
		detail += fmt.Sprintf("，跳过 %d（该站点无成长中心）", gated)
	}
	if firstErr != "" {
		detail += "；首个错误: " + firstErr
	}
	m.logf("[旅行] %s", detail)
	return detail, nil
}

// travelOne 单账号单趟：查有无猫 + 查状态 + 最多一个动作。
// 返回本次实际做的动作名（adopt / depart / claim / "" 表示跳过）。
func (m *Manager) travelOne(ctx context.Context, tg target) (string, error) {
	buddy, err := m.client.BuddyInfo(ctx, tg.Cred, tg.Prof)
	if err != nil {
		return "", fmt.Errorf("查询猫档案失败: %w", err)
	}
	if buddy == nil {
		if err := m.travelAdopt(ctx, tg); err != nil {
			return "", err
		}
		return travelAdoptAction, nil
	}

	st, err := m.client.TravelStatus(ctx, tg.Cred, tg.Prof)
	if err != nil {
		return "", fmt.Errorf("查询旅行状态失败: %w", err)
	}
	if st == nil {
		return "", nil
	}
	action, reason := decideTravel(st.State, st.DailyLimitReached, st.RecordID)
	if reason != "" {
		// 「已到站却缺 record_id」是上游数据异常：领奖一定失败，
		// 报出来而不是静默跳过 —— 静默会让这一趟永远领不到奖且无从察觉。
		return "", fmt.Errorf("%s", reason)
	}
	switch action {
	case travelClaimAction:
		credit, err := m.client.TravelClaim(ctx, tg.Cred, tg.Prof, st.RecordID)
		if err != nil {
			return "", fmt.Errorf("领奖失败: %w", err)
		}
		m.logf("[旅行] 账号 %s 领奖成功 +%d", tg.Nick, credit)
		return travelClaimAction, nil
	case travelDepartAction:
		if err := m.client.TravelDepart(ctx, tg.Cred, tg.Prof, travelLocationID); err != nil {
			return "", fmt.Errorf("出发失败: %w", err)
		}
		return travelDepartAction, nil
	default:
		return "", nil
	}
}

// 旅行状态机的动作取值（也是 TravelInspect 汇总时的分类键）。
const (
	travelSkipAction   = ""
	travelAdoptAction  = "adopt"
	travelDepartAction = "depart"
	travelClaimAction  = "claim"
)

// decideTravel 由「旅行状态」决定该做什么 —— **纯函数**。
//
// 抽出来的理由不只是为了好测：状态机的分支条件是「语义」而不是「实现细节」，
// 混在带网络调用的函数里时，任何一次重构都可能悄悄改掉某条分支的判断，
// 而唯一的回归信号是「某个账号不再领奖了」——那要等一整天才会被发现。
//
// 返回 (action, reason)：reason 非空表示这是一条**应该报出来的异常**，
// 而不是正常的「跳过」。
func decideTravel(state string, dailyLimitReached bool, recordID int64) (string, string) {
	switch state {
	case travelStateArrived:
		if recordID == 0 {
			return travelSkipAction, "已到站但缺少 record_id，无法领奖"
		}
		return travelClaimAction, ""
	case travelStateIdle:
		if dailyLimitReached {
			return travelSkipAction, "" // 今日已派出过，正常跳过
		}
		return travelDepartAction, ""
	default:
		// traveling / 未知状态：都不动。未知状态**不猜** ——
		// 猜错会发出一个上游不接受的动作，把一次巡检变成一条错误日志。
		return travelSkipAction, ""
	}
}

// travelAdopt 无猫时走领养链路：report → agreement → buddy/first。
//
// report 必须在前：一条 chat_request_send 上报会点亮连登并**解锁 first_buddy 任务**；
// 未上报时 buddy/first 直接返回 400「first_buddy task not completed yet」。
// 该门槛的真实来源是「当日无活跃上报」，不是账号问题。
//
// 门槛未达标属预期行为：记一次「当日已试」后**当日不再重试**，避免对上游刷请求。
func (m *Manager) travelAdopt(ctx context.Context, tg target) error {
	if m.adoptTriedToday(tg.UID) {
		return nil
	}

	// 前置：解锁 first_buddy 任务。失败不阻塞 —— 让 buddy/first 按既有错误路径暴露真实原因。
	cid := fmt.Sprintf("wbgw-adopt-%d", time.Now().UnixMilli())
	if err := m.client.ReportChatActivity(ctx, tg.Cred, tg.Prof, cid, "", "", ""); err != nil {
		m.logf("[旅行] 账号 %s 领养前置上报失败: %v", tg.Nick, err)
	} else if !sleepCtx(ctx, adoptReportGap) {
		return ctx.Err()
	}

	if err := m.client.BuddyAgreement(ctx, tg.Cred, tg.Prof); err != nil {
		return fmt.Errorf("同意领养协议失败: %w", err)
	}
	err := m.client.BuddyFirst(ctx, tg.Cred, tg.Prof)
	switch {
	case err == nil:
		m.logf("[旅行] 账号 %s 领养成功（+300 分）", tg.Nick)
		return nil
	case upstream.IsBuddyTaskIncomplete(err):
		m.markAdoptTried(tg.UID)
		m.logf("[旅行] 账号 %s 领养跳过（对话门槛未达，次日再试）", tg.Nick)
		return nil
	default:
		return fmt.Errorf("领养失败: %w", err)
	}
}

// -----------------------------------------------------------------------------
// 「当日已试过领养」记录
//
// 只放内存、不落盘：它唯一的用途是「今天别再打了」，
// 重启后多打一次 400 的代价远小于维护一份状态文件。
// -----------------------------------------------------------------------------

func (m *Manager) adoptTriedToday(uid string) bool {
	m.adoptMu.Lock()
	defer m.adoptMu.Unlock()
	return m.adoptTried[uid] == travelDay(time.Now())
}

func (m *Manager) markAdoptTried(uid string) {
	m.adoptMu.Lock()
	defer m.adoptMu.Unlock()
	if m.adoptTried == nil {
		m.adoptTried = map[string]string{}
	}
	m.adoptTried[uid] = travelDay(time.Now())
}
