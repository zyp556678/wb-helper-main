package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// 领养门槛未达标的判定**必须看状态码**，而不是只匹配文案。
//
// 为什么：上游用 HTTP 400 承载「first_buddy task not completed yet」这个
// 正常语义（当日无活跃上报），调用方要静默跳过、且当日不再重试。
// 只做字符串匹配的话，上游改一个标点就会让判定失效 ——
// 那时的表现是「每个账号每天多打几次注定失败的请求」，而日志里全是红色。
func TestIsBuddyTaskIncomplete(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "400 + first_buddy 关键词 → 是门槛未达",
			err:  &HTTPStatusError{Status: http.StatusBadRequest, Body: `{"msg":"first_buddy task not completed yet"}`},
			want: true,
		},
		{
			name: "关键词大小写不敏感",
			err:  &HTTPStatusError{Status: http.StatusBadRequest, Body: `{"msg":"FIRST_BUDDY Task Not Completed Yet"}`},
			want: true,
		},
		{
			name: "400 但无关键词 → 不是（别把其它 400 也当门槛）",
			err:  &HTTPStatusError{Status: http.StatusBadRequest, Body: `{"msg":"invalid param"}`},
			want: false,
		},
		{
			name: "有关键词但状态码不是 400 → 不是",
			err:  &HTTPStatusError{Status: http.StatusForbidden, Body: `first_buddy task not completed yet`},
			want: false,
		},
		{
			name: "普通错误 → 不是",
			err:  fmt.Errorf("first_buddy task not completed yet"),
			want: false,
		},
		{
			name: "包装过的类型错误仍能识别",
			err:  fmt.Errorf("领养失败: %w", &HTTPStatusError{Status: http.StatusBadRequest, Body: "first_buddy task not completed yet"}),
			want: true,
		},
		{name: "nil → 不是", err: nil, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsBuddyTaskIncomplete(c.err); got != c.want {
				t.Fatalf("IsBuddyTaskIncomplete = %v，期望 %v（err=%v）", got, c.want, c.err)
			}
		})
	}
}

// 改成类型化错误之后，**错误文案不能变**：既有代码与日志都在按这句话排查。
func TestHTTPStatusErrorKeepsLegacyMessage(t *testing.T) {
	err := &HTTPStatusError{Status: 500, Body: `{"msg":"boom"}`}
	msg := err.Error()
	want := "成长域请求失败 HTTP 500: "
	if len(msg) < len(want) || msg[:len(want)] != want {
		t.Fatalf("错误文案前缀变了：%q（期望以 %q 开头）", msg, want)
	}
	if !errors.As(error(err), new(*HTTPStatusError)) {
		t.Fatal("应能被 errors.As 还原为 *HTTPStatusError")
	}
}
