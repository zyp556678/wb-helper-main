package tasks

import (
	"errors"
	"fmt"
	"testing"
)

// TestPreconditionErrorIsRecognized 守住「前提不满足」这一类错误能被识别出来。
//
// 这条分类决定了 HTTP 状态码：被认出来 → 409（重试无用），
// 认不出来 → 502（会被用户理解成「网络/上游故障，重试一下」）。
// 认错的代价很具体：用户对着一个永远不会成功的按钮反复重试。
func TestPreconditionErrorIsRecognized(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"直接构造", precondition("国际站没有成长中心活动"), true},
		{"带格式化参数", precondition("%s 没有成长中心活动", "国际站"), true},
		{"被 fmt.Errorf 包裹", fmt.Errorf("执行失败: %w", precondition("账号不存在")), true},
		{"普通错误", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsPrecondition(c.err); got != c.want {
				t.Errorf("IsPrecondition(%v) = %v，期望 %v", c.err, got, c.want)
			}
		})
	}
}

// TestPreconditionKeepsMessage 守住「分类不改变文案」。
//
// 前端是把 detail 原样展示出来的，所以分类重构不能顺手改掉人话。
func TestPreconditionKeepsMessage(t *testing.T) {
	err := precondition("%s 没有成长中心活动，无法执行旅行动作", "国际站")
	want := "国际站 没有成长中心活动，无法执行旅行动作"
	if err.Error() != want {
		t.Errorf("文案被改动了：\n got = %q\nwant = %q", err.Error(), want)
	}
}
