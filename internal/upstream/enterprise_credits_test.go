package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy-gateway/internal/auth"
)

// 本文件的用例锚在两个**真实缺陷**上：
//
//  1. 企业号的积分资源明细走了个人口径的三个端点 —— 那对企业号恒返回空
//     （与额度汇总同一个坑，见 enterpriseQuota 注释），于是面板「积分明细」对
//     企业号永远是空的、选号层的到期快照也拿不到数据，而额度其实是有的。
//  2. 企业接口的 `credit` 是**本周期已用**，与个人口径的 `Remain` 语义相反。
//     照抄个人口径会把「已用 812」当成「还剩 812」，数字看着正常、方向完全反了。

// enterpriseCreditServer 起一个假上游：企业端点返回给定 body，
// 并记录收到的路径与请求头（用于断言走对了路、带对了企业头）。
//
// 注意 body 会被包进上游信封 `{"code":0,"data":…}`：doJSON 走的是信封协议，
// 直接返裸 JSON 会被解析成「data 为空」。
func enterpriseCreditServer(t *testing.T, body string, seen *[]string, headers *http.Header) *httptest.Server {
	t.Helper()
	wrapped := `{"code":0,"msg":"","data":` + body + `}`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = append(*seen, r.URL.Path)
		}
		if headers != nil {
			*headers = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(wrapped))
	}))
}

func enterpriseProfile(base string) *Profile {
	return &Profile{Key: auth.SiteCN, Label: "国内站", Base: base, Origin: base, Platform: "test"}
}

func enterpriseCred() *CredentialView {
	return &CredentialView{AccessToken: "test-token", UID: "u-test", EnterpriseID: "ent-1"}
}

// TestEnterpriseCreditResourcesUsesEnterpriseEndpoint 企业号必须走企业端点，
// 且带上 X-Enterprise-Id；不得再打个人口径的三个资源端点。
func TestEnterpriseCreditResourcesUsesEnterpriseEndpoint(t *testing.T) {
	var seen []string
	var got http.Header
	srv := enterpriseCreditServer(t, `{"credit":300,"limitNum":2000,"cycleResetTime":"2026-11-01 00:00:00"}`,
		&seen, &got)
	defer srv.Close()

	c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
	exp, err := c.FetchCreditResources(context.Background(), enterpriseCred(), enterpriseProfile(srv.URL))
	if err != nil {
		t.Fatalf("企业版资源明细查询失败: %v", err)
	}

	if len(seen) != 1 || seen[0] != "/v2/billing/meter/get-enterprise-user-usage" {
		t.Fatalf("企业号应只打企业端点一次，实际请求路径=%v", seen)
	}
	if got.Get("X-Enterprise-Id") != "ent-1" {
		t.Errorf("企业端点必须带 X-Enterprise-Id，实际 %q", got.Get("X-Enterprise-Id"))
	}
	if len(exp.Resources) != 1 {
		t.Fatalf("企业号应映射成一条资源，实际 %d 条", len(exp.Resources))
	}
}

// TestEnterpriseCreditResourcesSemanticsAreInverted 钉住最反直觉的那条：
// credit 是「已用」，故 剩余 = limitNum − credit。
func TestEnterpriseCreditResourcesSemanticsAreInverted(t *testing.T) {
	srv := enterpriseCreditServer(t, `{"credit":300,"limitNum":2000}`, nil, nil)
	defer srv.Close()

	c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
	exp, err := c.FetchCreditResources(context.Background(), enterpriseCred(), enterpriseProfile(srv.URL))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	res := exp.Resources[0]
	if res.Total != 2000 {
		t.Errorf("总量应为 limitNum=2000，实际 %v", res.Total)
	}
	if res.Used != 300 {
		t.Errorf("已用应为 credit=300，实际 %v", res.Used)
	}
	if res.Remaining != 1700 {
		t.Errorf("剩余应为 limitNum−credit=1700，实际 %v（照抄个人口径会得到 300）", res.Remaining)
	}
	if exp.TotalRemaining != 1700 {
		t.Errorf("汇总剩余应为 1700，实际 %v", exp.TotalRemaining)
	}
}

// TestEnterpriseCreditResourcesUnlimited limitNum < 0 表示不限量：
// 不报错、不显示成「额度已用尽」（Total/Remaining 留 0，靠名字说明）。
func TestEnterpriseCreditResourcesUnlimited(t *testing.T) {
	srv := enterpriseCreditServer(t, `{"credit":0,"limitNum":-1}`, nil, nil)
	defer srv.Close()

	c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
	exp, err := c.FetchCreditResources(context.Background(), enterpriseCred(), enterpriseProfile(srv.URL))
	if err != nil {
		t.Fatalf("不限量不应报错: %v", err)
	}
	res := exp.Resources[0]
	if res.PackageName != "企业版（不限量）" {
		t.Errorf("不限量应在名字上说明，实际 %q", res.PackageName)
	}
	if res.Total != 0 || res.Remaining != 0 {
		t.Errorf("不限量时不该编造总量/剩余，实际 total=%v remaining=%v", res.Total, res.Remaining)
	}
}

// TestEnterpriseCreditResourcesOveruseClamped 超额使用时剩余钳到 0，
// 不出现负的「剩余」。
func TestEnterpriseCreditResourcesOveruseClamped(t *testing.T) {
	srv := enterpriseCreditServer(t, `{"credit":2500,"limitNum":2000}`, nil, nil)
	defer srv.Close()

	c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
	exp, err := c.FetchCreditResources(context.Background(), enterpriseCred(), enterpriseProfile(srv.URL))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got := exp.Resources[0].Remaining; got != 0 {
		t.Errorf("超额时剩余应钳到 0，实际 %v", got)
	}
}

// TestEnterpriseCreditResourcesResetTimeAsExpiry cycleResetTime 是企业号唯一的
// 「到期」概念，要落在 ExpireAt 上（否则选号层的到期快照拿不到数据）。
func TestEnterpriseCreditResourcesResetTimeAsExpiry(t *testing.T) {
	srv := enterpriseCreditServer(t, `{"credit":10,"limitNum":100,"cycleResetTime":"2099-11-01 00:00:00"}`, nil, nil)
	defer srv.Close()

	c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
	exp, err := c.FetchCreditResources(context.Background(), enterpriseCred(), enterpriseProfile(srv.URL))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if exp.Resources[0].ExpireAt <= 0 {
		t.Fatalf("cycleResetTime 应被解析成到期时间，实际 %v", exp.Resources[0].ExpireAt)
	}
	if exp.SoonestExpireAt <= 0 {
		t.Errorf("有到期时间时 SoonestExpireAt 应被填上，实际 %v", exp.SoonestExpireAt)
	}
}

// TestEnterpriseCreditResourcesSnakeCaseFields 字段命名两套都收
// （官方两处实现分别读 camelCase 与 snake_case）。
func TestEnterpriseCreditResourcesSnakeCaseFields(t *testing.T) {
	srv := enterpriseCreditServer(t, `{"used_num":77,"limit_num":500}`, nil, nil)
	defer srv.Close()

	c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
	exp, err := c.FetchCreditResources(context.Background(), enterpriseCred(), enterpriseProfile(srv.URL))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	res := exp.Resources[0]
	if res.Total != 500 || res.Used != 77 || res.Remaining != 423 {
		t.Errorf("snake_case 字段未被接收: total=%v used=%v remaining=%v", res.Total, res.Used, res.Remaining)
	}
}

// TestPersonalCreditResourcesUnchanged 个人号仍走原来的三路端点，
// 不被企业分流误伤。
func TestPersonalCreditResourcesUnchanged(t *testing.T) {
	// 三路端点是**并发**请求的，handler 会在多个 goroutine 里同时到 ——
	// 裸 slice 会数据竞争，且 append 可能互相覆盖导致少记一条（表现为
	// 偶发「实际 2 路」）。加锁收集。
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"Packages":[]}}`))
	}))
	defer srv.Close()

	c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
	// 无 EnterpriseID 的凭据 = 个人号
	cred := &CredentialView{AccessToken: "test-token", UID: "u-test"}
	if _, err := c.FetchCreditResources(context.Background(), cred, enterpriseProfile(srv.URL)); err != nil {
		t.Fatalf("个人号资源明细查询失败: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, p := range seen {
		if strings.Contains(p, "get-enterprise-user-usage") {
			t.Errorf("个人号不该打企业端点，实际请求=%v", seen)
		}
	}
	if len(seen) != 3 {
		t.Errorf("个人号应打三路资源端点，实际 %d 路: %v", len(seen), seen)
	}
}

// TestEnterpriseQuotaStillCorrect 额度汇总路径的重构不应改变行为：
// total=limitNum、remain=limitNum−credit、limitNum<0 为不限量。
func TestEnterpriseQuotaStillCorrect(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantOK  bool
		total   float64
		used    float64
		remain  float64
		unlimit bool
	}{
		{"正常", `{"credit":812.45,"limitNum":2000}`, true, 2000, 812.45, 1187.55, false},
		{"不限量", `{"credit":0,"limitNum":-1}`, true, 0, 0, 0, true},
		{"超额", `{"credit":3000,"limitNum":2000}`, true, 2000, 3000, 0, false},
		{"snake", `{"used_num":100,"limit_num":800}`, true, 800, 100, 700, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := enterpriseCreditServer(t, tc.body, nil, nil)
			defer srv.Close()
			c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
			q, err := c.FetchQuota(context.Background(), enterpriseCred(), enterpriseProfile(srv.URL))
			if err != nil {
				t.Fatalf("额度查询失败: %v", err)
			}
			if q.Total != tc.total || q.Used != tc.used || q.Remaining != tc.remain {
				t.Errorf("total=%v used=%v remaining=%v，期望 %v/%v/%v",
					q.Total, q.Used, q.Remaining, tc.total, tc.used, tc.remain)
			}
			wantPlan := "企业版"
			if tc.unlimit {
				wantPlan = "企业版（不限量）"
			}
			if q.Plan != wantPlan {
				t.Errorf("Plan=%q，期望 %q", q.Plan, wantPlan)
			}
			if !q.Paid {
				t.Errorf("企业版应标记为付费")
			}
			// 不限量与「剩余 0」必须可区分：上层据此跳过积分保底，
			// 面板据此显示「不限」而不是「已用尽」。
			if q.Unlimited != tc.unlimit {
				t.Errorf("Unlimited=%v，期望 %v", q.Unlimited, tc.unlimit)
			}
		})
	}
}

// TestEnterpriseUsageParsesResetTime 直接钉住解析函数对重置时间的处理：
// 秒级时间戳要换算成毫秒，解析不出时留 0 而不报错。
func TestEnterpriseUsageParsesResetTime(t *testing.T) {
	srv := enterpriseCreditServer(t, `{"limitNum":10,"credit":1,"cycleResetTime":1800000000}`, nil, nil)
	defer srv.Close()

	c := &Client{Control: srv.Client(), ChatHTTP: srv.Client()}
	usage, err := c.fetchEnterpriseUsage(context.Background(), enterpriseProfile(srv.URL),
		func(r *http.Request) {})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 1800000000 秒 → 毫秒级
	if usage.ResetAt != 1800000000*1000 {
		t.Errorf("秒级时间戳应换算成毫秒，实际 %v", usage.ResetAt)
	}
}
