package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy-gateway/internal/reqlog"
	"workbuddy-gateway/internal/upstream"
)

// fakeChatUpstream 起一个假上游，返回一段正常的 chat SSE（带 usage）。
//
// 首帧前刻意 sleep 一下：TTFB 是用毫秒整数记的，假上游即时响应的话
// 那个值恒为 0，断言就变成「等于 0 也算过」，钉不住东西。
func fakeChatUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		time.Sleep(20 * time.Millisecond)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":""}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`,
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

// pointUpstreamAtFake 把国内站上游指向假服务器（ProfileForSite 读的就是这两个全局）。
func pointUpstreamAtFake(t *testing.T, url string) {
	t.Helper()
	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base = url
	upstream.ProfileCN.Origin = url
	t.Cleanup(func() {
		upstream.ProfileCN.Base = oldBase
		upstream.ProfileCN.Origin = oldOrigin
	})
}

// /v1/messages 的流量必须进请求归档，且带上账号 / 模型 / TTFB / token / 速率。
//
// 这条测试对应一个真实缺口：面板「请求流水」里 Claude Code（Anthropic 协议）的记录
// 曾经只有时间、状态、耗时，账号、模型、TTFB、token、速率全是「—」——
// 因为这条路径从来没有调用过 logRequest，只在成功分支记了模型级 metrics。
// 而这些字段恰好是排查时唯一想看的东西（谁在服务、上游慢还是生成慢、花了多少 token）。
func TestMessagesStreamIsArchivedWithStats(t *testing.T) {
	ts := fakeChatUpstream(t)
	defer ts.Close()
	pointUpstreamAtFake(t, ts.URL)

	srv := newTestServer(t, ts.URL)
	rec := reqlog.New(reqlog.Config{
		Dir:     filepath.Join(srv.config().WorkDir, "request-logs"),
		Enabled: true,
	})
	defer rec.Close()
	srv.SetRequestLog(rec)

	payload := `{"model":"claude-sonnet-4","max_tokens":64,"stream":true,` +
		`"messages":[{"role":"user","content":"只回答一个字：好"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.withRequestLog(srv.handleMessages)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "event: message_stop") {
		t.Fatalf("响应不是完整的 Anthropic 事件流：%s", w.Body.String())
	}

	entries := rec.Snapshot().Recent
	if len(entries) == 0 {
		t.Fatal("请求没有进请求归档：面板「请求流水」会看不到这次调用")
	}
	e := entries[len(entries)-1]

	if e.Path != "/v1/messages" {
		t.Errorf("归档的路径不对：%q", e.Path)
	}
	if e.Model == "" {
		t.Error("归档里没有模型：面板「模型」列会显示为「—」")
	}
	if e.Account == "" {
		t.Error("归档里没有账号：面板「账号」列会显示为「—」")
	}
	if e.TTFBMs <= 0 {
		t.Errorf("归档里没有首字延迟（TTFB）：面板「TTFB」列会显示为「—」（实际 %d ms）", e.TTFBMs)
	}
	if e.TotalTokens != 15 {
		t.Errorf("归档里 token 总量应为上游 usage 的 15，实际 %d", e.TotalTokens)
	}
	// 注意：归档只记总量（reqlog.Event 的 PromptTokens / CompletionTokens 目前
	// 没有任何调用点会填，面板也只显示总 token）。这里刻意不断言拆分，
	// 免得后来者以为那两个字段是活的。
	if e.CompletionTokens != 0 || e.PromptTokens != 0 {
		t.Errorf("拆分字段目前不该被填：prompt=%d completion=%d", e.PromptTokens, e.CompletionTokens)
	}
	if e.TokensPerSec <= 0 {
		t.Errorf("归档里没有速率：面板「速率」列会显示为「—」（实际 %v）", e.TokensPerSec)
	}
	if !e.OK || e.Outcome != reqlog.OutcomeSuccess {
		t.Errorf("成功的请求应当记成 success，实际 ok=%v outcome=%q", e.OK, e.Outcome)
	}
	if e.DurationMs <= 0 {
		t.Errorf("归档里没有耗时，实际 %d ms", e.DurationMs)
	}
}

// 失败分支同样要进归档：上游以 error 帧结束时，面板上必须看得到「谁失败了」。
//
// 之前这条路径直接 return，归档里只剩一个 200 状态码的骨架记录 ——
// 表现是「请求流水一片成功，实际客户端拿到的是错误」。
func TestMessagesStreamFailureIsArchived(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: "+`{"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"半"},"finish_reason":""}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		// 上游开流之后报错（限流/内容拦截的真实形态）。
		_, _ = io.WriteString(w, "data: "+`{"error":{"code":"6004","message":"rate limited"}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer ts.Close()
	pointUpstreamAtFake(t, ts.URL)

	srv := newTestServer(t, ts.URL)
	rec := reqlog.New(reqlog.Config{
		Dir:     filepath.Join(srv.config().WorkDir, "request-logs"),
		Enabled: true,
	})
	defer rec.Close()
	srv.SetRequestLog(rec)

	payload := `{"model":"claude-sonnet-4","max_tokens":64,"stream":true,` +
		`"messages":[{"role":"user","content":"你好"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.withRequestLog(srv.handleMessages)(w, req)

	entries := rec.Snapshot().Recent
	if len(entries) == 0 {
		t.Fatal("失败的请求没有进请求归档")
	}
	e := entries[len(entries)-1]
	if e.OK || e.Outcome != reqlog.OutcomeStreamError {
		t.Errorf("上游 error 帧应当记成流式失败，实际 ok=%v outcome=%q", e.OK, e.Outcome)
	}
	if e.Model == "" || e.Account == "" {
		t.Errorf("失败的记录同样要带模型与账号，实际 model=%q account=%q", e.Model, e.Account)
	}
}

// 聚合（非流式）路径同样要记账 —— 它与流式是两套响应函数，漏一个就少一半覆盖。
func TestMessagesAggregateIsArchivedWithStats(t *testing.T) {
	ts := fakeChatUpstream(t)
	defer ts.Close()
	pointUpstreamAtFake(t, ts.URL)

	srv := newTestServer(t, ts.URL)
	rec := reqlog.New(reqlog.Config{
		Dir:     filepath.Join(srv.config().WorkDir, "request-logs"),
		Enabled: true,
	})
	defer rec.Close()
	srv.SetRequestLog(rec)

	payload := `{"model":"claude-sonnet-4","max_tokens":64,` +
		`"messages":[{"role":"user","content":"只回答一个字：好"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.withRequestLog(srv.handleMessages)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"type":"message"`) {
		t.Fatalf("响应不是聚合后的 Anthropic Message：%s", w.Body.String())
	}

	entries := rec.Snapshot().Recent
	if len(entries) == 0 {
		t.Fatal("聚合请求没有进请求归档")
	}
	e := entries[len(entries)-1]
	if e.Model == "" || e.Account == "" || e.TotalTokens != 15 {
		t.Errorf("聚合路径的归档字段不全：model=%q account=%q total=%d",
			e.Model, e.Account, e.TotalTokens)
	}
	if !e.OK || e.Outcome != reqlog.OutcomeSuccess {
		t.Errorf("聚合成功应当记成 success，实际 ok=%v outcome=%q", e.OK, e.Outcome)
	}
}
