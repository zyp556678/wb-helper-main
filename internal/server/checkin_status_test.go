package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/upstream"
)

// 签到状态的查询路径必须**两个都在、且有先后**。
//
// 只认新端点的话，上游一旦把 `checkin-activity-status` 摘掉，签到状态会整体退化
// 成「查不到」——而这时旧端点其实还是好的。这是照抄上游客户端的回落行为。
func TestCheckinStatusURLsOrderAndOrigin(t *testing.T) {
	for _, p := range []*upstream.Profile{upstream.ProfileForSite("cn"), upstream.ProfileForSite("intl")} {
		urls := p.CheckinStatusURLs()
		if len(urls) != 2 {
			t.Fatalf("%s：应有 2 个候选路径，实际 %d", p.Label, len(urls))
		}
		if !strings.HasSuffix(urls[0], "/v2/billing/meter/checkin-activity-status") {
			t.Fatalf("%s：首选应为 checkin-activity-status，实际 %s", p.Label, urls[0])
		}
		if !strings.HasSuffix(urls[1], "/v2/billing/meter/checkin-status") {
			t.Fatalf("%s：回退应为 checkin-status，实际 %s", p.Label, urls[1])
		}
		for _, u := range urls {
			// 必须是站点 **Web 域**（Origin），不是 CLI 域（Base）——
			// 签到族端点在 Web 域，走错域名会 404。
			if !strings.HasPrefix(u, p.Origin) {
				t.Fatalf("%s：应以 Origin(%s) 开头，实际 %s", p.Label, p.Origin, u)
			}
		}
	}
}

// 缓存必须会过期、且能被主动失效。
//
// 不过期 → 昨天查到的「已签到」会一直显示到今天；
// 不失效 → 用户刚点完签到，卡片还显示「今日未签到」。
func TestCheckinStatusCacheTTLAndInvalidate(t *testing.T) {
	c := newCheckinStatusCache()
	now := time.Now()

	if _, ok := c.get("a.json", now); ok {
		t.Fatal("空缓存不应命中")
	}

	c.put("a.json", CheckinStatusView{TodayCheckedIn: true, CheckedAt: now.Unix()})
	v, ok := c.get("a.json", now)
	if !ok || !v.TodayCheckedIn {
		t.Fatal("刚写入的项应命中")
	}

	// TTL 内命中。
	if _, ok := c.get("a.json", now.Add(checkinStatusTTL-time.Second)); !ok {
		t.Fatal("TTL 内应命中")
	}
	// 过期不命中。
	if _, ok := c.get("a.json", now.Add(checkinStatusTTL+time.Second)); ok {
		t.Fatal("超过 TTL 不应命中")
	}

	// 主动失效。
	c.put("a.json", CheckinStatusView{TodayCheckedIn: true, CheckedAt: now.Unix()})
	c.invalidate("a.json")
	if _, ok := c.get("a.json", now); ok {
		t.Fatal("invalidate 后不应命中")
	}
	// 失效不存在的键不应 panic。
	c.invalidate("never-seen.json")
}

// 端到端：批量签到状态。接一个假上游，验证四件事 ——
//  1. 国内站会打到 checkin-activity-status 并正确解析 today_checked_in；
//  2. **国际站一个请求都不发**（它是能力缺失，不是失败）；
//  3. 首选端点 404 时回落到 checkin-status；
//  4. TTL 缓存生效：第二次调用不再打上游。
//
// 这四条都是「不测就不知道」的行为：把它们合成一条断言「返回了 4 个键」
// 只能证明函数被调用了，证明不了它做对了什么。
func TestAccountsCheckinStatusEndToEnd(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
		// mode 控制假上游的行为，分三段推进：
		//   ok          —— 首选端点正常返回
		//   activity404 —— 首选 404，验证回落到 checkin-status
		//   both404     —— 两个端点都 404，验证如实报错
		mode = "ok"
	)
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		m := mode
		mu.Unlock()

		isActivity := strings.HasSuffix(r.URL.Path, "checkin-activity-status")
		switch {
		case m == "both404":
			w.WriteHeader(http.StatusNotFound)
			return
		case m == "activity404" && isActivity:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":true}}`))
	}))
	defer upstreamSrv.Close()

	// 上游地址通过全局 Profile 注入（见 degrade_test.go 的 newTestServer）。
	origCN := upstream.ProfileCN
	upstream.ProfileCN = withURL(origCN, upstreamSrv.URL)
	defer func() { upstream.ProfileCN = origCN }()

	srv := newTestServer(t, upstreamSrv.URL)

	// 再加一个国际站账号：它必须完全不产生请求。
	intlPath := filepath.Join(t.TempDir(), "workbuddy-intl.json")
	intlCred := auth.NewForLogin(intlPath, auth.SiteINTL, "T2", "R2", 4102444800,
		"www.workbuddy.ai", "u2", "", "国际测试")
	if err := intlCred.SaveFull(); err != nil {
		t.Fatal(err)
	}
	srv.pool.Add(intlCred)

	call := func() map[string]CheckinStatusView {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/checkin-status", strings.NewReader("{}"))
		srv.handleAccountsCheckinStatus(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Accounts map[string]CheckinStatusView `json:"accounts"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是 JSON: %v", err)
		}
		return out.Accounts
	}
	takePaths := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := append([]string(nil), paths...)
		paths = nil
		return out
	}
	setMode := func(m string) {
		mu.Lock()
		mode = m
		mu.Unlock()
	}

	// ── 第一段：首选端点正常 ────────────────────────────────────────────────
	accounts := call()
	if len(accounts) != 2 {
		t.Fatalf("应有 2 个账号的状态，实际 %d", len(accounts))
	}
	cn := accounts["workbuddy-test.json"]
	if cn.Error != "" {
		t.Fatalf("国内站查询不该失败: %s", cn.Error)
	}
	if cn.Unsupported {
		t.Fatal("国内站不应被标为 unsupported")
	}
	if !cn.TodayCheckedIn {
		t.Fatal("国内站应解析出 today_checked_in=true")
	}
	intl := accounts["workbuddy-intl.json"]
	if !intl.Unsupported {
		t.Fatal("国际站应标为 unsupported")
	}
	if intl.TodayCheckedIn {
		t.Fatal("unsupported 时不能断言今日已签到")
	}
	// 只应有国内站那一次请求：**国际站一个字节都没发**。
	got := takePaths()
	if len(got) != 1 || !strings.HasSuffix(got[0], "/v2/billing/meter/checkin-activity-status") {
		t.Fatalf("应只发 1 次且打在 activity-status，实际: %v", got)
	}

	// ── 第二段：TTL 缓存 ───────────────────────────────────────────────────
	_ = call()
	if got := takePaths(); len(got) != 0 {
		t.Fatalf("TTL 内第二次调用不应再打上游，实际: %v", got)
	}

	// ── 第三段：首选 404 → 回落 ────────────────────────────────────────────
	setMode("activity404")
	srv.checkinCache.invalidate("workbuddy-test.json")
	accounts = call()
	got = takePaths()
	var sawActivity, sawFallback bool
	for _, p := range got {
		if strings.HasSuffix(p, "checkin-activity-status") {
			sawActivity = true
		}
		if strings.HasSuffix(p, "/v2/billing/meter/checkin-status") {
			sawFallback = true
		}
	}
	if !sawActivity || !sawFallback {
		t.Fatalf("首选 404 时应先打 activity-status 再回落 checkin-status，实际: %v", got)
	}
	if !accounts["workbuddy-test.json"].TodayCheckedIn {
		t.Fatal("回落到旧端点后应仍能解析出已签到")
	}

	// ── 第四段：两个端点都 404 → 如实报错 ─────────────────────────────────
	setMode("both404")
	srv.checkinCache.invalidate("workbuddy-test.json")
	accounts = call()
	cn = accounts["workbuddy-test.json"]
	if cn.Error == "" {
		t.Fatal("两个端点都 404 时必须报错，不能退化成一个「今日未签到」")
	}
	if cn.TodayCheckedIn {
		t.Fatal("报错时不能断言任何签到状态")
	}
	// 失败不进缓存：再查应该会重试（而不是把错误钉住 5 分钟）。
	takePaths()
	_ = call()
	if got := takePaths(); len(got) == 0 {
		t.Fatal("失败结果不应进缓存，重查应当重新打上游")
	}
}

// withURL 把 Profile 的两个域都指向假上游。
//
// Base 与 Origin 都要换：签到族端点在 **Origin**（Web 域）上，
// 只改 Base 会绕过假上游、真去打 www.codebuddy.cn。
func withURL(p upstream.Profile, base string) upstream.Profile {
	p.Base = base
	p.Origin = base
	return p
}
