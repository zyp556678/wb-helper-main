package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// okHandler 记录中间件是否把请求放行到了业务处理。
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

// TestCORSIsLimitedToModelAPI 守住「跨域只对 /v1/* 放开」。
//
// 回归背景：这里曾经对**所有**路由设 `Access-Control-Allow-Origin: *`，而面板接口
// 与模型接口共用一个端口 —— 于是任何网站都能向 http://127.0.0.1:8317 发带
// Authorization 头的跨域请求并读到响应，唯一的门槛只剩密钥本身。
func TestCORSIsLimitedToModelAPI(t *testing.T) {
	srv := &Server{}
	reached := false
	handler := srv.cors(okHandler(&reached))

	cases := []struct {
		path      string
		wantAllow bool
	}{
		{"/v1/chat/completions", true},
		{"/v1", true},
		{"/panel/api/accounts", false},
		{"/panel/", false},
		{"/status", false},
		{"/healthz", false},
	}
	for _, c := range cases {
		reached = false
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))

		got := rec.Header().Get("Access-Control-Allow-Origin")
		if c.wantAllow && got != "*" {
			t.Errorf("%s：应放开跨域，实际 Allow-Origin=%q", c.path, got)
		}
		if !c.wantAllow && got != "" {
			t.Errorf("%s：不该放开跨域，实际 Allow-Origin=%q", c.path, got)
		}
		if !reached {
			t.Errorf("%s：请求应被放行到业务处理", c.path)
		}
	}
}

// TestCORSPreflightOnlyForModelAPI 预检只对模型接口短路。
func TestCORSPreflightOnlyForModelAPI(t *testing.T) {
	srv := &Server{}
	reached := false
	handler := srv.cors(okHandler(&reached))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil))
	if rec.Code != http.StatusOK || reached {
		t.Errorf("模型接口的 OPTIONS 应由中间件直接应答，实际 code=%d reached=%v", rec.Code, reached)
	}

	reached = false
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/panel/api/accounts", nil))
	if !reached {
		t.Error("面板接口的 OPTIONS 不该被 CORS 中间件吃掉，应交给下游（面板鉴权）处理")
	}
}

// TestPanelSecurityRejectsCrossOrigin 守住同源校验。
//
// 这一层防的是 DNS rebinding：攻击者把自己的域名解析到 127.0.0.1，浏览器就会把
// 他的页面当成与我们同源，同源策略此时帮不上忙。Origin / Sec-Fetch-Site 是浏览器
// 自己填的、页面脚本无法伪造的信号。
func TestPanelSecurityRejectsCrossOrigin(t *testing.T) {
	srv := &Server{}

	cases := []struct {
		name         string
		path         string
		origin       string
		secFetchSite string
		wantBlocked  bool
	}{
		{"无 Origin（命令行/同源导航）", "/panel/api/accounts", "", "", false},
		{"同源 Origin", "/panel/api/accounts", "http://127.0.0.1:8317", "", false},
		{"同源但大小写不同", "/panel/api/accounts", "http://127.0.0.1:8317", "same-origin", false},
		{"跨站 Origin", "/panel/api/accounts", "http://evil.example", "", true},
		{"端口不同的 Origin", "/panel/api/accounts", "http://127.0.0.1:9999", "", true},
		{"带 userinfo 的伪造 Origin", "/panel/api/accounts", "http://127.0.0.1:8317@evil.example", "", true},
		{"带路径的畸形 Origin", "/panel/api/accounts", "http://127.0.0.1:8317/x", "", true},
		{"非 http(s) 协议", "/panel/api/accounts", "file://127.0.0.1:8317", "", true},
		{"Sec-Fetch-Site 跨站", "/panel/api/accounts", "", "cross-site", true},
		{"面板页面本身也受保护", "/panel/", "http://evil.example", "", true},
		{"根路径也算管理面", "/", "http://evil.example", "", true},
		{"模型接口不受影响", "/v1/chat/completions", "http://evil.example", "cross-site", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached := false
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if c.secFetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", c.secFetchSite)
			}
			req.Host = "127.0.0.1:8317"

			srv.panelSecurity(okHandler(&reached)).ServeHTTP(rec, req)

			if c.wantBlocked {
				if rec.Code != http.StatusForbidden {
					t.Errorf("应拒绝（403），实际 code=%d", rec.Code)
				}
				if reached {
					t.Error("被拒绝的请求不该到达业务处理")
				}
				if !strings.Contains(rec.Body.String(), "403") {
					t.Errorf("拒绝响应应带错误码，实际 body=%s", rec.Body.String())
				}
				// 拒绝的响应同样要带上安全头。
				if rec.Header().Get("Cache-Control") != "no-store" {
					t.Error("被拒绝的响应也应带 no-store")
				}
			} else {
				if rec.Code != http.StatusOK || !reached {
					t.Errorf("应放行，实际 code=%d reached=%v", rec.Code, reached)
				}
			}
		})
	}
}

// TestPanelSecurityHeadersOnAdminPaths 守住安全头只加在管理面上。
//
// 面板页面会把接入密钥显示给用户，因此不能被缓存、不能被别的站点嵌进 iframe。
func TestPanelSecurityHeadersOnAdminPaths(t *testing.T) {
	srv := &Server{}

	for _, path := range []string{"/", "/panel", "/panel/", "/panel/api/config"} {
		reached := false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1:8317"
		srv.panelSecurity(okHandler(&reached)).ServeHTTP(rec, req)

		h := rec.Header()
		for _, key := range []string{"Cache-Control", "X-Content-Type-Options", "Referrer-Policy", "Content-Security-Policy"} {
			if h.Get(key) == "" {
				t.Errorf("%s：缺少安全头 %s", path, key)
			}
		}
		if h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s：Cache-Control 应为 no-store，实际 %q", path, h.Get("Cache-Control"))
		}
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s：CSP 应禁止被内嵌，实际 %q", path, csp)
		}
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "'unsafe-inline'; script") {
			t.Errorf("%s：CSP 的 script-src 不该放开内联，实际 %q", path, csp)
		}
	}

	// 模型接口不该被加上这些头：它是给外部客户端调的，缓存与嵌入限制没有意义。
	reached := false
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Host = "127.0.0.1:8317"
	srv.panelSecurity(okHandler(&reached)).ServeHTTP(rec, req)
	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Error("模型接口不该带管理面的 CSP")
	}
	if !reached {
		t.Error("模型接口应被放行")
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		origin string
		host   string
		want   bool
	}{
		{"http://127.0.0.1:8317", "127.0.0.1:8317", true},
		{"https://127.0.0.1:8317", "127.0.0.1:8317", true},
		{"http://LOCALHOST:8317", "localhost:8317", true},
		{"http://127.0.0.1:8317", "127.0.0.1:9999", false},
		{"http://127.0.0.1:8317@evil.example", "127.0.0.1:8317", false},
		{"http://evil.example", "127.0.0.1:8317", false},
		{"http://127.0.0.1:8317/panel", "127.0.0.1:8317", false},
		{"http://127.0.0.1:8317?a=1", "127.0.0.1:8317", false},
		{"file://127.0.0.1:8317", "127.0.0.1:8317", false},
		{"://坏掉的", "127.0.0.1:8317", false},
	}
	for _, c := range cases {
		if got := sameOrigin(c.origin, c.host); got != c.want {
			t.Errorf("sameOrigin(%q, %q) = %v，期望 %v", c.origin, c.host, got, c.want)
		}
	}
}
