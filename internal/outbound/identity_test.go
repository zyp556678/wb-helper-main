package outbound

import (
	"strings"
	"testing"
)

// 本文件覆盖 identity 配置化（对照 wb-gateway v1.14.29 / v1.14.40）。
//
// 核心诉求有两条，方向相反，必须同时钉住：
//   - 已知身份前缀（DSH / Claude Code / Codex / ZCode）**必须**被改写，
//     否则那些客户端一律被上游 11-128 拦下；
//   - 用户正文、工具内容、以及正文中段出现的同样字样**必须**保持原样，
//     否则就是改动用户内容。

const claudeCodeSystem = "You are Claude Code, Anthropic's official CLI tool for Claude."

// TestIdentityClaudeCodePrefix Claude Code 的身份前缀要改写，前缀之后逐字保留。
func TestIdentityClaudeCodePrefix(t *testing.T) {
	list, res := prepareWithMessages(t, []any{
		systemMsg(claudeCodeSystem),
		userMsg("hi"),
	}, Options{})

	if res.SystemPrefixSanitized != 1 {
		t.Fatalf("应改写 1 处，实际 %d", res.SystemPrefixSanitized)
	}
	got, _ := list[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(got, neutralPrefix) {
		t.Fatalf("前缀未改写为中性句: %q", got)
	}
	// 前缀之后的正文必须逐字保留 —— 包括 "tool" 这个词。
	if !strings.Contains(got, ", Anthropic's official CLI tool for Claude.") {
		t.Errorf("前缀之后的正文被改动了: %q", got)
	}
}

// TestIdentityCodexAndZCodePrefix Codex / ZCode 的身份前缀同样改写。
func TestIdentityCodexAndZCodePrefix(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"Codex", "You are Codex, based on GPT-5."},
		{"ZCode", "You are ZCode, a coding agent."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, res := prepareWithMessages(t, []any{systemMsg(tc.text), userMsg("hi")}, Options{})
			if res.SystemPrefixSanitized != 1 {
				t.Fatalf("应改写 1 处，实际 %d", res.SystemPrefixSanitized)
			}
			got, _ := list[0].(map[string]any)["content"].(string)
			if !strings.HasPrefix(got, neutralPrefix) {
				t.Errorf("前缀未改写: %q", got)
			}
		})
	}
}

// TestIdentityPrefixLongestFirst "You are Claude" 不能先把 "You are Claude Code" 截断。
func TestIdentityPrefixLongestFirst(t *testing.T) {
	list, _ := prepareWithMessages(t, []any{systemMsg(claudeCodeSystem), userMsg("hi")}, Options{})
	got, _ := list[0].(map[string]any)["content"].(string)
	if strings.Contains(got, " Code,") {
		t.Errorf("长前缀被短前缀截断，残留了 \" Code\": %q", got)
	}
}

// TestIdentityDisabled 关掉开关后一个字都不动。
func TestIdentityDisabled(t *testing.T) {
	disabled := true
	list, res := prepareWithMessages(t, []any{
		systemMsg(claudeCodeSystem),
		userMsg("hi"),
	}, Options{Identity: IdentitySettings{Disabled: disabled}})

	if res.SystemPrefixSanitized != 0 || res.AssistantPrefixSanitized != 0 {
		t.Fatalf("关掉后不该改写，实际 system=%d assistant=%d",
			res.SystemPrefixSanitized, res.AssistantPrefixSanitized)
	}
	got, _ := list[0].(map[string]any)["content"].(string)
	if got != claudeCodeSystem {
		t.Errorf("关掉后正文被改动: %q", got)
	}
}

// TestIdentityCustomPrefix 自定义替换前缀生效。
func TestIdentityCustomPrefix(t *testing.T) {
	list, _ := prepareWithMessages(t, []any{systemMsg(claudeCodeSystem), userMsg("hi")},
		Options{Identity: IdentitySettings{Prefix: "You are WorkBuddy"}})
	got, _ := list[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(got, "You are WorkBuddy") {
		t.Errorf("自定义前缀未生效: %q", got)
	}
}

// TestIdentityDefaultChannelReplacements 两条默认渠道兼容替换生效。
func TestIdentityDefaultChannelReplacements(t *testing.T) {
	body := "The workspace root is /x. These are paths the user can read."
	list, _ := prepareWithMessages(t, []any{systemMsg(body), userMsg("hi")}, Options{})
	got, _ := list[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, "workspace base") {
		t.Errorf("workspace root 未替换: %q", got)
	}
	if !strings.Contains(got, "are paths that the user") {
		t.Errorf("are paths the user 未替换: %q", got)
	}
}

// TestIdentityCustomReplacementsReplaceDefaults 自定义替换表整体替代默认表。
func TestIdentityCustomReplacementsReplaceDefaults(t *testing.T) {
	body := "The workspace root is /x. Claude Code is great."
	list, _ := prepareWithMessages(t, []any{systemMsg(body), userMsg("hi")},
		Options{Identity: IdentitySettings{
			Replacements: map[string]string{"Claude Code": "WorkBuddy"},
		}})
	got, _ := list[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, "WorkBuddy is great") {
		t.Errorf("自定义替换未生效: %q", got)
	}
	// 自定义表替代了默认表，所以 workspace root 不再被替换。
	if !strings.Contains(got, "workspace root") {
		t.Errorf("自定义表应当整体替代默认表，但 workspace root 被改了: %q", got)
	}
}

// TestIdentityOnlyAtOffsetZero 正文中段出现的同样字样不动。
func TestIdentityOnlyAtOffsetZero(t *testing.T) {
	body := "请解释这句话：You are Claude Code, Anthropic's official CLI tool for Claude."
	list, res := prepareWithMessages(t, []any{systemMsg(body), userMsg("hi")}, Options{})
	if res.SystemPrefixSanitized != 0 {
		t.Fatalf("中段不该被改写，实际 %d", res.SystemPrefixSanitized)
	}
	got, _ := list[0].(map[string]any)["content"].(string)
	if got != body {
		t.Errorf("中段正文被改动: %q", got)
	}
}

// TestIdentityRewriteDoesNotDisableFingerprintSanitize 身份改写之后，指纹脱敏仍须生效。
//
// 这是一条**回归测试**：脱敏路径由 hasFingerprint 预检把关，而它的特征串里原本只有
// "You are Claude Code"。身份前缀一旦被改写，这个特征串就消失了，预检会误判成
// 「干净」而整段跳过脱敏 —— 结果反而是句子里剩下的
// "Anthropic's official CLI for Claude" 原样发给上游，指纹漏得更彻底。
//
// 修法是给特征串补上改写后残留的那个形态（见 sanitize.go）。
func TestIdentityRewriteDoesNotDisableFingerprintSanitize(t *testing.T) {
	// 输入用**运行时去掉 "tool "** 的方式构造，而不是再手写一遍字面量：
	// 这条断言的要点就是「有没有 tool」，手写字面量一旦抄错就变成自证。
	withoutTool := strings.Replace(claudeCodeSystem, "tool ", "", 1)
	if strings.Contains(withoutTool, "tool") {
		t.Fatalf("构造失败：输入里仍含 tool: %q", withoutTool)
	}
	list, res := prepareWithMessages(t, []any{
		systemMsg(withoutTool),
		userMsg("hi"),
	}, Options{Sanitize: true})

	if res.SystemPrefixSanitized != 1 {
		t.Fatalf("应改写 1 处身份前缀，实际 %d", res.SystemPrefixSanitized)
	}
	if !res.Sanitized {
		t.Fatalf("身份改写后脱敏仍须生效，但 Sanitized=false")
	}
	got, _ := list[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(got, neutralPrefix) {
		t.Errorf("前缀未改写: %q", got)
	}
	// 脱敏把 "tool" 插了回来，说明它确实跑了。
	if !strings.Contains(got, "official CLI tool for Claude") {
		t.Errorf("脱敏没有打断残留指纹: %q", got)
	}
}
