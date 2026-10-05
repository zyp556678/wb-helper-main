package creditwatch

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "wb-credit-history.json"))
}

func TestSnapshotDedupeWindow(t *testing.T) {
	st := newTestStore(t)
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)

	if !st.recordSnapshotAt("a", "账号A", "cn", 100, 80, base) {
		t.Fatal("首次快照应写入")
	}
	if st.recordSnapshotAt("a", "账号A", "cn", 100, 80, base.Add(2*time.Minute)) {
		t.Fatal("窗口内同值应被去重")
	}
	if !st.recordSnapshotAt("a", "账号A", "cn", 100, 70, base.Add(3*time.Minute)) {
		t.Fatal("值变化应立即写入（消耗要能归因到新快照）")
	}
	if st.recordSnapshotAt("a", "账号A", "cn", 100, 70, base.Add(6*time.Minute)) {
		t.Fatal("窗口内同值应被去重（第二次）")
	}
	if !st.recordSnapshotAt("a", "账号A", "cn", 100, 70, base.Add(9*time.Minute)) {
		t.Fatal("超过去重窗口后同值应写入")
	}
}

func TestStatisticsUsageWindowsAndZeroFill(t *testing.T) {
	st := newTestStore(t)
	day := func(d int, h int) time.Time {
		return time.Date(2026, 10, d, h, 0, 0, 0, time.Local)
	}
	// 10-01 余额 100 → 10-02 余额 90（消耗 10）→ 10-03 未快照
	// → 10-04 09:00 余额 60（消耗 30）→ 10-04 11:00 余额 55（消耗 5）
	st.recordSnapshotAt("a", "账号A", "cn", 100, 100, day(1, 10))
	st.recordSnapshotAt("a", "账号A", "cn", 100, 90, day(2, 10))
	st.recordSnapshotAt("a", "账号A", "cn", 100, 60, day(4, 9))
	st.recordSnapshotAt("a", "账号A", "cn", 100, 55, day(4, 11))
	// 余额回升（签到/发放）不计入消耗。
	st.recordSnapshotAt("a", "账号A", "cn", 100, 120, day(4, 12))

	stats := st.Statistics([]AccountInfo{{ID: "a", Name: "账号A", Site: "cn"}},
		day(4, 13))

	if stats.CoverageStartAt == nil || localDate(*stats.CoverageStartAt) != "2026-10-01" {
		t.Fatalf("覆盖起点应为 10-01，得到 %v", stats.CoverageStartAt)
	}
	if len(stats.Daily) != 4 {
		t.Fatalf("逐日序列应覆盖 10-01..10-04，共 4 天，得到 %d", len(stats.Daily))
	}
	want := map[string]float64{"2026-10-01": 0, "2026-10-02": 10, "2026-10-03": 0, "2026-10-04": 35}
	for _, point := range stats.Daily {
		if point.Usage != want[point.Date] {
			t.Errorf("%s 消耗应为 %v，得到 %v", point.Date, want[point.Date], point.Usage)
		}
	}
	if stats.Summary.UsageToday != 35 || stats.Summary.Usage7Days != 45 || stats.Summary.UsageThisMonth != 45 {
		t.Errorf("窗口消耗应为 35/45/45，得到 %v/%v/%v",
			stats.Summary.UsageToday, stats.Summary.Usage7Days, stats.Summary.UsageThisMonth)
	}
	if stats.Summary.CurrentRemaining != 120 || stats.Summary.CurrentCapacity != 100 {
		t.Errorf("当前余额应为 120/100（取最新快照），得到 %v/%v",
			stats.Summary.CurrentRemaining, stats.Summary.CurrentCapacity)
	}
	if len(stats.Accounts) != 1 {
		t.Fatalf("应有 1 个账号，得到 %d", len(stats.Accounts))
	}
	acc := stats.Accounts[0]
	if !acc.IsCurrent || acc.CurrentRemaining == nil || *acc.CurrentRemaining != 120 {
		t.Errorf("账号行应标记为当前账号且余额 120，得到 %+v", acc)
	}
	if len(acc.Daily) != 4 || acc.UsageToday != 35 {
		t.Errorf("账号逐日应 4 天、今日 35，得到 %d 天 / %v", len(acc.Daily), acc.UsageToday)
	}
}

func TestStatisticsCheckins(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 10, 4, 13, 0, 0, 0, time.Local)
	// 记录按时间顺序写入（生产路径天然如此；save 会裁掉「晚于当前时刻」的记录）。
	st.recordCheckinAt("c", "账号C", "cn", "error", "超时", time.Date(2026, 10, 3, 8, 0, 0, 0, time.Local))
	st.recordCheckinAt("a", "账号A", "cn", "success", "", time.Date(2026, 10, 4, 8, 0, 0, 0, time.Local))
	st.recordCheckinAt("b", "账号B", "cn", "already", "", time.Date(2026, 10, 4, 8, 30, 0, 0, time.Local))
	st.recordCheckinAt("a", "账号A", "cn", "error", "上游 500", time.Date(2026, 10, 4, 9, 0, 0, 0, time.Local))

	stats := st.Statistics(nil, now)
	if stats.Summary.TodaySuccess != 1 || stats.Summary.TodayAlready != 1 || stats.Summary.TodayFailed != 1 {
		t.Errorf("今日签到计数应为 1/1/1（10-03 的错误不算今天），得到 %d/%d/%d",
			stats.Summary.TodaySuccess, stats.Summary.TodayAlready, stats.Summary.TodayFailed)
	}
	// 账号A 今天先成功后失败 → 最新一次是失败，不算「已签到」。
	if stats.Summary.TodayCheckedInAccounts != 1 {
		t.Errorf("今日已签到账号应为 1（账号B），得到 %d", stats.Summary.TodayCheckedInAccounts)
	}

	byID := map[string]AccountStat{}
	for _, acc := range stats.Accounts {
		byID[acc.AccountID] = acc
	}
	a := byID["a"]
	if a.CheckedInToday == nil || *a.CheckedInToday || a.CheckinStatusToday == nil || *a.CheckinStatusToday != "error" {
		t.Errorf("账号A 今日最新一次是 error，应未签且状态 error，得到 %+v", a)
	}
	if a.LastCheckinResult == nil || *a.LastCheckinResult != "error" {
		t.Errorf("账号A 最近一次应为 error，得到 %v", a.LastCheckinResult)
	}

	// 事件按时间倒序，且两类事件都在。
	if len(stats.Events) == 0 || stats.Events[0].TS != time.Date(2026, 10, 4, 9, 0, 0, 0, time.Local).UnixMilli() {
		t.Errorf("事件应按时间倒序（首条为最新），得到 %+v", stats.Events[:min(1, len(stats.Events))])
	}
	kinds := map[string]int{}
	for _, event := range stats.Events {
		kinds[event.Kind]++
	}
	if kinds["checkin"] != 4 || kinds["usage"] != 0 {
		t.Errorf("事件构成应为 4 条签到，得到 %v", kinds)
	}
}

func TestRetentionPrunesOldRecords(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)
	st.recordSnapshotAt("old", "旧账号", "cn", 100, 50, now.AddDate(0, 0, -100))
	st.recordSnapshotAt("new", "新账号", "cn", 100, 80, now.Add(-time.Hour))

	stats := st.Statistics([]AccountInfo{{ID: "new", Name: "新账号", Site: "cn"}}, now)
	if stats.CoverageStartAt == nil || localDate(*stats.CoverageStartAt) != "2026-10-04" {
		t.Fatalf("100 天前的快照应被裁剪，覆盖起点应为今天，得到 %v", stats.CoverageStartAt)
	}
	for _, acc := range stats.Accounts {
		if acc.AccountID == "old" {
			t.Errorf("过期账号不应出现在统计里: %+v", acc)
		}
	}
}

func TestEmptyStoreGivesEmptySeries(t *testing.T) {
	st := newTestStore(t)
	stats := st.Statistics(nil, time.Now())
	if stats.CoverageStartAt != nil {
		t.Errorf("无快照时覆盖起点应为 nil，得到 %v", *stats.CoverageStartAt)
	}
	if len(stats.Daily) != 0 || len(stats.Accounts) != 0 || len(stats.Events) != 0 {
		t.Errorf("无快照时三个序列都应非 nil 且为空: %d/%d/%d",
			len(stats.Daily), len(stats.Accounts), len(stats.Events))
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var st *Store
	if st.RecordSnapshot("a", "A", "cn", 1, 1) {
		t.Error("nil Store 应返回 false")
	}
	st.RecordCheckin("a", "A", "cn", "success", "")
	stats := st.Statistics(nil, time.Now())
	if stats.RetentionDays != RetentionDays {
		t.Errorf("nil Store 也应给出保留期常量，得到 %d", stats.RetentionDays)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// -----------------------------------------------------------------------------
// 批次 5：签到日志（面板「签到日志」区）
// -----------------------------------------------------------------------------

// 空台账返回空切片（而不是 nil）：面板按数组渲染，nil 会被序列化成 null。
func TestCheckinLogsEmptyStore(t *testing.T) {
	st := newTestStore(t)
	logs := st.CheckinLogs(30, time.Now())
	if logs == nil || len(logs) != 0 {
		t.Fatalf("空台账应返回非 nil 空切片，得到 %#v", logs)
	}

	var nilStore *Store
	if got := nilStore.CheckinLogs(30, time.Now()); got == nil || len(got) != 0 {
		t.Fatalf("nil Store 也应返回非 nil 空切片，得到 %#v", got)
	}
}

// days 窗口裁剪（含默认与上限）与倒序。
func TestCheckinLogsWindowClampAndOrder(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local)
	old := now.AddDate(0, 0, -20)
	recent := now.Add(-time.Hour)

	st.recordCheckinAt("old", "旧账号", "cn", "success", "", old)
	st.recordCheckinAt("recent", "新账号", "cn", "error", "上游 500", recent)

	// 默认（days<=0）与上限（days>30）都按 30 天：两条都在窗口内，最新在前。
	for _, days := range []int{0, 30, 100} {
		logs := st.CheckinLogs(days, now)
		if len(logs) != 2 {
			t.Fatalf("days=%d 应返回 2 条，得到 %d", days, len(logs))
		}
		if logs[0].AccountID != "recent" || logs[1].AccountID != "old" {
			t.Fatalf("days=%d 应按时间倒序（recent 在前），得到 %q, %q",
				days, logs[0].AccountID, logs[1].AccountID)
		}
	}

	// 7 天窗口：只保留 recent。
	logs := st.CheckinLogs(7, now)
	if len(logs) != 1 || logs[0].AccountID != "recent" {
		t.Fatalf("days=7 应只剩 recent，得到 %+v", logs)
	}

	// 字段映射：date 是本地日历日，错误原因保留。
	item := logs[0]
	if item.Date != localDate(recent.UnixMilli()) {
		t.Fatalf("date 应为本地日历日 %q，得到 %q", localDate(recent.UnixMilli()), item.Date)
	}
	if item.AccountName != "新账号" || item.Site != "cn" || item.Result != "error" || item.Error != "上游 500" {
		t.Fatalf("字段映射错误: %+v", item)
	}
}
