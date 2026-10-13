package server

import (
	"testing"
)

// 本文件的用例锚在一条缺失的能力上：**用真实请求被动学习价格**。
//
// 背景：价格探测默认关闭（它消耗积分）。若只有探测这一个结论来源，关掉之后价格列
// 会永远停在「未知」、「免费站优先」也不倾斜。而正常流量一直带着证据 ——
// usage.credit 就是上游对这次请求的实际扣费。本文件钉住这条路径的判定规则。

// TestCreditVerdictFromUsage 钉住三条判定规则的边界。
func TestCreditVerdictFromUsage(t *testing.T) {
	cases := []struct {
		name        string
		usage       map[string]any
		wantVerdict string
		wantOK      bool
	}{
		{
			name:        "credit 为正 → 收费（最硬证据）",
			usage:       map[string]any{"credit": 0.8231, "total_tokens": 500},
			wantVerdict: "paid",
			wantOK:      true,
		},
		{
			name:        "credit 为 0 且样本足够 → 免费",
			usage:       map[string]any{"credit": 0, "total_tokens": 500},
			wantVerdict: "free",
			wantOK:      true,
		},
		{
			name:        "credit 为 0 但样本过小 → 不判定",
			usage:       map[string]any{"credit": 0, "total_tokens": 12},
			wantVerdict: "",
			wantOK:      false,
		},
		{
			name:        "credit 为 0 且完全没有 token 数 → 不判定",
			usage:       map[string]any{"credit": 0},
			wantVerdict: "",
			wantOK:      false,
		},
		{
			name:        "usage 里没有 credit → 不判定",
			usage:       map[string]any{"total_tokens": 500},
			wantVerdict: "",
			wantOK:      false,
		},
		{
			name:        "usage 为 nil → 不判定",
			usage:       nil,
			wantVerdict: "",
			wantOK:      false,
		},
		{
			name:        "整数形态的 credit",
			usage:       map[string]any{"credit": int64(3), "total_tokens": int64(200)},
			wantVerdict: "paid",
			wantOK:      true,
		},
		{
			name:        "字符串形态的 credit 不解析（上游给的是数字）",
			usage:       map[string]any{"credit": "0.5", "total_tokens": 500},
			wantVerdict: "",
			wantOK:      false,
		},
		{
			name:        "prompt+completion 也能凑出样本量",
			usage:       map[string]any{"credit": 0, "prompt_tokens": 80, "completion_tokens": 30},
			wantVerdict: "free",
			wantOK:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, _, ok := creditVerdictFromUsage(tc.usage)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v，期望 %v", ok, tc.wantOK)
			}
			if verdict != tc.wantVerdict {
				t.Errorf("verdict=%q，期望 %q", verdict, tc.wantVerdict)
			}
		})
	}
}

// TestCreditVerdictSmallSampleIsTheGuard 单独强调「样本过小不判定」这条防误判规则。
//
// 它守的是一个具体的坏结果：探针、极短补全这类请求的 credit 常常就是 0，
// 若据此把收费模型记成免费，选号层会把流量全压到它上面。
func TestCreditVerdictSmallSampleIsTheGuard(t *testing.T) {
	// 恰好差一个 token：仍应拒绝判定（阈值是「小于 100」即拒）。
	if _, _, ok := creditVerdictFromUsage(map[string]any{"credit": 0, "total_tokens": 99}); ok {
		t.Error("99 tokens 应判为样本不足，拒绝据此认定免费")
	}
	// 刚好达标：可以判定。
	verdict, _, ok := creditVerdictFromUsage(map[string]any{"credit": 0, "total_tokens": 100})
	if !ok || verdict != "free" {
		t.Errorf("100 tokens 应达到样本下限并判为免费，得到 ok=%v verdict=%q", ok, verdict)
	}
}

// TestCreditFieldNumericShapes 数值字段的宽容解析（token 用整数版 numField，
// credit 是浮点，需要单独一条）。
func TestCreditFieldNumericShapes(t *testing.T) {
	cases := []struct {
		value any
		want  float64
		ok    bool
	}{
		{float64(1.5), 1.5, true},
		{float32(2.5), 2.5, true},
		{int(3), 3, true},
		{int64(4), 4, true},
		{nil, 0, false},
		{"1.5", 0, false},
		{true, 0, false},
	}
	for _, tc := range cases {
		got, ok := creditField(map[string]any{"credit": tc.value}, "credit")
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("creditField(%v) = (%v, %v)，期望 (%v, %v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}
