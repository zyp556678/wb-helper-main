package outbound

import (
	"encoding/json"
	"testing"
)

// TestClampGPTMinMaxTokens 覆盖 GPT 系 max_tokens 下限。
//
// 背景：上游 GPT 系对 max_tokens < 16 一律 400 code=11133（15 拒、16 过，同号同 body
// 对照）；Claude Code 切模型时会发极小上限的探针，全号轮转同样被拒 → 客户端 503，
// 模型永远切不过去。这是模型参数级拒绝，换账号无用，只能在发送前抬到下限。
func TestClampGPTMinMaxTokens(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		maxTokens string // 原始 JSON 片段（空串表示不带该字段）
		want      string
		clamped   bool
	}{
		{name: "GPT 系过小抬到下限", model: "gpt-6-sol", maxTokens: `"max_tokens":5`, want: `"max_tokens":16`, clamped: true},
		{name: "GPT 系 0 也抬", model: "gpt-6-luna", maxTokens: `"max_tokens":0`, want: `"max_tokens":16`, clamped: true},
		{name: "已达下限不动", model: "gpt-5.6-sol", maxTokens: `"max_tokens":16`, want: `"max_tokens":16`},
		{name: "大于下限不动", model: "gpt-5.6-sol", maxTokens: `"max_tokens":4096`, want: `"max_tokens":4096`},
		{name: "非 GPT 模型不动", model: "hy4", maxTokens: `"max_tokens":1`, want: `"max_tokens":1`},
		{name: "非 GPT 的类 GPT 名不动", model: "deepseek-v4.1-flash", maxTokens: `"max_tokens":2`, want: `"max_tokens":2`},
		{name: "未携带字段不注入", model: "gpt-6-sol", maxTokens: ``, want: ``},
		{name: "非数值不猜", model: "gpt-6-sol", maxTokens: `"max_tokens":"5"`, want: `"max_tokens":"5"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"` + tc.model + `"`
			if tc.maxTokens != "" {
				body += "," + tc.maxTokens
			}
			body += `}`

			res := Prepare([]byte(body), Options{})
			var obj map[string]any
			if err := json.Unmarshal(res.Body, &obj); err != nil {
				t.Fatalf("改写结果不是合法 JSON: %v", err)
			}
			if tc.maxTokens == "" {
				if _, has := obj["max_tokens"]; has {
					t.Fatalf("未携带字段时不应注入 max_tokens：%v", obj["max_tokens"])
				}
			} else {
				got, _ := json.Marshal(map[string]any{"max_tokens": obj["max_tokens"]})
				if string(got) != `{`+tc.want+`}` {
					t.Fatalf("max_tokens 结果不符：%s", got)
				}
			}
			if res.MaxTokensClamped != tc.clamped {
				t.Errorf("MaxTokensClamped=%t，期望 %t", res.MaxTokensClamped, tc.clamped)
			}
			if tc.clamped && res.MaxTokensTo != gptMinMaxTokens {
				t.Errorf("MaxTokensTo=%d，期望 %d", res.MaxTokensTo, gptMinMaxTokens)
			}
		})
	}
}

// TestClampGPTMinMaxTokensAfterAliasTranslate 别名翻译必须先于下限抬升：
// 只发 max_completion_tokens 的新客户端同样要吃到下限保护。
func TestClampGPTMinMaxTokensAfterAliasTranslate(t *testing.T) {
	res := Prepare([]byte(`{"model":"gpt-6-sol","max_completion_tokens":3}`), Options{})
	var obj map[string]any
	if err := json.Unmarshal(res.Body, &obj); err != nil {
		t.Fatalf("改写结果不是合法 JSON: %v", err)
	}
	if v, _ := obj["max_tokens"].(float64); int64(v) != gptMinMaxTokens {
		t.Fatalf("别名翻译后的 max_tokens=%v，期望 %d", obj["max_tokens"], gptMinMaxTokens)
	}
	if _, has := obj["max_completion_tokens"]; has {
		t.Errorf("别名应被删除（上游不认 max_completion_tokens）")
	}
	if !res.MaxTokensClamped {
		t.Errorf("别名翻译后过小的值也应被抬升")
	}
}
