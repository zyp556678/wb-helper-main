package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// 面板静态资源服务的 MIME 与缓存语义。
//
// 为什么值得测：wb-switch 踩过一个很贵的坑 —— 前端路由回退时
// **按请求路径**算 Content-Type，于是 `/panel/some/route` 得到
// `application/octet-stream`，浏览器把页面当文件下载，**整站打不开**。
// 本项目的回退路径目前是**对的**（`serveEmbeddedIndex` 硬编码 text/html），
// 但没有任何断言兜住它；一旦有人「顺手改成按请求路径算 MIME」就会原样复现。
// 这条测试就是那个断言。
func panelTestFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":      &fstest.MapFile{Data: []byte("<!doctype html><div id=root>")},
		"assets/main.js":  &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/main.css": &fstest.MapFile{Data: []byte("body{}")},
	}
}

func newPanelServer(t *testing.T, withPanel bool) *Server {
	t.Helper()
	srv := newTestServer(t, "")
	if withPanel {
		srv.panelFS = panelTestFS()
	} else {
		srv.panelFS = nil
	}
	return srv
}

func servePanel(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handlePanelStatic(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// 根路径返回 index.html，且必须是 text/html。
func TestPanelStaticRootServesHTML(t *testing.T) {
	srv := newPanelServer(t, true)
	rec := servePanel(t, srv, "/panel/")

	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("index.html 的 Content-Type 应为 text/html，实际 %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "id=root") {
		t.Fatalf("返回的不是 index.html: %q", rec.Body.String())
	}
}

// **核心断言**：命中不到文件时的回退，必须是 text/html。
//
// 用「无扩展名的深层路径」与「未知扩展名」两种输入来逼出这个行为：
// 若实现改成按请求路径算 MIME，前者会得到 octet-stream、后者也会，
// 浏览器于是把页面当文件下载 —— 这个测试就会红。
func TestPanelStaticFallbackAlwaysHTML(t *testing.T) {
	srv := newPanelServer(t, true)

	for _, path := range []string{
		"/panel/accounts",          // 前端路由（无扩展名）
		"/panel/a/b/c",             // 深层前端路由
		"/panel/route.with.dot",    // 带点但非已知扩展名
		"/panel/whatever.octet",    // 明确会被误判成 octet-stream 的扩展名
		"/panel/路由",                // 非 ASCII 路径
		"/panel/assets/missing.js", // assets 下但文件不存在
	} {
		t.Run(path, func(t *testing.T) {
			rec := servePanel(t, srv, path)
			if rec.Code != http.StatusOK {
				t.Fatalf("HTTP %d", rec.Code)
			}
			ct := rec.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, "text/html") {
				t.Fatalf("回退路径 %q 的 Content-Type 必须是 text/html（否则浏览器会下载页面），实际 %q",
					path, ct)
			}
			if strings.Contains(ct, "octet-stream") {
				t.Fatalf("回退路径 %q 返回了 octet-stream —— 这正是 wb-switch 那个坑", path)
			}
			if !strings.Contains(rec.Body.String(), "id=root") {
				t.Fatalf("回退时返回的不是 index.html: %q", rec.Body.String())
			}
		})
	}
}

// 真实存在的静态文件要按扩展名给正确的 MIME，并且带内容哈希的产物可长缓存。
func TestPanelStaticAssetsMIMEAndCache(t *testing.T) {
	srv := newPanelServer(t, true)

	cases := []struct {
		path      string
		wantCT    string
		immutable bool
	}{
		{"/panel/assets/main.js", "javascript", true},
		{"/panel/assets/main.css", "text/css", true},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec := servePanel(t, srv, c.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("HTTP %d", rec.Code)
			}
			ct := rec.Header().Get("Content-Type")
			if !strings.Contains(ct, c.wantCT) {
				t.Fatalf("Content-Type 应含 %q，实际 %q", c.wantCT, ct)
			}
			cc := rec.Header().Get("Cache-Control")
			if c.immutable && !strings.Contains(cc, "immutable") {
				t.Fatalf("assets/ 下的产物应可长缓存，实际 Cache-Control=%q", cc)
			}
		})
	}
}

// index.html 必须 no-cache：否则升级后浏览器会拿旧壳去请求已被替换的产物哈希，
// 表现是「升级完页面白屏，强刷才好」。
func TestPanelStaticIndexIsNotCached(t *testing.T) {
	srv := newPanelServer(t, true)

	// 直接命中 index.html 与回退两条路径都要 no-cache。
	for _, path := range []string{"/panel/", "/panel/index.html", "/panel/deep/route"} {
		rec := servePanel(t, srv, path)
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
			t.Fatalf("%q 的 Cache-Control 应为 no-cache，实际 %q", path, cc)
		}
	}
}

// 面板未构建时给出可操作的 503，而不是 500 或空响应。
func TestPanelStaticWithoutPanelReturns503(t *testing.T) {
	srv := newPanelServer(t, false)
	rec := servePanel(t, srv, "/panel/")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("面板未构建时应返回 503，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "npm run build") {
		t.Fatalf("错误信息应告诉用户怎么修，实际 %q", rec.Body.String())
	}
}
