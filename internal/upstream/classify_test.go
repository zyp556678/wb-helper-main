package upstream

import (
	"context"
	"testing"
	"time"
)

// TestFrameClass 覆盖 SSE error 帧的分类。用途是**账号处置**：上游「200 已开流 +
// 一帧 error」是真实形态（6004 限流 / 内容拦截 / 审核），这一帧决定账号该不该冷却。
func TestFrameClass(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{
			name:  "6004 限流按模型级处置",
			frame: `{"error":{"code":6004,"message":"模型 gpt-5.6-sol 触发频率限制，将在 2026-10-05 10:00:00 重置"}}`,
			want:  ErrModelRate,
		},
		{
			// 一些上游把业务码发成字符串：识别必须不受引号影响，
			// 否则限流号不会被冷却（静默漏罚比误罚更糟）。
			name:  "字符串形态的 6004",
			frame: `{"error":{"code":"6004","message":"model rate limit"}}`,
			want:  ErrModelRate,
		},
		{
			name:  "内容拦截不罚账号",
			frame: `{"error":{"message":"blocked by security policy"}}`,
			want:  ErrContentBlock,
		},
		{
			name:  "账号级限流文案",
			frame: `{"error":{"message":"请求过于频繁，请稍后再试"}}`,
			want:  ErrSoftRate,
		},
		{
			name:  "认不出的帧归 unknown（调用方只记录不处罚）",
			frame: `{"error":{"message":"something went wrong"}}`,
			want:  ErrUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FrameClass(tc.frame).Kind; got != tc.want {
				t.Fatalf("分类=%s，期望 %s", got, tc.want)
			}
		})
	}
}

// TestFrameClassKeepsResetTime 帧里的重置时刻要能解析出来：模型级冷却靠它精确对齐
// 上游恢复墙钟（看不懂的时间就退化为有界退避）。
func TestFrameClassKeepsResetTime(t *testing.T) {
	class := FrameClass(`{"error":{"code":6004,"message":"将在 2026-10-05 10:00:00 重置"}}`)
	if class.ResetAt.IsZero() {
		t.Fatalf("未解析出重置时刻：%+v", class)
	}
	want := time.Date(2026, 10, 5, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if !class.ResetAt.Equal(want) {
		t.Errorf("重置时刻=%v，期望 %v", class.ResetAt, want)
	}
}

// TestIsUpstreamTimeout 覆盖超时三态判定：超时必须与「网络抖动」「客户端断连」分开
// —— 抖动换号仍可能成功，超时换号注定白换（撞的是同一个慢上游）。
//
// clientGone 只约束第三态（context.Canceled）：客户端已经走了，那我们看到的取消
// 是它造成的，不该算成上游停滞。
func TestIsUpstreamTimeout(t *testing.T) {
	if IsUpstreamTimeout(nil, false) {
		t.Errorf("nil 错误不是超时")
	}
	if !IsUpstreamTimeout(timeoutErr{}, false) {
		t.Errorf("net.Error.Timeout() 应判为超时")
	}
	if !IsUpstreamTimeout(context.DeadlineExceeded, false) {
		t.Errorf("显式 deadline 应判为超时")
	}
	if !IsUpstreamTimeout(context.Canceled, false) {
		t.Errorf("客户端仍在而 ctx 被取消：只能是我们自己的空闲看门狗掐的流，算上游停滞")
	}
	if IsUpstreamTimeout(context.Canceled, true) {
		t.Errorf("客户端已断连时不应把取消算在上游头上")
	}
}

// timeoutErr 是最小的 net.Error 实现（Timeout() 为真）。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
