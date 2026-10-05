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

// 默认必须是**关闭**：打开意味着开始向上游伪造客户端行为，代价由账号承担。
// 默认开会让用户在不知情的情况下接受这个风险。
func TestDesktopEventsDefaultOff(t *testing.T) {
	c := newTestConfig(t)
	if c.TaskDesktopEventsEnabled() {
		t.Fatal("默认必须关闭")
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
