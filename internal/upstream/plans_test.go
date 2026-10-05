package upstream

import (
	"encoding/json"
	"testing"
	"time"
)

// 国内站的四档名必须认出来 —— 显示的正是用户点开账号卡片要看的那一格。
// 认出编码就不该回落成「免费」/「pro」（那是国际站的说法）。
func TestResourcePlanNameCNTiers(t *testing.T) {
	cases := map[string]string{
		"TCACA_code_008_cfWoLwvjU4": "体验版",
		"TCACA_code_002_AkiJS3ZHF5": "标准版",
		"TCACA_code_026_BaESVICNoi": "高级版",
		"TCACA_code_027_0FCGVA6vSa": "旗舰版",
	}
	for code, want := range cases {
		got := resourcePlanName("cn", PlanResource{PackageCode: code})
		if got != want {
			t.Fatalf("国内站编码 %s 应显示 %s，实际 %s", code, want, got)
		}
	}
	// 同一个编码在国际站是另一套名字（pro 系列），不能互相套用。
	if got := resourcePlanName("intl", PlanResource{PackageCode: "TCACA_code_002_AkiJS3ZHF5"}); got != "pro" {
		t.Fatalf("国际站同一编码应显示 pro，实际 %s", got)
	}
}

// PackageName 的全名要归一进四档（「CodeBuddy个人标准版」→「标准版」），
// 但**不认识的名字必须原样保留**：强行归入四档会把新套餐伪装成已知档位。
func TestResourcePlanNameCNPackageName(t *testing.T) {
	cases := map[string]string{
		"标准版":            "标准版",
		"个人标准版":          "标准版",
		"CodeBuddy个人旗舰版": "旗舰版",
		"CodeBuddy青春版":   "CodeBuddy青春版",
		"":               "未识别订阅",
	}
	for name, want := range cases {
		got := resourcePlanName("cn", PlanResource{PackageName: name})
		if got != want {
			t.Fatalf("PackageName %q 应显示 %q，实际 %q", name, want, got)
		}
	}
}

// 国际站按官网英文名归一。
func TestResourcePlanNameINTL(t *testing.T) {
	cases := map[string]string{
		"Free Plan Subscription":        "免费",
		"Pro Plan Monthly Subscription": "pro",
		"Pro Plan Annual Subscription":  "pro",
		"Pro Plan Trial Subscription":   "Pro试用",
		"Team Plan":                     "Team Plan",
	}
	for name, want := range cases {
		if got := resourcePlanName("intl", PlanResource{PackageName: name}); got != want {
			t.Fatalf("国际站 PackageName %q 应显示 %q，实际 %q", name, want, got)
		}
	}
}

// Status=3 是「积分用尽」而不是「套餐过期」：把它当无效会让月内用光积分的
// 付费用户显示成免费 —— 这是参考实现踩过的坑。
func TestActivePlanResourceStatus3IsActive(t *testing.T) {
	now := time.Now()
	future := now.AddDate(0, 1, 0).UnixMilli()
	r := PlanResource{
		Status:           json.RawMessage("3"),
		DeductionEndTime: json.RawMessage(itoa64(future)),
	}
	active, err := activePlanResource(r, now)
	if err != nil || !active {
		t.Fatalf("Status=3（积分用尽）应仍是有效套餐：active=%v err=%v", active, err)
	}
	// Status 1/2 是退款/过期 → 无效。
	for _, st := range []string{"1", "2"} {
		r.Status = json.RawMessage(st)
		if active, _ := activePlanResource(r, now); active {
			t.Fatalf("Status=%s 应为无效权益", st)
		}
	}
}

// 已过期的权益不算有效；尚未开始的也不算。
func TestActivePlanResourceTimeWindow(t *testing.T) {
	now := time.Now()
	// 已过期
	past := PlanResource{
		Status:           json.RawMessage("0"),
		DeductionEndTime: json.RawMessage(itoa64(now.Add(-24 * time.Hour).UnixMilli())),
	}
	if active, _ := activePlanResource(past, now); active {
		t.Fatal("已过期的权益不应算有效")
	}
	// 未来开始
	futureStart := PlanResource{
		Status:             json.RawMessage("0"),
		DeductionStartTime: json.RawMessage(itoa64(now.Add(24 * time.Hour).UnixMilli())),
	}
	if active, _ := activePlanResource(futureStart, now); active {
		t.Fatal("尚未开始的权益不应算有效")
	}
	// 无截止时间 = 长期权益
	longTerm := PlanResource{Status: json.RawMessage("0")}
	if active, _ := activePlanResource(longTerm, now); !active {
		t.Fatal("无截止时间的有效状态应算长期权益")
	}
}

// 时间字段既可能是毫秒时间戳，也可能是「2006-01-02 15:04:05」（按 UTC+8 解释）。
func TestPlanTimestampFormats(t *testing.T) {
	ms := int64(1789000000000)
	got, present, err := planTimestamp(json.RawMessage(itoa64(ms)))
	if err != nil || !present || got.UnixMilli() != ms {
		t.Fatalf("毫秒时间戳解析失败：%v %v %v", got, present, err)
	}
	// 秒级时间戳自动换算
	sec := int64(1789000000)
	got, present, err = planTimestamp(json.RawMessage(itoa64(sec)))
	if err != nil || !present || got.Unix() != sec {
		t.Fatalf("秒级时间戳解析失败：%v %v %v", got, present, err)
	}
	// 无时区字符串按 UTC+8 解释，与部署机时区无关
	got, present, err = planTimestamp(json.RawMessage(`"2026-10-24 12:00:00"`))
	if err != nil || !present {
		t.Fatalf("字符串时间解析失败：%v %v", present, err)
	}
	if h := got.UTC().Hour(); h != 4 {
		t.Fatalf("UTC+8 的 12:00 换算成 UTC 应是 04:00，实际 %02d", h)
	}
	// 空 / 0 / null 表示「没有这个时间」，不是错误
	for _, raw := range []string{"", `""`, "null", "0"} {
		_, present, err := planTimestamp(json.RawMessage(raw))
		if err != nil || present {
			t.Fatalf("%q 应表示无时间且不报错：present=%v err=%v", raw, present, err)
		}
	}
}

// 套餐名净化：方向控制符能篡改终端显示，控制字符同理，一律去掉并截断。
func TestSafePlanName(t *testing.T) {
	if got := safePlanName("  pro\u202E evil  "); got != "pro evil" {
		t.Fatalf("方向控制符应被去掉，实际 %q", got)
	}
	if got := safePlanName("标准版\n"); got != "标准版" {
		t.Fatalf("换行应被去掉，实际 %q", got)
	}
	long := ""
	for i := 0; i < 100; i++ {
		long += "字"
	}
	if n := len([]rune(safePlanName(long))); n != 80 {
		t.Fatalf("超长名称应截断到 80 字，实际 %d", n)
	}
}

// identifyPlan 的主路径：摘要指定编码且该权益有效 → 用该档位名。
func TestIdentifyPlanSummaryMatch(t *testing.T) {
	now := time.Now()
	resources := []PlanResource{
		{
			PackageCode:      "TCACA_code_002_AkiJS3ZHF5",
			SubProductCode:   subProductIDE,
			Status:           json.RawMessage("0"),
			DeductionEndTime: json.RawMessage(itoa64(now.AddDate(1, 0, 0).UnixMilli())),
		},
		// 加油包不是套餐身份，必须被剔除。
		{
			PackageCode:    "TCACA_code_006_DbXS0lrypC",
			SubProductCode: subProductBonusPack,
			Status:         json.RawMessage("0"),
		},
	}
	label, err := identifyPlan("cn", planSummaryInput{SubscriptionPackageCode: "TCACA_code_002_AkiJS3ZHF5"}, resources, now)
	if err != nil {
		t.Fatalf("应能识别：%v", err)
	}
	if label != "标准版" {
		t.Fatalf("应识别为标准版，实际 %s", label)
	}
}

// 摘要声称付费、却没有任何有效权益 → **报错**而不是回落「免费」。
// 把不一致说成免费，用户会以为自己白用；报「待确认」才是诚实的。
func TestIdentifyPlanPaidWithoutEntitlement(t *testing.T) {
	now := time.Now()
	label, err := identifyPlan("intl", planSummaryInput{IsPaidUser: true}, nil, now)
	if err == nil {
		t.Fatalf("付费标记存在但无有效权益时应报错，实际返回 %q", label)
	}
}

// 有效权益里出现未知子产品 → 歧义，不能静默忽略后假称免费。
func TestIdentifyPlanAmbiguousSubProduct(t *testing.T) {
	now := time.Now()
	resources := []PlanResource{{
		PackageCode:    "TCACA_code_999_unknown",
		SubProductCode: "sp_something_new",
		Status:         json.RawMessage("0"),
	}}
	if _, err := identifyPlan("cn", planSummaryInput{}, resources, now); err == nil {
		t.Fatal("未知子产品应报「无法完整确认」而不是当成免费")
	}
}

// 完全没有权益（查询成功且列表为空）→ 免费档。
func TestIdentifyPlanFreeWhenNoEntitlements(t *testing.T) {
	label, err := identifyPlan("cn", planSummaryInput{}, nil, time.Now())
	if err != nil {
		t.Fatalf("空权益列表应判免费：%v", err)
	}
	if label != "体验版" {
		t.Fatalf("国内站空列表应显示体验版，实际 %s", label)
	}
	label, err = identifyPlan("intl", planSummaryInput{}, nil, time.Now())
	if err != nil || label != "免费" {
		t.Fatalf("国际站空列表应显示免费：%s %v", label, err)
	}
}

// 只读摘要时的保守值：编码未知不假装免费；摘要没编码时按站点给保守答案。
func TestPlanLabelFromSummary(t *testing.T) {
	cases := []struct {
		site string
		in   quotaSummary
		want string
	}{
		{"cn", quotaSummary{SubscriptionPackageCode: "TCACA_code_026_BaESVICNoi"}, "高级版"},
		{"intl", quotaSummary{SubscriptionPackageCode: "TCACA_code_002_AkiJS3ZHF5"}, "pro"},
		{"cn", quotaSummary{SubscriptionPackageCode: "TCACA_code_999_unknown"}, "付费订阅"},
		{"intl", quotaSummary{IsPaidUser: true}, "pro"},
		{"intl", quotaSummary{ProTrialStatus: float64(1)}, "Pro试用"},
		{"intl", quotaSummary{}, "免费"},
		{"cn", quotaSummary{}, "体验版"},
		{"cn", quotaSummary{IsPaidUser: true}, "付费订阅"},
	}
	for _, c := range cases {
		if got := planLabelFromSummary(c.site, c.in); got != c.want {
			t.Fatalf("%s %+v 应为 %q，实际 %q", c.site, c.in, c.want, got)
		}
	}
}

// itoa64 是测试里的小工具（避免为一行转换引入 strconv）。
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
