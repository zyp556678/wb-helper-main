package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy-gateway/internal/upstream"
)

// 本文件守住「流被截断时不能当成成功」。
//
// 背景（对照 wb-gateway v1.13.7 的 errStreamClosedWithoutFinish）：上游把连接
// **干净地**关掉（没有读错误）但整段流从未出现非空 finish_reason 时，之前会被
// 当成完整回复 —— 残缺内容记成成功、还补发 [DONE]，下游据此把残缺的工具调用
// 当完整结果执行。这类失败没有任何报错，是最难发现的一类。
//
// 两个方向都要测：只测「截断要报错」会诱导出「一律报错」的实现，
// 那会把所有正常回复也判成不完整。

// streamServer 起一个假上游，按脚本吐 SSE。
func streamServer(t *testing.T, chunks []string, withDone bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		if withDone {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

func chatStreamRequest(t *testing.T, srv *Server, upstreamURL string) *httptest.ResponseRecorder {
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
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "hi"},
		},
		"stream": true,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)
	return rec
}

const contentChunk = `{"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":""}]}`
const finishChunk = `{"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

// TestStreamClosedWithoutFinishRejected 干净关闭但没有 finish_reason → 报不完整、不补 [DONE]。
func TestStreamClosedWithoutFinishRejected(t *testing.T) {
	// 只有内容分片和 [DONE]，**没有任何非空 finish_reason**
	ts := streamServer(t, []string{contentChunk}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	rec := chatStreamRequest(t, srv, ts.URL)
	body := rec.Body.String()

	if !strings.Contains(body, "stream_closed_without_finish") {
		t.Fatalf("截断的流应当返回 stream_closed_without_finish，实际 body=%s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatal("截断的流**不能**补发 [DONE] —— 那等于告诉下游「正常结束」，会让残缺的工具调用被当成完整结果执行")
	}
}

// TestStreamWithFinishReasonSucceeds 正常带 finish_reason 的流 → 补 [DONE]、不报错。
func TestStreamWithFinishReasonSucceeds(t *testing.T) {
	ts := streamServer(t, []string{contentChunk, finishChunk}, true)
	defer ts.Close()

	srv := newTestServer(t, ts.URL)
	rec := chatStreamRequest(t, srv, ts.URL)
	body := rec.Body.String()

	if strings.Contains(body, "stream_closed_without_finish") {
		t.Fatalf("带 finish_reason 的正常流被误判为截断，body=%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("正常结束应当补发 [DONE]，body=%s", body)
	}
}

// TestNoteFinishReason 空串不算结束标记。
//
// 上游会在每个中间分片上带 finish_reason:""，把它当结束标记会让截断永远检测不出来。
func TestNoteFinishReason(t *testing.T) {
	cases := []struct {
		name    string
		current string
		chunk   string
		want    string
	}{
		{"空串不算", "", `{"choices":[{"finish_reason":""}]}`, ""},
		{"非空算", "", `{"choices":[{"finish_reason":"stop"}]}`, "stop"},
		{"tool_calls 算", "", `{"choices":[{"finish_reason":"tool_calls"}]}`, "tool_calls"},
		{"已有值不覆盖", "stop", `{"choices":[{"finish_reason":"length"}]}`, "stop"},
		{"无 choices", "", `{"usage":{"total_tokens":1}}`, ""},
		{"非法 JSON", "", `not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var chunk map[string]any
			_ = json.Unmarshal([]byte(tc.chunk), &chunk)
			if got := noteFinishReason(tc.current, chunk); got != tc.want {
				t.Fatalf("noteFinishReason(%q, %s) = %q，期望 %q", tc.current, tc.chunk, got, tc.want)
			}
		})
	}
}
