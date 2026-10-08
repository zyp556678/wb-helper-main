// Package pool 是账号池：负责凭据加载与热加载、选号、Token 有效性，以及冷却 / 熔断 /
// 连败降权 / 在途租约等治理状态（治理实现见 governance.go，选号见 pick.go）。
//
// 选号默认走三因子加权 + Top5 短名单 + 加权随机 + 防惊群，全冷却时按最早到期兜底；
// Strategy="roundrobin" 可切回轮询语义（对照与回退用）。
package pool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/upstream"
)

// TokenRefreshLead 距过期不足该时长即视为需要刷新（与 wb-gateway 的 15 分钟一致）。
const TokenRefreshLead = 15 * time.Minute

// ErrNoAccount 表示池内没有任何可用账号。
var ErrNoAccount = errors.New("没有可用账号")

// Account 是池内账号：凭据 + 运行期状态。
type Account struct {
	Cred *auth.Credential

	mu             sync.Mutex // 串行化「同账号的上游请求」与「令牌刷新」，防并发双发触发风控
	disabled       bool
	disabledReason string
	// paused 是「暂停选号」：只把它从派发里摘出来，维护任务照常跑。
	// 语义与落盘见 ops.go 的 pausedMarkerSuffix。
	paused       bool
	lastError    string
	successCount int64
	failureCount int64

	// ---- 治理状态（切片 2）----
	govPtr           *atomic.Pointer[GovParams] // 共享治理参数（面板热改后立即生效）
	cooldownUntil    time.Time                  // 冷却截止（soft/hard 共用）
	cooldownReason   string
	coolKind         CoolKind
	breakerUntil     time.Time // 熔断截止（与冷却正交）
	fails            int       // 熔断连续失败计数
	retryCount       int       // 熔断退避指数
	degradeUntil     time.Time // 连败降权截止
	degradeFails     int
	degradeTimes     int
	softStreak       int                      // 软冷却连续次数（指数退避用）
	sessionDeadFails int                      // session 连续失效计数（达阈值才禁用）
	modelCooldowns   map[string]modelCooldown // (账号,模型) 级冷却，支持 6004 切模型豁免
	inFlight         int64                    // 在途请求数（原子读写）
	lastUsedAt       time.Time
	usedSeq          uint64 // 单调选中序号：比墙钟可靠（Windows 时间精度低）

	// fingerprint 是凭据文件的 (mtime, size) 指纹。
	// 热加载据此判断文件是否真的变了：没变就复用原对象（不重建），
	// 这样额度、计数、冷却等运行状态不会被无谓地清空。我们自己写回凭据后要同步更新它。
	fingerprint string

	// 额度快照（切片 1：账号与登录）
	quotaKnown     bool
	quotaTotal     float64
	quotaUsed      float64
	quotaRemaining float64
	quotaPlan      string
	quotaPaid      bool
	quotaUpdatedAt int64
	// quotaExpiring 是 7 天内到期的余额子集，供选号层「快过期优先」使用。
	// 只有 paid/free 明细端点才给得出（summary 没有到期字段），
	// 所以它由 RefreshQuotaDetailed 写入，未取过明细时为 0。
	quotaExpiring float64
	// quotaSoonestExpire 是仍有余额的包里最早的到期时间（Unix 毫秒，0 表示无）。
	quotaSoonestExpire int64

	// tokenRefreshedAt 是本进程内最近一次成功刷新令牌的时间（零值 = 本进程未刷过）。
	// 排程保活的惰性窗口据此判断「刚刷过就别再刷」，见 scheduler.KeepaliveWindow。
	tokenRefreshedAt time.Time
}

// Site 返回归一化站点。
func (a *Account) Site() string { return a.Cred.Site() }

// SiteLabel 返回站点中文名。
func (a *Account) SiteLabel() string { return auth.SiteLabel(a.Site()) }

// Profile 返回该账号对应的上游站点参数。
func (a *Account) Profile() *upstream.Profile { return upstream.ProfileForSite(a.Site()) }

// Lock 串行化同账号的上游请求。注意：持锁期间不要调用需要同一把锁的方法。
func (a *Account) Lock()   { a.mu.Lock() }
func (a *Account) Unlock() { a.mu.Unlock() }

// View 返回反代层需要的凭据视图（加锁读取，避免刷新期间读到半更新状态）。
func (a *Account) View() *upstream.CredentialView {
	a.mu.Lock()
	defer a.mu.Unlock()
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

// QuotaView 是额度的对外视图。
type QuotaView struct {
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Plan      string  `json:"plan"`
	Paid      bool    `json:"paid"`
	Exhausted bool    `json:"exhausted"`
	UpdatedAt int64   `json:"updated_at"`
}

type AccountState struct {
	// ID 是账号稳定标识（凭据文件名），单账号操作接口用它做路径参数。
	ID           string `json:"id"`
	File         string `json:"file"`
	Site         string `json:"site"`
	SiteLabel    string `json:"site_label"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterprise_id"`
	// IsEnterprise 是**计算值**（EnterpriseID 非空即真），不落盘。
	// 面板据此隐藏「签到 / 任务」这类企业版做不了的入口，并显示「企业版」标签 ——
	// 让前端自己去判 enterprise_id 是否为空，等于把这条规则抄成两份。
	IsEnterprise   bool   `json:"is_enterprise"`
	Disabled       bool   `json:"disabled"`
	DisabledReason string `json:"disabled_reason"`
	// Paused 是「暂停选号」：只不派发，签到 / 保活 / 旅行 / 成长任务照常跑。
	// 与 Disabled 是两种状态，面板要分开显示 —— 混在一起用户会以为号被停用了。
	Paused         bool   `json:"paused"`
	CooldownUntil  int64  `json:"cooldown_until"`
	CooldownReason string `json:"cooldown_reason"`
	TokenExpiresAt int64  `json:"token_expires_at"`
	TokenValid     bool   `json:"token_valid"`
	SuccessCount   int64  `json:"success_count"`
	FailureCount   int64  `json:"failure_count"`
	LastError      string `json:"last_error"`
	// Quota 为 nil 表示尚未查询过额度。
	Quota *QuotaView `json:"quota"`

	// ---- 治理状态（切片 2）----
	CooldownKind string `json:"cooldown_kind"` // "" | soft | hard | breaker
	BreakerUntil int64  `json:"breaker_until"` // Unix 秒
	Fails        int    `json:"fails"`         // 熔断连续失败计数
	InFlight     int64  `json:"in_flight"`     // 当前在途请求数
	MaxInFlight  int    `json:"max_in_flight"` // 生效的在途上限（0=不限）
	LastUsedAt   int64  `json:"last_used_at"`  // Unix 秒，0=从未使用
	BaseURL      string `json:"base_url"`      // 该账号实际使用的上游域名（双站排查用）
}

// Pool 是账号池。
type Pool struct {
	cfg    *config.Config
	client *upstream.Client
	// Logf 是日志输出函数，由 main 注入（默认空 = 不输出）。
	Logf func(format string, args ...any)

	mu       sync.RWMutex
	accounts []*Account
	rrIndex  int
	lastScan time.Time
	pickSeq  uint64

	govParams atomic.Pointer[GovParams]
	// Strategy 是选号策略："" 或 "weighted" = 三因子加权（默认）。
	// "roundrobin" 保留 wb-gateway 的轮询语义，便于对照与回退。
	Strategy string

	// creditFloor 是积分保底（0 = 关闭）：余额已实测低于该值时，
	// 不再让该账号承接**实测收费**的模型。见 eligible 处的完整理由。
	creditFloor float64
}

// SetCreditFloor 设置积分保底阈值（<=0 表示关闭）。
//
// 负值一律归一成 0 而不是报错：它来自用户手写的 config.json，
// 写错一个负号不该让启动失败。
func (p *Pool) SetCreditFloor(v int64) {
	if v <= 0 {
		v = 0
	}
	p.mu.Lock()
	p.creditFloor = float64(v)
	p.mu.Unlock()
}

// CreditFloor 返回当前积分保底阈值（0 = 关闭）。
func (p *Pool) CreditFloor() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.creditFloor
}

// AccountsBlockedByCreditFloor 统计「仅因积分保底而不可用」的账号数。
//
// 用途只有一个：选号失败时把原因说清楚。少这一条，「全池被保底拦住」会退化
// 成一句笼统的「全部账号均不可用」，而用户看到余额还有几百却调不动，
// 只会去怀疑网络或凭据 —— 这两者的处置方向完全相反。
//
// 判据必须与 eligible 里那条**逐字对应**（含 CreditsKnown 这一条），
// 否则会出现「报错说被保底拦住、实际是别的账号问题」这种更难查的误导。
func (p *Pool) AccountsBlockedByCreditFloor(paidSites map[string]bool, model string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.creditFloor <= 0 || len(paidSites) == 0 {
		return 0
	}
	n := 0
	for _, a := range p.accounts {
		if a.IsDisabled() || !paidSites[a.Site()] || !a.CreditsKnown() {
			continue
		}
		if a.Credits() >= p.creditFloor {
			continue
		}
		// 已被名单排除的账号不该算进保底的账上：两个原因同时存在时，
		// 报「名单排除」更准确（那是配置问题，改配置即可）。
		if ok, _ := p.modelAccountAllowedLocked(model, a); !ok {
			continue
		}
		n++
	}
	return n
}

// New 构造账号池。
func New(cfg *config.Config, client *upstream.Client) *Pool {
	p := &Pool{cfg: cfg, client: client, Strategy: "weighted"}
	d := DefaultGov()
	p.govParams.Store(&d)
	return p
}

// newAccount 构造账号并接上共享治理参数。
func (p *Pool) newAccount(cred *auth.Credential) *Account {
	return &Account{Cred: cred, govPtr: &p.govParams}
}

// govParamsOf 读账号所属池的治理参数（账号不持池指针，避免环状引用）。
func (a *Account) govParams() GovParams {
	if a.govPtr != nil {
		if v := a.govPtr.Load(); v != nil {
			return *v
		}
	}
	return DefaultGov()
}

// Accounts 返回账号切片快照（元素指针共享）。
func (p *Pool) Accounts() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, len(p.accounts))
	copy(out, p.accounts)
	return out
}

// Len 返回账号总数。
func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// Load 从配置的来源加载全部凭据，构建账号池。返回成功加载的数量。
// 单个文件解析失败只跳过该文件，不影响其余账号（与 wb-gateway 一致）。
func (p *Pool) Load() (int, []error) {
	paths := auth.Discover(p.cfg.WorkDir, p.cfg.AuthFile, p.cfg.AuthDir, p.cfg.AuthExplicit)

	var (
		accs []*Account
		errs []error
	)
	markers := p.loadMarkers(paths)
	pausedMarkers := p.loadPausedMarkers(paths)
	for _, path := range paths {
		cred, err := auth.LoadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		acc := p.newAccount(cred)
		acc.fingerprint = fileFingerprint(path)
		if reason, ok := markers[path]; ok {
			acc.disabled = true
			acc.disabledReason = reason
		}
		if pausedMarkers[path] {
			acc.paused = true
		}
		accs = append(accs, acc)
	}

	p.mu.Lock()
	p.accounts = accs
	p.rrIndex = 0
	p.lastScan = time.Now()
	p.mu.Unlock()
	return len(accs), errs
}

// Reload 重新扫描凭据来源：新增 / 更新 / 删除凭据免重启生效。
//
// 关键点：**按文件指纹判断是否真的变了**。
// 指纹未变的账号直接复用原对象，运行状态（额度、成功失败计数、冷却、粘性）原样保留；
// 指纹变了（外部改过文件，例如重新登录或别处刷新了令牌）才重建对象，
// 并把可保留的运行状态迁移过去。
func (p *Pool) Reload() bool {
	paths := auth.Discover(p.cfg.WorkDir, p.cfg.AuthFile, p.cfg.AuthDir, p.cfg.AuthExplicit)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastScan = time.Now()

	existing := make(map[string]*Account, len(p.accounts))
	for _, a := range p.accounts {
		existing[a.Cred.Path] = a
	}

	markers := p.loadMarkers(paths)
	pausedMarkers := p.loadPausedMarkers(paths)
	next := make([]*Account, 0, len(paths))
	for _, path := range paths {
		fp := fileFingerprint(path)

		// 指纹未变：文件没动过，整对象复用，运行状态零损失
		if old := existing[path]; old != nil && old.fingerprint == fp && fp != "" {
			if _, marked := markers[path]; marked {
				old.setDisabled(true, markers[path])
			}
			// 暂停状态以标记文件为准（用户可能在面板上改过）
			old.mu.Lock()
			old.paused = pausedMarkers[path]
			old.mu.Unlock()
			next = append(next, old)
			continue
		}

		cred, err := auth.LoadFile(path)
		if err != nil {
			// 文件暂时不可读（正在被替换）时保留旧对象，避免账号凭空消失
			if old := existing[path]; old != nil {
				next = append(next, old)
			}
			continue
		}

		acc := p.newAccount(cred)
		acc.fingerprint = fp
		if old := existing[path]; old != nil {
			old.migrateRuntimeTo(acc)
		}
		if pausedMarkers[path] {
			acc.paused = true
		}
		if reason, ok := markers[path]; ok {
			acc.disabled = true
			acc.disabledReason = reason
		}
		next = append(next, acc)
	}

	changed := len(next) != len(p.accounts)
	if !changed {
		for i := range next {
			if next[i] != p.accounts[i] {
				changed = true
				break
			}
		}
	}

	p.accounts = next
	if len(next) == 0 {
		p.rrIndex = 0
	} else if p.rrIndex >= len(next) {
		p.rrIndex %= len(next)
	}
	return changed
}

// setDisabled 设置禁用状态（需在持 a.mu 前调用方自行决定是否加锁；内部会加锁）。
func (a *Account) setDisabled(disabled bool, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.disabled = disabled
	a.disabledReason = reason
}

// migrateRuntimeTo 把可保留的运行状态迁移到新对象（文件已变、对象需重建时）。
func (a *Account) migrateRuntimeTo(dst *Account) {
	a.mu.Lock()
	defer a.mu.Unlock()
	dst.disabled = a.disabled
	dst.disabledReason = a.disabledReason
	dst.cooldownUntil = a.cooldownUntil
	dst.cooldownReason = a.cooldownReason
	dst.lastError = a.lastError
	dst.successCount = a.successCount
	dst.failureCount = a.failureCount
	dst.lastUsedAt = a.lastUsedAt
	dst.quotaKnown = a.quotaKnown
	dst.quotaTotal = a.quotaTotal
	dst.quotaUsed = a.quotaUsed
	dst.quotaRemaining = a.quotaRemaining
	dst.quotaPlan = a.quotaPlan
	dst.quotaPaid = a.quotaPaid
	dst.quotaUpdatedAt = a.quotaUpdatedAt
	dst.quotaExpiring = a.quotaExpiring
	dst.quotaSoonestExpire = a.quotaSoonestExpire
	// 保活惰性窗口的计时也要随对象迁移，否则外部改一次凭据就会让下一个
	// 保活时点立刻再刷一遍（同一个 token 刚刷过）。
	dst.tokenRefreshedAt = a.tokenRefreshedAt
	// 治理状态同样必须迁移，否则外部改一次凭据就把冷却/熔断/计数清空
	dst.coolKind = a.coolKind
	dst.breakerUntil = a.breakerUntil
	dst.fails = a.fails
	dst.retryCount = a.retryCount
	dst.degradeUntil = a.degradeUntil
	dst.degradeFails = a.degradeFails
	dst.degradeTimes = a.degradeTimes
	dst.softStreak = a.softStreak
	dst.modelCooldowns = a.modelCooldowns
	dst.usedSeq = a.usedSeq
}

// MarkSuccess 记录一次成功：清零瞬时健康信号（熔断计数/退避、连败降权、
// 软冷却连续计数、冷却与熔断截止）。余额类硬冷却不在此清理，等签到或额度恢复解冻。
func (a *Account) MarkSuccess() {
	a.successCountAdd()
	a.ClearFailureState()
}

func (a *Account) successCountAdd() {
	a.mu.Lock()
	a.successCount++
	a.mu.Unlock()
}

// MarkFailure 记录一次失败（只计数与记原因，处置由调用方按错误分类决定）。
func (a *Account) MarkFailure(reason string) {
	a.mu.Lock()
	a.failureCount++
	a.lastError = reason
	a.mu.Unlock()
}

// Disable 永久禁用账号（授权失效）。
func (a *Account) Disable(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.disabled = true
	a.disabledReason = reason
}

// IsDisabled 报告账号是否已禁用。
func (a *Account) IsDisabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.disabled
}

// IsPaused 报告账号是否被暂停选号（只不派发，维护任务照常）。
func (a *Account) IsPaused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.paused
}

// TokenRefreshedAt 返回本进程内最近一次成功刷新令牌的时间；零值表示尚未刷过。
//
// 供排程保活判断惰性窗口（刷新后 N 小时内不重复刷新），不用于对外展示。
func (a *Account) TokenRefreshedAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokenRefreshedAt
}

// TokenExpiresAt 返回令牌到期时间（Unix 秒，0 = 未知），加锁读取。
func (a *Account) TokenExpiresAt() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Cred.ExpiresAt
}

// EnsureToken 保证账号令牌在有效期内：距过期不足 TokenRefreshLead 时刷新。
// 刷新因授权失效失败时会禁用该账号。
func (p *Pool) EnsureToken(ctx context.Context, a *Account) error {
	return p.refreshToken(ctx, a, false, false)
}

// ForceRefreshToken 强制刷新令牌（保活任务用），不判断是否临近过期。
func (p *Pool) ForceRefreshToken(ctx context.Context, a *Account) error {
	return p.refreshToken(ctx, a, true, false)
}

// ForceRefreshTokenIncludingDisabled 同 ForceRefreshToken，但**允许已禁用账号**。
//
// 供 schedule.include_disabled_in_tasks 打开后的保活任务使用：「禁用」只关选号、
// 不停保号 —— 闲置待命的号同样要续 token，否则等它被启用时 refresh 会话早已被
// 官方服务端清理，只能重新登录。刷新成功照常清零 session 失效计数。
func (p *Pool) ForceRefreshTokenIncludingDisabled(ctx context.Context, a *Account) error {
	return p.refreshToken(ctx, a, true, true)
}

// refreshToken 是令牌刷新的唯一实现；force 为真时无条件刷新，
// allowDisabled 为真时允许刷新已禁用账号（见 ForceRefreshTokenIncludingDisabled）。
func (p *Pool) refreshToken(ctx context.Context, a *Account, force, allowDisabled bool) error {
	a.mu.Lock()
	cred := a.Cred
	expiresAt := cred.ExpiresAt
	refreshToken := cred.RefreshToken
	disabled := a.disabled
	a.mu.Unlock()

	if disabled && !allowDisabled {
		return errors.New("账号已失效")
	}
	now := time.Now()
	if !force && expiresAt > 0 && now.Add(TokenRefreshLead).Unix() < expiresAt {
		return nil // 仍有效
	}
	if refreshToken == "" {
		if expiresAt > 0 && now.Unix() >= expiresAt {
			return errors.New("令牌已过期且缺少 refreshToken，请重新登录")
		}
		return nil
	}

	prof := a.Profile()
	// 串行化：与同账号的上游请求互斥，避免刷新期间请求读到半更新状态
	a.mu.Lock()
	defer a.mu.Unlock()

	view := &upstream.CredentialView{
		AccessToken:  cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		UID:          cred.UID,
		EnterpriseID: cred.EnterpriseID,
		Domain:       cred.Domain,
		DeviceToken:  cred.DeviceToken,
	}
	tok, status, err := p.client.RefreshToken(ctx, view, prof)
	if err != nil {
		if status == 401 || status == 403 {
			a.disabled = true
			a.disabledReason = fmt.Sprintf("令牌刷新被拒 (HTTP %d)，请重新登录", status)
			return errors.New(a.disabledReason)
		}
		return fmt.Errorf("刷新令牌失败: %w", err)
	}

	// ---- 覆盖写回之前先校验（切片 18）----
	//
	// 这一步会多花一次只读请求（最长 CredentialVerifyTimeout）。它是在**账号锁内**
	// 做的，所以这段时间该账号的请求会被挡住 —— 这是刻意的取舍：刷新本来就很稀有
	//（只有临近过期或手动触发才发生），而「把别的账号的令牌写进来」这种事故
	// 一旦发生就无法自行恢复。宁可偶尔多等几秒。
	newDomain := cred.Domain
	if tok.Domain != "" {
		newDomain = tok.Domain
	}
	newRefresh := cred.RefreshToken
	if tok.RefreshToken != "" {
		newRefresh = tok.RefreshToken
	}
	newView := &upstream.CredentialView{
		AccessToken:  tok.AccessToken,
		RefreshToken: newRefresh,
		UID:          cred.UID,
		EnterpriseID: cred.EnterpriseID,
		Domain:       newDomain,
		DeviceToken:  cred.DeviceToken,
	}
	if err := p.validateRefreshedCredential(cred.AccessToken, newView, prof); err != nil {
		// 内存与磁盘都不动：保留旧凭据继续用（它至少还能用到过期）。
		return err
	}

	cred.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		cred.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		cred.Domain = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		cred.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	// 惰性窗口的判定依据：刷新已实质性完成（内存中的令牌已换新），
	// 即使下面写盘失败也应记下时间 —— 否则下次排程保活会立刻再刷一遍。
	a.tokenRefreshedAt = time.Now()
	if err := cred.SaveAtomic(); err != nil {
		// 内存已更新，磁盘写回失败不应让本次请求失败，但要让运维知道
		return fmt.Errorf("令牌已刷新但写回凭据失败: %w", err)
	}
	// 我们自己改写了文件，同步指纹，避免下一轮热加载误判成「外部变更」而重建对象
	a.fingerprint = fileFingerprint(cred.Path)
	return nil
}

// EnsureAllTokens 启动时批量保证令牌有效。
func (p *Pool) EnsureAllTokens(ctx context.Context) {
	for _, a := range p.Accounts() {
		if a.IsDisabled() {
			continue
		}
		_ = p.EnsureToken(ctx, a)
	}
}

// Snapshot 返回全部账号状态快照。
func (p *Pool) Snapshot() []AccountState {
	accs := p.Accounts()
	out := make([]AccountState, 0, len(accs))
	for _, a := range accs {
		out = append(out, p.StateOf(a))
	}
	return out
}

// StateOf 返回单个账号的状态快照。
func (p *Pool) StateOf(a *Account) AccountState {
	now := time.Now()
	a.mu.Lock()
	st := AccountState{
		ID: a.Cred.AccountID(),
		// File 现在给的是绝对路径（Discover 会拼上工作目录），但面板上只该显示文件名，
		// 否则界面上会铺一路 C:\Users\<用户名>\... 既难看也没必要。
		File:           filepath.Base(a.Cred.Path),
		Site:           a.Site(),
		SiteLabel:      a.SiteLabel(),
		UID:            a.Cred.UID,
		Nickname:       a.Cred.Nickname,
		EnterpriseID:   a.Cred.EnterpriseID,
		IsEnterprise:   a.Cred.IsEnterprise(),
		Disabled:       a.disabled,
		DisabledReason: a.disabledReason,
		Paused:         a.paused,
		CooldownReason: a.cooldownReason,
		TokenExpiresAt: a.Cred.ExpiresAt,
		TokenValid:     a.Cred.ExpiresAt == 0 || a.Cred.ExpiresAt > now.Unix(),
		SuccessCount:   a.successCount,
		FailureCount:   a.failureCount,
		LastError:      a.lastError,
	}
	if !a.cooldownUntil.IsZero() && now.Before(a.cooldownUntil) {
		st.CooldownUntil = a.cooldownUntil.Unix()
	}
	if now.Before(a.breakerUntil) {
		st.BreakerUntil = a.breakerUntil.Unix()
	}
	st.CooldownKind = string(a.coolKind)
	st.Fails = a.fails
	st.InFlight = a.inFlight
	st.LastUsedAt = a.lastUsedAt.Unix()
	if a.lastUsedAt.IsZero() {
		st.LastUsedAt = 0
	}
	if a.quotaKnown {
		st.Quota = &QuotaView{
			Total:     a.quotaTotal,
			Used:      a.quotaUsed,
			Remaining: a.quotaRemaining,
			Plan:      a.quotaPlan,
			Paid:      a.quotaPaid,
			Exhausted: a.quotaRemaining <= 0,
			UpdatedAt: a.quotaUpdatedAt,
		}
	}
	a.mu.Unlock()
	return st
}

// Summary 返回池级汇总计数。
type Summary struct {
	Total    int `json:"total"`
	Active   int `json:"active"`
	Disabled int `json:"disabled"`
	// Paused 是「暂停选号」的账号数：它只不派发，维护任务照常跑。
	// **必须与 Disabled 分列** —— 上游 panel@1b90f7f（issue #125）修的就是这个：
	// 两者混成一列时，"我把号暂停了"和"我把号停用了"在界面上看不出区别。
	Paused   int `json:"paused"`
	Cooldown int `json:"cooldown"`
	// CreditsRemaining 是**已查到额度**的账号的剩余积分合计。
	CreditsRemaining float64 `json:"credits_remaining"`
	// QuotaKnown 是已查到额度的账号数（用于说明上者只覆盖了部分账号）。
	QuotaKnown int `json:"quota_known"`
	// InFlight 是全部账号当前在途请求数合计。
	InFlight int64 `json:"in_flight"`
}

// Summary 统计各状态账号数（互斥，和等于 Total）。
func (p *Pool) Summary() Summary {
	now := time.Now()
	var s Summary
	for _, a := range p.Snapshot() {
		s.Total++
		// 顺序即优先级：禁用 > 暂停 > 冷却 > 可用。四类互斥，和恒等于 Total。
		switch {
		case a.Disabled:
			s.Disabled++
		case a.Paused:
			s.Paused++
		case a.CooldownUntil > now.Unix():
			s.Cooldown++
		default:
			s.Active++
		}
		if a.Quota != nil {
			s.QuotaKnown++
			s.CreditsRemaining += a.Quota.Remaining
		}
		s.InFlight += a.InFlight
	}
	return s
}

// LastScan 返回最近一次凭据扫描时刻。
func (p *Pool) LastScan() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastScan
}
