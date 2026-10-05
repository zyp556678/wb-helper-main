package auth

import (
	"encoding/base64"
	"testing"
)

// jwt 造一个不签名的 JWT（只有载荷有意义 —— 本模块刻意不校验签名）。
func jwt(payload string) string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

func TestJWTClaimsParsesPayload(t *testing.T) {
	claims := JWTClaims(jwt(`{"sub":"acct-1","iss":"https://x/auth/realms/cn"}`))
	if claims == nil {
		t.Fatal("应能解析出载荷")
	}
	if claims["sub"] != "acct-1" {
		t.Fatalf("sub 解析错误: %v", claims["sub"])
	}
}

// 非 JWT / 坏 base64 / 非 JSON 一律返回 nil —— 上游可能给非 JWT 形态的令牌，
// 那是**正常情况**，不是错误。返回 nil 让调用方跳过比对，而不是判成「不一致」。
func TestJWTClaimsReturnsNilForNonJWT(t *testing.T) {
	for _, tok := range []string{"", "not-a-jwt", "a.b", "a.!!!.c", jwt("not json")} {
		if got := JWTClaims(tok); got != nil {
			t.Fatalf("%q 应返回 nil，实际 %v", tok, got)
		}
	}
}

// realm 从 iss 的 `/auth/realms/` 之后截取。
func TestIdentityOfExtractsRealm(t *testing.T) {
	id, realm := IdentityOf(jwt(`{"sub":"acct-1","iss":"https://x.example/auth/realms/global"}`))
	if id != "acct-1" {
		t.Fatalf("accountID 错误: %q", id)
	}
	if realm != "global" {
		t.Fatalf("realm 应从 iss 截取，实际 %q", realm)
	}
}

// iss 里没有 `/auth/realms/` 时原样返回 —— 不猜、不截。
func TestIdentityOfRealmFallback(t *testing.T) {
	_, realm := IdentityOf(jwt(`{"sub":"a","iss":"https://x.example/simple"}`))
	if realm != "https://x.example/simple" {
		t.Fatalf("无标准前缀时应原样返回 iss，实际 %q", realm)
	}
}

// 缺 iss / sub 时返回空串（调用方据此跳过比对，而不是判成不一致）。
func TestIdentityOfMissingFields(t *testing.T) {
	id, realm := IdentityOf(jwt(`{}`))
	if id != "" || realm != "" {
		t.Fatalf("缺字段应返回空串，实际 id=%q realm=%q", id, realm)
	}
	id2, realm2 := IdentityOf("garbage")
	if id2 != "" || realm2 != "" {
		t.Fatalf("非 JWT 应返回空串，实际 id=%q realm=%q", id2, realm2)
	}
}

// ShortID 必须**不可逆**（不能直接截取原文）且长度稳定。
func TestShortID(t *testing.T) {
	a := ShortID("some-long-account-identifier")
	if len(a) != 12 {
		t.Fatalf("应为 12 位，实际 %d: %q", len(a), a)
	}
	if a == "some-long-ac" {
		t.Fatal("不能直接截取原文（那等于没脱敏）")
	}
	if ShortID("some-long-account-identifier") != a {
		t.Fatal("同一输入必须稳定")
	}
	if ShortID("other") == a {
		t.Fatal("不同输入应不同")
	}
	if ShortID("") != "" {
		t.Fatal("空输入应返回空串")
	}
}
