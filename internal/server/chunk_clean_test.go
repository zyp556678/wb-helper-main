package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件守住「上游分片里的空值字段必须清掉」这条语义。
//
// 背景（实测，非推断）：上游每个分片都下发**整套** delta 字段 —— 一次 283 分片的
// 真实抓包里，`content:""` / `refusal:""` / `tool_calls:[]` / `function_call:null` /
// `extra_fields:null` 各出现 283 次，真正有值的只有 263 个 reasoning_content
// 与 18 个 content。
//
// 这个 bug 的隐蔽之处在于**它是客户端相关的**：用「值是否非空」判断分片类型的客户端
// 完全正常，只有用「字段是否存在」判断的客户端才会把思考分片里夹带的 `content:""`
// 读成「正文开始了」，于是每一片都关掉当前思考块再开一个 —— UI 上是一串 Thought 碎片。
// 同一份流的实测：前者修复前后都是 1 个思考块，后者从 283 个降到 1 个。
//
// 所以测试必须直接钉住**输出形态**，只测「能不能跑通」是抓不住它的。

// realThinkingChunk 是上游思考分片的真实形态（照抓包原样，含全部空壳字段）。
func realThinkingChunk(reasoning string) string {
	return `{"choices":[{"delta":{"content":"","extra_fields":null,"function_call":null,` +
		`"reasoning_content":"` + reasoning + `","refusal":"","role":"assistant","tool_calls":[]},` +
		`"finish_reason":null,"index":0,"logprobs":null}],"created":1790686086,` +
		`"id":"cmb-x","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","usage":null}`
}

// realContentChunk 是上游正文分片的真实形态。
func realContentChunk(text string) string {
	return `{"choices":[{"delta":{"content":"` + text + `","extra_fields":null,` +
		`"function_call":null,"reasoning_content":"","refusal":"","tool_calls":[]},` +
		`"finish_reason":null,"index":0,"logprobs":null}],"created":1790686086,` +
		`"id":"cmb-x","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","usage":null}`
}

func deltaOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, s)
	}
	choices, _ := doc["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("输出没有 choices: %s", s)
	}
	choice, _ := choices[0].(map[string]any)
	d, _ := choice["delta"].(map[string]any)
	if d == nil {
		t.Fatalf("输出没有 delta: %s", s)
	}
	return d
}

// blocksByFieldPresence 模拟「看字段是否存在」的客户端 —— 也就是本 bug 的暴露面。
// 返回它会把这段流切成几个思考块。
func blocksByFieldPresence(deltas []map[string]any) int {
	inReasoning, starts := false, 0
	for _, d := range deltas {
		if _, ok := d["reasoning_content"]; ok && !inReasoning {
			starts++
			inReasoning = true
		}
		if _, ok := d["content"]; ok && inReasoning {
			inReasoning = false
		}
	}
	return starts
}

func TestChunkCleanerStripsBlankDeltaFields(t *testing.T) {
	c := &chunkCleaner{}
	d := deltaOf(t, c.clean(realThinkingChunk("We")))

	for _, k := range []string{"content", "tool_calls", "function_call", "refusal", "extra_fields"} {
		if _, ok := d[k]; ok {
			t.Errorf("空值字段 %q 应当被清掉，实际仍在：%v", k, d[k])
		}
	}
	if got, _ := d["reasoning_content"].(string); got != "We" {
		t.Errorf("有值的 reasoning_content 不能丢，实际 %v", d["reasoning_content"])
	}
}

func TestChunkCleanerKeepsNonBlankValues(t *testing.T) {
	c := &chunkCleaner{}
	d := deltaOf(t, c.clean(realContentChunk("答案")))

	if got, _ := d["content"].(string); got != "答案" {
		t.Errorf("有值的 content 不能丢，实际 %v", d["content"])
	}
	if _, ok := d["reasoning_content"]; ok {
		t.Error("这是正文分片，空的 reasoning_content 应当被清掉")
	}
}

func TestChunkCleanerRoleOnlyOnFirstChunk(t *testing.T) {
	c := &chunkCleaner{}

	first := deltaOf(t, c.clean(realThinkingChunk("A")))
	if first["role"] != "assistant" {
		t.Fatalf("首个分片应当保留 role，实际 %v", first["role"])
	}

	second := deltaOf(t, c.clean(realThinkingChunk("B")))
	if _, ok := second["role"]; ok {
		t.Error("第二个分片不应再下发 role（规范只在首个分片给）")
	}
	if got, _ := second["reasoning_content"].(string); got != "B" {
		t.Errorf("去 role 的同时不能丢掉内容，实际 %v", second["reasoning_content"])
	}
}

func TestChunkCleanerNormalizesBlankFinishReason(t *testing.T) {
	c := &chunkCleaner{}
	out := c.clean(`{"choices":[{"delta":{"content":"x"},"finish_reason":"","index":0}]}`)

	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	choice := doc["choices"].([]any)[0].(map[string]any)
	if fr, ok := choice["finish_reason"]; !ok {
		t.Error("finish_reason 键应当保留（值为 null）")
	} else if fr != nil {
		t.Errorf("空 finish_reason 应当归一为 null，实际 %v", fr)
	}
}

func TestChunkCleanerKeepsRealFinishReason(t *testing.T) {
	c := &chunkCleaner{}
	out := c.clean(`{"choices":[{"delta":{},"finish_reason":"tool_calls","index":0}]}`)

	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("真实的 finish_reason 不能被改动：%s", out)
	}
}

// 数字 0 与布尔 false 是有效值，不是「空」——误删会改变语义。
func TestChunkCleanerKeepsZeroAndFalse(t *testing.T) {
	c := &chunkCleaner{}
	d := deltaOf(t, c.clean(`{"choices":[{"delta":{"content":"x","n":0,"b":false},"finish_reason":null}]}`))

	if _, ok := d["n"]; !ok {
		t.Error("数字 0 是有意义的值，不能被当成空值删掉")
	}
	if _, ok := d["b"]; !ok {
		t.Error("false 是有意义的值，不能被当成空值删掉")
	}
}

func TestChunkCleanerLeavesUnparsableAlone(t *testing.T) {
	c := &chunkCleaner{}
	for _, in := range []string{"event: ping", "{不是 JSON", ""} {
		if got := c.clean(in); got != in {
			t.Errorf("clean(%q) = %q，期望原样返回", in, got)
		}
	}
}

// 回归测试：这条形态的流，修复前在「字段存在」判断下会被切成「每片一个」思考块。
// 这里同时跑「清理后」与「未清理」两组，确保测试本身真的能抓到该 bug ——
// 只断言「清理后是 1」而不放对照组，这个测试在实现被改坏时也不会红。
func TestChunkCleanerCollapsesReasoningFragments(t *testing.T) {
	const reasoningChunks, contentChunks = 263, 18

	c := &chunkCleaner{}
	var cleaned, raw []map[string]any
	for i := 0; i < reasoningChunks; i++ {
		cleaned = append(cleaned, deltaOf(t, c.clean(realThinkingChunk("x"))))
		raw = append(raw, map[string]any{
			"content": "", "reasoning_content": "x", "role": "assistant",
			"tool_calls": []any{}, "function_call": nil, "refusal": "", "extra_fields": nil,
		})
	}
	for i := 0; i < contentChunks; i++ {
		cleaned = append(cleaned, deltaOf(t, c.clean(realContentChunk("y"))))
		raw = append(raw, map[string]any{
			"content": "y", "reasoning_content": "", "tool_calls": []any{},
			"function_call": nil, "refusal": "", "extra_fields": nil,
		})
	}

	if got := blocksByFieldPresence(cleaned); got != 1 {
		t.Errorf("清理后应当只有 1 个思考块，实际 %d", got)
	}
	// 对照组：未清理时**每一片**都带着 reasoning_content 字段（正文片带的是空串，
	// 但「判断字段存在」的写法认的是字段而非值），所以每片都会开一个新的思考块。
	if got := blocksByFieldPresence(raw); got != reasoningChunks+contentChunks {
		t.Fatalf("对照组（未清理）应当切成 %d 个思考块（每片一个），实际 %d —— 测试的模拟逻辑本身有问题",
			reasoningChunks+contentChunks, got)
	}
}
