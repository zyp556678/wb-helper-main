package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// stripDataPrefix 去掉 SSE 行的 "data:" 前缀并裁掉空白。
func stripDataPrefix(line string) string {
	if !strings.HasPrefix(line, "data:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:"))
}

// noteFinishReason 从分片里提取非空 finish_reason，已有值时不覆盖。
//
// 空串**不算**：上游会在每个中间分片上带 `finish_reason:""`，把它当结束标记会让
// 「流被截断」永远检测不出来。只有非空值（stop / length / tool_calls …）才代表
// 上游真的宣布了这一轮结束。
func noteFinishReason(current string, chunk map[string]any) string {
	if current != "" || chunk == nil {
		return current
	}
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		choice, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := choice["finish_reason"].(string); ok && v != "" {
			return v
		}
	}
	return current
}

// chunkCleaner 把上游分片规范成标准 OpenAI 形态后再透传。
//
// 上游是「类 OpenAI」实现，分片里带大量偏离规范的噪声。一次真实抓包的实测
// （283 个分片）：`content:""` 与 `refusal:""` 各出现 283 次、`tool_calls:[]` 283 次、
// `function_call:null` 283 次、`extra_fields:null` 283 次 —— 也就是**每一片都带全套
// delta 字段**，其中真正有值的只有 263 个 reasoning_content 与 18 个 content。
//
// 为什么必须清掉空值字段（这是「思考内容碎成一串」的直接原因）：
// 客户端判断「这一片是思考还是正文」有两种常见写法 —— 看值是否非空，看字段是否存在。
// 后者碰到思考分片里夹带的 `content:""`，会读成「正文开始了」，于是每来一片就关掉
// 当前思考块、再开一个新的。同一份流的实测对比：
//
//	用「值非空」判断：修复前 1 个思考块 → 修复后 1 个（无差别）
//	用「字段存在」判断：修复前 283 个思考块 → 修复后 1 个
//
// UI 上就是「一串 Thought 碎片」与「一个完整思考块」的区别。清掉空值字段后，
// 两种写法都得到正确结果，且对有值的内容零改动。
//
// 另外两处一并归一：
//   - `finish_reason:""` → `null`。规范要求中间分片为 null、只有终止分片给真实原因；
//     翻译层会取「第一个非 null 的 finish_reason」当 stop_reason，于是首个分片就把
//     stop_reason 锁成 end_turn，真实终止分片的 tool_calls 不再被采纳（表现为工具不执行）。
//   - `role` 只保留在首个分片。规范如此；重复下发同样是「存在但无信息」的噪声，
//     可能被读成「新消息开始」。
//
// 无法解析时原样返回：宁可不改，也不能把可用内容弄丢。
type chunkCleaner struct {
	roleSent bool
}

func (c *chunkCleaner) clean(raw string) string {
	if raw == "" {
		return ""
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		return raw
	}
	changed := false

	if choices, ok := chunk["choices"].([]any); ok {
		for _, cv := range choices {
			choice, ok := cv.(map[string]any)
			if !ok {
				continue
			}
			if fr, exists := choice["finish_reason"]; exists {
				if s, isStr := fr.(string); isStr && s == "" {
					choice["finish_reason"] = nil
					changed = true
				}
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			// 先在普通字段里清空值；role 单独处理（它需要跨分片去重）。
			for k, v := range delta {
				if k == "role" {
					continue
				}
				if isEmptyValue(v) {
					delete(delta, k)
					changed = true
				}
			}
			if role, exists := delta["role"]; exists {
				switch {
				case isEmptyValue(role), c.roleSent:
					delete(delta, "role")
					changed = true
				default:
					c.roleSent = true
				}
			}
		}
	}

	if !changed {
		return raw
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return raw
	}
	return string(out)
}

// isEmptyValue 判断字段是否「存在但不携带信息」。
//
// 刻意只认这四类：null、空串、空数组、空对象。数字与布尔一律不视为空
// —— `0` / `false` 是有意义的值，误删会改变语义。
func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	default:
		return false
	}
}

// usageFromCompletion 从聚合结果里取 usage 对象。
func usageFromCompletion(completionJSON []byte) map[string]any {
	var doc map[string]any
	if err := json.Unmarshal(completionJSON, &doc); err != nil {
		return nil
	}
	if u, ok := doc["usage"].(map[string]any); ok {
		return u
	}
	return nil
}

// tokensFromUsage 取总 token 数：优先 total_tokens，否则 prompt+completion。
func tokensFromUsage(usage map[string]any) int64 {
	if usage == nil {
		return 0
	}
	if v, ok := numField(usage, "total_tokens"); ok {
		return v
	}
	p, _ := numField(usage, "prompt_tokens")
	c, _ := numField(usage, "completion_tokens")
	return p + c
}

// splitTokensFromUsage 拆出输入与输出 token。
//
// 为什么要拆：总量答不了「消耗主要来自提示词还是生成」——输入侧长上下文贵但常命中缓存，
// 输出侧单价通常更高。更重要的是**官方计费口径本身就是分开的**，不拆就没法把
// 网关自己的用量与官方用量逐项对照，而对照正是发现漏记/重复计的手段。
//
// 上游若只给 total_tokens（不分输入输出），这里两个返回值都是 0；
// 调用方必须同时把总量传给 stats.RecordInput.TotalTokens，否则会丢量。
func splitTokensFromUsage(usage map[string]any) (input, output int64) {
	if usage == nil {
		return 0, 0
	}
	input, _ = numField(usage, "prompt_tokens")
	output, _ = numField(usage, "completion_tokens")
	return input, output
}

func numField(m map[string]any, key string) (int64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

// mergedToolCall 按 index 累积工具调用增量。
type mergedToolCall struct {
	id   string
	typ  string
	name string
	args strings.Builder
}

// aggregateCompletion 把上游 SSE 流聚合成一次完整的 Chat Completions 响应。
// 支持 content / reasoning_content / tool_calls 三类增量，并保留 usage。
//
// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）时返回错误而不是假成功：
// error 帧没有 choices，若当成普通分片忽略，聚合结果会是一条「finish_reason=stop、
// 正文为空」的完整响应 —— 客户端与统计都会把它当成一次正常回复。
func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var (
		content   strings.Builder
		reasoning strings.Builder
		role      = "assistant"
		finish    any
		id        string
		created   int64
		usage     map[string]any

		toolCalls = map[int]*mergedToolCall{}
		order     []int

		errorFrame string
	)

	for scanner.Scan() {
		clean := stripDataPrefix(scanner.Text())
		if clean == "" || clean == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(clean), &chunk); err != nil {
			continue
		}
		if _, hasErr := chunk["error"]; hasErr {
			errorFrame = clean
		}
		if id == "" {
			if s, ok := chunk["id"].(string); ok {
				id = s
			}
		}
		if c, ok := numField(chunk, "created"); ok && created == 0 {
			created = c
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, ok := chunk["choices"].([]any)
		if !ok {
			continue
		}
		for _, cv := range choices {
			choice, ok := cv.(map[string]any)
			if !ok {
				continue
			}
			if fr, exists := choice["finish_reason"]; exists {
				if s, isStr := fr.(string); !isStr || s != "" {
					finish = fr
				}
			}
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			if s, ok := delta["role"].(string); ok && s != "" {
				role = s
			}
			if s, ok := delta["content"].(string); ok {
				content.WriteString(s)
			}
			if s, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(s)
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				mergeToolCalls(toolCalls, &order, tcs)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if errorFrame != "" {
		return nil, fmt.Errorf("上游以 error 帧报错: %s", truncateForLog(errorFrame, 300))
	}

	message := map[string]any{"role": role}
	if content.Len() > 0 {
		message["content"] = content.String()
	} else {
		message["content"] = nil
	}
	if reasoning.Len() > 0 {
		// 推理内容回填：多轮对话时客户端会把它作为 assistant 消息带回，
		// 缺失会导致推理模型上下文断裂。
		message["reasoning_content"] = reasoning.String()
	}
	if len(order) > 0 {
		message["tool_calls"] = buildToolCalls(toolCalls, order)
	}

	if finish == nil {
		if len(order) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}

	out := map[string]any{
		"id":      ifEmpty(id, "chatcmpl-"+randomID()),
		"object":  "chat.completion",
		"created": chooseInt64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

func mergeToolCalls(calls map[int]*mergedToolCall, order *[]int, tcs []any) {
	for _, tcv := range tcs {
		tc, ok := tcv.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if n, ok := numField(tc, "index"); ok {
			idx = int(n)
		}
		cur, exists := calls[idx]
		if !exists {
			cur = &mergedToolCall{}
			calls[idx] = cur
			*order = append(*order, idx)
		}
		if s, ok := tc["id"].(string); ok && s != "" {
			cur.id = s
		}
		if s, ok := tc["type"].(string); ok && s != "" {
			cur.typ = s
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if s, ok := fn["name"].(string); ok && s != "" {
				cur.name = s
			}
			if s, ok := fn["arguments"].(string); ok {
				cur.args.WriteString(s)
			}
		}
	}
}

func buildToolCalls(calls map[int]*mergedToolCall, order []int) []any {
	out := make([]any, 0, len(order))
	for _, idx := range order {
		c := calls[idx]
		if c == nil || c.name == "" {
			continue
		}
		typ := c.typ
		if typ == "" {
			typ = "function"
		}
		out = append(out, map[string]any{
			"id":   ifEmpty(c.id, "call_"+randomID()),
			"type": typ,
			"function": map[string]any{
				"name":      c.name,
				"arguments": c.args.String(),
			},
		})
	}
	return out
}

func ifEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func chooseInt64(created int64) int64 {
	if created == 0 {
		return nowUnix()
	}
	return created
}
