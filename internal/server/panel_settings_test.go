package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy-gateway/internal/creditwatch"
)

// -----------------------------------------------------------------------------
// 批次 5：设置页只读查询（签到日志 / 日志落点 / 日志下载）
// -----------------------------------------------------------------------------

func decodeJSONBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v，体=%s", err, rec.Body.String())
	}
	return body
}

// 签到日志接口：读本地台账（空台账返回 [] 而不是 null）、days 默认与上限都按 30。
func TestCheckinLogsEndpointDefaultsAndClamp(t *testing.T) {
	s := newTestServer(t, "")
	store := creditwatch.New(filepath.Join(t.TempDir(), "wb-credit-history.json"))
	s.SetCreditWatch(store)

	// 空台账：logs 必须是 []（Go 的 nil slice 会序列化成 null，前端按数组取值会抛错）。
	rec := httptest.NewRecorder()
	s.handleCheckinLogs(rec, httptest.NewRequest(http.MethodGet, "/panel/api/checkin/logs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("空台账应 200，实际 %d", rec.Code)
	}
	body := decodeJSONBody(t, rec)
	if logs, ok := body["logs"].([]any); !ok || len(logs) != 0 {
		t.Fatalf("空台账应返回空数组，得到 %#v", body["logs"])
	}
	if body["retention_days"] != float64(creditwatch.CheckinKeepDays) {
		t.Fatalf("retention_days 应为 %d，得到 %v", creditwatch.CheckinKeepDays, body["retention_days"])
	}
	if body["max_records"] != float64(creditwatch.CheckinLogCap) {
		t.Fatalf("max_records 应为 %d，得到 %v", creditwatch.CheckinLogCap, body["max_records"])
	}

	// 记录两条后：days 非法/超限都收敛到 30，字段齐全。
	store.RecordCheckin("a.json", "账号A", "cn", "success", "")
	store.RecordCheckin("b.json", "账号B", "cn", "error", "上游 500")
	for _, query := range []string{"", "?days=0", "?days=-3", "?days=999", "?days=abc"} {
		rec := httptest.NewRecorder()
		s.handleCheckinLogs(rec, httptest.NewRequest(http.MethodGet, "/panel/api/checkin/logs"+query, nil))
		body := decodeJSONBody(t, rec)
		if body["days"] != float64(creditwatch.CheckinKeepDays) {
			t.Fatalf("query %q: days 应为 30，得到 %v", query, body["days"])
		}
		logs, _ := body["logs"].([]any)
		if len(logs) != 2 {
			t.Fatalf("query %q: 应返回 2 条，得到 %d", query, len(logs))
		}
		first, _ := logs[0].(map[string]any)
		for _, key := range []string{"ts", "date", "account_id", "account_name", "site", "result"} {
			if _, ok := first[key]; !ok {
				t.Fatalf("query %q: 记录缺少字段 %q: %#v", query, key, first)
			}
		}
	}

	// 只接受 GET。
	recMethod := httptest.NewRecorder()
	s.handleCheckinLogs(recMethod, httptest.NewRequest(http.MethodPost, "/panel/api/checkin/logs", nil))
	if recMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应 405，实际 %d", recMethod.Code)
	}
}

// 日志落点接口：路径来自配置（数据目录），目录不存在也照实返回。
func TestLogPathsAndDownload(t *testing.T) {
	s := newTestServer(t, "")
	dir := s.config().WorkDir
	reqDir := filepath.Join(dir, "request-logs")

	// 1) 目录尚不存在：照实返回路径 + exists=false。
	rec := httptest.NewRecorder()
	s.handleLogPaths(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs/paths", nil))
	body := decodeJSONBody(t, rec)
	if body["data_dir"] != dir || body["request_log_dir"] != reqDir {
		t.Fatalf("路径应来自配置: %#v", body)
	}
	if body["request_log_dir_exists"] != false || body["latest_request_log"] != "" {
		t.Fatalf("目录不存在时应如实标记: %#v", body)
	}

	// 2) 造两个归档（旧 / 新），最新按修改时间判定。
	if err := os.MkdirAll(reqDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldName := "requests-2026-10-01.jsonl"
	newName := "requests-2026-10-04.jsonl"
	if err := os.WriteFile(filepath.Join(reqDir, oldName), []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(reqDir, oldName), past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reqDir, newName), []byte(`{"new":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	s.handleLogPaths(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs/paths", nil))
	body = decodeJSONBody(t, rec)
	if body["request_log_dir_exists"] != true {
		t.Fatalf("目录已存在应标记 true: %#v", body)
	}
	if body["latest_request_log"] != filepath.Join(reqDir, newName) {
		t.Fatalf("最新归档应为 %s，得到 %v", newName, body["latest_request_log"])
	}

	// 3) 缺省下载（无 name）→ 最新那个文件；指定 name → 对应文件。
	for _, query := range []string{"", "?name=" + newName} {
		rec = httptest.NewRecorder()
		s.handleRequestLogDownload(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs/download"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("query %q 应 200，实际 %d（体=%s）", query, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != `{"new":true}` {
			t.Fatalf("query %q 下载内容不符: %s", query, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Disposition"); got == "" {
			t.Fatalf("query %q 应带下载头", query)
		}
	}

	// 4) 路径穿越 / 非法名字一律 404（只能命中目录内的归档）。
	for _, name := range []string{"../config.json", "CONFIG.JSON", "requests-2026-10-04.txt", "missing.jsonl"} {
		rec = httptest.NewRecorder()
		s.handleRequestLogDownload(rec, httptest.NewRequest(http.MethodGet, "/panel/api/logs/download?name="+name, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("name=%q 应 404，实际 %d", name, rec.Code)
		}
	}
}
