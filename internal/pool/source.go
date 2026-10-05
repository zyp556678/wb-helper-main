package pool

import (
	"context"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/upstream"
)

// 本文件实现目录层需要的账号能力。
//
// 锁契约（很重要，踩过两次坑）：
//
//	sync.Mutex 不可重入。探测流程会**长时间持有某个账号的串行锁**
//	（读余额 → 发请求 → 再读余额，全程不能被其他请求插进来污染余额差分）。
//	因此凡是设计为「调用方已持锁」的方法，一律命名为 ...NoLock，
//	内部绝不调用任何会加锁的方法（包括 View / IsDisabled / Find）。
//
//	另外锁序统一为「先池锁 p.mu、后账号锁 a.mu」。探测路径如果持着账号锁再回头
//	去取池锁（例如按 ID 回查账号），就会与选号路径形成锁序反转而死锁，
//	所以探测拿到的是 *Account 指针，而不是每次按 ID 回查。

// Sites 返回当前有账号的站点集合（cn 在前，顺序稳定）。
func (p *Pool) Sites() []string {
	has := map[string]bool{}
	for _, a := range p.Accounts() {
		has[a.Site()] = true
	}
	out := make([]string, 0, 2)
	for _, s := range []string{auth.SiteCN, auth.SiteINTL} {
		if has[s] {
			out = append(out, s)
		}
	}
	return out
}

// AccountsForSiteCount 返回某站点的账号数。
func (p *Pool) AccountsForSiteCount(site string) int {
	n := 0
	for _, a := range p.Accounts() {
		if a.Site() == site {
			n++
		}
	}
	return n
}

// CredentialForSite 取该站点一个「未禁用且令牌非空」的账号，用于拉取目录
// （只读请求，不需要独占账号，短暂持锁读一次即可）。
func (p *Pool) CredentialForSite(site string) (string, *upstream.CredentialView, *upstream.Profile, bool) {
	for _, a := range p.Accounts() {
		if a.Site() != site {
			continue
		}
		if a.IsDisabled() {
			continue
		}
		if a.Cred.AccessToken == "" {
			continue
		}
		return a.Cred.AccountID(), a.View(), a.Profile(), true
	}
	return "", nil, nil, false
}

// AcquireForProbe 取该站点一个适合探测的账号并**独占其串行锁**，返回该账号与释放函数。
//
// 挑选顺序：跳过已禁用与「已知额度为 0」的账号（后者探测必然 402，白跑），
// 同等条件下取剩余额度最大的，尽量少影响正在服务用户的账号。
func (p *Pool) AcquireForProbe(ctx context.Context, site string) (*Account, func(), bool) {
	now := time.Now()
	var (
		best        *Account
		bestCredits float64 = -1
	)

	// 先用 Accounts() 取快照（内部会释放池锁），避免持账号锁时再去取池锁。
	// 注意：这里逐个短锁读状态，读的是值而不是引用，不会有持锁嵌套。
	for _, a := range p.Accounts() {
		if a.Site() != site {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		a.mu.Lock()
		eligible := !a.disabled && a.healthyLocked(now) && a.Cred.AccessToken != ""
		known := a.quotaKnown
		remaining := a.quotaRemaining
		a.mu.Unlock()

		if !eligible {
			continue
		}
		if known && remaining <= 0 {
			continue // 额度为 0 的账号探测必然失败
		}
		score := remaining
		if !known {
			score = 0.0001 // 额度未知：可用但优先级低于已知有余额的账号
		}
		if score > bestCredits {
			bestCredits = score
			best = a
		}
	}
	if best == nil {
		return nil, nil, false
	}

	best.mu.Lock()
	// 加锁后复核：等待期间账号可能被禁用。
	// 这里必须读字段而不是调 IsDisabled()，否则会重复加锁自锁。
	if best.disabled || best.Cred.AccessToken == "" {
		best.mu.Unlock()
		return nil, nil, false
	}
	return best, best.mu.Unlock, true
}

// -----------------------------------------------------------------------------
// 供「调用方已持账号锁」的路径使用的免锁访问器
// -----------------------------------------------------------------------------

// ViewNoLock 返回凭据视图。调用方必须已持有该账号的锁。
func (a *Account) ViewNoLock() *upstream.CredentialView {
	c := a.Cred
	return &upstream.CredentialView{
		AccessToken:  c.AccessToken,
		RefreshToken: c.RefreshToken,
		UID:          c.UID,
		EnterpriseID: c.EnterpriseID,
		Domain:       c.Domain,
		DeviceToken:  c.DeviceToken,
	}
}

// ID 返回账号标识（凭据文件名；只读不可变字段，无需加锁）。
func (a *Account) ID() string { return a.Cred.AccountID() }

// ProfileRef 返回站点参数（只读不可变字段，无需加锁）。
func (a *Account) ProfileRef() *upstream.Profile { return a.Profile() }

// QuotaRemainingNoLock 读剩余额度。调用方必须已持有该账号的锁。
func (a *Account) QuotaRemainingNoLock() float64 { return a.quotaRemaining }

// DisabledNoLock 读禁用状态。调用方必须已持有该账号的锁。
//
// 与 IsDisabled 的区别只在锁：IsDisabled 会自己加锁，在「已持锁」的路径上调用
// 会直接自锁。指定账号探测（/admin/probe）正是这样一条路径 —— 它必须先持锁
// 才能用 ...NoLock 系列读余额，于是判定禁用状态也只能走免锁版本。
func (a *Account) DisabledNoLock() bool { return a.disabled }

// SetQuotaNoLock 写回额度快照。调用方必须已持有该账号的锁。
func (a *Account) SetQuotaNoLock(q upstream.Quota) {
	a.quotaKnown = true
	a.quotaTotal = q.Total
	a.quotaUsed = q.Used
	a.quotaRemaining = q.Remaining
	a.quotaPlan = q.Plan
	a.quotaPaid = q.Paid
	a.quotaUpdatedAt = time.Now().Unix()
}

// ReviveIfCreditsRecoveredNoLock 额度恢复解冻。调用方必须已持有该账号的锁。
func (a *Account) ReviveIfCreditsRecoveredNoLock(remaining float64) bool {
	if remaining <= 0 {
		return false
	}
	if a.coolKind == CoolHard && time.Now().Before(a.cooldownUntil) {
		a.cooldownUntil = time.Time{}
		a.cooldownReason = ""
		a.coolKind = CoolNone
		return true
	}
	return false
}

// RefreshQuotaNoLock 查询并写回额度。**调用方必须已持有该账号的锁**，
// 否则并发请求会污染余额差分读数。
func (p *Pool) RefreshQuotaNoLock(ctx context.Context, a *Account) (float64, error) {
	q, err := p.client.FetchQuota(ctx, a.ViewNoLock(), a.ProfileRef())
	if err != nil {
		return 0, err
	}
	a.SetQuotaNoLock(q)
	return a.QuotaRemainingNoLock(), nil
}
