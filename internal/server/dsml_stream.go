package server

import (
	"workbuddy-gateway/internal/dsml"
)

// markupStreamRepair 把上游 chat 流里的「模型原生工具调用标记」还原成结构化
// delta.tool_calls。它是 dsml.MarkupRepair 的流式包装：状态机本身与流无关，
// 这里的职责只有「一帧拆成多帧」。
//
// 为什么不塞进 chunkCleaner：两者的语义正交且都需要跨帧状态。
//   - chunkCleaner 是「逐帧清空值」：原地改一个 chunk，一帧进一帧出。
//   - 本组件是「一帧拆多帧」：一帧可能拆成「正文帧 + N 个调用帧 + 收尾帧」，
//     还要重排 finish_reason 的位置。
//
// 合成帧不带 role，cleaner 的 role 去重因此天然不受影响。
type markupStreamRepair struct {
	repair *dsml.MarkupRepair
	// synthIdx 是已合成的调用数，用作续接的下标（同一条流里前面可能已有真实调用帧）。
	synthIdx int
	// toolFinishSent 记「已经把收尾的 stop 改写为 tool_calls 并发出去」，
	// 避免在多帧里重复改写。
	toolFinishSent bool
}

// newMarkupStreamRepair 按请求声明构造。declaredTools 为空且未开启弱判定时，
// 返回的实例 Enabled() 为 false，所有方法恒等透传（零改写零分配）。
func newMarkupStreamRepair(declaredTools map[string]bool, allowUnknown bool) *markupStreamRepair {
	return &markupStreamRepair{repair: dsml.NewMarkupRepair(declaredTools, allowUnknown)}
}

// Enabled 报告是否启用。关闭时调用方应跳过整个流程。
func (m *markupStreamRepair) Enabled() bool {
	return m != nil && m.repair.Enabled()
}

// rewrite 消费一个上游分片，返回「应当写出的分片序列」。
//
// 未命中标记时原样返回入参（单元素切片），与未启用修复时逐字节一致。
// 命中时返回：正文帧（标记被摘掉后的可见文本）+ N 个 tool_calls 帧 + 收尾帧，
// 且收尾帧的 finish_reason 排在调用帧之后（客户端读到调用之前就认为回合结束）。
func (m *markupStreamRepair) rewrite(chunk map[string]any) []map[string]any {
	if !m.Enabled() {
		return []map[string]any{chunk}
	}
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return []map[string]any{chunk}
	}

	var calls []dsml.MarkupCall
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := c["delta"].(map[string]any)
		if !ok {
			continue
		}
		text, ok := delta["content"].(string)
		if !ok || text == "" {
			continue
		}
		emit, got := m.repair.Feed(text)
		calls = append(calls, got...)
		if emit == "" {
			delete(delta, "content")
		} else {
			delta["content"] = emit
		}
	}

	if len(calls) == 0 {
		// 本帧没命中标记；但本流此前还原过调用 → 把上游的 stop 收尾改写成
		// tool_calls，客户端据此知道「回合以工具调用结束」而不是「模型只说了话」。
		if m.synthIdx > 0 && rewriteFinishReason(chunk, "tool_calls") {
			m.toolFinishSent = true
		}
		return []map[string]any{chunk}
	}

	// 命中：finish_reason 必须挪到工具调用帧之后。注意「本帧本来没有 finish_reason」
	// 时**不得**补一帧收尾 —— 上游的收尾帧随后就到，补了会变成两个 finish_reason
	// （后续那个由 rewriteFinishReason 改写）。
	fr := takeFinishReason(chunk)
	out := []map[string]any{chunk}
	for _, call := range calls {
		out = append(out, markupCallFrame(chunk, m.synthIdx, call))
		m.synthIdx++
	}
	if fr != "" {
		if fr == "stop" {
			fr = "tool_calls"
		}
		out = append(out, finishOnlyFrame(chunk, fr))
		m.toolFinishSent = fr == "tool_calls"
	}
	return out
}

// flush 在流结束时调用：把状态机里未判定的尾部缓冲交还（可能有未闭合块）。
// 附带一次收尾改写：若整条流以标记结尾、上游的 stop 又被截断在缓冲里，
// 这里补上 tool_calls 收尾。
func (m *markupStreamRepair) flush() (string, bool) {
	if !m.Enabled() {
		return "", false
	}
	return m.repair.Flush(), m.synthIdx > 0 && !m.toolFinishSent
}

// takeFinishReason 取走并删除所有 choice 的 finish_reason，返回第一个非空值。
func takeFinishReason(obj map[string]any) string {
	fr := ""
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v != "" && fr == "" {
			fr = v
		}
		delete(c, "finish_reason")
	}
	return fr
}

// rewriteFinishReason 把 stop 收尾改写成 want。只动 stop，不碰别的终态。
func rewriteFinishReason(obj map[string]any, want string) bool {
	changed := false
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v == "stop" {
			c["finish_reason"] = want
			changed = true
		}
	}
	return changed
}

// markupCallFrame 造一个只含单个 tool_calls 增量的分片。created / model 从
// 源分片继承，保持与上游同一轮的元信息。
func markupCallFrame(src map[string]any, idx int, call dsml.MarkupCall) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": dsml.ToolCallDeltas([]dsml.MarkupCall{call}, idx),
			},
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}

// finishOnlyFrame 造一个只带 finish_reason 的收尾分片。
func finishOnlyFrame(src map[string]any, finish string) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}

// contentOnlyFrame 造一个只带正文增量的分片（流尾回吐未判定的缓冲用）。
func contentOnlyFrame(text string) map[string]any {
	return map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": text},
		}},
	}
}
