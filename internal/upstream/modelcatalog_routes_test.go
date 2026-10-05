package upstream

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy-gateway/internal/auth"
)

// 本文件覆盖「/v3/config 主路 + 企业端点补路」目录链路的回归：
//  1. intl 三路 UA 的 v3 并集（桌面端字段权威）+ 企业端点补缺 + include 继承；
//  2. v3 部分 UA 失败 → 用成功路继续，不报错；
//  3. v3 全失败 + 企业端点成功 → 结果逐字段等于企业端点结果（= 改动前的行为，零回归）；
//  4. v3 成功 + 企业端点失败 → 用 v3 结果，不报错；
//  5. CN 站点只发 IDE UA 一路 /v3/config（按假上游收到的路径+UA 计数断言）；
//  6. nonChatModel 过滤对 v3 条目生效（含 /v3/config 解析路径）。
//
// 以及 /v3/config 的字段映射、促销折算与 daily 时段窗口的单元测试。
// 全部用 httptest 假上游，不打真实网络、不读写用户凭据。

// 假上游路径与三条 v3 主路 UA（测试里作为可区分的常量）。
//
// 注意桌面端路**不是** Profile.ClientUA：它必须是桌面端三段式（见 v3UARoutes 注释），
// 与 Profile 的 CLI 两段式 UA 区分开，否则「三路并集」的断言会退化成一路。
const (
	v3ConfigPath   = "/v3/config"
	enterprisePath = "/v2/enterprises/personal/models"
	desktopUAIntl  = "WorkBuddy/5.5.6 WorkBuddy AI/5.5.6 CLI/2.137.1"
	enterpriseUA   = "CLI/2.143.1 CodeBuddy/2.143.1"
)

// catalogTestProfile 构造目录探测用的站点 Profile（Base 指向假上游）。
func catalogTestProfile(base, site, ua string) *Profile {
	return &Profile{
		Key: site, Label: auth.SiteLabel(site),
		Base: base, Origin: base, Platform: "test",
		ClientUA: ua, ClientID: "codebuddy-cli", ClientVer: "2.143.1", Product: "SaaS",
	}
}

// catalogTestCred 是假凭据视图（绝不使用真实令牌）。
func catalogTestCred() *CredentialView {
	return &CredentialView{AccessToken: "test-token", UID: "u-test"}
}

// liveModelJSON 构造单条目录条目的 JSON；tags 为空时输出空数组。
func liveModelJSON(id, name, desc string, maxInput, maxOutput int, tags ...string) string {
	tagsJSON := "[]"
	if len(tags) > 0 {
		quoted := make([]string, len(tags))
		for i, tag := range tags {
			quoted[i] = strconv.Quote(tag)
		}
		tagsJSON = "[" + strings.Join(quoted, ",") + "]"
	}
	return fmt.Sprintf(`{"id":%s,"name":%s,"descriptionZh":%s,"maxInputTokens":%d,"maxOutputTokens":%d,"tags":%s}`,
		strconv.Quote(id), strconv.Quote(name), strconv.Quote(desc), maxInput, maxOutput, tagsJSON)
}

// liveCatalogJSON 组装目录的 data 段（models 为 liveModelJSON 产物）。
func liveCatalogJSON(models ...string) string {
	return `{"mergeStrategy":"merge","models":[` + strings.Join(models, ",") + `]}`
}

// liveCatalogJSONWithInclude 组装带 include 声明的 data 段（国际站覆盖层形态）。
func liveCatalogJSONWithInclude(include string, models ...string) string {
	return `{"include":[` + strconv.Quote(include) + `],"mergeStrategy":"merge","models":[` +
		strings.Join(models, ",") + `]}`
}

// catalogEnvelope 把 data 段包成上游标准包络（doJSON 会解开一层 data）。
func catalogEnvelope(data string) string {
	return `{"code":0,"msg":"ok","data":` + data + `}`
}

func catalogOK(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { fmt.Fprint(w, catalogEnvelope(body)) }
}

func catalogFail(w http.ResponseWriter) {
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprint(w, `{"code":500,"msg":"boom"}`)
}

// routeKey 是假上游的分发表键：路径 + UA（企业端点与 v3 桌面端主路 UA 同值，
// 必须靠路径区分）。
func routeKey(path, ua string) string { return path + " " + ua }

// routeLog 记录假上游收到的 (路径, UA) 组合，供「只发某几路」类断言。
type routeLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *routeLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, routeKey(r.URL.Path, r.Header.Get("User-Agent")))
}

func (l *routeLog) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *routeLog) countRoute(path, ua string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.entries {
		if e == routeKey(path, ua) {
			n++
		}
	}
	return n
}

func (l *routeLog) countPath(path string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.entries {
		if strings.HasPrefix(e, path+" ") {
			n++
		}
	}
	return n
}

// catalogRouteServer 起一个按 (路径, UA) 分发的假上游；未登记的请求判测试失败
// （这条同时充当「CN 不得发桌面端/CLI 路」这类断言的执行者）。
func catalogRouteServer(t *testing.T, log *routeLog, handlers map[string]func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if log != nil {
			log.add(r)
		}
		h, ok := handlers[routeKey(r.URL.Path, r.Header.Get("User-Agent"))]
		if !ok {
			t.Errorf("收到未登记的请求: path=%q ua=%q", r.URL.Path, r.Header.Get("User-Agent"))
			http.Error(w, "unexpected route", http.StatusBadRequest)
			return
		}
		h(w)
	}))
}

func findLiveModel(cat *LiveCatalog, id string) *LiveModel {
	for i := range cat.Models {
		if cat.Models[i].ID == id {
			return &cat.Models[i]
		}
	}
	return nil
}

func liveModelIDs(cat *LiveCatalog) []string {
	ids := make([]string, 0, len(cat.Models))
	for _, m := range cat.Models {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestFetchModelCatalogV3UnionAndEnterpriseFill 覆盖要求 1：
//   - intl 三路 v3（桌面端 / IDE / CLI）返回不同集合 → 并集；
//   - 同名模型字段来自桌面端主路（IDE / CLI / 企业端点都不得覆盖）；
//   - 企业端点只补 v3 缺失的 id，且 include 声明（只有企业端点下发）不能丢；
//   - 总请求数 = 3 路 v3 + 1 企业端点。
func TestFetchModelCatalogV3UnionAndEnterpriseFill(t *testing.T) {
	log := &routeLog{}
	srv := catalogRouteServer(t, log, map[string]func(http.ResponseWriter){
		routeKey(v3ConfigPath, desktopUAIntl): catalogOK(liveCatalogJSON(
			liveModelJSON("shared", "桌面端共享", "桌面端描述", 1000000, 128000),
			liveModelJSON("desktop-only", "仅桌面端", "", 500000, 64000),
		)),
		routeKey(v3ConfigPath, codeBuddyIDEUA): catalogOK(liveCatalogJSON(
			liveModelJSON("shared", "IDE共享", "IDE描述", 176000, 24000),
			liveModelJSON("ide-only", "仅IDE", "", 200000, 32000),
		)),
		routeKey(v3ConfigPath, codeBuddyCLIUA): catalogOK(liveCatalogJSON(
			liveModelJSON("shared", "CLI共享", "CLI描述", 272000, 72000),
			liveModelJSON("cli-only", "仅CLI", "", 256000, 32000),
		)),
		routeKey(enterprisePath, enterpriseUA): catalogOK(liveCatalogJSONWithInclude("../common/product.json",
			liveModelJSON("shared", "企业共享", "企业描述", 1000, 2000),
			liveModelJSON("ent-only", "仅企业", "", 3000, 4000),
		)),
	})
	defer srv.Close()

	c := &Client{Control: srv.Client()}
	p := catalogTestProfile(srv.URL, auth.SiteINTL, enterpriseUA)
	cat, err := c.FetchModelCatalog(context.Background(), catalogTestCred(), p)
	if err != nil {
		t.Fatalf("v3 主路 + 企业补路探测失败: %v", err)
	}

	wantIDs := []string{"shared", "desktop-only", "ide-only", "cli-only", "ent-only"}
	if got := liveModelIDs(cat); strings.Join(got, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("合并顺序 = %v，期望 %v（v3 主路原序在前、补充项按序追加）", got, wantIDs)
	}

	// 字段权威：shared 必须来自桌面端主路。
	shared := findLiveModel(cat, "shared")
	if shared == nil {
		t.Fatal("shared 模型缺失")
	}
	if shared.MaxInputTokens != 1000000 || shared.Name != "桌面端共享" || shared.Description != "桌面端描述" {
		t.Errorf("shared 字段被非主路覆盖: %+v（期望桌面端的 1000000/桌面端共享/桌面端描述）", *shared)
	}
	// 补充路条目字段取自其所属路。
	if m := findLiveModel(cat, "ide-only"); m == nil || m.MaxInputTokens != 200000 {
		t.Errorf("ide-only 条目缺失或字段错误: %+v", m)
	}
	if m := findLiveModel(cat, "cli-only"); m == nil || m.MaxInputTokens != 256000 {
		t.Errorf("cli-only 条目缺失或字段错误: %+v", m)
	}
	if m := findLiveModel(cat, "ent-only"); m == nil || m.MaxInputTokens != 3000 {
		t.Errorf("ent-only 条目缺失或字段错误: %+v", m)
	}

	// include 只有企业端点下发；合并后必须保留（否则国际站目录会丢掉公共基座合并）。
	if !cat.WantsInclude() || len(cat.Include) != 1 || cat.Include[0] != "../common/product.json" {
		t.Errorf("企业端点的 include 声明在合并后丢失: include=%v", cat.Include)
	}
	if cat.MergeStrategy != "merge" {
		t.Errorf("mergeStrategy 丢失: %q", cat.MergeStrategy)
	}

	if n := log.total(); n != 4 {
		t.Errorf("探测请求数 = %d，期望 4（3 路 v3 + 1 企业端点）", n)
	}
}

// TestFetchModelCatalogV3PartialFailureIntl 覆盖要求 2：v3 两路 500、一路成功
// → 用成功路结果且不报错（企业端点同时失败，隔离 v3 自身的逐路容错）。
func TestFetchModelCatalogV3PartialFailureIntl(t *testing.T) {
	srv := catalogRouteServer(t, nil, map[string]func(http.ResponseWriter){
		routeKey(v3ConfigPath, desktopUAIntl):  catalogFail,
		routeKey(v3ConfigPath, codeBuddyIDEUA): catalogOK(liveCatalogJSON(liveModelJSON("ide-only", "仅IDE", "描述", 200000, 32000))),
		routeKey(v3ConfigPath, codeBuddyCLIUA): catalogFail,
		routeKey(enterprisePath, enterpriseUA): catalogFail,
	})
	defer srv.Close()

	c := &Client{Control: srv.Client()}
	p := catalogTestProfile(srv.URL, auth.SiteINTL, enterpriseUA)
	cat, err := c.FetchModelCatalog(context.Background(), catalogTestCred(), p)
	if err != nil {
		t.Fatalf("v3 部分失败不应报错: %v", err)
	}
	if got := liveModelIDs(cat); len(got) != 1 || got[0] != "ide-only" {
		t.Fatalf("降级结果 = %v，期望 [ide-only]", got)
	}
	if m := findLiveModel(cat, "ide-only"); m == nil || m.MaxInputTokens != 200000 {
		t.Errorf("降级后字段错误: %+v", m)
	}
}

// TestFetchModelCatalogV3AllFailFallsBackToEnterprise 覆盖要求 3（零回归核心）：
// v3 三路全 500 + 企业端点成功 → 结果**逐字段等于**企业端点结果（含顺序与 include），
// 与「改动前只有企业端点」的行为一致。
func TestFetchModelCatalogV3AllFailFallsBackToEnterprise(t *testing.T) {
	enterpriseBody := liveCatalogJSONWithInclude("../common/product.json",
		liveModelJSON("hy3", "Hy3 企业", "企业描述", 128000, 32000),
		liveModelJSON("deepseek-v4.1-flash", "DS Flash", "企业描述", 640000, 64000),
	)
	srv := catalogRouteServer(t, nil, map[string]func(http.ResponseWriter){
		routeKey(v3ConfigPath, desktopUAIntl):  catalogFail,
		routeKey(v3ConfigPath, codeBuddyIDEUA): catalogFail,
		routeKey(v3ConfigPath, codeBuddyCLIUA): catalogFail,
		routeKey(enterprisePath, enterpriseUA): catalogOK(enterpriseBody),
	})
	defer srv.Close()

	c := &Client{Control: srv.Client()}
	p := catalogTestProfile(srv.URL, auth.SiteINTL, enterpriseUA)
	cat, err := c.FetchModelCatalog(context.Background(), catalogTestCred(), p)
	if err != nil {
		t.Fatalf("v3 全失败但企业端点成功，不应报错: %v", err)
	}

	// 期望值由同一份企业端点响应独立解析得到：真·逐字段相等。
	want, err := ParseLiveCatalog([]byte(enterpriseBody))
	if err != nil {
		t.Fatalf("解析企业端点期望值失败: %v", err)
	}
	if !reflect.DeepEqual(cat.Models, want.Models) {
		t.Fatalf("v3 全失败时结果与企业端点结果不一致（零回归被破坏）:\n got=%+v\nwant=%+v", cat.Models, want.Models)
	}
	if cat.WantsInclude() != want.WantsInclude() || len(cat.Include) != len(want.Include) {
		t.Fatalf("include 声明不一致: got=%v want=%v", cat.Include, want.Include)
	}
}

// TestFetchModelCatalogV3OnlyWhenEnterpriseFails 覆盖要求 4：v3 三路成功 +
// 企业端点 500 → 用 v3 并集结果，不报错。
func TestFetchModelCatalogV3OnlyWhenEnterpriseFails(t *testing.T) {
	srv := catalogRouteServer(t, nil, map[string]func(http.ResponseWriter){
		routeKey(v3ConfigPath, desktopUAIntl): catalogOK(liveCatalogJSON(
			liveModelJSON("gpt-6-sol", "GPT-6 Sol", "", 1000000, 128000),
		)),
		routeKey(v3ConfigPath, codeBuddyIDEUA): catalogOK(liveCatalogJSON(
			liveModelJSON("o4-mini", "o4-mini", "", 200000, 32000),
		)),
		routeKey(v3ConfigPath, codeBuddyCLIUA): catalogOK(liveCatalogJSON(
			liveModelJSON("kimi-k2.8-preview", "Kimi", "", 256000, 32000),
		)),
		routeKey(enterprisePath, enterpriseUA): catalogFail,
	})
	defer srv.Close()

	c := &Client{Control: srv.Client()}
	p := catalogTestProfile(srv.URL, auth.SiteINTL, enterpriseUA)
	cat, err := c.FetchModelCatalog(context.Background(), catalogTestCred(), p)
	if err != nil {
		t.Fatalf("企业端点失败但 v3 成功，不应报错: %v", err)
	}
	wantIDs := []string{"gpt-6-sol", "o4-mini", "kimi-k2.8-preview"}
	if got := liveModelIDs(cat); strings.Join(got, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("v3 结果 = %v，期望 %v", got, wantIDs)
	}
}

// TestFetchModelCatalogCNUsesIDEOnlyForV3 覆盖要求 5：CN 站点 /v3/config 只发
// IDE UA 一路（参考实现 FetchModels 口径，不做三 UA 并集）；企业端点仍单路。
// 假上游对未登记路由判失败，桌面前/CLI 路一旦发出即测试红。
func TestFetchModelCatalogCNUsesIDEOnlyForV3(t *testing.T) {
	log := &routeLog{}
	srv := catalogRouteServer(t, log, map[string]func(http.ResponseWriter){
		routeKey(v3ConfigPath, codeBuddyIDEUA): catalogOK(liveCatalogJSON(
			liveModelJSON("o4-mini", "o4-mini", "", 200000, 32000),
		)),
		routeKey(enterprisePath, enterpriseUA): catalogOK(liveCatalogJSON(
			liveModelJSON("hy3", "Hy3", "", 128000, 32000),
		)),
	})
	defer srv.Close()

	c := &Client{Control: srv.Client()}
	p := catalogTestProfile(srv.URL, auth.SiteCN, enterpriseUA)
	cat, err := c.FetchModelCatalog(context.Background(), catalogTestCred(), p)
	if err != nil {
		t.Fatalf("国内站探测失败: %v", err)
	}
	wantIDs := []string{"o4-mini", "hy3"} // v3 主路在前，企业端点补 hy3
	if got := liveModelIDs(cat); strings.Join(got, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("国内站目录 = %v，期望 %v", got, wantIDs)
	}
	if n := log.countPath(v3ConfigPath); n != 1 {
		t.Errorf("/v3/config 请求数 = %d，期望 1（CN 只发 IDE 单路）", n)
	}
	if n := log.countRoute(v3ConfigPath, codeBuddyIDEUA); n != 1 {
		t.Errorf("/v3/config IDE-UA 请求数 = %d，期望 1", n)
	}
	if n := log.countRoute(enterprisePath, enterpriseUA); n != 1 {
		t.Errorf("企业端点请求数 = %d，期望 1（现状单路）", n)
	}
	if n := log.total(); n != 2 {
		t.Errorf("总请求数 = %d，期望 2（1 路 v3 + 1 企业端点）", n)
	}
}

// TestParseV3ConfigFiltersNonChatModels 覆盖要求 6：nonChatModel 三类规则
// （id 前缀 / maxOutputTokens≤256 / 生成类 tags）在 /v3/config 解析路径同样生效。
func TestParseV3ConfigFiltersNonChatModels(t *testing.T) {
	raw := []byte(liveCatalogJSON(
		liveModelJSON("seedance-2.5", "Seedance", "", 0, 8192, "text-to-video"),
		liveModelJSON("gpt-image-2.5", "Image", "", 0, 4096, "text-to-image"),
		liveModelJSON("tiny-model", "Tiny", "", 128000, 256),
		liveModelJSON("nes-1.2", "Embed", "", 8192, 8192),
		liveModelJSON("gpt-6-sol", "GPT-6 Sol", "对话模型", 1000000, 128000),
		liveModelJSON("kimi-k2.8-preview", "Kimi", "", 256000, 32000),
	))

	cat, err := parseV3Config(raw)
	if err != nil {
		t.Fatalf("解析 /v3/config 失败: %v", err)
	}
	wantIDs := []string{"gpt-6-sol", "kimi-k2.8-preview"}
	if got := liveModelIDs(cat); strings.Join(got, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("过滤后 v3 目录 = %v，期望 %v", got, wantIDs)
	}
}

// TestParseV3ConfigMapsFieldsAndAppliesPromotions 覆盖要求「映射到既有 LiveModel
// 结构」：窗口 / 能力 / credits 牌价 / efforts 照实映射；生效促销折算倍率
// （牌价 × factor），不编造。
func TestParseV3ConfigMapsFieldsAndAppliesPromotions(t *testing.T) {
	raw := []byte(`{
	  "mergeStrategy": "merge",
	  "models": [{
	    "id": "glm-5.2", "name": "GLM-5.2", "descriptionZh": "智谱 GLM", "credits": "x0.29 credits",
	    "maxInputTokens": 200000, "maxOutputTokens": 128000, "supportsReasoning": true, "supportsToolCall": true,
	    "contextWindow": {"defaultLength": 200000, "supportedLengths": [128000, 200000]},
	    "reasoning": {"defaultEffort": "high", "supportedEfforts": ["low", "high", "max"], "canDisableThinking": true}
	  }],
	  "modelPromotions": [{
	    "enabled": true, "priority": 100, "modelIds": ["glm-5.2"],
	    "badge": {"label": "限时五折"},
	    "discount": {"discountedCredits": "0.15x", "factor": 0.5},
	    "schedule": {"validUntil": "2099-01-01T00:00:00Z"}
	  }]
	}`)

	cat, err := parseV3Config(raw)
	if err != nil {
		t.Fatalf("解析 /v3/config 失败: %v", err)
	}
	m := findLiveModel(cat, "glm-5.2")
	if m == nil {
		t.Fatal("glm-5.2 缺失")
	}
	if m.Credits != "x0.29 credits" || !m.HasMultiplier || m.BaseMultiplier != 0.29 {
		t.Errorf("credits 牌价映射错误: %+v", *m)
	}
	if math.Abs(m.Multiplier-0.145) > 1e-9 {
		t.Errorf("生效倍率 = %v，期望 0.29×0.5=0.145", m.Multiplier)
	}
	if m.PromoFactor == nil || *m.PromoFactor != 0.5 || m.PromoLabel != "限时五折" {
		t.Errorf("促销字段映射错误: factor=%v label=%q", m.PromoFactor, m.PromoLabel)
	}
	if m.PromoUntil != "2099-01-01T00:00:00Z" {
		t.Errorf("PromoUntil = %q", m.PromoUntil)
	}
	if m.ContextWindowDefault != 200000 || len(m.ContextWindowOptions) != 2 {
		t.Errorf("窗口映射错误: default=%d options=%v", m.ContextWindowDefault, m.ContextWindowOptions)
	}
	if !m.SupportsReasoning || !m.SupportsToolCall || !m.CanDisableThinking {
		t.Errorf("能力字段映射错误: %+v", *m)
	}
	if m.ReasoningEffort != "high" || strings.Join(m.SupportedEfforts, ",") != "low,high,max" {
		t.Errorf("efforts 映射错误: default=%q supported=%v", m.ReasoningEffort, m.SupportedEfforts)
	}
}

// TestParseV3ConfigAddsTrialBannerModel 覆盖参考实现的试用横幅补入：
// data.models 之外的 ModelTrialBanner 模型（实测 hy4-preview-f 只在此）也要进目录，
// 能力字段继承 targetModelId 条目，「转正后」的计费/标签字段清空。
func TestParseV3ConfigAddsTrialBannerModel(t *testing.T) {
	raw := []byte(`{
	  "models": [{"id": "hy4-preview", "name": "HY4 Preview", "credits": "x0.29 credits",
	    "maxInputTokens": 256000, "maxOutputTokens": 64000, "tags": ["craft"]}],
	  "productFeaturesConfig": {"ModelTrialBanner": {"banners": [
	    {"modelId": "hy4-preview-f", "targetModelId": "hy4-preview"}
	  ]}}
	}`)

	cat, err := parseV3Config(raw)
	if err != nil {
		t.Fatalf("解析 /v3/config 失败: %v", err)
	}
	m := findLiveModel(cat, "hy4-preview-f")
	if m == nil {
		t.Fatal("试用横幅模型 hy4-preview-f 未补入目录")
	}
	if m.MaxInputTokens != 256000 {
		t.Errorf("试用模型的能力字段应继承 targetModelId 条目: %+v", *m)
	}
	if m.Credits != "" || m.HasMultiplier || len(m.Tags) != 0 {
		t.Errorf("试用模型的「转正后」计费/标签字段应清空: %+v", *m)
	}
}

// TestV3PromoActiveDailyWindow 覆盖促销时段窗口（可跨午夜）：参考实现实测
// glm-5.2 夜间五折只在 23:00→7:50 生效，白天不能把折扣当常驻 —— 误判会
// 污染免费/收费判定（factor=0 类）与展示价。
func TestV3PromoActiveDailyWindow(t *testing.T) {
	promo := v3Promotion{
		Enabled:  true,
		Priority: 100,
		ModelIDs: []string{"glm-5.2"},
		Schedule: &v3PromotionSchedule{Daily: []v3PromotionDailyWindow{{Start: "23:00", End: "7:50"}}},
	}
	cases := []struct {
		h, m int
		want bool
	}{
		{0, 30, true},  // 凌晨在窗口内（跨午夜后段）
		{7, 49, true},  // 结束前一分钟
		{7, 50, false}, // 结束时刻为开区间
		{12, 0, false}, // 白天不生效
		{22, 59, false},
		{23, 0, true}, // 窗口起点
		{23, 30, true},
	}
	for _, c := range cases {
		now := time.Date(2026, 10, 5, c.h, c.m, 0, 0, v3PromoZone)
		if got := v3PromoActive(&promo, now); got != c.want {
			t.Errorf("v3PromoActive(%02d:%02d) = %v, want %v", c.h, c.m, got, c.want)
		}
	}

	expired := v3Promotion{Enabled: true, Schedule: &v3PromotionSchedule{ValidUntil: "2020-01-01T00:00:00Z"}}
	if v3PromoActive(&expired, time.Now().In(v3PromoZone)) {
		t.Error("validUntil 已过期的促销不应生效")
	}
	off := v3Promotion{Enabled: false}
	if v3PromoActive(&off, time.Now().In(v3PromoZone)) {
		t.Error("enabled=false 的促销不应生效")
	}
}
