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

// ---------------------------------------------------------------------------
// 空 function.name 的剔除（对齐 wb2api-panel 535e19e）
//
// 事故形态：上游流式响应在工具调用 name 分片到达前异常结束（200 空流也算），
// 聚合器按占位符产出 name:"" 的条目 → 客户端写进会话历史 → 下一轮原样转发 →
// 上游判 400 code=11133「tool_calls[i].function.name must be a non-empty string」。
// 这是**消息参数级**拒绝，换账号也无效，整条会话从此每轮 503。
//
// 关键是**两侧对称剔除**：只删调用不删结果，会留下「无 tool_calls 的 assistant +
// 孤儿 tool」→ 上游判 11148，比原 bug 更糟。所以下面每个用例都同时断言
// 最终拓扑与报告计数。
// ---------------------------------------------------------------------------

// badCallMsg 构造一条 name 为空（或缺失）的调用声明。
// nameJSON 传裸 JSON 片段，便于区分「name 是空串」与「根本没有 name 字段」。
func badCallMsg(id, nameJSON string) string {
	return `{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"` + id + `","type":"function","function":{` + nameJSON + `}}]}`
}

// TestRepairDropsEmptyNameCallWithResult 带配对结果的空 name 调用必须被剔除 ——
// 这正是原 bug 的形态：按 id 对称裁剪对「配对齐全的脏条目」不设防，原样放行。
func TestRepairDropsEmptyNameCallWithResult(t *testing.T) {
	report, list, topo := repair(t, `[`+
		badCallMsg("c1", `"name":"","arguments":"{}"`)+`,`+
		toolMsg("c1")+`]`)

	if topo != "" {
		t.Fatalf("空 name 的调用与它的结果都应被剔除，拓扑=%q", topo)
	}
	if report.DroppedCalls != 1 || report.DroppedOutputs != 1 {
		t.Errorf("两侧应各剔一条，实际 calls=%d outputs=%d",
			report.DroppedCalls, report.DroppedOutputs)
	}
	if len(list) != 0 {
		t.Errorf("剔除后不应留任何消息，实际 %d 条", len(list))
	}
}

// TestRepairDropsEmptyNameCallLeavesNoOrphan 混合场景：一批里只有脏项该被剔除，
// 干净项的调用与结果都原样保留、顺序不变。
func TestRepairDropsEmptyNameCallLeavesNoOrphan(t *testing.T) {
	report, _, topo := repair(t, `[`+
		callMsg("c0")+`,`+
		`{"role":"assistant","content":null,"tool_calls":[`+
		`{"id":"c1","type":"function","function":{"name":"  ","arguments":"{}"}},`+
		`{"id":"c2","type":"function","function":{"name":"f","arguments":"{}"}}]},`+
		toolMsg("c0")+`,`+
		toolMsg("c1")+`,`+
		toolMsg("c2")+`]`)

	// c1 被剔；剩 c0 与 c2 变成背靠背的空正文调用，被既有的「形态一」规则
	// 合并成一个并行批次（两条都无正文，合并无语义损失），结果顺序不变。
	if topo != "assistant[c0,c2] > tool[c0] > tool[c2]" {
		t.Fatalf("只应剔掉 c1 及其结果，拓扑=%s", topo)
	}
	if report.DroppedCalls != 1 || report.DroppedOutputs != 1 {
		t.Errorf("应恰好各剔一条，实际 calls=%d outputs=%d",
			report.DroppedCalls, report.DroppedOutputs)
	}
}

// TestRepairEmptyNameIsWhitespaceOnly 纯空白（空格、制表、换行）与空串同判：
// 上游只要求「非空字符串」，纯空白同样被拒。
func TestRepairEmptyNameIsWhitespaceOnly(t *testing.T) {
	for _, name := range []string{`""`, `"   "`, `"\t\n"`} {
		_, list, topo := repair(t, `[`+
			badCallMsg("c1", `"name":`+name+`,"arguments":"{}"`)+`,`+
			toolMsg("c1")+`]`)
		if topo != "" {
			t.Errorf("name=%s 应被判脏剔除，拓扑=%q", name, topo)
		}
		if len(list) != 0 {
			t.Errorf("name=%s 剔除后不应留消息", name)
		}
	}
}

// TestRepairKeepsCallWithoutNameField 缺 name 字段的占位条目不判脏：
// 上游不因缺字段拒参，这里**不扩大打击面**（与「空 id 双侧不识别」同口径）。
func TestRepairKeepsCallWithoutNameField(t *testing.T) {
	_, _, topo := repair(t, `[`+
		badCallMsg("c1", `"arguments":"{}"`)+`,`+
		toolMsg("c1")+`]`)

	if topo != "assistant[c1] > tool[c1]" {
		t.Fatalf("缺 name 字段不应被判脏，拓扑=%s", topo)
	}
}

// TestRepairEmptyNameWithoutResultStillDropped 空 name 且**没有**配对结果的条目：
// 走孤儿规则被删。此前若在收集阶段跳过登记，这种形态会漏出站。
func TestRepairEmptyNameWithoutResultStillDropped(t *testing.T) {
	report, list, topo := repair(t, `[`+
		badCallMsg("c1", `"name":"","arguments":"{}"`)+`]`)

	if topo != "" {
		t.Fatalf("无结果的空 name 调用也应被剔除，拓扑=%q", topo)
	}
	if report.DroppedCalls != 1 {
		t.Errorf("应剔除 1 条调用，实际 %d", report.DroppedCalls)
	}
	if len(list) != 0 {
		t.Errorf("剔除后不应留消息，实际 %d 条", len(list))
	}
}

// TestToolCallNameEmptyPrecise 直接钉住判定函数的精确边界：
// 只有「function 存在 + name 字段存在 + 值为空白」才判真。
func TestToolCallNameEmptyPrecise(t *testing.T) {
	cases := []struct {
		name string
		call any
		want bool
	}{
		{"空串", map[string]any{"function": map[string]any{"name": ""}}, true},
		{"纯空白", map[string]any{"function": map[string]any{"name": "  "}}, true},
		{"正常名字", map[string]any{"function": map[string]any{"name": "f"}}, false},
		{"缺 name 字段", map[string]any{"function": map[string]any{"arguments": "{}"}}, false},
		{"缺 function 对象", map[string]any{"id": "c1"}, false},
		{"name 不是字符串", map[string]any{"function": map[string]any{"name": 42}}, false},
		{"不是对象", "c1", false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := toolCallNameEmpty(tc.call); got != tc.want {
			t.Errorf("%s: toolCallNameEmpty=%v，期望 %v", tc.name, got, tc.want)
		}
	}
}
