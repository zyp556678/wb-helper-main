package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// Anthropic Messages 协议转换的测试
//
// 协议翻译最容易出的错是「形状对但语义错」（stop_reason 映射反了、
// usage 把缓存算两遍、块顺序错了），所以这里逐条钉住用户可见的行为。
// -----------------------------------------------------------------------------

// 请求转换：system 数组 / tool_result → tool 消息 / tool_use → tool_calls / thinking → reasoning_effort。
func TestAnthropicToChatRequest(t *testing.T) {
	body := map[string]any{
		"model":      "claude-sonnet-4",
		"max_tokens": float64(1024),
		"system": []any{
			map[string]any{"type": "text", "text": "你是助手", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "回答要简短"},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "帮我看看这段代码"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "我先读文件"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"path": "a.go"}},
				map[string]any{"type": "thinking", "thinking": "历史思维链不上传"},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "文件内容"},
				map[string]any{"type": "text", "text": "继续"},
			}},
		},
		"tools": []any{
			map[string]any{"name": "Read", "description": "读文件", "input_schema": map[string]any{"type": "object"}},
		},
		"tool_choice":    map[string]any{"type": "tool", "name": "Read"},
		"thinking":       map[string]any{"type": "enabled", "budget_tokens": float64(20000)},
		"stop_sequences": []any{"\n\n"},
	}
	chat, err := anthropicToChatRequest(body, "claude-sonnet-4")
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if chat["max_tokens"] != 1024 {
		t.Fatalf("max_tokens 应透传，实际 %v", chat["max_tokens"])
	}
	if chat["reasoning_effort"] != "high" {
		t.Fatalf("budget 20000 应映射为 high，实际 %v", chat["reasoning_effort"])
	}
	messages, _ := chat["messages"].([]any)
	if len(messages) != 5 {
		t.Fatalf("应转出 5 条消息（system + user + assistant + tool + user），实际 %d：%+v", len(messages), messages)
	}
	if m, _ := messages[0].(map[string]any); m["role"] != "system" || !strings.Contains(strOfAny(m["content"]), "你是助手") {
		t.Fatalf("第一条应是合并后的 system，实际 %+v", messages[0])
	}
	assistant, _ := messages[2].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("第三条应是 assistant，实际 %+v", assistant)
	}
	if calls, ok := assistant["tool_calls"].([]any); !ok || len(calls) != 1 {
		t.Fatalf("assistant 应带 1 个 tool_call，实际 %+v", assistant)
	} else {
		fn, _ := calls[0].(map[string]any)["function"].(map[string]any)
		if fn["name"] != "Read" || !strings.Contains(strOfAny(fn["arguments"]), "a.go") {
			t.Fatalf("tool_call 应带名字与参数，实际 %+v", fn)
		}
	}
	tool, _ := messages[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" || tool["content"] != "文件内容" {
		t.Fatalf("第四条应是 tool 消息且带 tool_use_id，实际 %+v", tool)
	}
	// tool_choice 指定工具：只暴露该工具 + required（上游只认字符串枚举）。
	if chat["tool_choice"] != "required" {
		t.Fatalf("指定工具名的 tool_choice 应为 required，实际 %v", chat["tool_choice"])
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("指定工具时应只暴露该工具，实际 %d 个", len(tools))
	}
	if _, ok := chat["stop"]; !ok {
		t.Fatal("stop_sequences 应转成 stop")
	}
}

// thinking 关闭时不注入档位；adaptive 也算启用（新版 Claude Code 对未知模型一律发 adaptive）。
func TestAnthropicThinkingModes(t *testing.T) {
	disabled := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"thinking": map[string]any{"type": "disabled"},
	}
	chat, err := anthropicToChatRequest(disabled, "m")
	if err != nil {
		t.Fatal(err)
	}
	if chat["reasoning_effort"] != "off" {
		t.Fatalf("disabled 应显式关闭，实际 %v", chat["reasoning_effort"])
	}
	if anthropicThinkingOn(disabled) {
		t.Fatal("disabled 不该被当作启用")
	}

	adaptive := map[string]any{"thinking": map[string]any{"type": "adaptive"}}
	if !anthropicThinkingOn(adaptive) {
		t.Fatal("adaptive 必须算启用（否则思考过程会被整段丢弃）")
	}
}

// 完全没有 messages 必须拒绝（而不是发一个空请求给上游）；
// 只有 system 的请求是合法的（会转成一条 system 消息）。
func TestAnthropicToChatRequestRejectsEmpty(t *testing.T) {
	if _, err := anthropicToChatRequest(map[string]any{"max_tokens": float64(10)}, "m"); err == nil {
		t.Fatal("messages 缺失或为空时应报错")
	}
	chat, err := anthropicToChatRequest(map[string]any{"system": "只有系统提示"}, "m")
	if err != nil {
		t.Fatalf("只有 system 的请求应被接受: %v", err)
	}
	if msgs, _ := chat["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("应转出一条 system 消息，实际 %+v", chat["messages"])
	}
}

// 流式回译：事件顺序与内容必须严格符合 Anthropic 语义。
func TestAnthropicStreamEventOrder(t *testing.T) {
	state := newAnthropicStreamState("test-model", false)
	var all []string
	feed := func(chunk map[string]any) {
		out := state.feed(chunk)
		all = append(all, string(out))
	}
	feed(map[string]any{
		"model":   "test-model",
		"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant"}}},
	})
	feed(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "你好"}}}})
	feed(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "，世界"}}}})
	feed(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": float64(0), "id": "call_1",
				"function": map[string]any{"name": "Read", "arguments": `{"path":`},
			}}},
		}},
	})
	feed(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": float64(0), "function": map[string]any{"arguments": `"a.go"}`},
			}}},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": float64(100), "completion_tokens": float64(20),
			"prompt_tokens_details": map[string]any{"cached_tokens": float64(40)}},
	})
	all = append(all, string(state.finishEvents()))
	joined := strings.Join(all, "")

	// 事件顺序：message_start → text 块 → tool_use 块 → message_delta → message_stop
	order := []string{
		"event: message_start",
		`"type":"content_block_start"`,
		"event: message_delta",
		"event: message_stop",
	}
	pos := -1
	for _, want := range order {
		idx := strings.Index(joined, want)
		if idx < 0 {
			t.Fatalf("缺少事件 %q\n%s", want, joined)
		}
		if idx < pos {
			t.Fatalf("事件顺序错误：%q 出现在 %q 之前\n%s", want, order[0], joined)
		}
		pos = idx
	}
	if !strings.Contains(joined, `"text":"你好"`) || !strings.Contains(joined, `"type":"text_delta"`) {
		t.Fatalf("正文增量应逐段透传，实际：%s", joined)
	}
	if !strings.Contains(joined, `"type":"input_json_delta"`) || !strings.Contains(joined, `partial_json`) {
		t.Fatalf("工具参数应分片透传，实际：%s", joined)
	}
	if strings.Count(joined, `"type":"content_block_start"`) != 2 {
		t.Fatalf("应有 2 个内容块（text + tool_use），实际：%s", joined)
	}
	// usage：input = 100-40（缓存单列），缓存 40，输出 20。
	if !strings.Contains(joined, `"input_tokens":60`) || !strings.Contains(joined, `"cache_read_input_tokens":40`) {
		t.Fatalf("usage 口径错误（缓存被算了两遍？）：%s", joined)
	}
	if !strings.Contains(joined, `"stop_reason":"tool_use"`) {
		t.Fatalf("tool_calls 应映射为 tool_use：%s", joined)
	}
}

// 非流式聚合：正文 + 工具调用拼装成完整 Message，参数不是合法 JSON 时原样返回。
func TestAnthropicAggregate(t *testing.T) {
	state := newAnthropicStreamState("test-model", true)
	state.feed(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"reasoning_content": "先想一下",
	}}}})
	state.feed(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "答案是 42"}}}})
	state.feed(map[string]any{"choices": []any{map[string]any{
		"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": float64(0), "id": "call_1",
			"function": map[string]any{"name": "Read", "arguments": `{"path":"a.go"}`},
		}}},
		"finish_reason": "tool_calls",
	}}})
	msg := state.aggregate()
	content, _ := msg["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("应聚合出 3 个块（thinking + text + tool_use），实际 %d：%+v", len(content), content)
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "thinking" || first["signature"] == "" {
		t.Fatalf("thinking 块应带签名，实际 %+v", first)
	}
	last, _ := content[2].(map[string]any)
	if last["type"] != "tool_use" || last["name"] != "Read" {
		t.Fatalf("tool_use 块应带名字，实际 %+v", last)
	}
	if input, ok := last["input"].(map[string]any); !ok || input["path"] != "a.go" {
		t.Fatalf("tool_use 的 input 应解析成对象，实际 %+v", last["input"])
	}
	if msg["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason 应为 tool_use，实际 %v", msg["stop_reason"])
	}
}

// 未启用 thinking 时，上游的推理增量**不**回译（否则客户端会看到不认识的块）。
func TestAnthropicThinkingNotEmittedWhenDisabled(t *testing.T) {
	state := newAnthropicStreamState("m", false)
	out := string(state.feed(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"reasoning_content": "内部推理",
	}}}}))
	if strings.Contains(out, "thinking") {
		t.Fatalf("未启用 thinking 时不该下发 thinking 块：%s", out)
	}
}

// count_tokens 本地估算：CJK 约 1 token/字、ASCII 约 4 字符/token，且不调用上游。
func TestEstimateAnthropicInputTokens(t *testing.T) {
	body := map[string]any{
		"system": "系统提示",
		"messages": []any{
			map[string]any{"role": "user", "content": "你好世界"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t", "content": "结果"},
			}},
		},
	}
	got := estimateAnthropicInputTokens(body)
	// 8 个 CJK 字 + 换行 → 至少 8，且远小于 100。
	if got < 8 || got > 30 {
		t.Fatalf("估算值应落在合理区间，实际 %d", got)
	}
}

// 错误形状：Anthropic 路由回 Anthropic 形状，其余回 OpenAI 形状。
func TestErrorShapeByProtocol(t *testing.T) {
	s := &Server{}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	s.writeAPIError(rec, req, http.StatusUnauthorized, "invalid_api_key", "未提供有效 API 密钥")
	var anthropicErr map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &anthropicErr); err != nil {
		t.Fatal(err)
	}
	if anthropicErr["type"] != "error" {
		t.Fatalf("Anthropic 形状应有顶层 type=error，实际 %+v", anthropicErr)
	}
	inner, _ := anthropicErr["error"].(map[string]any)
	if inner["type"] != "authentication_error" {
		t.Fatalf("invalid_api_key 应映射为 authentication_error，实际 %+v", inner)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.writeAPIError(rec2, req2, http.StatusUnauthorized, "invalid_api_key", "未提供有效 API 密钥")
	var openaiErr map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &openaiErr); err != nil {
		t.Fatal(err)
	}
	if _, ok := openaiErr["error"]; !ok || openaiErr["type"] == "error" {
		t.Fatalf("OpenAI 形状应是 {error:{...}}，实际 %+v", openaiErr)
	}
}
