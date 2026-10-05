package outbound

import (
	"encoding/json"
	"strings"
)

// 提示词三种模式的语义（这是本项目里「行为忠实」与「少撞审核」之间唯一需要取舍的地方）：
//
//	passthrough 客户端 system 原样发上游。行为最忠实，但客户端模板句会被上游按逐字精确匹配
//	            判违规，误报概率最高。适合排障，不适合做默认。
//	append      客户端消息逐字不动，只在「开头连续 system/developer 块」之后插入一条网关提示词。
//	            客户端项目规范与工具约定完整保留（agent 行为不退化），同时整体提示词结构变了，
//	            大部分逐字匹配型误报被消掉。代价是指纹原文仍在请求里，撞到拦截需靠降级重试兜底。
//	custom      删除全部 system/developer，头部只放一条网关提示词。上游从源头看不到客户端指纹，
//	            误报率最低、行为最可预期。代价是客户端自己的系统提示词被丢掉，
//	            依赖项目规范与工具约定的 CLI 能力会下降。
const (
	PromptPassthrough = "passthrough"
	PromptAppend      = "append"
	PromptCustom      = "custom"
)

// DefaultSystemPrompt 是内置的网关自有提示词。
//
// 刻意写得中性且简短：它要在 custom 模式下独立承担全部 system 语义，
// 因此必须交代「用用户的语言回答、遵循用户指令、直接简洁」这类最低限度的行为约束；
// 同时不能包含任何可能被逐字匹配的模板句式。
const DefaultSystemPrompt = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// DegradedSystemPrompt 是拦截降级重试用的极简中性提示词。
//
// 触发场景：passthrough / append 模式下请求被上游内容策略拦截（HTTP 400 + 审核文案），
// 判定为指纹误报后，换这条最小提示词重试一次。它刻意比 DefaultSystemPrompt 更短，
// 目的就是「把 system 侧的指纹面收到最小」，不是为了改变用户指令的合法性语义。
const DegradedSystemPrompt = "You are a helpful assistant."

// RewriteMessages 执行 custom 模式：删除全部 system/developer，头部插入一条网关提示词。
//
// 解析失败原样返回（绝不失败）——这是出站关键路径，任何解析错误都不该阻塞转发，
// 让上游按原始语义去处理，报错也要报上游的真实错误。
func RewriteMessages(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		if isSystemRole(roleOf(mm)) {
			continue
		}
		kept = append(kept, m)
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": systemPrompt}}, kept...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// AppendMessages 执行 append 模式：在「开头连续 system/developer 块」之后插入一条网关提示词。
//
// 插入点判定：从 messages[0] 起向后，只要 role 是 system/developer 就继续，
// 遇到第一条其他角色（含非对象消息、缺 role 的消息）即停。块长为 0 就是插在最前面。
// 所有既有消息逐字不动——客户端项目规范、工具约定与网关提示词并用。
//
// 边界判定必须同时认 system 与 developer：role 归一（developer→system）发生在管线的
// normalizeRoles 阶段，本函数执行时开头块里的 developer 还是 developer。
// 插入的网关消息固定用 system 而非 developer——上游 role 白名单不含 developer。
func AppendMessages(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	insertAt := 0
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			break
		}
		if !isSystemRole(roleOf(mm)) {
			break
		}
		insertAt++
	}
	gw := map[string]any{"role": "system", "content": systemPrompt}
	rewritten := make([]any, 0, len(msgs)+1)
	rewritten = append(rewritten, msgs[:insertAt]...)
	rewritten = append(rewritten, gw)
	rewritten = append(rewritten, msgs[insertAt:]...)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// ResolveSystemPrompt 决定本次请求实际使用的网关提示词文本。
//
//	file 非空 → 用文件内容（读失败返回错误，调用方 fail fast：配置错了就该立刻暴露）；
//	text 非空 → 用 text；
//	都为空   → 用内置默认。
func ResolveSystemPrompt(mode, text, file string, readFile func(string) ([]byte, error)) (string, error) {
	if strings.TrimSpace(file) != "" {
		if readFile == nil {
			return DefaultSystemPrompt, nil
		}
		raw, err := readFile(file)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
	if strings.TrimSpace(text) != "" {
		return text, nil
	}
	return DefaultSystemPrompt, nil
}

func roleOf(m map[string]any) string {
	r, _ := m["role"].(string)
	return strings.ToLower(strings.TrimSpace(r))
}

func isSystemRole(role string) bool {
	return role == "system" || role == "developer"
}
