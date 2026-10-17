package upstream

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// 本文件实现对话请求的**会话上下文头**（对照 wb-gateway v1.14.19 的 conversation.go）。
//
// WorkBuddy 5.8.4 客户端在每次模型调用上带三个头：
//
//	X-Conversation-ID          本会话的稳定标识
//	X-Conversation-Request-ID  本轮的标识（同一次逻辑请求的重试/换号复用它）
//	X-Conversation-Message-ID  本条消息的标识（每次模型调用新生成）
//
// 缺少它们时上游对工具调用请求会返 11-128。这一点在我们的链路里尤其要紧：
// **网关自身会换号重试**，如果每次尝试都重新生成这三个 ID，同一个逻辑请求在
// 上游看来就是三个不同会话的三条消息 —— 既可能触发上面的错误，也让上游的
// 前缀缓存与限流统计全部对不上。
//
// 因此：
//   - 三个 ID 在**进入 dispatch 之前**算好，挂在请求 context 上；
//   - 同一次调用里的所有重试、换号、跨站回落共用同一组值；
//   - 客户端显式给了会话键就沿用它（规范化成 UUID），没给才独立生成；
//   - 只从会话键派生，**不从提示词/模型名推测会话** —— 那会把不同会话合并。

const (
	conversationIDHeader        = "X-Conversation-ID"
	conversationRequestIDHeader = "X-Conversation-Request-ID"
	conversationMessageIDHeader = "X-Conversation-Message-ID"
)

// conversationHeaders 是一组已就绪的会话标识。
type conversationHeaders struct {
	ID        string
	RequestID string
	MessageID string
	// Source 说明会话 ID 的来源，仅用于日志（不含任何会话键原文）。
	Source string
}

// conversationKey 是 context 键类型。
type conversationKey struct{}

// conversationNamespace 是 UUIDv5 的命名空间。
//
// 与标准 DNS/URL 命名空间**刻意分开**：这里需要的是「我们的会话键 → 我们的 UUID」
// 这种一对一映射，用公共命名空间只会让不同用途的键有撞上的可能。
var conversationNamespace = [16]byte{
	0x75, 0x18, 0x35, 0x2d, 0xf2, 0x97, 0x5e, 0xde,
	0x95, 0x83, 0xa3, 0x2f, 0x34, 0x13, 0xb7, 0x4d,
}

// PrepareConversation 计算并缓存本次请求的会话头。
//
// body 是**已解析的入站请求体**（chat / responses / messages 各自的形态），
// 用于读取客户端给出的会话字段（conversation_id / session_id 等）。
// 第二次及以后调用直接返回已有结果 —— 重试循环里可以放心重复调用。
func PrepareConversation(r *http.Request, body map[string]any) *conversationHeaders {
	if r == nil {
		return nil
	}
	if h := ConversationFrom(r); h != nil {
		return h
	}
	h := buildConversationHeaders(r, body)
	*r = *r.WithContext(context.WithValue(r.Context(), conversationKey{}, h))
	return h
}

// ConversationFrom 取出已准备好的会话头；未准备时返回 nil。
func ConversationFrom(r *http.Request) *conversationHeaders {
	if r == nil {
		return nil
	}
	if h, ok := r.Context().Value(conversationKey{}).(*conversationHeaders); ok {
		return h
	}
	return nil
}

// SetConversationHeaders 把会话头写进上游请求。
//
// 未准备时**复用请求上已有的那组值**（若已设过），否则生成一组并立即固定下来。
// 这一步是为重试服务的：doChatWithRetry 每轮都重建 http.Request 并重新调用
// BackendHeaders，若这里无条件新建，同一个逻辑请求的每次重试都会换一组 ID ——
// 正是本文件要消除的问题。读已有头而不是另建缓存，是因为重建出来的请求本身
// 不带 context 值，头是唯一能跨轮次传递的载体。
func SetConversationHeaders(req *http.Request) {
	if id := req.Header.Get(conversationIDHeader); id != "" {
		// 已经带上（同一逻辑请求的重建轮次）：原样保留，只补缺失字段。
		ensureConversationHeader(req, conversationRequestIDHeader)
		ensureConversationHeader(req, conversationMessageIDHeader)
		return
	}
	h := ConversationFrom(req)
	if h == nil {
		h = buildConversationHeaders(req, nil)
	}
	req.Header.Set(conversationIDHeader, h.ID)
	req.Header.Set(conversationRequestIDHeader, h.RequestID)
	req.Header.Set(conversationMessageIDHeader, h.MessageID)
}

// ensureConversationHeader 在头缺失时补一个随机值（已有则不动）。
func ensureConversationHeader(req *http.Request, name string) {
	if req.Header.Get(name) == "" {
		req.Header.Set(name, randomHex(16))
	}
}

// buildConversationHeaders 是会话头的唯一实现。
func buildConversationHeaders(r *http.Request, body map[string]any) *conversationHeaders {
	h := &conversationHeaders{
		ID:        randomHex(16),
		RequestID: randomHex(16),
		MessageID: randomHex(16),
		Source:    "独立生成",
	}
	if r == nil {
		return h
	}

	// 调用方指纹只参与摘要，**绝不落日志**：同一份 API Key 下不同客户端的
	// 同名会话键不应互相串味，但把 Key 本身写进日志等于泄露凭据。
	caller := apiFingerprint(r.Header.Get("Authorization"))
	if caller == "" {
		caller = apiFingerprint(r.Header.Get("x-api-key"))
	}

	metadata, _ := body["metadata"].(map[string]any)
	pick := func(obj map[string]any, names ...string) string {
		for _, name := range names {
			if v, ok := obj[name].(string); ok {
				if seed := usableConversationSeed(v); seed != "" {
					return seed
				}
			}
		}
		return ""
	}

	seed := usableConversationSeed(r.Header.Get(conversationIDHeader))
	switch {
	case seed != "":
		h.Source = "客户端会话头"
	case usableConversationSeed(r.Header.Get("X-Session-ID")) != "":
		seed = usableConversationSeed(r.Header.Get("X-Session-ID"))
		h.Source = "客户端 Session 头"
	default:
		if s := pick(body, "conversation_id", "conversationId", "session_id", "sessionId"); s != "" {
			seed = s
			h.Source = "请求会话字段"
		} else if s := pick(metadata, "conversation_id", "conversationId", "session_id", "sessionId"); s != "" {
			seed = s
			h.Source = "请求 metadata 会话字段"
		} else if s := pick(metadata, "user_id", "userId"); s != "" {
			// Anthropic 客户端（Claude Code）不传会话字段，常见只带 metadata.user_id。
			// 会话粘性（internal/session）也把它当会话键 —— 这里必须同一口径，
			// 否则粘性把多轮钉在同一账号、会话头却每轮新生成。
			seed = s
			h.Source = "请求 metadata.user_id"
		}
	}
	if seed != "" {
		h.ID = normalizeConversationID(seed, "conversation", caller)
	}

	for _, f := range []struct {
		header string
		names  []string
		target *string
		kind   string
	}{
		{conversationRequestIDHeader, []string{"conversation_request_id", "conversationRequestId"}, &h.RequestID, "request"},
		{conversationMessageIDHeader, []string{"conversation_message_id", "conversationMessageId"}, &h.MessageID, "message"},
	} {
		v := usableConversationSeed(r.Header.Get(f.header))
		if v == "" {
			v = pick(body, f.names...)
		}
		if v == "" {
			v = pick(metadata, f.names...)
		}
		if v != "" {
			*f.target = normalizeConversationID(v, f.kind, caller+"\x00"+h.ID)
		}
	}
	return h
}

// usableConversationSeed 判断客户端给的值能否当作会话键。
//
// 拒绝控制字符与超长值：这些既不是合法标识，也会把日志与上游请求头搞坏。
func usableConversationSeed(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

// normalizeConversationID 把任意客户端会话键规范成 UUID 形态。
//
// 已经是 UUID 的原样沿用（32 位无连字符的形式也保留，官方主路径用的就是它）；
// 其它形式做 UUIDv5 —— 上游只接受 UUID，而直接把任意文本塞进头里既不合规，
// 也会让不同格式的同一个会话被当成两个会话。
func normalizeConversationID(value, kind, caller string) string {
	if id, ok := parseUUID(value); ok {
		if len(value) == 32 {
			return strings.ToLower(value)
		}
		return id
	}
	return uuidV5(conversationNamespace, caller+"\x00"+kind+"\x00"+value)
}

// parseUUID 校验并格式化 UUID；不是 UUID 时 ok=false。
func parseUUID(s string) (string, bool) {
	compact := strings.ReplaceAll(strings.ToLower(s), "-", "")
	if len(compact) != 32 || compact == strings.Repeat("0", 32) {
		return "", false
	}
	for _, c := range compact {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		compact[0:8], compact[8:12], compact[12:16], compact[16:20], compact[20:32]), true
}

// uuidV5 按 RFC 4122 计算名字型 UUID（SHA-1）。
func uuidV5(ns [16]byte, name string) string {
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)[:16]
	sum[6] = (sum[6] & 0x0f) | 0x50 // 版本 5
	sum[8] = (sum[8] & 0x3f) | 0x80 // RFC 4122 变体
	return formatUUID(sum)
}

func formatUUID(b []byte) string {
	return hex.EncodeToString(b[0:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:16])
}

// apiFingerprint 返回调用方凭据的短摘要，用作会话 ID 派生的一部分。
func apiFingerprint(header string) string {
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if token == "" {
		return ""
	}
	sum := sha1.Sum([]byte(token))
	return hex.EncodeToString(sum[:8])
}
