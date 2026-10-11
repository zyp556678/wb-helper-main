package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy-gateway/internal/upstream"
)

// 本文件守住「Anthropic 端点也要还原正文里的模型原生工具调用标记」。
//
// 与 chat 端点同一份状态机（internal/dsml），区别只在回译形态：这里是
// content_block(tool_use) + stop_reason=tool_use，而不是 delta.tool_calls。

// messagesStreamRequest 以 Anthropic 协议发一个流式请求，返回响应体。
func messagesStreamRequest(t *testing.T, srv *Server, upstreamURL string, withTools bool) string {
	t.Helper()
	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base = upstreamURL
	upstream.ProfileCN.Origin = upstreamURL
	t.Cleanup(func() {
		upstream.ProfileCN.Base = oldBase
		upstream.ProfileCN.Origin = oldOrigin
	})

	body := map[string]any{
		"model":      "deepseek-v4.1-flash",
		"max_tokens": float64(1024),
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":     true,
	}
	if withTools {
		body["tools"] = []any{map[string]any{
			"name": "exec_command", "input_schema": map[string]any{"type": "object"},
		}}
	}
	payload, _ := json.Marshal(body)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	srv.handleMessages(rec, req)
	return rec.Body.String()
}

// TestMessagesMarkupRepairToolUse 声明了 tools 的标记块 → tool_use 块 + stop_reason=tool_use。
func TestMessagesMarkupRepairToolUse(t *testing.T) {
	block := dsmlBlock("exec_command", "ls -la")
	ts := streamServer(t, []string{
		dsmlChunk("先看一下。", ""),
		dsmlChunk(block, ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	body := messagesStreamRequest(t, srv, ts.URL, true)

	if !strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("应产出 tool_use 内容块，body=%s", body)
	}
	if !strings.Contains(body, `"name":"exec_command"`) {
		t.Fatalf("tool_use 块应带工具名，body=%s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop_reason 应映射为 tool_use，body=%s", body)
	}
	if strings.Contains(body, dsmlPipe+"DSML") {
		t.Fatalf("标记块不应原样透出为正文，body=%s", body)
	}
}

// TestMessagesMarkupWeakMode 没声明 tools → 弱判定仍还原。
func TestMessagesMarkupWeakMode(t *testing.T) {
	block := dsmlBlock("exec_command", "pwd")
	ts := streamServer(t, []string{
		dsmlChunk(block, ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	body := messagesStreamRequest(t, srv, ts.URL, false)

	if !strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("弱判定下也应还原为 tool_use，body=%s", body)
	}
}

// TestMessagesMarkupAbsent 普通正文 → tool_use 不出现，stop_reason 保持 end_turn。
func TestMessagesMarkupAbsent(t *testing.T) {
	ts := streamServer(t, []string{
		dsmlChunk("这是一段正常回复。", ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	body := messagesStreamRequest(t, srv, ts.URL, true)

	if strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("无标记时不应产出 tool_use，body=%s", body)
	}
	if !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("无标记时 stop_reason 应为 end_turn，body=%s", body)
	}
	if !strings.Contains(body, "正常回复") {
		t.Fatalf("正文应原样透出，body=%s", body)
	}
}
