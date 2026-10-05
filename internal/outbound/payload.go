package outbound

import (
	"encoding/json"
	"strings"
)

// 本文件是出站请求体改写管线的唯一入口。
//
// 为什么集中在一个函数里：这些改写彼此有**顺序依赖**，散落在各处极易出现
// 「改了 A 但 B 又把它还原」的隐蔽 bug。顺序与理由：
//
//  1. 提示词模式（custom 删全部 system / append 插一条网关 system）
//     —— 放在最前，让它决定「system 侧长什么样」，后续步骤都在此基础上工作。
//  2. 首条保底 system —— append 模式下若原本首条不是 system，插入后已满足；
//     passthrough 模式下靠这一步兜住上游「首条必须是 system」的硬校验。
//  3. 强制 stream:true —— 上游不接受非流式。
//  4. stream_options 兜底 include_usage —— 不带它上游末帧不给 usage，指标就没数据。
//  5. max_completion_tokens → max_tokens —— 上游只认后者，别名字段会被忽略后回落默认上限。
//  6. tool_choice / role / image_url 归一 —— 都是上游参数校验的白名单/形态问题。
//  7. 工具序列自愈 —— 必须在 role 归一之后：developer→system 会影响消息拓扑判定。
//  8. 思维链开关与档位降级 —— 注入的默认档要经过降级管线，所以注入在降级之前。
//  9. reasoning_content 回填 —— 依赖第 8 步注入后的 thinking 状态。
//  10. 指纹脱敏 —— 放最后：前面所有步骤写入的内容都在此统一过一遍。
type Options struct {
	// PromptMode 见 prompt.go 的三个常量；空串等价于 passthrough。
	PromptMode string
	// SystemPrompt 是本次实际使用的网关提示词（由 ResolveSystemPrompt 解析）。
	SystemPrompt string
	// Sanitize 控制指纹脱敏开关。
	Sanitize bool
	// StrictFirstSystem 控制首条消息保底 system。
	StrictFirstSystem bool
	// SupportedEfforts / DefaultEfforts 来自模型目录，用于档位降级与补默认档。
	SupportedEfforts map[string][]string
	DefaultEfforts   map[string]string
	// Degrade 为真时强制使用降级中性提示词（拦截后重试用）。
	Degrade bool
}

// Result 汇总本次改写的实际动作，供日志与面板展示。
type Result struct {
	Body []byte `json:"-"`
	// PromptMode 是实际生效的模式（重试时可能被降级覆盖）。
	PromptMode string `json:"prompt_mode"`
	// PromptApplied 表示本次是否真的改写了 messages 里的 system。
	PromptApplied bool `json:"prompt_applied"`
	// DroppedSystemMessages 记录 custom 模式删掉的 system/developer 数量。
	DroppedSystemMessages int `json:"dropped_system_messages"`
	// SystemInjected 表示是否因首条不是 system 而补了保底 system。
	SystemInjected bool `json:"system_injected"`
	// Sanitized 表示是否真的改动了内容（命中指纹）。
	Sanitized bool `json:"sanitized"`
	// ToolRepair 是工具序列自愈的统计。
	ToolRepair ToolRepairReport `json:"tool_repair"`
	// ToolPatternsFixed 是被修掉的非标正则转义处数（`pattern` 值 + `patternProperties` 键）。
	// 非零表示本次请求**本来会被上游整体拒收**（400 code=11129）。
	ToolPatternsFixed int `json:"tool_patterns_fixed"`
	// ThinkingInjected 表示是否注入了思考开关。
	ThinkingInjected bool `json:"thinking_injected"`
	// EffortFrom / EffortTo 非空表示发生了档位降级。
	EffortFrom string `json:"effort_from,omitempty"`
	EffortTo   string `json:"effort_to,omitempty"`
	// ReasoningBackfilled 表示是否回填过 reasoning_content。
	ReasoningBackfilled bool `json:"reasoning_backfilled"`
	// MaxTokensFrom / MaxTokensTo 记录 GPT 系 max_tokens 被抬到下限的动作
	// （From 为 0 且 Clamped 为真表示原本是 0/缺失但被判为需抬升）。
	MaxTokensClamped bool  `json:"max_tokens_clamped"`
	MaxTokensFrom    int64 `json:"max_tokens_from,omitempty"`
	MaxTokensTo      int64 `json:"max_tokens_to,omitempty"`
}

// Prepare 执行完整出站改写。body 解析失败时原样返回（绝不失败）——
// 这是转发关键路径，坏 body 应该让上游去报真实的参数错误，而不是我们先造一个。
func Prepare(body []byte, opts Options) Result {
	res := Result{PromptMode: normalizeMode(opts.PromptMode)}
	if len(body) == 0 {
		res.Body = body
		return res
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		res.Body = body
		return res
	}

	prompt := opts.SystemPrompt
	if opts.Degrade {
		// 降级重试：无论配置成哪种模式，都换成中性提示词并走 custom 语义
		// （把 system 侧的指纹面收到最小，这才是重试的意义）。
		prompt = DegradedSystemPrompt
		res.PromptMode = PromptCustom
	}

	// 1. 提示词模式
	switch res.PromptMode {
	case PromptCustom:
		if prompt != "" {
			before := len(systemMessages(obj))
			if out := RewriteMessages(mustJSON(obj), prompt); out != nil {
				if obj2, ok := decode(out); ok {
					obj = obj2
					res.DroppedSystemMessages = before
					res.PromptApplied = true
				}
			}
		}
	case PromptAppend:
		if prompt != "" {
			if out := AppendMessages(mustJSON(obj), prompt); out != nil {
				if obj2, ok := decode(out); ok {
					obj = obj2
					res.PromptApplied = true
				}
			}
		}
	}

	// 2. 首条保底 system
	if opts.StrictFirstSystem && ensureLeadingSystem(obj) {
		res.SystemInjected = true
	}

	// 3. 上游强制 stream
	obj["stream"] = true

	// 4. stream_options 兜底
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}

	// 5. max_completion_tokens → max_tokens
	translateMaxCompletionTokens(obj)
	// 5b. GPT 系 max_tokens 下限（抬到 16）：上游对 GPT 系小于 16 的值一律 400
	// code=11133，而这是模型参数级拒绝 —— 换账号同样被拒，必须在发送前修。
	if from, clamped := clampGPTMinMaxTokens(obj); clamped {
		res.MaxTokensClamped = true
		res.MaxTokensFrom = from
		res.MaxTokensTo = gptMinMaxTokens
	}

	// 6. 参数形态归一
	normalizeToolChoice(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	// 工具 schema 里的非标转义（`\_`）会让上游**整体拒收**（400 code=11129），
	// 而这是 schema 级错误：换账号无用，会喂连败计数 → 打挂整池路由权重。
	// 独立于指纹脱敏开关 —— 这是「让请求通过」，不是脱敏。
	res.ToolPatternsFixed = normalizeToolPatterns(obj)

	// 7. 工具序列自愈
	res.ToolRepair = RepairToolSequence(obj)

	// 8. 思维链开关 + 档位降级
	model, _ := obj["model"].(string)
	if isDeepSeekModel(model) {
		injectThinking(obj, strings.TrimSpace(opts.DefaultEfforts[model]))
		res.ThinkingInjected = hasThinkingEnabled(obj)
	}
	if from, to, changed := normalizeReasoningEffort(obj, opts.SupportedEfforts); changed {
		res.EffortFrom, res.EffortTo = from, to
	}

	// 9. reasoning_content 回填
	if isDeepSeekModel(model) {
		res.ReasoningBackfilled = backfillReasoningMessages(obj)
	}

	// 10. 指纹脱敏
	if opts.Sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			res.Sanitized = sanitizeMessages(msgs)
		}
	}

	out, err := json.Marshal(obj)
	if err != nil {
		res.Body = body
		return res
	}
	res.Body = out
	return res
}

func normalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case PromptCustom:
		return PromptCustom
	case PromptAppend:
		return PromptAppend
	default:
		return PromptPassthrough
	}
}

func mustJSON(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return out
}

func decode(b []byte) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

func systemMessages(obj map[string]any) []any {
	msgs, _ := obj["messages"].([]any)
	out := make([]any, 0)
	for _, mAny := range msgs {
		if m, ok := mAny.(map[string]any); ok && isSystemRole(roleOf(m)) {
			out = append(out, mAny)
		}
	}
	return out
}

// ensureLeadingSystem 在首条消息不是 system/developer 时补一条保底 system。
// 返回是否补了。部分客户端以 assistant / tool 续写首条消息，会触发上游参数校验失败。
func ensureLeadingSystem(obj map[string]any) bool {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	if first, ok := msgs[0].(map[string]any); ok {
		if isSystemRole(roleOf(first)) {
			return false
		}
	}
	obj["messages"] = append([]any{
		map[string]any{"role": "system", "content": DegradedSystemPrompt},
	}, msgs...)
	return true
}

func hasThinkingEnabled(obj map[string]any) bool {
	th, ok := obj["thinking"].(map[string]any)
	if !ok {
		return false
	}
	typ, _ := th["type"].(string)
	return strings.EqualFold(strings.TrimSpace(typ), "enabled")
}

// backfillReasoningMessages 是 backfillReasoningContent 的「是否发生改动」包装。
func backfillReasoningMessages(obj map[string]any) bool {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return false
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		return false
	}
	changed := false
	for _, mAny := range msgs {
		m, ok := mAny.(map[string]any)
		if !ok || roleOf(m) != "assistant" {
			continue
		}
		if _, ok := m["reasoning_content"].(string); !ok {
			changed = true
		} else if r, ok := m["reasoning"].(string); !ok || r == "" {
			changed = true
		}
	}
	backfillReasoningContent(obj)
	return changed
}

// -----------------------------------------------------------------------------
// 参数形态归一
// -----------------------------------------------------------------------------

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游认的 max_tokens。
//   - 显式 max_tokens 优先，别名只删不译；
//   - 别名非正数（0/null/负数）不翻译——0/null 语义是「未设置」，负数非法；
//   - 非数值（字符串等畸形）不翻译，原样交给上游报参数错。
//
// 别名无论是否翻译一律删除：减少 body 体积与排障噪音。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens")
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return
	}
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}

// gptMinMaxTokens 是 GPT 系上游接受的 max_tokens 下限。
const gptMinMaxTokens = 16

// clampGPTMinMaxTokens 把 GPT 系模型过小的 max_tokens 抬到下限。
//
// 背景：上游 GPT 系（实测 gpt-6-sol / gpt-6-luna / gpt-5.6-sol）对 max_tokens < 16
// 一律 400 code=11133 model_param_invalid（15 拒、16 过，同号同 body 对照）；hy4 等
// 非 GPT 模型无此限制。Claude Code 切模型时会发 max_tokens 极小的探针，全号轮转同样
// 被拒 → 客户端 503，模型永远切不过去。账号与 body 其余部分无关，换号无用，只能
// 在发送前修。抬到下限只放宽输出上限、不改语义；未携带字段 / 非数值一律不动。
//
// 返回 (原值, 是否抬升)。模型名判定用 "gpt-" 子串（与上游族名口径一致，
// 大小写不敏感）：宁宽勿漏 —— 抬一个非 GPT 模型的极小上限只放宽了输出，
// 漏掉一个 GPT 模型则是必错。
func clampGPTMinMaxTokens(obj map[string]any) (int64, bool) {
	model, _ := obj["model"].(string)
	if !strings.Contains(strings.ToLower(model), "gpt-") {
		return 0, false
	}
	var v int64
	switch n := obj["max_tokens"].(type) {
	case float64:
		v = int64(n)
	case int64:
		v = n
	case int:
		v = int64(n)
	default:
		return 0, false
	}
	if v >= gptMinMaxTokens {
		return v, false
	}
	obj["max_tokens"] = int64(gptMinMaxTokens)
	return v, true
}

// normalizeToolChoice 按上游「tool_choice 是字符串」的实际形态改写 OpenAI 的对象写法。
//   - "none" / {"type":"none"} → 删 tool_choice 并同时删 tools/functions
//   - {"type":"auto"|"required"} → 字符串
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象 / 非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ := strings.ToLower(strings.TrimSpace(str(v["type"])))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeRoles 把 developer 角色归一为 system。
//
// 上游对 role 做白名单校验，developer 不在白名单内（命中即 400）。
// developer 是 OpenAI 新规范里 system 的别名（Codex / Cursor 等新客户端用它承载
// system 级指令），改写为 system 不丢语义。
//
// 只认 developer 这一个值：其余 role 一律原样保留，不合并不重排不删除
// （上游对多 system 的行为未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, mAny := range msgs {
		msg, ok := mAny.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
		}
	}
}

// normalizeImageURL 兼容 image_url 的字符串形态。
//
// OpenAI 规范用对象形态 {"url":"..."}，但部分客户端与 Responses 转换器会发字符串。
// 上游只接受对象形态，字符串会直接参数错误。这里只做形状转换：
// 已有对象及其字段原样保留；空串/缺失/对象内非法 url 一律不补默认值，让上游报真实错误。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			if imageURL, ok := part["image_url"].(string); ok && imageURL != "" {
				part["image_url"] = map[string]any{"url": imageURL}
			}
		}
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
