package main

// CLI 子命令（login / refresh / monitor）的轻量测试：
//   - 参数解析：`-h` / `help` 打用法、未知 flag 报错、站点与间隔校验；
//   - login -dry-run：走 httptest 假上游跑完整授权流程，验证不落盘且不泄露令牌。
//
// 测试一律不碰用户真实凭据目录：数据目录指向 t.TempDir()（WB_GATEWAY_DATA_DIR）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/upstream"
)

const (
	fakeAccessToken  = "ACCESS-TOKEN-MUST-NOT-LEAK"
	fakeRefreshToken = "REFRESH-TOKEN-MUST-NOT-LEAK"
	fakeUID          = "uid-001"
	fakeNickname     = "测试账号"
)

// cliTestEnv 把数据目录指向临时目录，返回该目录。
func cliTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WB_GATEWAY_DATA_DIR", dir)
	return dir
}

// fakeLoginUpstream 模拟设备授权登录的三个上游端点。
func fakeLoginUpstream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v2/plugin/auth/state":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"state": "state-001", "authUrl": "https://example.invalid/authorize?code=abc"},
		})
	case "/v2/plugin/auth/token":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"accessToken":  fakeAccessToken,
				"refreshToken": fakeRefreshToken,
				"expiresIn":    3600,
				"domain":       "example.invalid",
			},
		})
	case "/v2/plugin/login/account":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"uid": fakeUID, "enterpriseId": "ent-001", "nickname": fakeNickname},
		})
	default:
		http.NotFound(w, r)
	}
}

// pointLoginAtTestServer 把国内站 Profile 指到假上游，并调小登录轮询间隔。
func pointLoginAtTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fakeLoginUpstream))
	t.Cleanup(srv.Close)

	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base, upstream.ProfileCN.Origin = srv.URL, srv.URL
	t.Cleanup(func() {
		upstream.ProfileCN.Base, upstream.ProfileCN.Origin = oldBase, oldOrigin
	})

	oldPoll := loginPollEvery
	loginPollEvery = 5 * time.Millisecond
	t.Cleanup(func() { loginPollEvery = oldPoll })
	return srv
}

// TestCLIHelpPrintsUsage `help` 与 `-h` 都必须打印用法并成功返回（不得执行命令）。
func TestCLIHelpPrintsUsage(t *testing.T) {
	cliTestEnv(t)
	cmds := []struct {
		name string
		run  func(args []string, out, errOut io.Writer) int
	}{
		{"login", cmdLogin},
		{"refresh", cmdRefresh},
		{"monitor", cmdMonitor},
	}
	for _, cmd := range cmds {
		for _, helpArg := range []string{"help", "-h", "--help"} {
			t.Run(cmd.name+"/"+helpArg, func(t *testing.T) {
				var out, errOut bytes.Buffer
				if code := cmd.run([]string{helpArg}, &out, &errOut); code != 0 {
					t.Fatalf("%s %s 应退出码 0，实际 %d（stderr=%s）", cmd.name, helpArg, code, errOut.String())
				}
				if !strings.Contains(out.String(), "用法:") {
					t.Fatalf("%s %s 应打印用法，实际输出:\n%s", cmd.name, helpArg, out.String())
				}
			})
		}
	}
}

// TestCLIUnknownFlag 未知 flag 必须报参数错误（退出码 2），且不产生正常输出。
func TestCLIUnknownFlag(t *testing.T) {
	cliTestEnv(t)
	cmds := []struct {
		name string
		run  func(args []string, out, errOut io.Writer) int
	}{
		{"login", cmdLogin},
		{"refresh", cmdRefresh},
		{"monitor", cmdMonitor},
	}
	for _, cmd := range cmds {
		t.Run(cmd.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := cmd.run([]string{"-definitely-not-a-flag"}, &out, &errOut); code != exitUsage {
				t.Fatalf("%s 未知 flag 应退出码 %d，实际 %d", cmd.name, exitUsage, code)
			}
			if !strings.Contains(errOut.String(), "参数错误") {
				t.Fatalf("%s 应提示参数错误，实际 stderr:\n%s", cmd.name, errOut.String())
			}
			if out.Len() != 0 {
				t.Fatalf("%s 参数错误时不应有正常输出: %s", cmd.name, out.String())
			}
		})
	}
}

// TestCLIRejectsBadValues 站点与刷新间隔的取值校验必须在触网/进入循环之前完成。
func TestCLIRejectsBadValues(t *testing.T) {
	cliTestEnv(t)

	t.Run("login/site", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := cmdLogin([]string{"-site", "mars"}, &out, &errOut); code != exitUsage {
			t.Fatalf("非法站点应退出码 %d，实际 %d", exitUsage, code)
		}
		if !strings.Contains(errOut.String(), "不支持的站点") {
			t.Fatalf("应提示站点不支持，实际: %s", errOut.String())
		}
	})

	t.Run("monitor/interval", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := cmdMonitor([]string{"-interval", "0"}, &out, &errOut); code != exitUsage {
			t.Fatalf("非正数间隔应退出码 %d，实际 %d", exitUsage, code)
		}
		if !strings.Contains(errOut.String(), "-interval 必须为正数") {
			t.Fatalf("应提示间隔非法，实际: %s", errOut.String())
		}
	})

	t.Run("refresh/empty", func(t *testing.T) {
		dir := cliTestEnv(t)
		var out, errOut bytes.Buffer
		if code := cmdRefresh([]string{"-auth-dir", dir}, &out, &errOut); code != 0 {
			t.Fatalf("空账号池应正常退出，实际 %d（stderr=%s）", code, errOut.String())
		}
		if !strings.Contains(out.String(), "账号池为空") {
			t.Fatalf("应提示账号池为空，实际输出:\n%s", out.String())
		}
	})
}

// TestLoginDryRunDoesNotWriteCredential 跑完整授权流程但 -dry-run 不落盘，
// 同时钉住安全红线：令牌原文不得出现在 stdout / stderr。
func TestLoginDryRunDoesNotWriteCredential(t *testing.T) {
	dir := cliTestEnv(t)
	pointLoginAtTestServer(t)

	var out, errOut bytes.Buffer
	if code := cmdLogin([]string{"-site", "cn", "-dry-run"}, &out, &errOut); code != 0 {
		t.Fatalf("dry-run 登录应成功，实际退出码 %d（stderr=%s）", code, errOut.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取数据目录失败: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dry-run 不应写任何文件，实际写入: %v", names)
	}

	text := out.String()
	if !strings.Contains(text, "[dry-run]") {
		t.Fatalf("应标注 dry-run，实际输出:\n%s", text)
	}
	if !strings.Contains(text, fakeNickname) || !strings.Contains(text, fakeUID) {
		t.Fatalf("应打印账号摘要（昵称 + UID），实际输出:\n%s", text)
	}
	wantPath := filepath.Join(dir, "workbuddy-"+fakeUID+".json")
	if !strings.Contains(text, wantPath) {
		t.Fatalf("应预览凭据路径 %s，实际输出:\n%s", wantPath, text)
	}
	assertNoTokenLeak(t, out.String(), errOut.String())
	t.Logf("dry-run 真实输出:\n%s", text)
}

// TestLoginWritesCredential 非 dry-run 时凭据应落盘且可被既有 auth 解析器读回。
func TestLoginWritesCredential(t *testing.T) {
	dir := cliTestEnv(t)
	pointLoginAtTestServer(t)

	var out, errOut bytes.Buffer
	if code := cmdLogin([]string{"-site", "cn"}, &out, &errOut); code != 0 {
		t.Fatalf("登录应成功，实际退出码 %d（stderr=%s）", code, errOut.String())
	}
	path := filepath.Join(dir, "workbuddy-"+fakeUID+".json")
	cred, err := auth.LoadFile(path)
	if err != nil {
		t.Fatalf("凭据应可被 auth 解析: %v", err)
	}
	if cred.UID != fakeUID || cred.Nickname != fakeNickname {
		t.Fatalf("凭据账号信息不符: uid=%q nickname=%q", cred.UID, cred.Nickname)
	}
	if cred.AccessToken != fakeAccessToken {
		t.Fatalf("凭据内令牌未写入（读到 %q）", cred.AccessToken)
	}
	if cred.Site() != auth.SiteCN {
		t.Fatalf("站点应为 cn，实际 %q", cred.Site())
	}
	if !strings.Contains(out.String(), "登录成功") {
		t.Fatalf("应打印登录成功，实际输出:\n%s", out.String())
	}
	assertNoTokenLeak(t, out.String(), errOut.String())
}

// assertNoTokenLeak 检查输出中不含任一令牌原文（仓库红线）。
func assertNoTokenLeak(t *testing.T, outputs ...string) {
	t.Helper()
	for _, s := range outputs {
		if strings.Contains(s, fakeAccessToken) || strings.Contains(s, fakeRefreshToken) {
			t.Fatalf("输出泄露了令牌原文:\n%s", s)
		}
	}
}

// TestRenderCLITableAlignsWideRunes 表格按显示宽度对齐：所有行（含中文单元格）等宽。
func TestRenderCLITableAlignsWideRunes(t *testing.T) {
	rows := [][]string{
		{"测试账号", "国内站", "可用", "0", "100.00", "-", "1"},
		{"a-very-long-account-name", "国际站", "冷却中", "12", "-", "1m30s", "0"},
	}
	out := renderCLITable(
		[]string{"账号", "站点", "状态", "在途", "积分", "冷却剩余", "失败计数"},
		[]int{24, 8, 10, 6, 14, 12, 10}, rows,
	)
	lines := strings.Split(out, "\n")
	want := displayWidth(lines[0])
	for _, line := range lines {
		if got := displayWidth(line); got != want {
			t.Fatalf("各行显示宽度应一致（%d），实际 %d: %q", want, got, line)
		}
	}
	if !strings.Contains(out, "测试账号") {
		t.Fatalf("表格应包含中文账号名:\n%s", out)
	}
}

// TestMonitorRendersGatewayStatus 用假网关的 /status 跑一遍监控循环：
// 表格要能渲染出账号行与在途 / 积分 / 失败计数，且 Ctrl+C（ctx 取消）后正常退出。
func TestMonitorRendersGatewayStatus(t *testing.T) {
	cliTestEnv(t)
	payload := `{
		"version": "test",
		"uptime": 12,
		"summary": {"total":1,"active":1,"disabled":0,"cooldown":0,"credits_remaining":80.5,"quota_known":1,"in_flight":2},
		"accounts": [{
			"id":"workbuddy-u1.json","file":"workbuddy-u1.json","site":"cn","site_label":"国内站",
			"uid":"u1","nickname":"巡检号","disabled":false,"cooldown_until":0,
			"token_expires_at":0,"token_valid":true,"success_count":0,"failure_count":3,
			"quota":{"total":100,"used":19.5,"remaining":80.5,"plan":"免费","paid":false,"exhausted":false,"updated_at":0},
			"cooldown_kind":"","breaker_until":0,"fails":0,"in_flight":2,"max_in_flight":0,"last_used_at":0,"base_url":""
		}]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("解析测试服务器地址失败: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)
	cfg := &config.Config{Addr: host, Port: port}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	runMonitorLoop(ctx, cfg, 10*time.Millisecond, &out)

	text := out.String()
	for _, want := range []string{"巡检号", "国内站", "在途", "80.50", "失败计数", "监控已退出"} {
		if !strings.Contains(text, want) {
			t.Fatalf("监控输出应包含 %q，实际:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "在途 2") {
		t.Fatalf("汇总行应显示在途 2，实际:\n%s", text)
	}
	t.Logf("monitor 真实输出:\n%s", text)
}
