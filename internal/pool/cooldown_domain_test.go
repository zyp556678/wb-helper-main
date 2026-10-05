package pool

import (
	"testing"
	"time"

	"workbuddy-gateway/internal/auth"
)

// 本文件守住「解冻不得越界」这条不变式。
//
// 背景（对照 wb2api-panel 的 fix 602ed1b）：上游曾在**余额刷新**路径上无条件清空
// 整个冷却域，把模型级台账（6004，对齐上游重置墙钟）与软限流退避一起抹掉。
// 余额刷新是周期性动作（每 5 分钟），于是限流冷却的实际寿命被压到一个刷新周期内 ——
// 撞限号被误判健康、重新选中、再撞 429，全池冷却保护形同虚设。
//
// 我们的实现本就按「只解冻余额类硬冷却」写的，但这条不变式此前**没有测试钉住**：
// 任何一次「顺手把冷却一起清了」的重构都会静默退化，且症状是「偶发 429」，
// 很难回溯到这里。所以下面几条的价值不在验证现状，而在防止将来改坏。

// TestReviveKeepsModelCooldowns 余额恢复解冻不得清模型级台账。
//
// 注意构造顺序：CooldownHard 会主动清空 modelCooldowns（账号级冷却时模型豁免表
// 必须作废，否则豁免会泄漏到账号级限流上），所以必须先设硬冷却、再设模型级冷却，
// 才能造出「两者并存」的局面 —— 这正是余额恢复时刻的真实状态。
func TestReviveKeepsModelCooldowns(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	acc := p.accounts[0]

	acc.CooldownHard("余额耗尽")
	acc.CooldownModel("glm-5.3", time.Hour, time.Time{}, "上游 6004 模型限流")

	if _, ok := acc.ModelCooldowns()["glm-5.3"]; !ok {
		t.Fatalf("前置条件不成立：模型级冷却未建立，%v", acc.ModelCooldowns())
	}

	if !acc.ReviveIfCreditsRecovered(100) {
		t.Fatal("余额恢复后应解冻硬冷却")
	}

	until, ok := acc.ModelCooldowns()["glm-5.3"]
	if !ok {
		t.Fatal("余额恢复解冻清掉了模型级台账——限流恢复证据是上游重置墙钟到期，不是余额恢复")
	}
	if rem := time.Until(until); rem < 55*time.Minute {
		t.Fatalf("模型级冷却被缩短：剩余 %v，应保持约 1 小时", rem)
	}
}

// TestReviveIsNoopForSoftCooldown 软限流冷却下，余额恢复解冻必须是完全 no-op。
//
// 语义：余额恢复只证明 **billing 通道**健康，不证明 **chat 通道**健康。
// 软限流是 chat 通道被限的信号，其恢复证据是上游重置墙钟/退避到期。
// 若这里能解冻，一次余额刷新就能把限流保护抹掉。
func TestReviveIsNoopForSoftCooldown(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	acc := p.accounts[0]

	acc.CooldownSoft(10*time.Minute, time.Time{}, "上游限流")
	acc.mu.Lock()
	acc.softStreak = 3 // 已累计到第 3 次，指数退避的进度
	acc.mu.Unlock()

	if acc.ReviveIfCreditsRecovered(100) {
		t.Fatal("只有软冷却时不应报告「解冻成功」——余额恢复不构成限流解除证据")
	}

	acc.mu.Lock()
	defer acc.mu.Unlock()
	if acc.cooldownUntil.IsZero() || !time.Now().Before(acc.cooldownUntil) {
		t.Fatalf("软限流冷却被解除了：coolKind=%s until=%v", acc.coolKind, acc.cooldownUntil)
	}
	if acc.softStreak != 3 {
		t.Fatalf("软限流退避计数被清零：%d，应保持 3（否则下次退避从头算起，保护被削弱）", acc.softStreak)
	}
}

// TestReviveNoopWhenNoCredits 余额没恢复时不做任何事。
func TestReviveNoopWhenNoCredits(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	acc := p.accounts[0]
	acc.CooldownHard("余额耗尽")

	if acc.ReviveIfCreditsRecovered(0) {
		t.Fatal("余额仍为 0 时不应解冻")
	}

	acc.mu.Lock()
	defer acc.mu.Unlock()
	if acc.cooldownUntil.IsZero() {
		t.Fatal("余额为 0 时硬冷却被清掉了")
	}
}

// TestClearFailureStateKeepsHardCooldown 成功清零瞬时信号，但不动余额类硬冷却与模型台账。
//
// 与上几条的分工：成功（ClearFailureState）是**请求通道**健康的证据，
// 因此可以清软冷却/熔断/退避；但余额耗尽要靠签到或额度恢复才能解，
// 模型级台账要靠上游重置墙钟到期才能解。
func TestClearFailureStateKeepsHardCooldown(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	acc := p.accounts[0]

	acc.CooldownHard("余额耗尽")
	acc.CooldownModel("glm-5.3", time.Hour, time.Time{}, "上游 6004 模型限流")
	acc.ClearFailureState()

	acc.mu.Lock()
	hardKept := acc.coolKind == CoolHard && !acc.cooldownUntil.IsZero()
	acc.mu.Unlock()
	if !hardKept {
		t.Fatal("成功不该清掉余额类硬冷却（那要等签到或额度恢复）")
	}

	if _, ok := acc.ModelCooldowns()["glm-5.3"]; !ok {
		t.Fatal("成功不该清掉模型级台账——它的恢复证据是上游重置墙钟")
	}
}

// TestAccountLevelCooldownDropsModelExemptions 反向守住：账号级冷却**必须**清空
// 模型豁免表。
//
// 这条与上面几条是同一个不变式的另一面，缺了它就会走向另一个极端 ——
// 为了「保住模型台账」而不清，会让上一次模型级限流的豁免泄漏到本次账号级限流上，
// 表现为「账号明明在冷却，某个模型却还能被选中」。
func TestAccountLevelCooldownDropsModelExemptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(a *Account)
	}{
		{"硬冷却", func(a *Account) { a.CooldownHard("余额耗尽") }},
		{"软冷却", func(a *Account) { a.CooldownSoft(time.Minute, time.Time{}, "上游限流") }},
		{"固定冷却", func(a *Account) { a.CooldownFixed(time.Minute, "上游 404") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPool(t, auth.SiteCN)
			acc := p.accounts[0]

			acc.CooldownModel("glm-5.3", time.Hour, time.Time{}, "上游 6004 模型限流")
			tc.set(acc)

			if n := len(acc.ModelCooldowns()); n != 0 {
				t.Fatalf("账号级冷却后模型豁免表应清空，实际剩 %d 条", n)
			}
		})
	}
}
