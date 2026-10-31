package outbound

import (
	"sort"
	"strings"
)

// 本文件是出站「身份改写」的参数与规则（对照 wb-gateway v1.14.29 / v1.14.40 / v1.14.41）。
//
// 要解决的问题：上游按提示词里的**身份陈述**做渠道校验，命中即整单拦下（11-128）。
// 不同客户端自报的身份不同 —— DSH 说 "You are an AI agent powered by DeepSeek
// Harness"，Claude Code 说 "You are Claude Code..."，Codex 说 "You are Codex"。
// 网关必须在发出去之前把这段身份改掉，否则这些客户端一律不可用。
//
// # 与上游的取舍（本项目**有意不跟随**的部分）
//
// 上游默认把 5 个身份词（Claude Code / Claude / DeepSeek Harness / Codex / ZCode）
// 在 system/developer/assistant 文本里**全文替换**成 `WorkBuddy`，官方文档也写明了
// 「正文中的产品名和技能说明也会被替换；不是只改开头身份句」。这有两个副作用：
//
//  1. 冒充：请求会自称 WorkBuddy 官方客户端；
//  2. 误伤：system 里正常讨论「这个技能适用于 Claude Code」也会被改掉。
//
// 本项目改为：
//   - **默认只改偏移 0 的身份前缀**（就是上游那条校验真正看的位置），正文不动；
//   - 默认替换目标是**中性描述**，不冒充 WorkBuddy；
//   - 需要上游那种激进行为时，用户可以自己配 `identity.replacements`。
//
// 默认仍保留两条**渠道兼容替换**（v1.14.40 引入）：它们是 DSH 独有措辞的等义改写，
// 对非 DSH 请求是空操作，语义中性，因此默认开启。

// DefaultIdentityPrefix 是偏移 0 命中后的默认替换前缀。
//
// 为什么是一整句而不是一个词：上游的做法是 `You are <target>`，target 取一个词
// （默认 WorkBuddy）。本项目选一句中性的身份陈述，语义完整且不指向任何具体产品。
const DefaultIdentityPrefix = "You are an AI agent powered by a large language model"

// defaultChannelReplacements 是默认启用的渠道兼容替换（对照 wb-gateway v1.14.40）。
//
// 两条都只命中 DSH 注入的措辞，对其它请求是空操作；改写后语义完全等价。
func defaultChannelReplacements() map[string]string {
	return map[string]string{
		"workspace root":     "workspace base",
		"are paths the user": "are paths that the user",
	}
}

// identityPrefixes 是偏移 0 会被改写的身份前缀，**按长度降序**。
//
// 顺序有实质影响：`You are Claude` 必须排在 `You are Claude Code` 之后，
// 否则前者会先把后者截断，留下一个 " Code..." 的尾巴。
var identityPrefixes = func() []string {
	list := []string{
		"You are an AI agent powered by DeepSeek Harness",
		"You are Claude Code",
		"You are Claude",
		"You are Codex",
		"You are ZCode",
	}
	sort.SliceStable(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	return list
}()

// identityReplacement 是一条术语替换。Old 长的排前面，避免
// "Claude Code" 被 "Claude" 先拆成 "X Code"。
type identityReplacement struct {
	Old string
	New string
}

// IdentitySettings 是本次出站身份改写的生效参数。
//
// **零值等于内置默认**（启用 + 中性前缀 + 默认渠道兼容替换）：
// 这样 Options 的零值在测试与旧调用点上都是「按默认跑」，不会因为漏传而静默失效。
type IdentitySettings struct {
	// Disabled 为真时整段跳过（对应 config.json 的 identity.enabled=false）。
	Disabled bool
	// Prefix 是偏移 0 命中后的替换前缀；空串用 DefaultIdentityPrefix。
	Prefix string
	// Replacements 是术语替换表；为空时用默认渠道兼容替换。
	// 非空时**整体替代**默认表（与上游语义一致），想保留默认再追加就自己写全。
	Replacements map[string]string

	// sorted 是 Replacements 按 Old 长度降序展开后的结果，由 withDefaults 填好。
	sorted []identityReplacement
}

// withDefaults 补齐零值并预处理替换表。返回的是一份可直接使用的副本。
func (s IdentitySettings) withDefaults() IdentitySettings {
	out := s
	if strings.TrimSpace(out.Prefix) == "" {
		out.Prefix = DefaultIdentityPrefix
	}
	source := out.Replacements
	if len(source) == 0 {
		source = defaultChannelReplacements()
	}
	out.sorted = make([]identityReplacement, 0, len(source))
	for old, newValue := range source {
		if strings.TrimSpace(old) == "" {
			continue
		}
		out.sorted = append(out.sorted, identityReplacement{Old: old, New: newValue})
	}
	sort.SliceStable(out.sorted, func(i, j int) bool {
		if len(out.sorted[i].Old) != len(out.sorted[j].Old) {
			return len(out.sorted[i].Old) > len(out.sorted[j].Old)
		}
		return out.sorted[i].Old < out.sorted[j].Old
	})
	return out
}

// normalizeIdentityText 对一个文本单元做身份改写，返回改写结果与是否命中。
//
// 两步，顺序固定：
//  1. **偏移 0** 的身份前缀换成配置的前缀（前缀后正文逐字保留）；
//  2. 术语替换（默认只有两条渠道兼容替换）。
//
// 只应该被 system / developer / assistant 的文本调用；user / tool 不参与 ——
// 用户正文里讨论这些身份是正常内容，改了就是误伤。
func normalizeIdentityText(text string, s IdentitySettings) (string, bool) {
	if s.Disabled {
		return text, false
	}
	original := text
	for _, prefix := range identityPrefixes {
		if len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix) {
			text = s.Prefix + text[len(prefix):]
			break
		}
	}
	for _, r := range s.sorted {
		text = strings.ReplaceAll(text, r.Old, r.New)
	}
	return text, text != original
}
