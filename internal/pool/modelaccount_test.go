package pool

import (
	"testing"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/config"
)

// setModelAccountRules 给测试池装上模型账号名单。
func setModelAccountRules(p *Pool, model string, allow, block []string) {

	r := config.ModelAccountRule{}
	if len(allow) > 0 {
		r.Allow = map[string]bool{}
		for _, n := range allow {
			r.Allow[n] = true
		}
	}
	if len(block) > 0 {
		r.Block = map[string]bool{}
		for _, n := range block {
			r.Block[n] = true
		}
	}
	// 与已有的全局规则（models.accounts.all）合并，而不是整表替换：
	// 测试里常先装全局层再装模型层，替换会把刚配的全局层抹掉。
	if p.config().ModelAccountRules == nil {
		p.config().ModelAccountRules = map[string]config.ModelAccountRule{}
	}
	p.config().ModelAccountRules[model] = r
}

// TestModelAccountRuleBlocksUnlistedAccount 白名单之外的账号不得被选中。
func TestModelAccountRuleBlocksUnlistedAccount(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	// 两个账号的文件名分别是 cn-a.json / cn-b.json（见 newTestPool）
	setModelAccountRules(p, "glm-5.3", []string{"cn-a.json"}, nil)

	for i := 0; i < 20; i++ {
		acc, err := p.PickAccount(PickOptions{Model: "glm-5.3"})
		if err != nil {
			t.Fatalf("第 %d 次选号失败: %v", i, err)
		}
		if acc.CredentialFileName() != "cn-a.json" {
			t.Fatalf("选到了白名单外的账号 %s", acc.CredentialFileName())
		}
		acc.ReleaseInFlight()
	}
}

// TestModelAccountRuleBlockWinsOverAllow 同名同时出现在两个名单时，黑名单优先。
func TestModelAccountRuleBlockWinsOverAllow(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	setModelAccountRules(p, "glm-5.3", []string{"cn-a.json"}, []string{"cn-a.json"})

	ok, reason := p.ModelAccountAllowed("glm-5.3", p.accounts[0])
	if ok {
		t.Fatal("黑名单应优先于白名单")
	}
	if reason != "命中账号黑名单" {
		t.Fatalf("拒绝原因应为黑名单，实际 %q", reason)
	}
}

// TestModelAccountRuleUnconfiguredModelAllowsAll 未配置的模型不受任何限制。
//
// 这条是防止「配了一个模型就把别的模型全打死」—— 规则表按模型名索引，
// 查不到就必须放行，不能拿别的模型的规则去套。
func TestModelAccountRuleUnconfiguredModelAllowsAll(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	setModelAccountRules(p, "glm-5.3", []string{"cn-a.json"}, nil)

	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		acc, err := p.PickAccount(PickOptions{Model: "kimi-k3"})
		if err != nil {
			t.Fatalf("未配置的模型应能正常选号: %v", err)
		}
		seen[acc.CredentialFileName()] = true
		acc.ReleaseInFlight()
	}
	if len(seen) < 2 {
		t.Fatalf("未配置的模型不该被限制到单账号，实际只用到 %v", seen)
	}
	if p.ModelAccountRuleConfigured("kimi-k3") {
		t.Fatal("未配置的模型不该报告「已配置规则」")
	}
}

// TestModelAccountRuleModelNameCaseInsensitive 模型名大小写不敏感。
func TestModelAccountRuleModelNameCaseInsensitive(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	setModelAccountRules(p, "glm-5.3", nil, []string{"cn-a.json"})

	if ok, _ := p.ModelAccountAllowed("GLM-5.3", p.accounts[0]); ok {
		t.Fatal("大写模型名应命中同一条规则")
	}
	if ok, _ := p.ModelAccountAllowed("  glm-5.3  ", p.accounts[0]); ok {
		t.Fatal("带空格的模型名应命中同一条规则")
	}
}

// TestModelAccountRuleRejectsDuplicateFileNames 同名凭据同时存在时拒绝匹配。
//
// 规则是按文件名写的，池里有两个同名文件就无法判断用户指的是哪一个 ——
// 此时匹配任意一个都是猜，猜错等于把规则应用到了错误的账号上。
func TestModelAccountRuleRejectsDuplicateFileNames(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	// 人为制造同名：把第二个账号的路径改成与第一个相同
	p.accounts[1].mu.Lock()
	p.accounts[1].Cred.Path = p.accounts[0].Cred.Path
	p.accounts[1].mu.Unlock()

	setModelAccountRules(p, "glm-5.3", []string{"cn-a.json"}, nil)

	if ok, reason := p.ModelAccountAllowed("glm-5.3", p.accounts[0]); ok {
		t.Fatal("同名凭据应拒绝匹配")
	} else if reason != "凭据文件名重复" {
		t.Fatalf("拒绝原因应为文件名重复，实际 %q", reason)
	}
}

// TestAllowedAccountsForModelIgnoresHealth 统计的是「通过名单的账号数」，
// 与健康/冷却无关 —— 这正是它与「健康候选数」的区别所在。
func TestAllowedAccountsForModelIgnoresHealth(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	setModelAccountRules(p, "glm-5.3", nil, []string{"cn-a.json"})

	if n := p.AllowedAccountsForModel("glm-5.3"); n != 1 {
		t.Fatalf("应只有 1 个账号通过名单，实际 %d", n)
	}

	// 把剩下的账号打进硬冷却：健康候选归零，但「通过名单数」不该变。
	p.accounts[1].CooldownHard("余额耗尽")
	if n := p.AllowedAccountsForModel("glm-5.3"); n != 1 {
		t.Fatalf("冷却不应影响「通过名单」统计，实际 %d", n)
	}
}

// TestModelAccountRuleAllBlockedYieldsNoAccount 名单把全部账号挡掉时选号失败。
//
// 这是 403 model_account_disabled 的触发条件：规则已配置且通过名单数为 0。
func TestModelAccountRuleAllBlockedYieldsNoAccount(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	setModelAccountRules(p, "glm-5.3", []string{"nonexistent.json"}, nil)

	if _, err := p.PickAccount(PickOptions{Model: "glm-5.3"}); err == nil {
		t.Fatal("名单挡掉全部账号时应当选号失败")
	}
	if !p.ModelAccountRuleConfigured("glm-5.3") {
		t.Fatal("该模型应报告「已配置规则」")
	}
	if n := p.AllowedAccountsForModel("glm-5.3"); n != 0 {
		t.Fatalf("通过名单的账号数应为 0，实际 %d", n)
	}
}

// TestModelAccountRuleNotBypassedByCooldownFallback 全冷却兜底也不能绕过名单。
//
// 兜底路径是「账号都不可用时再捞一个」，很容易被写成「先捞了再说」。
// 但名单是配置层硬约束，绕过它等于「账号一冷却规则就失效」。
//
// 注意断言方向：兜底**本来就允许返回冷却中的账号**（这正是它存在的意义），
// 所以不能断言「必须失败」，而要断言「返回的账号必须通过名单」。
// 让两个账号都进软冷却以真正走到兜底分支。
func TestModelAccountRuleNotBypassedByCooldownFallback(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	setModelAccountRules(p, "glm-5.3", []string{"cn-b.json"}, nil)

	for _, a := range p.accounts {
		a.CooldownSoft(time.Minute, time.Time{}, "上游限流")
	}

	acc, err := p.PickAccount(PickOptions{Model: "glm-5.3"})
	if err != nil {
		// 兜底也捞不到是可接受的结果（规则确实生效了）
		return
	}
	if acc.CredentialFileName() != "cn-b.json" {
		t.Fatalf("兜底路径捞出了名单外的账号 %s", acc.CredentialFileName())
	}
}

// setGlobalAccountRule 给测试池装上**全局**账号名单（models.accounts.all）。
func setGlobalAccountRule(p *Pool, allow, block []string) {
	r := config.ModelAccountRule{}
	if len(allow) > 0 {
		r.Allow = map[string]bool{}
		for _, n := range allow {
			r.Allow[n] = true
		}
	}
	if len(block) > 0 {
		r.Block = map[string]bool{}
		for _, n := range block {
			r.Block[n] = true
		}
	}
	if p.config().ModelAccountRules == nil {
		p.config().ModelAccountRules = map[string]config.ModelAccountRule{}
	}
	p.config().ModelAccountRules[config.AccountsAllKey] = r
}

// TestModelAccountRuleGlobalAllBlocks 全局 all 层能单独挡账号（不必给每个模型都写一遍）。
func TestModelAccountRuleGlobalAllBlocks(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	setGlobalAccountRule(p, nil, []string{"cn-a.json"})

	// 未配任何模型专属规则，全局层照样生效 —— 而且对**任意**模型生效。
	if ok, reason := p.ModelAccountAllowed("随便一个模型", p.accounts[0]); ok {
		t.Fatal("全局名单应挡住该账号")
	} else if reason != "命中全局账号黑名单" {
		t.Fatalf("拒绝原因应指明是全局层，实际 %q", reason)
	}
	if ok, _ := p.ModelAccountAllowed("随便一个模型", p.accounts[1]); !ok {
		t.Fatal("全局名单之外的账号应放行")
	}
	if !p.ModelAccountRuleConfigured("随便一个模型") {
		t.Fatal("配了全局名单后，任何模型都应报告「已有账号名单约束」")
	}
	if n := p.AllowedAccountsForModel("随便一个模型"); n != 1 {
		t.Fatalf("通过全局名单的账号数应为 1，实际 %d", n)
	}
}

// TestModelAccountRuleGlobalAndModelAreTwoLayers 全局层与模型层是**两层 AND**：
// 任一层拒绝即拒绝，只有两层都通过才放行。
func TestModelAccountRuleGlobalAndModelAreTwoLayers(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	setGlobalAccountRule(p, nil, []string{"cn-a.json"})
	// 模型层同时也是白名单：cn-a 被模型层允许，但被全局层挡住。
	setModelAccountRules(p, "glm-5.3", []string{"cn-a.json", "cn-b.json"}, nil)

	if ok, reason := p.ModelAccountAllowed("glm-5.3", p.accounts[0]); ok {
		t.Fatal("模型层允许但全局层拒绝时应拒绝")
	} else if reason != "命中全局账号黑名单" {
		t.Fatalf("应先报全局层，实际 %q", reason)
	}
	// cn-b 两层都通过。
	if ok, reason := p.ModelAccountAllowed("glm-5.3", p.accounts[1]); !ok {
		t.Fatalf("两层都通过时应放行，实际 %q", reason)
	}
}

// TestModelAccountRuleGlobalAllowlistBlocksUnlisted 全局白名单非空时只放行列表内账号。
func TestModelAccountRuleGlobalAllowlistBlocksUnlisted(t *testing.T) {
	p := newTestPool(t, auth.SiteCN, auth.SiteCN)
	setGlobalAccountRule(p, []string{"cn-b.json"}, nil)

	if ok, reason := p.ModelAccountAllowed("glm-5.3", p.accounts[0]); ok {
		t.Fatal("全局白名单外的账号应被拒绝")
	} else if reason != "不在全局账号白名单" {
		t.Fatalf("拒绝原因应指明是全局白名单，实际 %q", reason)
	}
	if ok, _ := p.ModelAccountAllowed("glm-5.3", p.accounts[1]); !ok {
		t.Fatal("全局白名单内的账号应放行")
	}
}
