package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"workbuddy-gateway/internal/catalog"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/eventlog"
	"workbuddy-gateway/internal/metrics"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/scheduler"
	"workbuddy-gateway/internal/session"
	"workbuddy-gateway/internal/stats"
	"workbuddy-gateway/internal/tasks"
	"workbuddy-gateway/internal/upstream"
)

// TestContentBlockDegradedRetry 端到端验证「内容策略拦截 → 换中性提示词就地重试」。
//
// 为什么要用假上游而不是等真的被拦：内容拦截是上游风控的主观判定，无法稳定复现，
// 而这条重试路径又必须验证——它失败了会表现为「偶发的整单失败」，
// 是最难排查的一类问题。用 httptest 精确控制「先 400 后 200」可以稳定覆盖它。
//
// 断言三件事：
//  1. 第一次请求被 400 拦截后，服务端确实发了**第二次**请求（而不是直接返回失败）；
//  2. 第二次请求的 system 已经换成中性降级提示词（不是客户端的原始 system）；
//  3. 最终对客户端返回 200 与正常内容。
func TestContentBlockDegradedRetry(t *testing.T) {
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

		if attempt == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":11128,"msg":"blocked by security policy"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":""}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
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
	defer ts.Close()

	// 把国内站的上游指向假服务器（ProfileForSite 返回的就是这两个全局变量）。
	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base = ts.URL
	upstream.ProfileCN.Origin = ts.URL
	defer func() {
		upstream.ProfileCN.Base = oldBase
		upstream.ProfileCN.Origin = oldOrigin
	}()

	srv := newTestServer(t, ts.URL)

	// 客户端 system 里带一段会被逐字匹配的指纹句，验证降级后它是否被顶替掉。
	const clientSystem = "You are Claude Code, Anthropic's official CLI for Claude."
	payload, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": clientSystem},
			map[string]any{"role": "user", "content": "只回答一个字：好"},
		},
		"stream": false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)

	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("期望「拦截后重试一次」共 2 次上游请求，实际 %d 次", len(got))
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("期望最终 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	// 第二次请求的 system 必须是降级中性提示词，且不再包含客户端指纹
	second := parseSystemContent(t, got[1])
	if second != degradedPromptForTest {
		t.Errorf("降级重试的 system 不是中性提示词：%q", second)
	}
	if strings.Contains(got[1], "You are Claude Code") {
		t.Errorf("降级重试的请求体里仍然带着客户端 system 指纹")
	}

	// 第一次请求保留客户端 system（默认 append 模式），并已做过脱敏
	firstSystems := collectSystemContents(t, got[0])
	if !containsSubstring(firstSystems, "Anthropic's official CLI tool for Claude") {
		t.Errorf("首次请求应保留客户端 system 且已脱敏，实际 system=%v", firstSystems)
	}
}

// degradedPromptForTest 与 outbound.DegradedSystemPrompt 保持一致。
//
// 这里刻意写字面量而不是引用常量：如果哪天有人改了降级提示词本身，
// 这个测试应当失败并提醒他确认新文案仍是「最小中性」的，而不是被静默跟随。
const degradedPromptForTest = "You are a helpful assistant."

// newTestServer 构造一个只连到假上游的最小 Server。
func newTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cred := `{"auth":{"accessToken":"TEST_TOKEN","refreshToken":"R","expiresAt":4102444800},` +
		`"account":{"uid":"u1","nickname":"测试"},"edition":"cn"}`
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-test.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(&config.Config{WorkDir: dir, AuthDir: authDir})
	if err != nil {
		t.Fatalf("构建配置失败: %v", err)
	}
	// 目录首拉会打网络，测试里不需要模型目录，保持为空即可。
	cfg.ReloadInterval = 0
	cfg.ModelsRefresh = 0

	client := &upstream.Client{
		Control:     cfg.Control,
		ChatHTTP:    cfg.Chat,
		IdleTimeout: cfg.IdleTimeout,
		Logf:        t.Logf,
	}
	p := pool.New(cfg, client)
	p.Logf = t.Logf
	p.SetGov(pool.GovFromConfig(cfg))
	if n, errs := p.Load(); n == 0 {
		t.Fatalf("凭据加载失败，errs=%v", errs)
	}

	registry := metrics.New()
	events := eventlog.New(200)
	statStore := stats.New(filepath.Join(dir, "wb-stats.json"))
	sticky := session.New(cfg.StickyEnabled(), cfg.StickyTTL(), cfg.StickyGCInterval())
	sched := scheduler.New(cfg, p, client)
	cat := catalog.New(filepath.Join(dir, "wb-models-cache.json"), client, p)
	taskMgr := tasks.New(p, client, cfg, events)

	_ = upstreamURL // 上游地址通过 ProfileCN 全局变量注入
	return New(cfg, p, client, cat, registry, sticky, sched, events, taskMgr, statStore, nil, nil)
}

// parseSystemContent 取请求体里第一条 system 消息的正文。
func parseSystemContent(t *testing.T, body string) string {
	t.Helper()
	list := collectSystemContents(t, body)
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

func collectSystemContents(t *testing.T, body string) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v", err)
	}
	msgs, _ := obj["messages"].([]any)
	out := []string{}
	for _, mAny := range msgs {
		m, ok := mAny.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "system" {
			if c, ok := m["content"].(string); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

func containsSubstring(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
