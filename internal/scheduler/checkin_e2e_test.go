package scheduler

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/upstream"
)

// 端到端：启动时 checkHourlyTasks(dayOpen=true) 必须真的向上游发 checkin 请求。
// 这是「完全没签」的回归测试。
//
// 用 RoundTripper 劫持而不是 httptest：Profile 的域名是写死的，
// 直接打真上游会让测试依赖网络与真实令牌。
func TestCheckHourlyTasksFiresCheckinOnStartup(t *testing.T) {
	var called atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called.Add(1)
		return jsonResponse(`{"code":0,"msg":""}`), nil
	})

	dir := t.TempDir()
	cred := `{"accessToken":"at","refreshToken":"rt","uid":"u1","nickname":"n","domain":"codebuddy.cn","expiresAt":4102444800}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-t.json"), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{WorkDir: dir}
	cfg.ApplyDefaults()
	client := &upstream.Client{Control: &http.Client{Transport: rt}, ChatHTTP: &http.Client{Transport: rt}}
	// 池与调度器共用同一个 client：签到之后的额度刷新也走它，
	// 用 nil 会在 fetchQuota 里空指针崩掉（不是签到本身的问题）。
	p := pool.New(cfg, client)
	if n, errs := p.Load(); n != 1 {
		t.Fatalf("应加载 1 个账号，实际 %d（errs=%v）", n, errs)
	}

	s := New(cfg, p, client)
	s.checkHourlyTasks(context.Background(), true)

	if called.Load() == 0 {
		t.Fatal("启动首次检查没有发出任何请求 —— 这正是「完全没签」")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 关键回归：签到整轮失败时槽位必须归还，下一个 tick 还能再试。
// 之前 claim 在失败后也占着当天槽位，一次启动期失败 = 当天彻底不签。
func TestCheckinSlotReleasedAfterTotalFailure(t *testing.T) {
	var calls atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if strings.Contains(r.URL.Path, "daily-checkin") {
			return &http.Response{
				StatusCode: 500,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":500,"msg":"boom"}`)),
			}, nil
		}
		return jsonResponse(`{"code":0,"msg":"","data":{}}`), nil
	})

	dir := t.TempDir()
	cred := `{"accessToken":"at","refreshToken":"rt","uid":"u1","nickname":"n","domain":"codebuddy.cn","expiresAt":4102444800}`
	_ = os.WriteFile(filepath.Join(dir, "workbuddy-t.json"), []byte(cred), 0o600)

	cfg := &config.Config{WorkDir: dir}
	cfg.ApplyDefaults()
	client := &upstream.Client{Control: &http.Client{Transport: rt}, ChatHTTP: &http.Client{Transport: rt}}
	p := pool.New(cfg, client)
	p.Load()
	s := New(cfg, p, client)

	s.checkHourlyTasks(context.Background(), true)
	afterFirst := calls.Load()
	if afterFirst == 0 {
		t.Fatal("第一轮应至少发一次签到请求")
	}
	if s.LastRun("checkin") != "" {
		t.Fatalf("整轮失败后槽位应被归还，实际仍占着 %q", s.LastRun("checkin"))
	}
	// 下一分钟（dayOpen=false）必须还能再次触发。
	s.checkHourlyTasks(context.Background(), false)
	if calls.Load() <= afterFirst {
		t.Fatal("失败后下一个 tick 应重试，实际没有发出新请求")
	}
}
