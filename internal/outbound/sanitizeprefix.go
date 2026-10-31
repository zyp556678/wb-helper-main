package outbound

// 本文件是消息**开头**的提示词级渠道校验规避（对照 wb-gateway v1.14.21 / v1.14.24 /
// v1.14.29）。身份词表与替换目标见 identity.go。
//
// 与 sanitize.go 的分工先说清，两者互不替代：
//
//	sanitize.go           子串级：指纹出现在正文任何位置都处理（整句改写 / 整段剥离）。
//	sanitizeprefix.go     偏移 0 级：只认每个文本单元的**第一个字符**，
//	                      因为上游这条渠道校验是按「开头前缀」匹配的。
//
// 为什么不能直接塞进 sanitizeText：那条路会扫全文，把用户正常讨论这句身份
// （比如「帮我看看这句 system 提示词为什么被拦」）也一起改掉；而这里上游只
// 看开头，正文中段改了纯粹是误伤。
//
// 三条边界都是刻意的：
//   - 前缀只在偏移 0 匹配，前导空白不跳过（上游也不跳过）；
//   - 匹配忽略大小写，替换后从匹配长度之后**逐字保留**，不做二次 StripSpace
//     （那会吃掉客户端原文的首尾空白，属于改内容，不属于改前缀）；
//   - 覆盖 system / developer 与 **assistant 历史**的每一个文本单元：Messages
//     会把多个 system 文本块合并，合并后每个块的开头都可能成为整段开头；
//     assistant 历史则是多轮对话原样带回的上一轮回复。user / tool 不参与。

// sanitizeMessageContent 处理 string 与内容块数组两种 content 形态，
// 返回改写后的 content 与命中处数。
//
// 数组形态处理**每一个** type=="text" 的块（对照 wb-gateway v1.14.24）：
// 早先只处理第一个文本块，理由是「后续块不在开头」。但 Anthropic Messages
// 会把多个 system 文本块合并，合并后**每个块的开头都可能变成整段的开头**；
// 而 assistant 历史里的后续块本身就是独立的助手消息。两者都可能携带同一句
// 已知身份前缀，只认第一个块会漏。图片、工具等非文本块一律不碰，顺序与内容不动。
func sanitizeMessageContent(content any, s IdentitySettings) (any, int) {
	switch value := content.(type) {
	case string:
		if rewritten, changed := normalizeIdentityText(value, s); changed {
			return rewritten, 1
		}
		return value, 0
	case []any:
		modified := 0
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if part["type"] != "text" {
				continue
			}
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			if rewritten, changed := normalizeIdentityText(text, s); changed {
				part["text"] = rewritten
				modified++
			}
		}
		return value, modified
	}
	return content, 0
}

// normalizeIdentityPrefixes 扫描命中 match 的那些消息的全部文本单元，
// 逐处做身份改写，返回改写处数。
func normalizeIdentityPrefixes(messages []any, match func(role string) bool, s IdentitySettings) int {
	modified := 0
	for _, mAny := range messages {
		msg, ok := mAny.(map[string]any)
		if !ok || !match(roleOf(msg)) {
			continue
		}
		content, exists := msg["content"]
		if !exists {
			continue
		}
		rewritten, n := sanitizeMessageContent(content, s)
		if n > 0 {
			msg["content"] = rewritten
			modified += n
		}
	}
	return modified
}

// sanitizeSystemPrefixes 就地改写全部 system/developer 消息里的身份前缀，
// 返回被改写的文本单元数。
//
// 与指纹脱敏不同，这一步**不看 prompt.sanitize 开关**：它属于「让请求能通过」
// 而不是「脱敏」（同 normalizeToolPatterns 的定位）。关掉脱敏的用户依然需要它，
// 否则请求会在上游被整单拦下，而面板上只显示一个错误码。
func sanitizeSystemPrefixes(messages []any, s IdentitySettings) int {
	// developer 一并处理：role 归一发生在管线的后面，
	// 走到这里时 developer 还是 developer，而上游对两者同样校验。
	return normalizeIdentityPrefixes(messages, isSystemRole, s)
}

// normalizeAssistantPrefixes 处理 assistant **历史**里的同一句身份前缀
// （对照 wb-gateway v1.14.24）。
//
// 为什么 assistant 也要处理：多轮会话把上一轮的助手回复原样带回上游，
// 其中就带着这句身份陈述。只清洗 system 的话，第二轮起的历史仍然命中渠道校验。
// user / tool 内容不参与 —— 用户正文里讨论这句话是正常内容，改了就是误伤。
func normalizeAssistantPrefixes(messages []any, s IdentitySettings) int {
	return normalizeIdentityPrefixes(messages, func(role string) bool {
		return role == "assistant"
	}, s)
}
