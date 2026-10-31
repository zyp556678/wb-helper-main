package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 监听端口热切换（对照 wb-gateway v1.14.38）。
//
// 这组用例守的是「失败不改变运行状态」这条：新端口起不来时，保存必须被拒绝，
// 内存配置与面板显示都不能变 —— 否则面板会显示一个实际没在监听的地址。

type fakeSwapper struct {
	err   error
	calls []string
}

func (f *fakeSwapper) SwapListenAddr(addr string) error {
	f.calls = append(f.calls, addr)
	return f.err
}

func postConfig(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.saveConfig(rec, req)
	return rec
}

// TestSaveConfigSwapsListenAddr 改端口时先换监听，成功才落库。
func TestSaveConfigSwapsListenAddr(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:1")
	sw := &fakeSwapper{}
	srv.SetListenerSwapper(sw)

	rec := postConfig(t, srv, `{"server":{"port":19999}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if len(sw.calls) != 1 {
		t.Fatalf("应调用一次换监听，实际 %v", sw.calls)
	}
	if sw.calls[0] != "127.0.0.1:19999" {
		t.Errorf("换监听的地址不对: %q", sw.calls[0])
	}
	if got := srv.config().ListenAddr(); got != "127.0.0.1:19999" {
		t.Errorf("配置未生效，ListenAddr=%q", got)
	}
}

// TestSaveConfigKeepsOldPortWhenSwapFails 新端口起不来时，保存被拒且配置不变。
func TestSaveConfigKeepsOldPortWhenSwapFails(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:1")
	before := srv.config().ListenAddr()
	sw := &fakeSwapper{err: errPortBusy{}}
	srv.SetListenerSwapper(sw)

	rec := postConfig(t, srv, `{"server":{"port":19998}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("期望 409，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := srv.config().ListenAddr(); got != before {
		t.Fatalf("保存失败后配置被改动了：%q -> %q", before, got)
	}
	// 错误信息要能让人知道「旧端口还在服务」。
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	errObj, _ := payload["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "保留原端口") {
		t.Errorf("错误信息没有说明旧端口仍在服务: %v", payload)
	}
}

// TestSaveConfigSkipsSwapWhenAddrUnchanged 地址没变时不该白白换一次监听。
func TestSaveConfigSkipsSwapWhenAddrUnchanged(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:1")
	sw := &fakeSwapper{}
	srv.SetListenerSwapper(sw)

	if rec := postConfig(t, srv, `{"prompt":{"sanitize":true}}`); rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if len(sw.calls) != 0 {
		t.Errorf("地址没变却换了监听: %v", sw.calls)
	}
}

// TestSaveConfigWithoutSwapper 没有注入实现时（例如单元测试或旧装配路径）
// 改端口不该 panic，也不能静默当成成功换过端口。
func TestSaveConfigWithoutSwapper(t *testing.T) {
	srv := newTestServer(t, "http://127.0.0.1:1")
	srv.SetListenerSwapper(nil)
	if rec := postConfig(t, srv, `{"server":{"port":19997}}`); rec.Code != http.StatusOK {
		t.Fatalf("期望 200（降级为仅写配置），实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

type errPortBusy struct{}

func (errPortBusy) Error() string {
	return "listen tcp :19998: bind: Only one usage of each socket address"
}
