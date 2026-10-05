package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/pool"
)

// newTestScheduler 造一个空池、无上游的调度器（只测窗口判定这一层）。
func newTestScheduler(t *testing.T, start, end string) (*Scheduler, *config.Config) {
	t.Helper()
	cfg := &config.Config{WorkDir: t.TempDir()}
	cfg.Schedule.CheckinStart = start
	cfg.Schedule.CheckinEnd = end
	cfg.ApplyDefaults()
	return New(cfg, pool.New(cfg, nil), nil), cfg
}

// 窗口外：整轮跳过，**返回 skipped 且不报错**。
//
// 跳过不是错误 —— 一次上游请求都没发、账号状态也没动，
// 报成错误会让面板显示红色的失败提示，而实际上什么都没出错。
func TestRunCheckinNowIfInWindowSkipsOutside(t *testing.T) {
	// 造一个「肯定不包含当前时刻」的窗口：取当前时刻前后各 8 分钟的补集。
	now := time.Now()
	startMin := (now.Hour()*60 + now.Minute() + 600) % (24 * 60)
	endMin := (startMin + 5) % (24 * 60)
	if startMin >= endMin { // 跨午夜，换一个安全的
		startMin, endMin = 0, 1
	}
	clock := func(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

	s, cfg := newTestScheduler(t, clock(startMin), clock(endMin))
	if cfg.InCheckinWindow(now) {
		t.Skip("当前时刻恰好落在构造的窗口内（概率极低），跳过该断言")
	}

	detail, skipped, err := s.RunCheckinNowIfInWindow(context.Background())
	if err != nil {
		t.Fatalf("窗口外跳过不该报错: %v", err)
	}
	if !skipped {
		t.Fatal("窗口外应报告 skipped=true")
	}
	if detail == "" {
		t.Fatal("应给出可读的跳过原因（面板要显示给用户）")
	}
}

// 窗口内：走正常签到路径（空池 → 报错，说明它**没有**被窗口拦下）。
func TestRunCheckinNowIfInWindowProceedsInside(t *testing.T) {
	now := time.Now()
	startMin := now.Hour()*60 + now.Minute()
	endMin := startMin + 5
	if endMin >= 24*60 {
		endMin = 24*60 - 1
	}
	clock := func(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

	s, _ := newTestScheduler(t, clock(startMin), clock(endMin))
	_, skipped, err := s.RunCheckinNowIfInWindow(context.Background())
	if skipped {
		t.Fatal("窗口内不该跳过")
	}
	if err == nil {
		t.Fatal("空池应报错 —— 说明确实走进了签到路径（没被窗口拦下）")
	}
}

// 未配置窗口时必须**完全等价于立即签到**（行为与引入前一致）。
func TestRunCheckinNowIfInWindowNoWindowIsImmediate(t *testing.T) {
	s, _ := newTestScheduler(t, "", "")
	_, skipped, err := s.RunCheckinNowIfInWindow(context.Background())
	if skipped {
		t.Fatal("未配置窗口时不该跳过")
	}
	if err == nil {
		t.Fatal("空池应报错（说明走的是正常签到路径）")
	}
}

// minutesToClock 的面板文案格式。
func TestMinutesToClock(t *testing.T) {
	cases := map[int]string{0: "00:00", 9 * 60: "09:00", 20*60 + 5: "20:05", 23*60 + 59: "23:59"}
	for m, want := range cases {
		if got := minutesToClock(m); got != want {
			t.Fatalf("minutesToClock(%d) = %q，期望 %q", m, got, want)
		}
	}
}

// 计数归类：四种处置各自进正确的桶，首个错误只记一次。
func TestCheckinSummaryCount(t *testing.T) {
	var sum CheckinSummary
	sum.Count(CheckinSuccess, "")
	sum.Count(CheckinAlready, "")
	sum.Count(CheckinSkipped, "")
	sum.Count(CheckinFailed, "第一个错误")
	sum.Count(CheckinFailed, "第二个错误")
	sum.Count(CheckinSkipped, "")

	if sum.Success != 1 || sum.Already != 1 || sum.Failed != 2 || sum.Skipped != 2 {
		t.Fatalf("计数归类错误: %+v", sum)
	}
	if sum.FirstError != "第一个错误" {
		t.Fatalf("FirstError 应只记第一个，实际 %q", sum.FirstError)
	}
	if sum.OutsideWindow {
		t.Fatal("正常路径不应标 outside_window")
	}

	detail := sum.Detail()
	for _, want := range []string{"成功 1", "已签过 1", "失败 2", "跳过 2", "第一个错误"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("Detail 应包含 %q，实际 %q", want, detail)
		}
	}
}

// 排除决定（排程签到与批量签到共用）：
// 名单内跳过、名单外照常、禁用优先于一切；
// schedule.include_disabled_in_tasks 打开后禁用号照常签到（禁用只关选号、不停保号）。
func TestDecideCheckinAction(t *testing.T) {
	cases := []struct {
		disabled, excluded, includeDisabled bool
		want                                checkinAction
	}{
		{false, false, false, actionCheckin},       // 不在名单 → 照常参与
		{false, true, false, actionSkipAndRefresh}, // 名单内 → 跳过签到但额度照刷
		{true, false, false, actionSkipOnly},       // 禁用 → 完全跳过
		{true, true, false, actionSkipOnly},        // 禁用优先
		{true, false, true, actionCheckin},         // 覆盖开关打开 → 禁用号照常签到
		{true, true, true, actionSkipAndRefresh},   // 覆盖 + 排除名单 → 按排除名单处理
	}
	for _, c := range cases {
		if got := decideCheckinAction(c.disabled, c.excluded, c.includeDisabled); got != c.want {
			t.Fatalf("decideCheckinAction(disabled=%v, excluded=%v, includeDisabled=%v) = %v，期望 %v",
				c.disabled, c.excluded, c.includeDisabled, got, c.want)
		}
	}
}

// TestDecideCheckinActionFollowsConfig 开关来自配置快照，热改立即生效
// （面板保存配置走「换指针」，因此必须每次现场读）。
func TestDecideCheckinActionFollowsConfig(t *testing.T) {
	s, cfg := newTestScheduler(t, "", "")
	if s.includeDisabledInTasks() {
		t.Fatal("默认应为关闭（保持「禁用的跳过」既有语义）")
	}
	next := cfg.Clone()
	on := true
	next.Schedule.IncludeDisabledInTasks = &on
	s.SetConfig(next)
	if !s.includeDisabledInTasks() {
		t.Fatal("SetConfig 换入新快照后开关应立即生效")
	}
}

// 排除名单来自当前配置快照，且 SetConfig 换入后立即可见（热改）。
func TestExcludedCheckinIDsFollowsConfig(t *testing.T) {
	s, cfg := newTestScheduler(t, "", "")
	if len(s.excludedCheckinIDs()) != 0 {
		t.Fatal("默认不应有排除名单")
	}

	cfg.Schedule.CheckinExcludedAccounts = []string{" a.json ", "", "b.json", "a.json"}
	set := s.excludedCheckinIDs()
	if len(set) != 2 || !set["a.json"] || !set["b.json"] {
		t.Fatalf("名单应去空白/去重，实际 %v", set)
	}

	// SetConfig 换入新快照（模拟面板保存配置）后必须立即生效。
	next := cfg.Clone()
	next.Schedule.CheckinExcludedAccounts = []string{"c.json"}
	s.SetConfig(next)
	set = s.excludedCheckinIDs()
	if len(set) != 1 || !set["c.json"] {
		t.Fatalf("SetConfig 后应看到新名单，实际 %v", set)
	}
}

// 空池：结构化入口同样报错（与 RunCheckinNow 的既有语义一致）。
func TestRunCheckinNowSummaryEmptyPool(t *testing.T) {
	s, _ := newTestScheduler(t, "", "")
	if _, err := s.RunCheckinNowSummary(context.Background()); err == nil {
		t.Fatal("空池应报错")
	}
}

// 排程保活窗口的判定表：
//   - 剩余有效期 > keepalive_days → 不刷（阈值语义）
//   - keepalive_days = 0 → 无条件刷
//   - 距上次刷新不足 lazy_refresh_hours → 跳过（惰性窗口）
//   - 零值窗口（手动 button 用）→ 恒为真，不受任何阈值限制
func TestKeepaliveWindowDue(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)
	day := int64(24 * 60 * 60)
	recent := now.Add(-time.Hour)   // 1 小时前刚刷过
	old := now.Add(-48 * time.Hour) // 48 小时前刷过

	cases := []struct {
		name      string
		window    KeepaliveWindow
		expiresAt int64
		refreshed time.Time
		want      bool
	}{
		{
			name:      "剩余 30 天 > 阈值 7 天：不刷",
			window:    KeepaliveWindow{Days: 7},
			expiresAt: now.Unix() + 30*day,
			want:      false,
		},
		{
			name:      "剩余 3 天 < 阈值 7 天：刷",
			window:    KeepaliveWindow{Days: 7},
			expiresAt: now.Unix() + 3*day,
			want:      true,
		},
		{
			name:      "到期时间未知：视为需要保活",
			window:    KeepaliveWindow{Days: 7},
			expiresAt: 0,
			want:      true,
		},
		{
			name:      "阈值 0：无条件刷（剩余 30 天也刷）",
			window:    KeepaliveWindow{Days: 0},
			expiresAt: now.Unix() + 30*day,
			want:      true,
		},
		{
			name:      "惰性窗口内（1 小时前刷过，窗口 12 小时）：跳过",
			window:    KeepaliveWindow{Days: 0, LazyHours: 12},
			expiresAt: now.Unix() + 30*day,
			refreshed: recent,
			want:      false,
		},
		{
			name:      "超出惰性窗口（48 小时前刷过，窗口 12 小时）：刷",
			window:    KeepaliveWindow{Days: 0, LazyHours: 12},
			expiresAt: now.Unix() + 30*day,
			refreshed: old,
			want:      true,
		},
		{
			name:      "本进程从未刷过：惰性窗口不拦",
			window:    KeepaliveWindow{Days: 0, LazyHours: 12},
			expiresAt: now.Unix() + 30*day,
			want:      true,
		},
		{
			name:      "零值窗口（手动）：刚刷过且剩余充足也照样刷",
			window:    KeepaliveWindow{},
			expiresAt: now.Unix() + 30*day,
			refreshed: now,
			want:      true,
		},
	}
	for _, c := range cases {
		if got := c.window.Due(c.expiresAt, c.refreshed, now); got != c.want {
			t.Fatalf("%s: Due() = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// 排程保活的窗口取自当前配置快照，且 SetConfig 换入后立即生效（热改）。
func TestScheduledKeepaliveFollowsConfig(t *testing.T) {
	s, cfg := newTestScheduler(t, "", "")
	cfg.Schedule.KeepaliveDays = 7
	cfg.Schedule.LazyRefreshHours = 12
	got := KeepaliveWindow{Days: s.config().KeepaliveDays(), LazyHours: s.config().LazyRefreshHours()}
	if got != (KeepaliveWindow{Days: 7, LazyHours: 12}) {
		t.Fatalf("窗口应取配置值，得到 %+v", got)
	}

	next := cfg.Clone()
	next.Schedule.KeepaliveDays = 0
	next.Schedule.LazyRefreshHours = 0
	s.SetConfig(next)
	if s.config().KeepaliveDays() != 0 || s.config().LazyRefreshHours() != 24 {
		t.Fatalf("SetConfig 后应看到新窗口（lazy 非正数回落 24），得到 %d/%d",
			s.config().KeepaliveDays(), s.config().LazyRefreshHours())
	}
}
