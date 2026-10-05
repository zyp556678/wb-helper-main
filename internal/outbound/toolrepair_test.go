package outbound

import (
	"encoding/json"
	"testing"
)

// 本文件的用例都锚在**实测的 11148 事故形态**上：上游（deepseek 系最严）要求
// 「声明了 tool_calls 的 assistant 之后必须紧跟它自己的结果」，任何断裂都会让
// 整条会话报废。修复动作宁可少做也不能做错，所以每个形态都断言**修后的拓扑字符串**。

// repair 跑一次自愈并返回报告、消息与拓扑。
func repair(t *testing.T, messagesJSON string) (ToolRepairReport, []any, string) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(`{"messages":`+messagesJSON+`}`), &obj); err != nil {
		t.Fatalf("测试消息不是合法 JSON: %v", err)
	}
	report := RepairToolSequence(obj)
	list, _ := obj["messages"].([]any)
	return report, list, toolMessageTopology(list)
}

// callMsg 构造一条「只有调用、没有正文」的 assistant 消息。
func callMsg(ids ...string) string {
	calls := ""
	for i, id := range ids {
		if i > 0 {
			calls += ","
		}
		calls += `{"id":"` + id + `","type":"function","function":{"name":"f","arguments":"{}"}}`
	}
	return `{"role":"assistant","content":null,"tool_calls":[` + calls + `]}`
}

// toolMsg 构造一条工具结果消息。
func toolMsg(id string) string { return `{"role":"tool","tool_call_id":"` + id + `","content":"ok"}` }

// sysMsg 构造一条普通消息（模拟 Codex 插在结果之间的 image_resize_notice）。
func sysMsg(text string) string { return `{"role":"developer","content":"` + text + `"}` }

// TestRepairFormTwoFoldsTextIntoCallMessage 覆盖形态二：纯正文 assistant 紧跟
// 「只有调用没有正文」的 assistant。
//
// 这是实机定位的 11148 形态之一：部分 OpenAI 兼容客户端回放历史时把「正文 + 调用声明」
// 拆成两条独立 assistant，于是结果前面隔了一条纯正文 assistant → 上游判
// tool_call_sequence_broken，整条会话之后每条消息都 400。
func TestRepairFormTwoFoldsTextIntoCallMessage(t *testing.T) {
	report, list, topo := repair(t, `[`+
		callMsg("c00")+`,`+
		`{"role":"assistant","content":"我先查一下文档"} ,`+
		toolMsg("c00")+`]`)

	if topo != "assistant[c00] > tool[c00]" {
		t.Fatalf("形态二未被修复，拓扑=%s", topo)
	}
	if report.FoldedTextMessages != 1 {
		t.Errorf("期望折叠 1 条正文，实际 %d", report.FoldedTextMessages)
	}
	if !report.Changed() {
		t.Errorf("折叠后报告应标记为已改动")
	}
	head, _ := list[0].(map[string]any)
	if content, _ := head["content"].(string); content != "我先查一下文档" {
		t.Errorf("正文没有折进调用消息：%v", head["content"])
	}
	calls, _ := head["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Errorf("调用声明被破坏：%d 条", len(calls))
	}
}

// TestRepairFormTwoKeepsExistingText 覆盖形态二的克制条件：上一条已带正文时不折叠
// （两条都有正文，拼接会改变对话含义），只把这条挪到结果之后。
func TestRepairFormTwoKeepsExistingText(t *testing.T) {
	report, _, topo := repair(t, `[`+
		`{"role":"assistant","content":"旧正文","tool_calls":[{"id":"c00","type":"function","function":{"name":"f","arguments":"{}"}}]},`+
		`{"role":"assistant","content":"新正文"},`+
		toolMsg("c00")+`]`)

	if topo != "assistant[c00] > tool[c00] > assistant" {
		t.Fatalf("两条都有正文时只应后移，实际拓扑=%s", topo)
	}
	if report.FoldedTextMessages != 0 || report.MovedMessages != 1 {
		t.Errorf("期望「只后移」：折叠=%d 后移=%d", report.FoldedTextMessages, report.MovedMessages)
	}
}

// TestRepairSingleCallMovesInsertedMessage 覆盖单调用场景下的插入物后移：
// 调用与结果之间隔着一条普通消息（Codex 的 image_resize_notice 就是这种形态），
// 上游同样判配对断裂。单调用也要后移，不能只在并行批次时才处理。
func TestRepairSingleCallMovesInsertedMessage(t *testing.T) {
	report, _, topo := repair(t, `[`+
		callMsg("c00")+`,`+
		sysMsg("&lt;image_resize_notice&gt;")+`,`+
		toolMsg("c00")+`]`)

	if topo != "assistant[c00] > tool[c00] > developer" {
		t.Fatalf("单调用的插入物未被后移，拓扑=%s", topo)
	}
	if report.MovedMessages != 1 || !report.Changed() {
		t.Errorf("期望后移 1 条并标记改动：%+v", report)
	}
}

// TestRepairParallelBatchMovesInsertedMessage 覆盖并行批次：结果之间夹着普通消息时，
// 结果必须连续，插入物整体后移。
func TestRepairParallelBatchMovesInsertedMessage(t *testing.T) {
	report, _, topo := repair(t, `[`+
		callMsg("c00", "c01")+`,`+
		toolMsg("c00")+`,`+
		sysMsg("<image_resize_notice>")+`,`+
		toolMsg("c01")+`]`)

	if topo != "assistant[c00,c01] > tool[c00] > tool[c01] > developer" {
		t.Fatalf("并行批次的结果没有连续，拓扑=%s", topo)
	}
	if report.MovedMessages != 1 {
		t.Errorf("期望后移 1 条，实际 %d", report.MovedMessages)
	}
}

// TestRepairFormOneMergesBackToBackCalls 覆盖形态一：背靠背的两条单调用 assistant
// 合成一条并行批次（2026-09-20 线上实测的 11148 正面修复）。
func TestRepairFormOneMergesBackToBackCalls(t *testing.T) {
	report, list, topo := repair(t, `[`+
		callMsg("c00")+`,`+
		callMsg("c01")+`,`+
		toolMsg("c00")+`,`+
		toolMsg("c01")+`]`)

	if topo != "assistant[c00,c01] > tool[c00] > tool[c01]" {
		t.Fatalf("形态一未被修复，拓扑=%s", topo)
	}
	if report.MergedCallMessages != 1 || report.ParallelBatches != 1 {
		t.Errorf("期望合并成 1 个并行批次：%+v", report)
	}
	head, _ := list[0].(map[string]any)
	calls, _ := head["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Errorf("并行调用声明应为 2 条，实际 %d", len(calls))
	}
}

// TestRepairReasoningTraceKeptWhenFolding 折叠/合并时思维链痕迹不能丢：
// deepseek 多轮要求 assistant 带 reasoning_content 回填，丢掉会换一个错误。
func TestRepairReasoningTraceKeptWhenFolding(t *testing.T) {
	_, list, topo := repair(t, `[`+
		callMsg("c00")+`,`+
		`{"role":"assistant","content":"正文","reasoning_content":"思考A"},`+
		toolMsg("c00")+`]`)

	if topo != "assistant[c00] > tool[c00]" {
		t.Fatalf("形态二未被修复，拓扑=%s", topo)
	}
	head, _ := list[0].(map[string]any)
	if rc, _ := head["reasoning_content"].(string); rc != "思考A" {
		t.Errorf("折叠后思维链痕迹丢失：%v", head["reasoning_content"])
	}
}

// TestRepairUntouchedWhenAlreadyValid 已合法的序列必须**零改动**：
// 「没有可归一的东西」时不能顺手重排，否则等于给正常会话引入新变量。
func TestRepairUntouchedWhenAlreadyValid(t *testing.T) {
	report, _, topo := repair(t, `[`+
		`{"role":"user","content":"你好"},`+
		callMsg("c00")+`,`+
		toolMsg("c00")+`]`)

	if topo != "user > assistant[c00] > tool[c00]" {
		t.Fatalf("合法序列被改动了，拓扑=%s", topo)
	}
	if report.Changed() {
		t.Errorf("合法序列不应报告改动：%+v", report)
	}
}

// TestRepairDropsOrphans 对称裁剪照旧：无结果的调用、无调用的结果一律剔除，
// 且不留半截配对（调用被删光时整条 assistant 消息若无正文也一并删掉）。
func TestRepairDropsOrphans(t *testing.T) {
	report, _, topo := repair(t, `[`+
		`{"role":"user","content":"你好"},`+
		callMsg("c1")+`,`+
		toolMsg("c2")+`]`)

	if topo != "user" {
		t.Fatalf("孤儿条目未被清干净，拓扑=%s", topo)
	}
	if report.DroppedCalls != 1 || report.DroppedOutputs != 1 {
		t.Errorf("裁剪计数不符：%+v", report)
	}
}

// TestRepairAssistantWithTextIsNotMerged 带正文的 assistant 声明不能被并入批次
// （那是语义内容，合并会改变对话含义），原样保留交给上游。
func TestRepairAssistantWithTextIsNotMerged(t *testing.T) {
	_, _, topo := repair(t, `[`+
		callMsg("c00")+`,`+
		`{"role":"assistant","content":"带正文的调用声明","tool_calls":[{"id":"c01","type":"function","function":{"name":"f","arguments":"{}"}}]},`+
		toolMsg("c00")+`,`+
		toolMsg("c01")+`]`)

	if topo != "assistant[c00] > assistant[c01] > tool[c00] > tool[c01]" {
		t.Fatalf("带正文的声明不应被合并/重排，拓扑=%s", topo)
	}
}

// TestRepairUnknownFieldsBlockMerge 带额外字段（name 等）的 assistant 不参与合并：
// 合并会静默丢字段，宁可不动。
func TestRepairUnknownFieldsBlockMerge(t *testing.T) {
	_, _, topo := repair(t, `[`+
		callMsg("c00")+`,`+
		`{"role":"assistant","content":null,"name":"probe","tool_calls":[{"id":"c01","type":"function","function":{"name":"f","arguments":"{}"}}]},`+
		toolMsg("c00")+`,`+
		toolMsg("c01")+`]`)

	if topo != "assistant[c00] > assistant[c01] > tool[c00] > tool[c01]" {
		t.Fatalf("带额外字段的消息不应被合并，拓扑=%s", topo)
	}
}
