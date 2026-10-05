package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy-gateway/internal/auth"
)

// markerSuffix 是「手工禁用」标记文件后缀。
//
// 为什么要落盘：轮询内存状态只在本进程内有效，重启后禁用会丢失，
// 被禁用的账号（例如已撤销授权的账号）会重新进入调度。标记文件让状态跨重启保持，
// 同时因为凭据文件仍然保留，用户重新登录即可自动恢复。
const markerSuffix = ".disabled"

// ErrAccountNotFound 表示按 ID 找不到账号。
var ErrAccountNotFound = errors.New("账号不存在")

// ValidAccountID 校验账号 ID（凭据文件名）：只允许 [A-Za-z0-9_.-]，禁止任何路径成分。
// 这是防路径穿越的第一道闸；第二道是 Find 只按文件名匹配池内已知账号。
func ValidAccountID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// Find 按账号 ID（凭据文件名）查找账号。
func (p *Pool) Find(id string) (*Account, error) {
	if !ValidAccountID(id) {
		return nil, fmt.Errorf("%w: 非法的账号标识 %q", ErrAccountNotFound, id)
	}
	for _, a := range p.Accounts() {
		if a.Cred.AccountID() == id {
			return a, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
}

// RefreshQuota 查询并记录某账号的额度。
func (p *Pool) RefreshQuota(ctx context.Context, acc *Account) error {
	return p.refreshQuota(ctx, acc, false)
}

// RefreshQuotaIncludingDisabled 同 RefreshQuota，但**允许已禁用账号**。
//
// 供 schedule.include_disabled_in_tasks 打开后的余额刷新使用：「禁用」的语义是
// 「不参与选号」，而轮换养号用法（一次只放开一个号）恰恰要靠禁用号的余额判断
// 「下一个该启用谁」——积分不刷新的话，这个判断只能靠人工去面板点。
//
// 注意这只让积分保持新鲜：解冻逻辑对 disabled 是 no-op，本方法不会复活禁用号。
func (p *Pool) RefreshQuotaIncludingDisabled(ctx context.Context, acc *Account) error {
	return p.refreshQuota(ctx, acc, true)
}

// refreshQuota 是额度查询的唯一实现；allowDisabled 控制是否跳过已禁用账号。
func (p *Pool) refreshQuota(ctx context.Context, acc *Account, allowDisabled bool) error {
	if acc == nil {
		return errors.New("账号为空")
	}
	if acc.IsDisabled() && !allowDisabled {
		return errors.New("账号已禁用，跳过额度查询")
	}

	// 与上游请求串行，避免与聊天请求并发读取半更新状态
	view := acc.View()
	prof := acc.Profile()

	q, err := p.client.FetchQuota(ctx, view, prof)
	if err != nil {
		acc.MarkFailure("额度查询失败")
		return err
	}

	now := time.Now().Unix()
	acc.mu.Lock()
	prevRemaining := acc.quotaRemaining
	prevKnown := acc.quotaKnown
	acc.quotaKnown = true
	acc.quotaTotal = q.Total
	acc.quotaUsed = q.Used
	acc.quotaRemaining = q.Remaining
	acc.quotaPlan = q.Plan
	acc.quotaPaid = q.Paid
	acc.quotaUpdatedAt = now
	// 冷却已到期时顺手把记录清干净（含 coolKind，避免留下「有 kind 无截止」的
	// 半截状态）。
	//
	// 这里**只做清理，不做解冻**：条件要求 now >= cooldownUntil（已过期），
	// 所以它永远不会把还在生效的冷却提前掐掉。真正「余额恢复 → 提前解除硬冷却」
	// 的语义在 ReviveIfCreditsRecovered（签到后与探测后调用）。
	//
	// 刻意不碰 modelCooldowns 与 softStreak：模型级台账（6004）与软限流退避的
	// 恢复证据是**上游重置墙钟到期**，不是余额恢复。余额刷新是周期性动作，
	// 在这里清它们会把限流冷却的实际寿命压到一个刷新周期内 ——
	// 撞限号会被误判健康、重新选中、再撞 429。
	if q.Remaining > 0 && !acc.cooldownUntil.IsZero() && now >= acc.cooldownUntil.Unix() {
		acc.cooldownUntil = time.Time{}
		acc.cooldownReason = ""
		acc.coolKind = CoolNone
	}
	acc.mu.Unlock()

	p.logf("[额度] 账号 %s [%s] 总额度=%.2f 已用=%.2f 剩余=%.2f 套餐=%s",
		acc.Cred.AccountID(), prof.Label, q.Total, q.Used, q.Remaining, q.Plan)
	if prevKnown && prevRemaining <= 0 && q.Remaining > 0 {
		p.logf("[额度] 账号 %s 额度已恢复，自动解除冻结并恢复调度", acc.Cred.AccountID())
	}
	return nil
}

// SetPlan 写回套餐展示名（完整识别后的结果，见 upstream.IdentifyPlan）。
//
// 为什么不并进 RefreshQuota：完整识别要多打一次分页权益查询，只适合按需触发
// （面板点一下），挂在周期性额度刷新上会成倍放大上游请求量。
func (p *Pool) SetPlan(acc *Account, plan string) {
	if acc == nil {
		return
	}
	acc.mu.Lock()
	acc.quotaPlan = plan
	acc.mu.Unlock()
}

// RefreshExpiringSnapshot 只拉积分包明细并记录到期快照，**不碰额度**。
//
// 与 RefreshQuota 拆开是刻意的：额度刷新成功后要做解冻（ReviveIfCreditsRecovered），
// 而明细要多打 paid/free 两个端点、更容易失败。若把两者绑成一个方法，
// 明细失败会连带跳过解冻 —— 那是比「没有到期数据」严重得多的回归。
//
// 失败时保留上一次的快照，不把它打回 0。
func (p *Pool) RefreshExpiringSnapshot(ctx context.Context, acc *Account) error {
	if acc == nil || acc.IsDisabled() {
		return errors.New("账号不可用，跳过积分明细查询")
	}
	expiry, err := p.client.FetchCreditResources(ctx, acc.View(), acc.Profile())
	if err != nil {
		return err
	}
	acc.SetExpiringSnapshot(expiry.ExpiringRemaining, expiry.SoonestExpireAt)
	return nil
}

// SetExpiringSnapshotOf 是 SetExpiringSnapshot 的池级入口（供 server 层调用）。
func (p *Pool) SetExpiringSnapshotOf(acc *Account, expiring float64, soonest int64) {
	if acc == nil {
		return
	}
	acc.SetExpiringSnapshot(expiring, soonest)
}

// SetExpiringSnapshot 记录「7 天内到期余额」与「最早到期时间」快照。
func (a *Account) SetExpiringSnapshot(expiring float64, soonest int64) {
	if expiring < 0 {
		expiring = 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// 钳到总额：上游脏数据不该让「快过期」超过「总剩余」。
	if a.quotaRemaining > 0 && expiring > a.quotaRemaining {
		expiring = a.quotaRemaining
	}
	a.quotaExpiring = expiring
	a.quotaSoonestExpire = soonest
}

// ExpiringSnapshot 返回 (7 天内到期余额, 最早到期时间 Unix 毫秒)。
func (a *Account) ExpiringSnapshot() (float64, int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.quotaExpiring, a.quotaSoonestExpire
}

// RefreshAllQuotas 并发刷新全部账号额度，返回 (成功数, 失败数)。
func (p *Pool) RefreshAllQuotas(ctx context.Context) (int, int) {
	return p.RefreshAllQuotasIncluding(ctx, false)
}

// RefreshAllQuotasIncluding 同 RefreshAllQuotas，includeDisabled 为真时连已禁用账号
// 一起刷新（schedule.include_disabled_in_tasks）。
func (p *Pool) RefreshAllQuotasIncluding(ctx context.Context, includeDisabled bool) (int, int) {
	accs := p.Accounts()
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		ok, failed int
		sem        = make(chan struct{}, 4) // 控制并发，避免同时打太多上游请求
	)
	for _, a := range accs {
		if a.IsDisabled() && !includeDisabled {
			continue
		}
		wg.Add(1)
		go func(acc *Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := p.refreshQuota(ctx, acc, includeDisabled)
			mu.Lock()
			if err != nil {
				failed++
			} else {
				ok++
			}
			mu.Unlock()
		}(a)
	}
	wg.Wait()
	return ok, failed
}

// Revive 人工复活账号：清禁用 + 清全部失败状态（冷却 / 熔断 / 连败降权）。
//
// 与 `SetDisabled(false)` 的差别是**刻意保留的**：
//
//	SetDisabled(false) 只清「禁用」与「冷却」—— 它服务的是「我上次禁用错了，改回来」，
//	                  所以不该顺手把熔断计数也抹掉（那会掩盖真实的持续故障）。
//	Revive            是运维口径的**无条件恢复**：账号在别处被证明可用了
//	                  （重登、换设备、上游修好了），要让它立刻回到可用状态。
//	                  此时留着熔断/降权只会让它继续被跳过，而用户看到的按钮
//	                  写着「已启用」，状态却依然是「熔断中」。
//
// 刻意**不**清余额类硬冷却（CoolHard）：那一条的依据是「余额为 0」这个客观事实，
// 人工点一下按钮并不会让余额回来，清了只会让它立刻又被冷却一次。
func (p *Pool) Revive(acc *Account) error {
	if acc == nil {
		return errors.New("账号为空")
	}
	// **刻意不调用 SetDisabled(false)**：那一项为了「刚启用不被旧冷却挡住」
	// 会**无条件清空冷却**，包括余额类硬冷却 —— 而硬冷却恰恰是复活唯一不该碰的
	// （它的依据是「余额为 0」这个客观事实）。
	// 委托给它会让「保留硬冷却」这条约定在实现里悄悄失效：代码注释写着保留，
	// 实际先被清掉了。所以这里自己改禁用位。
	acc.mu.Lock()
	acc.disabled = false
	acc.disabledReason = ""
	acc.mu.Unlock()

	// ClearFailureState 自己会跳过 CoolHard，正是我们要的语义。
	acc.ClearFailureState()
	return p.writeMarker(acc.Cred.Path, false)
}

// SetDisabled 手工启用 / 禁用账号，并落盘标记以便跨重启保持。
func (p *Pool) SetDisabled(acc *Account, disabled bool, reason string) error {
	if acc == nil {
		return errors.New("账号为空")
	}
	acc.mu.Lock()
	acc.disabled = disabled
	if disabled {
		acc.disabledReason = reason
		if acc.disabledReason == "" {
			acc.disabledReason = "已手工禁用"
		}
	} else {
		acc.disabledReason = ""
		// 启用时清掉冷却，避免刚启用就又被旧冷却挡住
		acc.cooldownUntil = time.Time{}
		acc.cooldownReason = ""
	}
	acc.mu.Unlock()
	return p.writeMarker(acc.Cred.Path, disabled)
}

// writeMarker 写 / 删禁用标记文件。
func (p *Pool) writeMarker(path string, disabled bool) error {
	marker := path + markerSuffix
	if !disabled {
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("清除禁用标记失败: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(marker, []byte("disabled\r\n"), 0o600); err != nil {
		return fmt.Errorf("写入禁用标记失败: %w", err)
	}
	return nil
}

// archiveDirName 是「已移除」凭据的归档目录名。
//
// 语义说明：账号池的来源是「凭据发现目录」（自动发现 workbuddy*.json 或 -auth-dir），
// 因此单纯把账号从内存里摘掉会被下一次热加载重新加回来。要让「移除」真正生效，
// 凭据文件必须离开发现路径。于是两种模式：
//   - deleteFile=true  → 直接删除文件（不可恢复）
//   - deleteFile=false → 移动到 <凭据目录>/.removed/（离开发现路径但可恢复）
const archiveDirName = ".removed"

// Remove 从账号池移除账号。deleteFile 为真时删除凭据文件，否则归档到 .removed/。
// 返回归档后的路径（删除模式返回空串）。
func (p *Pool) Remove(acc *Account, deleteFile bool) (string, error) {
	if acc == nil {
		return "", errors.New("账号为空")
	}
	path := acc.Cred.Path

	p.mu.Lock()
	idx := -1
	for i, a := range p.accounts {
		if a == acc {
			idx = i
			break
		}
	}
	if idx < 0 {
		p.mu.Unlock()
		return "", fmt.Errorf("%w: %s", ErrAccountNotFound, acc.Cred.AccountID())
	}
	p.accounts = append(p.accounts[:idx], p.accounts[idx+1:]...)
	if len(p.accounts) == 0 {
		p.rrIndex = 0
	} else if p.rrIndex >= len(p.accounts) {
		p.rrIndex %= len(p.accounts)
	}
	p.mu.Unlock()

	// 标记文件始终清理，避免残留导致下次登录后仍被判定禁用
	_ = p.writeMarker(path, false)

	if deleteFile {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("账号已从池中移除，但删除凭据文件失败: %w", err)
		}
		return "", nil
	}

	// 归档：移动到 <目录>/.removed/，该子目录不在凭据发现范围内
	dir := filepath.Join(filepath.Dir(path), archiveDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("账号已从池中移除，但创建归档目录失败: %w", err)
	}
	base := filepath.Base(path)
	archived := filepath.Join(dir, fmt.Sprintf("%s.%s", strings.TrimSuffix(base, ".json"),
		time.Now().Format("20060102-150405")+".json"))
	if err := os.Rename(path, archived); err != nil {
		return "", fmt.Errorf("账号已从池中移除，但归档凭据文件失败: %w", err)
	}
	return archived, nil
}

// Add 把新登录成功的凭据加入池（若同路径已存在则替换）。
func (p *Pool) Add(cred *auth.Credential) {
	if cred == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accounts {
		if a.Cred.Path == cred.Path {
			// 重新登录：替换凭据并清除失效状态
			a.mu.Lock()
			a.Cred = cred
			a.fingerprint = fileFingerprint(cred.Path)
			a.disabled = false
			a.disabledReason = ""
			a.cooldownUntil = time.Time{}
			a.cooldownReason = ""
			a.lastError = ""
			a.mu.Unlock()
			_ = a
			p.accounts[i] = a
			_ = os.Remove(cred.Path + markerSuffix)
			return
		}
	}
	acc := p.newAccount(cred)
	acc.fingerprint = fileFingerprint(cred.Path)
	p.accounts = append(p.accounts, acc)
}

// loadMarkers 读取目录下全部 .disabled 标记，供 Load / Reload 恢复禁用状态。
func (p *Pool) loadMarkers(paths []string) map[string]string {
	out := map[string]string{}
	for _, path := range paths {
		if _, err := os.Stat(path + markerSuffix); err == nil {
			out[path] = "已手工禁用（存在 .disabled 标记，删除该文件或重新登录可恢复）"
		}
	}
	return out
}

// CredentialPathFor 生成新登录账号的凭据文件路径。
// 放在「凭据目录」下（如果配置了 -auth-dir），否则放在工作目录。
func (p *Pool) CredentialPathFor(uid string) string {
	dir := p.cfg.WorkDir
	if strings.TrimSpace(p.cfg.AuthDir) != "" {
		dir = p.cfg.AuthDir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(p.cfg.WorkDir, dir)
		}
	}
	name := "workbuddy"
	if comp := auth.SanitizeFileNameComponent(uid); comp != "" {
		name += "-" + comp
	} else {
		name += "-" + time.Now().Format("20060102-150405")
	}
	return filepath.Join(dir, name+".json")
}

// fileFingerprint 返回凭据文件的 (mtime, size) 指纹，用于热加载判断文件是否真的变了。
// 文件不可读时返回空串（调用方据此走保守路径）。
func fileFingerprint(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", st.ModTime().UnixNano(), st.Size())
}

func (p *Pool) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}
