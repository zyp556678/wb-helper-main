package outbound

import "strings"

// normalizeToolPatterns 修掉工具定义里 `pattern` 的非标转义 `\_`。
//
// 为什么需要它：上游对工具 schema 做严格正则文法校验（唯一对得上的实现是
// V8 严格文法带 `u` 标志），`\_`（转义的**字面量下划线**）不是任何正则文法的
// 合法转义 —— RE2/PCRE/JS Annex B 都宽容视为 `_`，但严格文法整体拒收：
//
//	400 code=11129 invalid_function_call_parameters
//
// **后果远不止"一个请求失败"**：这是 schema 级错误，换账号无用（不是额度/鉴权问题），
// 会被归成 ErrClient（只换号不罚），喂进连败计数 → 轮转烧满 5 连败 → 降权 →
// 客户端拿到 503。**即工具定义里的两个反斜杠能打挂整条账号池的路由权重。**
//
// 返回被改写的处数（便于观测与单测）。
//
// 四条刻意的约束（照抄上游口径，**不要自行扩面**）：
//
//  1. **只动 tools 子树**（`pattern` 的值与 `patternProperties` 的**键**），
//     消息正文里的 `\_` 绝不碰 —— 反例：Windows 路径 `C:\_x` 是合法内容。
//  2. 覆盖两种形态：`tools[].function.parameters` 与裸 `tools[].parameters`。
//  3. **独立于指纹脱敏开关** —— 这是「让请求通过」，不是脱敏，
//     与「要不要伪装客户端指纹」是两件无关的事。
//  4. **只处理 `\_`，其余非标转义（`\:` 等）不动** —— 未证实会触发，
//     有实案再议。写清楚是为了后人别当成多余的字符串替换删掉。
func normalizeToolPatterns(obj map[string]any) int {
	tools, ok := obj["tools"].([]any)
	if !ok {
		return 0
	}
	total := 0
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// 两种形态都要覆盖：function.parameters（OpenAI 标准）与裸 parameters。
		if fn, ok := tool["function"].(map[string]any); ok {
			total += normalizeSchemaNode(fn["parameters"])
		}
		total += normalizeSchemaNode(tool["parameters"])
	}
	return total
}

// normalizeSchemaNode 递归遍历一个 JSON Schema 子树，修掉其中的 `\_`。
//
// 递归范围只覆盖**能承载 pattern 的容器**：properties / patternProperties 的值、
// items / additionalProperties、组合子（allOf/anyOf/oneOf）、$defs/definitions。
// 不做「任意 map 全遍历」—— 那会误伤 `default`、`examples`、`enum` 里
// 用户数据中的反斜杠（它们不是正则，改了就是破坏请求内容）。
func normalizeSchemaNode(node any) int {
	m, ok := node.(map[string]any)
	if !ok {
		// items 可以是数组（元组式 schema），逐项处理。
		if arr, ok := node.([]any); ok {
			n := 0
			for _, item := range arr {
				n += normalizeSchemaNode(item)
			}
			return n
		}
		return 0
	}

	n := 0

	// ---- pattern 的值 ----
	if pat, ok := m["pattern"].(string); ok {
		if fixed, changed := unescapeLiteralUnderscore(pat); changed {
			m["pattern"] = fixed
			n++
		}
	}

	// ---- patternProperties 的键 ----
	// 键是 map 的键、不能原地改，命中时必须重建这一层。
	if pp, ok := m["patternProperties"].(map[string]any); ok {
		rebuilt := make(map[string]any, len(pp))
		layerChanged := false
		for k, v := range pp {
			nk, changed := unescapeLiteralUnderscore(k)
			if changed {
				layerChanged = true
				n++
			}
			rebuilt[nk] = v
		}
		if layerChanged {
			m["patternProperties"] = rebuilt
		}
		// patternProperties 的**值**也是 schema，继续往下走。
		for _, v := range rebuilt {
			n += normalizeSchemaNode(v)
		}
	}

	// ---- properties 的值 ----
	if props, ok := m["properties"].(map[string]any); ok {
		for _, v := range props {
			n += normalizeSchemaNode(v)
		}
	}

	// ---- $defs / definitions 的值 ----
	//
	// **它们和 properties 一样是「名字 → schema」的 map，必须逐值递归。**
	// 曾经把它们放进下面那张「值是单个 schema」的通用列表里 ——
	// 那样只会对 map 本身找 `pattern`（找不到），**根本不会进到里面的 schema**，
	// 于是 `$defs` 里的非标转义被完整漏掉（单测抓到）。
	if defs, ok := m["$defs"].(map[string]any); ok {
		for _, v := range defs {
			n += normalizeSchemaNode(v)
		}
	}
	if defs, ok := m["definitions"].(map[string]any); ok {
		for _, v := range defs {
			n += normalizeSchemaNode(v)
		}
	}

	// ---- 其余承载**单个** schema 的位置 ----
	for _, key := range []string{"items", "additionalProperties", "not", "if", "then", "else", "propertyNames", "contains"} {
		if v, ok := m[key]; ok {
			n += normalizeSchemaNode(v)
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if arr, ok := m[key].([]any); ok {
			for _, v := range arr {
				n += normalizeSchemaNode(v)
			}
		}
	}

	return n
}

// unescapeLiteralUnderscore 把正则里的 `\_` 还原成 `_`。
//
// **必须按反斜杠的奇偶来判断，不能朴素替换子串 `\_`**：
//
//	输入 `\\_`（三个字符：反斜杠、反斜杠、下划线）
//	  语义 = 正则 `\\`（一个字面反斜杠）+ `_`（字面下划线），本身**完全合法**。
//	  朴素替换会把第二个反斜杠与下划线当成 `\_` 吃掉，
//	  结果变成 `\_` = 一个字面下划线 —— **语义被悄悄改掉了**，
//	  而调用方完全看不出来。
//
// 所以：数连续的 backslash，只有**奇数**个（说明最后一个是转义符）时才吃掉它。
func unescapeLiteralUnderscore(s string) (string, bool) {
	if !strings.ContainsRune(s, '\\') || !strings.ContainsRune(s, '_') {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s))
	changed := false
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// 数这一串连续的反斜杠。
		j := i
		for j < len(s) && s[j] == '\\' {
			j++
		}
		run := j - i
		if run%2 == 1 && j < len(s) && s[j] == '_' {
			// 奇数个：最后一个是转义符，作用于后面的 `_`。
			// 丢掉它，保留 `_`。
			b.WriteString(s[i : j-1])
			b.WriteByte('_')
			i = j + 1
			changed = true
			continue
		}
		// 偶数个：反斜杠两两成对，后面的 `_` 未被转义，原样保留。
		b.WriteString(s[i:j])
		i = j
	}
	return b.String(), changed
}
