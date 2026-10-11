package outbound

import (
	"encoding/json"
	"testing"
)

// TestEnsureCatalogMaxTokens 覆盖「未声明输出预算时按目录上限补 max_tokens」。
//
// 背景：上游按「输入 token + 输出预算」判窗口，请求不带 max_tokens 时上游为输出
// 默认预留 384000，标称 1048576 窗口的模型实际只剩约 664576 可用输入，长会话会在
// 远未到窗口上限时被 400 code=11133 拒绝（报错文案不指明具体参数）。
// 补上目录声明的输出上限即可把被默认预留的额度收回来。
func TestEnsureCatalogMaxTokens(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		table    map[string]int
		want     any  // 期望的 max_tokens 值；nil 表示不应存在该字段
		injected int
	}{
		{
			name:     "未声明时按目录补",
			body:     `{"model":"deepseek-v4.1-flash","messages":[]}`,
			table:    map[string]int{"deepseek-v4.1-flash": 128000},
			want:     float64(128000),
			injected: 128000,
		},
		{
			name:  "客户端显式声明时不覆盖",
			body:  `{"model":"deepseek-v4.1-flash","max_tokens":4096}`,
			table: map[string]int{"deepseek-v4.1-flash": 128000},
			want:  float64(4096),
		},
		{
			name:  "显式 0 也不覆盖",
			body:  `{"model":"deepseek-v4.1-flash","max_tokens":0}`,
			table: map[string]int{"deepseek-v4.1-flash": 128000},
			want:  float64(0),
		},
		{
			name:  "max_tokens 为 null 视为未声明",
			body:  `{"model":"deepseek-v4.1-flash","max_tokens":null}`,
			table: map[string]int{"deepseek-v4.1-flash": 128000},
			want:  float64(128000), injected: 128000,
		},
		{
			name:  "目录未收录该模型时不猜",
			body:  `{"model":"unknown-model","messages":[]}`,
			table: map[string]int{"deepseek-v4.1-flash": 128000},
			want:  nil,
		},
		{
			name:  "表为空时不做任何事",
			body:  `{"model":"deepseek-v4.1-flash","messages":[]}`,
			table: nil,
			want:  nil,
		},
		{
			name:  "目录上限为 0 时不补",
			body:  `{"model":"deepseek-v4.1-flash","messages":[]}`,
			table: map[string]int{"deepseek-v4.1-flash": 0},
			want:  nil,
		},
		{
			name:  "别名 max_completion_tokens 已翻译时不覆盖",
			body:  `{"model":"deepseek-v4.1-flash","max_completion_tokens":8192}`,
			table: map[string]int{"deepseek-v4.1-flash": 128000},
			want:  float64(8192),
		},
		{
			name:  "别名 null 时视为未声明并补",
			body:  `{"model":"deepseek-v4.1-flash","max_completion_tokens":null}`,
			table: map[string]int{"deepseek-v4.1-flash": 128000},
			want:  float64(128000), injected: 128000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Prepare([]byte(tc.body), Options{MaxOutputTokens: tc.table})
			var obj map[string]any
			if err := json.Unmarshal(res.Body, &obj); err != nil {
				t.Fatalf("改写结果不是合法 JSON: %v", err)
			}
			got, has := obj["max_tokens"]
			if tc.want == nil {
				if has {
					t.Fatalf("不应注入 max_tokens，实际注入 %v", got)
				}
			} else {
				if !has {
					t.Fatalf("应注入 max_tokens=%v，实际缺失", tc.want)
				}
				if got != tc.want {
					t.Fatalf("max_tokens=%v，期望 %v", got, tc.want)
				}
			}
			if res.MaxTokensInjected != tc.injected {
				t.Errorf("MaxTokensInjected=%d，期望 %d", res.MaxTokensInjected, tc.injected)
			}
		})
	}
}

// TestCatalogMaxTokensDisabledWhenNil 表为空（等价于开关关闭）时，行为必须与改动前
// 完全一致：不注入、Result 也不报注入。上游随后自行决定默认预算。
func TestCatalogMaxTokensDisabledWhenNil(t *testing.T) {
	res := Prepare([]byte(`{"model":"deepseek-v4.1-flash","messages":[]}`), Options{})
	var obj map[string]any
	if err := json.Unmarshal(res.Body, &obj); err != nil {
		t.Fatalf("改写结果不是合法 JSON: %v", err)
	}
	if _, has := obj["max_tokens"]; has {
		t.Fatalf("开关关闭时不应注入 max_tokens")
	}
	if res.MaxTokensInjected != 0 {
		t.Errorf("开关关闭时 MaxTokensInjected 应为 0，实际 %d", res.MaxTokensInjected)
	}
}
