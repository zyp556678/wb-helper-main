package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy-gateway/internal/upstream"
)

// 本文件用假上游端到端验证请求主链路上的三条止损规则。它们的共同点是
// 「一次失败会连坐整条会话或整池账号」，且都只在真实上游的特定形态下触发，
// 靠人工观察很难稳定复现：
//
//  1. 上游「200 已开流 + 一帧 error」（6004 限流 / 内容拦截）→ 该账号必须被处置，
//     不能被记成健康号（否则粘性会把整个会话钉在限流号上，后续每轮都失败）；
//  2. 上游超时 → 不换号、不罚号，返回可区分的 upstream_timeout；
//  3. 请求体被上游拒绝（11101）→ 立即 400 透传原文、不再轮转。
//
// 三条都用「数上游被调用了几次」来确认轮转行为，只断言状态码与调用次数无法解释
// 因果，所以同时断言账号的冷却台账与模型指标。

// sseServer 起一个假上游：按 handler 返回响应，并记录每次请求。
func sseServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, attempt int)) (*httptest.Server, func() int) {
	t.Helper()
	var (
		mu       sync.Mutex
		attempts int
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		handler(w, r, n)
	}))
	t.Cleanup(ts.Close)
	return ts, func() int {
		mu.Lock()
		defer mu.Unlock()
		return attempts
	}
}

// chatRequest 构造一个最小 chat 请求体。
func chatRequest(t *testing.T, stream bool) *strings.Reader {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    "deepseek-v4.1-flash",
		"stream":   stream,
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
	})
	return strings.NewReader(string(body))
}

// writeSSE 往响应里写一帧。
func writeSSE(w http.ResponseWriter, frame string) {
	_, _ = io.WriteString(w, "data: "+frame+"\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// withFakeUpstream 把国内站上游指向假服务器。
func withFakeUpstream(t *testing.T, url string) {
	t.Helper()
	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base = url
	upstream.ProfileCN.Origin = url
	t.Cleanup(func() {
		upstream.ProfileCN.Base = oldBase
		upstream.ProfileCN.Origin = oldOrigin
	})
}

// TestStreamErrorFramePenalizesAccount 覆盖「200 + error 帧」：账号要被按分类处置
// （6004 → 模型级冷却），且**不计成功**。
//
// 修复前这条流会被当成空流收尾：账号既没被冷却（下轮选号还会选它），
// 指标上也算一次失败——但会话粘性仍可能把它钉住，于是整段对话反复撞限流。
func TestStreamErrorFramePenalizesAccount(t *testing.T) {
	ts, attempts := sseServer(t, func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 上游真实形态：HTTP 200 先开流，随后用 error 帧报错。
		writeSSE(w, `{"error":{"code":6004,"message":"model rate limit, reset at 2026-10-05 10:00:00"}}`)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	withFakeUpstream(t, ts.URL)
	srv := newTestServer(t, ts.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chatRequest(t, true))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)

	if got := attempts(); got != 1 {
		t.Fatalf("流已开流后不该换号，上游调用次数=%d", got)
	}
	// 客户端应拿到上游 error 帧原文（透明透传，不伪造 [DONE]）
	if !strings.Contains(rec.Body.String(), "model rate limit") {
		t.Errorf("error 帧没有透传给客户端：%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("失败流不该补发 [DONE]：%s", rec.Body.String())
	}

	acc := srv.pool.Accounts()[0] // 池里只有测试凭据这一个账号
	if _, ok := acc.ModelCooldowns()["deepseek-v4.1-flash"]; !ok {
		t.Errorf("6004 错误帧应让该账号的该模型进入冷却，实际冷却台账=%v", acc.ModelCooldowns())
	}

	// 指标：记失败、不记成功
	for _, snap := range srv.metrics.Snapshot() {
		if snap.Model != "deepseek-v4.1-flash" {
			continue
		}
		if snap.Success != 0 {
			t.Errorf("error 帧流被记成了成功：success=%d", snap.Success)
		}
		if snap.Failures == 0 {
			t.Errorf("error 帧流没有记失败")
		}
	}
}

// TestUpstreamTimeoutStopsRotation 覆盖上游超时：不轮转、不罚号，返回可区分的 503。
//
// 超时不是账号的问题 —— 同一份请求换到别的号撞上的是同一个慢上游，
// 换号只会把客户端拖到 maxTries × header_timeout，并给一串健康号喂失败计数。
func TestUpstreamTimeoutStopsRotation(t *testing.T) {
	ts, attempts := sseServer(t, func(w http.ResponseWriter, r *http.Request, attempt int) {
		// 慢到触发 ResponseHeaderTimeout（下面把上限压到 80ms）
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"choices":[{"index":0,"delta":{"content":"迟"},"finish_reason":"stop"}]}`)
	})
	withFakeUpstream(t, ts.URL)
	srv := newTestServer(t, ts.URL)

	// 把「请求发出 → 收到响应头」的上限压到 80ms（默认 5 分钟，测试里等不起）。
	if tr, ok := srv.client.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = 80 * time.Millisecond
	} else {
		t.Fatalf("测试前提不成立：ChatHTTP 用的不是 *http.Transport")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chatRequest(t, false))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)

	if got := attempts(); got != 1 {
		t.Fatalf("超时后应停止轮换（只打一次上游），实际 %d 次", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码=%d，期望 503，body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream_timeout") {
		t.Errorf("错误码应能区分超时与「没有可用账号」：%s", rec.Body.String())
	}

	// 不罚号：既没有冷却，失败计数也不该把账号打出健康池。
	acc := srv.pool.Accounts()[0] // 池里只有测试凭据这一个账号
	if n := len(acc.ModelCooldowns()); n != 0 {
		t.Errorf("超时不该建立模型级冷却，实际 %v", acc.ModelCooldowns())
	}
	if acc.IsDisabled() {
		t.Errorf("超时不该禁用账号")
	}
}

// TestBadParamsFailsFastWithoutRotation 覆盖 11101：请求体被上游拒绝是请求级问题，
// 换号照样被拒 —— 立即 400 透传原文，且不罚账号。
func TestBadParamsFailsFastWithoutRotation(t *testing.T) {
	const upstreamBody = `{"code":11101,"msg":"Unmarshal chat params failed"}`
	ts, attempts := sseServer(t, func(w http.ResponseWriter, r *http.Request, attempt int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, upstreamBody)
	})
	withFakeUpstream(t, ts.URL)
	srv := newTestServer(t, ts.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chatRequest(t, true))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)

	if got := attempts(); got != 1 {
		t.Fatalf("11101 应 fail-fast（只打一次上游），实际 %d 次", got)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码=%d，期望 400，body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Unmarshal chat params failed") {
		t.Errorf("应透传上游原文：%s", rec.Body.String())
	}

	acc := srv.pool.Accounts()[0] // 池里只有测试凭据这一个账号
	if n := len(acc.ModelCooldowns()); n != 0 {
		t.Errorf("请求体问题不该罚账号，实际 %v", acc.ModelCooldowns())
	}
}
