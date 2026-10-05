package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/upstream"
)

// 本文件钉住 schedule.include_disabled_in_tasks 的真实效果：「禁用」只关选号，
// 不停保号。缺省行为（禁用即跳过）与打开后的行为都要测 —— 这是一个「开关没生效」
// 很难被发现的功能（表现是禁用号静默拿不到签到积分、token 不续期），
// 只测纯函数不够，必须跑一遍真实路径。

// withProfileURL 把某站 Profile 的两条基址指到假上游（ProfileForSite 返回的就是这些全局值）。
func withProfileURL(p upstream.Profile, url string) upstream.Profile {
	p.Base = url
	p.Origin = url
	return p
}

// TestIncludeDisabledInTasksCoversKeepaliveAndBalance 覆盖开关打开后：
// 禁用号照常签到、照常续期 token，**但不被解冻**（解冻是人工动作）。
func TestIncludeDisabledInTasksCoversKeepaliveAndBalance(t *testing.T) {
	var checkinCalls, refreshCalls int32
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			atomic.AddInt32(&checkinCalls, 1)
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			atomic.AddInt32(&refreshCalls, 1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","expiresIn":3600}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer upstreamSrv.Close()

	origCN := upstream.ProfileCN
	upstream.ProfileCN = withProfileURL(origCN, upstreamSrv.URL)
	defer func() { upstream.ProfileCN = origCN }()

	dir := t.TempDir()
	mk := func(name, uid string) *auth.Credential {
		cred := auth.NewForLogin(filepath.Join(dir, name), auth.SiteCN,
			"TEST_ACCESS", "TEST_REFRESH", 4102444800, "copilot.tencent.com", uid, "", uid)
		if err := cred.SaveFull(); err != nil {
			t.Fatalf("写入测试凭据失败: %v", err)
		}
		return cred
	}

	client := &upstream.Client{
		Control:  upstreamSrv.Client(),
		ChatHTTP: upstreamSrv.Client(),
		Logf:     t.Logf,
	}
	cfg := &config.Config{WorkDir: dir}
	cfg.ApplyDefaults()
	p := pool.New(cfg, client)
	p.SetGov(pool.GovFromConfig(cfg))
	p.Add(mk("workbuddy-u1.json", "u1"))
	p.Add(mk("workbuddy-u2.json", "u2"))
	accs := p.Accounts()
	if len(accs) != 2 {
		t.Fatalf("前置不成立：池里应有 2 个账号，实际 %d", len(accs))
	}
	disabled := accs[1]
	if err := p.SetDisabled(disabled, true, "手工禁用（测试）"); err != nil {
		t.Fatalf("禁用账号失败: %v", err)
	}

	s := New(cfg, p, client)
	ctx := context.Background()

	// ── 缺省：禁用号被跳过（锁定既有行为）────────────────────────────────
	if _, err := s.RunCheckinNow(ctx); err != nil {
		t.Fatalf("签到失败: %v", err)
	}
	if got := atomic.LoadInt32(&checkinCalls); got != 1 {
		t.Fatalf("缺省应只给 1 个账号签到（禁用号跳过），实际 %d 次", got)
	}

	// ── 打开开关（走「换配置快照」热生效路径）──────────────────────────────
	next := cfg.Clone()
	on := true
	next.Schedule.IncludeDisabledInTasks = &on
	s.SetConfig(next)

	if _, err := s.RunCheckinNow(ctx); err != nil {
		t.Fatalf("签到失败: %v", err)
	}
	if got := atomic.LoadInt32(&checkinCalls); got != 2 {
		t.Fatalf("开关打开后禁用号也应签到（共 2 次），实际 %d 次", got)
	}
	if !disabled.IsDisabled() {
		t.Error("覆盖开关不得解冻禁用号（解冻只能人工操作）")
	}

	// ── 保活：禁用号也要续期 token ────────────────────────────────────────
	if _, err := s.RunKeepaliveNow(ctx); err != nil {
		t.Fatalf("保活失败: %v", err)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 2 {
		t.Fatalf("保活应覆盖禁用号（共 2 次刷新），实际 %d 次", got)
	}
	if !disabled.IsDisabled() {
		t.Error("保活不得解冻禁用号")
	}

	// ── 关回去：行为立即回到「禁用即跳过」（热改两个方向都要成立）───────────
	offCfg := cfg.Clone()
	off := false
	offCfg.Schedule.IncludeDisabledInTasks = &off
	s.SetConfig(offCfg)
	atomic.StoreInt32(&checkinCalls, 0)
	if _, err := s.RunCheckinNow(ctx); err != nil {
		t.Fatalf("签到失败: %v", err)
	}
	if got := atomic.LoadInt32(&checkinCalls); got != 1 {
		t.Fatalf("关回开关后应只剩 1 个账号签到，实际 %d 次", got)
	}
}

// TestIncludeDisabledInTasksKeepsDisabledOutOfSelection 选号侧不受开关影响：
// 禁用号即使参与了保号任务，也依旧不会被选中。
func TestIncludeDisabledInTasksKeepsDisabledOutOfSelection(t *testing.T) {
	cfg := &config.Config{WorkDir: t.TempDir()}
	cfg.ApplyDefaults()
	on := true
	cfg.Schedule.IncludeDisabledInTasks = &on
	p := pool.New(cfg, nil)
	dir := t.TempDir()
	mk := func(name, uid string) *auth.Credential {
		cred := auth.NewForLogin(filepath.Join(dir, name), auth.SiteCN,
			"TEST_ACCESS", "TEST_REFRESH", 4102444800, "copilot.tencent.com", uid, "", uid)
		if err := cred.SaveFull(); err != nil {
			t.Fatalf("写入测试凭据失败: %v", err)
		}
		return cred
	}
	p.Add(mk("workbuddy-u1.json", "u1"))
	p.Add(mk("workbuddy-u2.json", "u2"))
	if err := p.SetDisabled(p.Accounts()[1], true, "手工禁用（测试）"); err != nil {
		t.Fatalf("禁用账号失败: %v", err)
	}
	cfg.ApplyDefaults()
	p.SetGov(pool.GovFromConfig(cfg))

	for i := 0; i < 20; i++ {
		acc, err := p.PickAccount(pool.PickOptions{})
		if err != nil {
			t.Fatalf("选号失败: %v", err)
		}
		if acc.IsDisabled() {
			t.Fatalf("禁用号不应被选中（第 %d 次）", i+1)
		}
		acc.ReleaseInFlight()
	}
}
