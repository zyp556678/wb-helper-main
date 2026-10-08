package server

import (
	"math"
	"testing"
	"time"
)

// TestTokensPerSecondFallsBackWhenWindowTooSmall 守住那条守卫。
//
// 回归背景（上游 workbuddy2api-panel issue #127）：上游把整个响应攒到最后一次性
// 下发时（假流式 / 中间层攒批刷新），首个 SSE 帧与末帧几乎同时到达，
// total − ttfb 只剩几毫秒 —— 几百 token 除出**上万 tok/s 的幻数**。
// 那种形态下"生成时长"根本不可测，诚实的分母只有端到端耗时。
func TestTokensPerSecondFallsBackWhenWindowTooSmall(t *testing.T) {
	cases := []struct {
		name    string
		tokens  int64
		total   time.Duration
		ttfb    time.Duration
		want    float64
		comment string
	}{
		{
			name: "真流式：首帧到末帧 4s 铺满窗口", tokens: 500,
			total: 5 * time.Second, ttfb: time.Second, want: 125,
			comment: "500 / (5-1) = 125 tok/s",
		},
		{
			name: "攒批下发：扣除后只剩 50ms → 退回端到端", tokens: 500,
			total: 5 * time.Second, ttfb: 4950 * time.Millisecond, want: 100,
			comment: "拿 50ms 当分母会得到 10000 tok/s 的幻数；退回端到端是 500/5 = 100",
		},
		{
			name: "恰好达到 200ms 下限 → 照常扣除", tokens: 100,
			total: time.Second, ttfb: 800 * time.Millisecond, want: 500,
			comment: "100 / 0.2 = 500（边界取闭区间）",
		},
		{
			name: "刚低于下限 → 退回端到端", tokens: 100,
			total: time.Second, ttfb: 850 * time.Millisecond, want: 100,
			comment: "100 / 1 = 100",
		},
		{
			name: "非流式：没有首字时刻", tokens: 200,
			total: 2 * time.Second, ttfb: 0, want: 100,
			comment: "分母落到端到端",
		},
		{
			name: "没有输出 token（上游没报）", tokens: 0,
			total: 5 * time.Second, ttfb: time.Second, want: 0,
			comment: "宁可给 0，也不给一个凭空的速率",
		},
		{
			name: "耗时为 0（异常）", tokens: 100,
			total: 0, ttfb: 0, want: 0,
			comment: "防除零",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tokensPerSecond(c.tokens, c.total, c.ttfb)
			if math.Abs(got-c.want) > 0.01 {
				t.Errorf("tokensPerSecond = %.2f，期望 %.2f（%s）", got, c.want, c.comment)
			}
		})
	}
}

// TestTokensPerSecondNeverProducesPhantomRates 守住守卫真正承诺的那条不变量：
//
//	**分母永远不会小于 minGenWindow**（除非退回端到端，那时分母更大）。
//
// 它等价于「速率不会超过 tokens / minGenWindow」—— 这正是 200ms 这个阈值的作用：
// 把"窗口小到没有意义"的情形挡掉，而不是给速率设一个人为上限。
//
// 第一版我把它写成了「速率不超过 1000 tok/s」，结果 ttfb=4525ms（窗口 475ms、
// 500 token）被判失败 —— 1053 tok/s。那条断言是错的：475ms 的窗口按设计就是
// 可信的，算出来多少就是多少。断言该盯的是分母下限，不是速率数值。
func TestTokensPerSecondNeverProducesPhantomRates(t *testing.T) {
	const tokens = 500
	const total = 5 * time.Second
	endToEnd := float64(tokens) / total.Seconds() // 100 tok/s
	maxByDesign := float64(tokens) / minGenWindow.Seconds()

	for ttfbMs := 0; ttfbMs <= 5000; ttfbMs += 25 {
		ttfb := time.Duration(ttfbMs) * time.Millisecond
		got := tokensPerSecond(tokens, total, ttfb)
		if got > maxByDesign+0.01 {
			t.Fatalf("ttfb=%vms 时速率 %.1f 超过设计上限 %.1f —— 分母被压到 minGenWindow 以下了",
				ttfbMs, got, maxByDesign)
		}
		if got < endToEnd-0.01 {
			t.Fatalf("ttfb=%vms 时速率 %.2f 低于端到端口径 %.2f —— 不该比端到端还慢",
				ttfbMs, got, endToEnd)
		}
	}
}
