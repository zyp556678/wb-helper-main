package outbound

import (
	"strings"
	"testing"
)

// 本文件覆盖 wb-gateway v1.14.24 移植过来的两条扩展（见 sanitizeprefix.go 与
// dshguicontext.go）：
//
//  1. 身份前缀不再只看 system 的**第一个**文本块，而是 system/developer 与
//     assistant 历史的**每一个**文本单元；
//  2. DSH 的 GUI 环境说明等义归一，但必须跳过围栏代码、缩进代码与行内代码。
//
// 这两条的失败模式相反：漏处理会让请求被上游整单拦下（11-128），
// 处理过头则是改动用户正文 —— 所以两边都要钉住。

func assistantMsg(content any) map[string]any {
	return map[string]any{"role": "assistant", "content": content}
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// TestAssistantPrefixSanitized assistant 历史里的身份前缀也要改写。
//
// 多轮会话会把上一轮的助手回复原样带回上游，只清 system 的话第二轮起仍会命中。
func TestAssistantPrefixSanitized(t *testing.T) {
	list, res := prepareWithMessages(t, []any{
		systemMsg("You are a helpful assistant."),
		userMsg("你好"),
		assistantMsg(harnessPrefix + prefixPromptBody),
	}, Options{})

	if res.AssistantPrefixSanitized != 1 {
		t.Fatalf("应报告改写 1 处 assistant 前缀，实际 %d", res.AssistantPrefixSanitized)
	}
	if res.SystemPrefixSanitized != 0 {
		t.Errorf("system 侧没有前缀，不该计数，实际 %d", res.SystemPrefixSanitized)
	}
	got, _ := list[2].(map[string]any)["content"].(string)
	if !strings.HasPrefix(got, neutralPrefix) {
		t.Errorf("assistant 前缀未改写: %q", got)
	}
	if !strings.HasSuffix(got, prefixPromptBody) {
		t.Errorf("前缀之后的正文必须逐字保留，实际 %q", got)
	}
}

// TestAssistantPrefixAllTextBlocks assistant 的每个文本块都要判，不只第一个。
func TestAssistantPrefixAllTextBlocks(t *testing.T) {
	list, res := prepareWithMessages(t, []any{
		systemMsg("You are a helpful assistant."),
		assistantMsg([]any{
			textBlock(harnessPrefix + " 第一段"),
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "AAAA"}},
			textBlock(harnessPrefix + " 第二段"),
		}),
	}, Options{})

	if res.AssistantPrefixSanitized != 2 {
		t.Fatalf("两个文本块都应改写，实际 %d", res.AssistantPrefixSanitized)
	}
	blocks, _ := list[1].(map[string]any)["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("块数不该变化，实际 %d", len(blocks))
	}
	// 非文本块原样保留
	if blocks[1].(map[string]any)["type"] != "image" {
		t.Errorf("图片块被改动了: %v", blocks[1])
	}
	for _, i := range []int{0, 2} {
		text, _ := blocks[i].(map[string]any)["text"].(string)
		if !strings.HasPrefix(text, neutralPrefix) {
			t.Errorf("块 %d 未改写: %q", i, text)
		}
	}
}

// TestSystemPrefixAllTextBlocks system 的多个文本块同样全部处理。
//
// 这条直接对应上游的 11-128 场景：Anthropic Messages 会把多个 system 文本块
// 合并，合并后每个块的开头都可能成为整段的开头。
func TestSystemPrefixAllTextBlocks(t *testing.T) {
	list, res := prepareWithMessages(t, []any{
		systemMsg([]any{
			textBlock(harnessPrefix + " 甲"),
			textBlock(harnessPrefix + " 乙"),
		}),
		userMsg("hi"),
	}, Options{})

	if res.SystemPrefixSanitized != 2 {
		t.Fatalf("两个 system 文本块都应改写，实际 %d", res.SystemPrefixSanitized)
	}
	blocks, _ := list[0].(map[string]any)["content"].([]any)
	for i, raw := range blocks {
		text, _ := raw.(map[string]any)["text"].(string)
		if !strings.HasPrefix(text, neutralPrefix) {
			t.Errorf("system 块 %d 未改写: %q", i, text)
		}
	}
}

// TestAssistantPrefixIgnoresUserAndTool user / tool 内容不参与身份前缀改写。
//
// 用户正文里讨论这句话是正常内容，改了就是误伤。
func TestAssistantPrefixIgnoresUserAndTool(t *testing.T) {
	_, res := prepareWithMessages(t, []any{
		systemMsg("You are a helpful assistant."),
		userMsg(harnessPrefix + " 这句话为什么会被拦？"),
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": harnessPrefix + " 工具结果"},
	}, Options{})

	if res.AssistantPrefixSanitized != 0 || res.SystemPrefixSanitized != 0 {
		t.Fatalf("user / tool 不该被改写，实际 system=%d assistant=%d",
			res.SystemPrefixSanitized, res.AssistantPrefixSanitized)
	}
}

// --- DSH GUI 环境句 ---

func guiBody(prefix string) string {
	return prefix + dshGUIContextOpening + "\n\n" + prefixPromptBody
}

// TestDSHGUIContextNormalized GUI 环境句归一，第二句与前后文逐字保留。
func TestDSHGUIContextNormalized(t *testing.T) {
	list, res := prepareWithMessages(t, []any{
		systemMsg(guiBody("")),
		userMsg("hi"),
	}, Options{})

	if res.GUIContextNormalized != 1 {
		t.Fatalf("应归一 1 处 GUI 环境句，实际 %d", res.GUIContextNormalized)
	}
	got, _ := list[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, dshGUIContextNeutral) {
		t.Errorf("第一句未归一: %q", got)
	}
	if strings.Contains(got, "DeepSeek Harness Web GUI") {
		t.Errorf("仍含可识别特征串: %q", got)
	}
	// 第二句必须原样保留（只改第一句里的一个短语）
	second := dshGUIContextOpening[len(dshGUIContextSentence):]
	if !strings.Contains(got, second) {
		t.Errorf("第二句被改动了: %q", got)
	}
	if !strings.Contains(got, prefixPromptBody) {
		t.Errorf("环境句之后的正文必须逐字保留: %q", got)
	}
}

// TestDSHGUIContextSkipsFencedCode 围栏代码里的内容不动。
func TestDSHGUIContextSkipsFencedCode(t *testing.T) {
	for _, fence := range []string{"```", "~~~"} {
		body := fence + "\n" + dshGUIContextOpening + "\n" + fence + "\n"
		_, res := prepareWithMessages(t, []any{systemMsg(body), userMsg("hi")}, Options{})
		if res.GUIContextNormalized != 0 {
			t.Errorf("围栏 %q 内的内容不该被改写，实际 %d", fence, res.GUIContextNormalized)
		}
	}
}

// TestDSHGUIContextSkipsIndentedCode 缩进代码（4 空格或 Tab 起行）不动。
func TestDSHGUIContextSkipsIndentedCode(t *testing.T) {
	for _, indent := range []string{"    ", "\t"} {
		body := indent + dshGUIContextOpening + "\n"
		_, res := prepareWithMessages(t, []any{systemMsg(body), userMsg("hi")}, Options{})
		if res.GUIContextNormalized != 0 {
			t.Errorf("缩进 %q 起的代码块不该被改写，实际 %d", indent, res.GUIContextNormalized)
		}
	}
}

// TestDSHGUIContextSkipsInlineCode 行内代码（反引号包裹）不动。
func TestDSHGUIContextSkipsInlineCode(t *testing.T) {
	body := "示例：`" + dshGUIContextOpening + "`\n"
	_, res := prepareWithMessages(t, []any{systemMsg(body), userMsg("hi")}, Options{})
	if res.GUIContextNormalized != 0 {
		t.Errorf("行内代码不该被改写，实际 %d", res.GUIContextNormalized)
	}
}

// TestDSHGUIContextAfterFence 围栏结束后的正文仍要处理 —— 跳过逻辑不能一路跳到底。
func TestDSHGUIContextAfterFence(t *testing.T) {
	body := "```\ncode\n```\n" + dshGUIContextOpening + "\n"
	list, res := prepareWithMessages(t, []any{systemMsg(body), userMsg("hi")}, Options{})
	if res.GUIContextNormalized != 1 {
		t.Fatalf("围栏外的环境句应被归一，实际 %d", res.GUIContextNormalized)
	}
	got, _ := list[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(got, "```\ncode\n```\n") {
		t.Errorf("围栏代码必须原样保留: %q", got)
	}
}

// TestDSHGUIContextIgnoresUser user 内容不参与 GUI 归一。
func TestDSHGUIContextIgnoresUser(t *testing.T) {
	_, res := prepareWithMessages(t, []any{
		systemMsg("You are a helpful assistant."),
		userMsg(dshGUIContextOpening),
	}, Options{})
	if res.GUIContextNormalized != 0 {
		t.Errorf("user 内容不该被改写，实际 %d", res.GUIContextNormalized)
	}
}

// TestDSHGUIContextAssistantHistory assistant 历史里的环境句同样归一。
func TestDSHGUIContextAssistantHistory(t *testing.T) {
	list, res := prepareWithMessages(t, []any{
		systemMsg("You are a helpful assistant."),
		assistantMsg(dshGUIContextOpening),
	}, Options{})
	if res.GUIContextNormalized != 1 {
		t.Fatalf("assistant 历史里的环境句应被归一，实际 %d", res.GUIContextNormalized)
	}
	got, _ := list[1].(map[string]any)["content"].(string)
	if !strings.Contains(got, dshGUIContextNeutral) {
		t.Errorf("未归一: %q", got)
	}
}
