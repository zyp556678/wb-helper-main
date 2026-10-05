package outbound

import (
	"strings"
)

// 本文件是 DeepSeek 系模型的思维链处理，三件事：
//  1. 开思考开关：出站必须显式带 thinking:{type:"enabled"} **且**带一档 reasoning_effort，
//     否则上游按「不思考」应答（思维链不返回）。只带开关不带档位同样无效（实测）。
//  2. 档位降级：请求带的档位模型不支持时，落到「≤ 请求档的最高支持档」，
//     保证出站永远是模型认的档位，不因为客户端给了个陌生值就 400。
//  3. reasoning_content 回填：上游要求带 thinking 的会话里，每条 assistant 消息都要有
//     reasoning_content 字段（缺失会 400）。第三方客户端普遍不传，所以在出站补齐。
//
// 非 deepseek 模型一律零改动——glm / kimi / qwen 走的是另一套 thinkingFormat。

// defaultDeepSeekEffort 是无从获知模型默认档时的兜底档位。
const defaultDeepSeekEffort = "high"

// effortRank 档位从低到高（用于降级排序）。
var effortRank = map[string]int{
	"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// isDeepSeekModel 以 deepseek 前缀判定（不区分大小写），
// 覆盖 deepseek-v4.1-flash / deepseek-v4-pro / deepseek-r1 等变体。
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectThinking 按「开关 + 档位」的组合规则改写请求体。
//
// 分支口径：
//   - thinking.type 已是 disabled → 尊重客户端意图，并删掉 effort 双字段（关思考不该带档位）。
//   - thinking.type 已是 enabled → 只补缺失的档位，不动 type。
//   - 无 thinking / type 为空 → 注入 enabled 并补档位（有 effort 则保留不覆盖）。
//
// defaultEffort 来自目录里该模型的 reasoning 默认档；空串时回退硬编码 high。
func injectThinking(obj map[string]any, defaultEffort string) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	th, hasThinking := obj["thinking"].(map[string]any)
	typ := ""
	if hasThinking {
		typ, _ = th["type"].(string)
		typ = strings.TrimSpace(typ)
	}

	if typ != "" {
		if strings.EqualFold(typ, "disabled") {
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
			return
		}
		ensureDeepSeekEffort(obj, defaultEffort)
		return
	}

	if !hasThinking {
		obj["thinking"] = map[string]any{"type": "enabled"}
	} else {
		th["type"] = "enabled"
	}
	ensureDeepSeekEffort(obj, defaultEffort)
}

// ensureDeepSeekEffort 缺档位时补默认档（snake 优先，camel 兜底）。
// 客户端已显式给了任一形态的档位 → 不覆盖（显式档位不该被改写，降级交给 normalizeReasoningEffort）。
func ensureDeepSeekEffort(obj map[string]any, defaultEffort string) {
	if _, ok := obj["reasoning_effort"]; ok {
		return
	}
	if _, ok := obj["reasoningEffort"]; ok {
		return
	}
	if strings.TrimSpace(defaultEffort) == "" {
		defaultEffort = defaultDeepSeekEffort
	}
	obj["reasoning_effort"] = defaultEffort
}

// normalizeReasoningEffort 按模型支持档位降级请求里的 reasoning_effort（snake/camel 双兼容）。
//
//   - 请求档位模型支持 → 原样透传
//   - 不支持           → 取 ≤ 请求档的最高支持档
//   - 支持档全都高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型 / 未知档位 / 未携带 / 模型未缓存 → 一律透传
//
// 返回是否发生了改写（用于日志）。
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) (from, to string, changed bool) {
	if len(efforts) == 0 {
		return "", "", false
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return "", "", false
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return "", "", false
	}

	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return "", "", false
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return "", "", false
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return "", "", false
	}

	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if strings.EqualFold(best, reqStr) {
			return "", "", false
		}
		obj[key] = best
		return reqStr, best, true
	}

	// 支持档全部高于请求档：取最低支持档
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest == "" {
		return "", "", false
	}
	obj[key] = lowest
	return reqStr, lowest, true
}

// backfillReasoningContent 保证带 thinking 的会话里每条 assistant 消息都有
// reasoning_content 字段（上游对缺失的会判 400）。
//
// 门控：thinkingEnabled（注入后的 thinking.type == enabled）或 hasTrace（会话里已有推理痕迹）。
// 两者都不成立就不动它——客户端明确关了思考时不该硬塞字段。
//
// 字段处理（对齐「只认 string」的语义，null / 数字都不算「已有」）：
//   - reasoning_content 已是非空 string → 原样保留
//   - 有 reasoning 字符串 → 复制进 reasoning_content
//   - 都没有 → 补空串（与上游「字段存在即可」的校验口径一致）
//
// 另做一步镜像：reasoning 缺失/空时用 reasoning_content 补上（都空则补单个空格）。
// 原因是有部分账号/租户对 thinking 形态校验「len(reasoning) > 0」，
// 空串 400、空白串 200；而这个字段是透传校验位、不参与内容消费，占位无副作用。
func backfillReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}

	thinkingEnabled := false
	if th, ok := obj["thinking"].(map[string]any); ok {
		if typ, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "enabled") {
			thinkingEnabled = true
		}
	}
	hasTrace := false
	for _, mAny := range msgs {
		msg, ok := mAny.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !thinkingEnabled && !hasTrace {
		return
	}

	for _, mAny := range msgs {
		msg, ok := mAny.(map[string]any)
		if !ok || roleOf(msg) != "assistant" {
			continue
		}
		rc, hasRC := msg["reasoning_content"].(string)
		if !hasRC {
			if r, ok := msg["reasoning"].(string); ok {
				rc = r
			} else {
				rc = ""
			}
			msg["reasoning_content"] = rc
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			continue
		}
		if rc != "" {
			msg["reasoning"] = rc
		} else {
			msg["reasoning"] = " "
		}
	}
}
