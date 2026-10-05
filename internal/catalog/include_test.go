package catalog

import "testing"

func modelIDs(ms []Model) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		out[m.ID] = true
	}
	return out
}

// TestMergeSiteCatalogAppendsCommonWhenIncluded 守住国际站的修复：
// 声明了 include 的站点，公共基座里的模型必须被并进来。
//
// 真实数据：国际站实时 18 + npm 覆盖层 22 + 公共基座 22 → 44；
// 其中 deepseek-v4.1-flash 只存在于公共基座，正是用户报缺的那个。
func TestMergeSiteCatalogAppendsCommonWhenIncluded(t *testing.T) {
	live := []Model{{ID: "hy3"}}
	npm := []Model{{ID: "deepseek-v4-pro"}}
	common := []Model{{ID: "deepseek-v4.1-flash"}, {ID: "kimi-k2.8-preview"}}

	got := modelIDs(mergeSiteCatalog(live, npm, common, true))

	for _, want := range []string{"hy3", "deepseek-v4-pro", "deepseek-v4.1-flash", "kimi-k2.8-preview"} {
		if !got[want] {
			t.Errorf("声明 include 后仍缺模型 %q（得到 %d 个）", want, len(got))
		}
	}
}

// TestMergeSiteCatalogSkipsCommonWhenNotIncluded 守住反例：未声明 include 的站点
// 绝不能并入公共基座。
//
// 这是本修复最容易搞错的一侧 —— 「反正基座里有就都并上」看着更省事，
// 但国内站实测不支持 gpt-6-astra / deepseek-v4.1-flash-sg（11102 not found）。
// 把它们列成可用，用户选中后必然报错，比「列表少一个」严重得多。
func TestMergeSiteCatalogSkipsCommonWhenNotIncluded(t *testing.T) {
	live := []Model{{ID: "deepseek-v4.1-flash"}}
	npm := []Model{{ID: "hy3"}}
	common := []Model{{ID: "gpt-6-astra"}, {ID: "deepseek-v4.1-flash-sg"}}

	got := modelIDs(mergeSiteCatalog(live, npm, common, false))

	if len(got) != 2 || !got["deepseek-v4.1-flash"] || !got["hy3"] {
		t.Fatalf("未声明 include 时不应并入基座，得到: %#v", got)
	}
	if got["gpt-6-astra"] || got["deepseek-v4.1-flash-sg"] {
		t.Fatal("未声明 include 却并入了基座模型（会把本站不可用的模型报成可用）")
	}
}

// TestMergeSiteCatalogSiteOverlayWinsOverCommon 守住优先级：
// 同一模型既在本站覆盖层又在公共基座时，必须保留**本站**那份（倍率等字段以本站为准）。
//
// MergeCatalogs 是「先到先得」，所以基座必须最后并入；一旦顺序写反，
// 站点自己的倍率会被基座的默认值覆盖 —— 表现是价格显示不对，但列表长度看不出异常。
func TestMergeSiteCatalogSiteOverlayWinsOverCommon(t *testing.T) {
	live := []Model{}
	npm := []Model{paidModel("deepseek-v4.1-flash", 0.4)}
	common := []Model{paidModel("deepseek-v4.1-flash", 9.9)}

	got := mergeSiteCatalog(live, npm, common, true)
	if len(got) != 1 {
		t.Fatalf("同名模型应去重为 1 条，得到 %d 条", len(got))
	}
	if got[0].Multiplier != 0.4 {
		t.Fatalf("本站覆盖层应胜出（期望 0.4），实际 %v", got[0].Multiplier)
	}
}

// TestMergeSiteCatalogNoCommonKeepsSiteCatalog 覆盖「基座取不到」的情形：
// 合并失败时目录应原样保留，而不是变成空。
func TestMergeSiteCatalogNoCommonKeepsSiteCatalog(t *testing.T) {
	live := []Model{{ID: "hy3"}}
	npm := []Model{{ID: "deepseek-v4-pro"}}

	got := mergeSiteCatalog(live, npm, nil, true)
	if len(got) != 2 {
		t.Fatalf("基座为空时不应丢本站模型，得到 %d 条", len(got))
	}
}
