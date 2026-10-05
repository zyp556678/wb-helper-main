package upstream

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrClass 是上游错误的统一分类。判定优先级：余额耗尽 → session 失效 → 限流文案 → 状态码兜底。
//
// 分类决定「罚不罚账号」：请求体畸形与内容拦截是请求侧问题，换号也会再撞，
// 因此不冷却、不熔断、不计连败，只做轮换与降级重试。
type ErrClass struct {
	Kind      string // 见下方 Err* 常量
	Status    int
	Message   string
	Code      int
	ResetAt   time.Time // 上游给出的恢复墙钟（6004 / 限流文案带「将在 … 重置」时）
	Model     string    // 6004 的触发模型（用于模型级冷却与切模型豁免）
	Retryable bool
}

// 错误类别。
const (
	ErrHardCredit   = "hard_credit"   // 余额耗尽：账号级硬冷却到次日 04:00
	ErrSessionDead  = "session_dead"  // 会话失效：连续 N 次才禁用账号
	ErrSoftRate     = "soft_rate"     // 账号级限流：有界指数退避
	ErrModelRate    = "model_rate"    // 模型级限流（6004）：只冷却该模型，切模型立即可用
	ErrContentBlock = "content_block" // 内容拦截：不罚账号
	ErrBadRequest   = "bad_request"   // 请求体畸形（11101 等）：不罚账号
	ErrUpstream5xx  = "upstream_5xx"  // 服务端错误：喂熔断器
	ErrNetwork      = "network"       // 网络层错误
	ErrUnknown      = "unknown"       // 其余：连败降权
)

// hardCreditMarkers 是余额耗尽的关键词（不限状态码，上游偶尔用 400 表达）。
var hardCreditMarkers = []string{
	"credits exhausted", "insufficient credit", "insufficient balance",
	"quota exhausted", "余额不足", "积分不足", "额度已用完",
}

// sessionDeadMarkers 是会话失效的关键词。
var sessionDeadMarkers = []string{
	"offline user session not found", "session not found", "session expired",
}

// rateLimitMarkers 是限流关键词（不限状态码）。
var rateLimitMarkers = []string{
	"rate limit", "rate-limit", "too many requests", "请求过于频繁", "频率限制", "限流",
}

// contentBlockMarkers 是内容拦截关键词。
var contentBlockMarkers = []string{
	"blocked by security policy", "unapproved channel", "illegal api invocation",
	"内容安全", "安全策略",
}

// badRequestMarkers 是请求体畸形关键词（客户端问题，不罚账号）。
var badRequestMarkers = []string{
	"unmarshal chat params failed", "invalid_request", "malformed",
}

// resetTimePatterns 解析上游文案里的恢复时刻。
var resetTimePatterns = []*regexp.Regexp{
	// 将在 2026-09-17 14:57:00 重置
	regexp.MustCompile(`(?:将在|将于|reset at|available at|reset)\s*[:：]?\s*(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})`),
	regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})`),
}

// codePattern 提取响应体里的业务码（形如 "code":6004 / "code": "6004" / code=6004）。
var codePattern = regexp.MustCompile(`"code"\s*:\s*"?(\d+)"?|code\s*=\s*"?(\d+)"?`)

// FrameClass 对 SSE error 帧做分类（上游「200 已开流 + 一帧 error」的流式报错形态：
// 6004 限流 / 内容拦截 / 审核）。
//
// 按 400 口径交给 Classify：帧内的业务码与文案就是 Classify 认的那些（6004 优先命中，
// 其余靠 error.message 里的关键词）。分类结果的用途是**账号处置**——限流号要按模型冷却，
// 而不是被当成健康号继续接流量。
func FrameClass(payload string) ErrClass {
	return Classify(http.StatusBadRequest, payload)
}

// Classify 对上游响应做统一分类。body 为响应体原文（截断后即可）。
func Classify(status int, body string) ErrClass {
	c := ErrClass{Status: status, Message: truncate(strings.TrimSpace(body), 300)}
	c.Code = extractCode(body)

	// 6004 是模型级限流：上游文案里带「将在 … 重置」，只冷却触发模型
	if c.Code == 6004 || (status == 429 && strings.Contains(body, "6004")) {
		c.Kind = ErrModelRate
		c.Retryable = true
		c.ResetAt = extractResetTime(body)
		c.Model = extractModel(body)
		return c
	}

	// 1. 余额耗尽
	if status == 402 || containsAny(body, hardCreditMarkers) {
		c.Kind = ErrHardCredit
		return c
	}

	// 2. session 失效
	if containsAny(body, sessionDeadMarkers) || c.Code == 12153 {
		c.Kind = ErrSessionDead
		return c
	}

	// 3. 内容拦截（请求侧问题，不罚账号）
	if status == 400 && containsAny(body, contentBlockMarkers) {
		c.Kind = ErrContentBlock
		return c
	}

	// 4. 请求体畸形（客户端问题，不罚账号）
	if status == 400 && (containsAny(body, badRequestMarkers) || c.Code == 11101) {
		c.Kind = ErrBadRequest
		return c
	}

	// 5. 限流文案（不限状态码）
	if status == 429 || containsAny(body, rateLimitMarkers) {
		c.Kind = ErrSoftRate
		c.Retryable = true
		c.ResetAt = extractResetTime(body)
		return c
	}

	// 6. 状态码兜底
	switch {
	case status == 401 || status == 403:
		c.Kind = ErrSessionDead
	case status >= 500:
		c.Kind = ErrUpstream5xx
		c.Retryable = true
	case status == 404:
		c.Kind = ErrUpstream5xx // 上游 404：短冷却
		c.Retryable = true
	default:
		c.Kind = ErrUnknown
	}
	return c
}

// Punishable 报告该类别是否应该处罚账号（冷却/熔断/降权）。
func (c ErrClass) Punishable() bool {
	switch c.Kind {
	case ErrContentBlock, ErrBadRequest:
		return false
	default:
		return true
	}
}

// IsAuthFailure 报告是否属于授权失效（刷新令牌失败时用于决定是否禁用账号）。
func IsAuthFailure(status int, body string) bool {
	if status == 401 || status == 403 {
		return true
	}
	low := strings.ToLower(body)
	return strings.Contains(low, "invalid token") || strings.Contains(low, "token expired") ||
		strings.Contains(low, "unauthorized")
}

func containsAny(body string, markers []string) bool {
	low := strings.ToLower(body)
	for _, m := range markers {
		if strings.Contains(low, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func extractCode(body string) int {
	m := codePattern.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	for _, g := range m[1:] {
		if g == "" {
			continue
		}
		if n, err := strconv.Atoi(g); err == nil {
			return n
		}
	}
	return 0
}

// extractResetTime 从文案里解析恢复墙钟，按 UTC+8 解释（上游文案给的是北京时间）。
func extractResetTime(body string) time.Time {
	loc := time.FixedZone("CST", 8*3600)
	for _, re := range resetTimePatterns {
		m := re.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		nums := make([]int, 0, 6)
		for _, g := range m[1:] {
			n, err := strconv.Atoi(g)
			if err != nil {
				nums = nil
				break
			}
			nums = append(nums, n)
		}
		if len(nums) != 6 {
			continue
		}
		return time.Date(nums[0], time.Month(nums[1]), nums[2], nums[3], nums[4], nums[5], 0, loc)
	}
	return time.Time{}
}

// extractModel 从响应体里尽力提取触发限流的模型名。
func extractModel(body string) string {
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err == nil {
		if m, ok := doc["model"].(string); ok && m != "" {
			return m
		}
		if d, ok := doc["data"].(map[string]any); ok {
			if m, ok := d["model"].(string); ok && m != "" {
				return m
			}
		}
	}
	re := regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)
	if m := re.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}
