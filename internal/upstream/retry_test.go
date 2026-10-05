package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTransport 按脚本模拟上游行为：前 failFirst 次返回错误，之后返回 200。
//
// markHeaders 决定失败前是否触发 WroteHeaders 回调 —— 这是本功能的核心判据，
// 必须能精确控制，否则测不出「已写出就不重试」这条。
type fakeTransport struct {
	mu          sync.Mutex
	calls       int
	failFirst   int
	transient   bool
	markHeaders bool
	failErr     error
}

func (t *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.calls++
	n := t.calls
	t.mu.Unlock()

	if n <= t.failFirst {
		if t.markHeaders {
			if tr := httptrace.ContextClientTrace(req.Context()); tr != nil && tr.WroteHeaders != nil {
				tr.WroteHeaders()
			}
		}
		if t.failErr != nil {
			return nil, t.failErr
		}
		if t.transient {
			return nil, errors.New("read tcp 10.0.0.1:443: connection reset by peer")
		}
		return nil, errors.New("dial tcp: no such host")
	}
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("data: ok\n\n")),
	}, nil
}

func (t *fakeTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func newRetryClient(ft *fakeTransport, retries int) *Client {
	return &Client{
		ChatHTTP:              &http.Client{Transport: ft},
		TransientRetries:      retries,
		TransientRetryBackoff: time.Millisecond,
	}
}

func testProfile() *Profile {
	return &Profile{Key: "cn", Base: "http://upstream.invalid"}
}

// TestChatRetriesWhenHeadersNotWritten 守住核心行为：
// 瞬时错误 + 请求头未写出 → 换新连接重试，最终成功。
func TestChatRetriesWhenHeadersNotWritten(t *testing.T) {
	ft := &fakeTransport{failFirst: 1, transient: true, markHeaders: false}
	c := newRetryClient(ft, 2)

	resp, err := c.Chat(context.Background(), &CredentialView{UID: "u1"}, testProfile(), []byte(`{}`))
	if err != nil {
		t.Fatalf("应当重试后成功，实际报错: %v", err)
	}
	defer resp.Body.Close()
	if ft.count() != 2 {
		t.Fatalf("期望调用 2 次（1 失败 + 1 重试），实际 %d 次", ft.count())
	}
}

// TestChatDoesNotRetryAfterHeadersWritten 守住最关键的安全边界：
// 请求头已写出后失败**绝不重试** —— 上游可能已经收到并开始生成，
// 重放会重复生成与重复计费。这条错了比「不重试」严重得多。
func TestChatDoesNotRetryAfterHeadersWritten(t *testing.T) {
	ft := &fakeTransport{failFirst: 1, transient: true, markHeaders: true}
	c := newRetryClient(ft, 2)

	if _, err := c.Chat(context.Background(), &CredentialView{UID: "u1"}, testProfile(), []byte(`{}`)); err == nil {
		t.Fatal("请求头已写出时应当直接报错，不能重试")
	}
	if ft.count() != 1 {
		t.Fatalf("请求头已写出后不应重试，实际调用 %d 次", ft.count())
	}
}

// TestChatDoesNotRetryNonTransientError 非瞬时错误（如 DNS 解析失败）不重试。
func TestChatDoesNotRetryNonTransientError(t *testing.T) {
	ft := &fakeTransport{failFirst: 1, transient: false}
	c := newRetryClient(ft, 2)

	if _, err := c.Chat(context.Background(), &CredentialView{UID: "u1"}, testProfile(), []byte(`{}`)); err == nil {
		t.Fatal("非瞬时错误应当直接报错")
	}
	if ft.count() != 1 {
		t.Fatalf("非瞬时错误不应重试，实际调用 %d 次", ft.count())
	}
}

// TestChatRetryIsBounded 重试次数必须有界：总共 1 次原始 + N 次重试。
func TestChatRetryIsBounded(t *testing.T) {
	ft := &fakeTransport{failFirst: 99, transient: true}
	c := newRetryClient(ft, 2)

	if _, err := c.Chat(context.Background(), &CredentialView{UID: "u1"}, testProfile(), []byte(`{}`)); err == nil {
		t.Fatal("一直失败时应当报错")
	}
	if ft.count() != 3 {
		t.Fatalf("retries=2 时总共应尝试 3 次，实际 %d 次", ft.count())
	}
}

// TestChatRetryDisabledByZero 显式配 0 表示禁用重试。
func TestChatRetryDisabledByZero(t *testing.T) {
	ft := &fakeTransport{failFirst: 1, transient: true}
	c := newRetryClient(ft, 0)

	if _, err := c.Chat(context.Background(), &CredentialView{UID: "u1"}, testProfile(), []byte(`{}`)); err == nil {
		t.Fatal("禁用重试后应当直接报错")
	}
	if ft.count() != 1 {
		t.Fatalf("禁用重试时只应尝试 1 次，实际 %d 次", ft.count())
	}
}

// TestChatRetryHonoursContextCancel 客户端已断开时不再重试（避免白烧额度）。
func TestChatRetryHonoursContextCancel(t *testing.T) {
	ft := &fakeTransport{failFirst: 99, transient: true}
	c := newRetryClient(ft, 5)
	c.TransientRetryBackoff = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Chat(ctx, &CredentialView{UID: "u1"}, testProfile(), []byte(`{}`)); err == nil {
		t.Fatal("上下文已取消时应当报错")
	}
	// 第一次尝试失败后，重试前的 select 会立刻走 ctx.Done()，不应再发第二次。
	if ft.count() > 2 {
		t.Fatalf("上下文取消后不应继续重试，实际调用 %d 次", ft.count())
	}
}

// TestIsTransientNetworkError 钉住错误分类表。
//
// 两侧都要测：漏判会让本该重试的瞬时故障直接失败；
// 误判（把不可重试的当可重试）会导致重复生成，代价更大。
func TestIsTransientNetworkError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"客户端取消", context.Canceled, false},
		{"上下文超时", context.DeadlineExceeded, false},
		{"EOF", io.EOF, true},
		{"连接被重置", errors.New("read tcp: connection reset by peer"), true},
		{"连接被重置（简写）", errors.New("connection reset"), true},
		{"管道破裂", errors.New("write: broken pipe"), true},
		{"连接已关闭", errors.New("use of closed network connection"), true},
		{"连接被拒", errors.New("dial tcp: connection refused"), true},
		{"keep-alive 被回收", errors.New("http: server closed idle connection"), true},
		{"意外 EOF", errors.New("unexpected EOF"), true},
		{"HTTP2 GOAWAY", errors.New("http2: server sent GOAWAY"), true},
		{"业务错误不重试", errors.New("上游业务错误 code=11102 msg=not found"), false},
		{"解析失败不重试", errors.New("解析上游 JSON 失败"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientNetworkError(tc.err); got != tc.want {
				t.Fatalf("IsTransientNetworkError(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}
