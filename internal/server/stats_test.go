package server

import (
	"testing"
	"time"

	"workbuddy-gateway/internal/upstream"
)

// 补零的目标是「图表 X 轴不跳日」。这条测试钉住的正是那个后果：
// 只有 09-20 与 09-24 有数据时，窗口内其余 3 天必须作为 0 值点存在。
func TestZeroFillDaysFillsGapsInOrder(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.Local)
	sparse := []upstream.OfficialUsageDay{
		{Date: "2026-09-24", Requests: 177, Credit: 459.35},
		{Date: "2026-09-20", Requests: 3, Credit: 1.5},
	}

	got := zeroFillDays(sparse, now, 6) // 09-20 .. 09-25

	if len(got) != 6 {
		t.Fatalf("补零后应有 6 天，实际 %d", len(got))
	}
	want := []string{"2026-09-20", "2026-09-21", "2026-09-22", "2026-09-23", "2026-09-24", "2026-09-25"}
	for i, date := range want {
		if got[i].Date != date {
			t.Fatalf("第 %d 天应为 %s，实际 %s（顺序必须升序）", i, date, got[i].Date)
		}
	}
	if got[1].Credit != 0 || got[1].Requests != 0 {
		t.Fatalf("空白日应补 0，实际 credit=%v requests=%v", got[1].Credit, got[1].Requests)
	}
	if got[4].Credit != 459.35 {
		t.Fatalf("09-24 的 credit 被改动了：%v", got[4].Credit)
	}
}

// Models 为空时必须是**空切片**而不是 nil：nil 会被序列化成 null，
// 前端按数组处理会抛错并把整棵 React 树带走。
func TestZeroFillDaysNeverLeavesNilModels(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.Local)
	got := zeroFillDays([]upstream.OfficialUsageDay{{Date: "2026-09-25"}}, now, 2)
	for _, d := range got {
		if d.Models == nil {
			t.Fatalf("%s 的 Models 是 nil，序列化后会变成 null", d.Date)
		}
	}
}

// 窗口外的日期必须被丢掉：上游按 days 参数返回，但边界上多一天会让
// 「近 30 天」的合计把前一天也算进去。
func TestZeroFillDaysDropsOutOfRangeDates(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.Local)
	got := zeroFillDays([]upstream.OfficialUsageDay{
		{Date: "2026-09-19", Credit: 999},
		{Date: "2026-09-25", Credit: 1},
	}, now, 3) // 09-23 .. 09-25

	if len(got) != 3 {
		t.Fatalf("应只保留窗口内 3 天，实际 %d", len(got))
	}
	for _, d := range got {
		if d.Credit == 999 {
			t.Fatalf("窗口外的日期 %s 未被丢弃", d.Date)
		}
	}
}
