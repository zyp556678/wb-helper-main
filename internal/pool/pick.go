// 选号（切片 2）：三因子加权 Top5 短名单 + 加权随机 + 防惊群 + 全冷却兜底 + 在途过滤。
//
// 权重口径（对齐 wb2api-panel 现行实现）：
//
//		weight = credits 比例 × 10 + 闲置补偿 + 快过期积分加成
//
//	  - credits 比例 = 该号剩余积分 / 候选集最大剩余积分（避免量纲爆炸）
//	  - 闲置补偿 = min(闲置小时 × idleWeightPerHour, idleWeightMax)，从未使用给满分
//	  - 快过期加成 = 快过期积分 / 总积分 × 8（让快作废的积分优先被消耗）
//
// 为什么不用「成功率」因子：errTotal 是终身累计、只增不减，成功率 = success/(success+err)
// 会让早期出过错的号被永久压权且永不恢复；瞬时健康信号已由冷却/熔断/连败降权承接。
//
// Top5 截断而不是直接取加权最大：截断后仍在短名单内加权随机，打散热点避免永远打同一账号。
package pool

import (
	"math/rand/v2"
	"sort"
	"time"
)

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非短名单全部刚被用过）。
var minPickGap = 100 * time.Millisecond

// PickOptions 是选号条件。
type PickOptions struct {
	// Exclude 中的账号会被跳过（请求级轮换）。
	Exclude map[*Account]bool
	// Model 非空时启用模型感知健康判定（6004 模型豁免生效）。
	Model string
	// Site 非空时只在该站点内选号（双站分池）。
	Site string
	// PreferredSites 是「免费站点优先」的白名单。
	//
	// 语义是**软优先**而非硬过滤：只有当这些站点里确实存在健康候选时才收窄范围；
	// 若它们全都冷却/在途占满/被排除，就自动回落到全部站点，而不是报「无可用账号」。
	// 这样做的理由是：优先只是省钱手段，不该在免费站不可用时让请求直接失败。
	PreferredSites map[string]bool
	// Pin 指定的账号优先（会话粘性命中时使用）；不可用时回落到正常选号。
	Pin *Account
	// PaidSitesForModel 是「该模型在这些站点上被实测为收费」的站点集合。
	//
	// 与积分保底配合使用：只对**实测收费**（tier 2）的站点施加余额保底。
	// 免费（tier 0）与无观测（tier 1）的站点**必须不受限** ——
	// 否则账本被清空（重启 / 过期）后，触底账号会被永久锁在「学不回来」的死锁里：
	// 它拿不到任何请求，也就永远没有机会证明这个模型其实是免费的。
	PaidSitesForModel map[string]bool
}

// PickAccount 按当前策略选一个账号。返回 nil 表示无可用账号。
func (p *Pool) PickAccount(opts PickOptions) (*Account, error) {
	if p.Strategy == "roundrobin" {
		return p.pickRoundRobin(opts)
	}
	return p.pickWeightedAccount(opts)
}

// pickWeightedAccount 加权随机选号。
func (p *Pool) pickWeightedAccount(opts PickOptions) (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// 会话粘性命中：指定的账号仍健康且未被排除时优先复用，保证多轮上下文不跳号
	if opts.Pin != nil && !(opts.Exclude != nil && opts.Exclude[opts.Pin]) {
		if p.eligible(opts.Pin, opts, now) {
			p.noteSelectedLocked(opts.Pin, now)
			return opts.Pin, nil
		}
	}

	acquired := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if opts.Exclude != nil && opts.Exclude[a] {
			continue
		}
		a.pruneExpiredModelCooldowns(now)
		if opts.Site != "" && a.Site() != opts.Site {
			continue
		}
		// 模型专属账号名单：配了就必须遵守，连失败换号也不能绕过。
		if ok, _ := p.modelAccountAllowedLocked(opts.Model, a); !ok {
			continue
		}
		// 积分保底：与 eligible 共用同一条判据（见 creditFloorBlocks）。
		// 这里必须**单独写一次**而不是指望 eligible —— 这条路径是内联判断的，
		// 早期就是在这一步漏掉，导致保底只在粘性路径生效、普通选号照放。
		if p.creditFloorBlocks(a, opts) {
			continue
		}
		if !a.healthyForModel(now, opts.Model) {
			continue
		}
		if !a.AcquireInFlight(p.inFlightLimit(a)) {
			continue // 在途占满：跳过（上限 0 时恒可取）
		}
		acquired = append(acquired, a)
	}

	if len(acquired) == 0 {
		return p.pickEarliestExpiryLocked(opts, now)
	}

	// 免费站点优先：只在这些站点内再筛一道；筛完为空则不做倾斜（软优先，见 PickOptions 注释）。
	cands := acquired
	if len(opts.PreferredSites) > 0 {
		pref := make([]*Account, 0, len(acquired))
		for _, a := range acquired {
			if opts.PreferredSites[a.Site()] {
				pref = append(pref, a)
			}
		}
		if len(pref) > 0 {
			cands = pref
		}
	}

	// 预计算权重与 credits 基准（O(n)），避免在比较器里现算
	maxCredits := 0.0
	weights := make(map[*Account]float64, len(cands))
	for _, a := range cands {
		if c := a.Credits(); c > maxCredits {
			maxCredits = c
		}
	}
	g := p.gov()
	for _, a := range cands {
		weights[a] = weightOf(a, maxCredits, now, g)
	}

	// 等权重且候选多于 5 个时先洗牌：否则按字典序截断会让靠后的等权重账号永远进不了短名单
	if len(cands) > 5 {
		eq := false
		for i := 1; i < len(cands); i++ {
			if weights[cands[i]] == weights[cands[0]] {
				eq = true
				break
			}
		}
		if eq {
			shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(cands))))
			shuf.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		wi, wj := weights[cands[i]], weights[cands[j]]
		if wi != wj {
			return wi > wj
		}
		return cands[i].Cred.AccountID() < cands[j].Cred.AccountID() // 稳定兜底
	})

	all := cands // 截断前的全量（权重降序），供 LRU 兜底用
	shortlist := cands
	if len(shortlist) > 5 {
		shortlist = shortlist[:5]
	}

	// 防惊群：先排除窗口内刚被选中的账号
	eligible := make([]*Account, 0, len(shortlist))
	for _, a := range shortlist {
		if p.lastPickGapOK(a, now) {
			eligible = append(eligible, a)
		}
	}

	var chosen *Account
	if len(eligible) == 0 {
		// 短名单全部刚被用过：在全量里按选中序号取最旧者（usedSeq 是严格全序，
		// 不依赖 time.Now() 精度，避免 Windows 上毫秒级精度导致恒选同一个）
		chosen = all[0]
		for _, c := range all[1:] {
			if c.usedSeqLocked() < chosen.usedSeqLocked() {
				chosen = c
			}
		}
	} else {
		chosen = weightedRandom(eligible, weights)
	}

	// 落选者释放在途名额（只有被选中的那个保留占用，交由调用方 Release）。
	// 注意遍历的是 acquired 而不是 cands：被免费站点优先筛掉的那些也占着在途名额，
	// 漏掉它们会让名额被白占，几次之后该账号就再也选不进来。
	for _, a := range acquired {
		if a != chosen {
			a.ReleaseInFlight()
		}
	}
	p.noteSelectedLocked(chosen, now)
	return chosen, nil
}

// eligible 判断单个账号（粘性指定）当前是否可用；不可用时不占用在途名额。
func (p *Pool) eligible(a *Account, opts PickOptions, now time.Time) bool {
	if opts.Site != "" && a.Site() != opts.Site {
		return false
	}
	// 粘性命中也要过账号名单：规则是硬约束，不因为会话粘性而豁免。
	if ok, _ := p.modelAccountAllowedLocked(opts.Model, a); !ok {
		return false
	}
	// 积分保底（credit_floor）见 creditFloorBlocks 的注释。
	if p.creditFloorBlocks(a, opts) {
		return false
	}
	if !a.healthyForModel(now, opts.Model) {
		return false
	}
	return a.AcquireInFlight(p.inFlightLimit(a))
}

// creditFloorBlocks 报告该账号是否因**积分保底**而不可用。
//
// 抽成共用判据是必需的，不是为了整洁：选号有两条独立路径
// （pickWeightedAccount 内联判断、eligible 供粘性/轮询用），
// 把条件写两处的结果是「改一处漏一处」，而症状极难看出 ——
// 「会话粘性命中的账号被保底拦住、普通选号却照放」，
// 会让人以为是粘性功能坏了。
//
// 为什么值得拦：收费请求会把最后一点余额打穿，而余额归零后连**免费模型**
// 都会因「余额不足」被冷却到次日签到 —— 那是最坏 11 小时不可用。
// 留一点余额，免费模型就还能跑。
//
// 三个前提缺一不可（少一个都会误伤）：
//  1. 该站点上这个模型**实测收费** —— 免费 / 无观测的站点不受限（见 PickOptions 注释）；
//  2. 余额**已实测**（CreditsKnown）—— 没查过时 quotaRemaining 也是 0，
//     不加这一条会在进程启动后、首次刷新余额之前把全池判成触底（一启动就 503）；
//  3. 余额确实低于阈值。
//
// 调用方需持有 p.mu（读 p.creditFloor）。
func (p *Pool) creditFloorBlocks(a *Account, opts PickOptions) bool {
	if p.creditFloor <= 0 || !opts.PaidSitesForModel[a.Site()] {
		return false
	}
	if !a.CreditsKnown() {
		return false
	}
	return a.Credits() < p.creditFloor
}

// pickRoundRobin 保留 wb-gateway 的轮询语义（作为可配策略与对照基线）。
func (p *Pool) pickRoundRobin(opts PickOptions) (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	// 粘性指定优先于免费站点倾斜：粘性是为了多轮上下文不跳号，
	// 而且这个账号当初就是按同样的优先规则选出来的，此处再判一次只会打断会话。
	if opts.Pin != nil {
		if p.eligible(opts.Pin, opts, now) {
			p.noteSelectedLocked(opts.Pin, now)
			return opts.Pin, nil
		}
	}

	n := len(p.accounts)
	// 免费站点优先在轮询下同样是软优先：先只在优先站点里转一圈，没有可选再放宽到全部。
	type siteFilter func(*Account) bool
	passes := []siteFilter{nil}
	if len(opts.PreferredSites) > 0 {
		pref := opts.PreferredSites
		passes = []siteFilter{func(a *Account) bool { return pref[a.Site()] }, nil}
	}
	for _, allow := range passes {
		for i := 0; i < n; i++ {
			idx := (p.rrIndex + i) % n
			a := p.accounts[idx]
			if opts.Exclude != nil && opts.Exclude[a] {
				continue
			}
			if allow != nil && !allow(a) {
				continue
			}
			if !p.eligible(a, opts, now) {
				continue
			}
			p.rrIndex = (idx + 1) % n
			p.noteSelectedLocked(a, now)
			return a, nil
		}
	}
	return p.pickEarliestExpiryLocked(opts, now)
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的冷却/熔断账号里选截止最早的一个。
//
// 排除规则：
//   - 永久禁用的账号永不参与
//   - 处于有效**硬冷却**（余额耗尽）的账号不参与：调了必然 402，白耗一轮并刷噪音日志
//   - 在途占满、被请求级排除的账号同样跳过
func (p *Pool) pickEarliestExpiryLocked(opts PickOptions, now time.Time) (*Account, error) {
	var (
		best     *Account
		bestExp  time.Time
		fallback []*Account
	)
	for _, a := range p.accounts {
		if opts.Exclude != nil && opts.Exclude[a] {
			continue
		}
		if opts.Site != "" && a.Site() != opts.Site {
			continue
		}
		// 全冷却兜底同样不能绕过账号名单：名单是配置层的硬约束，
		// 绕过它等于「账号一冷却，规则就自动失效」。
		if ok, _ := p.modelAccountAllowedLocked(opts.Model, a); !ok {
			continue
		}
		a.mu.Lock()
		disabled := a.disabled
		hardActive := a.coolKind == CoolHard && now.Before(a.cooldownUntil)
		exp := a.expiryLocked(now)
		a.mu.Unlock()
		if disabled || hardActive || exp.IsZero() {
			continue
		}
		if !a.AcquireInFlight(p.inFlightLimit(a)) {
			continue
		}
		fallback = append(fallback, a)
		if best == nil || exp.Before(bestExp) {
			best = a
			bestExp = exp
		}
	}
	// 落选者释放在途名额
	for _, a := range fallback {
		if a != best {
			a.ReleaseInFlight()
		}
	}
	if best == nil {
		return nil, ErrNoAccount
	}
	p.logf("[池] 全部账号处于冷却，兜底选中 %s（截止 %s）",
		best.Cred.AccountID(), bestExp.Format("15:04:05"))
	p.noteSelectedLocked(best, now)
	return best, nil
}

// noteSelectedLocked 记录选中时刻与单调序号（调用方需持有 p.mu）。
func (p *Pool) noteSelectedLocked(a *Account, now time.Time) {
	a.mu.Lock()
	a.lastUsedAt = now
	a.mu.Unlock()
	p.pickSeq++
	seq := p.pickSeq
	a.mu.Lock()
	a.usedSeq = seq
	a.mu.Unlock()
}

func (a *Account) usedSeqLocked() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usedSeq
}

// lastPickGapOK 判断账号是否已越过防撞号窗口。
func (p *Pool) lastPickGapOK(a *Account, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return now.Sub(a.lastUsedAt) >= minPickGap
}

// weightOf 计算单个账号的权重（三因子）。
func weightOf(a *Account, maxCredits float64, now time.Time, g GovParams) float64 {
	w := 1.0

	credits, expiring := a.CreditsDetailed()
	if maxCredits > 0 {
		w += credits / maxCredits * 10
	}
	// 快过期积分加成：官方活动赠送的积分按批过期，不用就作废
	if credits > 0 && expiring > 0 {
		w += expiring / credits * 8.0
	}

	// 闲置补偿
	lastUsed := a.LastUsedAt()
	if lastUsed.IsZero() {
		w += g.IdleWeightMax
	} else {
		idleW := now.Sub(lastUsed).Hours() * g.IdleWeightPerHour
		if idleW > g.IdleWeightMax {
			idleW = g.IdleWeightMax
		}
		if idleW < 0 {
			idleW = 0
		}
		w += idleW
	}
	return w
}

// weightedRandom 在候选内按权重随机抽签。
func weightedRandom(cands []*Account, weights map[*Account]float64) *Account {
	const scale = 1_000_000
	total := int64(0)
	ws := make([]int64, len(cands))
	for i, a := range cands {
		ws[i] = int64(weights[a] * scale)
		if ws[i] < 0 {
			ws[i] = 0
		}
		total += ws[i]
	}
	if total <= 0 {
		return cands[rand.IntN(len(cands))]
	}
	r := rand.Int64N(total)
	var acc int64
	for i, a := range cands {
		acc += ws[i]
		if r < acc {
			return a
		}
	}
	return cands[len(cands)-1]
}

// Credits / CreditsDetailed / LastUsedAt 是账号额度与使用时刻的读访问器。
func (a *Account) Credits() float64 {
	c, _ := a.CreditsDetailed()
	return c
}

// CreditsKnown 报告该账号的余额是否**被真实查询过**。
//
// 这个区分对积分保底是必需的：`quotaRemaining` 在「从未查过余额」与「余额真的是 0」
// 两种情况下都是 0。若不加区分，保底会在启动后、首次刷新余额之前把**所有**账号
// 判成触底，表现为「刚启动时全部 503」—— 而原因跟余额毫无关系。
func (a *Account) CreditsKnown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.quotaKnown
}

// CreditsDetailed 返回 (剩余积分, 其中即将过期的子集)。
//
// 第二个返回值来自上游 paid/free 明细端点的 `ExpiringRemaining`（7 天内到期的余额之和），
// 由 RefreshQuotaDetailed / 面板积分查询写入。
//
// 注意：**必须打 paid/free 两个明细端点才拿得到到期时间**，summary 端点只返回容量数字。
// （2026-09-29 实测推翻了此前「上游不返回到期字段」的错误结论：free 端点返回的
// Accounts[] 里带 CycleStartTime / CycleEndTime / DeductionEndTime。）
// 未取过明细时该值为 0，weightOf 里的快过期加成不生效 —— 这是「还没拉过」而不是
// 「策略失效」，下次刷新即可恢复。
func (a *Account) CreditsDetailed() (float64, float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.quotaRemaining, a.quotaExpiring
}

// LastUsedAt 返回最近一次被选中的时刻。
func (a *Account) LastUsedAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastUsedAt
}
