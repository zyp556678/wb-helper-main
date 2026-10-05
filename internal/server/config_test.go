package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 配置接口下发 api_key 的边界：配了才发，而且**必须带对密钥**才拿得到。
//
// 这条守的是安全性质。configView 里多了一个密钥字段，一旦路由上的
// withPanelAuth 被去掉、或换成不带鉴权的包装，密钥就对任何本机进程敞开了 ——
// 而这种改动不会让任何功能报错，只有测试能拦住。
func TestConfigViewExposesAPIKeyOnlyBehindAuth(t *testing.T) {
	s := newTestServer(t, "")
	const key = "test-panel-key-1234"
	s.config().APIKey = key

	// 1) 不带密钥 → 401，且响应体里不能出现密钥
	rec := httptest.NewRecorder()
	s.withPanelAuth(s.handlePanelConfigRoute)(
		rec, httptest.NewRequest(http.MethodGet, "/panel/api/config", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未带密钥应 401，实际 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), key) {
		t.Fatal("401 响应里泄露了密钥")
	}

	// 2) 带错密钥 → 同样 401
	req := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	recWrong := httptest.NewRecorder()
	s.withPanelAuth(s.handlePanelConfigRoute)(recWrong, req)
	if recWrong.Code != http.StatusUnauthorized {
		t.Fatalf("密钥不对应 401，实际 %d", recWrong.Code)
	}
	if strings.Contains(recWrong.Body.String(), key) {
		t.Fatal("错误密钥的响应里泄露了真密钥")
	}

	// 3) 带对密钥 → 200，且能读到密钥（面板靠它显示「实际密钥」给别的工具用）
	reqOK := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	reqOK.Header.Set("Authorization", "Bearer "+key)
	recOK := httptest.NewRecorder()
	s.withPanelAuth(s.handlePanelConfigRoute)(recOK, reqOK)
	if recOK.Code != http.StatusOK {
		t.Fatalf("带对密钥应 200，实际 %d，体=%s", recOK.Code, recOK.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recOK.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body["api_key"] != key {
		t.Fatalf("api_key 应为 %q，实际 %v", key, body["api_key"])
	}
	if body["auth_check_enabled"] != true {
		t.Fatalf("auth_check_enabled 应为 true，实际 %v", body["auth_check_enabled"])
	}
}

// 没配密钥时下发空串，前端据此显示「网关未启用鉴权」。
//
// 关键是**空串而不是缺字段**：前端用 `configuredApiKey || 回落` 的写法会
// 把 undefined 和 "" 当成同一件事，但缺字段在 JSON 里更容易被误当成
// 「这个后端版本不支持」而走错分支。
func TestConfigViewSendsEmptyAPIKeyWhenUnset(t *testing.T) {
	s := newTestServer(t, "")
	if s.config().APIKey != "" {
		t.Fatal("测试前提：默认不应配置密钥")
	}
	view := s.configView()
	got, ok := view["api_key"]
	if !ok {
		t.Fatal("api_key 字段必须存在（未配置时为空串）")
	}
	if got != "" {
		t.Fatalf("未配置时应为空串，实际 %v", got)
	}
	if view["auth_check_enabled"] != false {
		t.Fatalf("auth_check_enabled 应为 false，实际 %v", view["auth_check_enabled"])
	}
}
