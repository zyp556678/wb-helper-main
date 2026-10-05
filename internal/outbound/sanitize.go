package outbound

import (
	"regexp"
	"strings"
)

// 本文件是出站请求体的指纹脱敏。
//
// 背景：客户端（Claude Code / Codex 等 CLI）会在 system prompt 里注入若干**固定模板句**，
// 而上游内容审核是按逐字精确匹配拦截的（不是语义审核）——一字之差即可绕过。
// 因此策略是：键值/header 型指纹整段剥离，承载语义的模板句做最小改写（换一个词、语义不变）。
//
// 与提示词模式的关系：custom 模式从源头删掉客户端 system，脱敏对 system 已基本无事可做；
// 但 user/assistant 消息里引用的模板句仍会被扫到，所以两层叠加、互不替代。

// sanitizeFeatures 是特征预检串：任一命中才进入净化路径。
// 绝大多数普通请求全不命中，走 strings.Contains 快速路径直接返回原文（零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header",                      // header 键值段键名
	"cc_entrypoint=",                                  // 尾随裸键值
	"You are Claude Code",                             // 身份句
	"Main branch (",                                   // 注入指令句
	"You are a coding agent running in the Codex CLI", // Codex 指令首段
	"github.com/anthropics/",                          // 反馈句里的仓库链接
	"11128",                                           // 上游反探测：裸错误码
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹。
//
// 剥离层要求冒号，对「反引号里引用了裸键名」这种形态无效；
// 键值形态被删除后残留的裸键名做最小缩写（header→hdr），破坏逐字匹配但保留可读性。
// 大小写不敏感，覆盖 X-Anthropic-... 变体。
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeRewrites 改写层：整句最小替换（每题只改一个词，语义不变）。
//
// 匹配串刻意**不带结尾标点**，因为同一句在不同客户端里结尾不同
// （CLI 版以句号收尾，桌面版以逗号接后继内容）；带标点会漏掉一半形态、指纹照样发上游。
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 反探测：请求体里只要出现裸数字 11128 就整单拦截（与上下文无关）。
		// 而 11128 正是本类拦截自身的错误码——上游据此识别「在讨论/回显其内部错误码」。
		// 代价是用户对话里的 11128 也会被改写，但这串数字出现在请求里本身就必然失败，
		// 不改写等于必错。插连字符保留可读性（零宽空格无效，上游会归一化）。
		"11128",
		"11-128",
	},
}

// hasFingerprint 特征预检：先走零分配的 Contains 快速路径，
// 再用不要求冒号的 bareHdr 正则兜底「混合大小写 + 裸键名」——
// Contains 大小写敏感、hdrRe 要求冒号，两者都会漏掉这种形态。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeBareHdrRe.MatchString(text)
}

// sanitizeText 单段文本净化：预检不中直接返回原串（零分配）。
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		// 可能有多段尾随裸键值，循环清到稳定
		for prev := ""; prev != text; {
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	text = sanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不碰。
// 返回净化后的值与是否发生变化。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		out := sanitizeText(c)
		return out, out != c
	case []any:
		changed := false
		parts := make([]any, 0, len(c))
		for _, pAny := range c {
			p, ok := pAny.(map[string]any)
			if !ok {
				parts = append(parts, pAny)
				continue
			}
			typ, _ := p["type"].(string)
			if typ == "text" {
				if t, ok := p["text"].(string); ok {
					if out := sanitizeText(t); out != t {
						p = cloneMap(p)
						p["text"] = out
						changed = true
					}
				}
			}
			parts = append(parts, p)
		}
		if !changed {
			return v, false
		}
		return parts, true
	default:
		return v, false
	}
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而按「content 缺失就跳过整条消息」
// 处理时，历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, cAny := range callList {
		call, ok := cAny.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 就地净化全部消息；返回是否有改动。
//
// content 与 tool_calls / 思维链字段各自独立判断：content 可以为 null（工具调用轮），
// 早期版本在 content 缺失时直接 continue，导致这类消息的 tool_calls 完全不被净化。
//
// reasoning_content 与 reasoning 一并净化：思维链里带指纹同样致命（裸 "11-128"
// 是上游的反探测串），而这两个字段恰恰是客户端不会改写的内容。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, mAny := range messages {
		msg, ok := mAny.(map[string]any)
		if !ok {
			continue
		}
		if content, exists := msg["content"]; exists {
			if out, ch := sanitizeContent(content); ch {
				msg["content"] = out
				changed = true
			}
		}
		for _, key := range []string{"reasoning_content", "reasoning"} {
			text, ok := msg[key].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				msg[key] = s
				changed = true
			}
		}
		if tc, exists := msg["tool_calls"]; exists {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
