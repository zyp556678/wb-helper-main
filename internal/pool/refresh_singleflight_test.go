package pool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/upstream"
)

// 同账号 refresh 单飞（对照 wb2api-panel「同账号 refresh 单飞，避免 RT 复用撞 12153」）。
//
// 要复现的竞态：并发请求同时发现令牌过期，一起排队等账号锁。等它们拿到锁时，
// 第一个已经把令牌换新、refreshToken 也随之轮换；若第二个仍用锁外抓到的旧 RT
// 再刷一次，上游会判成 refresh token 复用并返回 12153，账号直接被踢下线。
//
// 所以断言很简单也很硬：**并发 N 次，上游刷新端点只能被打到 1 次**。
func TestRefreshTokenSingleFlight(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "token/refresh") {
			mu.Lock()
			calls++
			mu.Unlock()
			// 两层都要对：外层是网关统一的 code/msg/data 包络（doJSON 解的是 data），
			// 内层字段名是 expiresIn（相对秒数）而不是 expiresAt —— refreshToken 只在
			// tok.ExpiresIn > 0 时更新 cred.ExpiresAt。任一处写错都会让「刷了但还是过期」，
			// 于是每个协程都再刷一次，测试会以「8 次」的形式暴露它。
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"new-token","refreshToken":"rt-2","expiresIn":7200}}`))
			return
		}
		// 凭据校验 / 额度查询一律成功，避免校验失败把写回挡掉、让后续协程再刷一次。
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base = srv.URL
	upstream.ProfileCN.Origin = srv.URL
	defer func() {
		upstream.ProfileCN.Base = oldBase
		upstream.ProfileCN.Origin = oldOrigin
	}()

	cfg := &config.Config{WorkDir: t.TempDir()}
	cfg.ApplyDefaults()
	p := New(cfg, &upstream.Client{Control: &http.Client{Timeout: 5 * time.Second}})

	// 令牌已过期：每个协程进来都会判成「需要刷新」。
	// Path 必须落在临时目录里：refreshToken 成功后会 SaveAtomic() 写回该路径，
	// 用相对路径会把凭据文件写到包目录下（测试污染源码树）。
	cred := &auth.Credential{
		Path:         filepath.Join(t.TempDir(), "cn-a.json"),
		AccessToken:  "old-token",
		RefreshToken: "rt-1",
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
		Edition:      "cn",
	}
	acc := p.newAccount(cred)
	p.accounts = append(p.accounts, acc)

	const goroutines = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 尽量让它们同时起跑，把竞态窗口放大
			_ = p.refreshToken(context.Background(), acc, false, false)
		}()
	}
	close(start)
	wg.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()

	if got != 1 {
		t.Fatalf("并发 %d 次刷新只应打 1 次上游，实际 %d 次 —— "+
			"多于 1 次说明锁内没有二次确认，真实上游会判 RT 复用并返回 12153",
			goroutines, got)
	}
}

// TestRefreshTokenSkipsWhenAlreadyFresh 锁内二次确认的正向行为：
// 令牌已经有效时不该刷新（这既是性能，也是「别把好令牌换掉」的正确性）。
func TestRefreshTokenSkipsWhenAlreadyFresh(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "token/refresh") {
			mu.Lock()
			calls++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	defer srv.Close()

	oldBase, oldOrigin := upstream.ProfileCN.Base, upstream.ProfileCN.Origin
	upstream.ProfileCN.Base = srv.URL
	upstream.ProfileCN.Origin = srv.URL
	defer func() {
		upstream.ProfileCN.Base = oldBase
		upstream.ProfileCN.Origin = oldOrigin
	}()

	cfg := &config.Config{WorkDir: t.TempDir()}
	cfg.ApplyDefaults()
	p := New(cfg, &upstream.Client{Control: &http.Client{Timeout: 5 * time.Second}})

	cred := &auth.Credential{
		Path:         filepath.Join(t.TempDir(), "cn-b.json"),
		AccessToken:  "fresh-token",
		RefreshToken: "rt-1",
		ExpiresAt:    time.Now().Add(24 * time.Hour).Unix(),
		Edition:      "cn",
	}
	acc := p.newAccount(cred)
	p.accounts = append(p.accounts, acc)

	if err := p.refreshToken(context.Background(), acc, false, false); err != nil {
		t.Fatalf("未过期时不该报错: %v", err)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 0 {
		t.Fatalf("令牌仍有效时不该刷新，实际打了 %d 次上游", got)
	}
}
