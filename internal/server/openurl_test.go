package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 「在浏览器中打开授权链接」这条路的护栏。
//
// 桌面壳里 window.open 会被 WebView 静默拦掉，所以面板会退回这个接口 ——
// 于是它必须**只**对两类请求生效：来自本机的、且目标是 http/https 的。
// 远程部署时在服务器上弹浏览器毫无意义（而且很危险），必须拒绝。
func TestPanelOpenURLGuards(t *testing.T) {
	s := newTestServer(t, "")
	const key = "open-url-key-7"
	s.config().APIKey = key
	h := s.Handler()

	var opened []string
	oldOpen := openInSystemBrowser
	openInSystemBrowser = func(url string) error {
		opened = append(opened, url)
		return nil
	}
	t.Cleanup(func() { openInSystemBrowser = oldOpen })

	do := func(method, remoteAddr, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/panel/api/open-url", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		if remoteAddr != "" {
			req.RemoteAddr = remoteAddr
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 非回环（例如面板在远端浏览器里打开）：拒绝，并且**不能**真的去开浏览器。
	rec := do(http.MethodPost, "203.0.113.9:5000", `{"url":"https://www.workbuddy.ai/login?state=x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非本机请求应 403，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if len(opened) != 0 {
		t.Fatalf("非本机请求绝不能触发打开浏览器，实际打开了 %v", opened)
	}

	// 协议白名单：file:// 之类一律拒绝（URL 来自网络，不能当可信输入）。
	for _, bad := range []string{
		`{"url":"file:///C:/Windows/System32/calc.exe"}`,
		`{"url":"javascript:alert(1)"}`,
		`{"url":"ftp://example.com/x"}`,
		`{"url":"/relative/path"}`,
		`{"url":""}`,
	} {
		rec := do(http.MethodPost, "127.0.0.1:5555", bad)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d", bad, rec.Code)
		}
	}
	if len(opened) != 0 {
		t.Fatalf("被拒绝的链接不能交给系统，实际 %v", opened)
	}

	// 方法白名单。
	if rec := do(http.MethodGet, "127.0.0.1:5555", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405，实际 %d", rec.Code)
	}

	// 正常路径：回环 + https → 交给系统默认浏览器。
	want := "https://www.workbuddy.ai/login?platform=workbuddy-ai&state=abc"
	rec = do(http.MethodPost, "127.0.0.1:5555", `{"url":"`+want+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("本机 https 链接应 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if len(opened) != 1 || opened[0] != want {
		t.Fatalf("应把原样的链接交给系统，实际 %v", opened)
	}

	// IPv6 回环同样算本机。
	rec = do(http.MethodPost, "[::1]:5555", `{"url":"http://127.0.0.1:8317/panel/"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("IPv6 回环应 200，实际 %d", rec.Code)
	}
}
