package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy-gateway/internal/upstream"
)

// 本文件守住「Responses 端点也要还原正文里的模型原生工具调用标记」。
//
// 回译形态是 response.output_item.done 里的 function_call 条目（Codex 只执行
// 这种结构化条目；标记原文落进 output_text 会让它把整段标记当普通回复写进历史，
// 回合直接结束、工具一次不跑）。

// responsesRequest 以 Responses 协议发一个请求，返回响应体。
func responsesRequest(t *testing.T, srv *Server, upstreamURL string, stream bool) string {
	t.Helper()
	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base = upstreamURL
	upstream.ProfileCN.Origin = upstreamURL
	t.Cleanup(func() {
		upstream.ProfileCN.Base = oldBase
		upstream.ProfileCN.Origin = oldOrigin
	})

	payload, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"type": "function", "name": "exec_command",
			"parameters": map[string]any{"type": "object"},
		}},
		"stream": stream,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	srv.handleResponses(rec, req)
	return rec.Body.String()
}

// TestResponsesMarkupRepairStream 流式：标记还原成 function_call 条目，不进正文。
func TestResponsesMarkupRepairStream(t *testing.T) {
	block := dsmlBlock("exec_command", "ls -la")
	ts := streamServer(t, []string{
		dsmlChunk("先看一下。", ""),
		dsmlChunk(block, ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	body := responsesRequest(t, srv, ts.URL, true)

	if !strings.Contains(body, `"type":"function_call"`) {
		t.Fatalf("应产出 function_call 条目，body=%s", body)
	}
	if !strings.Contains(body, `"name":"exec_command"`) {
		t.Fatalf("function_call 应带工具名，body=%s", body)
	}
	if strings.Contains(body, dsmlPipe+"DSML") {
		t.Fatalf("标记块不应原样出现在响应里，body=%s", body)
	}
}

// TestResponsesMarkupRepairNonStream 非流式：聚合路径同样还原。
func TestResponsesMarkupRepairNonStream(t *testing.T) {
	block := dsmlBlock("exec_command", "pwd")
	ts := streamServer(t, []string{
		dsmlChunk(block, ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	body := responsesRequest(t, srv, ts.URL, false)

	if !strings.Contains(body, `"type":"function_call"`) {
		t.Fatalf("非流式也应还原为 function_call，body=%s", body)
	}
	if strings.Contains(body, dsmlPipe+"DSML") {
		t.Fatalf("非流式响应里也不应残留标记，body=%s", body)
	}
}

// TestResponsesMarkupAbsent 普通正文 → output_text，无 function_call。
func TestResponsesMarkupAbsent(t *testing.T) {
	ts := streamServer(t, []string{
		dsmlChunk("这是一段正常回复。", ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	body := responsesRequest(t, srv, ts.URL, true)

	if strings.Contains(body, `"type":"function_call"`) {
		t.Fatalf("无标记时不应产出 function_call，body=%s", body)
	}
	if !strings.Contains(body, "正常回复") {
		t.Fatalf("正文应出现在 output_text 里，body=%s", body)
	}
}
