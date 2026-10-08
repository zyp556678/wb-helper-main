package upstream

import "testing"

// TestGrowthAllowedIsOrthogonalToSite 守住「站点」与「账号类型」两道门控的正交性。
//
// 这是本次对齐上游 workbuddy2api-panel@7e3bc51 的核心：企业版账号**没有个人成长
// 体系**，而它可以出现在国内站 —— 所以「国内站」不代表能做成长任务。
// 四种组合必须各自判对，漏掉任何一格都会让某类账号每轮白发一批注定 400/403 的请求。
func TestGrowthAllowedIsOrthogonalToSite(t *testing.T) {
	cn := &Profile{Key: "cn"}
	intl := &Profile{Key: "intl"}
	personal := &CredentialView{}
	enterprise := &CredentialView{EnterpriseID: "ent-123"}
	// 只有空白字符的 enterpriseId 不算企业版（与 auth.Credential.IsEnterprise 同判据）。
	blankEnterprise := &CredentialView{EnterpriseID: "  "}

	cases := []struct {
		name string
		prof *Profile
		cred *CredentialView
		want bool
	}{
		{"国内站 + 个人版 → 可以", cn, personal, true},
		{"国内站 + 企业版 → 不可以（企业号没有个人成长体系）", cn, enterprise, false},
		{"国际站 + 个人版 → 不可以（没有成长中心）", intl, personal, false},
		{"国际站 + 企业版 → 不可以（两条都占）", intl, enterprise, false},
		{"国内站 + 空白 enterpriseId → 可以", cn, blankEnterprise, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := GrowthAllowed(c.prof, c.cred); got != c.want {
				t.Errorf("GrowthAllowed = %v，期望 %v", got, c.want)
			}
		})
	}
}

// TestGrowthAllowedDegradesToSiteCheckOnNilCredential 守住 nil 凭据不 panic。
//
// 凭据为 nil 时 IsEnterprise 返回 false，于是退化成「只看站点」——
// 这正是加这道门控之前的既有行为，不该因为新增判断而炸掉或收紧。
func TestGrowthAllowedDegradesToSiteCheckOnNilCredential(t *testing.T) {
	cn := &Profile{Key: "cn"}
	intl := &Profile{Key: "intl"}
	if !GrowthAllowed(cn, nil) {
		t.Error("国内站 + nil 凭据应退化成站点判断（可以）")
	}
	if GrowthAllowed(intl, nil) {
		t.Error("国际站 + nil 凭据仍应判为不可以")
	}
}

func TestCredentialViewIsEnterprise(t *testing.T) {
	cases := []struct {
		cred *CredentialView
		want bool
	}{
		{nil, false},
		{&CredentialView{}, false},
		{&CredentialView{EnterpriseID: "   "}, false},
		{&CredentialView{EnterpriseID: "ent-1"}, true},
	}
	for _, c := range cases {
		if got := c.cred.IsEnterprise(); got != c.want {
			t.Errorf("IsEnterprise(enterpriseID=%q) = %v，期望 %v",
				func() string {
					if c.cred == nil {
						return "<nil>"
					}
					return c.cred.EnterpriseID
				}(), got, c.want)
		}
	}
}
