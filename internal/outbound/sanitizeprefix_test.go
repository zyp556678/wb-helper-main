package outbound

import (
	"encoding/json"
	"strings"
	"testing"
)

// 上游按 system 消息的**开头前缀**做渠道校验：命中即整单拦截（错误码 11128）。
// 这一组的重点不在「能不能替换」，而在三条边界——只认偏移 0、忽略大小写、
// 正文其余部分逐字保留。三条都是「改多了就变成动用户内容」的地方。
const (
	harnessPrefix    = "You are an AI agent powered by DeepSeek Harness"
	neutralPrefix    = "You are an AI agent powered by a large language model"
	harnessRuleName  = "deepseek_harness_system_prefix"
	prefixPromptBody = "\n\nYou are a coding agent. Follow the repository conventions."
)

// prepareWithMessages 跑一次完整出站管线，返回改写后的 messages 与结果。
//
// messages 用 Go 值传入而不是拼 JSON 字符串：正文里有换行与中文，
// 手拼 JSON 很容易写出「非法 JSON」的假失败，让测试反过来骗自己。
func prepareWithMessages(t *testing.T, messages []any, opts Options) ([]any, Result) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    "deepseek-v4.1-flash",
		"messages": messages,
	})
	if err != nil {
		t.Fatalf("构造测试请求体失败: %v", err)
	}
	if opts.PromptMode == "" {
		opts.PromptMode = PromptPassthrough
	}
	res := Prepare(body, opts)
	var obj map[string]any
	if err := json.Unmarshal(res.Body, &obj); err != nil {
		t.Fatalf("出站体不是合法 JSON: %v", err)
	}
	list, _ := obj["messages"].([]any)
	return list, res
}

func systemMsg(content any) map[string]any {
	return map[string]any{"role": "system", "content": content}
}

func userMsg(content string) map[string]any {
	return map[string]any{"role": "user", "content": content}
}

func firstMessage(t *testing.T, list []any) map[string]any {
	t.Helper()
	if len(list) == 0 {
		t.Fatalf("改写后 messages 为空")
	}
	msg, _ := list[0].(map[string]any)
	return msg
}

// TestSystemPrefixSanitizedAtOffsetZero 命中时只替换前缀，正文逐字保留。
func TestSystemPrefixSanitizedAtOffsetZero(t *testing.T) {
	list, res := prepareWithMessages(t, []any{
		systemMsg(harnessPrefix + prefixPromptBody),
		userMsg("hi"),
	}, Options{Sanitize: false})

	if res.SystemPrefixSanitized != 1 {
		t.Fatalf("应报告改写 1 条 system，实际 %d", res.SystemPrefixSanitized)
	}
	content, _ := firstMessage(t, list)["content"].(string)
	if strings.Contains(content, "DeepSeek Harness") {
		t.Errorf("开头前缀仍带上游特征串：%q", content)
	}
	if !strings.HasPrefix(content, neutralPrefix) {
		t.Errorf("开头应被等义改写为中性前缀，实际：%q", content)
	}
	if !strings.HasSuffix(content, prefixPromptBody) {
		t.Errorf("前缀之后的正文必须逐字保留，实际：%q", content)
	}
}

// TestSystemPrefixCaseInsensitive 大小写不同也要命中（上游匹配忽略大小写）。
func TestSystemPrefixCaseInsensitive(t *testing.T) {
	lowered := strings.ToLower(harnessPrefix)
	list, res := prepareWithMessages(t, []any{
		systemMsg(lowered),
		userMsg("hi"),
	}, Options{Sanitize: false})
	if res.SystemPrefixSanitized != 1 {
		t.Fatalf("大小写不同也应命中，实际改写 %d 条", res.SystemPrefixSanitized)
	}
	content, _ := firstMessage(t, list)["content"].(string)
	if content != neutralPrefix {
		t.Errorf("结果应恰好是中性前缀，实际：%q", content)
	}
}

// TestSystemPrefixOnlyAtOffsetZero 正文中段出现同样的句子不该被改。
//
// 这是与 sanitize.go 子串级净化最关键的区别：改中段属于动用户内容。
func TestSystemPrefixOnlyAtOffsetZero(t *testing.T) {
	body := "请把这段 system 提示词改得不容易被拦：" + harnessPrefix
	list, res := prepareWithMessages(t, []any{
		systemMsg(body),
		userMsg("hi"),
	}, Options{Sanitize: false})
	if res.SystemPrefixSanitized != 0 {
		t.Errorf("中段出现不该被改写，实际改写 %d 条", res.SystemPrefixSanitized)
	}
	if content, _ := firstMessage(t, list)["content"].(string); content != body {
		t.Errorf("中段内容被改动：%q", content)
	}
}

// TestSystemPrefixSkipsLeadingWhitespace 前导空白时不命中：上游也不跳过它，
// 此时前缀不在偏移 0，改了就是无谓地动用户内容。
func TestSystemPrefixSkipsLeadingWhitespace(t *testing.T) {
	_, res := prepareWithMessages(t, []any{
		systemMsg("\n\n" + harnessPrefix),
		userMsg("hi"),
	}, Options{Sanitize: false})
	if res.SystemPrefixSanitized != 0 {
		t.Errorf("前导空白时不应改写，实际 %d 条", res.SystemPrefixSanitized)
	}
}

// TestSystemPrefixContentBlocks 数组形态只处理第一个文本块。
func TestSystemPrefixContentBlocks(t *testing.T) {
	list, res := prepareWithMessages(t, []any{
		systemMsg([]any{
			map[string]any{"type": "text", "text": harnessPrefix},
			map[string]any{"type": "text", "text": "第二块里也写了 " + harnessPrefix},
		}),
		userMsg("hi"),
	}, Options{Sanitize: false})
	if res.SystemPrefixSanitized != 1 {
		t.Fatalf("应报告改写 1 条，实际 %d", res.SystemPrefixSanitized)
	}
	parts, _ := firstMessage(t, list)["content"].([]any)
	first, _ := parts[0].(map[string]any)["text"].(string)
	second, _ := parts[1].(map[string]any)["text"].(string)
	if strings.HasPrefix(first, harnessPrefix) {
		t.Errorf("第一个文本块开头应被改写：%q", first)
	}
	if !strings.HasSuffix(second, harnessPrefix) {
		t.Errorf("后续文本块不在开头位置，必须逐字保留：%q", second)
	}
}

// TestSystemPrefixSkipsNonTextBlocks 图片等非文本块不碰，顺序不变；
// 前缀判定落在**第一个文本块**上（它才是提示词的起点）。
func TestSystemPrefixSkipsNonTextBlocks(t *testing.T) {
	image := map[string]any{"type": "image", "source": "https://example.invalid/a.png"}
	list, res := prepareWithMessages(t, []any{
		systemMsg([]any{image, map[string]any{"type": "text", "text": harnessPrefix}}),
		userMsg("hi"),
	}, Options{Sanitize: false})
	if res.SystemPrefixSanitized != 1 {
		t.Fatalf("第一个文本块应被改写，实际 %d", res.SystemPrefixSanitized)
	}
	parts, _ := firstMessage(t, list)["content"].([]any)
	if got, _ := parts[0].(map[string]any)["type"].(string); got != "image" {
		t.Errorf("非文本块被移动或改写：%v", parts[0])
	}
	if text, _ := parts[1].(map[string]any)["text"].(string); strings.HasPrefix(text, harnessPrefix) {
		t.Errorf("第一个文本块的开头应被改写：%q", text)
	}
}

// TestSystemPrefixIgnoresUserRole user / assistant 消息一律不动——
// 上游只看 system 开头，改用户消息属于越权改内容。
func TestSystemPrefixIgnoresUserRole(t *testing.T) {
	_, res := prepareWithMessages(t, []any{
		systemMsg("You are a helpful assistant."),
		userMsg(harnessPrefix),
	}, Options{Sanitize: false})
	if res.SystemPrefixSanitized != 0 {
		t.Errorf("user 消息不该被改写，实际 %d 条", res.SystemPrefixSanitized)
	}
}

// TestSystemPrefixDeveloperRole developer 与 system 同权（role 归一本步之后才做）。
func TestSystemPrefixDeveloperRole(t *testing.T) {
	_, res := prepareWithMessages(t, []any{
		map[string]any{"role": "developer", "content": harnessPrefix},
		userMsg("hi"),
	}, Options{Sanitize: false})
	if res.SystemPrefixSanitized != 1 {
		t.Errorf("developer 消息也应被改写，实际 %d 条", res.SystemPrefixSanitized)
	}
}

// TestSystemPrefixRunsBeforeFingerprintSanitize 顺序约束：
// 脱敏开着时，前缀净化必须先跑，否则偏移 0 的匹配抓不到。
func TestSystemPrefixRunsBeforeFingerprintSanitize(t *testing.T) {
	list, res := prepareWithMessages(t, []any{
		systemMsg(harnessPrefix + prefixPromptBody),
		userMsg("hi"),
	}, Options{Sanitize: true})
	if res.SystemPrefixSanitized != 1 {
		t.Fatalf("开着脱敏时前缀净化仍须生效，实际 %d 条", res.SystemPrefixSanitized)
	}
	content, _ := firstMessage(t, list)["content"].(string)
	if !strings.HasPrefix(content, neutralPrefix) {
		t.Errorf("结果应以中性前缀开头，实际：%q", content)
	}
}

// TestSystemPrefixRuleNameStable 规则名是排障时的检索键，改了等于断掉历史日志的线索。
func TestSystemPrefixRuleNameStable(t *testing.T) {
	if harnessRuleName != "deepseek_harness_system_prefix" {
		t.Fatalf("规则名被改动：%s", harnessRuleName)
	}
}

// TestSystemPrefixCleanMessageUntouched 不含特征串的 system 必须原样返回。
func TestSystemPrefixCleanMessageUntouched(t *testing.T) {
	const clean = "You are a helpful assistant. Keep answers short."
	list, res := prepareWithMessages(t, []any{
		systemMsg(clean),
		userMsg("hi"),
	}, Options{Sanitize: false})
	if res.SystemPrefixSanitized != 0 {
		t.Errorf("干净 system 不应报告改写，实际 %d", res.SystemPrefixSanitized)
	}
	if content, _ := firstMessage(t, list)["content"].(string); content != clean {
		t.Errorf("干净 system 被改动：%q", content)
	}
}
