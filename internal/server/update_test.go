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
	checker := update.New(s.config(), update.Options{
		Current:     "0.9.2",
		Client:      fake.Client(),
		APIBase:     fake.URL,
		GOOS:        "linux",
		GOARCH:      "amd64",
		DownloadDir: dir,
		Logf:        t.Logf,
	})
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
