package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestConfig 构造一个把 config.json 放在临时目录里的配置。
func newTestConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	c := &Config{
		ConfigFile: filepath.Join(dir, "config.json"),
		WorkDir:    dir,
	}
	c.ApplyDefaults()
	return c
}

// save 以真实请求形态（原始 JSON 文本）提交补丁 —— 与面板走同一条路径。
func save(t *testing.T, c *Config, body string) (applied, needRestart []string) {
	t.Helper()
	applied, needRestart, err := c.SavePatch([]byte(body))
	if err != nil {
		t.Fatalf("保存补丁失败: %v", err)
	}
	return applied, needRestart
}

// TestPartialPoolPatchKeepsOtherFields 验证局部提交不会把同段其它字段清零。
//
// 这条对应一个真实缺陷：早期 applyTo 对 pool / cooldown 是整段覆盖，
// 于是「只把 prefer_free_site 改成 false」会把 max_in_flight、
// 熔断阈值等全部写成 0 —— 而 0 在 max_in_flight 上表示「不限并发」，
// 看起来配置还在，行为却已经变了。
func TestPartialPoolPatchKeepsOtherFields(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"pool":{"max_in_flight":7,"breaker_threshold":5,"idle_weight_max":3}}`)

	save(t, c, `{"pool":{"prefer_free_site":false}}`)

	if c.Pool.MaxInFlight != 7 {
		t.Fatalf("局部提交后 max_in_flight 被清零，期望 7 实际 %d", c.Pool.MaxInFlight)
	}
	if c.Pool.BreakerThreshold != 5 {
		t.Fatalf("局部提交后 breaker_threshold 被清零，期望 5 实际 %d", c.Pool.BreakerThreshold)
	}
	if c.Pool.IdleWeightMax != 3 {
		t.Fatalf("局部提交后 idle_weight_max 被清零，期望 3 实际 %v", c.Pool.IdleWeightMax)
	}
	if c.PreferFreeSiteEnabled() {
		t.Fatal("prefer_free_site 应已被关闭")
	}
	// 落盘内容同样不能出现被清零的字段
	raw := readConfigJSON(t, c)
	pool, _ := raw["pool"].(map[string]any)
	if pool["max_in_flight"] != float64(7) {
		t.Fatalf("config.json 里的 max_in_flight 被覆盖，实际 %v", pool["max_in_flight"])
	}
}

// TestExplicitZeroIsAccepted 验证「显式提交 0」能生效。
//
// max_in_flight = 0 是合法取值（不限并发），任何「<=0 就跳过」的写法都会
// 让用户永远关不掉并发限制。
func TestExplicitZeroIsAccepted(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"pool":{"max_in_flight":4}}`)
	save(t, c, `{"pool":{"max_in_flight":0}}`)

	if c.Pool.MaxInFlight != 0 {
		t.Fatalf("显式提交 0 应当生效（不限并发），实际 %d", c.Pool.MaxInFlight)
	}
}

// TestScheduleTasksFieldsAreApplied 验证切片 5 新增的排程字段真的会进内存配置。
//
// 这条对应的缺陷更隐蔽：字段在结构体里、在 config.json 里、在面板上都有，
// 唯独 applyTo 没接线 —— 用户改了保存成功、重启后设置还原，不报任何错。
func TestScheduleTasksFieldsAreApplied(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"schedule":{"tasks_hours":[6,18],"blackcat_enabled":false,"blackcat_hours":[22]}}`)

	if len(c.Schedule.TasksHours) != 2 || c.Schedule.TasksHours[0] != 6 || c.Schedule.TasksHours[1] != 18 {
		t.Fatalf("tasks_hours 未生效，实际 %v", c.Schedule.TasksHours)
	}
	if c.Schedule.TasksEnabled == nil || !*c.Schedule.TasksEnabled {
		t.Fatal("未提交 tasks_enabled 时应保留默认值 true")
	}
	if c.Schedule.BlackCatEnabled == nil || *c.Schedule.BlackCatEnabled {
		t.Fatal("blackcat_enabled=false 未生效")
	}
	if len(c.Schedule.BlackCatHours) != 1 || c.Schedule.BlackCatHours[0] != 22 {
		t.Fatalf("blackcat_hours 未生效，实际 %v", c.Schedule.BlackCatHours)
	}
}

// TestPartialSchedulePatchKeepsOtherSlots 验证只改一个时点不会清掉其它时点数组。
func TestPartialSchedulePatchKeepsOtherSlots(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`)
	save(t, c, `{"schedule":{"balance_refresh_minutes":10}}`)

	if len(c.Schedule.CheckinHours) != 2 {
		t.Fatalf("签到时点被清空，实际 %v", c.Schedule.CheckinHours)
	}
	if len(c.Schedule.KeepaliveHours) != 1 {
		t.Fatalf("保活时点被清空，实际 %v", c.Schedule.KeepaliveHours)
	}
	if c.Schedule.BalanceRefreshMinutes != 10 {
		t.Fatalf("balance_refresh_minutes 未生效，实际 %d", c.Schedule.BalanceRefreshMinutes)
	}
}

// TestUnsubmittedSectionsUntouched 验证没提交的段完全不受影响。
func TestUnsubmittedSectionsUntouched(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"cooldown":{"soft_rate_seconds":900},"session_sticky":{"ttl_seconds":600}}`)

	save(t, c, `{"pool":{"prefer_free_site":true}}`)

	if c.Cooldown.SoftRateSeconds != 900 {
		t.Fatalf("未提交的 cooldown 段被改动，实际 %d", c.Cooldown.SoftRateSeconds)
	}
	if c.Sticky.TTLSeconds != 600 {
		t.Fatalf("未提交的 session_sticky 段被改动，实际 %d", c.Sticky.TTLSeconds)
	}
}

// TestPatchDoesNotPolluteFileWithNull 验证未提交的段不会以 null 落盘
// （null 会让下次加载把它当成显式配置，从而绕过默认值）。
func TestPatchDoesNotPolluteFileWithNull(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"pool":{"prefer_free_site":false}}`)

	raw := readConfigJSON(t, c)
	for key, v := range raw {
		if v == nil {
			t.Fatalf("config.json 出现 null 段: %s", key)
		}
	}
}

// TestPromptOutboundSwitchesRoundTrip 守住三个出站改写开关的热改往返。
//
// 为什么值得单测：prompt 段在 applyTo 里是**显式手写**的（不走反射），新增字段
// 若忘了接线，表现是「面板能改、config.json 写进去了、内存配置却不变」—— 不编译
// 报错、不抛异常，只是静默失效。同类事故在本项目发生过（切片 5 的四个排程字段）。
func TestPromptOutboundSwitchesRoundTrip(t *testing.T) {
	c := newTestConfig(t)
	// 三个开关默认开（未提交时）。
	if !c.PromptCatalogMaxTokens() || !c.PromptDSMLRepair() || !c.PromptDSMLWeakMatch() {
		t.Fatalf("三个开关默认应为开：catalog=%t dsml=%t weak=%t",
			c.PromptCatalogMaxTokens(), c.PromptDSMLRepair(), c.PromptDSMLWeakMatch())
	}

	save(t, c, `{"prompt":{"catalog_max_tokens":false,"dsml_repair":false,"dsml_weak_match":false}}`)
	if c.PromptCatalogMaxTokens() {
		t.Fatal("catalog_max_tokens 显式关掉后仍为开 —— 补丁没接进内存配置")
	}
	if c.PromptDSMLRepair() {
		t.Fatal("dsml_repair 显式关掉后仍为开 —— 补丁没接进内存配置")
	}
	if c.PromptDSMLWeakMatch() {
		t.Fatal("dsml_weak_match 显式关掉后仍为开 —— 补丁没接进内存配置")
	}

	// 只提交其中一个：另外两个不能被重置（指针三态的意义）。
	save(t, c, `{"prompt":{"dsml_repair":true}}`)
	if !c.PromptDSMLRepair() {
		t.Fatal("dsml_repair 应被重新打开")
	}
	if c.PromptCatalogMaxTokens() || c.PromptDSMLWeakMatch() {
		t.Fatal("只提交 dsml_repair 时，另外两个开关不应被改动")
	}
}

func readConfigJSON(t *testing.T, c *Config) map[string]any {
	t.Helper()
	data, err := os.ReadFile(c.ConfigFile)
	if err != nil {
		t.Fatalf("读取 config.json 失败: %v", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("解析 config.json 失败: %v", err)
	}
	return out
}

// -----------------------------------------------------------------------------
// 切片 18：旅行巡检 / 活跃上报两类排程
// -----------------------------------------------------------------------------

// 默认值必须**开箱即用**：两个开关默认开、时点回落到参考实现的默认值。
//
// 为什么时点是 travel=[9,21]、activity=[10]：旅行一轮要「出发 + 到站领奖」
// 两次推进才闭环，一个时点只能走一半；活跃上报是每天一次即可的前置条件。
func TestTravelAndActivityScheduleDefaults(t *testing.T) {
	c := newTestConfig(t)

	if !c.TravelEnabled() {
		t.Fatal("travel 默认应启用")
	}
	if got := c.Schedule.TravelHours; len(got) != 2 || got[0] != 9 || got[1] != 21 {
		t.Fatalf("travel_hours 默认应为 [9 21]，实际 %v", got)
	}
	if !c.ActivityEnabled() {
		t.Fatal("activity 默认应启用")
	}
	if got := c.Schedule.ActivityHours; len(got) != 1 || got[0] != 10 {
		t.Fatalf("activity_hours 默认应为 [10]，实际 %v", got)
	}
}

// 非法小时必须被拒：静默跑错时点比报错更难查。
func TestTravelScheduleRejectsIllegalHour(t *testing.T) {
	c := newTestConfig(t)
	if _, _, err := c.SavePatch([]byte(`{"schedule":{"travel_hours":[25]}}`)); err == nil {
		t.Fatal("travel_hours=25 应被拒绝")
	}
	c2 := newTestConfig(t)
	if _, _, err := c2.SavePatch([]byte(`{"schedule":{"activity_hours":[-1]}}`)); err == nil {
		t.Fatal("activity_hours=-1 应被拒绝")
	}
}

// 局部提交 travel 不能把 activity（以及其它排程）清成 nil
// —— 那等于「静默关掉所有定时任务」。
func TestPartialTravelPatchKeepsOtherSchedules(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"schedule":{"travel_hours":[3,15]}}`)

	if got := c.Schedule.TravelHours; len(got) != 2 || got[0] != 3 || got[1] != 15 {
		t.Fatalf("travel_hours 未生效，实际 %v", got)
	}
	if len(c.Schedule.CheckinHours) == 0 || len(c.Schedule.ActivityHours) == 0 || len(c.Schedule.BlackCatHours) == 0 {
		t.Fatalf("局部提交把其它排程清空了: checkin=%v activity=%v blackcat=%v",
			c.Schedule.CheckinHours, c.Schedule.ActivityHours, c.Schedule.BlackCatHours)
	}
}

// 关掉 travel 开关必须能表达出来（指针语义），且不影响 activity。
func TestDisableTravelKeepsActivity(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"schedule":{"travel_enabled":false}}`)

	if c.TravelEnabled() {
		t.Fatal("travel 应已关闭")
	}
	if !c.ActivityEnabled() {
		t.Fatal("只关 travel 不该连带关掉 activity")
	}
}

// -----------------------------------------------------------------------------
// 切片 18：积分保底（pool.credit_floor）
// -----------------------------------------------------------------------------

// 键缺席时默认 0 = 关闭，行为与引入保底之前完全一致。
func TestCreditFloorDefaultsToZero(t *testing.T) {
	c := newTestConfig(t)
	if got := c.CreditFloor(); got != 0 {
		t.Fatalf("默认应为 0（关闭），实际 %d", got)
	}
}

// 显式配置生效。
func TestCreditFloorParsedFromPatch(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"pool":{"credit_floor":100}}`)
	if got := c.CreditFloor(); got != 100 {
		t.Fatalf("应为 100，实际 %d", got)
	}
}

// 负值钳 0 而不是报错：它来自用户手写的 config.json，写错一个负号不该让启动失败。
func TestCreditFloorNegativeClamped(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"pool":{"credit_floor":-5}}`)
	if got := c.CreditFloor(); got != 0 {
		t.Fatalf("负值应钳成 0，实际 %d", got)
	}
}

// 局部提交 credit_floor 不能把 pool 段其它字段清零。
func TestPartialCreditFloorPatchKeepsOtherPoolFields(t *testing.T) {
	c := newTestConfig(t)
	before := c.Pool.MaxInFlight
	save(t, c, `{"pool":{"credit_floor":50}}`)

	if c.CreditFloor() != 50 {
		t.Fatalf("credit_floor 未生效，实际 %d", c.CreditFloor())
	}
	if c.Pool.MaxInFlight != before {
		t.Fatalf("局部提交把 max_in_flight 改掉了: %d → %d", before, c.Pool.MaxInFlight)
	}
}

// -----------------------------------------------------------------------------
// 切片 18：签到时间段（respect_window）
// -----------------------------------------------------------------------------

// 两个字段都留空 = 不限制（默认，行为与引入前一致）。
func TestCheckinWindowEmptyMeansNoLimit(t *testing.T) {
	c := newTestConfig(t)
	if _, _, ok := c.CheckinWindow(); ok {
		t.Fatal("留空时窗口不应生效")
	}
	// 任何时刻都在窗口内（= 不限制）。
	for _, h := range []int{0, 6, 12, 23} {
		at := time.Date(2026, 10, 4, h, 30, 0, 0, time.Local)
		if !c.InCheckinWindow(at) {
			t.Fatalf("%02d:30 应视为窗口内（未配置即不限制）", h)
		}
	}
}

// 合法窗口生效，且 **end 是开区间**。
func TestCheckinWindowEndExclusive(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"schedule":{"checkin_start":"09:00","checkin_end":"11:00"}}`)

	start, end, ok := c.CheckinWindow()
	if !ok || start != 9*60 || end != 11*60 {
		t.Fatalf("窗口解析错误: start=%d end=%d ok=%v", start, end, ok)
	}

	cases := []struct {
		hm   [2]int
		want bool
	}{
		{[2]int{8, 59}, false},
		{[2]int{9, 0}, true}, // 左闭
		{[2]int{10, 59}, true},
		{[2]int{11, 0}, false}, // 右开：到点即停
		{[2]int{11, 1}, false},
		{[2]int{23, 0}, false},
	}
	for _, tc := range cases {
		at := time.Date(2026, 10, 4, tc.hm[0], tc.hm[1], 0, 0, time.Local)
		if got := c.InCheckinWindow(at); got != tc.want {
			t.Fatalf("%02d:%02d 期望 %v，实际 %v", tc.hm[0], tc.hm[1], tc.want, got)
		}
	}
}

// 只填一个、格式非法、start >= end 一律**拒绝启动**。
//
// 刻意不采用「非法即静默不限制」：那会让用户看到「我明明设了时间段，却整天在签」，
// 而没有任何地方告诉他配置被忽略了 —— 这正是本项目一贯要避免的「改了没生效」。
func TestCheckinWindowInvalidConfigRejected(t *testing.T) {
	bad := []string{
		`{"schedule":{"checkin_start":"09:00"}}`,                       // 只填一个
		`{"schedule":{"checkin_end":"11:00"}}`,                         // 只填一个
		`{"schedule":{"checkin_start":"abc","checkin_end":"11:00"}}`,   // 格式非法
		`{"schedule":{"checkin_start":"09:00","checkin_end":"9-11"}}`,  // 格式非法
		`{"schedule":{"checkin_start":"25:00","checkin_end":"26:00"}}`, // 越界
		`{"schedule":{"checkin_start":"11:00","checkin_end":"09:00"}}`, // 逆序
		`{"schedule":{"checkin_start":"09:00","checkin_end":"09:00"}}`, // 空窗口
		`{"schedule":{"checkin_start":"22:00","checkin_end":"02:00"}}`, // 跨午夜不支持
	}
	for _, body := range bad {
		c := newTestConfig(t)
		if _, _, err := c.SavePatch([]byte(body)); err == nil {
			t.Fatalf("应拒绝非法窗口配置: %s", body)
		}
	}
}

// 清空两个字段可以回到「不限制」（面板要能取消这个约束）。
func TestCheckinWindowCanBeCleared(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"schedule":{"checkin_start":"09:00","checkin_end":"11:00"}}`)
	if _, _, ok := c.CheckinWindow(); !ok {
		t.Fatal("前置条件错误：窗口应已生效")
	}
	save(t, c, `{"schedule":{"checkin_start":"","checkin_end":""}}`)
	if _, _, ok := c.CheckinWindow(); ok {
		t.Fatal("清空后应回到不限制")
	}
}

// -----------------------------------------------------------------------------
// 切片 18：客户端事件链开关（tasks.desktop_events_enabled）
// -----------------------------------------------------------------------------

// 默认必须是**开启**（对齐上游：上游没有这个开关，全部任务开箱可用）。
// 默认关的后果是任务中心形同虚设——12+ 条任务永远落进「需客户端」不执行。
// 开关保留是为了让「不想要伪造客户端行为」的人能显式关掉。
func TestDesktopEventsDefaultOn(t *testing.T) {
	c := newTestConfig(t)
	if !c.TaskDesktopEventsEnabled() {
		t.Fatal("默认必须开启（对齐上游）")
	}
}

// 显式打开 / 关闭都能表达（指针语义：关掉 ≠ 未提交）。
func TestDesktopEventsToggle(t *testing.T) {
	on := newTestConfig(t)
	save(t, on, `{"tasks":{"desktop_events_enabled":true}}`)
	if !on.TaskDesktopEventsEnabled() {
		t.Fatal("显式打开未生效")
	}

	off := newTestConfig(t)
	save(t, off, `{"tasks":{"desktop_events_enabled":false}}`)
	if off.TaskDesktopEventsEnabled() {
		t.Fatal("显式关闭未生效")
	}
}

// tasks 段是热生效字段：保存后立即影响 actionFor 的过滤，不需重启。
func TestTasksSectionIsHotReloadable(t *testing.T) {
	found := false
	for _, f := range HotFields() {
		if f == "tasks" {
			found = true
		}
	}
	if !found {
		t.Fatal("tasks 应在热生效字段列表里")
	}
}

// -----------------------------------------------------------------------------
// 批次 4：自动签到排除名单（schedule.checkin_excluded_accounts）
// -----------------------------------------------------------------------------

// 手工写的驼峰 config.json 必须被读到（本项目一直兼容两种拼写），
// 面板补丁写蛇形；两份都异常时按并集去重。
func TestCheckinExcludedAccountsCamelAndSnake(t *testing.T) {
	// 1) 手写 config.json 的驼峰写法（走完整 Load 路径）。
	dir := t.TempDir()
	raw := `{"schedule":{"checkinExcludedAccounts":[" a.json ","","b.json","a.json"]}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(&Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	ids := loaded.CheckinExcludedAccountIDs()
	if len(ids) != 2 || ids[0] != "a.json" || ids[1] != "b.json" {
		t.Fatalf("驼峰名单应去空白/去重为 [a.json b.json]，实际 %v", ids)
	}
	if !loaded.IsCheckinExcluded("a.json") || loaded.IsCheckinExcluded("c.json") {
		t.Fatalf("IsCheckinExcluded 判定错误: %v", ids)
	}

	// 2) 面板补丁写蛇形；且改同段其它字段时名单不被清掉。
	c := newTestConfig(t)
	save(t, c, `{"schedule":{"checkin_excluded_accounts":["c.json"]}}`)
	if !c.IsCheckinExcluded("c.json") {
		t.Fatal("蛇形补丁未生效")
	}
	save(t, c, `{"schedule":{"checkin_start":"09:00","checkin_end":"11:00"}}`)
	if !c.IsCheckinExcluded("c.json") {
		t.Fatal("局部提交其它调度字段不应清掉排除名单")
	}

	// 3) 显式清空可用空数组表达。
	save(t, c, `{"schedule":{"checkin_excluded_accounts":[]}}`)
	if got := c.CheckinExcludedAccountIDs(); len(got) != 0 {
		t.Fatalf("清空后名单应为空，实际 %v", got)
	}
}

// -----------------------------------------------------------------------------
// 批次 5：排程保活阈值 / 惰性窗口
// -----------------------------------------------------------------------------

// 默认值语义与 wb-switch 一致：keepalive_days=0（无条件刷新）、lazy_refresh_hours=24。
func TestKeepaliveParamsDefaults(t *testing.T) {
	c := newTestConfig(t)
	if got := c.KeepaliveDays(); got != 0 {
		t.Fatalf("keepalive_days 默认应为 0（每天无条件刷新），实际 %d", got)
	}
	if got := c.LazyRefreshHours(); got != 24 {
		t.Fatalf("lazy_refresh_hours 默认应为 24，实际 %d", got)
	}
}

// 面板补丁可改两个参数，且只改它们时不影响同段其它排程字段。
func TestKeepaliveParamsPatchAndPartialKeep(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"schedule":{"keepalive_days":7,"lazy_refresh_hours":12}}`)
	if c.KeepaliveDays() != 7 || c.LazyRefreshHours() != 12 {
		t.Fatalf("补丁未生效: %d/%d", c.KeepaliveDays(), c.LazyRefreshHours())
	}
	save(t, c, `{"schedule":{"keepalive_hours":[9,21]}}`)
	if c.KeepaliveDays() != 7 || c.LazyRefreshHours() != 12 {
		t.Fatalf("局部提交其它字段不应重置保活参数: %d/%d", c.KeepaliveDays(), c.LazyRefreshHours())
	}
	// 显式提交 0 是合法值（无条件刷新），不能被默认值顶掉。
	save(t, c, `{"schedule":{"keepalive_days":0}}`)
	if c.KeepaliveDays() != 0 {
		t.Fatalf("显式 0 应保留（无条件刷新），实际 %d", c.KeepaliveDays())
	}
}

// 非正数的惰性窗口回落 24；负数的保活阈值按 0（无条件）处理。
func TestKeepaliveParamsClamp(t *testing.T) {
	c := newTestConfig(t)
	c.Schedule.KeepaliveDays = -3
	c.Schedule.LazyRefreshHours = 0
	if got := c.KeepaliveDays(); got != 0 {
		t.Fatalf("负阈值应按 0 处理，实际 %d", got)
	}
	if got := c.LazyRefreshHours(); got != 24 {
		t.Fatalf("非正惰性窗口应回落 24，实际 %d", got)
	}
}

// TestPartialModelsPatchKeepsOtherList 验证只提交一个名单时不会把另一个清空。
//
// 这条对应一个真实缺陷（对照上游 v1.14.0 时查出）：applyTo 对 models 段是**整段覆盖**，
// 而面板的模型页切换单个模型的禁用只提交 `{"models":{"blocklist":[...]}}` —— 没提交的
// 白名单被清成 nil，磁盘上的 config.json 却因 deepMerge 保留着它。
// 后果是「内存里白名单没了、被封的模型重新可用，重启后白名单又回来」，且全程无提示。
func TestPartialModelsPatchKeepsOtherList(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"models":{"allowlist":["hy3","glm-5.2"]}}`)
	save(t, c, `{"models":{"blocklist":["old-model"]}}`)

	if len(c.ModelAllow) != 2 {
		t.Fatalf("只提交黑名单后白名单被清空: %#v", c.ModelAllow)
	}
	if len(c.ModelBlock) != 1 || c.ModelBlock[0] != "old-model" {
		t.Fatalf("黑名单未生效: %#v", c.ModelBlock)
	}
	// 反向：只提交白名单时不能把黑名单清掉。
	save(t, c, `{"models":{"allowlist":["hy3"]}}`)
	if len(c.ModelBlock) != 1 || c.ModelBlock[0] != "old-model" {
		t.Fatalf("只提交白名单后黑名单被清空: %#v", c.ModelBlock)
	}
	// 落盘内容同样不能被改写（内存与磁盘必须一致）。
	raw := readConfigJSON(t, c)
	models, _ := raw["models"].(map[string]any)
	if got, _ := models["blocklist"].([]any); len(got) != 1 {
		t.Fatalf("磁盘上的黑名单被改写: %#v", models["blocklist"])
	}
	if got, _ := models["allowlist"].([]any); len(got) != 1 || got[0] != "hy3" {
		t.Fatalf("磁盘上的白名单被改写: %#v", models["allowlist"])
	}
}

// TestEmptyModelsListClearsList 验证显式提交空数组仍然表示「清空该名单」。
//
// present 命中即整体覆盖，所以「清空」不需要额外的语义区分 —— 但不能因为
// 上面那条修复就把清空也变成「未提交」。
func TestEmptyModelsListClearsList(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"models":{"blocklist":["a","b"],"allowlist":["c"]}}`)
	save(t, c, `{"models":{"blocklist":[]}}`)

	if len(c.ModelBlock) != 0 {
		t.Fatalf("显式空数组应清空黑名单，实际 %#v", c.ModelBlock)
	}
	if len(c.ModelAllow) != 1 || c.ModelAllow[0] != "c" {
		t.Fatalf("同一次提交里未涉及的白名单不应被改动，实际 %#v", c.ModelAllow)
	}
}

// TestUpdateAutoDownloadRoundTrip 自动下载开关：默认开、显式关能关掉、
// 只改其它字段不会把它重置（用指针记「显式提交」正是为了这个）。
func TestUpdateAutoDownloadRoundTrip(t *testing.T) {
	c := newTestConfig(t)
	if !c.UpdateAutoDownload() {
		t.Fatal("未配置时应默认开启自动下载")
	}

	save(t, c, `{"update":{"auto_download":false}}`)
	if c.UpdateAutoDownload() {
		t.Fatal("显式关掉后仍报告开启")
	}

	// 只改间隔：不能把开关带回默认值（那会让「关掉自动下载」在下次改别的设置时失效）。
	save(t, c, `{"update":{"check_hours":12}}`)
	if c.UpdateAutoDownload() {
		t.Fatal("局部提交不该重置自动下载开关")
	}
	if c.Update.CheckHours != 12 {
		t.Fatalf("检查间隔没生效: %d", c.Update.CheckHours)
	}

	save(t, c, `{"update":{"auto_download":true}}`)
	if !c.UpdateAutoDownload() {
		t.Fatal("重新打开后应当报告开启")
	}
	// 落盘的键名与面板/文档一致。
	raw := readConfigJSON(t, c)
	upd, _ := raw["update"].(map[string]any)
	if upd["auto_download"] != true {
		t.Fatalf("config.json 里的键不对: %#v", upd)
	}
}

// TestUpdateDownloadMirrorDefaultAndOptOut 下载加速前缀的三态语义。
//
//	键不存在      → 默认加速（DefaultDownloadMirror）
//	键存在且非空  → 用填的值
//	键存在且为空  → 显式关闭加速，退回直连
//
// 空串必须有确切的含义，否则「清空并保存 = 退回直连」这条操作走不通：
// 会被当成「没配」而让默认值又把它填回来，变成一个关不掉的开关。
func TestUpdateDownloadMirrorDefaultAndOptOut(t *testing.T) {
	c := newTestConfig(t)

	// 一、未配置 → 默认开启加速。
	if got := c.UpdateDownloadMirror(); got != DefaultDownloadMirror {
		t.Fatalf("未配置时应默认开启加速（%q），得到 %q", DefaultDownloadMirror, got)
	}

	// 二、填了自定义前缀 → 用它。
	save(t, c, `{"update":{"download_mirror":"https://mirror.example.com"}}`)
	if got := c.UpdateDownloadMirror(); got != "https://mirror.example.com" {
		t.Fatalf("自定义前缀没生效: %q", got)
	}

	// 三、清空并保存 → 退回直连，且**不会**被默认值重新填上。
	save(t, c, `{"update":{"download_mirror":""}}`)
	if got := c.UpdateDownloadMirror(); got != "" {
		t.Fatalf("显式清空应退回直连（空串），得到 %q —— 默认值把它填回来了，开关关不掉", got)
	}

	// 四、之后改别的 update 字段，不能把「已关闭」重置回默认值。
	save(t, c, `{"update":{"check_hours":12}}`)
	if got := c.UpdateDownloadMirror(); got != "" {
		t.Fatalf("局部提交不该重置下载加速设置，得到 %q", got)
	}

	// 五、重新填写能再打开。
	save(t, c, `{"update":{"download_mirror":"https://ghfast.top"}}`)
	if got := c.UpdateDownloadMirror(); got != "https://ghfast.top" {
		t.Fatalf("重新填写后应生效: %q", got)
	}
}

// TestUpdateDownloadMirrorSurvivesReload 重启后三态语义仍然成立。
//
// 内存里的 updateMirrorPresent 来自加载期，而「键存在且为空」必须能在
// 重新读盘后依旧被认出来 —— 否则重启一次就把用户的「退回直连」抹掉了。
func TestUpdateDownloadMirrorSurvivesReload(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"update":{"download_mirror":""}}`)

	reloaded, err := Load(&Config{ConfigFile: c.ConfigFile, WorkDir: c.WorkDir})
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if got := reloaded.UpdateDownloadMirror(); got != "" {
		t.Fatalf("重启后应保持「已退回直连」，得到 %q", got)
	}
}

// TestProbeDisabledByDefaultButOptIn 钉住价格探测的默认值：**默认关闭**，
// 显式配置才能打开。
//
// 为什么默认必须关：探测会向模型发真实的生成请求（消耗积分）。默认开等于
// 「装好就开始悄悄花用户的积分」—— 与「下载加速」那种零成本默认开不是一回事。
// 这条断言防的是「哪次重构顺手把默认值改回 true」。
//
// 开关走的是**配置文件**（config.json 的顶层 models_probe），不是面板保存：
// 它与相邻的 models_npm 一样不在 SavePatch 的 Patch 结构里，属于「编辑文件生效」
// 那一类。所以这里用真实加载路径验证，而不是 save() 补丁。
func TestProbeDisabledByDefaultButOptIn(t *testing.T) {
	// 一、零配置：关闭。
	if c := newTestConfig(t); c.ProbeEnabled() {
		t.Fatal("价格探测必须默认关闭（它会消耗账号积分）")
	}

	// 二、显式 true → 打开。
	if c := configFromJSON(t, `{"models_probe":true}`); !c.ProbeEnabled() {
		t.Fatal("models_probe=true 应启用价格探测")
	}

	// 三、显式 false → 关闭。
	if c := configFromJSON(t, `{"models_probe":false}`); c.ProbeEnabled() {
		t.Fatal("models_probe=false 应保持关闭")
	}

	// 四、只配了邻居字段时不能被带偏（默认仍是关）。
	if c := configFromJSON(t, `{"models_npm":false}`); c.ProbeEnabled() {
		t.Fatal("只改 models_npm 不该影响价格探测的默认值")
	}
}

// configFromJSON 走真实加载路径构造配置（写文件 + Load），
// 用于验证那些不在面板 Patch 里的配置文件字段。
func configFromJSON(t *testing.T, body string) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}
	c, err := Load(&Config{ConfigFile: path, WorkDir: dir})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return c
}

// TestProbeDisabledSurvivesReload 重启后默认值语义仍然成立。
func TestProbeDisabledSurvivesReload(t *testing.T) {
	c := newTestConfig(t)

	// 未配置 → 重新加载后仍是关闭。
	reloaded, err := Load(&Config{ConfigFile: c.ConfigFile, WorkDir: c.WorkDir})
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if reloaded.ProbeEnabled() {
		t.Fatal("未配置时重新加载后仍应保持关闭")
	}

	// 显式打开 → 重新加载后仍是打开。
	save(t, c, `{"models_probe":true}`)
	reloaded2, err := Load(&Config{ConfigFile: c.ConfigFile, WorkDir: c.WorkDir})
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if !reloaded2.ProbeEnabled() {
		t.Fatal("显式打开后重新加载应保持打开")
	}
}

// TestModelsNPMStillDefaultsOn 顺带钉住邻居字段没被这次改动带偏：
// npm 目录兜底是零成本行为，默认仍是**开**。
func TestModelsNPMStillDefaultsOn(t *testing.T) {
	c := newTestConfig(t)
	if !c.ModelsNPMEnabled() {
		t.Fatal("npm 目录兜底应默认开启（它不消耗积分）")
	}
}

// TestDefaultDownloadMirrorPointsAtLiveHost 默认加速前缀必须是**能连上**的那个。
//
// 这条不测网络（单测不打外网），只钉住「不许改回已失效的 ghfast.top」这个事实：
// 该站在 2026-10 实测直连 TLS 握手无响应，默认值指向它会让新用户的下载永远转圈。
func TestDefaultDownloadMirrorPointsAtLiveHost(t *testing.T) {
	if DefaultDownloadMirror != "https://gh-proxy.org" {
		t.Fatalf("默认加速前缀应是实测可用的 gh-proxy.org，得到 %q", DefaultDownloadMirror)
	}
}
