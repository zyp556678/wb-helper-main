// Package pool —— 治理与调度（切片 2）
//
// 本文件定义治理参数与状态迁移原语，口径来自 wb2api-panel：
//
//	冷却（cool until）       按错误类别给固定/对齐上游的恢复时刻，与熔断正交
//	熔断（breaker）          只由「反复失败」驱动，指数退避、有封顶
//	连败降权（degrade）      未知错误连败 N 次临时出池，成功即恢复
//	在途租约（in-flight）    单账号并发上限，占满即跳过（防并发双发触发风控）
//	软冷却指数退避           同账号连续软冷却按 2 倍放大，封顶 softRateMax
package pool

import (
	"sync/atomic"
	"time"

	"workbuddy-gateway/internal/config"
)

// CoolKind 是冷却类别：决定展示文案与兜底时的取舍。
type CoolKind string

const (
	CoolNone    CoolKind = ""
	CoolSoft    CoolKind = "soft"    // 限流：对齐上游重置墙钟或有界指数退避
	CoolHard    CoolKind = "hard"    // 余额耗尽：冷却到次日 04:00，等签到恢复
	CoolBreaker CoolKind = "breaker" // 熔断：反复失败后的指数退避
)

// GovParams 是治理参数（可由面板热改，故单独成结构体并原子替换）。
type GovParams struct {
	// 限流软冷却基数与封顶
	SoftRate    time.Duration
	SoftRateMax time.Duration
	// 连败降权
	DegradeThreshold   int
	DegradeCooldown    time.Duration
	DegradeCooldownMax time.Duration
	// 在途租约
	MaxInFlight       int
	MaxInFlightGlobal int
	// 熔断
	BreakerThreshold   int
	BreakerCooldown    time.Duration
	BreakerCooldownMax time.Duration
	// 选号权重
	IdleWeightPerHour float64
	IdleWeightMax     float64
}

// DefaultGov 返回与 wb2api-panel 一致的默认治理参数。
func DefaultGov() GovParams {
	return GovParams{
		SoftRate:           600 * time.Second,
		SoftRateMax:        2 * time.Hour,
		DegradeThreshold:   5,
		DegradeCooldown:    10 * time.Minute,
		DegradeCooldownMax: 2 * time.Hour,
		MaxInFlight:        3,
		MaxInFlightGlobal:  2,
		BreakerThreshold:   3,
		BreakerCooldown:    30 * time.Minute,
		BreakerCooldownMax: 6 * time.Hour,
		IdleWeightPerHour:  0.5,
		IdleWeightMax:      5.0,
	}
}

// gov 读当前治理参数。
func (p *Pool) gov() GovParams {
	if v := p.govParams.Load(); v != nil {
		return *v
	}
	return DefaultGov()
}

// SetGov 热替换治理参数（面板保存配置时调用）。
func (p *Pool) SetGov(g GovParams) { p.govParams.Store(&g) }

// Government 返回当前治理参数副本（供面板展示）。
func (p *Pool) Government() GovParams { return p.gov() }

// modelCooldown 是 (账号, 模型) 级冷却条目。
type modelCooldown struct {
	Until   time.Time
	ResetAt time.Time // 上游给出的重置墙钟（6004 才有）
	Reason  string
}

// -----------------------------------------------------------------------------
// 健康判定
// -----------------------------------------------------------------------------

// healthy 报告账号此刻是否可调度（不含模型维度）。
// 注意：模型级冷却**不**影响本判定 —— 那正是「切模型立即可用」的实现方式。
func (a *Account) healthy(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.healthyLocked(now)
}

func (a *Account) healthyLocked(now time.Time) bool {
	if a.disabled || a.Cred.AccessToken == "" {
		return false
	}
	if now.Before(a.cooldownUntil) || now.Before(a.breakerUntil) || now.Before(a.degradeUntil) {
		return false
	}
	return true
}

// HealthyForModel 是对外暴露的模型感知健康判定（面板统计「可调度该模型的账号数」用）。
func (a *Account) HealthyForModel(now time.Time, model string) bool {
	return a.healthyForModel(now, model)
}

// healthyForModel 在 healthy 之上叠加模型级冷却判定。
func (a *Account) healthyForModel(now time.Time, model string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.healthyLocked(now) {
		return false
	}
	if model == "" {
		return true
	}
	mc, ok := a.modelCooldowns[model]
	if !ok {
		return true
	}
	return !now.Before(mc.Until)
}

// expiry 返回账号当前生效的失效截止时刻（冷却与熔断取较晚者）；全无则零值。
func (a *Account) expiry(now time.Time) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.expiryLocked(now)
}

func (a *Account) expiryLocked(now time.Time) time.Time {
	var out time.Time
	for _, t := range []time.Time{a.cooldownUntil, a.breakerUntil, a.degradeUntil} {
		if t.After(now) && t.After(out) {
			out = t
		}
	}
	return out
}

// pruneExpiredModelCooldowns 惰性清理过期的模型级冷却条目，避免 map 无限膨胀。
func (a *Account) pruneExpiredModelCooldowns(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for m, mc := range a.modelCooldowns {
		if !now.Before(mc.Until) {
			delete(a.modelCooldowns, m)
		}
	}
	if len(a.modelCooldowns) == 0 {
		a.modelCooldowns = nil
	}
}

// ModelCooldowns 返回模型级冷却快照（供面板台账展示）。
func (a *Account) ModelCooldowns() map[string]time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]time.Time, len(a.modelCooldowns))
	for m, mc := range a.modelCooldowns {
		out[m] = mc.Until
	}
	return out
}

// -----------------------------------------------------------------------------
// 冷却入口
// -----------------------------------------------------------------------------

// CooldownSoft 账号级软冷却。语义与 wb2api 一致：
//   - 有上游重置墙钟（resetAt 非零）→ 截止 = min(resetAt, now+softRateMax)，绝不指数堆加
//   - 无重置时间且**不在**软冷却中 → 有界指数退避（base × 2^(streak-1)，封顶 softRateMax）
//   - 无重置时间但**已在**软冷却中 → 不推进 streak、不延长：重试不得把冷却越堆越厚
func (a *Account) CooldownSoft(base time.Duration, resetAt time.Time, reason string) {
	g := a.govParams()
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if !resetAt.IsZero() {
		a.cooldownUntil = capSoft(now, resetAt, g.SoftRateMax)
	} else if a.coolKind != CoolSoft || !now.Before(a.cooldownUntil) {
		d := softDuration(base, a.softStreak+1, g.SoftRateMax)
		a.softStreak++
		a.cooldownUntil = now.Add(d)
	}
	a.coolKind = CoolSoft
	a.cooldownReason = reason
	// 账号级冷却清空模型豁免表：否则上一次模型级限流的豁免会泄漏到本次账号级限流上
	a.modelCooldowns = nil
}

// CooldownHard 余额耗尽类硬冷却：冷却到次日 04:00，等签到任务恢复。
func (a *Account) CooldownHard(reason string) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cooldownUntil = nextDay4AM(now)
	a.coolKind = CoolHard
	a.cooldownReason = reason
	a.modelCooldowns = nil
}

// CooldownFixed 固定时长冷却（如上游 404 的短冷却），不再喂熔断器。
func (a *Account) CooldownFixed(d time.Duration, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cooldownUntil = time.Now().Add(d)
	a.coolKind = CoolSoft
	a.cooldownReason = reason
	a.modelCooldowns = nil
}

// CooldownModel 模型级软冷却（429 且带模型信息）：
//   - 有上游重置墙钟 → 只写该模型的条目（账号级 until 不动），因此切模型立即可用
//   - 无重置时间 → 退化为一轮有界账号级软冷却
func (a *Account) CooldownModel(model string, base time.Duration, resetAt time.Time, reason string) {
	if model == "" {
		a.CooldownSoft(base, resetAt, reason)
		return
	}
	g := a.govParams()
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if resetAt.IsZero() {
		// 无重置时间：该模型按有界退避冷却（不进账号级 until）
		if a.modelCooldowns == nil {
			a.modelCooldowns = map[string]modelCooldown{}
		}
		prev := a.modelCooldowns[model]
		d := softDuration(base, 1, g.SoftRateMax)
		if now.Before(prev.Until) {
			d = prev.Until.Sub(now) // 冷却中不延长
		}
		a.modelCooldowns[model] = modelCooldown{Until: now.Add(d), Reason: reason}
		return
	}
	if a.modelCooldowns == nil {
		a.modelCooldowns = map[string]modelCooldown{}
	}
	a.modelCooldowns[model] = modelCooldown{
		Until:   capSoft(now, resetAt, g.SoftRateMax),
		ResetAt: resetAt,
		Reason:  reason,
	}
}

// NoteBreaker 累计一次熔断失败；达阈值则按指数退避熔断。
func (a *Account) NoteBreaker() {
	g := a.govParams()
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fails++
	if a.fails < g.BreakerThreshold {
		return
	}
	d := g.BreakerCooldown
	for i := 0; i < a.retryCount; i++ {
		d *= 2
		if d >= g.BreakerCooldownMax {
			d = g.BreakerCooldownMax
			break
		}
	}
	a.fails = 0
	a.retryCount++
	a.breakerUntil = now.Add(d)
	a.coolKind = CoolBreaker
	a.cooldownReason = "连续失败达阈值，进入熔断退避"
}

// NoteDegrade 记录一次「未知错误」连败；达阈值临时出池（成功即清零）。
func (a *Account) NoteDegrade() {
	g := a.govParams()
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.degradeFails++
	if a.degradeFails < g.DegradeThreshold {
		return
	}
	d := g.DegradeCooldown
	for i := 0; i < a.degradeTimes; i++ {
		d *= 2
		if d >= g.DegradeCooldownMax {
			d = g.DegradeCooldownMax
			break
		}
	}
	a.degradeTimes++
	a.degradeUntil = now.Add(d)
	a.degradeFails = 0
}

// ClearFailureState 成功后清零瞬时健康信号：
// 熔断计数与退避、连败降权、软冷却连续计数、冷却 / 熔断截止。
// 注意不清 expiry 之外的东西：余额不足的硬冷却只有签到或额度恢复能解。
func (a *Account) ClearFailureState() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fails = 0
	a.retryCount = 0
	a.degradeFails = 0
	a.degradeTimes = 0
	a.degradeUntil = time.Time{}
	a.softStreak = 0
	if a.coolKind != CoolHard {
		a.cooldownUntil = time.Time{}
		a.cooldownReason = ""
		a.coolKind = CoolNone
	}
	a.breakerUntil = time.Time{}
}

// ReviveIfCreditsRecovered 额度恢复时解冻：只清余额类硬冷却，不碰熔断。
func (a *Account) ReviveIfCreditsRecovered(remaining float64) bool {
	if remaining <= 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.coolKind == CoolHard && time.Now().Before(a.cooldownUntil) {
		a.cooldownUntil = time.Time{}
		a.cooldownReason = ""
		a.coolKind = CoolNone
		return true
	}
	return false
}

// -----------------------------------------------------------------------------
// 在途租约
// -----------------------------------------------------------------------------

// AcquireInFlight 尝试占用一个在途名额；上限 0 表示不限。
func (a *Account) AcquireInFlight(limit int) bool {
	if limit <= 0 {
		atomic.AddInt64(&a.inFlight, 1)
		return true
	}
	for {
		cur := atomic.LoadInt64(&a.inFlight)
		if cur >= int64(limit) {
			return false
		}
		if atomic.CompareAndSwapInt64(&a.inFlight, cur, cur+1) {
			return true
		}
	}
}

// ReleaseInFlight 释放在途名额。
func (a *Account) ReleaseInFlight() {
	for {
		cur := atomic.LoadInt64(&a.inFlight)
		if cur <= 0 {
			return
		}
		if atomic.CompareAndSwapInt64(&a.inFlight, cur, cur-1) {
			return
		}
	}
}

// InFlight 返回当前在途数。
func (a *Account) InFlight() int64 { return atomic.LoadInt64(&a.inFlight) }

// inFlightLimit 返回该账号生效的在途上限（国际站风控更紧，上限更低）。
func (p *Pool) inFlightLimit(a *Account) int {
	g := p.gov()
	if a.Site() == "intl" && g.MaxInFlightGlobal > 0 {
		return g.MaxInFlightGlobal
	}
	return g.MaxInFlight
}

// InFlightLimit 返回该账号生效的在途上限（面板展示用）。
func (p *Pool) InFlightLimit(a *Account) int { return p.inFlightLimit(a) }

// -----------------------------------------------------------------------------
// 工具
// -----------------------------------------------------------------------------

// softDuration 按连续软冷却次数把基数指数放大，封顶 max。
func softDuration(base time.Duration, streak int, max time.Duration) time.Duration {
	if streak <= 1 {
		return base
	}
	shift := streak - 1
	if shift > 10 {
		shift = 10 // 左移上限，防溢出
	}
	d := base << uint(shift)
	if d > max || d <= 0 {
		d = max
	}
	return d
}

// capSoft 把上游重置墙钟截断到 now+max；已过期的重置时间视作立即恢复。
func capSoft(now, resetAt time.Time, max time.Duration) time.Time {
	if max <= 0 {
		max = 2 * time.Hour
	}
	cap := now.Add(max)
	if resetAt.After(cap) {
		return cap
	}
	if resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Millisecond)
}

// nextDay4AM 返回 now 之后最近的一个 04:00（本地时区）。
// now 在当天 04:00 之前时返回当天 04:00（该窗内等到当天签到即可恢复）。
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// -----------------------------------------------------------------------------
// session 失效与降级重试
// -----------------------------------------------------------------------------

// SessionDeadThreshold 是「连续 session 失效多少次才禁用账号」的阈值。
//
// 为什么不一次就禁用：单次 12153 / Offline user session 多为临时抖动
// （网络闪断、刷新竞态），一次就禁用会让可用账号池不断缩水。
const SessionDeadThreshold = 3

// NoteSessionDead 累计一次 session 失效；连续达阈值返回 true 表示已禁用账号。
func (a *Account) NoteSessionDead() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionDeadFails++
	if a.sessionDeadFails < SessionDeadThreshold {
		return false
	}
	a.disabled = true
	a.disabledReason = "连续多次 session 失效，已自动禁用（重新登录可恢复）"
	return true
}

// ClearSessionDead 在任意成功后清零 session 失效计数。
func (a *Account) ClearSessionDead() {
	a.mu.Lock()
	a.sessionDeadFails = 0
	a.mu.Unlock()
}

// GovFromConfig 把配置里的治理参数转成运行时参数（启动与热改共用同一转换）。
func GovFromConfig(c *config.Config) GovParams {
	return GovParams{
		SoftRate:           c.SoftRate(),
		SoftRateMax:        c.SoftRateMax(),
		DegradeThreshold:   c.Cooldown.DegradeThreshold,
		DegradeCooldown:    c.DegradeCooldown(),
		DegradeCooldownMax: c.DegradeCooldownMax(),
		MaxInFlight:        c.Pool.MaxInFlight,
		MaxInFlightGlobal:  c.Pool.MaxInFlightGlobal,
		BreakerThreshold:   c.Pool.BreakerThreshold,
		BreakerCooldown:    c.BreakerCooldown(),
		BreakerCooldownMax: c.BreakerCooldownMax(),
		IdleWeightPerHour:  c.Pool.IdleWeightPerHour,
		IdleWeightMax:      c.Pool.IdleWeightMax,
	}
}
