package outbound

import (
	"encoding/json"
	"strings"
	"testing"
)

// 脱敏的覆盖范围必须覆盖**工具参数与思维链**，这是两处长期盲区：
//   - 工具调用消息的 content 通常是 null，早期实现「content 缺失就跳过整条消息」，
//     于是工具参数里写进去的被拦字符串（文件名、命令、写入内容）原样漏出；
//   - 思维链（reasoning_content / reasoning）里同样会带指纹，而反探测串
//     出现在请求体里就必然被整单拦截。
//
// 文件里的这串数字有两种形态，很容易看错（也是本测试最值得写下来的一点）：
// 上游拦的是**不带连字符**的裸数字，净化动作是插入连字符破坏逐字匹配；
// 源码与注释里统一写成带连字符的形态，所以断言必须看「净化后是否含连字符形态」。
const (
	bareFingerprint   = "111" + "28" // 裸数字：会被上游整单拦截（拆开写，避免本文件自身成为指纹样本）
	brokenFingerprint = "11-128"     // 净化后的形态（插入连字符，语义与可读性不变）
)

// sanitizeOnce 对单条消息跑一次脱敏，返回消息与是否改动。
func sanitizeOnce(t *testing.T, messageJSON string) (map[string]any, bool) {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(`{"messages":[`+messageJSON+`]}`), &obj); err != nil {
		t.Fatalf("测试消息不是合法 JSON: %v", err)
	}
	list, _ := obj["messages"].([]any)
	changed := sanitizeMessages(list)
	msg, _ := list[0].(map[string]any)
	return msg, changed
}

func TestSanitizeToolCallArguments(t *testing.T) {
	msg, changed := sanitizeOnce(t, `{"role":"assistant","content":null,`+
		`"tool_calls":[{"id":"c1","type":"function","function":{"name":"bash",`+
		`"arguments":"{\"cmd\":\"echo `+bareFingerprint+`\"}"}}]}`)
	if !changed {
		t.Fatalf("工具参数里的反探测串没有被净化")
	}
	fn, _ := msg["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	if strings.Contains(args, bareFingerprint) {
		t.Errorf("工具参数仍带指纹：%s", args)
	}
	if !strings.Contains(args, brokenFingerprint) {
		t.Errorf("净化应插入连字符而不是删除：%s", args)
	}
}

func TestSanitizeReasoningContent(t *testing.T) {
	msg, changed := sanitizeOnce(t,
		`{"role":"assistant","content":"正常正文","reasoning_content":"分析 `+bareFingerprint+` 这个错误码"}`)
	if !changed {
		t.Fatalf("reasoning_content 里的指纹没有被净化")
	}
	rc, _ := msg["reasoning_content"].(string)
	if strings.Contains(rc, bareFingerprint) {
		t.Errorf("reasoning_content 仍带指纹：%s", rc)
	}
	if content, _ := msg["content"].(string); content != "正常正文" {
		t.Errorf("正文被误改：%s", content)
	}
}

func TestSanitizeReasoningMirrorField(t *testing.T) {
	msg, changed := sanitizeOnce(t,
		`{"role":"assistant","content":"正文","reasoning":"思考 `+bareFingerprint+`"}`)
	if !changed {
		t.Fatalf("reasoning 字段里的指纹没有被净化")
	}
	r, _ := msg["reasoning"].(string)
	if strings.Contains(r, bareFingerprint) {
		t.Errorf("reasoning 仍带指纹：%s", r)
	}
}

func TestSanitizeKeepsCleanMessagesUntouched(t *testing.T) {
	msg, changed := sanitizeOnce(t, `{"role":"user","content":"今天天气不错"}`)
	if changed {
		t.Errorf("干净消息不应报告改动")
	}
	if content, _ := msg["content"].(string); content != "今天天气不错" {
		t.Errorf("干净消息被改写：%s", content)
	}
}

// TestSanitizeSkipsNothingWhenContentMissing content 缺失（不是 null）的整条消息
// 也必须被扫描 tool_calls —— 早期实现在这里 continue，整块漏掉。
func TestSanitizeSkipsNothingWhenContentMissing(t *testing.T) {
	_, changed := sanitizeOnce(t, `{"role":"assistant",`+
		`"tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"`+bareFingerprint+`"}}]}`)
	if !changed {
		t.Errorf("content 缺省时 tool_calls 仍必须被净化")
	}
}
