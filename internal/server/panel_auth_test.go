package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// 面板静态资源必须在鉴权之外，数据接口必须在鉴权之内。
//
// 这条守的是一个**死锁**：浏览器导航带不了 Authorization 头，如果 `/panel/`
// 也要鉴权，设了 --api-key 之后面板就完全打不开 —— 返回一段 401 JSON，
// 前端脚本加载不了，连「让用户输入密钥」的那个界面都渲染不出来。
//
// 反过来，`/panel/api/*` 与 `/panel/local/*` 必须继续挡住：
// 前者能改配置、删账号，后者持有读本机数据的能力。
func TestPanelAssetsBypassAuthButAPIsDoNot(t *testing.T) {
	s := newTestServer(t, "")
	const key = "panel-auth-key-9"
	s.config().APIKey = key
	// 注入一个最小面板产物，让静态分支走到 200 而不是「未构建」的 503。
	s.panelFS = fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>panel</title>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
	h := s.Handler()

	cases := []struct {
		path string
		want int
		why  string
	}{
		{"/panel/", http.StatusOK, "面板壳必须能匿名加载，否则输入密钥的界面都出不来"},
		{"/panel", http.StatusFound, "无尾斜杠的重定向同样要放行"},
		{"/panel/index.html", http.StatusOK, "面板 HTML"},
		{"/panel/assets/app.js", http.StatusOK, "面板静态资源"},

		{"/panel/api/config", http.StatusUnauthorized, "数据接口必须鉴权"},
		{"/panel/api/accounts", http.StatusUnauthorized, "数据接口必须鉴权"},
		{"/panel/local/capabilities", http.StatusUnauthorized, "本机代理反代必须鉴权"},
		{"/v1/models", http.StatusUnauthorized, "对话接口必须鉴权"},
	}

	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s：期望 %d，实际 %d（%s）", tc.path, tc.want, rec.Code, tc.why)
		}
	}

	// 带上密钥后，数据接口应当放行。
	req := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("带对密钥访问 /panel/api/config 应 200，实际 %d", rec.Code)
	}
}

// 未配置密钥时一切照旧放行（不能因为这次改动引入新的拦截）。
func TestNoAPIKeyKeepsEverythingOpen(t *testing.T) {
	s := newTestServer(t, "")
	if s.config().APIKey != "" {
		t.Fatal("测试前提：默认不应配置密钥")
	}
	s.panelFS = fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}
	h := s.Handler()
	for _, path := range []string{"/panel/", "/panel/api/config", "/v1/models"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s：未配置密钥时不应 401", path)
		}
	}
}
