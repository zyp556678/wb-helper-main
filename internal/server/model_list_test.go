package server

import (
	"strings"
	"testing"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/catalog"
)

// 本文件守住 `/v1/models` 的两条对外契约：
//
//  1. **id 带站点头，且与请求侧的前缀解析严格互逆** —— 客户端拿列表里的 ID
//     原样发请求，必须能调到同一个模型。不互逆的症状是「列表给了个 ID，贴过去调不通」，
//     而用户根本不会怀疑是列表的问题。
//  2. **思考能力字段完整** —— 客户端要据此决定 reasoning_effort 传什么；
//     字段残缺时它只能保守取值或干脆不传，表现为「思考按默认档跑，用户以为设置没生效」。

func TestSiteModelIDRoundTrips(t *testing.T) {
	cases := []struct{ site, model string }{
		{auth.SiteCN, "deepseek-v4.1-flash"},
		{auth.SiteINTL, "glm-5.3"},
		{auth.SiteCN, "deepseek-v4.1-flash-max"},
		{auth.SiteINTL, "vendor/nested-model"},
	}
	for _, c := range cases {
		id := siteModelID(c.site, c.model)
		gotSite, gotModel, ok := splitSitePrefix(id)
		if !ok {
			t.Fatalf("siteModelID(%s, %s) = %q，但 splitSitePrefix 认不出它是前缀形态",
				c.site, c.model, id)
		}
		if gotSite != c.site || gotModel != c.model {
			t.Errorf("往返不一致：siteModelID → %q → (%s, %s)，期望 (%s, %s)",
				id, gotSite, gotModel, c.site, c.model)
		}
	}
}

// 前缀必须与真实模型名天然隔离：上游模型名全小写，而前缀是大写。
// 一旦有人把前缀改成小写，`cn-xxx` 这类真实模型名就会被永久劫持 ——
// 症状是「这个模型怎么也调不到」，很难联想到是路由前缀干的。
func TestSitePrefixIsUpperCaseWithSeparator(t *testing.T) {
	for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
		p := prefixFor(site)
		if p == "" {
			t.Fatalf("站点 %s 没有前缀", site)
		}
		if p != strings.ToUpper(p) {
			t.Errorf("站点 %s 的前缀是 %q —— 必须全大写才能与全小写的真实模型名隔离", site, p)
		}
		if !strings.HasSuffix(p, sitePrefixSeparator) {
			t.Errorf("站点 %s 的前缀 %q 未含分隔符 %q", site, p, sitePrefixSeparator)
		}
	}
}

func TestSiteModelIDHandlesMissingInput(t *testing.T) {
	if got := siteModelID(auth.SiteCN, ""); got != "" {
		t.Errorf("空模型名应当原样返回，实际 %q", got)
	}
	if got := siteModelID("unknown-site", "m"); got != "m" {
		t.Errorf("未知站点应当原样返回模型名，实际 %q", got)
	}
}

// 上游没给档位清单时用默认档兜底 —— 默认档必然是支持的档位。
// 改之前只在 supportedEfforts 非空时才输出该字段：实测 56 条里只有 11 条带清单、
// 却有 35 条带默认档，客户端拿到的是残缺信息。
func TestAttachReasoningFallsBackToDefaultEffort(t *testing.T) {
	item := map[string]any{}
	attachReasoning(item, catalog.Model{
		ID: "deepseek-v4.1-flash", OnlyReasoning: true, ReasoningEffort: "high",
	})

	r, ok := item["reasoning"].(map[string]any)
	if !ok {
		t.Fatal("应当输出 reasoning 块")
	}
	if r["only"] != true {
		t.Error("仅推理模型应当标 only=true")
	}
	if r["default_effort"] != "high" {
		t.Errorf("默认档应当透出，实际 %v", r["default_effort"])
	}
	efforts, _ := r["supported_efforts"].([]string)
	if len(efforts) != 1 || efforts[0] != "high" {
		t.Errorf("档位清单应当以默认档兜底为 [high]，实际 %v", r["supported_efforts"])
	}
	// 扁平原字段也要带上兜底值（既有消费方可能只读它）。
	if got, _ := item["reasoning_supported_efforts"].([]string); len(got) != 1 {
		t.Errorf("扁平字段也要兜底，实际 %v", item["reasoning_supported_efforts"])
	}
}

func TestAttachReasoningKeepsUpstreamEfforts(t *testing.T) {
	item := map[string]any{}
	attachReasoning(item, catalog.Model{
		ID: "glm-5.3", SupportsReasoning: true, CanDisableThinking: true,
		ReasoningEffort: "high", SupportedEfforts: []string{"low", "high", "max"},
	})

	r, ok := item["reasoning"].(map[string]any)
	if !ok {
		t.Fatal("应当输出 reasoning 块")
	}
	if r["supported"] != true || r["can_disable"] != true {
		t.Errorf("能力位不对：%v", r)
	}
	efforts, _ := r["supported_efforts"].([]string)
	if len(efforts) != 3 {
		t.Errorf("上游给的档位清单不该被兜底覆盖，实际 %v", efforts)
	}
	if r["default_effort"] != "high" {
		t.Errorf("默认档缺失：%v", r)
	}
}

// 不支持推理的模型不该带 reasoning 块 —— 带上会让客户端以为可以传 effort 参数。
func TestAttachReasoningSkippedForPlainModel(t *testing.T) {
	item := map[string]any{}
	attachReasoning(item, catalog.Model{ID: "hunyuan-image-alpha"})
	if _, ok := item["reasoning"]; ok {
		t.Error("不支持推理的模型不该输出 reasoning 块")
	}
	if _, ok := item["reasoning_default_effort"]; ok {
		t.Error("不支持推理的模型不该输出默认档")
	}
}
