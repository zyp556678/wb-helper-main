package stats

import (
	"testing"
	"time"
)

// localDayIndex 的语义是「本地日历日」，这是与官方账本做差集的前提。
//
// 这个测试存在的理由：原来的实现是 `t.Unix()/86400`（UTC 日），在 GMT+8 下
// 「今天」从本地 08:00 才起算，与官方账本（本地零点切日）错开 8 小时。
// 任何把它改回 UTC 日、或改成 `Add(-offset)` 的写法都会在这里失败。
func TestLocalDayIndexIsCivilDay(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)

	// 同一个本地日的两个时刻必须落在同一个键上 —— 包括跨 UTC 日的那个时刻。
	early := time.Date(2026, 9, 25, 1, 0, 0, 0, loc)  // 本地 01:00 = UTC 前一天 17:00
	late := time.Date(2026, 9, 25, 23, 30, 0, 0, loc) // 本地 23:30 = UTC 当天 15:30
	if a, b := localDayIndex(early), localDayIndex(late); a != b {
		t.Fatalf("同一本地日应当同键：01:00 -> %d，23:30 -> %d", a, b)
	}

	// 尤其要盯住「UTC 日不同但本地日相同」的情形：这正是旧实现的错处。
	if early.UTC().Day() == late.UTC().Day() {
		t.Fatal("测试前提不成立：这两个时刻的 UTC 日应当不同")
	}

	// 跨本地零点必须换键，且恰好差 1。
	prev := time.Date(2026, 9, 24, 23, 59, 59, 0, loc)
	next := time.Date(2026, 9, 25, 0, 0, 1, 0, loc)
	if a, b := localDayIndex(prev), localDayIndex(next); b-a != 1 {
		t.Fatalf("跨本地零点应当恰好差一天：%d -> %d（差 %d）", a, b, b-a)
	}
}

// 本地日索引与 dayKey 必须自洽：索引格式化出来的日期就是那个本地日期。
func TestDayKeyMatchesLocalDate(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	for _, d := range []int{1, 15, 28, 30} {
		tm := time.Date(2026, 9, d, 3, 15, 0, 0, loc)
		want := tm.Format("20060102")
		if got := dayKey(localDayIndex(tm)); got != want {
			t.Fatalf("%s 的日键应当是 %s，实际 %s", tm.Format(time.RFC3339), want, got)
		}
	}
}

// 负时区也要对：用本地时间格式化索引会渲染成前一天（这正是 dayKey 里
// 必须写 .UTC() 的原因）。这里用一个 UTC-5 的固定时区把这个回归钉住。
func TestDayKeyIsStableInNegativeOffset(t *testing.T) {
	loc := time.FixedZone("UTC-5", -5*3600)
	tm := time.Date(2026, 9, 25, 20, 0, 0, 0, loc)
	if got, want := dayKey(localDayIndex(tm)), "20260925"; got != want {
		t.Fatalf("负时区日键应当是 %s，实际 %s", want, got)
	}
}

// QueryDaily 的窗口必须按本地日切：「今天」这一格要覆盖从**本地零点**起的请求。
//
// 用 dailyWindow 而不是 QueryDaily 来测，是因为后者内部取 time.Now()，
// 只有恰好在本地零点附近跑才会暴露问题 —— 那种测试平时等于不跑。
func TestDailyWindowUsesLocalDay(t *testing.T) {
	// 本地 01:00（UTC 仍是前一天 17:00）。旧的 UTC 日实现会把这一格算成「昨天」。
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 9, 25, 1, 0, 0, 0, loc)

	from, nowDay := dailyWindow(now, 1)
	if from != nowDay {
		t.Fatalf("「今天」的范围应当只有一个格子：from=%d nowDay=%d", from, nowDay)
	}
	// 这一格必须就是「2026-09-25」这个本地日。
	if got := dayKey(nowDay); got != "20260925" {
		t.Fatalf("「今天」应当是 20260925，实际 %s", got)
	}

	// 跨 7 天窗口也要落在本地日上：起点 = 本地日 −6。
	from7, nowDay7 := dailyWindow(now, 7)
	if want := nowDay - 6; from7 != want {
		t.Fatalf("7 天窗口起点应当是 %d，实际 %d", want, from7)
	}
	if nowDay7 != nowDay {
		t.Fatalf("7 天窗口终点应当与 1 天窗口一致：%d vs %d", nowDay7, nowDay)
	}
}

// 记录一次请求后，它必须落在 QueryDaily(1) 的「今天」格里。
// 这条锁的是「Record 与 QueryDaily 用的是同一个日索引」——两边不一致时，
// 会出现「刚打完请求，图表今天还是 0」这种看起来像没记上的现象。
func TestRecordLandsInTodayBucket(t *testing.T) {
	s := New("")
	s.Load()

	s.Record(RecordInput{Model: "m", Account: "a", InputTokens: 3, OutputTokens: 4})

	snap := s.QueryDaily(1)
	if snap.Totals.Requests != 1 {
		t.Fatalf("刚记录的请求应当计入「今天」，实际 requests=%d", snap.Totals.Requests)
	}
	if n := len(snap.Series); n != 1 {
		t.Fatalf("QueryDaily(1) 应当只有 1 个点，实际 %d", n)
	}
	if snap.Series[0].Label != "今天" {
		t.Fatalf("最后一格应当是「今天」，实际 %q", snap.Series[0].Label)
	}
	if snap.Totals.Tokens != 7 {
		t.Fatalf("token 应当被累加，实际 %d", snap.Totals.Tokens)
	}
}
