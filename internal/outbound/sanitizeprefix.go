package outbound

import "strings"

// 本文件是 system 消息**开头**的提示词级渠道校验规避（对照 wb-gateway v1.14.21）。
//
// 与 sanitize.go 的分工先说清，两者互不替代：
//
//	sanitize.go           子串级：指纹出现在正文任何位置都处理（整句改写 / 整段剥离）。
//	sanitizeprefix.go     偏移 0 级：只认 system 消息的**第一个字符**，
//	                      因为上游这条渠道校验是按「开头前缀」匹配的。
//
// 为什么不能直接塞进 sanitizeText：那条路会扫全文，把用户正常讨论这句身份
// （比如「帮我看看这句 system 提示词为什么被拦」）也一起改掉；而这里上游只
// 看开头，正文中段改了纯粹是误伤。
//
// 三条边界都是刻意的：
//   - 只在偏移 0 匹配，前导空白不跳过（上游也不跳过）；
//   - 匹配忽略大小写，替换后从匹配长度之后**逐字保留**，不做二次 StripSpace
//     （那会吃掉客户端原文的首尾空白，属于改内容，不属于改前缀）；
//   - 数组形态只处理**第一个文本块**：后续块的位置不在开头，上游不看。
var blockedSystemPromptPrefixes = []string{
	"You are an AI agent powered by DeepSeek Harness",
}

// blockedSystemPromptPrefixReplacement 是等义改写的目标：
// 仍是一句中性的身份陈述，语义不变，只是不再逐字命中上游的特征串。
const blockedSystemPromptPrefixReplacement = "You are an AI agent powered by a large language model"

// sanitizeSystemPromptPrefix 返回改写后的文本与是否命中。
func sanitizeSystemPromptPrefix(text string) (string, bool) {
	for _, prefix := range blockedSystemPromptPrefixes {
		if len(text) < len(prefix) || !strings.EqualFold(text[:len(prefix)], prefix) {
			continue
		}
		return blockedSystemPromptPrefixReplacement + text[len(prefix):], true
	}
	return text, false
}

// sanitizeSystemMessageContent 处理 string 与内容块数组两种 content 形态。
//
// 数组形态只改写**第一个文本块**：它就是提示词的起始位置。图片等非文本块
// 出现在它前面时直接跳过——那些块不承载文本前缀，改写它们没有意义，
// 而顺序与内容一律不动（数组里的其它块逐字保留）。
func sanitizeSystemMessageContent(content any) (any, bool) {
	switch value := content.(type) {
	case string:
		return sanitizeSystemPromptPrefix(value)
	case []any:
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			// 第一个文本块判完就返回：它后面没有「开头」可言。
			if rewritten, changed := sanitizeSystemPromptPrefix(text); changed {
				part["text"] = rewritten
				return value, true
			}
			return value, false
		}
	}
	return content, false
}

// sanitizeSystemPrefixes 就地改写全部 system/developer 消息的开头前缀，
// 返回被改写的消息条数。
//
// 与指纹脱敏不同，这一步**不看 prompt.sanitize 开关**：它属于「让请求能通过」
// 而不是「脱敏」（同 normalizeToolPatterns 的定位）。关掉脱敏的用户依然需要它，
// 否则请求会在上游被整单拦下，而面板上只显示一个错误码。
func sanitizeSystemPrefixes(messages []any) int {
	modified := 0
	for _, mAny := range messages {
		msg, ok := mAny.(map[string]any)
		if !ok {
			continue
		}
		// developer 一并处理：role 归一发生在管线的后面，
		// 走到这里时 developer 还是 developer，而上游对两者同样校验。
		if !isSystemRole(roleOf(msg)) {
			continue
		}
		content, exists := msg["content"]
		if !exists {
			continue
		}
		if rewritten, changed := sanitizeSystemMessageContent(content); changed {
			msg["content"] = rewritten
			modified++
		}
	}
	return modified
}
