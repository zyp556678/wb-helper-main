package upstream

import "testing"

// TestParseLiveCatalogReadsInclude 守住「目录继承声明必须被解析出来」。
//
// 这条不是锦上添花：上游对国际站下发的是**覆盖层**（18 个模型）而不是完整目录，
// 靠 `include: ["../common/product.json"]` 声明「还要并上公共基座」。
// 不解析它，国际站就会少掉基座里的模型 —— 实测少了 deepseek-v4.1-flash 等 5 个，
// 而这些模型在该站**实际可调用**（HTTP 200 正常出字）。
func TestParseLiveCatalogReadsInclude(t *testing.T) {
	raw := []byte(`{
	  "include": ["../common/product.json"],
	  "mergeStrategy": "merge",
	  "models": [{"id": "hy3", "name": "Hy3"}]
	}`)

	cat, err := ParseLiveCatalog(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !cat.WantsInclude() {
		t.Fatal("声明了 include 却报告不需要合并公共基座")
	}
	if len(cat.Include) != 1 || cat.Include[0] != "../common/product.json" {
		t.Fatalf("include 解析错误: %#v", cat.Include)
	}
	if cat.MergeStrategy != "merge" {
		t.Fatalf("mergeStrategy 解析错误: %q", cat.MergeStrategy)
	}
	if len(cat.Models) != 1 || cat.Models[0].ID != "hy3" {
		t.Fatalf("模型解析错误: %#v", cat.Models)
	}
}

// TestParseLiveCatalogWithoutInclude 守住反例：国内站不下发 include，
// 必须报告「不需要合并」。
//
// 这条比上一条更重要 —— 如果这里判断错了（把「没有声明」也当成需要合并），
// 国内站会并进公共基座里那批**国内站用不了**的模型
// （实测 gpt-6-astra / deepseek-v4.1-flash-sg 在国内站返回 11102 not found）。
// 少列一个模型只是列表不全，多列一个不可用的模型会让客户端选中后直接报错。
func TestParseLiveCatalogWithoutInclude(t *testing.T) {
	raw := []byte(`{
	  "mergeStrategy": "merge",
	  "models": [{"id": "deepseek-v4.1-flash", "name": "DeepSeek V4.1 Flash"}]
	}`)

	cat, err := ParseLiveCatalog(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cat.WantsInclude() {
		t.Fatal("未声明 include 却报告需要合并公共基座（会把本站不可用的模型报成可用）")
	}
	if len(cat.Include) != 0 {
		t.Fatalf("include 应为空: %#v", cat.Include)
	}
}

// TestParseLiveCatalogIncludeEmptyArray 覆盖「显式空数组」形态。
func TestParseLiveCatalogIncludeEmptyArray(t *testing.T) {
	raw := []byte(`{"include": [], "models": [{"id": "hy3"}]}`)
	cat, err := ParseLiveCatalog(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cat.WantsInclude() {
		t.Fatal("空 include 数组不应触发合并")
	}
}

// TestParseLiveCatalogEmptyModelsStillErrors 守住既有行为：
// models 为空仍算失败（否则整站目录会被清空）。
func TestParseLiveCatalogEmptyModelsStillErrors(t *testing.T) {
	if _, err := ParseLiveCatalog([]byte(`{"include":["x"],"models":[]}`)); err == nil {
		t.Fatal("models 为空应当报错")
	}
}
