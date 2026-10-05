package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"workbuddy-gateway/internal/auth"
)

// req 构造一个带指定 header 的请求，便于表驱动测试。
//
// query 走 url.Values 编码，不手工拼字符串 —— 值里带空格（如 `" INTL "`）
// 时手拼会得到非法请求行，httptest.NewRequest 直接 panic。
func reqWith(query, header string) *http.Request {
	url := "/v1/chat/completions"
	if query != "" {
		url += "?" + query
	}
	r := httptest.NewRequest(http.MethodPost, url, nil)
	if header != "" {
		r.Header.Set("X-WB-Site", header)
	}
	return r
}

// reqSite 用 url.Values 安全地构造 `?site=` 请求。
func reqSite(site string) *http.Request {
	q := url.Values{}
	q.Set("site", site)
	return reqWith(q.Encode(), "")
}

// -----------------------------------------------------------------------------
// 模型名前缀
// -----------------------------------------------------------------------------

func TestSplitSitePrefix(t *testing.T) {
	cases := []struct {
		in       string
		wantSite string
		wantName string
		wantOK   bool
	}{
		{"AI-deepseek-v4.1-flash", auth.SiteINTL, "deepseek-v4.1-flash", true},
		{"CN-deepseek-v4.1-flash", auth.SiteCN, "deepseek-v4.1-flash", true},
		// 大小写不敏感：调用方手写前缀时不该栽在大小写上
		{"ai-gpt-5", auth.SiteINTL, "gpt-5", true},
		{"Cn-gpt-5", auth.SiteCN, "gpt-5", true},
		// 模型名本身含连字符是常态，只按**第一个**连字符切
		{"CN-glm-5.3-flashx", auth.SiteCN, "glm-5.3-flashx", true},
		// 模型名本身含斜杠是合法的（如 openai/gpt-4），前缀剥离后原样保留
		{"CN-openai/gpt-4", auth.SiteCN, "openai/gpt-4", true},
		// 无前缀
		{"deepseek-v4.1-flash", "", "deepseek-v4.1-flash", false},
		{"openai/gpt-4", "", "openai/gpt-4", false},
		// 前缀后为空：多半是写了一半，不认
		{"AI-", "", "AI-", false},
		{"cn-", "", "cn-", false},
		// 分隔符必须在。少打连字符时**必须不认**，否则 `cndeepseek-v4-flash`
		// 会被静默当成 `deepseek-v4-flash` 发出去 —— 用户少打一个字符，
		// 请求却「看起来成功」，只是打到了预期之外的站点。宁可当场报模型不存在。
		{"cndeepseek-v4-flash", "", "cndeepseek-v4-flash", false},
		{"aideepseek-v4-flash", "", "aideepseek-v4-flash", false},
		// 分隔符错了也不认：用 `_` 是在猜用户意图，猜错就是静默走错站。
		{"CN_deepseek", "", "CN_deepseek", false},
		{"CN/deepseek", "", "CN/deepseek", false},
		// 前缀必须在最前，不能出现在中间
		{"xCN-deepseek", "", "xCN-deepseek", false},
		// 空串
		{"", "", "", false},
		// 前缀是未知词，不能当成站点
		{"USA-gpt-4", "", "USA-gpt-4", false},
		// 前缀段必须与词表**完全相等**（不能用 HasPrefix）：
		// `cn2-xxx` / `aicloud-xxx` 会被 HasPrefix 误认成站点前缀。
		{"cn2-gpt-4", "", "cn2-gpt-4", false},
		{"aicloud-gpt-4", "", "aicloud-gpt-4", false},
	}
	for _, c := range cases {
		site, name, ok := splitSitePrefix(c.in)
		if ok != c.wantOK {
			t.Errorf("splitSitePrefix(%q) ok=%v，期望 %v", c.in, ok, c.wantOK)
			continue
		}
		if site != c.wantSite || name != c.wantName {
			t.Errorf("splitSitePrefix(%q) = (%q,%q)，期望 (%q,%q)",
				c.in, site, name, c.wantSite, c.wantName)
		}
	}
}

// -----------------------------------------------------------------------------
// 优先级
// -----------------------------------------------------------------------------

// URL 参数优先级最高，能覆盖请求头与模型名前缀。
func TestResolveSiteRouteQueryWinsOverEverything(t *testing.T) {
	r := reqWith("site=intl", "cn")
	got := resolveSiteRoute(r, "CN-gpt-4")
	if got.Site != auth.SiteINTL {
		t.Errorf("?site= 应最高优先，实际 site=%q source=%q", got.Site, got.Source)
	}
	if got.Source != "query" {
		t.Errorf("来源应为 query，实际 %q", got.Source)
	}
	// 站点由 query 指定时，模型名前缀**仍需剥离** —— 否则上游会收到带前缀的模型名。
	// 这是最容易漏的一条：只看站点对不对，忘了模型名还带着前缀。
	if got.Model != "gpt-4" {
		t.Errorf("模型名前缀应被剥离，实际 %q", got.Model)
	}
}

// 请求头次之，能覆盖模型名前缀。
func TestResolveSiteRouteHeaderWinsOverPrefix(t *testing.T) {
	r := reqWith("", "intl")
	got := resolveSiteRoute(r, "CN-gpt-4")
	if got.Site != auth.SiteINTL || got.Source != "header" {
		t.Errorf("请求头应覆盖前缀，实际 site=%q source=%q", got.Site, got.Source)
	}
	if got.Model != "gpt-4" {
		t.Errorf("模型名前缀应被剥离，实际 %q", got.Model)
	}
}

// 都没指定时用前缀。
func TestResolveSiteRouteUsesPrefixWhenNothingElse(t *testing.T) {
	got := resolveSiteRoute(reqWith("", ""), "AI-gpt-5")
	if got.Site != auth.SiteINTL || got.Source != "model_prefix" {
		t.Errorf("应使用前缀，实际 site=%q source=%q", got.Site, got.Source)
	}
	if got.Model != "gpt-5" {
		t.Errorf("模型名应为 gpt-5，实际 %q", got.Model)
	}
}

// 全都没指定时保持未指定，交回默认调度。
func TestResolveSiteRouteDefaultsToUnforced(t *testing.T) {
	got := resolveSiteRoute(reqWith("", ""), "deepseek-v4.1-flash")
	if got.Forced() {
		t.Errorf("未指定站点时不该是强制，实际 site=%q", got.Site)
	}
	if got.Model != "deepseek-v4.1-flash" {
		t.Errorf("模型名不该被改动，实际 %q", got.Model)
	}
}

// -----------------------------------------------------------------------------
// 归一化与容错
// -----------------------------------------------------------------------------

func TestResolveSiteRouteNormalizesAliases(t *testing.T) {
	cases := map[string]string{
		"intl":          auth.SiteINTL,
		"global":        auth.SiteINTL,
		"overseas":      auth.SiteINTL,
		"workbuddy.ai":  auth.SiteINTL,
		"codebuddy.ai":  auth.SiteINTL,
		"ai":            auth.SiteINTL,
		"cn":            auth.SiteCN,
		"china":         auth.SiteCN,
		"domestic":      auth.SiteCN,
		"mainland":      auth.SiteCN,
		"  INTL  ":      auth.SiteINTL, // 前后空白与大小写
		"International": auth.SiteINTL,
	}
	for raw, want := range cases {
		got := resolveSiteRoute(reqSite(raw), "gpt-4")
		if got.Site != want {
			t.Errorf("?site=%q 应归一化为 %q，实际 %q", raw, want, got.Site)
		}
	}
}

// 听不懂的站点值必须**忽略**，而不是当成国内站。
//
// 把打错的 `?site=usa` 理解成「走国内」是最坏的结果：用户以为自己在走国际站，
// 实际请求全打到了国内站，而界面上没有任何提示。
func TestResolveSiteRouteIgnoresUnknownSite(t *testing.T) {
	for _, bad := range []string{"usa", "eu", "jp", "south-korea"} {
		got := resolveSiteRoute(reqSite(bad), "gpt-4")
		if got.Site != "" {
			t.Errorf("未知站点 %q 应被忽略，实际被当成 %q", bad, got.Site)
		}
	}
}

// 未知站点值被忽略后，应继续往下找 —— 而不是整条链断掉。
func TestResolveSiteRouteFallsThroughAfterUnknownQuery(t *testing.T) {
	// query 听不懂 → 看 header
	got := resolveSiteRoute(reqWith("site=usa", "intl"), "gpt-4")
	if got.Site != auth.SiteINTL || got.Source != "header" {
		t.Errorf("query 无效时应继续用 header，实际 site=%q source=%q", got.Site, got.Source)
	}

	// query 与 header 都听不懂 → 看前缀
	got = resolveSiteRoute(reqWith("site=usa", "eu"), "CN-gpt-4")
	if got.Site != auth.SiteCN || got.Source != "model_prefix" {
		t.Errorf("两者都无效时应继续用前缀，实际 site=%q source=%q", got.Site, got.Source)
	}
}

// auto/default 等写法表示「显式要求用默认逻辑」，不该被当成没听懂而继续往下找。
//
// 差别在于：如果调用方写了 `?site=auto` 而模型名恰好是 `AI-gpt-4`，
// 我们**仍然**应该走前缀（因为 auto 只是说「别用 query 覆盖」）。
// 但如果是 `?site=auto` + 无前缀，结果必须是未指定。
func TestResolveSiteRouteAutoMeansUnspecified(t *testing.T) {
	got := resolveSiteRoute(reqWith("site=auto", ""), "gpt-4")
	if got.Forced() {
		t.Errorf("?site=auto 应表示未指定，实际 site=%q", got.Site)
	}
}

// 空模型名不应导致 panic，也不该被拆出站点。
func TestResolveSiteRouteHandlesEmptyModel(t *testing.T) {
	got := resolveSiteRoute(reqWith("", ""), "")
	if got.Forced() {
		t.Errorf("空模型名不该产生站点，实际 %q", got.Site)
	}
	if got.Model != "" {
		t.Errorf("空模型名应保持为空，实际 %q", got.Model)
	}
}

// -----------------------------------------------------------------------------
// 报错文案
// -----------------------------------------------------------------------------

// 报错必须说清「站点是从哪来的」。用户常常同时写了好几种指定方式，
// 只说「你指定了国际站」会让人不知道去哪改。
func TestRouteSourceLabelIsHumanReadable(t *testing.T) {
	cases := map[string]string{
		"query":        "URL 参数 ?site=",
		"header":       "请求头 X-WB-Site",
		"model_prefix": "模型名前缀",
	}
	for src, want := range cases {
		if got := routeSourceLabel(src); got != want {
			t.Errorf("routeSourceLabel(%q) = %q，期望 %q", src, got, want)
		}
	}
	if got := routeSourceLabel("unknown_src"); got == "" {
		t.Error("未知来源也应有兜底文案，不能是空串")
	}
}
