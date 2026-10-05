package pool

import (
	"testing"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/upstream"
)

// newTestPool 构造一个不落盘的池，并塞入指定站点的账号。
func newTestPool(t *testing.T, sites ...string) *Pool {
	t.Helper()
	cfg := &config.Config{WorkDir: t.TempDir()}
	cfg.ApplyDefaults()
	p := New(cfg, &upstream.Client{})
	for i, site := range sites {
		cred := &auth.Credential{
			Path:        site + "-" + string(rune('a'+i)) + ".json",
			AccessToken: "token",
			ExpiresAt:   time.Now().Add(24 * time.Hour).Unix(),
		}
		if site == auth.SiteINTL {
			cred.Realm = "global"
		} else {
			cred.Edition = "cn"
		}
		if got := cred.Site(); got != site {
			t.Fatalf("凭据站点归一化失败：期望 %s 得到 %s", site, got)
		}
		acc := p.newAccount(cred)
		p.accounts = append(p.accounts, acc)
	}
	return p
}

// TestPreferredSitesRestrictsPool 验证「软优先」的第一半：优先站点内有可用账号时，
// 只从优先站点选号（否则双站倾斜就是空的）。
func TestPreferredSitesRestrictsPool(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteINTL)
	pref := map[string]bool{auth.SiteCN: true}

	for i := 0; i < 20; i++ {
		acc, err := p.PickAccount(PickOptions{PreferredSites: pref})
		if err != nil {
			t.Fatalf("第 %d 次选号失败: %v", i, err)
		}
		if acc.Site() != auth.SiteCN {
			t.Fatalf("第 %d 次选到 %s 站点，期望只在 cn 内选（软优先失效）", i, acc.Site())
		}
		acc.ReleaseInFlight()
	}
}

// TestPreferredSitesFallsBack 验证「软优先」的第二半：优先站点不可用时自动回落到全部站点。
//
// 这条比上一条更重要：倾斜只是省钱手段，不该在免费站没号时让请求直接失败。
func TestPreferredSitesFallsBack(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteINTL)
	// 把 cn 账号禁用（模拟免费站不可用）
	for _, a := range p.Accounts() {
		if a.Site() == auth.SiteCN {
			a.Disable("测试：模拟免费站不可用")
		}
	}
	pref := map[string]bool{auth.SiteCN: true}

	acc, err := p.PickAccount(PickOptions{PreferredSites: pref})
	if err != nil {
		t.Fatalf("优先站点不可用时应当回落到其它站点，却报错: %v", err)
	}
	if acc.Site() != auth.SiteINTL {
		t.Fatalf("期望回落到 intl，实际选到 %s", acc.Site())
	}
	acc.ReleaseInFlight()
}

// TestPreferredSitesReleasesInFlight 验证被倾斜筛掉的账号会归还在途名额。
//
// 这是实现里最容易漏的一处：候选是先占用在途名额再筛站点的，
// 漏掉归还会让名额被白占，几次之后该账号再也选不进来（表现为「号池莫名缩水」）。
func TestPreferredSitesReleasesInFlight(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteINTL)
	pref := map[string]bool{auth.SiteCN: true}

	acc, err := p.PickAccount(PickOptions{PreferredSites: pref})
	if err != nil {
		t.Fatalf("选号失败: %v", err)
	}
	if acc.InFlight() != 1 {
		t.Fatalf("被选中的账号在途数应为 1，实际 %d", acc.InFlight())
	}
	// 另一侧（intl）被优先规则筛掉，必须已经归还名额
	for _, a := range p.Accounts() {
		if a.Site() == auth.SiteINTL && a.InFlight() != 0 {
			t.Fatalf("被筛掉的 intl 账号在途数应为 0，实际 %d（名额泄漏）", a.InFlight())
		}
	}
	acc.ReleaseInFlight()
}

// TestPreferredSitesRoundRobin 验证轮询策略下同样生效（两轮各选一次 cn，且不选 intl）。
func TestPreferredSitesRoundRobin(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN, auth.SiteINTL)
	p.Strategy = "roundrobin"
	pref := map[string]bool{auth.SiteCN: true}

	for i := 0; i < 10; i++ {
		acc, err := p.PickAccount(PickOptions{PreferredSites: pref})
		if err != nil {
			t.Fatalf("第 %d 次选号失败: %v", i, err)
		}
		if acc.Site() != auth.SiteCN {
			t.Fatalf("轮询策略下第 %d 次选到 %s，期望只在 cn 内选", i, acc.Site())
		}
		acc.ReleaseInFlight()
	}
}

// TestNoPreferredSitesKeepsBoth 验证不传优先集合时双站都参与（不误伤默认行为）。
func TestNoPreferredSitesKeepsBoth(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteINTL)
	seen := map[string]bool{}
	for i := 0; i < 60; i++ {
		acc, err := p.PickAccount(PickOptions{})
		if err != nil {
			t.Fatalf("第 %d 次选号失败: %v", i, err)
		}
		seen[acc.Site()] = true
		acc.ReleaseInFlight()
	}
	if !seen[auth.SiteCN] || !seen[auth.SiteINTL] {
		t.Fatalf("未开启倾斜时两个站点都应被选到，实际 %v", seen)
	}
}
