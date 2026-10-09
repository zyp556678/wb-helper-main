package server

// -----------------------------------------------------------------------------
// Anthropic Messages API（/v1/messages + /v1/messages/count_tokens）
//
// 移植自 wb-gateway 的 messages.go：网关只与上游 /v2/chat/completions 通信，
// 这里做双向协议转换 ——
//
//	请求：system / messages(content blocks) / tools(input_schema) / thinking
//	      → chat messages / tools(嵌套 function) / reasoning_effort
//	流式：上游 SSE 增量 → Anthropic 语义事件（message_start、content_block_*、
//	      message_delta、message_stop）
//	非流式：上游流式数据在本地聚合为完整 Message 对象
//
// stop_reason 映射：stop→end_turn、tool_calls→tool_use、length→max_tokens。
// 上游思维链（delta.reasoning_content）**只在请求启用 thinking 时**回译为 thinking
// 内容块 —— 没启用的客户端不处理这种块，多出来会被当成协议异常。
//
// 为什么值得单独做一层：Claude Code 这类客户端只会说 Anthropic 协议，
// 没有这个端点它们完全连不上（不是「功能少一点」，而是「用不了」）。
// -----------------------------------------------------------------------------

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy-gateway/internal/debuglog"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/session"
	"workbuddy-gateway/internal/stats"
	"workbuddy-gateway/internal/upstream"
)

// anthropicErrFmtKey 标记本次请求的错误响应要用 Anthropic 形状
// （{"type":"error","error":{...}}）而不是 OpenAI 形状。
type anthropicErrFmtKey struct{}

func withAnthropicErrFormat(ctx context.Context) context.Context {
	return context.WithValue(ctx, anthropicErrFmtKey{}, true)
}

// isAnthropicRequest 判断这次请求的错误出口该用哪种形状。
//
// 鉴权发生在 handler 之前（那时还没有协议标记），所以这里同时精确匹配两条
// Anthropic 路由 —— 否则 401 会以 OpenAI 形状返回，Anthropic 客户端解析不了。
func isAnthropicRequest(r *http.Request) bool {
	if r.Context().Value(anthropicErrFmtKey{}) == true {
		return true
	}
	switch r.URL.Path {
	case "/v1/messages", "/messages", "/v1/messages/count_tokens", "/messages/count_tokens":
		return true
	}
	return false
}

// writeAPIError 是**共用调度路径**的错误出口：Anthropic 请求回 Anthropic 形状，
// 其余回 OpenAI 形状。
func (s *Server) writeAPIError(w http.ResponseWriter, r *http.Request, statusCode int, errType, message string) {
	if isAnthropicRequest(r) {
		writeAnthropicError(w, statusCode, anthropicErrType(errType), message)
		return
	}
	writeOpenAIError(w, statusCode, errType, message)
}

// writeAnthropicError 输出 Anthropic 官方错误形状。
func writeAnthropicError(w http.ResponseWriter, statusCode int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

// anthropicErrType 把内部错误码翻译成 Anthropic 官方错误类型枚举。
func anthropicErrType(errType string) string {
	switch errType {
	case "no_auth", "invalid_api_key":
		return "authentication_error"
	case "model_disabled", "model_account_disabled":
		return "permission_error"
	case "model_rate_limited", "all_accounts_cooldown":
		return "rate_limit_error"
	case "no_available_account", "model_requires_quota", "credit_floor_blocked":
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// handleMessages 处理 POST /v1/messages。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "api_error", "仅支持 POST 请求")
		return
	}
	// 标记错误出口走 Anthropic 形状（含共用调度路径里的选号/名单类错误）。
	r = r.WithContext(withAnthropicErrFormat(r.Context()))
	started := time.Now()
	trace := randomID()[:8]
	r = withTrace(r, trace)

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "api_error", "读取请求体失败")
		return
	}
	_ = r.Body.Close()
	if debuglog.Enabled() {
		fields := debuglog.BodyFingerprint(bodyBytes)
		debuglog.Event(r, "debug", "request_body_read_completed", fields)
	}

	var msgReq map[string]any
	if err := json.Unmarshal(bodyBytes, &msgReq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "无效的 JSON 请求体")
		return
	}

	modelName, _ := msgReq["model"].(string)
	if strings.TrimSpace(modelName) == "" {
		modelName = "deepseek-v4.1-flash"
	}
	isStream, _ := msgReq["stream"].(bool)
	wantThinking := anthropicThinkingOn(msgReq)

	// 站点路由：与 chat 同口径（?site= / X-WB-Site / 模型名前缀）。
	route := resolveSiteRoute(r, modelName)
	if route.Model != modelName {
		modelName = route.Model
	}

	if disabled, reason := s.config().ModelDisabled(modelName); disabled {
		debuglog.Event(r, "warn", "model_blocked_by_config", map[string]any{
			"model": modelName, "reason": reason, "status_code": http.StatusForbidden,
		})
		s.logf("[请求被拒绝] 模型=%s 原因=%s（/v1/messages）", modelName, reason)
		writeAnthropicError(w, http.StatusForbidden, "permission_error", reason)
		return
	}

	chatReq, err := anthropicToChatRequest(msgReq, modelName)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// 上游强制流式：非流式由网关本地聚合（与 chat 端点同一套约定）。
	chatReq["stream"] = true

	rawBody, err := json.Marshal(chatReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "序列化请求失败")
		return
	}
	// 出站改写：提示词模式、首条保底 system、工具序列自愈、思维链档位、指纹脱敏 ——
	// 与 chat / responses 完全同一条管线（不共用的话三个入口的治理行为会漂移）。
	upstreamBytes, prep := s.prepareOutboundBody(rawBody, false)
	s.logOutbound(modelName, "", trace, prep)

	sessionKey := session.Key(chatReq)
	pinID := s.sticky.Lookup(sessionKey)
	s.metrics.RecordRequest(modelName)

	resp, acc, release, ok := s.dispatchRouted(w, r, modelName, upstreamBytes, pinID, route, nil)
	if !ok {
		return
	}
	defer release()

	if isStream {
		s.streamMessagesResponse(w, r, resp, modelName, acc, started, wantThinking)
	} else {
		s.writeMessagesAggregate(w, r, resp, modelName, acc, started, wantThinking)
	}
	_ = sessionKey
}

// -----------------------------------------------------------------------------
// 请求转换：Anthropic Messages → Chat Completions
// -----------------------------------------------------------------------------

// anthropicToChatRequest 把 Anthropic 请求体转成上游 chat 请求体。
func anthropicToChatRequest(body map[string]any, modelName string) (map[string]any, error) {
	chat := map[string]any{"model": modelName}

	messages := []any{}
	if sys := extractAnthropicSystemText(body["system"]); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			messages = append(messages, convertAnthropicMessage(msg)...)
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("messages 字段缺失或为空")
	}
	chat["messages"] = messages

	if v, ok := body["max_tokens"].(float64); ok && v > 0 {
		chat["max_tokens"] = int(v)
	}
	if v, ok := body["temperature"].(float64); ok {
		chat["temperature"] = v
	}
	if v, ok := body["top_p"].(float64); ok {
		chat["top_p"] = v
	}
	if v, ok := body["top_k"].(float64); ok {
		chat["top_k"] = v
	}
	if ss, ok := body["stop_sequences"].([]any); ok && len(ss) > 0 {
		chat["stop"] = ss
	}
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		if ct := convertAnthropicTools(tools); len(ct) > 0 {
			chat["tools"] = ct
		}
	}
	if tc, ok := body["tool_choice"].(map[string]any); ok {
		chat["tool_choice"] = convertAnthropicToolChoice(tc)
		if name := strOfAny(tc["name"]); name != "" {
			// 上游的 tool_choice 只接受字符串，发不了 OpenAI 的 function 对象：
			// 只暴露指定工具并设 required，保留「强制选定」的语义。
			var selected []any
			tools, _ := chat["tools"].([]any)
			for _, item := range tools {
				tool, _ := item.(map[string]any)
				fn, _ := tool["function"].(map[string]any)
				if strOfAny(fn["name"]) == name {
					selected = append(selected, item)
				}
			}
			if len(selected) == 0 {
				return nil, fmt.Errorf("tool_choice 指定的工具未在 tools 中定义")
			}
			chat["tools"] = selected
		}
	} else if tc, ok := body["tool_choice"].(string); ok {
		chat["tool_choice"] = tc
	}

	// thinking → reasoning_effort（上游只认扁平档位）。
	if th, ok := body["thinking"].(map[string]any); ok {
		switch anthropicThinkingType(th) {
		case "disabled":
			chat["reasoning_effort"] = "off"
		case "enabled", "adaptive":
			chat["reasoning_effort"] = anthropicEffort(th)
		}
	}
	return chat, nil
}

// strOfAny 取字符串字段（非字符串返回空串）。
func strOfAny(v any) string {
	s, _ := v.(string)
	return s
}

// anthropicThinkingOn 判断请求是否启用扩展思考（响应侧据此决定要不要回译 thinking 块）。
//
// enabled 与 adaptive 都算启用：新版 Claude Code 对不在它能力表里的模型一律发
// adaptive，只认 enabled 会让这类请求的思考过程被整段丢掉。
func anthropicThinkingOn(body map[string]any) bool {
	th, ok := body["thinking"].(map[string]any)
	if !ok {
		return false
	}
	t := anthropicThinkingType(th)
	return t != "" && t != "disabled"
}

func anthropicThinkingType(th map[string]any) string {
	return strings.ToLower(strings.TrimSpace(strOfAny(th["type"])))
}

// anthropicEffort 取 thinking.effort；没有时按预算量级分档。
func anthropicEffort(th map[string]any) string {
	if e := strings.TrimSpace(strOfAny(th["effort"])); e != "" {
		return e
	}
	if b, _ := th["budget_tokens"].(float64); b > 0 {
		switch {
		case b >= 65536:
			return "max"
		case b >= 16384:
			return "high"
		case b >= 4096:
			return "medium"
		default:
			return "low"
		}
	}
	return "high"
}

// extractAnthropicSystemText 把 system 提取成纯文本（支持 string 与 text 块数组）。
func extractAnthropicSystemText(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []any:
		parts := make([]string, 0, len(s))
		for _, b := range s {
			blk, ok := b.(map[string]any)
			if ok && blk["type"] == "text" {
				if t := strOfAny(blk["text"]); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// convertAnthropicMessage 把单条 Anthropic 消息转成 0..n 条 chat 消息。
func convertAnthropicMessage(msg map[string]any) []any {
	role := strOfAny(msg["role"])
	switch content := msg["content"].(type) {
	case string:
		return []any{map[string]any{"role": role, "content": content}}
	case []any:
		return convertAnthropicBlocks(role, content)
	case nil:
		return []any{map[string]any{"role": role, "content": ""}}
	}
	return nil
}

// convertAnthropicBlocks 处理 content blocks 数组。
//
// user：tool_result 块 → 独立 tool 消息（必须紧跟 assistant 的 tool_calls），
// 其余文本/图片合并成一条 user 消息放在 tool 消息之后；tool_result 里嵌的图片
// 提升到这条 user 消息（tool 消息的 content 只能是字符串，图片无处安放）。
// assistant：text 合并为 content、tool_use → tool_calls；thinking / redacted_thinking
// 历史块**不上传**（上游自行管理思维链）。
func convertAnthropicBlocks(role string, blocks []any) []any {
	switch role {
	case "user":
		var toolMsgs, parts []any
		for _, bAny := range blocks {
			blk, ok := bAny.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": strOfAny(blk["text"])})
			case "image":
				if url := anthropicImageURL(blk); url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			case "tool_result":
				content := anthropicContentText(blk["content"])
				hasImage := false
				if list, ok := blk["content"].([]any); ok {
					for _, subAny := range list {
						sub, ok := subAny.(map[string]any)
						if !ok || sub["type"] != "image" {
							continue
						}
						if url := anthropicImageURL(sub); url != "" {
							hasImage = true
							parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
						}
					}
				}
				if content == "" && hasImage {
					content = "[图片]"
				}
				toolMsgs = append(toolMsgs, map[string]any{
					"role":         "tool",
					"tool_call_id": strOfAny(blk["tool_use_id"]),
					"content":      content,
				})
			}
		}
		var out []any
		out = append(out, toolMsgs...)
		if len(parts) == 1 {
			// 纯文本压平成字符串：多数上游对字符串形态更宽容。
			if p, ok := parts[0].(map[string]any); ok && p["type"] == "text" {
				return append(out, map[string]any{"role": "user", "content": p["text"]})
			}
		}
		if len(parts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": parts})
		}
		return out
	case "assistant":
		var texts []string
		var toolCalls []any
		for _, bAny := range blocks {
			blk, ok := bAny.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				if t := strOfAny(blk["text"]); t != "" {
					texts = append(texts, t)
				}
			case "tool_use":
				args, err := json.Marshal(blk["input"])
				if err != nil || blk["input"] == nil {
					args = []byte("{}")
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   strOfAny(blk["id"]),
					"type": "function",
					"function": map[string]any{
						"name":      strOfAny(blk["name"]),
						"arguments": string(args),
					},
				})
			}
		}
		m := map[string]any{"role": "assistant", "content": strings.Join(texts, "")}
		if len(toolCalls) > 0 {
			m["tool_calls"] = toolCalls
		}
		return []any{m}
	}
	// 其它 role：文本兜底。
	var texts []string
	for _, bAny := range blocks {
		if blk, ok := bAny.(map[string]any); ok && blk["type"] == "text" {
			if t := strOfAny(blk["text"]); t != "" {
				texts = append(texts, t)
			}
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return []any{map[string]any{"role": role, "content": strings.Join(texts, "")}}
}

// anthropicContentText 提取 tool_result.content（字符串或块数组）为纯文本。
func anthropicContentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, b := range c {
			if blk, ok := b.(map[string]any); ok && blk["type"] == "text" {
				if t := strOfAny(blk["text"]); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// anthropicImageURL 把 image 块转成 data URL（base64 source）或原 URL；
// 上游不支持的形态返回空串（调用方静默丢弃）。
func anthropicImageURL(blk map[string]any) string {
	src, _ := blk["source"].(map[string]any)
	if src == nil {
		return ""
	}
	switch strOfAny(src["type"]) {
	case "base64":
		media, data := strOfAny(src["media_type"]), strOfAny(src["data"])
		if media == "" || data == "" {
			return ""
		}
		return "data:" + media + ";base64," + data
	case "url":
		return strOfAny(src["url"])
	}
	return ""
}

// convertAnthropicTools 把 Anthropic tools（input_schema）转成 chat 格式。
func convertAnthropicTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, tAny := range tools {
		t, ok := tAny.(map[string]any)
		if !ok {
			continue
		}
		if _, hasFn := t["function"]; hasFn {
			out = append(out, t) // 已是 chat 格式，透传
			continue
		}
		fn := map[string]any{"name": strOfAny(t["name"])}
		if d, ok := t["description"].(string); ok && d != "" {
			fn["description"] = d
		}
		if p, ok := t["input_schema"]; ok && p != nil {
			fn["parameters"] = p
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// convertAnthropicToolChoice 转成上游实际接受的字符串枚举（带工具名时用 required）。
func convertAnthropicToolChoice(tc map[string]any) any {
	if name := strOfAny(tc["name"]); name != "" {
		return "required"
	}
	switch strings.ToLower(strOfAny(tc["type"])) {
	case "none":
		return "none"
	case "any", "required":
		return "required"
	}
	return "auto"
}

// -----------------------------------------------------------------------------
// 流式回译：上游 Chat SSE → Anthropic Messages SSE
// -----------------------------------------------------------------------------

type anthropicTextSlot struct {
	open  bool
	index int
	buf   strings.Builder
}

type anthropicToolSlot struct {
	id, name string
	args     strings.Builder
	index    int
	open     bool
}

// anthropicStreamState 把逐个到来的 chat chunk 翻译成 Anthropic SSE 事件字节流；
// 同一个实例也能走 aggregate() 输出非流式的完整 Message。
type anthropicStreamState struct {
	msgID        string
	model        string
	wantThinking bool
	started      bool
	blockIndex   int
	thinking     *anthropicTextSlot
	text         *anthropicTextSlot
	tools        map[int]*anthropicToolSlot
	toolOrder    []int
	finish       string
	usage        map[string]any
	// firstChunk 是**上游第一个内容 chunk** 到达的时刻，用来算首字延迟（TTFB）。
	//
	// 为什么记在这里而不是调用方：聚合与流式两条路都走 feed，记在 state 上就只有
	// 一处实现；而 TTFB 是面板「请求流水」里判断"上游慢还是生成慢"的唯一依据。
	firstChunk time.Time
}

func newAnthropicStreamState(modelName string, wantThinking bool) *anthropicStreamState {
	return &anthropicStreamState{
		msgID:        "msg_" + strings.ReplaceAll(randomID(), "-", "")[:24],
		model:        modelName,
		wantThinking: wantThinking,
		tools:        map[int]*anthropicToolSlot{},
	}
}

// emit 格式化一个 Anthropic SSE 事件（event 行 + data 行 + 空行）。
func (st *anthropicStreamState) emit(eventType string, data map[string]any) []byte {
	data["type"] = eventType
	b, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	return []byte("event: " + eventType + "\ndata: " + string(b) + "\n\n")
}

// feed 消费一个上游 chunk，返回 0..n 个 Anthropic 事件。
func (st *anthropicStreamState) feed(chunk map[string]any) []byte {
	if st.firstChunk.IsZero() {
		st.firstChunk = time.Now()
	}
	var out []byte
	// 上游可能改写 model（别名解析等），message_start 用回显值。
	if m := strOfAny(chunk["model"]); m != "" {
		st.model = m
	}
	if !st.started {
		st.started = true
		out = append(out, st.emit("message_start", map[string]any{
			"message": map[string]any{
				"id":      st.msgID,
				"type":    "message",
				"role":    "assistant",
				"content": []any{},
				"model":   st.model,
				"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})...)
	}
	if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
		st.usage = u
	}
	choices, _ := chunk["choices"].([]any)
	for _, cAny := range choices {
		choice, ok := cAny.(map[string]any)
		if !ok {
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok && delta != nil {
			// 推理增量只在请求启用 thinking 时回译。
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" && st.wantThinking {
				out = append(out, st.appendThinking(rc)...)
			}
			if ct, ok := delta["content"].(string); ok && ct != "" {
				out = append(out, st.appendText(ct)...)
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tcAny := range tcs {
					if tc, ok := tcAny.(map[string]any); ok {
						out = append(out, st.appendToolCall(tc)...)
					}
				}
			}
		}
		if fr := strOfAny(choice["finish_reason"]); fr != "" {
			st.finish = fr
		}
	}
	return out
}

func (st *anthropicStreamState) stopText(out []byte) []byte {
	if st.text != nil && st.text.open {
		out = append(out, st.emit("content_block_stop", map[string]any{"index": st.text.index})...)
		st.text.open = false
	}
	return out
}

// stopThinking 先发 signature_delta 再收口：顺序反了的话，content_block_stop 之后
// 到达的 delta 会被客户端丢弃，签名就白发了。
func (st *anthropicStreamState) stopThinking(out []byte) []byte {
	if st.thinking == nil || !st.thinking.open {
		return out
	}
	st.thinking.open = false
	out = append(out, st.emit("content_block_delta", map[string]any{
		"index": st.thinking.index,
		"delta": map[string]any{"type": "signature_delta", "signature": anthropicSignature(st.thinking.buf.String())},
	})...)
	out = append(out, st.emit("content_block_stop", map[string]any{"index": st.thinking.index})...)
	return out
}

func (st *anthropicStreamState) appendThinking(rc string) []byte {
	var out []byte
	// 思维链只出现在正文前；正文块开着就先收口，保证块顺序 thinking → text。
	out = st.stopText(out)
	if st.thinking == nil || !st.thinking.open {
		st.thinking = &anthropicTextSlot{open: true, index: st.blockIndex}
		st.blockIndex++
		out = append(out, st.emit("content_block_start", map[string]any{
			"index":         st.thinking.index,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		})...)
	}
	st.thinking.buf.WriteString(rc)
	out = append(out, st.emit("content_block_delta", map[string]any{
		"index": st.thinking.index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
	})...)
	return out
}

func (st *anthropicStreamState) appendText(ct string) []byte {
	var out []byte
	out = st.stopThinking(out)
	if st.text == nil || !st.text.open {
		st.text = &anthropicTextSlot{open: true, index: st.blockIndex}
		st.blockIndex++
		out = append(out, st.emit("content_block_start", map[string]any{
			"index":         st.text.index,
			"content_block": map[string]any{"type": "text", "text": ""},
		})...)
	}
	st.text.buf.WriteString(ct)
	out = append(out, st.emit("content_block_delta", map[string]any{
		"index": st.text.index,
		"delta": map[string]any{"type": "text_delta", "text": ct},
	})...)
	return out
}

func (st *anthropicStreamState) appendToolCall(tc map[string]any) []byte {
	idx := 0
	if v, ok := tc["index"].(float64); ok {
		idx = int(v)
	}
	slot, exists := st.tools[idx]
	if !exists {
		slot = &anthropicToolSlot{index: st.blockIndex}
		st.blockIndex++
		st.tools[idx] = slot
		st.toolOrder = append(st.toolOrder, idx)
	}
	// 工具块开始：先收口 thinking / text，保证块顺序 thinking → text → tool_use。
	var out []byte
	out = st.stopThinking(out)
	out = st.stopText(out)
	if id := strOfAny(tc["id"]); id != "" {
		slot.id = id
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		if n := strOfAny(fn["name"]); n != "" {
			slot.name = n
		}
	}
	if !slot.open {
		slot.open = true
		out = append(out, st.emit("content_block_start", map[string]any{
			"index": slot.index,
			"content_block": map[string]any{
				"type": "tool_use", "id": slot.id, "name": slot.name, "input": map[string]any{},
			},
		})...)
	}
	if fn, ok := tc["function"].(map[string]any); ok {
		// 参数分片原样透传，拼接交给客户端（我们无从判断 JSON 何时完整）。
		if a := strOfAny(fn["arguments"]); a != "" {
			slot.args.WriteString(a)
			out = append(out, st.emit("content_block_delta", map[string]any{
				"index": slot.index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": a},
			})...)
		}
	}
	return out
}

// mapStopReason 把 chat finish_reason 映射成 Anthropic stop_reason
// （未列出的值一律按 end_turn 收尾）。
func mapStopReason(finish string) string {
	switch finish {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	}
	return "end_turn"
}

func intFloat(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// anthropicUsage 把上游 usage 转成 Anthropic 口径。
//
// 上游的 prompt_tokens **已包含**命中的缓存 token，而 Anthropic 的 input_tokens
// 不含缓存 —— 不减的话客户端把 input 与 cache_read 相加时缓存会被算两遍。
func (st *anthropicStreamState) anthropicUsage() map[string]any {
	in, out, cached := 0, 0, 0
	if st.usage != nil {
		in = intFloat(st.usage["prompt_tokens"])
		out = intFloat(st.usage["completion_tokens"])
		if d, ok := st.usage["prompt_tokens_details"].(map[string]any); ok {
			cached = intFloat(d["cached_tokens"])
		}
	}
	if cached > in {
		cached = in // 上游给了畸形命中数时钳到非负
	}
	return map[string]any{
		"input_tokens":                in - cached,
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": 0,
		"output_tokens":               out,
	}
}

// finishEvents 收口所有未关闭的块，并输出 message_delta + message_stop。
func (st *anthropicStreamState) finishEvents() []byte {
	var out []byte
	out = st.stopThinking(out)
	out = st.stopText(out)
	for _, idx := range st.toolOrder {
		slot := st.tools[idx]
		if slot.open {
			out = append(out, st.emit("content_block_stop", map[string]any{"index": slot.index})...)
			slot.open = false
		}
	}
	out = append(out, st.emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": mapStopReason(st.finish), "stop_sequence": nil},
		"usage": st.anthropicUsage(),
	})...)
	out = append(out, st.emit("message_stop", map[string]any{})...)
	return out
}

// aggregate 输出完整的 Anthropic Message 对象（非流式响应）。
func (st *anthropicStreamState) aggregate() map[string]any {
	content := []any{}
	if st.thinking != nil && st.thinking.buf.Len() > 0 {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": st.thinking.buf.String(),
			"signature": anthropicSignature(st.thinking.buf.String()),
		})
	}
	if st.text != nil && st.text.buf.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": st.text.buf.String()})
	}
	for _, idx := range st.toolOrder {
		slot := st.tools[idx]
		var input any
		args := slot.args.String()
		if args == "" {
			input = map[string]any{}
		} else if err := json.Unmarshal([]byte(args), &input); err != nil {
			input = args // 参数不是合法 JSON 时原样返回，不让单次调用失败
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": slot.id, "name": slot.name, "input": input,
		})
	}
	return map[string]any{
		"id":            st.msgID,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         st.model,
		"stop_reason":   mapStopReason(st.finish),
		"stop_sequence": nil,
		"usage":         st.anthropicUsage(),
	}
}

// anthropicSignature 生成 thinking 块的回传签名。
//
// 请求侧不上传 thinking 历史（上游自行管理思维链），签名只为满足严格客户端的
// 协议校验 —— 取推理正文的哈希即可，且可确定复现。
func anthropicSignature(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// -----------------------------------------------------------------------------
// 响应出口：流式 / 聚合 / count_tokens
// -----------------------------------------------------------------------------

// scanUpstreamSSE 逐行扫描上游 SSE 并把每个 chunk 喂给状态机。
// scanUpstreamSSE 扫一条上游 SSE 流并喂给状态机。
//
// 返回（上游 error 帧原文, 读错误）。error 帧是上游「200 已开流 + 一帧 error」的
// 报错形态（6004 限流 / 内容拦截 / 审核），报错帧没有 choices，若当普通分片忽略掉，
// 调用方会把它当成一次「空但成功」的回复。
func scanUpstreamSSE(body io.Reader, state *anthropicStreamState, sink func([]byte)) (string, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	errorFrame := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if _, hasErr := chunk["error"]; hasErr {
			errorFrame = data
			continue
		}
		if out := state.feed(chunk); sink != nil && len(out) > 0 {
			sink(out)
		}
	}
	return errorFrame, scanner.Err()
}

// streamMessagesResponse 把上游流式响应实时回译成 Anthropic SSE。
func (s *Server) streamMessagesResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, acc *pool.Account, started time.Time, wantThinking bool) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.recordMessagesRequest(r, "messages/stream", modelName, acc, nil, started, true)
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "服务器不支持流式响应 Flush")
		return
	}
	writeEvent := func(b []byte) {
		if len(b) == 0 {
			return
		}
		_, _ = w.Write(b)
		flusher.Flush()
	}

	state := newAnthropicStreamState(modelName, wantThinking)
	errorFrame, scanErr := scanUpstreamSSE(resp.Body, state, writeEvent)
	if scanErr != nil {
		debuglog.Event(r, "error", "stream_response_failed", map[string]any{"error": scanErr.Error()})
		// 上游流中断时**不能伪造 message_stop**：那会让客户端把残缺输出当成完整结果。
		// 改为下发 error 事件并直接返回，明确告知本次响应不完整。
		reason := "上游流式响应中断，本次回复不完整"
		if errors.Is(scanErr, context.DeadlineExceeded) {
			reason = "上游长时间无数据，判定连接卡死并中断，本次回复不完整"
		}
		writeEvent(state.emit("error", map[string]any{
			"error": map[string]any{"type": "api_error", "message": reason},
		}))
		s.logf("[异常] /v1/messages 上游流式读取中断（%s）：%v", modelName, scanErr)
		s.recordMessagesRequest(r, "messages/stream", modelName, acc, state, started, true)
		return
	}
	if errorFrame != "" {
		// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧分类处置账号，
		// 拒发 message_stop —— 这不是一次完整回复。与 chat 端点同一口径：
		// 不记成功、不记账号成功（限流号不能被当成健康号）。
		class := upstream.FrameClass(errorFrame)
		s.applyFrameClass(acc, modelName, class, errorFrame)
		debuglog.Event(r, "error", "upstream_error_frame", map[string]any{
			"model": modelName, "kind": class.Kind,
			"payload": truncateForLog(errorFrame, 300),
		})
		writeEvent(state.emit("error", map[string]any{
			"error": map[string]any{
				"type":    "api_error",
				"message": "上游以错误帧结束了本次响应: " + truncateForLog(errorFrame, 300),
			},
		}))
		s.logf("[异常] /v1/messages 上游以 error 帧报错（%s，分类=%s）：%s",
			modelName, class.Kind, truncateForLog(errorFrame, 200))
		s.recordMessagesRequest(r, "messages/stream", modelName, acc, state, started, true)
		return
	}
	if state.finish == "" {
		// 干净 EOF 但没有 finish_reason：同样拒绝伪造完成。
		debuglog.Event(r, "error", "stream_closed_without_finish", map[string]any{"model": modelName})
		writeEvent(state.emit("error", map[string]any{
			"error": map[string]any{"type": "api_error", "message": "上游流结束但没有 finish_reason，本次回复不完整"},
		}))
		s.logf("[异常] /v1/messages 上游流结束但无 finish_reason（%s）", modelName)
		s.recordMessagesRequest(r, "messages/stream", modelName, acc, state, started, true)
		return
	}
	writeEvent(state.finishEvents())
	// Anthropic 协议这条路径也要计入模型级统计：否则 Claude Code 的流量
	// 在面板上完全不可见（请求数记了，token/耗时/最近状态全空）。
	s.recordMessagesMetrics(modelName, state.usage, started)
	s.recordMessagesRequest(r, "messages/stream", modelName, acc, state, started, false)
	s.markHopSuccess(acc)
	debuglog.Event(r, "info", "stream_response_completed", map[string]any{
		"status_code": http.StatusOK, "model": modelName,
		"elapsed_ms": time.Since(started).Milliseconds(),
	})
}

// recordMessagesRequest 把一次 /v1/messages 请求记进请求归档、事件日志与用量统计。
//
// 为什么要收成一个函数：这条协议有**两条响应路径 × 各四个完成分支**
// （成功 / 读流失败 / 上游 error 帧 / 没有 finish_reason），每个分支都要记同一套账。
// 之前只在成功分支记了 metrics，于是面板「请求流水」里 Claude Code 的记录
// 只有时间、状态、耗时，账号 / 模型 / TTFB / token / 速率全是「—」——
// 而这些恰好是排查问题时唯一想看的东西。手写八遍必然漏掉某一支，
// 而漏记的表现是「成功率偏高」，没有人会发现，所以收成一个入口。
//
// 与 chat / responses 同一口径：
//   - tokens 取上游 usage 的**总量**，速率用**输出** token 做分子（见 reqlog.event）；
//   - 失败分支也记已经拿到的部分用量（上游开了流才报错时，前面的 token 是真花掉的）；
//   - stats 记的是**用量**（账号维度、按小时聚合），metrics 记的是模型级指标。
func (s *Server) recordMessagesRequest(r *http.Request, mode, modelName string, acc *pool.Account,
	state *anthropicStreamState, started time.Time, failed bool) {
	ttft := time.Duration(0)
	var usage map[string]any
	if state != nil {
		if !state.firstChunk.IsZero() {
			ttft = state.firstChunk.Sub(started)
		}
		usage = state.usage
	}
	total := tokensFromUsage(usage)
	inputTokens, outputTokens := splitTokensFromUsage(usage)

	accountLabel, accountID := "", ""
	if acc != nil {
		accountLabel = accountLogLabel(acc)
		accountID = acc.Cred.AccountID()
	}

	s.logRequest(r, modelName, accountLabel, mode, ttft, time.Since(started), total, outputTokens, failed)
	s.stats.Record(stats.RecordInput{
		Model:        modelName,
		Account:      accountID,
		OK:           !failed,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  total,
		TTFTMs:       ttft.Milliseconds(),
	})
}

// recordMessagesMetrics 把一次成功的 Anthropic 响应计入模型级统计。
func (s *Server) recordMessagesMetrics(modelName string, usage map[string]any, started time.Time) {
	s.metrics.RecordTokens(modelName, tokensFromUsage(usage))
	s.metrics.RecordLatency(modelName, time.Since(started))
	s.metrics.RecordSuccess(modelName)
}

// writeMessagesAggregate 本地聚合上游流式数据，输出完整的 Anthropic Message。
func (s *Server) writeMessagesAggregate(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, acc *pool.Account, started time.Time, wantThinking bool) {
	defer resp.Body.Close()
	state := newAnthropicStreamState(modelName, wantThinking)
	errorFrame, scanErr := scanUpstreamSSE(resp.Body, state, nil)
	if scanErr != nil {
		debuglog.Event(r, "error", "aggregate_response_failed", map[string]any{"error": scanErr.Error()})
		s.logf("[异常] /v1/messages 聚合上游流式响应失败（%s）：%v", modelName, scanErr)
		s.recordMessagesRequest(r, "messages/aggregate", modelName, acc, state, started, true)
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "聚合上游流式响应失败: "+scanErr.Error())
		return
	}
	if errorFrame != "" {
		// 上游以 error 帧报错：这一跳失败，按分类处置账号（不记成功）。
		class := upstream.FrameClass(errorFrame)
		s.applyFrameClass(acc, modelName, class, errorFrame)
		debuglog.Event(r, "error", "upstream_error_frame", map[string]any{
			"model": modelName, "kind": class.Kind,
			"payload": truncateForLog(errorFrame, 300),
		})
		s.logf("[异常] /v1/messages 聚合遇到上游 error 帧（%s，分类=%s）：%s",
			modelName, class.Kind, truncateForLog(errorFrame, 200))
		s.recordMessagesRequest(r, "messages/aggregate", modelName, acc, state, started, true)
		writeAnthropicError(w, http.StatusBadGateway, "api_error",
			"上游以错误帧结束了本次响应: "+truncateForLog(errorFrame, 300))
		return
	}
	if state.finish == "" {
		s.recordMessagesRequest(r, "messages/aggregate", modelName, acc, state, started, true)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "上游流结束但没有 finish_reason，本次回复不完整")
		return
	}
	out, err := json.Marshal(state.aggregate())
	if err != nil {
		s.recordMessagesRequest(r, "messages/aggregate", modelName, acc, state, started, true)
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "序列化响应失败")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	s.recordMessagesMetrics(modelName, state.usage, started)
	s.recordMessagesRequest(r, "messages/aggregate", modelName, acc, state, started, false)
	s.markHopSuccess(acc)
	debuglog.Event(r, "info", "aggregate_response_completed", map[string]any{
		"status_code": http.StatusOK, "model": modelName, "response_bytes": len(out),
		"elapsed_ms": time.Since(started).Milliseconds(),
	})
}

// handleCountTokens 处理 POST /v1/messages/count_tokens。
//
// **本地估算，不打上游**：CJK 约 1 token/字、ASCII 约 4 字符/token。
// 估算只覆盖客户端输入，不含网关注入的提示词 —— 这一点在日志里说明，
// 免得有人拿它对账上游的计费。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "api_error", "仅支持 POST 请求")
		return
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "api_error", "读取请求体失败")
		return
	}
	_ = r.Body.Close()
	var msgReq map[string]any
	if err := json.Unmarshal(bodyBytes, &msgReq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "无效的 JSON 请求体")
		return
	}
	tokens := estimateAnthropicInputTokens(msgReq)
	// 记进请求归档，但**不记用量统计**：这个数是本地估算的，不是上游口径，
	// 混进用量统计会与真实计费对不上账。归档里带上模型名，
	// 至少让「谁在什么时候估过一次」有据可查（面板上 token 列为 0 是准确的）。
	s.logRequest(r, strOfAny(msgReq["model"]), "", "messages/count_tokens", 0, time.Since(started), 0, 0, false)
	debuglog.Event(r, "info", "tokens_estimated_locally", map[string]any{
		"input_tokens": tokens, "request_bytes": len(bodyBytes),
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": tokens})
}

// estimateAnthropicInputTokens 估算输入 token 数（本地，不调用上游）。
func estimateAnthropicInputTokens(body map[string]any) int {
	var sb strings.Builder
	if sys := extractAnthropicSystemText(body["system"]); sys != "" {
		sb.WriteString(sys)
		sb.WriteByte('\n')
	}
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			if msg, ok := m.(map[string]any); ok {
				appendAnthropicContentText(&sb, msg["content"])
				sb.WriteByte('\n')
			}
		}
	}
	n := approxTokens(sb.String())
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			n += len(b) / 4
		}
	}
	return n
}

func appendAnthropicContentText(sb *strings.Builder, content any) {
	switch c := content.(type) {
	case string:
		sb.WriteString(c)
	case []any:
		for _, bAny := range c {
			blk, ok := bAny.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				sb.WriteString(strOfAny(blk["text"]))
			case "tool_result":
				sb.WriteString(anthropicContentText(blk["content"]))
			case "tool_use":
				sb.WriteString(strOfAny(blk["name"]))
				if b, err := json.Marshal(blk["input"]); err == nil {
					sb.Write(b)
				}
			}
		}
	}
}

// approxTokens 粗略估算 token：CJK 按 1 token/字，其余按 4 字符/token。
func approxTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}
