package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// 本节是切片 2 新增的治理与调度配置。
//
// 设计取舍：**时长一律用秒/分钟的整数表达**，不用 Go 的 duration 字符串（如 "30m"）。
// 原因是这份配置要由面板表单直接编辑，整数输入框比字符串解析更不容易出错；
// 内部再换算成 time.Duration。

// CooldownSection 是冷却与熔断参数。
type CooldownSection struct {
	SoftRateSeconds           int `json:"soft_rate_seconds"`
	SoftRateMaxSeconds        int `json:"soft_rate_max_seconds"`
	DegradeThreshold          int `json:"degrade_threshold"`
	DegradeCooldownSeconds    int `json:"degrade_cooldown_seconds"`
	DegradeCooldownMaxSeconds int `json:"degrade_cooldown_max_seconds"`
}

// PoolSection 是账号池与选号参数。
type PoolSection struct {
	MaxInFlight               int     `json:"max_in_flight"`
	MaxInFlightGlobal         int     `json:"max_in_flight_global"`
	BreakerThreshold          int     `json:"breaker_threshold"`
	BreakerCooldownSeconds    int     `json:"breaker_cooldown_seconds"`
	BreakerCooldownMaxSeconds int     `json:"breaker_cooldown_max_seconds"`
	IdleWeightPerHour         float64 `json:"idle_weight_per_hour"`
	IdleWeightMax             float64 `json:"idle_weight_max"`
	// PreferFreeSite 开启「免费站点优先」：同一模型在两站之间只有一侧确认免费、
	// 另一侧确认收费时，把调度倾斜到免费那侧。用指针以区分「没提交」与「显式关掉」。
	PreferFreeSite *bool `json:"prefer_free_site"`
	// CreditFloor 是积分保底（切片 18）：账号余额**已实测**低于该值时，
	// 不再让它承接「实测收费」的模型 —— 防止收费请求把余额打穿，
	// 之后连免费模型都被 402 冷却到次日签到（最坏 11 小时不可用）。
	//
	// 默认 0 = 关闭（行为与引入前完全一致）；负值钳 0。
	// 免费（tier 0）与无观测（tier 1）的站点**不受限**，理由见 pool.PickOptions。
	CreditFloor int64 `json:"credit_floor"`
}

// StickySection 是会话粘性参数。Enabled 用指针区分「未配置」与「显式关掉」。
type StickySection struct {
	Enabled           *bool `json:"enabled"`
	TTLSeconds        int   `json:"ttl_seconds"`
	GCIntervalSeconds int   `json:"gc_interval_seconds"`
}

// ScheduleSection 是定时任务参数。
type ScheduleSection struct {
	CheckinEnabled        *bool `json:"checkin_enabled"`
	CheckinHours          []int `json:"checkin_hours"`
	KeepaliveEnabled      *bool `json:"keepalive_enabled"`
	KeepaliveHours        []int `json:"keepalive_hours"`
	BalanceRefreshEnabled *bool `json:"balance_refresh_enabled"`
	BalanceRefreshMinutes int   `json:"balance_refresh_minutes"`

	// KeepaliveDays 是排程保活的阈值（天）：0 = 每天无条件刷新全部账号；
	// 大于 0 时只刷新**剩余有效期不足该天数**（或到期时间未知）的账号。
	//
	// 语义与 wb-switch 的 checkin 配置 `keepalive_days` 完全一致（那边默认 0）。
	// 它只作用于**排程**保活：面板上的「令牌保活」手动按钮仍是立即语义
	//（显式意图不该被窗口/阈值否决），见 scheduler.RunKeepaliveNow 的注释。
	//
	// 为什么要这个阈值：高频保活是为了避免官方服务端清理闲置的 refresh 会话，
	// 但每天对全部账号无条件刷新会白白拉长令牌轮换链路；阈值让「快过期的才刷」。
	KeepaliveDays int `json:"keepalive_days"`
	// LazyRefreshHours 是排程保活的惰性窗口（小时）：距上次成功刷新不足该小时数的
	// 账号**跳过**，避免同一天多个保活时点重复刷新同一个账号。
	//
	// 默认 24（与 wb-switch 的 `lazy_refresh_hours` 默认一致）；配置缺失/非正数
	// 时回落到 24。注意这与 wb-switch 在取积分/签到前的 ensure_fresh_token 判定
	// 不同：那边是「剩余不足 N 小时就刷新」，本项目把该职责交给
	// pool.EnsureToken（距过期 15 分钟才刷）；这里的窗口只用于**排程保活去重**。
	LazyRefreshHours int `json:"lazy_refresh_hours"`

	// 任务中心（切片 5）：每日自动跑「报名 + 可自动完成 + 领奖」。
	//
	// 这一项与 wb2api-panel 的 `growth`（成长任务队列）**是同一件事**，
	// 只是默认时点不同（我们 09:00、对方 01:00，理由是对付零点半解锁的
	// Sequential 族）。所以这里不再另立一个 growth 排程 —— 两个排程跑同一套
	// 逻辑只会让人误以为它们是不同功能。
	TasksEnabled *bool `json:"tasks_enabled"`
	TasksHours   []int `json:"tasks_hours"`
	// 夜猫子任务独立排程：它的计分窗口是 23:00-08:00，必须单独定时点。
	BlackCatEnabled *bool `json:"blackcat_enabled"`
	BlackCatHours   []int `json:"blackcat_hours"`

	// 旅行巡检（切片 18）：对每个可用账号推进一趟猫猫旅行（无猫→领养，
	// 有猫→按状态派出/领奖/跳过）。默认 09 与 21 两个时点 —— 出发与领奖
	// 各需一趟，同一账号一天只能走完一轮，所以要两个时点才闭环。
	TravelEnabled *bool `json:"travel_enabled"`
	TravelHours   []int `json:"travel_hours"`
	// 活跃上报（切片 18）：每个可用账号补一条对话活跃事件。
	//
	// 为什么值得单独排程：连登与「领取一只 Buddy」的前置都是「当日有活跃上报」，
	// 而它**不属于任务中心的任何一条待办** —— 只在用户主动点「补活跃」时才会发。
	// 不做排程的话，不主动点按钮就永远差这一天。
	ActivityEnabled *bool `json:"activity_enabled"`
	ActivityHours   []int `json:"activity_hours"`

	// IncludeDisabledInTasks 让「保号类」任务（签到 / 活跃上报 / token 保活 /
	// 余额刷新）对**已禁用（disabled）**的账号也执行。
	//
	// 为什么需要它：面板「禁用」的语义是「不再参与选号」，但这四类任务此前一律跳过
	// 禁用号，等于把「停用流量」放大成「停止一切上游保号行为」—— 被禁用的号拿不到
	// 签到积分、不续 token、余额也不再刷新，而池子的解冻逻辑明确不复活 disabled，
	// 于是签到这条唯一的自动回血路径也断了，账号只能靠人工「解冻」回来。
	//
	// 对「一次只放开一个号、用禁用做流量开关」的轮换用法（同 IP 多号防风控），
	// 闲置待命的号恰恰是最需要签到的那批 —— 本开关即为该用法提供出口。
	//
	// 缺省 false = 保持既有行为，对老配置零影响。打开后禁用号仍会签到 / 保活 /
	// 刷新余额，但**依旧不参与选号**：pool 选号侧的 disabled 过滤不受本开关影响。
	// 只覆盖这四类保号任务；猫猫旅行、夜猫子、连登管家与成长任务队列仍跳过禁用号。
	IncludeDisabledInTasks *bool `json:"include_disabled_in_tasks"`

	// CheckinStart / CheckinEnd 是签到的允许时间段（"HH:MM"，**左闭右开**）。
	//
	// 作用：只影响显式要求「遵守时间段」的调用（面板的「刷新并签到」与排程补跑）——
	// 在窗口外整轮跳过，不查状态、不提交、不写日志。用户直接点单账号「签到」
	// 保持立即语义，不会被窗口拦住（那是显式意图，不该被策略否决）。
	//
	// 两个字段都为空 = 不限制（默认，行为与引入前一致）。
	// 只填一个、格式非法、start >= end（含跨午夜）一律视为**配置错误**并在启动时拒绝：
	// 静默把窗口关掉会让用户以为「我设了 9:00-11:00」，实际全天可签 ——
	// 这正是本项目一贯要避免的「改了没生效」。
	CheckinStart string `json:"checkin_start"`
	CheckinEnd   string `json:"checkin_end"`

	// CheckinExcludedAccounts 是「不参与自动签到」的账号 ID 名单
	//（存 AccountID / 凭据文件名，见 pool.AccountState.ID）。
	//
	// 语义与 wb-switch 的 `excluded_account_ids` 对齐：排程签到与批量
	// 「刷新积分并签到」会跳过名单内的账号（不查状态、不刷 token、不提交），
	// 但**单账号手动签到不受影响** —— 那是显式意图，不该被名单否决。
	//
	// 只认稳定 ID，不用昵称/邮箱匹配：后者会因改名、同名而误伤别的账号。
	CheckinExcludedAccounts []string `json:"checkin_excluded_accounts"`
	// CheckinExcludedAccountsCamel 兼容手写 config.json 的驼峰写法。
	// 保存补丁时会被归一进 CheckinExcludedAccounts 并清空，界面上不会出现两份。
	CheckinExcludedAccountsCamel []string `json:"checkinExcludedAccounts"`
}

// TasksSection 是任务中心的策略开关（切片 18）。
type TasksSection struct {
	// DesktopEventsEnabled 允许上报**客户端指纹事件链**类任务。
	//
	// 覆盖两套通道，判据都是「客户端行为事件」而非对话活跃：
	//   - 桌面端（workbuddy-desktop 指纹）：模板 / 灵感案例 / 设计画布 / 资料库介绍 /
	//     换肤 / 专家召唤与使用 / 技能加载 / 桌面端对话；
	//   - 小程序端（workbuddy-mp 指纹）：校园日与 Sequential_Tasks_1..7。
	//
	// 注意**专家系与技能**这一类与其它不同：它们不是「只发事件」，
	// 专家 id 取自真实市场列表、对话是真的对话、事件的 requestId 取自服务端返回 ——
	// 也就是说它们伪造的只是「用户点了召唤」这个动作，而不是整条链路。
	//
	// 默认 **false**，理由要说清楚：这些通路是**按参考实现的实测样本复刻的事件形状**，
	// 不是官方接口。形状是确定的（每个字段都有样本依据），但「上游是否接受、
	// 是否会计数」**无法离线验证** —— 要验证必须拿真实账号发一次并看任务进度，
	// 那会消耗额度。
	//
	// 还有一个更重要的理由：伪造客户端事件本身有**被风控识别的风险**，
	// 而代价由账号承担。本项目此前明确把这类动作归为「需客户端、不做」；
	// 现在把它做成**显式开关**而不是默认行为，是为了让打开它成为一个
	// 有意识的决定，而不是某次升级后的意外变化。
	DesktopEventsEnabled *bool `json:"desktop_events_enabled"`
}

// PromptSection 是提示词与出站改写参数（切片 4）。
//
// 三个开关都用指针：它们默认 true，用指针才能区分「没提交」与「显式关掉」，
// 否则前端只改一个开关会把另外两个静默重置成 false。
type PromptSection struct {
	// Mode 见 internal/outbound 的三个模式常量；空串按 append 处理。
	Mode string `json:"mode"`
	// Text 是网关自有提示词正文；留空表示用内置默认提示词。
	Text string `json:"text"`
	// File 非空时优先于 Text（由服务端读取文件内容）。
	File string `json:"file"`
	// Sanitize 出站指纹脱敏。
	Sanitize *bool `json:"sanitize"`
	// DegradedRetry 被内容策略拦截后换中性提示词重试一次。
	DegradedRetry *bool `json:"degraded_retry"`
	// StrictFirstSystem 首条消息非 system 时补一条保底 system。
	StrictFirstSystem *bool `json:"strict_first_system"`
}

// LocalSection 是本机代理参数（切片 7）。
//
// Enabled 用指针：默认 true，但服务端部署会显式关掉，必须能区分「没提交」与「显式关掉」。
type LocalSection struct {
	Enabled *bool `json:"enabled"`
	// Bin 是二进制名（不含扩展名）；留空用 wb-local-agent。
	Bin string `json:"bin"`
	// DataDir 透传给本机代理的客户端数据目录；留空由代理自己推断。
	DataDir string `json:"data_dir"`
}

// ApplyDefaults 填充未配置字段的默认值（口径与 wb2api-panel 一致）。
func (c *Config) ApplyDefaults() {
	if c.Cooldown.SoftRateSeconds <= 0 {
		c.Cooldown.SoftRateSeconds = 600
	}
	if c.Cooldown.SoftRateMaxSeconds <= 0 {
		c.Cooldown.SoftRateMaxSeconds = 7200
	}
	if c.Cooldown.DegradeThreshold <= 0 {
		c.Cooldown.DegradeThreshold = 5
	}
	if c.Cooldown.DegradeCooldownSeconds <= 0 {
		c.Cooldown.DegradeCooldownSeconds = 600
	}
	if c.Cooldown.DegradeCooldownMaxSeconds <= 0 {
		c.Cooldown.DegradeCooldownMaxSeconds = 7200
	}

	// 未在 config.json 里显式配置 max_in_flight 时取默认 3；
	// 显式配成 0 表示「不限并发」，这是合法配置，不能被默认值覆盖。
	if !c.poolPresent && c.Pool.MaxInFlight == 0 {
		c.Pool.MaxInFlight = 3
	}
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.BreakerCooldownSeconds <= 0 {
		c.Pool.BreakerCooldownSeconds = 1800
	}
	if c.Pool.BreakerCooldownMaxSeconds <= 0 {
		c.Pool.BreakerCooldownMaxSeconds = 21600
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Pool.PreferFreeSite == nil {
		// 默认开：两站价格不同时把流量倾向免费侧，纯收益、无行为风险
		//（只有在「一侧确认免费且另一侧确认收费」时才生效，否则不倾斜）。
		v := true
		c.Pool.PreferFreeSite = &v
	}
	if c.Pool.CreditFloor < 0 {
		// 负值钳 0 而不是报错：它来自用户手写的 config.json，
		// 写错一个负号不该让启动失败（与 wb2api-panel 同口径）。
		c.Pool.CreditFloor = 0
	}

	// 入站读取上限：空值回落默认 300s；"0" 合法（不限制）；负值/不可解析
	// 由 validateServer 记错，启动与保存配置时 fail fast。
	if strings.TrimSpace(c.Server.ReadTimeout) == "" {
		c.Server.ReadTimeout = DefaultServerReadTimeout
	}
	c.validateServer()

	if strings.TrimSpace(c.Prompt.Mode) == "" {
		// 默认 append：既保留客户端项目规范与工具约定，又把整体提示词结构改掉，
		// 消掉大部分「逐字精确匹配」型的误报。passthrough 太容易被拦、custom 会丢客户端规范，
		// 两者分别作为排障档与安全优先档由用户在面板上切。
		c.Prompt.Mode = "append"
	}
	if c.Local.Enabled == nil {
		v := true
		c.Local.Enabled = &v
	}
	if strings.TrimSpace(c.Local.Bin) == "" {
		c.Local.Bin = "wb-local-agent"
	}

	if c.Prompt.Sanitize == nil {
		v := true
		c.Prompt.Sanitize = &v
	}
	if c.Prompt.DegradedRetry == nil {
		v := true
		c.Prompt.DegradedRetry = &v
	}
	if c.Prompt.StrictFirstSystem == nil {
		v := true
		c.Prompt.StrictFirstSystem = &v
	}

	if c.Sticky.Enabled == nil {
		v := true
		c.Sticky.Enabled = &v
	}
	if c.Sticky.TTLSeconds <= 0 {
		c.Sticky.TTLSeconds = 1800
	}
	if c.Sticky.GCIntervalSeconds <= 0 {
		c.Sticky.GCIntervalSeconds = 300
	}

	if c.Schedule.CheckinEnabled == nil {
		v := true
		c.Schedule.CheckinEnabled = &v
	}
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if c.Schedule.KeepaliveEnabled == nil {
		v := true
		c.Schedule.KeepaliveEnabled = &v
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	// 保活阈值：0 是合法且有意义的取值（每天无条件刷新），负值钳 0；
	// 惰性窗口：缺失/非正数回落 24 小时（与 wb-switch 的默认一致）。
	if c.Schedule.KeepaliveDays < 0 {
		c.Schedule.KeepaliveDays = 0
	}
	if c.Schedule.LazyRefreshHours <= 0 {
		c.Schedule.LazyRefreshHours = 24
	}
	if c.Schedule.BalanceRefreshEnabled == nil {
		v := true
		c.Schedule.BalanceRefreshEnabled = &v
	}
	if c.Schedule.BalanceRefreshMinutes <= 0 {
		c.Schedule.BalanceRefreshMinutes = 5
	}

	if c.Schedule.TasksEnabled == nil {
		v := true
		c.Schedule.TasksEnabled = &v
	}
	if len(c.Schedule.TasksHours) == 0 {
		c.Schedule.TasksHours = []int{9}
	}
	if c.Schedule.BlackCatEnabled == nil {
		v := true
		c.Schedule.BlackCatEnabled = &v
	}
	if len(c.Schedule.BlackCatHours) == 0 {
		c.Schedule.BlackCatHours = []int{23}
	}
	if c.Schedule.TravelEnabled == nil {
		v := true
		c.Schedule.TravelEnabled = &v
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if c.Schedule.ActivityEnabled == nil {
		v := true
		c.Schedule.ActivityEnabled = &v
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	// 排除名单的驼峰别名归一进蛇形字段：手写 config.json 惯用驼峰，
	// 面板与接口只认蛇形，两处各读一份迟早漂移。
	if len(c.Schedule.CheckinExcludedAccounts) == 0 && len(c.Schedule.CheckinExcludedAccountsCamel) > 0 {
		c.Schedule.CheckinExcludedAccounts = c.Schedule.CheckinExcludedAccountsCamel
	}
	c.Schedule.CheckinExcludedAccountsCamel = nil
	c.validateScheduleHours()
}

// validateScheduleHours 校验小时值在 0-23 内：非法值直接拒绝启动，避免静默跑错时点。
func (c *Config) validateScheduleHours() {
	check := func(name string, hours []int) error {
		for _, h := range hours {
			if h < 0 || h > 23 {
				return fmt.Errorf("%s 含非法小时值 %d（应为 0-23）", name, h)
			}
		}
		return nil
	}
	c.scheduleErr = nil
	if err := check("schedule.checkin_hours", c.Schedule.CheckinHours); err != nil {
		c.scheduleErr = err
		return
	}
	if err := check("schedule.keepalive_hours", c.Schedule.KeepaliveHours); err != nil {
		c.scheduleErr = err
		return
	}
	if err := check("schedule.tasks_hours", c.Schedule.TasksHours); err != nil {
		c.scheduleErr = err
		return
	}
	if err := check("schedule.blackcat_hours", c.Schedule.BlackCatHours); err != nil {
		c.scheduleErr = err
		return
	}
	if err := check("schedule.travel_hours", c.Schedule.TravelHours); err != nil {
		c.scheduleErr = err
		return
	}
	if err := check("schedule.activity_hours", c.Schedule.ActivityHours); err != nil {
		c.scheduleErr = err
		return
	}
	if err := c.validateCheckinWindow(); err != nil {
		c.scheduleErr = err
	}
}

// validateCheckinWindow 校验签到时间段配置。
//
// **刻意「非法即拒绝启动」而不是「非法即不限制」**（参考实现取后者）：
// 静默把窗口关掉，用户看到的是「我明明设了时间段，却整天都在签」，
// 而没有任何地方告诉他配置被忽略了。宁可启动失败并说明原因。
func (c *Config) validateCheckinWindow() error {
	start := strings.TrimSpace(c.Schedule.CheckinStart)
	end := strings.TrimSpace(c.Schedule.CheckinEnd)
	if start == "" && end == "" {
		return nil // 不限制
	}
	if start == "" || end == "" {
		return fmt.Errorf("schedule.checkin_start / checkin_end 必须同时填写（只填一个无法确定窗口）")
	}
	sm, err := parseClockMinutes(start)
	if err != nil {
		return fmt.Errorf("schedule.checkin_start 格式非法（应为 HH:MM）: %w", err)
	}
	em, err := parseClockMinutes(end)
	if err != nil {
		return fmt.Errorf("schedule.checkin_end 格式非法（应为 HH:MM）: %w", err)
	}
	if sm >= em {
		return fmt.Errorf("schedule.checkin_start(%s) 必须早于 checkin_end(%s)；"+
			"不支持跨午夜（跨夜请改配两个分开的时间段，或直接用 checkin_hours 控制时点）", start, end)
	}
	return nil
}

// parseClockMinutes 把 "HH:MM" 解析成当日分钟数。
func parseClockMinutes(v string) (int, error) {
	h, m, ok := strings.Cut(v, ":")
	if !ok {
		return 0, fmt.Errorf("%q 缺少冒号", v)
	}
	hh, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil {
		return 0, fmt.Errorf("小时不是数字: %q", h)
	}
	mm, err := strconv.Atoi(strings.TrimSpace(m))
	if err != nil {
		return 0, fmt.Errorf("分钟不是数字: %q", m)
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("%q 超出范围（小时 0-23，分钟 0-59）", v)
	}
	return hh*60 + mm, nil
}

// ScheduleErr 返回调度小时校验错误（非 nil 时启动应直接失败）。
// ScheduleErr 返回调度配置的校验结果（由 ApplyDefaults 计算）。
func (c *Config) ScheduleErr() error { return c.scheduleErr }

// validateServer 解析入站读取上限。
//
// 空值已在 ApplyDefaults 里回落默认；"0" 是合法取值（不限制，http.Server 的零值即无超时）；
// 负值**拒绝**（静默钳 0 等于把保护悄悄关掉，而用户以为自己设了保护）与不可解析同样拒绝。
func (c *Config) validateServer() {
	c.serverErr = nil
	d, err := time.ParseDuration(strings.TrimSpace(c.Server.ReadTimeout))
	if err != nil {
		c.serverErr = fmt.Errorf("server.read_timeout: %w", err)
		return
	}
	if d < 0 {
		c.serverErr = fmt.Errorf("server.read_timeout: 负时长 %q 无意义", c.Server.ReadTimeout)
		return
	}
	c.ServerReadTimeout = d
}

// ServerErr 返回入站 HTTP 参数的校验结果（由 ApplyDefaults 计算）。
func (c *Config) ServerErr() error { return c.serverErr }

// IncludeDisabledInTasks 报告「保号类任务是否覆盖已禁用账号」是否打开（默认关闭）。
//
// 缺省 false = 保持「禁用的跳过」既有语义；打开后禁用号照常签到 / 活跃上报 / 保活 /
// 刷新余额，但**仍不参与选号**（pool 的 disabled 过滤与本开关无关）。
func (c *Config) IncludeDisabledInTasks() bool {
	return c.Schedule.IncludeDisabledInTasks != nil && *c.Schedule.IncludeDisabledInTasks
}

// SoftRate 等是给治理层用的时长访问器。
func (c *Config) SoftRate() time.Duration {
	return time.Duration(c.Cooldown.SoftRateSeconds) * time.Second
}

func (c *Config) SoftRateMax() time.Duration {
	return time.Duration(c.Cooldown.SoftRateMaxSeconds) * time.Second
}

func (c *Config) DegradeCooldown() time.Duration {
	return time.Duration(c.Cooldown.DegradeCooldownSeconds) * time.Second
}

func (c *Config) DegradeCooldownMax() time.Duration {
	return time.Duration(c.Cooldown.DegradeCooldownMaxSeconds) * time.Second
}

func (c *Config) BreakerCooldown() time.Duration {
	return time.Duration(c.Pool.BreakerCooldownSeconds) * time.Second
}

func (c *Config) BreakerCooldownMax() time.Duration {
	return time.Duration(c.Pool.BreakerCooldownMaxSeconds) * time.Second
}

func (c *Config) StickyTTL() time.Duration {
	return time.Duration(c.Sticky.TTLSeconds) * time.Second
}

func (c *Config) StickyGCInterval() time.Duration {
	return time.Duration(c.Sticky.GCIntervalSeconds) * time.Second
}

func (c *Config) StickyEnabled() bool {
	return c.Sticky.Enabled == nil || *c.Sticky.Enabled
}

// PreferFreeSiteEnabled 报告是否开启「免费站点优先」（slice 8）。
func (c *Config) PreferFreeSiteEnabled() bool {
	return c.Pool.PreferFreeSite == nil || *c.Pool.PreferFreeSite
}

// TaskDesktopEventsEnabled 报告是否允许上报桌面客户端指纹事件链。
//
// 默认 **false**：该通路按参考实现的实测样本复刻，形状确定但上游是否接受
// 无法离线验证，且伪造客户端事件有被风控识别的风险（代价由账号承担）。
// 显式打开才算数，见 TasksSection 的注释。
func (c *Config) TaskDesktopEventsEnabled() bool {
	return c.Tasks.DesktopEventsEnabled != nil && *c.Tasks.DesktopEventsEnabled
}

// CreditFloor 返回积分保底阈值（0 = 关闭）。
//
// 这里做一次钳制而不是只在解析时做：面板保存补丁走的是另一条路径
// （SavePatch → applyTo），那条路径不会经过解析期的归一化。
// 两处都钳，才不会出现「配置文件里是 0、内存里是 -5」这种不一致。
func (c *Config) CreditFloor() int64 {
	if c.Pool.CreditFloor <= 0 {
		return 0
	}
	return c.Pool.CreditFloor
}

func (c *Config) CheckinEnabled() bool {
	return c.Schedule.CheckinEnabled == nil || *c.Schedule.CheckinEnabled
}

func (c *Config) KeepaliveEnabled() bool {
	return c.Schedule.KeepaliveEnabled == nil || *c.Schedule.KeepaliveEnabled
}

// KeepaliveDays 返回排程保活阈值（天）；0 = 无条件刷新（负值按 0 处理）。
func (c *Config) KeepaliveDays() int {
	if c.Schedule.KeepaliveDays < 0 {
		return 0
	}
	return c.Schedule.KeepaliveDays
}

// LazyRefreshHours 返回排程保活的惰性窗口（小时）；非正数按默认 24 处理。
func (c *Config) LazyRefreshHours() int {
	if c.Schedule.LazyRefreshHours <= 0 {
		return 24
	}
	return c.Schedule.LazyRefreshHours
}

// PromptMode / PromptSanitize / PromptDegradedRetry / PromptStrictFirst 是提示词参数访问器。
func (c *Config) PromptMode() string { return c.Prompt.Mode }

func (c *Config) PromptSanitize() bool {
	return c.Prompt.Sanitize == nil || *c.Prompt.Sanitize
}

func (c *Config) PromptDegradedRetry() bool {
	return c.Prompt.DegradedRetry == nil || *c.Prompt.DegradedRetry
}

func (c *Config) PromptStrictFirstSystem() bool {
	return c.Prompt.StrictFirstSystem == nil || *c.Prompt.StrictFirstSystem
}

func (c *Config) LocalAgentEnabled() bool {
	return c.Local.Enabled == nil || *c.Local.Enabled
}

func (c *Config) TasksEnabled() bool {
	return c.Schedule.TasksEnabled == nil || *c.Schedule.TasksEnabled
}

func (c *Config) BlackCatEnabled() bool {
	return c.Schedule.BlackCatEnabled == nil || *c.Schedule.BlackCatEnabled
}

func (c *Config) BalanceRefreshEnabled() bool {
	return c.Schedule.BalanceRefreshEnabled == nil || *c.Schedule.BalanceRefreshEnabled
}

// TravelEnabled 报告旅行巡检排程是否启用（切片 18）。
func (c *Config) TravelEnabled() bool {
	return c.Schedule.TravelEnabled == nil || *c.Schedule.TravelEnabled
}

// ActivityEnabled 报告活跃上报排程是否启用（切片 18）。
func (c *Config) ActivityEnabled() bool {
	return c.Schedule.ActivityEnabled == nil || *c.Schedule.ActivityEnabled
}

// CheckinWindow 返回签到允许的时间段（当日分钟数，**左闭右开**）。
// ok=false 表示不限制（两个字段都为空，或配置非法 —— 非法情形在启动时已拒绝）。
func (c *Config) CheckinWindow() (start, end int, ok bool) {
	s := strings.TrimSpace(c.Schedule.CheckinStart)
	e := strings.TrimSpace(c.Schedule.CheckinEnd)
	if s == "" || e == "" {
		return 0, 0, false
	}
	sm, err1 := parseClockMinutes(s)
	em, err2 := parseClockMinutes(e)
	if err1 != nil || err2 != nil || sm >= em {
		return 0, 0, false
	}
	return sm, em, true
}

// InCheckinWindow 报告 now 是否落在签到时间段内。
//
// 窗口未生效时恒为 true（= 不限制）。`end` 是**开区间**：
// 配 09:00-11:00 时 11:00 整点已经**不在**窗口内 —— 这是「到点就停」的直觉，
// 也是参考实现的口径，改成闭区间会让最后一分钟多签一轮。
func (c *Config) InCheckinWindow(now time.Time) bool {
	start, end, ok := c.CheckinWindow()
	if !ok {
		return true
	}
	m := now.Hour()*60 + now.Minute()
	return m >= start && m < end
}

// CheckinExcludedAccountIDs 返回归一化后的自动签到排除名单。
//
// 归一化：去首尾空白、丢弃空串、按首次出现去重（手写配置里可能有重复或空项）。
// 返回的是**副本**，调用方可以随便排序/截断。
func (c *Config) CheckinExcludedAccountIDs() []string {
	raw := c.Schedule.CheckinExcludedAccounts
	if len(raw) == 0 {
		// 兜底：直接构造的 Config（单测 / 旧调用路径）可能只填了驼峰字段。
		raw = c.Schedule.CheckinExcludedAccountsCamel
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// IsCheckinExcluded 报告某账号是否被排除在自动签到之外（手动签到不受影响）。
func (c *Config) IsCheckinExcluded(accountID string) bool {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false
	}
	for _, id := range c.CheckinExcludedAccountIDs() {
		if id == accountID {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// 配置读写（面板在线编辑）
// -----------------------------------------------------------------------------

// ProbesSection 汇总探测节奏（供面板展示，只读）。

type Patch struct {
	Tasks    *TasksSection    `json:"tasks"`
	Cooldown *CooldownSection `json:"cooldown"`
	Pool     *PoolSection     `json:"pool"`
	Sticky   *StickySection   `json:"session_sticky"`
	Schedule *ScheduleSection `json:"schedule"`
	Prompt   *PromptSection   `json:"prompt"`
	Local    *LocalSection    `json:"local"`
	Server   *ServerSection   `json:"server"`
	Upstream *struct {
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		IdleTimeoutSeconds   int `json:"idle_timeout_seconds"`
	} `json:"upstream"`
	Models *struct {
		Blocklist []string `json:"blocklist"`
		Allowlist []string `json:"allowlist"`
	} `json:"models"`
	Debug *struct {
		Enabled *bool `json:"enabled"`
	} `json:"debug"`
}

// flattenPresence 把补丁的原始 JSON 摊平成「出现过哪些叶子键」的集合，
// 键形如 "pool.max_in_flight"、"schedule.tasks_hours"。
//
// 为什么需要它：结构体上的零值分不清「用户显式提交了 0」与「用户没提交这个字段」。
// 而 pool.max_in_flight = 0（不限并发）、cooldown 的若干秒数、schedule 的分钟数
// 都是合法取值，只看零值必然把「提交 0」误判成「没提交」。
func flattenPresence(prefix string, v any, out map[string]bool) {
	if m, ok := v.(map[string]any); ok {
		for k, sub := range m {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flattenPresence(key, sub, out)
		}
		return
	}
	if prefix != "" {
		out[prefix] = true
	}
}

// applyPresent 按 present 集合把 src 中**提交过的**字段覆盖到 dst 上。
//
// 用反射而不是手写逐字段赋值，是因为手写版本已经漏过一次：切片 5 给调度段新增了
// 四个字段（tasks_enabled / tasks_hours / blackcat_enabled / blackcat_hours），
// 但忘了在 applyTo 里接线，结果这四个设置在面板上能改、能写进 config.json，
// 却永远不会进内存配置 —— 不编译报错、不抛异常，只是静默失效。
// 反射版本会自动覆盖新增字段，从根上消掉这一类 bug。
//
// 规则：
//   - 指针字段为 nil → 视为未提交，保留原值
//   - 嵌套结构体 → 递归
//   - 其余（数值 / 字符串 / 切片 / 布尔）→ 只在 present 命中时覆盖
//
// 有意不处理那些「语义特殊」的段（prompt 的可清空文本、local 的只填空覆盖、
// models 的名单整体覆盖），它们继续用显式代码写在下面。
func applyPresent(dst, src any, prefix string, present map[string]bool) {
	dv := reflect.ValueOf(dst)
	sv := reflect.ValueOf(src)
	if dv.Kind() != reflect.Pointer || sv.Kind() != reflect.Pointer || dv.IsNil() || sv.IsNil() {
		return
	}
	dv, sv = dv.Elem(), sv.Elem()
	if dv.Kind() != reflect.Struct || sv.Kind() != reflect.Struct {
		return
	}
	t := dv.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // 未导出字段无法 Set
			continue
		}
		name := jsonFieldName(f)
		if name == "" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		sf, df := sv.Field(i), dv.Field(i)
		if !df.CanSet() {
			continue
		}
		switch {
		case sf.Kind() == reflect.Pointer:
			// nil = 未提交；非 nil 才覆盖（并保留指针语义：显式 false 也能表达）
			if !sf.IsNil() {
				df.Set(sf)
			}
		case sf.Kind() == reflect.Struct:
			applyPresent(df.Addr().Interface(), sf.Addr().Interface(), key, present)
		default:
			if present[key] {
				df.Set(sf)
			}
		}
	}
}

// jsonFieldName 取字段的 json 名（去掉 omitempty 等选项）；无 tag 或标记为 "-" 时返回空串。
func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if idx := strings.Index(tag, ","); idx >= 0 {
		tag = tag[:idx]
	}
	if tag == "" {
		return ""
	}
	return tag
}

// applyTo 把补丁合并进当前配置。present 是补丁里真实提交过的叶子键集合。
func (p *Patch) applyTo(c *Config, present map[string]bool) {
	if p.Cooldown != nil {
		applyPresent(&c.Cooldown, p.Cooldown, "cooldown", present)
	}
	if p.Pool != nil {
		applyPresent(&c.Pool, p.Pool, "pool", present)
	}
	if p.Sticky != nil {
		applyPresent(&c.Sticky, p.Sticky, "session_sticky", present)
	}
	if p.Prompt != nil {
		// 逐字段覆盖，避免把未提交的字段清成零值
		if strings.TrimSpace(p.Prompt.Mode) != "" {
			c.Prompt.Mode = p.Prompt.Mode
		}
		// Text / File 允许显式清空（用户删掉自定义提示词回到内置默认），
		// 所以这里不能用「非空才覆盖」的判断：提交了就整体覆盖。
		c.Prompt.Text = p.Prompt.Text
		c.Prompt.File = p.Prompt.File
		if p.Prompt.Sanitize != nil {
			c.Prompt.Sanitize = p.Prompt.Sanitize
		}
		if p.Prompt.DegradedRetry != nil {
			c.Prompt.DegradedRetry = p.Prompt.DegradedRetry
		}
		if p.Prompt.StrictFirstSystem != nil {
			c.Prompt.StrictFirstSystem = p.Prompt.StrictFirstSystem
		}
	}
	if p.Local != nil {
		if p.Local.Enabled != nil {
			c.Local.Enabled = p.Local.Enabled
		}
		if strings.TrimSpace(p.Local.Bin) != "" {
			c.Local.Bin = p.Local.Bin
		}
		c.Local.DataDir = p.Local.DataDir
	}
	if p.Models != nil {
		// 黑白名单按整体覆盖：空数组表示清空该名单（而不是「未提交」），
		// 因为面板提交的是名单全量而非增量。
		c.ModelBlock = p.Models.Blocklist
		c.ModelAllow = p.Models.Allowlist
	}
	if p.Schedule != nil {
		// 逐字段覆盖（靠 present 判断是否提交），不要整段覆盖：
		// 整段覆盖会把未提交的时点数组清成 nil，等于「关掉所有定时任务」。
		applyPresent(&c.Schedule, p.Schedule, "schedule", present)
		// 排除名单的两种拼写归一：提交后只保留蛇形字段，
		// 否则旧别名会在下次读取时把刚清空的名单“复活”。
		if present["schedule.checkin_excluded_accounts"] {
			c.Schedule.CheckinExcludedAccountsCamel = nil
		}
		if present["schedule.checkinExcludedAccounts"] {
			c.Schedule.CheckinExcludedAccounts = c.Schedule.CheckinExcludedAccountsCamel
			c.Schedule.CheckinExcludedAccountsCamel = nil
		}
	}
	if p.Tasks != nil {
		applyPresent(&c.Tasks, p.Tasks, "tasks", present)
	}
	if p.Server != nil {
		applyPresent(&c.Server, p.Server, "server", present)
	}
}

// hotFields 是需要重启才能生效的配置前缀。
// 治理与调度参数都可热生效；上游超时属于装配期字段（Transport 在启动时建好），
// 因此列在这里，保存后提示用户重启。
var hotFields = []string{"cooldown", "pool", "session_sticky", "schedule", "models", "prompt", "local", "tasks"}

// SavePatch 把补丁写入 config.json 并返回 (已热生效字段, 需重启字段)。
//
// 入参是**客户端提交的原始 JSON**，而不是解好的结构体 —— 这一点是刻意的：
// 结构体分不清「字段没提交」与「提交了零值」。把 `{"pool":{"prefer_free_site":false}}`
// 解成结构体再重新序列化，未提交的 max_in_flight 会变成 0 一起出现在 JSON 里，
// 于是「只改了一个开关」看起来像「把所有并发参数都改成 0」。
// 传原始字节可以同时拿到两份视图：结构体（取值）与 map（判断提交了哪些键）。
//
// 写入方式：读原文 → 只合并提交过的键 → 原子替换，保留用户手写的未知键。
func (c *Config) SavePatch(body []byte) ([]string, []string, error) {
	raw := map[string]any{}
	if data, err := os.ReadFile(c.ConfigFile); err == nil {
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, nil, errors.New("config.json 解析失败: " + err.Error())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, errors.New("读取 config.json 失败: " + err.Error())
	}

	var p Patch
	if len(body) > 0 {
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, nil, errors.New("请求体解析失败: " + err.Error())
		}
	}
	var submitted map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &submitted); err != nil {
			return nil, nil, errors.New("请求体解析失败: " + err.Error())
		}
	}

	// 只保留**真正提交**的段：客户端显式传 null 时（指针字段未提交的序列化形态）
	// 直接写盘会污染 config.json（出现 "pool": null），也会把「没改」误报成「已改」。
	patchMap := map[string]any{}
	for k, v := range submitted {
		if v != nil {
			patchMap[k] = v
		}
	}

	// 记录真实提交过的叶子键，供 applyTo 逐字段覆盖使用。
	present := map[string]bool{}
	for k, v := range patchMap {
		flattenPresence(k, v, present)
	}

	deepMerge(raw, patchMap)

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(c.ConfigFile, data); err != nil {
		return nil, nil, err
	}

	// 同步到内存并重算派生值
	p.applyTo(c, present)
	// poolPresent 参与默认值判定（max_in_flight 的 0 是合法值，只有「从没配置过」
	// 才该被默认值顶掉）。这里刚把 raw 写回磁盘，所以用 raw 重新判定最准确；
	// 不更新的话，本次提交显式写的 0 会在下面的 ApplyDefaults 里被立刻改成默认 3。
	c.poolPresent = hasNestedKey(raw, "pool", "max_in_flight")
	c.ApplyDefaults()
	if err := c.ScheduleErr(); err != nil {
		return nil, nil, err
	}
	if err := c.ServerErr(); err != nil {
		return nil, nil, err
	}
	c.syncUpstreamFromPatch(&p)

	applied := []string{}
	needRestart := []string{}
	for _, k := range hotFields {
		if _, ok := patchMap[k]; ok {
			applied = append(applied, k)
		}
	}
	if _, ok := patchMap["upstream"]; ok {
		needRestart = append(needRestart, "upstream")
	}
	if _, ok := patchMap["server"]; ok {
		// 入站读取上限属装配期字段：http.Server 只在启动时构造一次。
		needRestart = append(needRestart, "server.read_timeout")
	}
	if _, ok := patchMap["debug"]; ok {
		needRestart = append(needRestart, "debug")
	}
	return applied, needRestart, nil
}

// syncUpstreamFromPatch 处理上游超时补丁：只更新内存值供展示，实际生效需重启。
func (c *Config) syncUpstreamFromPatch(p *Patch) {
	if p.Upstream == nil {
		return
	}
	if p.Upstream.HeaderTimeoutSeconds > 0 {
		c.HeaderTimeout = time.Duration(p.Upstream.HeaderTimeoutSeconds) * time.Second
	}
	if p.Upstream.IdleTimeoutSeconds > 0 {
		c.IdleTimeout = time.Duration(p.Upstream.IdleTimeoutSeconds) * time.Second
	}
}

// hasNestedKey 判断原始 JSON 里是否存在 section.key（用于区分「未配置」与「显式零值」）。
func hasNestedKey(raw map[string]any, section, key string) bool {
	if raw == nil {
		return false
	}
	sec, ok := raw[section].(map[string]any)
	if !ok {
		return false
	}
	_, ok = sec[key]
	return ok
}

// deepMerge 把 src 深度合并进 dst（同键为对象时递归，否则覆盖）。
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		sv, isMap := v.(map[string]any)
		if !isMap {
			dst[k] = v
			continue
		}
		if dv, ok := dst[k].(map[string]any); ok {
			deepMerge(dv, sv)
			continue
		}
		dst[k] = sv
	}
}

// writeFileAtomic 原子写文件（tmp + rename）。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("替换配置文件失败: %w", err)
	}
	return nil
}

// ModelsNPMEnabled 是否启用 npm 静态目录兜底（默认启用）。
func (c *Config) ModelsNPMEnabled() bool {
	return c.ModelsNPM == nil || *c.ModelsNPM
}

// ProbeEnabled 是否启用价格实测探测（默认启用）。
func (c *Config) ProbeEnabled() bool {
	return c.ModelsProbe == nil || *c.ModelsProbe
}

// HotFields 返回可热生效的配置段名（供文档与面板说明）。
func HotFields() []string { return append([]string(nil), hotFields...) }
