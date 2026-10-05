package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/catalog"
)

// TestPanelSiteRouteAliasesMatchRealVocabulary 守住「面板教的写法 == 后端认的写法」。
//
// 这条不变式值得单独测，是因为它错了**不会报错**，只会让用户困惑：
//   - 词表里有、面板没列 → 用户不知道有这个写法（少用了一个能用的功能）
//   - 面板列了、词表里没有 → 用户照着面板写，请求被静默当成「未指定」，
//     表现为「我明明指定了站点却没用」
//
// 后一种尤其坏：它是**静默失效**，没有报错、没有日志，用户只会觉得网关不听话。
func TestPanelSiteRouteAliasesMatchRealVocabulary(t *testing.T) {
	for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
		aliases := queryAliasesFor(site)
		if len(aliases) == 0 {
			t.Fatalf("%s 没有任何别名，面板会教不出可用写法", auth.SiteLabel(site))
		}
		for _, alias := range aliases {
			// 反向验证：面板列出的每一个写法，词表必须都认得且认成同一个站点。
			got, ok := normalizeSiteQuery(alias)
			if !ok {
				t.Errorf("面板列出了 %q，但 normalizeSiteQuery 不认它", alias)
				continue
			}
			if got != site {
				t.Errorf("面板把 %q 列在 %s 下，但词表把它认成 %q", alias, site, got)
			}
		}
	}

	// 两个站点的别名集合必须不相交，否则同一种写法会有两种解释。
	cn := map[string]bool{}
	for _, alias := range queryAliasesFor(auth.SiteCN) {
		cn[alias] = true
	}
	for _, alias := range queryAliasesFor(auth.SiteINTL) {
		if cn[alias] {
			t.Errorf("别名 %q 同时被算进国内站与国际站", alias)
		}
	}
}

// TestPanelSiteRoutePrefixIsInvertible 守住站点前缀的唯一性。
//
// 前缀是「模型名里的一段」而不是独立字段，因此一旦两个站点共用同一个前缀，
// 路由就无法判定，且不会有任何报错。逆查函数 prefixFor 也要能覆盖两个站点。
func TestPanelSiteRoutePrefixIsInvertible(t *testing.T) {
	seen := map[string]string{}
	for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
		prefix := prefixFor(site)
		if prefix == "" {
			t.Fatalf("%s 没有对应的模型名前缀，面板无法给出示例", auth.SiteLabel(site))
		}
		if other, dup := seen[prefix]; dup {
			t.Errorf("前缀 %q 同时映射到 %q 与 %q", prefix, other, site)
		}
		seen[prefix] = site

		// 下发的前缀必须是**规范写法**（大写 + 分隔符），不是词表里的小写键。
		// 界面与文档都按这个值教用户，退化成小写会让两处说法不一致。
		if prefix != strings.ToUpper(prefix) {
			t.Errorf("站点 %s 的前缀 %q 应统一为大写规范写法", auth.SiteLabel(site), prefix)
		}
		// 逆查出来的前缀**已含分隔符**（如 `CN-`），必须真的能被解析回同一个站点。
		// 这里直接拼模型名而不是再补一个 `/` —— 补错分隔符的症状是
		// 「面板给的示例贴过去不生效」，而这正是该测试要拦住的。
		got, rest, ok := splitSitePrefix(prefix + "some-model")
		if !ok || got != site || rest != "some-model" {
			t.Errorf("前缀 %q 解析异常：site=%q rest=%q ok=%v", prefix, got, rest, ok)
		}
	}
}

// TestPickExampleModelSkipsVirtualNames 守住「示例不要用虚拟条目」。
//
// 目录里混着 auto / default 这类不是真模型的调度占位名，而它们在字母序里排最前。
// 直接取第一个会让示例长成 `AI-auto`，看起来像「前缀要配这种词」，
// 而用户照抄到真实调用里会得到一个不存在的模型。
func TestPickExampleModelSkipsVirtualNames(t *testing.T) {
	models := []catalog.Model{
		{ID: "auto"},
		{ID: "default"},
		{ID: ""},
		{ID: "any"},
		{ID: "zai-glm"},
		{ID: "claude-sonnet"},
	}
	got := pickExampleModel(models)
	if got != "claude-sonnet" {
		t.Errorf("期望跳过虚拟名后取字母序第一个 claude-sonnet，实际 %q", got)
	}

	// 全是虚拟名 / 空目录时回落到稳定字面量，而不是给一个空示例。
	for _, empty := range [][]catalog.Model{{{ID: "auto"}, {ID: "default"}}, nil, {}} {
		if got := pickExampleModel(empty); got == "" || isVirtualModelName(got) {
			t.Errorf("无合适模型时应回落到真实模型名，实际 %q", got)
		}
	}
}

// TestPanelSiteRouteEndpointShape 验证接口返回结构可用于渲染。
func TestPanelSiteRouteEndpointShape(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:1") // 地址不会被用到

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/site-route", nil)
	srv.handlePanelSiteRoute(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Options []struct {
			Kind     string `json:"kind"`
			Priority int    `json:"priority"`
			Example  string `json:"example"`
		} `json:"options"`
		Sites []struct {
			Site         string   `json:"site"`
			Label        string   `json:"label"`
			ModelPrefix  string   `json:"model_prefix"`
			QueryAliases []string `json:"query_aliases"`
		} `json:"sites"`
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}

	if len(body.Options) != 3 {
		t.Fatalf("期望 3 种指定方式，实际 %d 种", len(body.Options))
	}
	// 优先级必须是 1/2/3 且不重复 —— 界面按它排「哪种更优先」。
	byPriority := map[int]string{}
	for _, opt := range body.Options {
		if opt.Kind == "" || opt.Example == "" {
			t.Errorf("指定方式缺少 kind 或 example：%+v", opt)
		}
		if prev, dup := byPriority[opt.Priority]; dup {
			t.Errorf("优先级 %d 被 %q 与 %q 共用", opt.Priority, prev, opt.Kind)
		}
		byPriority[opt.Priority] = opt.Kind
	}
	for _, want := range []int{1, 2, 3} {
		if _, ok := byPriority[want]; !ok {
			t.Errorf("缺少优先级 %d 的指定方式", want)
		}
	}

	if len(body.Sites) != 2 {
		t.Fatalf("期望 2 个站点，实际 %d 个", len(body.Sites))
	}
	for _, site := range body.Sites {
		if site.ModelPrefix == "" || len(site.QueryAliases) == 0 {
			t.Errorf("站点 %s 的写法信息不完整：%+v", site.Site, site)
		}
		// 下发的前缀必须**自带分隔符**：前端拿到就展示/复制，不自己拼。
		// 若这里退化成裸 `CN`，界面会显示 `CN` 而用户拼成 `CNdeepseek`（不生效）。
		if !strings.HasSuffix(site.ModelPrefix, sitePrefixSeparator) {
			t.Errorf("站点 %s 的前缀 %q 未含分隔符 %q", site.Site, site.ModelPrefix, sitePrefixSeparator)
		}
	}
	// 示例必须能原样贴去用：拿它走一遍真实解析，站点与模型名都要对。
	// 这条专门拦 `AI--deepseek`（前缀已含分隔符又拼了一次 `-`）这类低级但致命的错误。
	for _, opt := range body.Options {
		if opt.Kind != "model_prefix" {
			continue
		}
		if opt.Example != "AI-"+pickExampleModel(srv.cat.Merged()) {
			t.Errorf("模型名前缀示例格式不对：%q（不应出现重复分隔符）", opt.Example)
		}
		gotSite, gotModel, ok := splitSitePrefix(opt.Example)
		if !ok || gotSite != auth.SiteINTL {
			t.Errorf("示例 %q 无法被自身解析回国际站：site=%q ok=%v", opt.Example, gotSite, ok)
		}
		if gotModel == "" || strings.Contains(gotModel, sitePrefixSeparator+sitePrefixSeparator) {
			t.Errorf("示例 %q 剥出的模型名不合法：%q", opt.Example, gotModel)
		}
	}
	if len(body.Notes) == 0 {
		t.Error("未下发任何说明，界面会缺少「不回落」等关键提醒")
	}
}
