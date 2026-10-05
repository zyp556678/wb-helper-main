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

// TestSitePrefixIsStrippedBeforeUpstream 端到端验证「模型名前缀在出站前被剥掉」。
//
// 为什么必须端到端测而不是只测 resolveSiteRoute：解析层返回的 route.Model 是对的，
// 不代表发给上游的 body 里就是对的 —— 中间还隔着「写回 reqObj」「marshal」
// 「prepareOutboundBody 出站改写」三步，任何一步漏掉前缀都会让上游收到
// `AI-deepseek-v4.1-flash` 这种它不认识的模型名，表现为 404「模型不存在」。
// 这个症状与站点路由本身毫无字面联系，是最容易排查错方向的一类问题。
//
// 同时验证前缀所在的客户端 body 字段被改写成了裸名，避免调用方从响应里
// 看到自己没传过的模型名而产生困惑。
func TestSitePrefixIsStrippedBeforeUpstream(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"deepseek-v4.1-flash",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"好"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
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
		"model":    "CN-deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "只回答一个字：好"}},
		"stream":   false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()

	if len(got) != 1 {
		t.Fatalf("期望 1 次上游请求，实际 %d 次", len(got))
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(got[0]), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v", err)
	}
	if model, _ := sent["model"].(string); model != "deepseek-v4.1-flash" {
		t.Errorf("发往上游的 model 应为裸名 deepseek-v4.1-flash，实际 %q", model)
	}
	if strings.Contains(got[0], "CN-deepseek-v4.1-flash") {
		t.Errorf("上游请求体里仍带着站点前缀: %s", got[0])
	}
}

// TestForcedSiteWithoutAccountsFailsLoudly 端到端验证「指定了没有账号的站点」的报错口径。
//
// 测试凭据的 edition 是 cn，因此池子里只有国内站账号、没有国际站账号 ——
// 正好用来覆盖这条分支。
//
// 断言的关键是**错误码与文案**：调用方指定站点时最需要知道的不是「没有可用账号」
// （这句话会让人去翻账号池，而账号池其实是好的），而是「你指的这一站一个账号都没有」。
// 并且必须明确告知不会自动改用另一站 —— 否则用户会以为请求退到了别处而在
// 排查方向上一路跑偏。
func TestForcedSiteWithoutAccountsFailsLoudly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("指定了没有账号的站点，不该真的发出上游请求")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	srv := newTestServer(t, ts.URL)

	payload, _ := json.Marshal(map[string]any{
		"model":    "AI-deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
		"stream":   false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("期望 503，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	var errObj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errObj); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %v", err)
	}
	inner, _ := errObj["error"].(map[string]any)
	// 注意信封结构：writeOpenAIError 把符号码放在 `type`，`code` 是数字 HTTP 状态。
	// （与 OpenAI 官方一致：code 为状态码，type 为机读的分类字符串。）
	if typ, _ := inner["type"].(string); typ != "no_account_for_site" {
		t.Errorf("期望错误分类 no_account_for_site，实际 %q，body=%s", typ, rec.Body.String())
	}
	msg, _ := inner["message"].(string)
	if !strings.Contains(msg, "国际站") {
		t.Errorf("报错文案应指明是哪一站，实际 %q", msg)
	}
	// 必须告知「来源」，否则用户不知道是自己哪个写法触发的。
	if !strings.Contains(msg, "模型名前缀") {
		t.Errorf("报错文案应说明站点指定的来源，实际 %q", msg)
	}
}

// TestQuerySiteOverridesPrefixAndStripsIt 端到端验证「URL 参数优先于前缀，且前缀仍被剥掉」。
//
// 这是解析层最容易写错的一处：早期实现在 query/header 命中时直接返回了原始模型名，
// 于是 `?site=cn` + `AI-xxx` 会把 `AI-xxx` 整串发给上游。单测已覆盖解析层，
// 这里再验一次它在真实请求链路上同样成立。
func TestQuerySiteOverridesPrefixAndStripsIt(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"deepseek-v4.1-flash",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"好"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
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

	// 模型名带 AI- 前缀，但 URL 参数强制走国内站（池子里只有国内站账号）。
	payload, _ := json.Marshal(map[string]any{
		"model":    "AI-deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "只回答一个字：好"}},
		"stream":   false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?site=cn", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200（参数覆盖前缀后走国内站），实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()

	if len(got) != 1 {
		t.Fatalf("期望 1 次上游请求，实际 %d 次", len(got))
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(got[0]), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v", err)
	}
	if model, _ := sent["model"].(string); model != "deepseek-v4.1-flash" {
		t.Errorf("发往上游的 model 应为裸名，实际 %q", model)
	}
}
