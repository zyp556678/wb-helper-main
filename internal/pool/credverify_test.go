package pool

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/upstream"
)

// stubQuotaServer 造一个只回指定状态码的假上游。
//
// 用它而不是 mock 接口：这条校验的**全部意义就是区分状态码**
// （401/403 = 明确拒绝，其它 = 无法判定），用真实 HTTP 往返才验得到。
func stubQuotaServer(t *testing.T, status int) (*httptest.Server, *upstream.Profile) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status >= 400 {
			_, _ = w.Write([]byte(`{"code":401,"msg":"unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"Packages":[]}}`))
	}))
	t.Cleanup(srv.Close)
	prof := &upstream.Profile{
		Key: "test", Label: "测试", Base: srv.URL, Origin: srv.URL,
		Platform: "web", ClientUA: "test-ua", ClientID: "cid", ClientVer: "1.0", Product: "test",
	}
	return srv, prof
}

// newVerifyPool 造一个只带 http 客户端、没有任何账号的池。
//
// 用真实的 *http.Client（指向 httptest 服务器）而不是 mock：
// 这条校验的全部意义就是区分 HTTP 状态码，用真实往返才验得到。
func newVerifyPool(t *testing.T, prof *upstream.Profile, timeout time.Duration) *Pool {
	t.Helper()
	cfg := &config.Config{WorkDir: t.TempDir()}
	cfg.ApplyDefaults()
	return New(cfg, &upstream.Client{Control: &http.Client{Timeout: timeout}})
}

func view(token string) *upstream.CredentialView {
	return &upstream.CredentialView{AccessToken: token, UID: "u1"}
}

// 上游正常返回 → 可用。
func TestVerifyCredentialUsable(t *testing.T) {
	_, prof := stubQuotaServer(t, http.StatusOK)
	p := newVerifyPool(t, prof, 3*time.Second)
	v, err := p.verifyCredential(context.Background(), view("tok"), prof)
	if v != verdictUsable || err != nil {
		t.Fatalf("应判可用，实际 verdict=%v err=%v", v, err)
	}
}

// 401 / 403 → **明确拒绝**（凭据确实不可用）。
func TestVerifyCredentialRejected(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		_, prof := stubQuotaServer(t, status)
		p := newVerifyPool(t, prof, 3*time.Second)
		v, _ := p.verifyCredential(context.Background(), view("tok"), prof)
		if v != verdictRejected {
			t.Fatalf("HTTP %d 应判「明确拒绝」，实际 %v", status, v)
		}
	}
}

// 5xx / 连不上 → **无法判定**，绝不能与「明确拒绝」混为一谈。
//
// 这两个结论的处置完全相反：明确拒绝可以删凭据，无法判定必须保守保留。
// 混起来的后果是「网络抖一下就把还能用的凭据删了」——不可逆。
func TestVerifyCredentialUnknown(t *testing.T) {
	// 5xx
	_, prof5 := stubQuotaServer(t, http.StatusInternalServerError)
	p := newVerifyPool(t, prof5, 3*time.Second)
	if v, _ := p.verifyCredential(context.Background(), view("tok"), prof5); v != verdictUnknown {
		t.Fatalf("5xx 应判「无法判定」，实际 %v", v)
	}

	// 连不上（端口关掉）
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	profDead := &upstream.Profile{Key: "test", Base: url, Origin: url, Platform: "web", ClientUA: "ua"}
	p2 := newVerifyPool(t, profDead, 3*time.Second)
	if v, _ := p2.verifyCredential(context.Background(), view("tok"), profDead); v != verdictUnknown {
		t.Fatalf("连接失败应判「无法判定」，实际 %v", v)
	}
}

// 身份不一致必须**在探测之前**就拒绝：这是防「上游串号把别人的令牌写进来」，
// 而那种情况下探测本身也会返回别人的数据（看起来是「可用」）。
func TestValidateRefreshedCredentialRejectsIdentityMismatch(t *testing.T) {
	_, prof := stubQuotaServer(t, http.StatusOK)
	p := newVerifyPool(t, prof, 3*time.Second)

	oldTok := jwtWith("account-A", "https://x/auth/realms/cn")
	newTok := jwtWith("account-B", "https://x/auth/realms/cn")
	err := p.validateRefreshedCredential(oldTok, view(newTok), prof)
	if err == nil {
		t.Fatal("账号标识不一致应拒绝覆盖")
	}
	if !strings.Contains(err.Error(), "账号标识") {
		t.Fatalf("错误信息应说明是账号不一致: %v", err)
	}
}

// realm（站点）不一致同样拒绝。
func TestValidateRefreshedCredentialRejectsRealmMismatch(t *testing.T) {
	_, prof := stubQuotaServer(t, http.StatusOK)
	p := newVerifyPool(t, prof, 3*time.Second)

	oldTok := jwtWith("account-A", "https://x/auth/realms/cn")
	newTok := jwtWith("account-A", "https://x/auth/realms/global")
	if err := p.validateRefreshedCredential(oldTok, view(newTok), prof); err == nil {
		t.Fatal("站点不一致应拒绝覆盖")
	}
}

// 一致且可用 → 放行。
func TestValidateRefreshedCredentialPasses(t *testing.T) {
	_, prof := stubQuotaServer(t, http.StatusOK)
	p := newVerifyPool(t, prof, 3*time.Second)
	tok := jwtWith("account-A", "https://x/auth/realms/cn")
	if err := p.validateRefreshedCredential(tok, view(tok), prof); err != nil {
		t.Fatalf("一致且可用应放行，实际 %v", err)
	}
}

// 探测不可用时**按通过处理**：刷新接口已经成功，不该因一次探测抖动丢掉新凭据。
func TestValidateRefreshedCredentialToleratesUnknown(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	prof := &upstream.Profile{Key: "test", Base: url, Origin: url, Platform: "web", ClientUA: "ua"}
	p := newVerifyPool(t, prof, 3*time.Second)

	tok := jwtWith("account-A", "https://x/auth/realms/cn")
	if err := p.validateRefreshedCredential(tok, view(tok), prof); err != nil {
		t.Fatalf("无法判定时应按通过处理，实际 %v", err)
	}
}

// 新令牌不可用（明确拒绝）→ 拒绝覆盖，保留旧凭据。
func TestValidateRefreshedCredentialRejectsUnusable(t *testing.T) {
	_, prof := stubQuotaServer(t, http.StatusUnauthorized)
	p := newVerifyPool(t, prof, 3*time.Second)
	tok := jwtWith("account-A", "https://x/auth/realms/cn")
	err := p.validateRefreshedCredential(tok, view(tok), prof)
	if err == nil || !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Fatalf("新凭据不可用应拒绝覆盖，实际 %v", err)
	}
}

// 解析不出的令牌（非 JWT）→ 跳过一致性比对，不误判成「不一致」。
func TestValidateRefreshedCredentialSkipsUnparsable(t *testing.T) {
	_, prof := stubQuotaServer(t, http.StatusOK)
	p := newVerifyPool(t, prof, 3*time.Second)
	if err := p.validateRefreshedCredential("not-a-jwt", view("also-not-a-jwt"), prof); err != nil {
		t.Fatalf("无法解析声明的令牌不该被当成不一致: %v", err)
	}
}

// 删除前校验：无法判定 → **保守保留**（删除不可逆）。
func TestDeleteKeepOnUnknown(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()
	prof := &upstream.Profile{Key: "test", Base: url, Origin: url, Platform: "web", ClientUA: "ua"}
	p := newVerifyPool(t, prof, 3*time.Second)

	acc := p.newAccount(&auth.Credential{Path: "x.json", AccessToken: "tok", Edition: "cn"})
	keep, note := p.CredentialStillUsableForDelete(context.Background(), acc, prof)
	if !keep {
		t.Fatalf("无法判定时应保守保留，实际 keep=false（%s）", note)
	}
}

// 删除前校验：仍可用 → 保留（这正是要防的「销毁还能用的凭据」）。
func TestDeleteKeepWhenStillUsable(t *testing.T) {
	_, prof := stubQuotaServer(t, http.StatusOK)
	p := newVerifyPool(t, prof, 3*time.Second)
	acc := p.newAccount(&auth.Credential{Path: "x.json", AccessToken: "tok", Edition: "cn"})
	keep, note := p.CredentialStillUsableForDelete(context.Background(), acc, prof)
	if !keep {
		t.Fatalf("凭据仍可用时应保留，实际 keep=false（%s）", note)
	}
	if !strings.Contains(note, "保留") {
		t.Fatalf("说明应讲清为什么保留: %s", note)
	}
}

// 删除前校验：明确不可用 → 可以删。
func TestDeleteAllowsWhenRejected(t *testing.T) {
	_, prof := stubQuotaServer(t, http.StatusForbidden)
	p := newVerifyPool(t, prof, 3*time.Second)
	acc := p.newAccount(&auth.Credential{Path: "x.json", AccessToken: "tok", Edition: "cn"})
	keep, note := p.CredentialStillUsableForDelete(context.Background(), acc, prof)
	if keep {
		t.Fatalf("上游明确拒绝时可以删除，实际 keep=true（%s）", note)
	}
}

// 无令牌 → 按已失效处理（没有可校验的东西）。
func TestDeleteWithoutToken(t *testing.T) {
	_, prof := stubQuotaServer(t, http.StatusOK)
	p := newVerifyPool(t, prof, 3*time.Second)
	acc := p.newAccount(&auth.Credential{Path: "x.json", Edition: "cn"})
	if keep, _ := p.CredentialStillUsableForDelete(context.Background(), acc, prof); keep {
		t.Fatal("无令牌时应按已失效处理")
	}
}

// jwtWith 造一个不签名的 JWT（只有载荷有意义）。
func jwtWith(sub, iss string) string {
	payload := `{"sub":"` + sub + `","iss":"` + iss + `"}`
	return "h." + b64url(payload) + ".s"
}

func b64url(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
