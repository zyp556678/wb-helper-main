package debuglog

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
)

// 本文件是「完整请求诊断」（debug.full_requests）的实现，对照 wb-gateway v1.14.21。
//
// 与普通调试事件的分界：普通事件只记**元数据**（大小、摘要、状态码），
// 这里在显式打开 full_requests 后记录**正文的可读副本**——排查协议转译 / 客户端差异时，
// 只有正文能回答「到底发出去的是什么」。
//
// 三条硬约束：
//  1. 两个开关都开才生效：debug.enabled（sink 存在）× debug.full_requests（本开关）。
//  2. 只覆盖模型 API（chat / responses / messages）；管理配置、登录、凭据接口
//     即使开关打开也**不记录正文**——那些请求体里是密钥与密码。
//  3. 脱敏在**结构化之后**做：先解析 JSON 再按字段名遮蔽，比正则扫全文可靠；
//     解析失败才退回文本级脱敏。已知凭据、URL userinfo / 敏感 query 都会被抹掉，
//     但正文里的任意写法无法保证 100% 识别 —— 这一点在文档里明说。
var fullRequests atomic.Bool

// MaxFullBodyBytes 是单条完整正文的上限（超出截断并标记）。
// 上游没有限制；这里加一条是因为 debug JSONL 是单文件追加，
// 一个多兆的请求会把日志撑到不可读，也会拖慢写盘。
const MaxFullBodyBytes = 512 << 10

// SetFullRequests 打开 / 关闭完整请求记录（配置热更新时调用）。
func SetFullRequests(on bool) { fullRequests.Store(on) }

// FullRequests 报告完整请求记录开关是否打开。
func FullRequests() bool { return fullRequests.Load() }

// FullRequestEnabled 报告本次请求是否应记录完整正文。
func FullRequestEnabled(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost || !Enabled() || !fullRequests.Load() {
		return false
	}
	switch r.URL.Path {
	case "/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses",
		"/v1/messages", "/messages":
		return true
	}
	// 管理配置 / 登录 / 凭据接口不在采集范围。
	return false
}

// FullRequest 写一条完整请求事件（只有两个开关都打开时才真正落盘）。
func FullRequest(r *http.Request, event string, fields map[string]any) {
	if !FullRequestEnabled(r) {
		return
	}
	Event(r, "debug", event, fields)
}

var (
	fullRequestTokenPattern      = regexp.MustCompile(`(?:github_pat_[A-Za-z0-9_]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{16,})`)
	fullRequestPrivateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)
)

// FullBodyFields 把请求体折算成可落盘的字段：脱敏后的正文 + 大小 + 摘要 + 截断标记。
func FullBodyFields(body []byte) map[string]any {
	fields := map[string]any{
		"body_bytes":     len(body),
		"body_truncated": false,
	}
	if len(body) == 0 {
		return fields
	}
	sum := sha256.Sum256(body)
	fields["body_sha256_8"] = hex.EncodeToString(sum[:])[:8]

	raw := body
	if len(raw) > MaxFullBodyBytes {
		raw = raw[:MaxFullBodyBytes]
		fields["body_truncated"] = true
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err == nil {
		fields["body"] = RedactValue(parsed)
	} else {
		// 非 JSON（或截断导致半截 JSON）：退回文本级脱敏。
		fields["body"] = RedactText(string(raw))
	}
	return fields
}

// FullHeaderFields 把请求头折算成脱敏后的 map（同名多值合并为数组）。
func FullHeaderFields(header http.Header) map[string]any {
	out := make(map[string]any, len(header))
	for key, values := range header {
		if fullRequestSecretField(key) {
			out[key] = "[REDACTED]"
			continue
		}
		cleaned := make([]string, 0, len(values))
		for _, value := range values {
			cleaned = append(cleaned, RedactText(value))
		}
		if len(cleaned) == 1 {
			out[key] = cleaned[0]
		} else {
			out[key] = cleaned
		}
	}
	return out
}

// RedactValue 递归脱敏一个 JSON 值：敏感字段名整值遮蔽，字符串做 token / URL 处理。
func RedactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, sub := range typed {
			if fullRequestSecretField(key) {
				out[key] = "[REDACTED]"
				continue
			}
			out[key] = RedactValue(sub)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, sub := range typed {
			out[i] = RedactValue(sub)
		}
		return out
	case string:
		if strings.HasPrefix(typed, "https://") || strings.HasPrefix(typed, "http://") {
			return RedactURL(typed)
		}
		return RedactText(typed)
	default:
		return value
	}
}

// RedactText 抹掉文本里的已知密钥形态。
//
// 只处理「可枚举的固定格式」：GitHub token、sk- 前缀密钥、PEM 私钥。
// 任意写法的秘密无能为力 —— 这是文档里明确写出的边界，不假装能全覆盖。
func RedactText(text string) string {
	text = fullRequestTokenPattern.ReplaceAllString(text, "[REDACTED]")
	text = fullRequestPrivateKeyPattern.ReplaceAllString(text, "[REDACTED PRIVATE KEY]")
	return text
}

// RedactURL 去掉 userinfo，并遮蔽 query 里的敏感键值。
func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return RedactText(raw)
	}
	if parsed.User != nil {
		parsed.User = url.User("[REDACTED]")
	}
	if parsed.RawQuery != "" {
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			parsed.RawQuery = "query=[REDACTED]" // 解析不了就整段遮蔽，宁可少信息
		} else {
			for key, values := range query {
				if fullRequestSecretField(key) {
					query[key] = []string{"[REDACTED]"}
					continue
				}
				for i := range values {
					values[i] = RedactText(values[i])
				}
			}
			parsed.RawQuery = query.Encode()
		}
	}
	return RedactText(parsed.String())
}

// fullRequestSecretField 判断字段名是否属于「值不能落盘」的一类。
//
// 归一化时去掉 _ - . 三种分隔符并转小写，这样 Authorization / api-key / api_key
// 会落到同一个判定上。宁可多遮蔽：多遮一个普通字段只是少点信息，漏一个就是泄露。
func fullRequestSecretField(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(key))
	return secretLogField(key) ||
		strings.Contains(normalized, "authorization") ||
		strings.Contains(normalized, "apikey") ||
		strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "cookie") ||
		strings.HasSuffix(normalized, "token") ||
		strings.HasSuffix(normalized, "signature") ||
		normalized == "key" || normalized == "sig" ||
		normalized == "credential" || normalized == "credentials"
}

// secretLogField 是既有调试事件里已在用的敏感字段判定（保持同一套口径）。
func secretLogField(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "api_key",
		"cookie", "set-cookie", "password", "passwd", "secret", "client_secret",
		"access_token", "refresh_token", "id_token":
		return true
	}
	return false
}

// debugRequestDigestKey 是进程内随机生成的 HMAC 密钥：
// 摘要只在本次运行内可比（判断「同一份内容」），进程退出即不可复算。
var debugRequestDigestKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil
	}
	return key
}()

// Digest 返回一个值的 HMAC-SHA256 前 16 位十六进制。
//
// 用途：把「处理前」与「处理后」的请求体各算一个摘要并排写出，
// 一眼看出改写到底动了哪里，而不必把两份正文都读一遍。
func Digest(value any) string {
	if len(debugRequestDigestKey) == 0 {
		return "unavailable"
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return "unavailable"
	}
	return digestBytes(payload)
}

// DigestBytes 直接对字节串算摘要（用于「处理前 / 处理后」的原始体积比对）。
func DigestBytes(body []byte) string {
	if len(debugRequestDigestKey) == 0 {
		return "unavailable"
	}
	return digestBytes(body)
}

func digestBytes(payload []byte) string {
	mac := hmac.New(sha256.New, debugRequestDigestKey)
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))[:16]
}
