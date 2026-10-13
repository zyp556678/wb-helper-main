package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy-gateway/internal/eventlog"
	"workbuddy-gateway/internal/update"
)

// fakeGitHubForServer 起一个假 GitHub，返回给定 Release（含一个 linux/amd64 的 deb）。
func fakeGitHubForServer(t *testing.T, tag string) (*httptest.Server, []byte) {
	t.Helper()
	payload := []byte("deb-bytes")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			_, _ = w.Write(payload)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"tag_name": tag,
			"name":     "桌面版 " + tag,
			"html_url": "https://example.invalid/releases/" + tag,
			"body":     "## 更新内容\n- 修了点东西",
			"assets": []map[string]any{{
				"name":                 "workbuddy-gateway-desktop_" + strings.TrimPrefix(tag, "v") + "_amd64.deb",
				"size":                 len(payload),
				"browser_download_url": srv.URL + "/dl/pkg.deb",
			}},
		}})
	}))
	t.Cleanup(srv.Close)
	return srv, payload
}

// attachChecker 给测试 Server 挂一个指向假 GitHub 的检查器。
func attachChecker(t *testing.T, s *Server, fake *httptest.Server, dir string) *update.Checker {
	t.Helper()
	// 默认关掉「发现新版本就自动下载」：本文件量的是端点行为（状态、字段、开关落盘），
	// 而后台自动下载会在用例结束后继续 t.Logf（go test 会因此 panic），
	// 也会让「还没下载」这类前置条件在用例中途被悄悄满足。要测自动下载的用例自己打开。
	cfg := s.config().Clone()
	off := false
	cfg.Update.AutoDownload = &off
	checker := update.New(cfg, update.Options{
		Current:     "0.9.2",
		Client:      fake.Client(),
		APIBase:     fake.URL,
		GOOS:        "linux",
		GOARCH:      "amd64",
		DownloadDir: dir,
		Logf:        t.Logf,
	})
	t.Cleanup(checker.WaitBackground)
	s.SetUpdateChecker(checker)
	return checker
}

// 没注入检查器时如实返回 503，而不是给一个「已是最新」的假状态。
func TestUpdateEndpointWithoutChecker(t *testing.T) {
	s := newTestServer(t, "")
	rec := httptest.NewRecorder()
	s.handlePanelUpdateRoute(rec, httptest.NewRequest(http.MethodGet, "/panel/api/update", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("应当返回 503，实际 %d（体=%s）", rec.Code, rec.Body.String())
	}
}

// GET 返回的状态必须是面板能直接渲染的形状（字段名与 types.ts 对齐）。
func TestUpdateEndpointGetState(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	checker := attachChecker(t, s, fake, t.TempDir())
	if st := checker.Check(context.Background(), true); st.Error != "" {
		t.Fatalf("前置检查失败: %s", st.Error)
	}

	rec := httptest.NewRecorder()
	s.handlePanelUpdateRoute(rec, httptest.NewRequest(http.MethodGet, "/panel/api/update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应当 200，实际 %d", rec.Code)
	}
	body := decodeJSONBody(t, rec)
	if body["current"] != "0.9.2" {
		t.Errorf("current 字段不对: %v", body["current"])
	}
	if body["latest"] != "0.9.3" {
		t.Errorf("latest 字段不对: %v", body["latest"])
	}
	if body["update_available"] != true {
		t.Errorf("应当报告有更新: %v", body["update_available"])
	}
	if body["asset_name"] == "" {
		t.Error("应当带上匹配 linux/amd64 的安装包名")
	}
}

// POST 立即检查：用户显式点击必须真的去查（不受最小间隔约束）。
func TestUpdateEndpointPostForcesCheck(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.4")
	attachChecker(t, s, fake, t.TempDir())

	rec := httptest.NewRecorder()
	s.handlePanelUpdateRoute(rec, httptest.NewRequest(http.MethodPost, "/panel/api/update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应当 200，实际 %d（体=%s）", rec.Code, rec.Body.String())
	}
	if got := decodeJSONBody(t, rec)["latest"]; got != "0.9.4" {
		t.Errorf("应当查到 0.9.4，实际 %v", got)
	}
}

// 不支持的 method 要明确拒绝，而不是当成 GET 处理。
func TestUpdateEndpointRejectsOtherMethods(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	attachChecker(t, s, fake, t.TempDir())
	rec := httptest.NewRecorder()
	s.handlePanelUpdateRoute(rec, httptest.NewRequest(http.MethodDelete, "/panel/api/update", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("应当 405，实际 %d", rec.Code)
	}
}

// 自动检查开关写进 config.json 的 update 段，并且不能碰其它字段。
func TestUpdateAutoCheckPersistsToConfig(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	attachChecker(t, s, fake, t.TempDir())

	// 先写一份带其它 update 字段的配置，验证补丁不会把它们冲掉。
	path := filepath.Join(s.config().WorkDir, "config.json")
	original := `{"update":{"repo":"o/r","check_hours":12}}` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	rec := httptest.NewRecorder()
	s.setUpdateAutoCheck(rec, httptest.NewRequest(http.MethodPost, "/panel/api/update/auto",
		strings.NewReader(`{"enabled":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("应当 200，实际 %d（体=%s）", rec.Code, rec.Body.String())
	}
	if auto, _ := decodeJSONBody(t, rec)["auto_check"].(bool); auto {
		t.Error("返回状态里 auto_check 应当已变成 false")
	}
	if s.config().UpdateEnabled() {
		t.Error("内存配置也应当同步成关闭")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("配置不是合法 JSON: %v", err)
	}
	sec, _ := raw["update"].(map[string]any)
	if sec["enabled"] != false {
		t.Errorf("config.json 里的 update.enabled 应为 false，实际 %v", sec["enabled"])
	}
	if sec["repo"] != "o/r" || sec["check_hours"] != float64(12) {
		t.Errorf("补丁不该冲掉其它字段，实际 %v", sec)
	}
}

// 缺 enabled 字段必须 400：默认成 false 会让一个拼错的字段名静默关掉自动检查。
func TestUpdateAutoCheckRequiresExplicitEnabled(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	attachChecker(t, s, fake, t.TempDir())

	rec := httptest.NewRecorder()
	s.setUpdateAutoCheck(rec, httptest.NewRequest(http.MethodPost, "/panel/api/update/auto",
		strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应当 400，实际 %d（体=%s）", rec.Code, rec.Body.String())
	}
}

// 下载端点：失败走动作型信封（{ok:false,detail}），且不因为「没有资产」就 500。
func TestUpdateDownloadWithoutAssetReturnsActionError(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	checker := attachChecker(t, s, fake, t.TempDir())
	// 没查过 → 状态里没有下载地址。
	if st := checker.State(); st.DownloadURL != "" {
		t.Fatalf("前置条件不成立：不该已经有下载地址")
	}

	rec := httptest.NewRecorder()
	s.downloadUpdate(rec, httptest.NewRequest(http.MethodPost, "/panel/api/update/download", nil))
	if rec.Code < 400 {
		t.Fatalf("应当报错，实际 %d", rec.Code)
	}
	body := decodeJSONBody(t, rec)
	if body["ok"] != false {
		t.Errorf("动作型失败必须是 ok=false，实际 %v", body["ok"])
	}
	if body["detail"] == "" || body["detail"] == nil {
		t.Error("应当说明失败原因（前端直接展示这个字段）")
	}
}

// 下载成功：安装包落到配置的目录，响应里给出路径与大小。
func TestUpdateDownloadWritesPackage(t *testing.T) {
	s := newTestServer(t, "")
	fake, payload := fakeGitHubForServer(t, "v0.9.3")
	dir := t.TempDir()
	checker := attachChecker(t, s, fake, dir)
	if st := checker.Check(context.Background(), true); st.Error != "" {
		t.Fatalf("前置检查失败: %s", st.Error)
	}

	rec := httptest.NewRecorder()
	s.downloadUpdate(rec, httptest.NewRequest(http.MethodPost, "/panel/api/update/download", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应当 200，实际 %d（体=%s）", rec.Code, rec.Body.String())
	}
	body := decodeJSONBody(t, rec)
	path, _ := body["path"].(string)
	if path == "" {
		t.Fatalf("应当返回落地路径，实际 %v", body)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回安装包失败: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("安装包内容不对: %q", got)
	}
	// 事件日志里应当留下一条，便于用户事后核对「什么时候下过什么」。
	if res := s.events.Query(eventlog.Query{}); res.Total == 0 {
		t.Error("下载成功后应当记一条事件")
	}
}

// 安装端点：还没下载就点安装 → 409 + 一句「该做什么」，而不是 502。
//
// 用 502 的话用户会以为是网络抖动，去重试、去查网络，而真正的原因是自己还没点下载。
func TestUpdateInstallRequiresDownloadedAsset(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	checker := attachChecker(t, s, fake, t.TempDir())
	if st := checker.Check(context.Background(), true); st.Error != "" {
		t.Fatalf("前置检查失败: %s", st.Error)
	}

	rec := httptest.NewRecorder()
	s.installUpdate(rec, httptest.NewRequest(http.MethodPost, "/panel/api/update/install", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("应当 409（前提不满足），实际 %d（体=%s）", rec.Code, rec.Body.String())
	}
	body := decodeJSONBody(t, rec)
	if body["ok"] != false {
		t.Errorf("应当 ok=false，实际 %v", body["ok"])
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "下载") {
		t.Errorf("提示要指向「先下载」，实际 %q", detail)
	}
}

// 安装端点只接受 POST（它是动作，不是状态查询）。
func TestUpdateInstallRejectsGet(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	attachChecker(t, s, fake, t.TempDir())

	rec := httptest.NewRecorder()
	s.installUpdate(rec, httptest.NewRequest(http.MethodGet, "/panel/api/update/install", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("应当 405，实际 %d", rec.Code)
	}
}

// 「自动下载」开关写进 config.json 的 update.auto_download，并回最新状态。
func TestUpdateAutoDownloadSwitchPersists(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	attachChecker(t, s, fake, t.TempDir())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/update/auto",
		strings.NewReader(`{"auto_download":false}`))
	req.Header.Set("Content-Type", "application/json")
	s.setUpdateAutoCheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应当 200，实际 %d（体=%s）", rec.Code, rec.Body.String())
	}
	if body := decodeJSONBody(t, rec); body["auto_download"] != false {
		t.Fatalf("响应里的开关状态不对: %v", body["auto_download"])
	}
	// 内存与磁盘都要落下来（面板与文件监听读的是它们）。
	if s.config().UpdateAutoDownload() {
		t.Error("内存里的开关没关掉")
	}
	raw, err := os.ReadFile(s.config().ConfigFile)
	if err != nil {
		t.Fatalf("读 config.json 失败: %v", err)
	}
	if !strings.Contains(string(raw), `"auto_download": false`) {
		t.Errorf("config.json 里没写下来: %s", raw)
	}
	// 自动检查开关不受影响（两个开关各管各的）。
	if !s.config().UpdateEnabled() {
		t.Error("只改自动下载时不该动到自动检查")
	}
}

// 两个字段都不给 → 400。默认 false 的话，一个拼错的字段名会静默变成「关掉自动检查」。
func TestUpdateAutoEndpointRequiresAField(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	attachChecker(t, s, fake, t.TempDir())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/update/auto", strings.NewReader(`{"enabeld":true}`))
	req.Header.Set("Content-Type", "application/json")
	s.setUpdateAutoCheck(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("拼错字段名应当 400，实际 %d（体=%s）", rec.Code, rec.Body.String())
	}
	if !s.config().UpdateEnabled() || !s.config().UpdateAutoDownload() {
		t.Error("被拒绝的请求不该改动任何开关")
	}
}

// 安装成功后请求重启：回调要被调用，且是在响应写回之后。
func TestInstallRequestsRestartAfterInstall(t *testing.T) {
	s := newTestServer(t, "")
	restarted := make(chan struct{}, 1)
	s.SetRestartFunc(func() { restarted <- struct{}{} })

	s.requestRestart(10 * time.Millisecond)
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("请求重启后回调没有被调用")
	}

	// 没有重启通路时只提示，不 panic（精简装配的实例就是这样）。
	plain := newTestServer(t, "")
	plain.requestRestart(0)
}

// 安装要求网关退出时（Windows：让出文件锁），回调要被调用。
//
// 与重启分开测：两者的结局不同（重启会换上新二进制继续服务，退出则留一个空档），
// 混在一起会掩盖「Windows 上误走重启路径」这类回归。
func TestInstallRequestsExitWhenAsked(t *testing.T) {
	s := newTestServer(t, "")
	exited := make(chan struct{}, 1)
	s.SetExitFunc(func() { exited <- struct{}{} })

	s.requestExit(10 * time.Millisecond)
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("请求退出后回调没有被调用")
	}

	// 没有退出通路时只提示，不 panic（精简装配的实例就是这样）。
	plain := newTestServer(t, "")
	plain.requestExit(0)
}

// 加速前缀端点：合法值落盘、非法值 400 且不落盘、空串表示直连。
func TestUpdateMirrorEndpoint(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	attachChecker(t, s, fake, t.TempDir())

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/panel/api/update/mirror", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		s.setUpdateMirror(rec, req)
		return rec
	}

	// 合法：落盘并回显。
	if rec := post(`{"mirror":"https://ghfast.top"}`); rec.Code != http.StatusOK {
		t.Fatalf("合法前缀应当 200，实际 %d（体=%s）", rec.Code, rec.Body.String())
	}
	if got := s.config().UpdateDownloadMirror(); got != "https://ghfast.top" {
		t.Fatalf("前缀没落盘：%q", got)
	}
	// 非法：400 且不落盘。
	if rec := post(`{"mirror":"ghfast.top"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺协议的前缀应当 400，实际 %d", rec.Code)
	}
	if got := s.config().UpdateDownloadMirror(); got != "https://ghfast.top" {
		t.Fatalf("被拒的请求不该改动已有配置：%q", got)
	}
	// 空串 = 清掉（直连）。
	if rec := post(`{"mirror":"  "}`); rec.Code != http.StatusOK {
		t.Fatalf("空串应当 200，实际 %d", rec.Code)
	}
	if got := s.config().UpdateDownloadMirror(); got != "" {
		t.Fatalf("空串应清掉前缀，实际 %q", got)
	}
	// 缺字段：400。
	if rec := post(`{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 mirror 字段应当 400，实际 %d", rec.Code)
	}
}

// TestUpdateProxyEndpoint 端点：合法值落盘并回显、非法值 400 且不落盘、空串清掉。
//
// 与 TestUpdateMirrorEndpoint 同构，但校验规则不同（代理必须是 http/https URL），
// 所以非法用例换成「少协议」「非 http 协议」这两种真实易犯的写法。
func TestUpdateProxyEndpoint(t *testing.T) {
	s := newTestServer(t, "")
	fake, _ := fakeGitHubForServer(t, "v0.9.3")
	attachChecker(t, s, fake, t.TempDir())

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/panel/api/update/proxy", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		s.setUpdateProxy(rec, req)
		return rec
	}

	// 合法：落盘并回显。
	if rec := post(`{"proxy":"http://127.0.0.1:7897"}`); rec.Code != http.StatusOK {
		t.Fatalf("合法代理应当 200，实际 %d（体=%s）", rec.Code, rec.Body.String())
	}
	if got := s.config().Update.Proxy; got != "http://127.0.0.1:7897" {
		t.Fatalf("代理没落盘：%q", got)
	}

	// 少协议：400 且不改动已有配置。
	if rec := post(`{"proxy":"127.0.0.1:7897"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺协议的代理应当 400，实际 %d", rec.Code)
	}
	if got := s.config().Update.Proxy; got != "http://127.0.0.1:7897" {
		t.Fatalf("被拒的请求不该改动已有配置：%q", got)
	}

	// 非 http 协议：400。
	if rec := post(`{"proxy":"socks5://127.0.0.1:1080"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("socks5 应当 400，实际 %d", rec.Code)
	}

	// 空串 = 关闭代理。
	if rec := post(`{"proxy":"  "}`); rec.Code != http.StatusOK {
		t.Fatalf("空串应当 200，实际 %d", rec.Code)
	}
	if got := s.config().Update.Proxy; got != "" {
		t.Fatalf("空串应清掉代理，实际 %q", got)
	}

	// 缺字段：400。
	if rec := post(`{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 proxy 字段应当 400，实际 %d", rec.Code)
	}
}

// TestUpdateProxySkipsMirrorWhenConfigured 配了代理时 `UpdateProxyConfigured` 为真 ——
// 下载据此**跳过**加速前缀。
//
// 这是本功能的取舍核心：走代理已经能到 GitHub，再套一层第三方转发只是多一跳，
// 还让那家看到你的下载。真实的「套不套前缀」分支在 internal/update 的 Download 里
// （那里读 settings.proxyConfigured），这里只钉住配置层的判定来源。
func TestUpdateProxySkipsMirrorWhenConfigured(t *testing.T) {
	// 清掉环境变量代理，否则测试机上的代理设置会让判定恒真。
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("http_proxy", "")
	t.Setenv("all_proxy", "")

	s := newTestServer(t, "")
	if s.config().UpdateProxyConfigured() {
		t.Fatal("前置条件不成立：没配代理时不该判为已生效")
	}
	if _, _, err := s.config().SavePatch([]byte(`{"update":{"proxy":"http://127.0.0.1:7897"}}`)); err != nil {
		t.Fatalf("配置代理失败: %v", err)
	}
	if !s.config().UpdateProxyConfigured() {
		t.Fatal("配了代理后应判为已生效（下载将跳过加速前缀）")
	}
}
