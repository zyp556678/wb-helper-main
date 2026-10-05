package upstream

import (
	"testing"
	"time"
)

// 三个容量字段两两兜底：上游不同版本给的名目不一样，且有时只给其中两个。
// 这条钉住「只给剩余与已用」时总量要能推出来 —— 推不出来会让进度条永远 0%。
func TestNormalizeResourceDerivesTotalFromRemainAndUsed(t *testing.T) {
	now := time.Now().UnixMilli()
	got := normalizeResource(resourcePackage{
		PackageCode:         "TCACA_code_007_nzdH5h4Nl0",
		CycleRemainCapacity: "75.25",
		CycleUsedCapacity:   "24.75",
	}, now)

	if got.Total != 100 {
		t.Fatalf("总量应为 75.25+24.75=100，实际 %v", got.Total)
	}
	if got.Remaining != 75.25 {
		t.Fatalf("剩余应为 75.25，实际 %v", got.Remaining)
	}
	if got.Used != 24.75 {
		t.Fatalf("已用应为 24.75，实际 %v", got.Used)
	}
}

// 数值可能是字符串也可能是数字：写死成 string 会在数字形态上解析失败，
// 而失败的表现是「额度显示为 0」—— 看起来像真的没额度，比报错更难排查。
func TestNormalizeResourceAcceptsNumberAndStringShapes(t *testing.T) {
	now := time.Now().UnixMilli()
	asNumber := normalizeResource(resourcePackage{
		CycleTotalCapacity:  100.5,
		CycleRemainCapacity: 50.25,
	}, now)
	asString := normalizeResource(resourcePackage{
		CycleTotalCapacity:  "100.5",
		CycleRemainCapacity: "50.25",
	}, now)

	if asNumber.Total != asString.Total || asNumber.Remaining != asString.Remaining {
		t.Fatalf("两种形态应等价：number=%+v string=%+v", asNumber, asString)
	}
	if asNumber.Used != 50.25 {
		t.Fatalf("已用应由总量-剩余推出 50.25，实际 %v", asNumber.Used)
	}
}

// 官方数据里出现过 DeductionEndTime=2049 与 CycleEndTime=当月月底并存。
// 前者是长期占位值，必须改用 CycleEndTime，否则界面会显示「还有 23 年到期」。
func TestResolveExpireAtPrefersCycleEndOverFarFuturePlaceholder(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.Local).UnixMilli()
	cycleEnd := time.Date(2026, 9, 30, 23, 59, 59, 0, time.Local).UnixMilli()
	placeholder := time.Date(2049, 1, 1, 0, 0, 0, 0, time.Local).UnixMilli()

	got := resolveExpireAt(resourcePackage{
		DeductionEndTime: placeholder,
		CycleEndTime:     cycleEnd,
	}, now)

	if got != cycleEnd {
		t.Fatalf("应回落到 CycleEndTime=%d，实际 %d", cycleEnd, got)
	}
}

// 远未来时间戳本身（没有 CycleEndTime 可回落）视为长期有效，返回 0。
func TestResolveExpireAtTreatsFarFutureAsPerpetual(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.Local).UnixMilli()
	far := time.Date(2049, 1, 1, 0, 0, 0, 0, time.Local).UnixMilli()

	if got := resolveExpireAt(resourcePackage{DeductionEndTime: far}, now); got != 0 {
		t.Fatalf("远未来应视为长期有效（0），实际 %d", got)
	}
}

// 日期字符串按本地时区解析：用 UTC 解析会让到期时间整体偏 8 小时。
func TestToMillisParsesDateStringInLocalZone(t *testing.T) {
	got, ok := toMillis("2026-09-30 23:59:59")
	if !ok {
		t.Fatal("日期字符串应能解析")
	}
	want := time.Date(2026, 9, 30, 23, 59, 59, 0, time.Local).UnixMilli()
	if got != want {
		t.Fatalf("本地时区解析应得 %d，实际 %d", want, got)
	}
}

// 秒级时间戳要换算成毫秒：上游部分字段给的是秒，不换算会得到 1970 年。
func TestToMillisUpgradesSecondTimestamps(t *testing.T) {
	got, ok := toMillis(float64(1790334429))
	if !ok {
		t.Fatal("秒级时间戳应能解析")
	}
	if got != 1790334429000 {
		t.Fatalf("应换算为毫秒 1790334429000，实际 %d", got)
	}
}

// 上游没给 Status 时用 -1 表示「未提供」，而不是 0（0 是有效的业务状态码）。
func TestNormalizeResourceMarksMissingStatusAsMinusOne(t *testing.T) {
	got := normalizeResource(resourcePackage{CycleTotalCapacity: "10"}, time.Now().UnixMilli())
	if got.Status != -1 {
		t.Fatalf("缺省 Status 应为 -1，实际 %d", got.Status)
	}
	if got.PackageCode != "" {
		t.Fatalf("未给商品码时应为空串，实际 %q", got.PackageCode)
	}
}
