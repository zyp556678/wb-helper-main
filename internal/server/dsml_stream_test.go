package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy-gateway/internal/upstream"
)

// 本文件守住「正文里的模型原生工具调用标记会被还原成结构化 tool_calls」。
//
// 背景（对照 wb2api-panel 的 dsml.go）：上游仅在请求声明了 tools 时才把标记解析成
// 结构化调用。tools 在中转环节丢失是常态，此时模型仍想调工具，标记以纯文本落进
// content，客户端当成普通正文，回合直接结束、工具一次都不跑。
//
// 分隔符是全角竖线（U+FF5C）×2，字面量易被编辑器改坏，夹具一律用常量拼装。

// dsmlPipe 与 dsml 包的 markupPipe 同值，这里独立声明避免导出内部常量。
const dsmlPipe = "｜｜"

// dsmlBlock 拼一个完整的 calls 块，invoke 名为 name、参数 cmd 为 string 类型。
func dsmlBlock(name, cmd string) string {
	return "<" + dsmlPipe + "DSML" + dsmlPipe + " calls>\n" +
		"<" + dsmlPipe + "DSML" + dsmlPipe + " invoke name=\"" + name + "\">\n" +
		"<" + dsmlPipe + "DSML" + dsmlPipe + " parameter name=\"cmd\" string=\"true\">" + cmd +
		"</" + dsmlPipe + "DSML" + dsmlPipe + " parameter>\n" +
		"</" + dsmlPipe + "DSML" + dsmlPipe + " invoke>\n" +
		"</" + dsmlPipe + "DSML" + dsmlPipe + " calls>"
}

// dsmlChunk 造一个带正文增量的分片。
func dsmlChunk(content string, finish string) string {
	fr := "null"
	if finish != "" {
		fr = `"` + finish + `"`
	}
	b, _ := json.Marshal(map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": content}, "finish_reason": json.RawMessage(fr),
		}},
	})
	return string(b)
}

// chatStreamRequestWithTools 与 chatStreamRequest 同，但请求体带 tools 声明，
// 返回响应体文本。
func chatStreamRequestWithTools(t *testing.T, srv *Server, upstreamURL string, tools []string) string {
	t.Helper()
	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base = upstreamURL
	upstream.ProfileCN.Origin = upstreamURL
	t.Cleanup(func() {
		upstream.ProfileCN.Base = oldBase
		upstream.ProfileCN.Origin = oldOrigin
	})

	toolList := make([]any, 0, len(tools))
	for _, name := range tools {
		toolList = append(toolList, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name, "parameters": map[string]any{"type": "object"}},
		})
	}
	payload, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools":  toolList,
		"stream": true,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)
	return rec.Body.String()
}

// TestStreamMarkupRepairWithTools 工具的标记块 + 声明了 tools → 还原成 tool_calls。
func TestStreamMarkupRepairWithTools(t *testing.T) {
	block := dsmlBlock("exec_command", "ls -la")
	ts := streamServer(t, []string{
		dsmlChunk("我先看一下。\n", ""),
		dsmlChunk(block, ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	body := chatStreamRequestWithTools(t, srv, ts.URL, []string{"exec_command"})

	if !strings.Contains(body, "tool_calls") {
		t.Fatalf("命中标记时应还原出 tool_calls，实际 body=%s", body)
	}
	if !strings.Contains(body, `"name":"exec_command"`) {
		t.Fatalf("还原的调用应带工具名，body=%s", body)
	}
	// finish_reason 必须是 tool_calls（而不是 stop），否则客户端认为「只说了话」。
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("收尾的 finish_reason 应为 tool_calls，body=%s", body)
	}
	// 标记原文不应再作为正文出现。
	if strings.Contains(body, dsmlPipe+"DSML") {
		t.Fatalf("标记块不应原样透出为正文，body=%s", body)
	}
}

// TestStreamMarkupWeakMode 没声明 tools（名单丢失的常态）→ 弱判定仍还原。
func TestStreamMarkupWeakMode(t *testing.T) {
	block := dsmlBlock("exec_command", "ls -la")
	ts := streamServer(t, []string{
		dsmlChunk(block, ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	// 不带 tools 声明：走弱判定（默认开）。
	body := chatStreamRequest(t, srv, ts.URL).Body.String()

	if !strings.Contains(body, "tool_calls") {
		t.Fatalf("弱判定下也应还原，实际 body=%s", body)
	}
}

// TestStreamMarkupMalformedStaysText 畸形块（未闭合）→ 原文透出，绝不吞字节。
func TestStreamMarkupMalformedStaysText(t *testing.T) {
	// 只有起始标记，没有结束标记。
	truncated := "<" + dsmlPipe + "DSML" + dsmlPipe + " calls>\n" +
		"<" + dsmlPipe + "DSML" + dsmlPipe + " invoke name=\"exec_command\">\n"
	ts := streamServer(t, []string{
		dsmlChunk(truncated, ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	body := chatStreamRequestWithTools(t, srv, ts.URL, []string{"exec_command"})

	if strings.Contains(body, "tool_calls") {
		t.Fatalf("未闭合的块不应被还原，body=%s", body)
	}
	// 原文必须完整透出（含起始标记）。
	if !strings.Contains(body, dsmlPipe+"DSML") {
		t.Fatalf("未闭合块应原文透出，body=%s", body)
	}
}

// TestStreamMarkupUnknownToolStrictMode 严格判定下工具名不在名单里 → 不还原，原文透出。
func TestStreamMarkupUnknownToolStrictMode(t *testing.T) {
	block := dsmlBlock("some_other_tool", "ls")
	ts := streamServer(t, []string{
		dsmlChunk(block, ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	// 声明了 tools 但名字不匹配 → 严格判定不认（弱判定也不会启用，因为名单非空）。
	body := chatStreamRequestWithTools(t, srv, ts.URL, []string{"exec_command"})

	if strings.Contains(body, "tool_calls") {
		t.Fatalf("工具名不在名单里时不应还原，body=%s", body)
	}
}

// TestStreamMarkupAbsentWhenNoMarkup 普通正文 → 逐字节不变，零改写。
func TestStreamMarkupAbsentWhenNoMarkup(t *testing.T) {
	ts := streamServer(t, []string{
		dsmlChunk("这是一段普通回复，没有任何标记。", ""),
		dsmlChunk("", "stop"),
	}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	body := chatStreamRequestWithTools(t, srv, ts.URL, []string{"exec_command"})

	if !strings.Contains(body, "普通回复") {
		t.Fatalf("普通正文应原样透出，body=%s", body)
	}
	if strings.Contains(body, "tool_calls") {
		t.Fatalf("无标记时不应凭空造出 tool_calls，body=%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("无标记时 finish_reason 应保持 stop，body=%s", body)
	}
}
