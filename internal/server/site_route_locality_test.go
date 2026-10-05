package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"workbuddy-gateway/internal/auth"
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

// 本文件锁住站点路由最核心的一条不变式：**换号只在同站内进行**。
//
// 为什么它值得一个专门的端到端测试，而不是靠「代码看起来是对的」：
//
// 站点路由的解析层（resolveSiteRoute）只负责算出「这次该走哪一站」，真正让这条
// 约束生效的是调度循环里每一轮 `PickAccount` 都带上 `Site`。这两处相隔几十行，
// 且中间夹着错误分类、降级重试、粘性账号等一堆分支。任何一次重构只要有一轮重试
// 忘了传 `Site`，症状就是「国内站账号不够时会偷偷用国际站账号把请求发出去」——
// 请求成功、日志正常、账单在另一个站上产生。这类 bug 不会自己暴露出来，
// 只有测试能拦住。

// testServerWithSites 构造一个拥有「指定站点组合」的测试 Server。
//
// counts 形如 {"cn": 2, "intl": 1}：为每个站点造出对应数量的凭据文件。
// 站点由凭据的 edition 决定（见 auth.ResolveSite），因此这里直接把 edition
// 写成站点值 —— 比去编造 realm/domain 组合更直白。
func testServerWithSites(t *testing.T, upstreamURL string, counts map[string]int) *Server {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for site, n := range counts {
		for i := 0; i < n; i++ {
			cred := fmt.Sprintf(
				`{"auth":{"accessToken":"TOKEN_%s_%d","refreshToken":"R","expiresAt":4102444800},`+
					`"account":{"uid":"%s-%d","nickname":"%s账号%d"},"edition":%q}`,
				site, i, site, i, auth.SiteLabel(site), i, site)
			name := fmt.Sprintf("workbuddy-%s-%d.json", site, i)
			if err := os.WriteFile(filepath.Join(authDir, name), []byte(cred), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	cfg, err := config.Load(&config.Config{WorkDir: dir, AuthDir: authDir})
	if err != nil {
		t.Fatalf("构建配置失败: %v", err)
	}
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

	_ = upstreamURL
	return New(cfg, p, client, cat, registry, sticky, sched, events, taskMgr, statStore, nil, nil)
}

// TestRetryStaysWithinForcedSite 端到端验证「指定站点后，换号绝不跨站」。
//
// 场景：池子里有 2 个国内站账号、1 个国际站账号，且**国际站那条上游是健康的**。
// 两个国内站账号都返回「余额不足」（可换号类错误），于是调度循环会依次试完国内站
// 账号。此时期望的结果是**失败**（503），而不是「悄悄用国际站账号把请求发出去」。
//
// 国际站假上游里挂了 t.Error：它被调用本身就是测试失败 —— 这比事后断言
// 「上游收到过几次请求」更直接，也把「跨站」这个错误行为钉死在发生的那一行。
func TestRetryStaysWithinForcedSite(t *testing.T) {
	var (
		mu       sync.Mutex
		cnCalls  []string
		intlCall int
	)

	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 记录这个国内站账号的令牌，用于断言「确实逐个试过国内站账号」。
		mu.Lock()
		cnCalls = append(cnCalls, r.Header.Get("Authorization"))
		mu.Unlock()
		// 余额不足：属于「可换号」类错误，调度会继续试下一个账号。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"insufficient balance","type":"insufficient_quota"}}`))
	}))
	defer cn.Close()

	intl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		intlCall++
		mu.Unlock()
		t.Error("指定了国内站，请求却被发到了国际站 —— 换号跨站了")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer intl.Close()

	oldCNBase, oldCNOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	oldINBase, oldINOrigin := upstream.ProfileINTL.Base, upstream.ProfileINTL.Origin
	upstream.ProfileCN.Base, upstream.ProfileCN.Origin = cn.URL, cn.URL
	upstream.ProfileINTL.Base, upstream.ProfileINTL.Origin = intl.URL, intl.URL
	defer func() {
		upstream.ProfileCN.Base, upstream.ProfileCN.Origin = oldCNBase, oldCNOrigin
		upstream.ProfileINTL.Base, upstream.ProfileINTL.Origin = oldINBase, oldINOrigin
	}()

	srv := testServerWithSites(t, cn.URL, map[string]int{auth.SiteCN: 2, auth.SiteINTL: 1})

	payload, _ := json.Marshal(map[string]any{
		"model":    "CN-deepseek-v4.1-flash",
		"messages": []any{map[string]any{"role": "user", "content": "你好"}},
		"stream":   false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)

	// 两个国内站账号都余额不足 → 必须失败，不能靠国际站兜底。
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("国内站账号全部失败时期望 503，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	gotCN := len(cnCalls)
	gotINTL := intlCall
	mu.Unlock()

	// 两个国内站账号都应被尝试过（换号确实发生了，只是没跨站）。
	if gotCN != 2 {
		t.Errorf("期望试过 2 个国内站账号，实际 %d 次国内站上游请求", gotCN)
	}
	if gotINTL != 0 {
		t.Errorf("国际站上游被调用了 %d 次 —— 指定国内站时绝不能跨站", gotINTL)
	}
}

// TestUnforcedRouteMayUseBothSites 是上一条测试的对照组。
//
// 不指定站点时，调度**允许**在两个站之间选号（这正是默认调度的语义）。
// 有了这个对照，才说明上一条测试拦住的确实是「强制站点时跨站」，
// 而不是「调度器只会用一个站」这种把功能测没了的假阳性。
func TestUnforcedRouteMayUseBothSites(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	// 国内站账号不可用，国际站可用 —— 未指定站点时应落到国际站。
	cn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"insufficient balance"}}`))
	}))
	defer cn.Close()

	intl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"deepseek-v4.1-flash",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"好"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
	}))
	defer intl.Close()

	oldCNBase, oldCNOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	oldINBase, oldINOrigin := upstream.ProfileINTL.Base, upstream.ProfileINTL.Origin
	upstream.ProfileCN.Base, upstream.ProfileCN.Origin = cn.URL, cn.URL
	upstream.ProfileINTL.Base, upstream.ProfileINTL.Origin = intl.URL, intl.URL
	defer func() {
		upstream.ProfileCN.Base, upstream.ProfileCN.Origin = oldCNBase, oldCNOrigin
		upstream.ProfileINTL.Base, upstream.ProfileINTL.Origin = oldINBase, oldINOrigin
	}()

	srv := testServerWithSites(t, cn.URL, map[string]int{auth.SiteCN: 1, auth.SiteINTL: 1})

	payload, _ := json.Marshal(map[string]any{
		"model":    "deepseek-v4.1-flash", // 无前缀 → 未指定站点
		"messages": []any{map[string]any{"role": "user", "content": "只回答一个字：好"}},
		"stream":   false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	srv.handleChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("未指定站点时期望落到可用的国际站并返回 200，实际 %d，body=%s",
			rec.Code, rec.Body.String())
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got == 0 {
		t.Error("未指定站点时调度应能跨站选号，实际国际站一次都没被用到")
	}
}
