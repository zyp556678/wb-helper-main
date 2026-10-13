package catalog

import (
	"testing"

	"workbuddy-gateway/internal/auth"
)

// 本文件的用例锚在被动学习（Catalog.Observe）的三条写入规矩上。
//
// 它存在的理由：价格探测默认关闭，被动学习成了「关掉探测后价格仍然可用」的唯一
// 保障。它写错了不会报错，只会让选号层拿着错的价格做决策 —— 必须钉住。

// TestObserveLearnsFreshVerdict 空白结论应被填入。
func TestObserveLearnsFreshVerdict(t *testing.T) {
	c := newCatalogFromFixture(t, nil, nil)

	if !c.Observe(auth.SiteCN, "m1", "paid", 0.5, 300) {
		t.Fatal("空白台账应接受被动结论")
	}
	p, ok := c.ProbeOf(auth.SiteCN, "m1")
	if !ok || p.Verdict != "paid" {
		t.Fatalf("结论未写入: ok=%v verdict=%q", ok, p.Verdict)
	}
	if p.Credit != 0.5 || p.Tokens != 300 {
		t.Errorf("应记下证据（credit/tokens），实际 credit=%v tokens=%v", p.Credit, p.Tokens)
	}
	if p.LastProbeAt == 0 {
		t.Error("应记下学习时间")
	}
}

// TestObserveDoesNotOverwriteExistingVerdict 已有定论时不得覆盖 —— 这是最重要的一条。
//
// 被动证据弱于余额差分（credit 实测有「=0 但余额确实下降」的情形）。
// 拿弱证据推翻强结论的代价很大：把一个已确认收费的模型改成免费，
// 选号层会把流量全压到它上面。
func TestObserveDoesNotOverwriteExistingVerdict(t *testing.T) {
	probes := map[string]Probe{
		probeKey(auth.SiteCN, "m1"): {Verdict: "paid", Detail: "余额差分判定收费"},
	}
	c := newCatalogFromFixture(t, nil, probes)

	if c.Observe(auth.SiteCN, "m1", "free", 0, 500) {
		t.Error("已有定论时不该报告写入成功")
	}
	p, _ := c.ProbeOf(auth.SiteCN, "m1")
	if p.Verdict != "paid" {
		t.Fatalf("已有结论被覆盖成 %q —— 被动证据不该推翻余额差分", p.Verdict)
	}
	if p.Detail != "余额差分判定收费" {
		t.Errorf("原有 detail 应原样保留，实际 %q", p.Detail)
	}
}

// TestObserveFillsUnknownVerdict 已有条目但**尚未定论**（Verdict 为空）时应该填入。
//
// 这种形态来自主动探测「样本不足/余额无变化但不明确免费」留下的占位条目：
// 它有价值（带着上次探测时间），但不该挡住被动学习。
func TestObserveFillsUnknownVerdict(t *testing.T) {
	probes := map[string]Probe{
		probeKey(auth.SiteCN, "m1"): {Verdict: "", Detail: "余额无变化但样本过小，本次不判定"},
	}
	c := newCatalogFromFixture(t, nil, probes)

	if !c.Observe(auth.SiteCN, "m1", "free", 0, 400) {
		t.Fatal("未定论的占位条目应可被填入结论")
	}
	p, _ := c.ProbeOf(auth.SiteCN, "m1")
	if p.Verdict != "free" {
		t.Fatalf("结论应被填入，实际 %q", p.Verdict)
	}
}

// TestObserveRejectsBadInput 非法输入一律拒绝，且不改动台账。
func TestObserveRejectsBadInput(t *testing.T) {
	c := newCatalogFromFixture(t, nil, nil)
	cases := []struct{ site, model, verdict string }{
		{"", "m1", "paid"},             // 缺站点
		{auth.SiteCN, "", "paid"},      // 缺模型
		{auth.SiteCN, "m1", ""},        // 空结论
		{auth.SiteCN, "m1", "unknown"}, // 非法结论
	}
	for _, tc := range cases {
		if c.Observe(tc.site, tc.model, tc.verdict, 1, 300) {
			t.Errorf("非法输入 (%q,%q,%q) 不该被接受", tc.site, tc.model, tc.verdict)
		}
	}
	if len(c.Probes()) != 0 {
		t.Errorf("非法输入不该在台账里留下条目，实际 %d 条", len(c.Probes()))
	}
}

// TestObservePersistsToCache 学到的东西要落缓存，否则重启一次就丢光，
// 「关掉探测后价格仍可用」就成了空话。
func TestObservePersistsToCache(t *testing.T) {
	c := newCatalogFromFixture(t, nil, nil)
	c.Observe(auth.SiteCN, "m1", "paid", 0.25, 300)

	// 用同一个缓存文件重新构造目录。
	reloaded := New(c.cacheFile, nil, nil)
	reloaded.LoadCache()
	p, ok := reloaded.ProbeOf(auth.SiteCN, "m1")
	if !ok || p.Verdict != "paid" {
		t.Fatalf("被动学习结论应随缓存持久化，重载后得到 ok=%v verdict=%q", ok, p.Verdict)
	}
}

// TestObserveNormalizesSiteAndModel 站点/模型两侧都做 trim 与大小写归一
// （与 probeKey 同口径），否则「CN 站学的结论」查不到「cn 站」。
func TestObserveNormalizesSiteAndModel(t *testing.T) {
	c := newCatalogFromFixture(t, nil, nil)
	c.Observe(auth.SiteCN, "  DeepSeek-V4  ", "paid", 1, 300)

	// probeKey 对小写化，查询时用不同大小写也应命中。
	if p, ok := c.ProbeOf(auth.SiteCN, "deepseek-v4"); !ok || p.Verdict != "paid" {
		t.Fatalf("模型名应大小写归一，得到 ok=%v verdict=%q", ok, p.Verdict)
	}
}

// TestObserveDrivesKnownPaid 落地的结论要真的影响展示与选号判定。
func TestObserveDrivesKnownPaid(t *testing.T) {
	c := newCatalogFromFixture(t, nil, nil)
	c.Observe(auth.SiteCN, "m1", "paid", 0.9, 500)

	if !c.KnownPaid(auth.SiteCN, "m1") {
		t.Error("学到的收费结论应让 KnownPaid 为真")
	}
	if c.KnownFree(auth.SiteCN, "m1") {
		t.Error("收费模型不该同时被判为免费")
	}
	if got := c.DisplayMultiplier(auth.SiteCN, "m1"); got != "收费(倍率未知)" {
		t.Errorf("展示倍率应反映收费结论，实际 %q", got)
	}
}

// TestObserveCacheFileEmptyDoesNotPanic 没有缓存文件时（纯内存目录）不该炸。
func TestObserveCacheFileEmptyDoesNotPanic(t *testing.T) {
	c := &Catalog{probes: map[string]Probe{}}
	if !c.Observe(auth.SiteCN, "m1", "free", 0, 300) {
		t.Fatal("无缓存文件的目录也应能记下结论")
	}
	if _, ok := c.ProbeOf(auth.SiteCN, "m1"); !ok {
		t.Error("结论应记在内存里")
	}
}

// TestObserveCreatesProbesMapOnNil 台账 map 为 nil 时要能自己建起来。
func TestObserveCreatesProbesMapOnNil(t *testing.T) {
	c := &Catalog{}
	c.Observe(auth.SiteCN, "m1", "paid", 1, 300)
	if _, ok := c.ProbeOf(auth.SiteCN, "m1"); !ok {
		t.Error("nil 台账应被惰性初始化")
	}
}
