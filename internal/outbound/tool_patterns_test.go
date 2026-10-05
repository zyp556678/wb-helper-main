package outbound

import (
	"encoding/json"
	"strings"
	"testing"
)

// bs 是**解码后**的单个反斜杠（Go 字符串里看到的那个字符）。
//
// **必须运行时拼装**：测试体里的反斜杠要经过多层工具链转义，
// 直接写字符串字面量极易被吞成 `_`，那样测试就变成「什么都没测」还显示通过。
func bs() string { return string(byte(92)) }

// wire 是**线上形式**的反斜杠（嵌在 JSON 字符串字面量里必须写两个）。
//
// 这一层编码关系是本次实现最容易搞错的地方，单独记下来：
//
//	客户端要表达的正则     \_      （一个反斜杠 + 下划线）
//	HTTP 线上的 JSON 文本  \\_     （JSON 里反斜杠必须自身转义 → 写成两个）
//	Go json.Unmarshal 之后 \_      （又变回一个反斜杠 + 下划线）
//	我们改写后             _
//	重新序列化上线         "_"
//
// 所以：**测试体里嵌入 JSON 字符串时必须用 wire()**。
// 直接写单个反斜杠会得到非法 JSON（Go 报 `invalid escape sequence`）——
// 那是测试数据本身不合法，不是被测代码的问题。（第一版就踩了这个。）
func wire() string { return bs() + bs() }

// runNormalize 走真实入口（Prepare），确保挂载点也对。
func runNormalize(t *testing.T, body string) (map[string]any, Result) {
	t.Helper()
	res := Prepare([]byte(body), Options{PromptMode: PromptPassthrough})
	if res.Body == nil {
		t.Fatalf("Prepare 返回空 body（输入: %s）", body)
	}
	var obj map[string]any
	if err := json.Unmarshal(res.Body, &obj); err != nil {
		t.Fatalf("产物不是合法 JSON: %v（产物: %s）", err, res.Body)
	}
	return obj, res
}

// patternOf 取 tools[0].function.parameters.properties.<name>.pattern（已解码）。
func patternOf(t *testing.T, obj map[string]any, name string) string {
	t.Helper()
	tools, _ := obj["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("产物里没有 tools")
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	p, _ := props[name].(map[string]any)
	got, _ := p["pattern"].(string)
	return got
}

// 上游实案：ZCode 的 exa 插件 `agent_run` 工具，`runId` 的 pattern 是 `^agent\_run\_`。
// deepseek 系全家确定性 400（code=11129）—— 这条是本次改写的**起因**。
func TestToolPatternRealUpstreamCase(t *testing.T) {
	body := `{"model":"deepseek-v4","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"agent_run","parameters":{` +
		`"type":"object","properties":{` +
		`"runId":{"type":"string","pattern":"^agent` + wire() + `_run` + wire() + `_"},` +
		`"previousRunId":{"type":"string","pattern":"^agent` + wire() + `_run` + wire() + `_"}` +
		`}}}}]}`

	obj, res := runNormalize(t, body)

	if got, want := patternOf(t, obj, "runId"), "^agent_run_"; got != want {
		t.Fatalf("runId.pattern 应为 %q，实际 %q", want, got)
	}
	if got, want := patternOf(t, obj, "previousRunId"), "^agent_run_"; got != want {
		t.Fatalf("previousRunId.pattern 应为 %q，实际 %q", want, got)
	}
	if res.ToolPatternsFixed != 2 {
		t.Fatalf("应修掉 2 处，实际 %d", res.ToolPatternsFixed)
	}
	// 产物里不能再出现那个非标转义（否则上游照样 400）。
	if strings.Contains(string(res.Body), wire()) {
		t.Fatalf("产物里仍残留线上形式的反斜杠: %s", res.Body)
	}
}

// `\\_` **不能**被改。
//
// 解码后是「两个反斜杠 + 下划线」= 正则「一个字面反斜杠」+「一个字面下划线」，
// **本身完全合法**。朴素替换子串 `\_` 会吃掉一个反斜杠，
// 语义被悄悄改成「一个字面下划线」—— 调用方完全看不出来。
func TestToolPatternEvenBackslashRunUntouched(t *testing.T) {
	decoded := bs() + bs() + "_" // 正则：\\ 后跟 _
	body := `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function",` +
		`"function":{"name":"f","parameters":{"type":"object","properties":{` +
		`"p":{"type":"string","pattern":"` + wire() + wire() + `_"}}}}}]}`

	obj, res := runNormalize(t, body)

	if got := patternOf(t, obj, "p"); got != decoded {
		t.Fatalf("偶数个反斜杠后跟下划线不该被改：want %q, got %q", decoded, got)
	}
	if res.ToolPatternsFixed != 0 {
		t.Fatalf("不该修任何处，实际 %d", res.ToolPatternsFixed)
	}
}

// 消息正文里的 `\_` **绝不碰** —— 反例：Windows 路径 `C:\_x` 是合法内容。
func TestToolPatternLeavesMessageContentAlone(t *testing.T) {
	want := "路径是 C:" + bs() + "_x" + bs() + "_y"
	body := `{"messages":[{"role":"user","content":"路径是 C:` + wire() + `_x` + wire() + `_y"}],` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object",` +
		`"properties":{"p":{"type":"string","pattern":"^a` + wire() + `_b"}}}}}]}`

	obj, res := runNormalize(t, body)

	msgs, _ := obj["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	if got, _ := m["content"].(string); got != want {
		t.Fatalf("消息正文被改动了：want %q, got %q", want, got)
	}
	// 而 tools 里的该改还是要改。
	if got, wantP := patternOf(t, obj, "p"), "^a_b"; got != wantP {
		t.Fatalf("tools 里的 pattern 应被修：want %q, got %q", wantP, got)
	}
	if res.ToolPatternsFixed != 1 {
		t.Fatalf("应只修 1 处，实际 %d", res.ToolPatternsFixed)
	}
}

// 其余非标转义（`\:`）**不动** —— 未证实会触发，有实案再议。
func TestToolPatternLeavesOtherEscapesAlone(t *testing.T) {
	decoded := "^a" + bs() + ":b"
	body := `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function",` +
		`"function":{"name":"f","parameters":{"type":"object","properties":{` +
		`"p":{"type":"string","pattern":"^a` + wire() + `:b"}}}}}]}`

	obj, res := runNormalize(t, body)
	if got := patternOf(t, obj, "p"); got != decoded {
		t.Fatalf("`\\:` 不该被改：want %q, got %q", decoded, got)
	}
	if res.ToolPatternsFixed != 0 {
		t.Fatalf("不该修任何处，实际 %d", res.ToolPatternsFixed)
	}
}

// 裸 `tools[].parameters`（不带 function 包装）也要覆盖。
func TestToolPatternBareParametersForm(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function",` +
		`"parameters":{"type":"object","properties":{"p":{"type":"string","pattern":"^a` + wire() + `_b"}}}}]}`

	obj, res := runNormalize(t, body)
	tools, _ := obj["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	params, _ := tool["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	p, _ := props["p"].(map[string]any)
	if got, want := p["pattern"], "^a_b"; got != want {
		t.Fatalf("裸 parameters 形态应被修：want %q, got %q", want, got)
	}
	if res.ToolPatternsFixed != 1 {
		t.Fatalf("应修 1 处，实际 %d", res.ToolPatternsFixed)
	}
}

// `patternProperties` 的**键**也要修，且命中时要重建该层（键不能原地改）。
func TestToolPatternPatternPropertiesKeys(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function",` +
		`"function":{"name":"f","parameters":{"type":"object","patternProperties":{` +
		`"^a` + wire() + `_b$":{"type":"string"}}}}}]}`

	obj, res := runNormalize(t, body)
	tools, _ := obj["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	params, _ := fn["parameters"].(map[string]any)
	pp, _ := params["patternProperties"].(map[string]any)

	if _, ok := pp["^a_b$"]; !ok {
		t.Fatalf("patternProperties 的键应被修正为 ^a_b$，实际键: %v", keysOf(pp))
	}
	if _, stale := pp["^a"+bs()+"_b$"]; stale {
		t.Fatalf("旧键仍在（没有重建该层）: %v", keysOf(pp))
	}
	if res.ToolPatternsFixed != 1 {
		t.Fatalf("应修 1 处，实际 %d", res.ToolPatternsFixed)
	}
}

// 深层嵌套（$defs / items / allOf）里的 pattern 也要覆盖。
//
// 这一条**用 map + json.Marshal 构造**而不是手写 JSON 文本：
// 手写时括号层级极难数对（第一版就多了一个 `}`），而且 json.Marshal
// 会自动把反斜杠转义成线上形式，正好省掉 wire()/bs() 的心智负担。
func TestToolPatternNestedSchemas(t *testing.T) {
	pat := func(s string) map[string]any { return map[string]any{"type": "string", "pattern": s} }

	params := map[string]any{
		"type": "object",
		"$defs": map[string]any{
			"inner": map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": pat("^x" + bs() + "_y")},
			},
		},
		"properties": map[string]any{
			"list": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":       "object",
					"properties": map[string]any{"b": pat("^p" + bs() + "_q")},
				},
			},
			"combo": map[string]any{
				"allOf": []any{pat("^m" + bs() + "_n")},
			},
		},
	}

	body, err := json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "f", "parameters": params},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	obj, res := runNormalize(t, string(body))
	if res.ToolPatternsFixed != 3 {
		t.Fatalf("$defs/items/allOf 共应修 3 处，实际 %d", res.ToolPatternsFixed)
	}

	// 逐处核对都真的被改了。
	tools, _ := obj["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	p, _ := fn["parameters"].(map[string]any)

	defs, _ := p["$defs"].(map[string]any)
	inner, _ := defs["inner"].(map[string]any)
	innerProps, _ := inner["properties"].(map[string]any)
	if got := innerProps["a"].(map[string]any)["pattern"]; got != "^x_y" {
		t.Fatalf("$defs 里的 pattern 未修：%v", got)
	}
	props, _ := p["properties"].(map[string]any)
	list, _ := props["list"].(map[string]any)
	items, _ := list["items"].(map[string]any)
	itemProps, _ := items["properties"].(map[string]any)
	if got := itemProps["b"].(map[string]any)["pattern"]; got != "^p_q" {
		t.Fatalf("items 里的 pattern 未修：%v", got)
	}
	combo, _ := props["combo"].(map[string]any)
	allOf, _ := combo["allOf"].([]any)
	if got := allOf[0].(map[string]any)["pattern"]; got != "^m_n" {
		t.Fatalf("allOf 里的 pattern 未修：%v", got)
	}
}

// 没有 tools 时不该 panic、不该改动、计数为 0。
func TestToolPatternNoToolsIsNoop(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"` + wire() + `_ 原样"}]}`
	obj, res := runNormalize(t, body)
	if res.ToolPatternsFixed != 0 {
		t.Fatalf("没有 tools 时不该有修正，实际 %d", res.ToolPatternsFixed)
	}
	msgs, _ := obj["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	if got, _ := m["content"].(string); got != bs()+"_ 原样" {
		t.Fatalf("没有 tools 时正文不该被改：%q", got)
	}
}

// enum / default / description 里的反斜杠**不是正则**，不能碰。
func TestToolPatternDoesNotTouchNonPatternFields(t *testing.T) {
	decoded := "a" + bs() + "_b"
	body := `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function",` +
		`"function":{"name":"f","parameters":{"type":"object","properties":{` +
		`"p":{"type":"string","enum":["a` + wire() + `_b"],"default":"a` + wire() + `_b",` +
		`"description":"a` + wire() + `_b"}}}}}]}`

	obj, res := runNormalize(t, body)
	if res.ToolPatternsFixed != 0 {
		t.Fatalf("enum/default/description 不是正则，不该被改，实际修了 %d 处", res.ToolPatternsFixed)
	}
	tools, _ := obj["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	params, _ := fn["parameters"].(map[string]any)
	props, _ := params["properties"].(map[string]any)
	p, _ := props["p"].(map[string]any)
	enum, _ := p["enum"].([]any)
	if got, _ := enum[0].(string); got != decoded {
		t.Fatalf("enum 被改动了：want %q, got %q", decoded, got)
	}
}

// 改写必须**独立于指纹脱敏开关**：这是「让请求通过」，不是脱敏。
func TestToolPatternIndependentOfSanitize(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function",` +
		`"function":{"name":"f","parameters":{"type":"object","properties":{` +
		`"p":{"type":"string","pattern":"^a` + wire() + `_b"}}}}}]}`

	res := Prepare([]byte(body), Options{PromptMode: PromptPassthrough, Sanitize: false})
	var obj map[string]any
	if err := json.Unmarshal(res.Body, &obj); err != nil {
		t.Fatal(err)
	}
	if res.ToolPatternsFixed != 1 {
		t.Fatalf("脱敏关闭时也应修 pattern，实际 %d", res.ToolPatternsFixed)
	}
	if got := patternOf(t, obj, "p"); got != "^a_b" {
		t.Fatalf("want ^a_b, got %q", got)
	}
}

// 单独测底层函数：反斜杠奇偶的判定。
func TestUnescapeLiteralUnderscoreOddEven(t *testing.T) {
	b := bs()
	cases := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{"单个转义下划线", "a" + b + "_b", "a_b", true},
		{"两个反斜杠后跟下划线（合法，不动）", b + b + "_", b + b + "_", false},
		{"三个反斜杠后跟下划线（前两个成对 + 第三个转义）", b + b + b + "_", b + b + "_", true},
		{"四个反斜杠后跟下划线（不动）", b + b + b + b + "_", b + b + b + b + "_", false},
		{"转义的是别的字符", "a" + b + ":b", "a" + b + ":b", false},
		{"转义下划线在中间", "^x" + b + "_y$", "^x_y$", true},
		{"多个独立出现", b + "_a" + b + "_b", "_a_b", true},
		{"无反斜杠", "abc", "abc", false},
		{"反斜杠在结尾", "ab" + b, "ab" + b, false},
		{"空串", "", "", false},
		{"只有下划线", "_", "_", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := unescapeLiteralUnderscore(c.in)
			if got != c.want || changed != c.changed {
				t.Fatalf("in=%q want=(%q,%v) got=(%q,%v)", c.in, c.want, c.changed, got, changed)
			}
		})
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
