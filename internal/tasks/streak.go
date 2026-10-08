package tasks

// -----------------------------------------------------------------------------
// 连登管家（对照 wb2api-panel 的 scheduler/streak.go）
//
// 机制：成长中心的连登档位（7d/14d/28d）按**连续登录天数**解锁，兑换发
// credit/energy/补签卡/**抽奖次数**；抽奖次数只能从兑换获得。兑换按钮在界面上恒可点，
// 但未解锁时服务端返回 403「连续登录天数不足」——所以放在每日签到之后跑一遍（幂等）：
// 到天数那天自动完成「兑换 → 抽奖」闭环，不需要人工盯着。
//
// 顺序（每一步都是幂等的，重复跑只会有一次真正生效）：
//
//	0.   补签保连登：昨日漏签且有补签卡就补上（连登一断就要重攒 7 天）
//	0.5  新手礼包 / 活动补偿（每号一次，已领过返回业务错误 → 静默跳过）
//	1.   兑换所有**已解锁且未领取**的档位
//	2.   按当前 chances 抽完所有奖
//
// 国际站账号不参与（没有 CN 的连登体系，发请求只会拿到业务错误）。
// -----------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"strings"
	"time"

	"workbuddy-gateway/internal/upstream"
)

// StreakBonusResult 是一次连登管家的逐账号结果（面板与日志共用）。
type StreakBonusResult struct {
	Account  string   `json:"account"`
	Makeup   string   `json:"makeup,omitempty"`
	Redeemed []string `json:"redeemed,omitempty"`
	Draws    int      `json:"draws,omitempty"`
	Prizes   []string `json:"prizes,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// RunStreakBonus 对全部可用账号执行连登管家（accountID 非空时只做那一个账号）。
func (m *Manager) RunStreakBonus(ctx context.Context, accountID string) (string, error) {
	var list []target
	if strings.TrimSpace(accountID) == "" {
		list = m.targets(nil)
	} else {
		list = m.targets([]string{accountID})
	}
	if len(list) == 0 {
		return "", fmt.Errorf("没有可用的账号")
	}
	var results []StreakBonusResult
	redeemed, draws := 0, 0
	for _, tg := range list {
		res := m.streakBonusAccount(ctx, tg)
		results = append(results, res)
		redeemed += len(res.Redeemed)
		draws += res.Draws
	}
	m.streakMu.Lock()
	m.lastStreakBonus = results
	m.streakMu.Unlock()
	summary := fmt.Sprintf("连登管家完成：兑换 %d 档、抽奖 %d 次", redeemed, draws)
	if redeemed == 0 && draws == 0 {
		summary = "连登管家完成：没有可兑换的档位或可用的抽奖次数"
	}
	return summary, nil
}

// StreakBonusResults 返回最近一次连登管家的逐账号结果（面板展示用）。
func (m *Manager) StreakBonusResults() []StreakBonusResult {
	m.streakMu.Lock()
	defer m.streakMu.Unlock()
	return m.lastStreakBonus
}

// streakBonusAccount 单账号：补签 → 礼包/补偿 → 兑换已解锁档位 → 抽完所有次数。
func (m *Manager) streakBonusAccount(ctx context.Context, tg target) StreakBonusResult {
	res := StreakBonusResult{Account: tg.Nick}
	// 国际站没有 CN 的连登体系（发请求只会拿到业务错误），直接跳过。
	if !upstream.GrowthAllowed(tg.Prof, tg.Cred) {
		res.Notes = append(res.Notes, "国际站账号不参与连登活动")
		return res
	}
	// 0. 补签保连登。
	res.Makeup = m.makeupYesterday(ctx, tg)

	// 0.5 礼包 / 补偿（每号一次；已领过是业务错误，静默跳过）。
	if credit, err := m.client.ClaimGift(ctx, tg.Cred, tg.Prof); err == nil && credit > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("新手礼包 +%d 积分", credit))
	}
	if credit, err := m.client.ClaimCompensation(ctx, tg.Cred, tg.Prof); err == nil && credit > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("活动补偿 +%d 积分", credit))
	}

	// 1. 兑换所有已解锁且未领取的档位。
	full, err := m.client.GrowthStreak(ctx, tg.Cred, tg.Prof)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	statuses := map[string]string{
		"7d":  full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	for _, tier := range full.RedemptionStatus.Tiers {
		status := statuses[tier.Tier]
		if status == "locked" || status == "claimed" {
			continue
		}
		if err := m.client.GrowthRedeemTier(ctx, tg.Cred, tg.Prof, tier.Tier); err != nil {
			// 未解锁（403）属预期，记在 notes 里而不是报错。
			res.Notes = append(res.Notes, fmt.Sprintf("兑换 %s 未成功：%v", tier.Tier, err))
			continue
		}
		res.Redeemed = append(res.Redeemed, fmt.Sprintf("%s（+%d 积分 +%d 能量 卡×%d 抽奖×%d）",
			tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances))
	}

	// 2. 抽完所有次数（兑换刚发的次数已在服务端累加）。
	summary, err := m.client.LotteryChances(ctx, tg.Cred, tg.Prof)
	if err != nil {
		res.Notes = append(res.Notes, "抽奖次数查询失败："+err.Error())
		return res
	}
	for i := 0; i < summary.Chances; i++ {
		raw, err := m.client.LotteryDraw(ctx, tg.Cred, tg.Prof)
		if err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("第 %d 抽失败：%v", i+1, err))
			return res
		}
		res.Draws++
		res.Prizes = append(res.Prizes, compactPrize(raw))
	}
	return res
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数）。
//
// 无卡 / 无漏签 / 查询失败一律静默返回空串 —— 这三件事都不影响主流程，
// 为它们报错只会让日志里全是噪音。
func (m *Manager) makeupYesterday(ctx context.Context, tg target) string {
	missed, err := m.client.HeatmapYesterdayMissed(ctx, tg.Cred, tg.Prof)
	if err != nil || !missed {
		return ""
	}
	full, err := m.client.GrowthStreak(ctx, tg.Cred, tg.Prof)
	if err != nil || full.MakeupCards.Balance <= 0 {
		return ""
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := m.client.UseMakeupCard(ctx, tg.Cred, tg.Prof, yesterday); err != nil {
		return fmt.Sprintf("补签 %s 失败：%v", yesterday, err)
	}
	return "已用补签卡补签 " + yesterday + "（保连登）"
}

// compactPrize 裁剪奖品载荷（日志单行可读）。
func compactPrize(raw []byte) string {
	s := string(raw)
	if len(s) > 220 {
		return s[:220] + "…"
	}
	return s
}

// StreakCheck 是「活跃上报后回读连登天数」的自检结果。
type StreakCheck struct {
	Days int
	// Suspect 为真表示「上报返回成功但连登天数仍是 0（或回读失败）」——
	// 上游可能静默丢弃了这次上报（缺字段时 200 但不计分）。
	Suspect bool
	Note    string
}

// CheckActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
//
// 背景：实测存在「上报 200 但静默丢弃」（缺 userId 时进度不动），
// 所以上报成功 ≠ 计分成功，需要回读验证闭环。GET 失败不影响主流程
// （上报本身已成功，且按天幂等，不做重试）。
func (m *Manager) CheckActivityStreak(ctx context.Context, tg target) StreakCheck {
	if !upstream.GrowthAllowed(tg.Prof, tg.Cred) {
		return StreakCheck{Note: "国际站账号无连登体系"}
	}
	full, err := m.client.GrowthStreak(ctx, tg.Cred, tg.Prof)
	if err != nil {
		return StreakCheck{Suspect: true, Note: "回读连登失败（上报本身已成功）：" + err.Error()}
	}
	days := full.Streak.Days
	if days == 0 {
		return StreakCheck{Days: 0, Suspect: true, Note: "上报返回成功但连登天数仍为 0（可能被上游静默丢弃）"}
	}
	return StreakCheck{Days: days}
}
