// Package scheduler 是定时任务调度器（切片 2）。
//
// 任务按「本地时区整点」触发，各自独立开关，互不影响；同一任务在同一小时内只跑一次。
// 设计上刻意不做「任务队列 + 持久化」：签到与保活都是幂等的上游调用，
// 进程重启后当小时内重跑一次没有副作用，简单可靠优先。
//
// 本切片实现三类任务：
//
//	checkin   每日签到 + 顺带刷新额度（余额恢复即解冻被硬冷却的账号）
//	keepalive 全量刷新令牌（保活，避免长期不用导致 refresh token 过期）
//	balance   周期刷新额度（两次签到时点之间保持积分新鲜）
//
// 切片 5 追加两类：
//
//	tasks      每日跑一次任务中心（报名 → 可自动完成 → 领奖）
//	blackcat   夜猫子任务独立排程（计分窗口 23:00-08:00，与上面时点不同）
//
// 切片 18 追加两类（对齐 wb2api-panel 的 travel / activity）：
//
//	travel     猫猫旅行巡检（默认 09 / 21 两个时点：出发与领奖各需一趟才闭环）
//	activity   对话活跃上报（默认 10 点）
//
// 这几类都不在这里实现具体逻辑，而是委托给 internal/tasks 的任务中心——
// 定时器只负责「什么时候跑」，怎么跑归任务中心管，避免两处各写一套语义。
//
// 与 wb2api-panel 的命名对照：它的 `growth`（成长任务队列）**就是我们的 `tasks`**
// （同一件事、不同默认时点），所以这里没有再立一个 growth 排程 ——
// 两个排程跑同一套逻辑只会让人以为它们是不同功能。
package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/tasks"
	"workbuddy-gateway/internal/upstream"
)

// CheckinRecorder 记录签到结果（积分统计的本地观察口径用）。
//
// 可选依赖：为 nil 时全部功能照常，只是签到记录不进本地台账。
type CheckinRecorder interface {
	RecordCheckin(accountID, accountName, site, result, errMsg string)
}

// Scheduler 按小时时点驱动定时任务。
type Scheduler struct {
	// cfgPtr 用原子指针持有配置：面板保存配置是「克隆 → 改克隆 → 换指针」，
	// 而排程循环在另一个 goroutine 里持续读。若这里固定持有构造时的那个对象，
	// 面板改完的签到时间段 / 排除名单 / 时点永远不会被排程看到 ——
	// 表现是「设置页改了但定时任务照旧跑」。
	cfgPtr   atomic.Pointer[config.Config]
	pool     *pool.Pool
	client   *upstream.Client
	recorder CheckinRecorder

	mu      sync.Mutex
	lastRun map[string]string // task -> "2006010215"（年月日时），保证同小时只跑一次
	Logf    func(format string, args ...any)

	// taskMgr 是任务中心，由 main 注入；未注入时任务类排程静默跳过。
	taskMgr TaskRunner
}

// TaskRunner 是任务中心暴露给调度器的最小接口。
//
// 直接用 tasks.RunOptions 而不是另立同构类型：两边各定义一份相同的结构，
// 以后加一个字段就要改两处，而漏改的那处不会编译报错、只会静默失效。
type TaskRunner interface {
	Run(ctx context.Context, opts tasks.RunOptions) int
	// TravelInspect 对全部可用账号推进一趟猫猫旅行（无猫→领养，有猫→派出/领奖/跳过）。
	TravelInspect(ctx context.Context) (string, error)
	// ReportActivity 补一条对话活跃上报；accountID 为空表示对全部可用账号各发一条。
	ReportActivity(ctx context.Context, accountID, model string) (string, error)
	// RunStreakBonus 连登管家：补签 → 礼包/补偿 → 兑换已解锁档位 → 抽完所有次数。
	RunStreakBonus(ctx context.Context, accountID string) (string, error)
}

// SetTaskManager 注入任务中心。
func (s *Scheduler) SetTaskManager(m TaskRunner) { s.taskMgr = m }

// New 构造调度器。
func New(cfg *config.Config, p *pool.Pool, client *upstream.Client) *Scheduler {
	s := &Scheduler{pool: p, client: client, lastRun: map[string]string{}}
	s.cfgPtr.Store(cfg)
	return s
}

// SetConfig 换入新的配置快照（面板保存配置后由 server 调用）。
func (s *Scheduler) SetConfig(cfg *config.Config) {
	if cfg != nil {
		s.cfgPtr.Store(cfg)
	}
}

// config 取当前生效的配置快照；未设置时返回 nil 安全语义的兜底空配置。
func (s *Scheduler) config() *config.Config {
	if cfg := s.cfgPtr.Load(); cfg != nil {
		return cfg
	}
	return &config.Config{}
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// SetCheckinRecorder 注入签到记录器（可选依赖，nil 安全）。
func (s *Scheduler) SetCheckinRecorder(rec CheckinRecorder) { s.recorder = rec }

// recordCheckin 把一次签到结果写进本地台账。
func (s *Scheduler) recordCheckin(a *pool.Account, result, errMsg string) {
	if s.recorder == nil {
		return
	}
	s.recorder.RecordCheckin(a.Cred.AccountID(), a.Cred.Nickname, a.Site(), result, errMsg)
}

// Start 启动调度循环（每分钟检查一次时点）。ctx 结束即停止。
func (s *Scheduler) Start(ctx context.Context) {
	go s.tickLoop(ctx)
	go s.balanceLoop(ctx)
}

// tickLoop 每分钟检查一次「今天有没有到点的任务还没跑」。
//
// **启动后先立刻检查一次**（dayOpen=true），不等第一个 60 秒 tick：
// 开机时刻若落在时点之后，这一刻就是补跑的时机 —— 让用户干等一分钟才看到
// 签到发生没有任何意义。检查本身很轻（几次时间比较 + 内存里的 claim 判断）。
//
// 首次检查还会带上 dayOpen：当天首次运行、时点又都还没到时，把**最早那个时点**
// 提前当成已到点（见 dayOpenSlot）。这样「打开软件就该把今天的签到做掉」成立，
// 而不必恰好等到 9 点整。
func (s *Scheduler) tickLoop(ctx context.Context) {
	s.checkHourlyTasks(ctx, true)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkHourlyTasks(ctx, false)
		}
	}
}

// checkHourlyTasks 检查各任务今天有没有到点。
//
// dayOpen 表示「这是当天第一次运行此检查」（tickLoop 启动时先调一次）。
func (s *Scheduler) checkHourlyTasks(ctx context.Context, dayOpen bool) {
	now := time.Now()
	cfg := s.config()

	// 各任务的时点判定统一走 dueSlot：它返回「今天已到点、且当天还没跑过」的槽位。
	// 槽位绑定**时点本身**而不是当前小时，所以补跑过后不会在下一个小时重复触发。
	// 每个任务各自判、各自 claim —— 六个时点互相独立，某个配错或关掉不影响其它。
	//
	// 「补跑已过的时点」对所有任务生效（早上 9:30 开机，9 点那趟补上）——
	// 旧实现只在整点那一小时才认，错过就整天不跑，这是「自启动了却没签到」的根因。

	// 签到：**软件一跑就签**，不等时点。
	//
	// 语义（按用户要求，2026-10-13 定）：
	//   · 已过时点 → 补跑最近那趟（9:30 开机补 9 点那趟）；
	//   · 还没到任何时点 → 启动那次直接提前签（8:00 开机就签，不等 9 点）。
	// 刻意**不看**签到时间段：那是面板里一个可选设置（默认空 = 不限制），
	// 用户的诉求是「开了就签」，不该因为配了时间段而在窗口外整轮跳过。
	//
	// 顺序很重要：先问 dueSlot（已到点），再问 dayOpenSlot（提前）。
	// 反过来写会重复签到 —— 22:00 启动时 dayOpenSlot 给出最早的 9 点槽位并签一次，
	// 下一分钟 dueSlot 又给出 21 点槽位，于是同一分钟内签两趟。
	//
	// 去重靠槽位绑定当日日期：先到先占，当天同一槽位不会被签第二次。
	// 于是常规一天最多两趟（9 点那趟 + 21 点那趟），启动早于所有时点时不额外多签。
	dueCheckin := func() (string, bool) {
		if slot, ok := dueSlot(now, cfg.Schedule.CheckinHours); ok {
			return slot, true
		}
		if dayOpen {
			return dayOpenSlot(now, cfg.Schedule.CheckinHours)
		}
		return "", false
	}

	if cfg.CheckinEnabled() {
		if slot, ok := dueCheckin(); ok && s.claim("checkin", slot) {
			s.logf("[调度] 到达签到时点（%02d:00），开始签到", slotHour(slot))
			if err := s.runCheckinSlot(ctx); err != nil {
				// 整轮没有任何成功（账号池为空 / 全部请求失败）时把槽位还回去，
				// 下一个 tick 还能再试。否则一次启动期的失败会让当天彻底不再签到。
				s.logf("[调度] 签到任务失败: %v（将在下一分钟重试）", err)
				s.release("checkin", slot)
			}
		}
	}
	if cfg.KeepaliveEnabled() {
		if slot, ok := dueSlot(now, cfg.Schedule.KeepaliveHours); ok && s.claim("keepalive", slot) {
			s.logf("[调度] 到达保活时点（%02d:00），开始刷新令牌（阈值 %d 天 / 惰性窗口 %d 小时）",
				slotHour(slot), cfg.KeepaliveDays(), cfg.LazyRefreshHours())
			if _, err := s.RunScheduledKeepalive(ctx); err != nil {
				s.logf("[调度] 保活任务失败: %v", err)
			}
		}
	}
	if cfg.TasksEnabled() {
		if slot, ok := dueSlot(now, cfg.Schedule.TasksHours); ok && s.claim("tasks", slot) {
			s.logf("[调度] 到达任务中心时点（%02d:00），开始执行成长任务", slotHour(slot))
			if _, err := s.RunTasksNow(ctx); err != nil {
				s.logf("[调度] 任务中心执行失败: %v", err)
			}
		}
	}
	if cfg.BlackCatEnabled() {
		if slot, ok := dueSlot(now, cfg.Schedule.BlackCatHours); ok && s.claim("blackcat", slot) {
			// 夜猫子的计分窗口是 23:00-08:00，而它的时点默认 23 点。
			// 补跑不会出问题：窗口外由任务中心自己拒绝并给出提示（见 RunBlackCatNow）。
			s.logf("[调度] 到达夜猫子时点（%02d:00），补报夜猫子任务", slotHour(slot))
			if _, err := s.RunBlackCatNow(ctx); err != nil {
				s.logf("[调度] 夜猫子任务失败: %v", err)
			}
		}
	}
	if cfg.TravelEnabled() {
		if slot, ok := dueSlot(now, cfg.Schedule.TravelHours); ok && s.claim("travel", slot) {
			s.logf("[调度] 到达旅行巡检时点（%02d:00），推进一趟猫猫旅行", slotHour(slot))
			if _, err := s.RunTravelNow(ctx); err != nil {
				s.logf("[调度] 旅行巡检失败: %v", err)
			}
		}
	}
	if cfg.ActivityEnabled() {
		if slot, ok := dueSlot(now, cfg.Schedule.ActivityHours); ok && s.claim("activity", slot) {
			s.logf("[调度] 到达活跃上报时点（%02d:00），补报对话活跃事件", slotHour(slot))
			if _, err := s.RunActivityNow(ctx); err != nil {
				s.logf("[调度] 活跃上报失败: %v", err)
			}
		}
	}
}

// dayOpenSlot 用于「当天首次运行」这一时刻：时点都还没到时，把**最早那个时点**
// 提前当成已到点，让当天第一次打开软件就把任务做掉。
//
// 为什么需要它：默认签到 / 任务时点是上午 9 点，而用户常常早上 8 点多开机 ——
// 此时 dueSlot 返回「还没到点」，于是要干等到 9:00 整。用户看到的仍然是
// 「软件开了，但今天还什么都没做」。把最早时点提前触发，符合
// 「每天首次运行就跑一次」的直觉。
//
// 只提前**最早**那个时点，不是把当天所有时点都跑一遍：后者会让 9/21 两趟
// 在早上一次性连发，而上游对同一账号的密集操作敏感（连发可能被判无效并回滚进度）。
// 21 点那趟仍按真实时点执行。
//
// 返回的槽位仍绑**该时点自己**（不是当前小时），所以等到真正 9:00 时
// dueSlot 给出的是同一个槽位，claim 会拦下重复执行。
func dayOpenSlot(now time.Time, hours []int) (string, bool) {
	earliest, found := 0, false
	for _, h := range hours {
		if h < 0 || h > 23 {
			continue
		}
		if !found || h < earliest {
			earliest, found = h, true
		}
	}
	if !found {
		return "", false
	}
	return fmt.Sprintf("%s-%d", now.Format("20060102"), earliest), true
}

// slotHour 从槽位标识（"20060102-9"）里取回时点，供日志显示。
// 与 dueSlot 是同一套格式的一对，改动要一起改。
func slotHour(slot string) int {
	i := strings.LastIndexByte(slot, '-')
	if i < 0 {
		return 0
	}
	h, err := strconv.Atoi(slot[i+1:])
	if err != nil {
		return 0
	}
	return h
}

// balanceLoop 按 balance_refresh_minutes 周期刷新额度。
//
// **启动时先刷一次**：原来只在第一个 ticker 周期（默认 5 分钟）之后才刷，
// 于是「打开软件」到「看到剩余积分」之间有一段空窗 —— 账号页与监控页的积分
// 要靠这次刷新才有值，用户看到的是空白，只能等或者手动点「刷新余额」。
//
// 首刷放在 goroutine 里（balanceLoop 本身由 Start 以 go 启动），不阻塞启动。
func (s *Scheduler) balanceLoop(ctx context.Context) {
	minutes := s.config().Schedule.BalanceRefreshMinutes
	if minutes <= 0 {
		return
	}
	s.refreshBalances(ctx, "启动首刷")
	ticker := time.NewTicker(time.Duration(minutes) * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshBalances(ctx, "周期刷新")
		}
	}
}

// refreshBalances 刷一次全部账号额度；配置关闭时直接返回。
//
// 周期刷新与启动首刷共用这一条路径，避免两处各写一遍
// 「先查开关、再刷、再按结果决定要不要打日志」而慢慢走岔。
func (s *Scheduler) refreshBalances(ctx context.Context, label string) {
	if !s.config().BalanceRefreshEnabled() {
		return
	}
	okCount, failed := s.pool.RefreshAllQuotasIncluding(ctx, s.includeDisabledInTasks())
	if okCount > 0 || failed > 0 {
		s.logf("[调度] %s额度：成功 %d，失败 %d", label, okCount, failed)
	}
}

// RunTasksNow 立即执行一次任务中心（报名 + 可自动完成 + 领奖）。
func (s *Scheduler) RunTasksNow(ctx context.Context) (string, error) {
	if s.taskMgr == nil {
		return "", fmt.Errorf("任务中心未启用")
	}
	n := s.taskMgr.Run(ctx, tasks.RunOptions{})
	if n < 0 {
		return "已有任务队列在执行中", nil
	}
	if n == 0 {
		return "没有需要执行的任务项", nil
	}
	return fmt.Sprintf("已提交 %d 个任务项到执行队列（结果见任务中心）", n), nil
}

// RunBlackCatNow 只跑夜猫子任务；窗口外由任务中心自己拒绝并给出提示。
func (s *Scheduler) RunBlackCatNow(ctx context.Context) (string, error) {
	if s.taskMgr == nil {
		return "", fmt.Errorf("任务中心未启用")
	}
	n := s.taskMgr.Run(ctx, tasks.RunOptions{TaskCodes: []string{"black_cat"}, Actions: []string{"auto"}})
	if n < 0 {
		return "已有任务队列在执行中", nil
	}
	if n == 0 {
		return "夜猫子任务当前无需执行（可能已达标或不在计分窗口）", nil
	}
	return fmt.Sprintf("已提交 %d 个夜猫子任务项", n), nil
}

// RunTravelNow 立即对全部可用账号推进一趟猫猫旅行巡检。
func (s *Scheduler) RunTravelNow(ctx context.Context) (string, error) {
	if s.taskMgr == nil {
		return "", fmt.Errorf("任务中心未启用")
	}
	return s.taskMgr.TravelInspect(ctx)
}

// RunActivityNow 立即对全部可用账号各补一条对话活跃上报。
//
// 传空 model 让上游用默认模型 —— 活跃上报的判据只看「有没有这条事件」，
// 具体模型不影响点亮结果，硬编一个模型名反而会在上游换默认模型时失配。
func (s *Scheduler) RunActivityNow(ctx context.Context) (string, error) {
	if s.taskMgr == nil {
		return "", fmt.Errorf("任务中心未启用")
	}
	return s.taskMgr.ReportActivity(ctx, "", "")
}

// claim 抢占某任务的本小时执行权；已被抢占返回 false。
func (s *Scheduler) claim(task, stamp string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastRun[task] == stamp {
		return false
	}
	s.lastRun[task] = stamp
	return true
}

// release 撤销一次 claim（任务整轮失败时调用）。
//
// 为什么必须有它：claim 在任务**开始前**占位，用来挡住同一槽位的重复触发。
// 但占位不等于成功 —— 启动瞬间令牌还没就绪、上游抖动、账号池暂时为空，
// 都会让这一轮什么都不做；若照样占着槽位，当天后续每小时都不会再试，
// 用户看到的就是「自动签到完全没签」，而且日志里只有一条失败。
//
// 只在该槽位仍是自己占的那个时才撤销：并发下另一个槽位已经占上时不能误删。
func (s *Scheduler) release(task, stamp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastRun[task] == stamp {
		delete(s.lastRun, task)
	}
}

// LastRun 返回任务最近一次执行的小时标记（面板展示用）。
func (s *Scheduler) LastRun(task string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRun[task]
}

// -----------------------------------------------------------------------------
// 手动触发入口（面板按钮 / 立即执行）
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// 签到：结构化结果与批量入口
// -----------------------------------------------------------------------------

// CheckinOutcome 是单账号签到的处置分类。
type CheckinOutcome string

const (
	// CheckinSuccess 签到成功。
	CheckinSuccess CheckinOutcome = "success"
	// CheckinAlready 今天已签到（上游幂等语义，不是失败）。
	CheckinAlready CheckinOutcome = "already"
	// CheckinFailed 签到失败（含 token 刷新失败）。
	CheckinFailed CheckinOutcome = "failed"
	// CheckinSkipped 未参与本次签到（已被排除自动签到 / 该站点没有签到接口 / 账号被禁用）。
	CheckinSkipped CheckinOutcome = "skipped"
)

// CheckinSummary 是一次批量签到的结构化计数（对齐 wb-switch 的 checkin_all 汇总）。
//
// `skipped` 把「排除名单 / 站点不支持 / 已禁用」合并成一类：三者对用户的观感
// 都是「这轮没签它」，而区分它们需要把账号状态一起带回前端 ——
// 卡片上已经有逐账号的说明，toast 只需要一个总数。
type CheckinSummary struct {
	Success int `json:"success"`
	Already int `json:"already"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	// OutsideWindow 为真表示整轮因不在签到时间段内被跳过（此时各计数为 0）。
	OutsideWindow bool `json:"outside_window"`
	// FirstError 是第一个失败的原始错误（便于面板给出可读原因）。
	FirstError string `json:"first_error,omitempty"`
}

// Count 归入一次处置结果；errMsg 非空时记录首个错误。
func (s *CheckinSummary) Count(outcome CheckinOutcome, errMsg string) {
	switch outcome {
	case CheckinSuccess:
		s.Success++
	case CheckinAlready:
		s.Already++
	case CheckinFailed:
		s.Failed++
	default:
		s.Skipped++
	}
	if errMsg != "" && s.FirstError == "" {
		s.FirstError = errMsg
	}
}

// Detail 组装人类可读的结果摘要（排程日志与面板「立即签到」共用）。
func (s CheckinSummary) Detail() string {
	detail := fmt.Sprintf("签到完成：成功 %d，已签过 %d，失败 %d", s.Success, s.Already, s.Failed)
	if s.Skipped > 0 {
		detail += fmt.Sprintf("，跳过 %d（无签到接口或已关闭自动签到）", s.Skipped)
	}
	if s.FirstError != "" {
		detail += "；首个错误: " + s.FirstError
	}
	return detail
}

// RunCheckinNow 对全部可用账号执行签到，并在签到后刷新额度（余额恢复即解冻）。
//
// 返回人类可读摘要；结构化计数（面板批量签到用）走 RunCheckinNowSummary。
func (s *Scheduler) RunCheckinNow(ctx context.Context) (string, error) {
	sum, err := s.RunCheckinNowSummary(ctx)
	if err != nil {
		return "", err
	}
	detail := sum.Detail()
	s.logf("[签到] %s", detail)
	return detail, nil
}

// runCheckinSlot 是排程用的签到入口：只有当这一轮**一条都没成功**时才报错，
// 让调用方把占位还回去、下一个 tick 重试。
//
// 语义分界：
//   - 账号池为空 → 报错（多半是启动时凭据还没加载完，重试就能好）；
//   - 全部账号失败 → 报错（大概率是上游/网络问题，重试是唯一出路）；
//   - 有任何一条成功或「今天已签」→ 不报错（这一轮已经达成目的，不该重跑）。
//
// 单个账号失败不单独触发整轮重试：签到是逐账号的，一个号失败不该让其它
// 已经签过的号在下一分钟被再打一遍（上游对密集操作敏感）。
func (s *Scheduler) runCheckinSlot(ctx context.Context) error {
	sum, err := s.RunCheckinNowSummary(ctx)
	if err != nil {
		return err
	}
	if sum.Success > 0 || sum.Already > 0 {
		return nil
	}
	if sum.Failed > 0 {
		return fmt.Errorf("本轮无任何账号签到成功（失败 %d）：%s", sum.Failed, sum.FirstError)
	}
	// 全是 skipped：没有国内站/非企业版账号可签，属于正常情况，不重试。
	return nil
}

// RunCheckinNowSummary 与 RunCheckinNow 同一套逻辑，返回结构化计数。
//
// 排除名单（config.schedule.checkin_excluded_accounts）在这里生效：名单内的
// 账号**不查状态、不刷 token、不提交签到**，只计入 skipped；额度刷新照做 ——
// 排除的是「签到动作」，不是「查询余额」。单账号手动签到不经过这里，
// 因此永远不受名单限制（显式意图不该被策略否决）。
func (s *Scheduler) RunCheckinNowSummary(ctx context.Context) (CheckinSummary, error) {
	return s.runCheckin(ctx, s.excludedCheckinIDs())
}

// excludedCheckinIDs 读取当前配置的排除名单并转成集合。
func (s *Scheduler) excludedCheckinIDs() map[string]bool {
	ids := s.config().CheckinExcludedAccountIDs()
	if len(ids) == 0 {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// checkinAction 是某账号在一轮批量签到里的处置决定。
type checkinAction int

const (
	// actionCheckin 参与签到（该站点是否真的提交再看 SupportsCheckin）。
	actionCheckin checkinAction = iota
	// actionSkipAndRefresh 跳过签到但仍刷新额度（排除名单：排除的是签到动作，不是余额查询）。
	actionSkipAndRefresh
	// actionSkipOnly 完全跳过（已禁用账号）。
	actionSkipOnly
)

// includeDisabledInTasks 报告「保号类任务是否覆盖已禁用账号」
// （schedule.include_disabled_in_tasks，缺省 false = 禁用即跳过）。
//
// 每次现场读配置：面板改开关走「换配置指针」热生效路径，持有构造时那一份会让勾选永远不生效。
func (s *Scheduler) includeDisabledInTasks() bool {
	return s.config().IncludeDisabledInTasks()
}

// decideCheckinAction 是处置决定的纯函数（便于单测）：
// 禁用（且未开启覆盖）> 排除名单 > 正常参与。
//
// includeDisabled 为真时禁用账号照常签到 —— 「禁用」只关选号、不停保号：
// 被禁用的账号同样需要积分回血（人工「解冻」是唯一的人工路径，签到是唯一的自动路径）。
func decideCheckinAction(disabled, excluded, includeDisabled bool) checkinAction {
	switch {
	case disabled && !includeDisabled:
		return actionSkipOnly
	case excluded:
		return actionSkipAndRefresh
	default:
		return actionCheckin
	}
}

// runCheckin 是批量签到的唯一实现（排除名单由调用方给出，nil = 不排除）。
func (s *Scheduler) runCheckin(ctx context.Context, excluded map[string]bool) (CheckinSummary, error) {
	var sum CheckinSummary
	accs := s.pool.Accounts()
	if len(accs) == 0 {
		return sum, fmt.Errorf("账号池为空，无账号可签到")
	}
	for _, a := range accs {
		switch decideCheckinAction(a.IsDisabled(), excluded[a.Cred.AccountID()], s.includeDisabledInTasks()) {
		case actionSkipOnly:
			sum.Count(CheckinSkipped, "")
			continue
		case actionSkipAndRefresh:
			sum.Count(CheckinSkipped, "")
			s.refreshQuotaAfterCheckin(ctx, a)
			continue
		}
		if err := s.pool.EnsureToken(ctx, a); err != nil {
			sum.Count(CheckinFailed, err.Error())
			continue
		}
		// 签到有两道门控，彼此正交：
		//   - 站点：只有国内站有签到接口（Profile.SupportsCheckin，与参考实现的
		//     WbVariant::supports_checkin 同口径）；
		//   - 账号类型：企业版没有个人成长体系，上游对签到一律
		//     400 code 10001「企业账号不支持该操作」（auth.Credential.IsEnterprise）。
		// 两者都**只跳过签到本身**，下面的额度刷新照做 —— 余额查询两站、两类账号
		// 都需要，一起跳过会让它们永远刷不到余额。
		if !a.Profile().SupportsCheckin() || a.Cred.IsEnterprise() {
			sum.Count(CheckinSkipped, "")
		} else {
			res, err := s.client.Checkin(ctx, a.View(), a.Profile())
			switch {
			case err != nil:
				sum.Count(CheckinFailed, err.Error())
				s.logf("[签到] 账号 %s 失败: %v", a.Cred.AccountID(), err)
				s.recordCheckin(a, "error", err.Error())
			case res.Already:
				sum.Count(CheckinAlready, "")
				s.recordCheckin(a, "already", "")
			default:
				sum.Count(CheckinSuccess, "")
				s.recordCheckin(a, "success", "")
			}
		}
		s.refreshQuotaAfterCheckin(ctx, a)
	}
	// 签到之后跑一遍连登管家（对照 wb2api-panel：签到排程末尾调用）。
	// 连登档位按连续登录天数解锁，刚签完正是可能跨过阈值的那一次；
	// 整段是幂等的（已领/未解锁自动跳过），失败只记日志、不影响签到结果。
	if s.taskMgr != nil {
		if detail, err := s.taskMgr.RunStreakBonus(ctx, ""); err != nil {
			s.logf("[连登] 管家执行失败: %v", err)
		} else if detail != "" {
			s.logf("[连登] %s", detail)
		}
	}
	return sum, nil
}

// refreshQuotaAfterCheckin 签到后刷新额度：额度恢复时自动解冻被硬冷却的账号
// （与 wb-gateway 同口径），并刷新「7 天内到期 / 最早到期」快照供选号层使用。
// 失败只记日志，不回退已经完成的签到结果。
func (s *Scheduler) refreshQuotaAfterCheckin(ctx context.Context, a *pool.Account) {
	// 已禁用账号（include_disabled_in_tasks 打开时）走允许禁用号的路径：
	// 否则 pool 的守卫会直接拒绝，禁用号的积分永远不刷新。
	var err error
	if a.IsDisabled() {
		err = s.pool.RefreshQuotaIncludingDisabled(ctx, a)
	} else {
		err = s.pool.RefreshQuota(ctx, a)
	}
	if err != nil {
		return
	}
	a.ReviveIfCreditsRecovered(a.Credits())
	if err := s.pool.RefreshExpiringSnapshot(ctx, a); err != nil {
		s.logf("[签到] 账号 %s 积分到期快照刷新失败: %v", a.Cred.AccountID(), err)
	}
}

// RunCheckinNowIfInWindow 在签到时间段内才执行签到，窗口外整轮跳过（切片 18）。
//
// 与 RunCheckinNow 的差别只有一个：**是否遵守配置的签到时间段**。
// 两者的分工是刻意的：
//
//	RunCheckinNow            排程补跑 / 「刷新并签到」→ 策略语义，
//	                         窗口外一律不查状态、不提交、不写日志；
//	RunCheckinNowIfInWindow  与上面同逻辑，只是多返回一个 skipped 供面板分支。
//
// 注意：单账号手动签到根本不经这里（见 internal/server 的 handleAccountCheckin），
// 所以窗口不会否决一个明确的用户意图。
//
// 返回 skipped=true 表示整轮被窗口跳过 —— 这是**正常结果而不是错误**：
// 上游一次请求都没发，账号状态也完全没动。
func (s *Scheduler) RunCheckinNowIfInWindow(ctx context.Context) (detail string, skipped bool, err error) {
	cfg := s.config()
	if !cfg.InCheckinWindow(time.Now()) {
		start, end, _ := cfg.CheckinWindow()
		// 刻意不写日志：窗口外每被调用一次就写一行，会在跨天后刷出一串
		// 「什么都没做」的记录，把真正有用的日志淹掉（参考实现同口径）。
		return fmt.Sprintf("当前不在签到时间段内（%s-%s），已整轮跳过（未查询状态、未提交）",
			minutesToClock(start), minutesToClock(end)), true, nil
	}
	d, err := s.RunCheckinNow(ctx)
	return d, false, err
}

// minutesToClock 把当日分钟数格式化成 HH:MM（仅用于面向用户的说明文案）。
func minutesToClock(m int) string {
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

// KeepaliveWindow 是排程保活的判定窗口（纯数据，便于单测）。
//
// 判定规则（与 wb-switch 的 checkin 配置 `keepalive_days` / `lazy_refresh_hours` 对齐）：
//   - Days <= 0：无条件刷新（默认语义）；
//     Days > 0 时仅刷新「到期时间未知或剩余有效期不足 Days 天」的账号。
//   - LazyHours > 0：距上次成功刷新不足该小时数的账号跳过，避免同一天多个
//     保活时点重复刷新同一个账号。
//
// 零值 {0, 0} 表示「全部立即刷新」——面板手动按钮传零值窗口，
// 因此**手动保活永远不受阈值/窗口限制**（显式意图不该被策略否决）。
type KeepaliveWindow struct {
	Days      int
	LazyHours int
}

// Due 报告某账号在给定窗口下是否需要保活刷新。
func (w KeepaliveWindow) Due(expiresAt int64, refreshedAt, now time.Time) bool {
	if w.LazyHours > 0 && !refreshedAt.IsZero() {
		if now.Sub(refreshedAt) < time.Duration(w.LazyHours)*time.Hour {
			return false
		}
	}
	if w.Days <= 0 {
		return true
	}
	if expiresAt <= 0 {
		// 到期时间未知：无法证明它还新鲜，按需要保活处理。
		return true
	}
	return expiresAt-now.Unix() < int64(w.Days)*24*60*60
}

// RunKeepaliveNow 对全部账号**立即**刷新令牌（面板手动按钮语义）。
//
// 刻意不经过 KeepaliveWindow 的阈值与惰性窗口：用户点了「立即保活」，
// 期望就是全部刷一遍；被「剩余有效期充足」拦下会让人以为按钮失灵。
func (s *Scheduler) RunKeepaliveNow(ctx context.Context) (string, error) {
	return s.runKeepalive(ctx, KeepaliveWindow{})
}

// RunScheduledKeepalive 是**排程**保活入口：按配置的保活阈值与惰性窗口筛选账号。
func (s *Scheduler) RunScheduledKeepalive(ctx context.Context) (string, error) {
	cfg := s.config()
	return s.runKeepalive(ctx, KeepaliveWindow{
		Days:      cfg.KeepaliveDays(),
		LazyHours: cfg.LazyRefreshHours(),
	})
}

// runKeepalive 是保活刷新的唯一实现（手动与排程共用，只有窗口不同）。
func (s *Scheduler) runKeepalive(ctx context.Context, window KeepaliveWindow) (string, error) {
	accs := s.pool.Accounts()
	if len(accs) == 0 {
		return "", fmt.Errorf("账号池为空，无账号可保活")
	}
	now := time.Now()
	var okCount, failed, skipped int
	var firstErr string
	includeDisabled := s.includeDisabledInTasks()
	for _, a := range accs {
		if a.IsDisabled() && !includeDisabled {
			continue
		}
		if !window.Due(a.TokenExpiresAt(), a.TokenRefreshedAt(), now) {
			skipped++
			continue
		}
		// 已禁用账号（只有 include_disabled_in_tasks 打开时才会走到这里）走允许禁用号的
		// 刷新路径：否则 pool 的 disabled 守卫会直接返回「账号已失效」，保活形同空转。
		if a.IsDisabled() {
			err := s.pool.ForceRefreshTokenIncludingDisabled(ctx, a)
			if err != nil {
				failed++
				if firstErr == "" {
					firstErr = err.Error()
				}
				s.logf("[保活] 账号 %s（已禁用）刷新失败: %v", a.Cred.AccountID(), err)
				continue
			}
			okCount++
			continue
		}
		if err := s.pool.ForceRefreshToken(ctx, a); err != nil {
			failed++
			if firstErr == "" {
				firstErr = err.Error()
			}
			s.logf("[保活] 账号 %s 刷新失败: %v", a.Cred.AccountID(), err)
			continue
		}
		okCount++
	}
	detail := fmt.Sprintf("令牌保活完成：成功 %d，失败 %d", okCount, failed)
	if skipped > 0 {
		detail += fmt.Sprintf("，跳过 %d（剩余有效期充足或刚刷新过）", skipped)
	}
	if firstErr != "" {
		detail += "；首个错误: " + firstErr
	}
	s.logf("[保活] %s", detail)
	return detail, nil
}

// RunBalanceNow 立即刷新全部账号额度。
func (s *Scheduler) RunBalanceNow(ctx context.Context) (string, error) {
	accs := s.pool.Accounts()
	if len(accs) == 0 {
		return "", fmt.Errorf("账号池为空，无账号可查询")
	}
	okCount, failed := s.pool.RefreshAllQuotasIncluding(ctx, s.includeDisabledInTasks())
	detail := fmt.Sprintf("额度刷新完成：成功 %d，失败 %d", okCount, failed)
	s.logf("[额度] %s", detail)
	return detail, nil
}

// dueSlot 返回「今天已到点、且本进程今天还没跑过」的时点槽位。
//
// 返回值是槽位标识（形如 "20261013-9"：当天日期 + 时点），claim 用它去重。
// **不能拿「当前小时」当标识** —— 补跑的正是「当前小时 ≠ 时点」这种情况：
// 10 点补跑了 9 点那趟，若标识记成 10，11 点时又会被判成"新槽位"再跑一遍。
// 绑定时点本身，同一时点当天才真正只跑一次。
//
// 为什么需要补跑，而不是简单地 hourIn(now.Hour(), hours)：
//
// 后者要求「当前这一小时恰好等于时点」。而检查是每分钟一次，于是**开机时刻
// 落在时点之后**（比如 9 点的签到、10 点才开机）时，当前小时永远等不到 `9`，
// 当天的签到就整个漏掉，且没有任何提示 —— 用户看到的是「软件自启动了，
// 但今天的签到没做」。真实踩到过：08:38 开机、签到时点 09/21，
// 一直等到 09:00 那个整点才跑第一次；若 09:30 才开机则当天整趟不跑。
//
// 新判据：取今天所有 ≤ 当前小时的时点里最大的那个，看它今天跑过没有。
// 于是 09:30 开机 → 立刻补跑 9 点那趟；21:00 整点 → 跑 21 点那趟。
// **跨天不补**：昨天错过的时点不在候选内（now 已是新的一天）。
//
// 返回的 ok 为 false 表示「今天还没有任何时点到点」（比如时点配在 23 点、
// 现在才 08:00），调用方跳过即可。
func dueSlot(now time.Time, hours []int) (slot string, ok bool) {
	best, found := 0, false
	for _, h := range hours {
		// 忽略越界时点：配置里打错一个数字不该让整个任务当天空转，
		// 也不该被当成"0 点"去误触发。
		if h < 0 || h > 23 {
			continue
		}
		if h <= now.Hour() && (!found || h > best) {
			best, found = h, true
		}
	}
	if !found {
		return "", false
	}
	return fmt.Sprintf("%s-%d", now.Format("20060102"), best), true
}
