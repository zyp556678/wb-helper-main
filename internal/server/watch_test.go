package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestConfigFileWatchHotReloads 手动改 config.json（不重启进程）后新配置生效。
//
// 「手动改文件不生效」是文件监听要补的那一半：面板保存一直是热生效的，
// 但 SSH/容器场景下改一行配置就只能重启，重启会掐断全部在途会话。
func TestConfigFileWatchHotReloads(t *testing.T) {
	ts, _ := sseServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {})
	withFakeUpstream(t, ts.URL)
	srv := newTestServer(t, ts.URL)

	path := srv.config().ConfigFile
	body := `{"upstream":{"header_timeout_seconds":17},` +
		`"models":{"blocklist":["glm-5.3"],"accounts":{"glm-5.3":{"blocklist":["workbuddy-test.json"]}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写 config.json 失败: %v", err)
	}

	srv.reloadConfigIfChanged() // 等价于监听循环 tick 一次

	got := srv.config()
	if got.HeaderTimeout != 17*time.Second {
		t.Fatalf("上游超时未热加载: %v", got.HeaderTimeout)
	}
	if len(got.ModelBlock) != 1 || got.ModelBlock[0] != "glm-5.3" {
		t.Fatalf("模型黑名单未热加载: %v", got.ModelBlock)
	}
	// 模型专属账号名单由**账号池**持有快照：池不换快照就会出现
	// 「文件改了、判据还停在启动那一刻」—— 这是热加载最容易漏的一处。
	acc := srv.pool.Accounts()[0]
	if acc == nil {
		t.Fatal("测试账号池为空")
	}
	if ok, _ := srv.pool.ModelAccountAllowed("glm-5.3", acc); ok {
		t.Fatal("模型账号名单未热加载（账号池仍用旧快照）")
	}

	// 内容没变：不该再造一份快照（否则每秒 tick 都会白白换一次配置）。
	same := srv.config()
	srv.reloadConfigIfChanged()
	if srv.config() != same {
		t.Fatal("内容没变时不应重复加载")
	}

	// 坏配置：拒绝并保留上一份有效配置。
	if err := os.WriteFile(path, []byte(`{"models":`), 0o600); err != nil {
		t.Fatalf("写 config.json 失败: %v", err)
	}
	srv.reloadConfigIfChanged()
	if srv.config() != same {
		t.Fatal("坏配置不应换掉上一份有效配置")
	}
	// 同一份坏内容重复出现时不该反复告警（监听循环每秒都会读到它）。
	if srv.cfgWatch.MarkFailed([]byte(`{"models":`)) {
		t.Fatal("同一份坏内容不该重复告警")
	}
}

// TestPanelSaveNotReplayedByWatcher 面板保存后，文件监听不该把这次保存当成外部改动重放。
func TestPanelSaveNotReplayedByWatcher(t *testing.T) {
	ts, _ := sseServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {})
	withFakeUpstream(t, ts.URL)
	srv := newTestServer(t, ts.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config",
		strings.NewReader(`{"models":{"blocklist":["glm-5.3"]}}`))
	req.Header.Set("Content-Type", "application/json")
	srv.handlePanelConfigRoute(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存配置失败: %d %s", rec.Code, rec.Body.String())
	}

	saved := srv.config()
	if len(saved.ModelBlock) != 1 {
		t.Fatalf("面板保存未生效: %v", saved.ModelBlock)
	}
	srv.reloadConfigIfChanged()
	if srv.config() != saved {
		t.Fatal("面板刚保存的内容不该被文件监听重放（游标未标记）")
	}

	// 外部再改一次仍应被发现。
	path := saved.ConfigFile
	if err := os.WriteFile(path, []byte(`{"models":{"blocklist":["glm-5.3","hy3"]}}`), 0o600); err != nil {
		t.Fatalf("写 config.json 失败: %v", err)
	}
	srv.reloadConfigIfChanged()
	if got := srv.config().ModelBlock; len(got) != 2 {
		t.Fatalf("外部改动未被监听接住: %v", got)
	}
}
