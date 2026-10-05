package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/session"
	"workbuddy-gateway/internal/stats"
	"workbuddy-gateway/internal/upstream"
)

// 本文件实现 OpenAI Responses API（/v1/responses）。
//
// 为什么值得单独做一个端点：Codex 这类新客户端已经改用 Responses 协议，
// 而我们的上游只说 Chat Completions。缺这一层转换，这些客户端要么退回老协议，
// 要么干脆用不了。
//
// 实现方式是**先转成 chat 请求发给上游，再把上游的流式输出转回 Responses 事件**。
// 好处是完全复用现有的选号、治理、出站改写与指标，协议面行为与 /v1/chat/completions 一致；
// 代价是 Responses 里有几个概念在 chat 协议里没有对应物（reasoning item、web_search_call），
// 这些一律丢弃而不是伪造——伪造会让客户端把不存在的工具结果当成真实历史。

// handleResponses 处理 Responses API 请求。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}
	started := time.Now()
	trace := randomID()[:8]
	r = withTrace(r, trace)

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "读取请求体失败")
		return
	}
	_ = r.Body.Close()

	var respReq map[string]any
	if err := json.Unmarshal(bodyBytes, &respReq); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
		return
	}

	modelName, _ := respReq["model"].(string)
	if strings.TrimSpace(modelName) == "" {
		modelName = "deepseek-v4.1-flash"
	}

	// 站点路由：与 /v1/chat/completions 同一套规则（?site= / X-WB-Site / 模型名前缀）。
	// 必须在 responsesToChatRequest 之前剥离前缀 —— 那个函数会把 modelName 原样
	// 写进转换后的 chat 请求体，剥晚了前缀就被带上去了。
	route := resolveSiteRoute(r, modelName)
	if route.Model != modelName {
		modelName = route.Model
		respReq["model"] = modelName
	}

	if disabled, reason := s.config().ModelDisabled(modelName); disabled {
		writeOpenAIError(w, http.StatusForbidden, "model_disabled", reason)
		return
	}

	chatReq, err := responsesToChatRequest(respReq, modelName)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	wantStream, _ := respReq["stream"].(bool)

	rawBody, err := json.Marshal(chatReq)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "encode_error", "序列化请求失败")
		return
	}
	upstreamBytes, prep := s.prepareOutboundBody(rawBody, false)
	s.logOutbound(modelName, "", trace, prep)

	sessionKey := session.Key(chatReq)
	pinID := s.sticky.Lookup(sessionKey)
	s.metrics.RecordRequest(modelName)

	// 降级重试：内容策略拦截时换中性提示词再发一次（与 chat 端点同一条策略）
	var degradedBody []byte
	resp, acc, release, ok := s.dispatchRouted(w, r, modelName, upstreamBytes, pinID, route, func() []byte {
		if degradedBody != nil {
			return degradedBody
		}
		if b, _ := s.prepareOutboundBody(rawBody, true); len(b) > 0 {
			degradedBody = b
			return b
		}
		return nil
	})
	if !ok {
		return
	}
	defer release()

	respID := "resp_" + randomID()
	createdAt := time.Now().Unix()

	if wantStream {
		s.streamResponses(w, r, resp, respID, modelName, createdAt, acc, sessionKey, started)
	} else {
		s.aggregateResponses(w, r, resp, respID, modelName, createdAt, acc, sessionKey, started)
	}
}

// responsesToChatRequest 把 Responses 请求体转成 Chat Completions 请求体。
func responsesToChatRequest(respReq map[string]any, modelName string) (map[string]any, error) {
	chat := map[string]any{"model": modelName}

	messages := []any{}
	if instructions, ok := respReq["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	switch input := respReq["input"].(type) {
	case string:
		if strings.TrimSpace(input) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": input})
		}
	case []any:
		for _, itemAny := range input {
			switch item := itemAny.(type) {
			case string:
				messages = append(messages, map[string]any{"role": "user", "content": item})
			case map[string]any:
				messages = append(messages, convertResponsesInputItem(item)...)
			}
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("input 为空：Responses 请求必须提供 input 或 instructions")
	}
	chat["messages"] = messages

	if v, ok := respReq["temperature"]; ok && v != nil {
		chat["temperature"] = v
	}
	if v, ok := respReq["top_p"]; ok && v != nil {
		chat["top_p"] = v
	}
	if v, ok := respReq["max_output_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	if tools := convertResponsesTools(respReq["tools"]); len(tools) > 0 {
		chat["tools"] = tools
	}
	if tc := convertResponsesToolChoice(respReq["tool_choice"]); tc != nil {
		chat["tool_choice"] = tc
	}
	if reasoning, ok := respReq["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" && effort != "none" {
			chat["reasoning_effort"] = effort
		}
	}
	return chat, nil
}

// convertResponsesInputItem 把单个 Responses input item 转成 0..n 条 chat 消息。
func convertResponsesInputItem(item map[string]any) []any {
	switch typ, _ := item["type"].(string); typ {
	case "function_call":
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		return []any{map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{map[string]any{
				"id":       ifEmpty(callID, "call_"+randomID()),
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}}
	case "function_call_output":
		callID, _ := item["call_id"].(string)
		return []any{map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      stringifyToolOutput(item["output"]),
		}}
	case "reasoning", "web_search_call":
		// 上游无法接收这些 Responses 历史项，直接丢弃。
		// 特别不能把 web_search_call 转成空 user 消息——那会插在并行 function_call
		// 与它的 output 之间，直接造成工具序列断裂（上游 11148）。
		return nil
	}

	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}
	return []any{map[string]any{"role": role, "content": convertResponsesContent(item["content"])}}
}

// convertResponsesContent 把 Responses content（string 或 parts 数组）转成 chat content。
func convertResponsesContent(content any) any {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		parts := make([]any, 0, len(c))
		for _, pAny := range c {
			p, ok := pAny.(map[string]any)
			if !ok {
				if str, ok := pAny.(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": str})
				}
				continue
			}
			switch typ, _ := p["type"].(string); typ {
			case "input_text", "output_text", "text":
				if t, ok := p["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "refusal":
				if t, ok := p["refusal"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "input_image":
				if url, _ := p["image_url"].(string); url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	default:
		return fmt.Sprintf("%v", c)
	}
}

func stringifyToolOutput(output any) string {
	switch o := output.(type) {
	case nil:
		return ""
	case string:
		return o
	default:
		if b, err := json.Marshal(o); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", o)
	}
}

// convertResponsesTools 把 Responses 的扁平 function 工具转成 chat 的嵌套形态。
func convertResponsesTools(toolsAny any) []any {
	arr, ok := toolsAny.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, tAny := range arr {
		t, ok := tAny.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := t["type"].(string); typ != "" && typ != "function" {
			continue // 仅支持 function 工具
		}
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		params := t["parameters"]
		if name == "" {
			if fn, ok := t["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
				desc, _ = fn["description"].(string)
				params = fn["parameters"]
			}
		}
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if desc != "" {
			fn["description"] = desc
		}
		if params != nil {
			fn["parameters"] = params
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// convertResponsesToolChoice 把 Responses tool_choice 转成 chat tool_choice。
func convertResponsesToolChoice(tc any) any {
	switch v := tc.(type) {
	case string:
		if v == "auto" || v == "none" || v == "required" {
			return v
		}
	case map[string]any:
		if typ, _ := v["type"].(string); typ == "function" {
			name, _ := v["name"].(string)
			if name == "" {
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			if name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// 输出：非流式
// -----------------------------------------------------------------------------

func (s *Server) aggregateResponses(w http.ResponseWriter, r *http.Request, resp *http.Response,
	respID, modelName string, createdAt int64, acc *pool.Account, sessionKey string, started time.Time) {
	defer resp.Body.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	_ = ctx
	body := upstreamWatchdog(resp.Body, s.config().IdleTimeout)
	defer body.Close()

	chatJSON, err := aggregateCompletion(body, modelName)
	if err != nil {
		s.metrics.RecordFailure(modelName, "聚合失败")
		s.sticky.Unbind(sessionKey)
		writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "聚合上游流式响应失败: "+err.Error())
		return
	}

	out, err := chatCompletionToResponses(chatJSON, respID, modelName, createdAt)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "convert_error", "转换为 Responses 响应失败: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)

	usage := usageFromCompletion(chatJSON)
	inputTokens, outputTokens := splitTokensFromUsage(usage)
	total := tokensFromUsage(usage)
	s.metrics.RecordTokens(modelName, total)
	s.metrics.RecordLatency(modelName, time.Since(started))
	s.metrics.RecordSuccess(modelName)
	// 聚合路径等聚合完成（上游无 error 帧才会走到这里）才记账号成功并绑粘性。
	s.markHopSuccess(acc)
	s.sticky.Bind(sessionKey, acc.Cred.AccountID())
	s.stats.Record(stats.RecordInput{
		Model:        modelName,
		Account:      acc.Cred.AccountID(),
		OK:           true,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  total,
	})
	s.logRequest(r, modelName, accountLogLabel(acc), "responses/aggregate",
		0, time.Since(started), total, false)

	s.logf("[Responses] 模型=%s 账号=%s 非流式 总耗时=%v 输出token=%d",
		modelName, acc.Cred.AccountID(), time.Since(started), total)
}

// chatCompletionToResponses 把 chat completion 结果转成 Responses 响应。
func chatCompletionToResponses(chatJSON []byte, respID, modelName string, createdAt int64) ([]byte, error) {
	var chat map[string]any
	if err := json.Unmarshal(chatJSON, &chat); err != nil {
		return nil, err
	}
	choice, _ := firstChoice(chat)
	msg, _ := choice["message"].(map[string]any)

	output := []any{}
	if text := extractText(msg["content"]); text != "" {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     "msg_" + randomID(),
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		})
	}
	for _, tcAny := range toolCallsOf(msg) {
		tc, ok := tcAny.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		callID, _ := tc["id"].(string)
		if callID == "" {
			callID = "call_" + randomID()
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        "fc_" + randomID(),
			"call_id":   callID,
			"name":      asString(fn["name"]),
			"arguments": asString(fn["arguments"]),
			"status":    "completed",
		})
	}

	envelope := buildResponsesEnvelope(respID, modelName, createdAt)
	envelope["status"] = "completed"
	envelope["output"] = output
	envelope["output_text"] = collectOutputText(output)
	envelope["usage"] = toResponsesUsage(usageFromCompletion(chatJSON))
	return json.Marshal(envelope)
}

// -----------------------------------------------------------------------------
// 输出：流式
// -----------------------------------------------------------------------------

// streamResponses 把上游 chat SSE 转成 Responses 事件流。
//
// 事件顺序（对齐 Responses 协议）：response.created → response.in_progress →
// 若干 response.output_text.delta → response.output_text.done / output_item.done →
// 工具调用（若有）→ response.completed。
//
// 出错时**不发** response.completed（那等于告诉客户端「正常结束」，会让残缺的工具调用
// 被当成完整结果执行），而是发 response.failed 带上具体原因。
func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, resp *http.Response,
	respID, modelName string, createdAt int64, acc *pool.Account, sessionKey string, started time.Time) {
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}

	body := upstreamWatchdog(resp.Body, s.config().IdleTimeout)
	defer body.Close()

	send := func(eventType string, payload map[string]any) {
		data, _ := json.Marshal(payload)
		_, _ = io.WriteString(w, "event: "+eventType+"\n")
		_, _ = io.WriteString(w, "data: "+string(data)+"\n\n")
		flusher.Flush()
	}

	envelope := buildResponsesEnvelope(respID, modelName, createdAt)
	send("response.created", map[string]any{"type": "response.created", "response": envelope})
	send("response.in_progress", map[string]any{"type": "response.in_progress", "response": envelope})

	msgItemID := "msg_" + randomID()
	itemAdded := false
	seq := 0

	var (
		fullText  strings.Builder
		usage     map[string]any
		ttft      time.Duration
		first     = true
		interrupt bool
		// errorFrame 是上游 error 帧原文：这类流 HTTP 200 已开流，但这一跳失败了，
		// 不能当成完成（否则会记成功、把粘性钉在限流号上）。
		errorFrame string
	)
	toolAcc := map[int]*mergedToolCall{}
	toolOrder := []int{}

	// 思考段的状态。
	//
	// 上游把思考放在 delta.reasoning_content，在此端点原先**被整段丢弃** ——
	// 客户端一条 reasoning 事件都收不到，表现为「模型想了很久，界面上什么都没有」。
	// Responses 协议里思考是一个独立的 output item（type=reasoning），
	// 内容通过 reasoning_summary_text.* 事件流式给出。
	reasoningItemID := "rs_" + randomID()
	var reasoningText strings.Builder
	reasoningStarted := false
	reasoningClosed := false

	// outputIndex 是顶层输出项序号。思考在前时它占 0，正文/工具调用顺延 ——
	// 不能写死 0，否则 reasoning 与 message 会撞在同一个序号上。
	outputIndex := 0
	msgIndex := 0
	reasoningIndex := 0

	// closeReasoning 收尾思考段。正文开始或流结束时都必须调用一次：
	// 协议要求 summary part 的 added/delta/done 完整配对，只发 delta 不发 done
	// 会让客户端一直停在「思考中」。
	closeReasoning := func() {
		if !reasoningStarted || reasoningClosed {
			return
		}
		reasoningClosed = true
		text := reasoningText.String()
		send("response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": reasoningItemID,
			"output_index": reasoningIndex, "summary_index": 0, "text": text,
		})
		send("response.reasoning_summary_part.done", map[string]any{
			"type": "response.reasoning_summary_part.done", "item_id": reasoningItemID,
			"output_index": reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": text},
		})
		send("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": reasoningIndex,
			"item": map[string]any{
				"type": "reasoning", "id": reasoningItemID, "status": "completed",
				"summary": []any{map[string]any{"type": "summary_text", "text": text}},
			},
		})
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		clean := stripDataPrefix(scanner.Text())
		if clean == "" {
			continue
		}
		if clean == "[DONE]" {
			break
		}
		if first {
			ttft = time.Since(started)
			s.metrics.RecordTTFT(modelName, ttft)
			first = false
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(clean), &chunk) != nil {
			continue
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		// 上游 error 帧（6004 限流 / 内容拦截 / 审核）：这一跳并没有成功，
		// 记下来在流尾按分类处置账号（见下方流尾分支）。
		if _, hasErr := chunk["error"]; hasErr {
			errorFrame = clean
			continue
		}
		choice, ok := firstChoice(chunk)
		if !ok {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if rc := extractReasoning(delta); rc != "" {
			if !reasoningStarted {
				reasoningStarted = true
				reasoningIndex = outputIndex
				outputIndex++
				send("response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": reasoningIndex,
					"item": map[string]any{
						"type": "reasoning", "id": reasoningItemID, "status": "in_progress",
						"summary": []any{},
					},
				})
				send("response.reasoning_summary_part.added", map[string]any{
					"type":          "response.reasoning_summary_part.added",
					"item_id":       reasoningItemID,
					"output_index":  reasoningIndex,
					"summary_index": 0,
					"part":          map[string]any{"type": "summary_text", "text": ""},
				})
			}
			reasoningText.WriteString(rc)
			send("response.reasoning_summary_text.delta", map[string]any{
				"type":          "response.reasoning_summary_text.delta",
				"item_id":       reasoningItemID,
				"output_index":  reasoningIndex,
				"summary_index": 0,
				"delta":         rc,
			})
		}

		if text := extractText(delta["content"]); text != "" {
			// 正文开始即思考结束。
			closeReasoning()
			if !itemAdded {
				itemAdded = true
				msgIndex = outputIndex
				outputIndex++
				send("response.output_item.added", map[string]any{
					"type":         "response.output_item.added",
					"output_index": msgIndex,
					"item": map[string]any{
						"type": "message", "id": msgItemID, "status": "in_progress",
						"role": "assistant", "content": []any{},
					},
				})
			}
			fullText.WriteString(text)
			seq++
			send("response.output_text.delta", map[string]any{
				"type":            "response.output_text.delta",
				"item_id":         msgItemID,
				"output_index":    msgIndex,
				"content_index":   0,
				"delta":           text,
				"sequence_number": seq,
			})
		}
		if tcs := toolCallsOf(delta); len(tcs) > 0 {
			mergeToolCalls(toolAcc, &toolOrder, tcs)
		}
	}
	if err := scanner.Err(); err != nil {
		interrupt = true
		reason := "上游流式响应中断，本次回复不完整"
		if errors.Is(err, context.DeadlineExceeded) || idleTripped(body) {
			reason = fmt.Sprintf("上游超过 %v 无数据，判定连接卡死并中断，本次回复不完整", s.config().IdleTimeout)
		}
		send("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": respID, "status": "failed",
				"error": map[string]any{"code": "stream_interrupted", "message": reason},
			},
		})
		s.metrics.RecordFailure(modelName, "流中断")
		s.logf("[异常] 账号=%s Responses 流式读取中断: %v", acc.Cred.AccountID(), err)
		s.sticky.Unbind(sessionKey)
	} else if errorFrame != "" {
		// 上游以 error 帧报错：与 chat 端点同一口径 —— 按帧分类处置账号、记失败、
		// 不记成功、不绑粘性，并向客户端下发 response.failed（帧原文一并带上，
		// 便于客户端看清是限流还是内容审核）。
		class := upstream.FrameClass(errorFrame)
		interrupt = true
		s.applyFrameClass(acc, modelName, class, errorFrame)
		s.metrics.RecordFailure(modelName, "上游错误帧("+class.Kind+")")
		s.logf("[异常] 账号=%s 模型=%s Responses 上游以 error 帧报错（分类=%s）：%s",
			acc.Cred.AccountID(), modelName, class.Kind, truncateForLog(errorFrame, 200))
		send("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": respID, "status": "failed",
				"error": map[string]any{
					"code":    "upstream_error_frame",
					"message": "上游以错误帧结束了本次响应: " + truncateForLog(errorFrame, 300),
				},
			},
		})
		s.sticky.Unbind(sessionKey)
	}

	// 流结束（正常结束、或只思考没出正文）都要收尾思考段。
	closeReasoning()

	if itemAdded {
		text := fullText.String()
		send("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": msgItemID,
			"output_index": msgIndex, "content_index": 0, "text": text,
		})
		send("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": msgIndex,
			"item": map[string]any{
				"type": "message", "id": msgItemID, "status": "completed",
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text", "text": text, "annotations": []any{},
				}},
			},
		})
	}
	for _, idx := range toolOrder {
		mc := toolAcc[idx]
		if mc == nil || mc.name == "" {
			continue
		}
		args := mc.args.String()
		callID := mc.id
		if callID == "" {
			callID = "call_" + randomID()
		}
		send("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": outputIndex,
			"item": map[string]any{
				"type": "function_call", "id": "fc_" + randomID(), "call_id": callID,
				"name": mc.name, "arguments": args, "status": "in_progress",
			},
		})
		send("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": outputIndex,
			"item": map[string]any{
				"type": "function_call", "id": "fc_" + randomID(), "call_id": callID,
				"name": mc.name, "arguments": args, "status": "completed",
			},
		})
		outputIndex++
	}

	total := tokensFromUsage(usage)
	s.metrics.RecordTokens(modelName, total)
	s.metrics.RecordLatency(modelName, time.Since(started))
	if !interrupt {
		// 真成功：读完且上游没有报 error 帧，才记成功、记账号成功并让粘性跟上。
		s.metrics.RecordSuccess(modelName)
		s.markHopSuccess(acc)
		final := buildResponsesEnvelope(respID, modelName, createdAt)
		final["status"] = "completed"
		final["output"] = []any{}
		final["output_text"] = fullText.String()
		final["usage"] = toResponsesUsage(usage)
		send("response.completed", map[string]any{"type": "response.completed", "response": final})
		s.sticky.Bind(sessionKey, acc.Cred.AccountID())
	}

	inputTokens, outputTokens := splitTokensFromUsage(usage)
	s.stats.Record(stats.RecordInput{
		Model:        modelName,
		Account:      acc.Cred.AccountID(),
		OK:           !interrupt,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  total,
		TTFTMs:       ttft.Milliseconds(),
	})
	s.logRequest(r, modelName, accountLogLabel(acc), "responses/stream",
		ttft, time.Since(started), total, interrupt)

	s.logf("[Responses] 模型=%s 账号=%s 流式 首字=%v 总耗时=%v 输出token=%d",
		modelName, acc.Cred.AccountID(), ttft, time.Since(started), total)
}

// -----------------------------------------------------------------------------
// 信封与工具函数
// -----------------------------------------------------------------------------

func buildResponsesEnvelope(id, model string, createdAt int64) map[string]any {
	return map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          createdAt,
		"status":              "in_progress",
		"model":               model,
		"output":              []any{},
		"parallel_tool_calls": true,
		"tools":               []any{},
		"usage":               nil,
	}
}

func toResponsesUsage(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	prompt, _ := numField(u, "prompt_tokens")
	completion, _ := numField(u, "completion_tokens")
	total, ok := numField(u, "total_tokens")
	if !ok {
		total = prompt + completion
	}
	return map[string]any{
		"input_tokens":  prompt,
		"output_tokens": completion,
		"total_tokens":  total,
	}
}

func collectOutputText(output []any) string {
	var sb strings.Builder
	for _, itemAny := range output {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := item["type"].(string); typ != "message" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, cAny := range content {
			c, ok := cAny.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := c["text"].(string); t != "" {
				sb.WriteString(t)
			}
		}
	}
	return sb.String()
}

// firstChoice 取第一个 choice（上游与 OpenAI 都是单 choice 语义）。
func firstChoice(obj map[string]any) (map[string]any, bool) {
	choices, ok := obj["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, false
	}
	c, ok := choices[0].(map[string]any)
	if !ok {
		return nil, false
	}
	return c, true
}

func toolCallsOf(msg map[string]any) []any {
	if msg == nil {
		return nil
	}
	tcs, _ := msg["tool_calls"].([]any)
	return tcs
}

// extractText 从 content 取纯文本：兼容 string 与 parts 数组两种形态。
// extractReasoning 取分片里的思考增量。
//
// 上游把思考放在 delta.reasoning_content（OpenAI 系第三方实现的常见字段），
// 少数实现用 delta.reasoning。两者都认，取有值的那个 ——
// 与 chat 侧 aggregateCompletion 的口径保持一致。
func extractReasoning(delta map[string]any) string {
	if s := asString(delta["reasoning_content"]); s != "" {
		return s
	}
	return asString(delta["reasoning"])
}

func extractText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, pAny := range c {
			p, ok := pAny.(map[string]any)
			if !ok {
				continue
			}
			switch typ, _ := p["type"].(string); typ {
			case "text", "output_text":
				sb.WriteString(asString(p["text"]))
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
