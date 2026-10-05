package pool

import (
	"path/filepath"
)

// 本文件实现「模型专属账号名单」（config.json 的 models.accounts）。
//
// 与 models.allowlist/blocklist 的区别：那两个控制「模型能不能被调用」，
// 这里控制「这个模型能用哪些**账号**」。典型用途是把某个模型钉死在特定凭据上
// （例如只有某几个账号开通了该模型的权限，或想让贵模型只走某个账号）。
//
// 三条判定规则，顺序不能换：
//  1. 未配置的模型 → 全部允许（不能因为「没配」就把模型打死）；
//  2. **同名凭据文件**同时存在于池里时 → 拒绝匹配。规则是按文件名写的，
//     同名意味着无法判断用户指的是哪一个，此时匹配任意一个都是猜；
//  3. 黑名单优先于白名单；白名单为空表示不限制。

// CredentialFileName 返回凭据文件名（不含目录）。
//
// 取 `filepath.Base(Path)` 而不是 AccountID()：后者是凭据**内容**里的 uid，
// 可能为空（部分凭据没有 uid），而规则表里写的是文件名。
// 加锁读取是因为 Cred 会在重新登录时被整体替换。
func (a *Account) CredentialFileName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Cred == nil {
		return ""
	}
	return filepath.Base(a.Cred.Path)
}

// ModelAccountRuleConfigured 报告该模型是否配了有效的账号名单。
func (p *Pool) ModelAccountRuleConfigured(model string) bool {
	rule, ok := p.cfg.ModelAccountRuleFor(model)
	return ok && rule.Configured()
}

// modelAccountAllowedLocked 判断账号是否被允许用于该模型。
// **调用方必须持有 p.mu**（需要遍历 p.accounts 检查同名凭据）。
func (p *Pool) modelAccountAllowedLocked(model string, acc *Account) (bool, string) {
	if acc == nil {
		return false, "账号为空"
	}
	rule, ok := p.cfg.ModelAccountRuleFor(model)
	if !ok || !rule.Configured() {
		return true, ""
	}

	name := acc.CredentialFileName()
	if name == "" {
		// 拿不到文件名就没法按名单判定。拒绝而不是放行：放行等于规则静默失效，
		// 而用户配这条规则的意图正是「别让别的账号用这个模型」。
		return false, "凭据文件名为空，无法匹配名单"
	}

	// 同名凭据重复：规则按文件名写，无法判断指的是哪一个。
	for _, other := range p.accounts {
		if other != acc && other.CredentialFileName() == name {
			return false, "凭据文件名重复"
		}
	}

	if rule.Block[name] {
		return false, "命中账号黑名单"
	}
	if len(rule.Allow) > 0 && !rule.Allow[name] {
		return false, "不在账号白名单"
	}
	return true, ""
}

// ModelAccountAllowed 是 modelAccountAllowedLocked 的加锁版本（供外部调用）。
func (p *Pool) ModelAccountAllowed(model string, acc *Account) (bool, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.modelAccountAllowedLocked(model, acc)
}

// AllowedAccountsForModel 统计通过账号名单的账号数（**不含**健康/冷却过滤）。
//
// 用途是区分两种「选不到号」：名单把账号全挡了（配置问题，重试无用，应报 403）
// 与账号都在冷却（临时状态，重试有意义）。只看健康候选数无法区分这两者。
func (p *Pool) AllowedAccountsForModel(model string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, a := range p.accounts {
		if ok, _ := p.modelAccountAllowedLocked(model, a); ok {
			n++
		}
	}
	return n
}
