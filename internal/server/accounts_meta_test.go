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
	"time"

	"workbuddy-gateway/internal/accountmeta"
	"workbuddy-gateway/internal/clientlimits"
	"workbuddy-gateway/internal/scheduler"
	"workbuddy-gateway/internal/upstream"
)

// -----------------------------------------------------------------------------
// 批次 4：账号备注 / 显示字段 / 自动签到开关 / 批量刷新积分并签到
// -----------------------------------------------------------------------------

// 账号列表必须**加法**下发 note / display_field / auto_checkin_enabled，
// 且原有字段（uid 等）一个不少 —— 契约只做加法。
func TestPanelAccountsDownlinksMetaAndAutoCheckin(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetAccountMeta(accountmeta.New(filepath.Join(t.TempDir(), "wb-account-meta.json")))

	note := "工作号"
	field := accountmeta.DisplayNote
	if _, err := srv.accountMeta.Update("workbuddy-test.json", &note, &field); err != nil {
		t.Fatalf("写入元数据失败: %v", err)
	}
	// 名单内的账号：auto_checkin_enabled 应为 false。
	srv.config().Schedule.CheckinExcludedAccounts = []string{"workbuddy-test.json"}

	rec := httptest.NewRecorder()
	srv.handlePanelAccounts(rec, httptest.NewRequest(http.MethodGet, "/panel/api/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("应有 1 个账号，实际 %d", len(out.Accounts))
	}
	acc := out.Accounts[0]
	if acc["note"] != note {
		t.Fatalf("note 应下发 %q，实际 %v", note, acc["note"])
	}
	if acc["display_field"] != field {
		t.Fatalf("display_field 应下发 %q，实际 %v", field, acc["display_field"])
	}
	if acc["auto_checkin_enabled"] != false {
		t.Fatalf("名单内账号 auto_checkin_enabled 应为 false，实际 %v", acc["auto_checkin_enabled"])
	}
	// 原有契约字段仍在（内嵌结构体的字段必须被提升）。
	if acc["id"] != "workbuddy-test.json" || acc["uid"] != "u1" {
		t.Fatalf("原有字段丢失: %v", acc)
	}
	// 没有数据源的企业名不应凭空出现。
	if _, ok := acc["enterprise_name"]; ok {
		t.Fatal("本项目没有企业名数据源，不应下发 enterprise_name")
	}
}

// PATCH 校验：24 字符上限按**字符**（中文）计；非法显示字段 400；
// 合法更新落盘并能从 GET /accounts/meta 读回。
func TestAccountMetaPatchValidationAndPersistence(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetAccountMeta(accountmeta.New(filepath.Join(t.TempDir(), "wb-account-meta.json")))

	patch := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/panel/api/accounts/workbuddy-test.json/meta",
			strings.NewReader(body))
		req.SetPathValue("id", "workbuddy-test.json")
		rec := httptest.NewRecorder()
		srv.handleAccountMetaUpdate(rec, req)
		return rec
	}

	tooLong := strings.Repeat("备", accountmeta.MaxNoteRunes+1)
	if rec := patch(fmt.Sprintf(`{"note":%q}`, tooLong)); rec.Code != http.StatusBadRequest {
		t.Fatalf("超长备注应 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := patch(`{"display_field":"email"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法显示字段应 400，实际 %d：%s", rec.Code, rec.Body.String())
	}

	okNote := strings.Repeat("备", accountmeta.MaxNoteRunes) // 24 个中文字符
	rec := patch(fmt.Sprintf(`{"note":%q,"display_field":"note"}`, okNote))
	if rec.Code != http.StatusOK {
		t.Fatalf("合法更新应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}

	// GET 全员元数据。
	rec = httptest.NewRecorder()
	srv.handleAccountsMeta(rec, httptest.NewRequest(http.MethodGet, "/panel/api/accounts/meta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("meta 一览应 200，实际 %d", rec.Code)
	}
	var metaOut struct {
		Accounts map[string]accountmeta.Entry `json:"accounts"`
		MaxLen   int                          `json:"note_max_length"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &metaOut); err != nil {
		t.Fatalf("解析 meta 响应失败: %v", err)
	}
	if metaOut.MaxLen != accountmeta.MaxNoteRunes {
		t.Fatalf("note_max_length 应为 %d，实际 %d", accountmeta.MaxNoteRunes, metaOut.MaxLen)
	}
	entry, ok := metaOut.Accounts["workbuddy-test.json"]
	if !ok || entry.Note != okNote || entry.DisplayField != accountmeta.DisplayNote {
		t.Fatalf("meta 应能读回更新后的条目，实际 %+v", metaOut.Accounts)
	}

	// 不存在的账号 → 404（校验通过但目标不存在）。
	req := httptest.NewRequest(http.MethodPatch, "/panel/api/accounts/nope.json/meta",
		strings.NewReader(`{"note":"x"}`))
	req.SetPathValue("id", "nope.json")
	rec = httptest.NewRecorder()
	srv.handleAccountMetaUpdate(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知账号应 404，实际 %d", rec.Code)
	}
}

// checkin_all 的窗口外语义：整轮跳过签到，但**积分照刷**。
func TestCheckinAllOutsideWindowRefreshesCreditsOnly(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[{"CycleTotalCapacity":"100","CycleUsedCapacity":"10","CycleRemainCapacity":"90"}],"IsPaidUser":false}}`))
	}))
	defer upstreamSrv.Close()
	origCN := upstream.ProfileCN
	upstream.ProfileCN = withURL(origCN, upstreamSrv.URL)
	defer func() { upstream.ProfileCN = origCN }()

	srv := newTestServer(t, upstreamSrv.URL)
	// 造一个肯定不含当前时刻的窗口（取当前时刻 +10 小时，宽 1 分钟）。
	now := time.Now()
	startMin := (now.Hour()*60 + now.Minute() + 600) % (24 * 60)
	endMin := startMin + 1
	if endMin > 24*60-1 {
		startMin, endMin = 0, 1
	}
	clock := func(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }
	srv.config().Schedule.CheckinStart = clock(startMin)
	srv.config().Schedule.CheckinEnd = clock(endMin)
	if srv.config().InCheckinWindow(now) {
		t.Skip("当前时刻恰好落在构造的窗口内（概率极低），跳过该断言")
	}

	rec := httptest.NewRecorder()
	srv.handleAccountsCheckinAll(rec, httptest.NewRequest(http.MethodPost, "/panel/api/accounts/checkin_all", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var out scheduler.CheckinSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if !out.OutsideWindow {
		t.Fatal("窗口外应返回 outside_window=true")
	}
	if out.Success != 0 || out.Already != 0 || out.Failed != 0 || out.Skipped != 0 {
		t.Fatalf("窗口外所有计数应为 0，实际 %+v", out)
	}

	mu.Lock()
	got := append([]string(nil), paths...)
	mu.Unlock()
	sawQuota, sawCheckin := false, false
	for _, p := range got {
		if strings.Contains(p, "get-user-resource-summary") {
			sawQuota = true
		}
		if strings.Contains(p, "daily-checkin") {
			sawCheckin = true
		}
	}
	if !sawQuota {
		t.Fatalf("窗口外必须照刷积分（应打额度端点），实际请求: %v", got)
	}
	if sawCheckin {
		t.Fatalf("窗口外不应发签到请求，实际请求: %v", got)
	}
}

// 排除名单在批量入口生效：名单内账号不签到但额度照刷；
// 移出名单后照常签到（对应「不在名单内照常」）。
func TestCheckinAllRespectsExclusionList(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "daily-checkin") {
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[{"CycleTotalCapacity":"100","CycleUsedCapacity":"10","CycleRemainCapacity":"90"}],"IsPaidUser":false}}`))
	}))
	defer upstreamSrv.Close()
	origCN := upstream.ProfileCN
	upstream.ProfileCN = withURL(origCN, upstreamSrv.URL)
	defer func() { upstream.ProfileCN = origCN }()

	srv := newTestServer(t, upstreamSrv.URL)
	srv.config().Schedule.CheckinExcludedAccounts = []string{"workbuddy-test.json"}

	call := func() (scheduler.CheckinSummary, []string) {
		t.Helper()
		mu.Lock()
		paths = nil
		mu.Unlock()
		rec := httptest.NewRecorder()
		srv.handleAccountsCheckinAll(rec, httptest.NewRequest(http.MethodPost, "/panel/api/accounts/checkin_all", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		var out scheduler.CheckinSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是 JSON: %v", err)
		}
		mu.Lock()
		got := append([]string(nil), paths...)
		mu.Unlock()
		return out, got
	}
	hasPath := func(paths []string, needle string) bool {
		for _, p := range paths {
			if strings.Contains(p, needle) {
				return true
			}
		}
		return false
	}

	// 名单内：跳过签到，但额度照刷。
	out, got := call()
	if out.OutsideWindow {
		t.Fatal("未配置窗口时不应标 outside_window")
	}
	if out.Skipped != 1 || out.Success != 0 || out.Already != 0 {
		t.Fatalf("名单内账号应计 skipped=1，实际 %+v", out)
	}
	if hasPath(got, "daily-checkin") {
		t.Fatalf("名单内账号不应发签到请求，实际请求: %v", got)
	}
	if !hasPath(got, "get-user-resource-summary") {
		t.Fatalf("名单内账号的积分仍应刷新，实际请求: %v", got)
	}

	// 移出名单：照常签到。
	srv.config().Schedule.CheckinExcludedAccounts = nil
	out, got = call()
	if out.Success != 1 || out.Skipped != 0 {
		t.Fatalf("移出名单后应照常签到 success=1，实际 %+v", out)
	}
	if !hasPath(got, "daily-checkin") {
		t.Fatalf("移出名单后应发签到请求，实际请求: %v", got)
	}

	// 单账号手动签到**不受名单限制**：同一时刻把名单加回来，
	// 直接调单账号 handler 仍应发签到请求并成功。
	srv.config().Schedule.CheckinExcludedAccounts = []string{"workbuddy-test.json"}
	mu.Lock()
	paths = nil
	mu.Unlock()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/workbuddy-test.json/checkin", nil)
	req.SetPathValue("id", "workbuddy-test.json")
	rec := httptest.NewRecorder()
	srv.handleAccountCheckin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("手动签到应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	got = append([]string(nil), paths...)
	mu.Unlock()
	if !hasPath(got, "daily-checkin") {
		t.Fatalf("手动签到不应受排除名单影响，实际请求: %v", got)
	}
}

// -----------------------------------------------------------------------------
// 模型级冷却提醒：账号列表下发 model_cooldowns（卡片「模型限额」提醒的数据源）
// -----------------------------------------------------------------------------

// 账号列表必须加法下发**未过期**的模型级冷却，且按恢复时间升序；
// 已过期的条目不下发（卡片据此决定是否渲染提醒）。
func TestPanelAccountsDownlinksModelCooldowns(t *testing.T) {
	srv := newTestServer(t, "")
	acc := srv.pool.Accounts()[0]
	// 未来恢复的一条（无重置时间 → 有界退避）。
	acc.CooldownModel("deepseek-v4.1-flash", time.Minute, time.Time{}, "测试限流")
	// 已过期的重置时间：capSoft 视作「立即恢复」（now+1ms），
	// 等过 1ms 再查询，它就该从下发里消失。
	acc.CooldownModel("stale-model", time.Minute, time.Now().Add(-time.Hour), "过期")
	time.Sleep(5 * time.Millisecond)

	rec := httptest.NewRecorder()
	srv.handlePanelAccounts(rec, httptest.NewRequest(http.MethodGet, "/panel/api/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Accounts []struct {
			ModelCooldowns []struct {
				Model string `json:"model"`
				Until int64  `json:"until"`
			} `json:"model_cooldowns"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("应有 1 个账号，实际 %d", len(out.Accounts))
	}
	got := out.Accounts[0].ModelCooldowns
	if len(got) != 1 || got[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("应只下发未过期的模型冷却，实际 %+v", got)
	}
	if got[0].Until <= time.Now().Unix() {
		t.Fatalf("恢复时间应在未来（Unix 秒），实际 %d", got[0].Until)
	}
}

// 客户端日志里的限流必须归因后并入 model_cooldowns：
// WorkBuddy 桌面客户端直连官方，它的 429 不经过网关（切片 19 追加的扫日志兜底通路）。
func TestPanelAccountsMergesClientLogLimits(t *testing.T) {
	srv := newTestServer(t, "")

	root := t.TempDir()
	dir := filepath.Join(root, time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 用实测原文的行形态：认证成功行（昵称归因）+ 模型请求行 + 带 requestId 的 429 行。
	content := "[2026/10/4 19:14:13.442] [Info] [pid=1] [AuthenticationSessionListener] [auth_success: 测试]  认证成功\n" +
		"[2026/10/4 19:14:15.179] [Info] [pid=1] [ModelProvider] Sending request: agent=cli, model=deepseek-v4.1-flash, requestId=01a1069f23ff7049abb5ed59e8b0c81e, stream=true\n" +
		"[2026/10/4 19:14:15.469] [Info] [pid=1] [SessionManager] error=429 您的使用量已超出频率限制，将在 2099-10-05 13:49:05 UTC+8 重置，您也可以切换其他模型继续使用。 (01a1069f23ff7049abb5ed59e8b0c81e/15e48553-c56e-4eea-9919-40f5ac680a4a)\n"
	if err := os.WriteFile(filepath.Join(dir, "task.log"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.SetClientLimits(clientlimits.New(root))

	rec := httptest.NewRecorder()
	srv.handlePanelAccounts(rec, httptest.NewRequest(http.MethodGet, "/panel/api/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Accounts []struct {
			ModelCooldowns []struct {
				Model  string `json:"model"`
				Until  int64  `json:"until"`
				Source string `json:"source"`
			} `json:"model_cooldowns"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("应有 1 个账号，实际 %d", len(out.Accounts))
	}
	got := out.Accounts[0].ModelCooldowns
	if len(got) != 1 || got[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("应合并客户端日志里的限流，实际 %+v", got)
	}
	if got[0].Source != "client" {
		t.Errorf("来源应标注为 client，实际 %q", got[0].Source)
	}
	if got[0].Until <= time.Now().Unix() {
		t.Errorf("恢复时刻应在未来，实际 %d", got[0].Until)
	}
}
