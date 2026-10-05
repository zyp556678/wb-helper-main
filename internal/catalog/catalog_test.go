package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/pool"
)

// newCatalogFromFixture 用缓存文件构造目录（避免测试依赖上游网络）。
func newCatalogFromFixture(t *testing.T, catalogs map[string][]Model, probes map[string]Probe) *Catalog {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	doc := cacheDoc{
		Schema:    cacheSchema,
		Source:    "test-fixture",
		FetchedAt: map[string]int64{auth.SiteCN: 1, auth.SiteINTL: 1},
		Catalogs:  catalogs,
		Probes:    probes,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("序列化测试缓存失败: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写入测试缓存失败: %v", err)
	}
	c := New(path, nil, (*pool.Pool)(nil))
	c.LoadCache()
	return c
}

// freeModel / paidModel 是构造目录条目的便捷函数。
func freeModel(id string) Model {
	return Model{ID: id, HasMultiplier: true, Multiplier: 0}
}

func paidModel(id string, m float64) Model {
	return Model{ID: id, HasMultiplier: true, Multiplier: m}
}

// TestPreferredFreeSitesTiltsWhenOneSideFree 验证核心规则：
// 一侧确认免费、另一侧确认收费 → 倾斜到免费那侧。
func TestPreferredFreeSitesTiltsWhenOneSideFree(t *testing.T) {
	c := newCatalogFromFixture(t,
		map[string][]Model{
			auth.SiteCN:   {freeModel("hy3")},
			auth.SiteINTL: {paidModel("hy3", 1.5)},
		}, nil)

	got := c.PreferredFreeSites("hy3")
	if len(got) != 1 || !got[auth.SiteCN] {
		t.Fatalf("期望倾斜到国内站，实际 %v", got)
	}
}

// TestPreferredFreeSitesNoTiltWhenBothPaid 验证两侧都收费时不倾斜
// （否则会把流量无理由压到单一站点）。
func TestPreferredFreeSitesNoTiltWhenBothPaid(t *testing.T) {
	c := newCatalogFromFixture(t,
		map[string][]Model{
			auth.SiteCN:   {paidModel("glm-5.2", 0.79)},
			auth.SiteINTL: {paidModel("glm-5.2", 0.85)},
		}, nil)

	if got := c.PreferredFreeSites("glm-5.2"); len(got) != 0 {
		t.Fatalf("两侧都收费时不应倾斜，实际 %v", got)
	}
}

// TestPreferredFreeSitesNoTiltWhenUnknown 验证「一侧未知」时不倾斜。
//
// 这是最重要的保守规则：拿未知当免费去倾斜，等于把流量压到可能收费的一侧 —— 那是真花钱。
func TestPreferredFreeSitesNoTiltWhenUnknown(t *testing.T) {
	c := newCatalogFromFixture(t,
		map[string][]Model{
			auth.SiteCN: {freeModel("kimi-k3-1")},
			// intl 上没有该模型条目，也没有探测结论 → 未知
		}, nil)

	if got := c.PreferredFreeSites("kimi-k3-1"); len(got) != 0 {
		t.Fatalf("另一侧结论未知时不应倾斜，实际 %v", got)
	}
}

// TestPreferredFreeSitesNoTiltWhenBothFree 验证两侧都免费时不倾斜。
func TestPreferredFreeSitesNoTiltWhenBothFree(t *testing.T) {
	c := newCatalogFromFixture(t,
		map[string][]Model{
			auth.SiteCN:   {freeModel("hy3")},
			auth.SiteINTL: {freeModel("hy3")},
		}, nil)

	if got := c.PreferredFreeSites("hy3"); len(got) != 0 {
		t.Fatalf("两侧都免费时不应倾斜，实际 %v", got)
	}
}

// TestPreferredFreeSitesTiltByProbe 验证探测结论也能驱动倾斜
// （上游不给倍率、只能实测的模型走这条路）。
func TestPreferredFreeSitesTiltByProbe(t *testing.T) {
	c := newCatalogFromFixture(t,
		map[string][]Model{
			auth.SiteCN:   {{ID: "deepseek-v4.1-flash"}},
			auth.SiteINTL: {{ID: "deepseek-v4.1-flash"}},
		},
		map[string]Probe{
			probeKey(auth.SiteCN, "deepseek-v4.1-flash"):   {Verdict: "free", LastProbeAt: 10},
			probeKey(auth.SiteINTL, "deepseek-v4.1-flash"): {Verdict: "paid", LastProbeAt: 11},
		})

	got := c.PreferredFreeSites("deepseek-v4.1-flash")
	if len(got) != 1 || !got[auth.SiteCN] {
		t.Fatalf("期望按探测结论倾斜到国内站，实际 %v", got)
	}
}

// TestKnownFreeIgnoresExpiredPromo 验证「促销已过期」不能算免费。
//
// 上游常把促销价固化进 credits：促销结束后 multiplier 仍是 0，但实际已经恢复收费。
// 把它当免费会同时污染展示与调度决策。
func TestKnownFreeIgnoresExpiredPromo(t *testing.T) {
	c := newCatalogFromFixture(t,
		map[string][]Model{
			auth.SiteCN: {{ID: "legacy-promo", HasMultiplier: true, Multiplier: 0, PromoExpired: true}},
		}, nil)

	if c.KnownFree(auth.SiteCN, "legacy-promo") {
		t.Fatal("促销已过期的 0 倍率不应被判为免费")
	}
}
