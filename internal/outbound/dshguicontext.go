package outbound

import "strings"

// 本文件是 DSH（DeepSeek Harness）GUI 环境说明的等义归一化（对照 wb-gateway v1.14.24）。
//
// 背景：上游对非官方客户端有通道校验，DSH 注入的 GUI 环境说明是其中一条可识别特征。
// 与 sanitizeprefix.go 的分工：
//
//	sanitizeprefix.go   只认**偏移 0** 的已知身份前缀句（"You are an AI agent powered by ..."）。
//	dshguicontext.go    认的是一句**出现在正文任意位置**的环境说明，且必须避开代码块 ——
//	                    用户在讨论这句话时不该被改写。
//
// 三条边界都是刻意的：
//   - 只改「已知两句开头」里的**第一句**中的一个短语（"the DeepSeek Harness Web GUI"
//     → "a Web GUI"）；第二句及前后文逐字保留，不依赖整段的版本或摘要。
//   - 围栏代码（``` / ~~~）、缩进代码（4 空格或 Tab 起行）里的内容一律不动。
//   - 行内反引号状态**跨行保留**：` 的分隔符状态属于整段文本，不是单行。

const (
	dshGUIContextSentence = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	dshGUIContextNeutral  = "You are interacting with the user through a Web GUI whose URL the runtime context names."
	// dshGUIContextOpening 是已知的两句开头。历史消息与更长的自定义提示词里
	// 也可能出现，前后文的指令不属于匹配范围，必须逐字保留。
	dshGUIContextOpening = dshGUIContextSentence + " When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI."
)

// normalizeDSHGUIContextText 只改「已知两句开头」里第一句的环境说明，且跳过代码区。
// 前后文一律保留。返回改写后的文本与命中处数。
func normalizeDSHGUIContextText(text string) (string, int) {
	if !strings.Contains(text, dshGUIContextOpening) {
		return text, 0
	}
	var out strings.Builder
	copied, changed := 0, 0
	var fence byte
	fenceLength := 0
	codeTicks := 0
	for start := 0; start < len(text); {
		end := start + strings.IndexByte(text[start:], '\n')
		if end < start {
			end = len(text)
		}
		line := strings.TrimSuffix(text[start:end], "\r")
		next := end
		if next < len(text) {
			next++
		}
		trimmed := strings.TrimLeft(line, " ")
		isFence := false
		if codeTicks == 0 && len(line)-len(trimmed) <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n := 1
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if n >= 3 {
				if fence == 0 {
					fence, fenceLength, isFence = trimmed[0], n, true
				} else if fence == trimmed[0] && n >= fenceLength && strings.TrimSpace(trimmed[n:]) == "" {
					fence, fenceLength, isFence = 0, 0, true
				}
			}
		}
		indentedCode := len(line)-len(trimmed) >= 4 || strings.HasPrefix(line, "\t")
		if !isFence && fence == 0 && (!indentedCode || codeTicks > 0) {
			if normalized, n := normalizeDSHGUIContextLine(line, &codeTicks); n > 0 {
				out.WriteString(text[copied:start])
				out.WriteString(normalized)
				if len(line) != end-start {
					out.WriteByte('\r')
				}
				copied, changed = end, changed+n
			}
		}
		start = next
	}
	if changed == 0 {
		return text, 0
	}
	out.WriteString(text[copied:])
	return out.String(), changed
}

// normalizeDSHGUIContextLine 在单行内做替换，并维护跨行的反引号分隔符状态。
func normalizeDSHGUIContextLine(line string, codeTicks *int) (string, int) {
	var out strings.Builder
	copied, changed := 0, 0
	for pos := 0; pos < len(line); {
		if line[pos] == '`' {
			n := 1
			for pos+n < len(line) && line[pos+n] == '`' {
				n++
			}
			if *codeTicks == 0 {
				*codeTicks = n
			} else if *codeTicks == n {
				*codeTicks = 0
			}
			pos += n
			continue
		}
		if *codeTicks == 0 && strings.HasPrefix(line[pos:], dshGUIContextOpening) {
			out.WriteString(line[copied:pos])
			out.WriteString(dshGUIContextNeutral)
			out.WriteString(dshGUIContextOpening[len(dshGUIContextSentence):])
			pos += len(dshGUIContextOpening)
			copied, changed = pos, changed+1
		} else {
			pos++
		}
	}
	if changed == 0 {
		return line, 0
	}
	out.WriteString(line[copied:])
	return out.String(), changed
}

// normalizeDSHGUIContexts 只处理 system 与 assistant 的文本内容；user、tool、
// 工具参数、reasoning 与图片块不参与。数组形态处理**每一个** type=="text" 的块。
func normalizeDSHGUIContexts(messages []any) int {
	changed := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := roleOf(message)
		if role != "system" && role != "developer" && role != "assistant" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			if normalized, n := normalizeDSHGUIContextText(content); n > 0 {
				message["content"] = normalized
				changed += n
			}
		case []any:
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				if block["type"] != "text" {
					continue
				}
				if text, ok := block["text"].(string); ok {
					if normalized, n := normalizeDSHGUIContextText(text); n > 0 {
						block["text"] = normalized
						changed += n
					}
				}
			}
		}
	}
	return changed
}
