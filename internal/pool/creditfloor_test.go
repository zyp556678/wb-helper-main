package pool

import (
	"testing"

	"workbuddy-gateway/internal/auth"
)

// 积分保底（credit_floor）的语义：账号余额**已实测**低于阈值时，
// 不再让它承接**实测收费**的模型 —— 防止收费请求把最后一点余额打穿，
// 之后连免费模型都被 402 冷却到次日签到。
//
// 这组用例重点钉住**三条不该拦的边界**：少任何一条，保底都会从
// 「保护余额」变成「让服务不可用」。

// setCredits 把一个账号的余额标成「已实测」的给定值。
//
// 必须同时置 quotaKnown：只看数值的话，「从未查过余额」与「余额真的是 0」
// 无法区分 —— 而保底正是靠这个区分才敢启用（见下一条用例）。
func setCredits(a *Account, remaining float64) {
	a.mu.Lock()
	a.quotaKnown = true
	a.quotaRemaining = remaining
	a.mu.Unlock()
}

// 触底 + 实测收费 → 拦下。
func TestCreditFloorBlocksPaidSiteBelowFloor(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	p.SetCreditFloor(100)
	acc := p.accounts[0]
	setCredits(acc, 30)

	paid := map[string]bool{auth.SiteCN: true}
	if _, err := p.PickAccount(PickOptions{Model: "m", PaidSitesForModel: paid}); err == nil {
		t.Fatal("余额低于保底且该站收费，应选不到号")
	}
	if n := p.AccountsBlockedByCreditFloor(paid, "m"); n != 1 {
		t.Fatalf("应统计出 1 个被保底拦下的账号，实际 %d", n)
	}
}

// 触底但对**免费/无观测**的站点不拦：这是保底能安全启用的前提。
//
// 若连没结论的站点也拦，账本被清空（重启 / 缓存过期）后触底账号会被永久锁死：
// 它拿不到任何请求，就永远没机会证明这个模型其实是免费的。
func TestCreditFloorDoesNotBlockUnlistedSites(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	p.SetCreditFloor(100)
	setCredits(p.accounts[0], 30) // 触底

	// paidSites 为空（该模型在这站没有收费结论）。
	acc, err := p.PickAccount(PickOptions{Model: "m", PaidSitesForModel: nil})
	if err != nil {
		t.Fatalf("没有收费结论的模型不该被保底拦住: %v", err)
	}
	acc.ReleaseInFlight()

	// paidSites 明确不含该账号所在的站点，同样放行。
	acc2, err := p.PickAccount(PickOptions{Model: "m", PaidSitesForModel: map[string]bool{auth.SiteINTL: true}})
	if err != nil {
		t.Fatalf("保底只对列出的站点生效，不该波及别的站点: %v", err)
	}
	acc2.ReleaseInFlight()
}

// **余额从未查过时不拦** —— 这条是保底最危险的边界。
//
// quotaRemaining 在「从未查过余额」与「余额真的是 0」两种情况下都是 0。
// 不加区分的话，进程刚启动、首次刷新余额之前，全池账号都会被判成触底，
// 表现为「一启动就全部 503」，而原因跟余额毫无关系。
func TestCreditFloorDoesNotBlockUnknownCredits(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	p.SetCreditFloor(100)
	// 刻意**不**调用 setCredits：quotaKnown 仍为 false。

	paid := map[string]bool{auth.SiteCN: true}
	acc, err := p.PickAccount(PickOptions{Model: "m", PaidSitesForModel: paid})
	if err != nil {
		t.Fatalf("余额未观测时不该被保底拦住（否则启动即全池 503）: %v", err)
	}
	acc.ReleaseInFlight()
	if n := p.AccountsBlockedByCreditFloor(paid, "m"); n != 0 {
		t.Fatalf("未观测的账号不该计入被拦数，实际 %d", n)
	}
}

// 阈值 0 = 关闭：行为与引入保底之前完全一致。
func TestCreditFloorZeroDisablesCheck(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	p.SetCreditFloor(0)
	setCredits(p.accounts[0], 0)

	paid := map[string]bool{auth.SiteCN: true}
	acc, err := p.PickAccount(PickOptions{Model: "m", PaidSitesForModel: paid})
	if err != nil {
		t.Fatalf("阈值为 0 时应完全关闭保底: %v", err)
	}
	acc.ReleaseInFlight()
}

// 负值归一成 0（关闭）而不是报错：它来自用户手写的 config.json。
func TestSetCreditFloorClampsNegative(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	p.SetCreditFloor(-5)
	if p.CreditFloor() != 0 {
		t.Fatalf("负值应钳成 0，实际 %v", p.CreditFloor())
	}
}

// 余额回血越过阈值后自动恢复，不需要任何复位动作。
//
// 这对应真实场景：签到当天回血 → 收费模型立刻可用。
func TestCreditFloorRecoversAfterTopUp(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	p.SetCreditFloor(100)
	acc := p.accounts[0]
	setCredits(acc, 30)

	paid := map[string]bool{auth.SiteCN: true}
	if _, err := p.PickAccount(PickOptions{Model: "m", PaidSitesForModel: paid}); err == nil {
		t.Fatal("触底时应被拦住")
	}

	// 签到回血：余额刷新到阈值之上。
	setCredits(acc, 500)
	got, err := p.PickAccount(PickOptions{Model: "m", PaidSitesForModel: paid})
	if err != nil {
		t.Fatalf("回血后应自动放行: %v", err)
	}
	got.ReleaseInFlight()
}

// 全池触底 + 全收费 → 选号失败（硬语义：宁可 503 也不放行打穿保底）。
func TestCreditFloorAllBelowFails(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	p.SetCreditFloor(100)
	for _, a := range p.accounts {
		setCredits(a, 10)
	}
	paid := map[string]bool{auth.SiteCN: true}
	if _, err := p.PickAccount(PickOptions{Model: "m", PaidSitesForModel: paid}); err == nil {
		t.Fatal("全池触底时应选不到号")
	}
	if n := p.AccountsBlockedByCreditFloor(paid, "m"); n != 2 {
		t.Fatalf("应统计出 2 个被拦账号，实际 %d", n)
	}
}

// 已禁用 / 已被名单排除的账号不该计入「被保底拦下」的统计：
// 原因不同、处置不同，报错了会把人引向错误的排查方向。
func TestCreditFloorCountExcludesAlreadyUnavailable(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	p.SetCreditFloor(100)
	acc := p.accounts[0]
	setCredits(acc, 10)

	paid := map[string]bool{auth.SiteCN: true}
	if n := p.AccountsBlockedByCreditFloor(paid, "m"); n != 1 {
		t.Fatalf("正常应计 1，实际 %d", n)
	}
	if err := p.SetDisabled(acc, true, "手工禁用"); err != nil {
		t.Fatal(err)
	}
	if n := p.AccountsBlockedByCreditFloor(paid, "m"); n != 0 {
		t.Fatalf("已禁用账号不该计入保底统计，实际 %d", n)
	}
}
