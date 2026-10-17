package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"

	"net/http"
)

func TestPrepareConversationReusedAcrossRetries(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := map[string]any{"messages": []any{}}
	first := PrepareConversation(r, body)
	if first == nil {
		t.Fatal("PrepareConversation 返回 nil")
	}
	// 模拟换号重试：同一请求对象再次进入 dispatch。
	second := PrepareConversation(r, body)
	if first != second {
		t.Fatal("同一请求的会话头被重建 —— 换号重试会被上游当成不同会话")
	}
	if first.ID == "" || first.RequestID == "" || first.MessageID == "" {
		t.Fatalf("会话头字段不完整：%+v", first)
	}
	if first.Source != "独立生成" {
		t.Fatalf("无会话键时来源应为独立生成，实际 %q", first.Source)
	}
}

func TestPrepareConversationUsesClientSeed(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Conversation-ID", "session-abc")
	h := PrepareConversation(r, map[string]any{})

	if h.Source != "客户端会话头" {
		t.Fatalf("来源应为客户端会话头，实际 %q", h.Source)
	}
	if _, ok := parseUUID(h.ID); !ok {
		t.Fatalf("会话 ID 未被规范成 UUID：%q", h.ID)
	}
	// 同一个会话键必须稳定派生出同一个 UUID —— 否则多轮之间上游看到的是新会话。
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r2.Header.Set("X-Conversation-ID", "session-abc")
	if got := PrepareConversation(r2, map[string]any{}).ID; got != h.ID {
		t.Fatalf("同一会话键派生出不同 ID：%q != %q", got, h.ID)
	}
	// 不同调用方（API Key）下的同名会话键必须分开。
	r3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r3.Header.Set("X-Conversation-ID", "session-abc")
	r3.Header.Set("Authorization", "Bearer other-key")
	if got := PrepareConversation(r3, map[string]any{}).ID; got == h.ID {
		t.Fatal("不同 API Key 下的同名会话键派生出同一 ID，会话会互相串味")
	}
}

func TestPrepareConversationSessionFields(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "顶层 conversation_id",
			body: map[string]any{"conversation_id": "c-1"},
			want: "请求会话字段",
		},
		{
			name: "metadata.session_id",
			body: map[string]any{"metadata": map[string]any{"session_id": "s-1"}},
			want: "请求 metadata 会话字段",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if got := PrepareConversation(r, tc.body).Source; got != tc.want {
				t.Fatalf("来源 %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestPrepareConversationRejectsUnusableSeed(t *testing.T) {
	// 控制字符不是合法标识，也不能进请求头。
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Conversation-ID", "bad\x01id")
	if got := PrepareConversation(r, map[string]any{}).Source; got != "独立生成" {
		t.Fatalf("非法会话键未被拒绝，来源=%q", got)
	}
	// 超长值同样拒绝。
	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Conversation-ID", strings.Repeat("a", 600))
	if got := PrepareConversation(r, map[string]any{}).Source; got != "独立生成" {
		t.Fatalf("超长会话键未被拒绝，来源=%q", got)
	}
}

func TestSetConversationHeadersFallsBackToFresh(t *testing.T) {
	// 直接构造的上游请求（价格探测、后台任务）没有准备步骤，也不能缺这三个头。
	req := httptest.NewRequest(http.MethodPost, "https://example.com", nil)
	SetConversationHeaders(req)
	for _, name := range []string{conversationIDHeader, conversationRequestIDHeader, conversationMessageIDHeader} {
		if req.Header.Get(name) == "" {
			t.Fatalf("缺少请求头 %s", name)
		}
	}
}

func TestParseUUID(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want string
	}{
		{"7518352df2975ede9583a32f3413b74d", true, "7518352d-f297-5ede-9583-a32f3413b74d"},
		{"7518352D-F297-5EDE-9583-A32F3413B74D", true, "7518352d-f297-5ede-9583-a32f3413b74d"},
		{"00000000000000000000000000000000", false, ""},
		{"not-a-uuid", false, ""},
		{"7518352df2975ede9583a32f3413b74z", false, ""},
	}
	for _, tc := range cases {
		got, ok := parseUUID(tc.in)
		if ok != tc.ok {
			t.Fatalf("%q: ok=%v，期望 %v", tc.in, ok, tc.ok)
		}
		if tc.ok && got != tc.want {
			t.Fatalf("%q: 得到 %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// 未 Prepare 的调用方（价格探测等）在重试重建请求时，必须沿用同一组会话 ID。
func TestSetConversationHeadersStableAcrossRebuild(t *testing.T) {
	first := httptest.NewRequest(http.MethodPost, "https://example.com", nil)
	SetConversationHeaders(first)
	id := first.Header.Get(conversationIDHeader)
	reqID := first.Header.Get(conversationRequestIDHeader)

	// 模拟 doChatWithRetry：新建一个请求对象，只把上一轮的头带过来。
	second := httptest.NewRequest(http.MethodPost, "https://example.com", nil)
	second.Header.Set(conversationIDHeader, id)
	second.Header.Set(conversationRequestIDHeader, reqID)
	SetConversationHeaders(second)

	if got := second.Header.Get(conversationIDHeader); got != id {
		t.Fatalf("会话 ID 被重建：%q != %q", got, id)
	}
	if got := second.Header.Get(conversationRequestIDHeader); got != reqID {
		t.Fatalf("轮次 ID 被重建：%q != %q", got, reqID)
	}
	if second.Header.Get(conversationMessageIDHeader) == "" {
		t.Fatal("缺失的消息 ID 应被补上")
	}
}

// Claude Code 常见只带 metadata.user_id；它与会话粘性必须同一口径。
func TestPrepareConversationUsesMetadataUserID(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	h := PrepareConversation(r, map[string]any{
		"metadata": map[string]any{"user_id": "user-42"},
	})
	if h.Source != "请求 metadata.user_id" {
		t.Fatalf("应识别 metadata.user_id，实际来源 %q", h.Source)
	}
	if _, ok := parseUUID(h.ID); !ok {
		t.Fatalf("会话 ID 不是 UUID：%q", h.ID)
	}
	// 同一 user_id 必须稳定派生同一会话。
	r2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	got := PrepareConversation(r2, map[string]any{"metadata": map[string]any{"user_id": "user-42"}})
	if got.ID != h.ID {
		t.Fatalf("同一 user_id 派生出不同会话：%q != %q", got.ID, h.ID)
	}
}
