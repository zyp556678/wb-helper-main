package outbound

import "strings"

// 本文件是工具调用（tool_calls / tool）消息序列的自愈。
//
// 为什么必须有：上游对工具调用序列的结构校验很严，一旦出现「调用没有结果」「结果找不到调用」
// 「并行调用被普通消息打断」这类断裂，就会对之后**每一条**消息都返 11148，
// 表现为整段会话彻底不可用、且用户完全看不出哪里错了。
// 而这类断裂几乎全来自「客户端把 Responses 历史项转成 chat 消息」的转换，
// 属于我们必须替客户端兜住的协议误差。
//
// 修法分三步：
//  1. 按 tool call ID 做对称裁剪——无结果的调用、无调用的结果、重复项全部删除；
//  2. 把「声明了 tool_calls 的 assistant」后面紧跟的纯正文 assistant 折进上一条，
//     合成 assistant(正文 + tool_calls)（形态二）；
//  3. 把夹在调用与结果之间的普通消息挪到整组之后，并把背靠背的单调用 assistant
//     合并成一个并行批次。
//
// 刻意不做的事：不按调用顺序重排结果（保持原始出现顺序），不合并**两条都有正文**的
// assistant 消息（那是语义内容，动了会改变对话含义）。
//
// 值得记住的实机对照：「声明了 tool_calls 的 assistant 之后必须紧跟它自己的结果」是
// 上游对 deepseek 系最严的一条校验——紧随的若是另一条 assistant，即 400 code=11148
// （tool_call_sequence_broken），整条会话报废：客户端每次重试重放同一条历史，
// 池侧换号也无效（不是账号问题）。同一批调用实测：拆成两条 assistant +
// deepseek-v4.1-flash → 503/11148；合成一条 assistant → 200（glm 系对该形态宽容）。

// ToolRepairReport 记录一次自愈的统计，用于日志与排查。
type ToolRepairReport struct {
	OriginalMessages   int
	FinalMessages      int
	ParallelBatches    int
	MovedMessages      int
	MergedCallMessages int
	// FoldedTextMessages 是「纯正文 assistant 被折进前一条 tool_calls 消息」的条数
	// （形态二）。
	FoldedTextMessages int
	DroppedCalls       int
	DroppedOutputs     int
	TopologyBefore     string
	TopologyAfter      string
}

// Changed 报告是否真的改动了消息序列。
func (r ToolRepairReport) Changed() bool {
	return r.OriginalMessages != r.FinalMessages || r.MovedMessages > 0 ||
		r.MergedCallMessages > 0 || r.FoldedTextMessages > 0 ||
		r.DroppedCalls > 0 || r.DroppedOutputs > 0
}

// RepairToolSequence 就地修复工具消息序列，返回统计。
func RepairToolSequence(obj map[string]any) ToolRepairReport {
	messages, ok := obj["messages"].([]any)
	report := ToolRepairReport{OriginalMessages: len(messages), FinalMessages: len(messages)}
	if !ok || len(messages) == 0 {
		return report
	}
	// 拓扑签名必须在就地修改前抓取（过滤阶段会改写 assistant 的 tool_calls）
	topologyBefore := toolMessageTopology(messages)

	// 每个 ID 只保留第一条调用，以及位于该调用之后的第一条结果
	callMessageIndex := make(map[string]int)
	callEntryIndex := make(map[string]int)
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok || roleOf(message) != "assistant" {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for entryIndex, callAny := range calls {
			id := toolCallID(callAny)
			if id == "" {
				continue
			}
			if _, exists := callMessageIndex[id]; !exists {
				callMessageIndex[id] = messageIndex
				callEntryIndex[id] = entryIndex
			}
		}
	}

	outputMessageIndex := make(map[string]int)
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok || roleOf(message) != "tool" {
			continue
		}
		id := toolOutputID(message)
		callIndex, exists := callMessageIndex[id]
		if !exists || messageIndex <= callIndex {
			continue
		}
		if _, exists := outputMessageIndex[id]; !exists {
			outputMessageIndex[id] = messageIndex
		}
	}

	pairedIDs := make(map[string]bool, len(outputMessageIndex))
	for id := range outputMessageIndex {
		pairedIDs[id] = true
	}

	filtered := make([]any, 0, len(messages))
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok {
			filtered = append(filtered, messageAny)
			continue
		}
		switch roleOf(message) {
		case "assistant":
			calls, hasCalls := message["tool_calls"].([]any)
			if !hasCalls || len(calls) == 0 {
				filtered = append(filtered, messageAny)
				continue
			}
			kept := make([]any, 0, len(calls))
			for entryIndex, callAny := range calls {
				id := toolCallID(callAny)
				if id != "" && pairedIDs[id] &&
					callMessageIndex[id] == messageIndex && callEntryIndex[id] == entryIndex {
					kept = append(kept, callAny)
				} else {
					report.DroppedCalls++
				}
			}
			if len(kept) > 0 {
				message["tool_calls"] = kept
				filtered = append(filtered, messageAny)
				continue
			}
			delete(message, "tool_calls")
			if assistantMessageHasPayload(message) {
				filtered = append(filtered, messageAny)
			}

		case "tool":
			id := toolOutputID(message)
			if id != "" && pairedIDs[id] && outputMessageIndex[id] == messageIndex {
				filtered = append(filtered, messageAny)
			} else {
				report.DroppedOutputs++
			}

		default:
			filtered = append(filtered, messageAny)
		}
	}

	repaired := repairToolBatches(filtered, &report)
	obj["messages"] = repaired
	report.FinalMessages = len(repaired)
	if report.Changed() {
		report.TopologyBefore = topologyBefore
		report.TopologyAfter = toolMessageTopology(repaired)
	}
	return report
}

// toolMessageTopology 生成有界的消息拓扑签名：只有角色与工具调用 ID，**不含任何正文**。
// 这样日志里能核对工具序列结构，又不会泄露提示词或工具结果。
func toolMessageTopology(messages []any) string {
	const maxEntries = 200
	parts := make([]string, 0, maxEntries)
	for i, messageAny := range messages {
		if i >= maxEntries {
			parts = append(parts, "...")
			break
		}
		message, ok := messageAny.(map[string]any)
		if !ok {
			parts = append(parts, "?")
			continue
		}
		role := roleOf(message)
		if role == "assistant" {
			if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
				ids := make([]string, 0, len(calls))
				for _, callAny := range calls {
					ids = append(ids, toolCallID(callAny))
				}
				parts = append(parts, "assistant["+strings.Join(ids, ",")+"]")
				continue
			}
		}
		if role == "tool" {
			parts = append(parts, "tool["+toolOutputID(message)+"]")
			continue
		}
		parts = append(parts, role)
	}
	return strings.Join(parts, " > ")
}

// repairToolBatches 把一组工具调用收拢成上游要求的形状：组头 assistant 声明全部调用，
// 紧跟其全部结果，插入物一律后移。
//
//	assistant[c00] > assistant("正文") > assistant[c01] > X > tool c00 > tool c01
//	→ assistant("正文"+[c00,c01]) > tool c00 > tool c01 > X
//
// 形态一（背靠背的单调用 assistant）合并成并行批次；形态二（纯正文 assistant 紧跟
// 只有调用没有正文的 assistant）把正文折进上一条，这样「声明了 tool_calls 的 assistant
// 紧跟它自己的结果」在两种拆分顺序下都成立。两种合并条件都从严，避免引入新语义：
//   - 形态一只认「本条 content 为空」且自身没带会丢失的额外字段；
//   - 形态二只认**字符串正文**（数组正文可能含多模态块，拼接会丢结构，原样交给后移处理），
//     且上一条必须**自身无正文**。
func repairToolBatches(messages []any, report *ToolRepairReport) []any {
	out := make([]any, 0, len(messages))
	for i := 0; i < len(messages); {
		head, headCalls, ok := assistantCalls(messages[i])
		if !ok {
			out = append(out, messages[i])
			i++
			continue
		}

		batchCalls := append([]any(nil), headCalls...)
		// last 是批次里最后一条 assistant 消息：形态二的正文折到它身上，
		// 合并进来的 reasoning 痕迹也归它。
		last := head
		deferred := make([]any, 0)
		mergedMessages := 0
		foldedText := 0
		j := i + 1
		for j < len(messages) {
			if next, calls, hasCalls := assistantCalls(messages[j]); hasCalls {
				if !mergeableCallMessage(next) {
					break // 本条带正文或额外字段：不能并入批次，就此收束
				}
				batchCalls = append(batchCalls, calls...)
				mergeReasoningTrace(last, next)
				last = next
				mergedMessages++
				j++
				continue
			}
			if message, ok := messages[j].(map[string]any); ok && roleOf(message) == "assistant" {
				// 形态二：纯正文 assistant 折进上一条「只有调用没有正文」的 assistant。
				// 折完这一条就不再有正文，后续的正文 assistant 只能后移（条件不重复满足）。
				if txt, ok := message["content"].(string); ok && txt != "" &&
					mergeableTextMessage(message) && !assistantMessageHasPayload(last) {
					last["content"] = txt
					mergeReasoningTrace(last, message)
					foldedText++
					j++
					continue
				}
			}
			if message, ok := messages[j].(map[string]any); ok && roleOf(message) == "tool" {
				break
			}
			deferred = append(deferred, messages[j])
			j++
		}

		ids := make(map[string]bool, len(batchCalls))
		for _, callAny := range batchCalls {
			if id := toolCallID(callAny); id != "" {
				ids[id] = true
			}
		}
		outputs := make([]any, 0, len(ids))
		found := make(map[string]bool, len(ids))
		k := j
		for k < len(messages) && len(found) < len(ids) {
			if _, _, nextBatch := assistantCalls(messages[k]); nextBatch {
				break
			}
			if message, ok := messages[k].(map[string]any); ok && roleOf(message) == "tool" {
				id := toolOutputID(message)
				if !ids[id] {
					// 属于**另一批**调用的结果：它是后继组的开头，不是本组的插入物。
					// 当插入物吞掉会把后移错位（把别人的结果排到自己前面），必须止步。
					break
				}
				if !found[id] {
					outputs = append(outputs, messages[k])
					found[id] = true
				}
				k++
				continue
			}
			deferred = append(deferred, messages[k])
			k++
		}

		if len(found) != len(ids) {
			// 结果不齐：保持原样，交给前面的对称裁剪处理，不在这里二次改写
			out = append(out, messages[i])
			i++
			continue
		}
		if mergedMessages == 0 && foldedText == 0 && len(deferred) == 0 {
			// 没有可归一的东西（声明后紧跟自己那批结果，顺序也对）：
			// 原样逐个输出，零改动 —— 不顺手重排正常会话。
			out = append(out, messages[i])
			i++
			continue
		}

		head["tool_calls"] = batchCalls
		out = append(out, head)
		out = append(out, outputs...)
		out = append(out, deferred...)
		if mergedMessages > 0 || foldedText > 0 {
			report.ParallelBatches++
		}
		report.MovedMessages += len(deferred)
		report.MergedCallMessages += mergedMessages
		report.FoldedTextMessages += foldedText
		i = k
	}
	return out
}

func assistantCalls(messageAny any) (map[string]any, []any, bool) {
	message, ok := messageAny.(map[string]any)
	if !ok || roleOf(message) != "assistant" {
		return nil, nil, false
	}
	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		return nil, nil, false
	}
	return message, calls, true
}

// mergeableCallMessage 判定「可以并入并行批次」的 assistant 消息：没有正文，
// 且只带 role/content/tool_calls 与两类思维链痕迹字段 —— 其余字段（name 等）在合并时
// 会被丢弃，宁可不合并也不静默丢信息。
//
// 空数组正文（`content: []`）也算无正文：有客户端把「没有正文」发成空数组而不是 null，
// 此前按「非字符串一律非空」处理 → 背靠背的两条 assistant(tool_calls) 不合并 →
// 正是形态一要挡的那类 11148。
func mergeableCallMessage(message map[string]any) bool {
	if assistantMessageHasPayload(message) {
		return false
	}
	return hasOnlyAssistantKeys(message)
}

// mergeableTextMessage 判定「可以折进上一条」的纯正文 assistant：同样限制字段集合。
func mergeableTextMessage(message map[string]any) bool {
	if _, hasCalls := message["tool_calls"]; hasCalls {
		return false
	}
	return hasOnlyAssistantKeys(message)
}

// allowedAssistantKeys 是合并/折叠允许出现（且能无损搬运）的字段。tool_calls 由调用方
// 单独搬运；content 是正文本身；reasoning 痕迹见 mergeReasoningTrace。
var allowedAssistantKeys = map[string]bool{
	"role": true, "content": true, "tool_calls": true,
	"reasoning_content": true, "reasoning": true,
}

func hasOnlyAssistantKeys(message map[string]any) bool {
	for key := range message {
		if !allowedAssistantKeys[key] {
			return false
		}
	}
	return true
}

// mergeReasoningTrace 把 src 的思维链痕迹并入 dst（两边都有则换行拼接）。
// deepseek 多轮要求 assistant 带思维链回填，合并时丢弃会换一个错误。
// reasoning_content 与 reasoning 两个字段名都要搬：thinking.go 的回填会在两者之间
// 镜像，只搬其一仍可能丢另一形态的原文。
func mergeReasoningTrace(dst, src map[string]any) {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		text, _ := src[key].(string)
		if text == "" {
			continue
		}
		if prev, _ := dst[key].(string); prev != "" {
			dst[key] = prev + "\n" + text
			continue
		}
		dst[key] = text
	}
}

func assistantMessageHasPayload(message map[string]any) bool {
	content, exists := message["content"]
	if !exists || content == nil {
		return false
	}
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) > 0
	default:
		return true
	}
}

func toolCallID(callAny any) string {
	call, ok := callAny.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := call["id"].(string)
	return strings.TrimSpace(id)
}

func toolOutputID(message map[string]any) string {
	id, _ := message["tool_call_id"].(string)
	return strings.TrimSpace(id)
}
