package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy-gateway/internal/upstream"
)

// 首字节前安全重放（对照 wb-gateway v1.14.24）。
//
// 场景：上游第一跳把 SSE 响应头发出来后**一个数据帧都没发就断了**（LB 换后端、
// 上游超时后干净关连接）。此时客户端什么都还没收到，网关应当重新走一次上游调度，
// 而不是直接给客户端一个 error 事件。
//
// 这条用例同时钉住反向约束：已经写出过数据时**不能**重放，否则客户端会看到
// 重复内容（见 TestStreamNoReplayAfterData）。

func TestStreamReplaysBeforeFirstByte(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		attempt := len(bodies)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if attempt == 1 {
			// 首字节前干净关闭：没有任何 data 帧，也没有 finish_reason。
			return
		}
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":""}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer ts.Close()

	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base = ts.URL
	upstream.ProfileCN.Origin = ts.URL
	defer func() {
		upstream.ProfileCN.Base = oldBase
		upstream.ProfileCN.Origin = oldOrigin
	}()

	srv := newTestServer(t, ts.URL)

	payload, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "只回答一个字：好"},
		},
		"stream": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleChatCompletions(rec, req)

	mu.Lock()
	attempts := len(bodies)
	mu.Unlock()

	if attempts != 2 {
		t.Fatalf("首字节前失败应重放一次（共 2 次上游请求），实际 %d 次", attempts)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"好"`) {
		t.Errorf("重放后的内容没有送达客户端: %q", body)
	}
	// 关键反向断言：重放成功时，客户端不该看到任何错误事件。
	if strings.Contains(body, "stream_closed_without_finish") ||
		strings.Contains(body, "stream_interrupted") {
		t.Errorf("重放成功却仍下发了错误帧: %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("缺少 [DONE] 收尾: %q", body)
	}
}

// TestStreamNoReplayAfterData 已经发出数据帧后**不允许**重放。
//
// 这是本功能最危险的失效方向：重放会让客户端看到重复内容（甚至是重复的工具调用），
// 比直接报错严重得多。上游发了半段就断时，正确行为是就地补一个错误帧、不重放。
func TestStreamNoReplayAfterData(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 只发一个数据帧就断：客户端已经收到内容了。
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"半"},"finish_reason":""}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer ts.Close()

	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base = ts.URL
	upstream.ProfileCN.Origin = ts.URL
	defer func() {
		upstream.ProfileCN.Base = oldBase
		upstream.ProfileCN.Origin = oldOrigin
	}()

	srv := newTestServer(t, ts.URL)
	payload, _ := json.Marshal(map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "只回答一个字：好"}},
		"stream":   true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleChatCompletions(rec, req)

	mu.Lock()
	attempts := len(bodies)
	mu.Unlock()

	if attempts != 1 {
		t.Fatalf("已经发出数据帧后不得重放，实际打了 %d 次上游", attempts)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"半"`) {
		t.Errorf("已发出的内容应当保留: %q", body)
	}
	// 半截流必须以错误帧收尾，而不是假装成功。
	if !strings.Contains(body, "stream_closed_without_finish") {
		t.Errorf("半截流应就地补错误帧: %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("不完整的流不能补 [DONE]（客户端会当成完整结果）: %q", body)
	}
}
