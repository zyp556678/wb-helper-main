// 凭据身份解析（切片 18）。
//
// 用途只有一个：在**不可逆动作之前**证明新旧凭据属于同一个账号。
//   - 刷新令牌后写回文件之前：防止上游串号或响应错配，把 A 账号的新令牌
//     写到 B 账号的凭据文件上 —— 那会让 B 账号彻底失效，而且看不出原因。
//   - 删除凭据之前：确认要删的这个确实已经不能用。
//
// 刻意**不校验 JWT 签名**：这里只做「本地一致性比对」，不是信任判定。
// 令牌能不能用由上游说了算（见 pool 里的只读校验）。
package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// JWTClaims 解码 JWT 载荷（**不做签名校验**）。
// 解析失败返回 nil —— 上游可能返回非 JWT 形态的令牌，那不是错误，只是没有可比对的声明。
func JWTClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil
	}
	return claims
}

// ShortID 生成不可逆的短标识，用于日志里指代账号而不暴露原始标识。
func ShortID(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// IdentityOf 取出访问令牌声明的账号标识（sub）与站点 realm（从 iss 里截取）。
//
// realm 的取法：iss 形如 `https://.../auth/realms/<realm>`，截取最后一段。
// 取不到时返回空串 —— 调用方据此跳过比对，而不是当成「不一致」。
func IdentityOf(accessToken string) (accountID, realm string) {
	claims := JWTClaims(accessToken)
	if claims == nil {
		return "", ""
	}
	if sub, ok := claims["sub"].(string); ok {
		accountID = sub
	}
	iss, _ := claims["iss"].(string)
	if iss == "" {
		return accountID, ""
	}
	if i := strings.Index(iss, "/auth/realms/"); i >= 0 {
		return accountID, iss[i+len("/auth/realms/"):]
	}
	return accountID, iss
}
