package upstream

import (
	"testing"

	"workbuddy-gateway/internal/auth"
)

// 站点能力门控。
//
// 这两条钉住的是「协议相同、能力不同」这个事实：登录 / 对话 / 模型目录 /
// 额度汇总 / 官方用量两站同构，但签到与成长中心活动**只有国内站有**。
// 参考实现（wb-switch）把这两项写成 `WbVariant::supports_checkin` /
// `supports_travel`，并且在发请求前就跳过国际站账号 ——
// 不门控的后果不是报错，而是每个周期白发一次注定 404 的请求，
// 在日志里留下噪声、在面板上显示成红色故障。
func TestSiteCapabilities(t *testing.T) {
	cn := ProfileForSite(auth.SiteCN)
	intl := ProfileForSite(auth.SiteINTL)

	if !cn.SupportsCheckin() {
		t.Error("国内站应当支持签到")
	}
	if intl.SupportsCheckin() {
		t.Error("国际站没有签到接口，不该被判为支持")
	}
	if !cn.SupportsGrowthActivity() {
		t.Error("国内站应当支持成长中心活动")
	}
	if intl.SupportsGrowthActivity() {
		t.Error("国际站没有成长中心活动，不该被判为支持")
	}
}

// 未识别站点要回退到国内站（与 ProfileForSite 的既有约定一致）。
func TestSiteCapabilitiesFallback(t *testing.T) {
	unknown := ProfileForSite("something-else")
	if unknown.Key != auth.SiteCN {
		t.Fatalf("未知站点应回退国内站，实际 %q", unknown.Key)
	}
	if !unknown.SupportsCheckin() || !unknown.SupportsGrowthActivity() {
		t.Error("回退到国内站后应当具备国内站的全部能力")
	}
}

// nil Profile 不能 panic（调用点拿到的可能是未初始化的账号）。
func TestSiteCapabilitiesNilSafe(t *testing.T) {
	var p *Profile
	if p.SupportsCheckin() || p.SupportsGrowthActivity() {
		t.Error("nil Profile 应当判为不支持，而不是 panic 或误判为支持")
	}
	if GrowthSupported(nil) {
		t.Error("GrowthSupported(nil) 应当为 false")
	}
}

// GrowthSupported 必须与档位判定同源。
//
// 这条防的是它退回到「按域名子串判断」：域名是外部事实，上游换域就会静默失效，
// 而失效的表现是国际站账号又开始被打 404。
func TestGrowthSupportedMatchesProfileCapability(t *testing.T) {
	for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
		p := ProfileForSite(site)
		if got, want := GrowthSupported(p), p.SupportsGrowthActivity(); got != want {
			t.Errorf("站点 %s：GrowthSupported=%v，与 SupportsGrowthActivity=%v 不一致", site, got, want)
		}
	}
}
