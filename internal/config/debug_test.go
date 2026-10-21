package config

import (
	"encoding/json"
	"testing"
)

// debug 段是排障时才手工编辑的，格式容错是刻意的（对照 wb-gateway v1.14.21）：
// 一次笔误不该让整个网关起不来。这一组把常见手写形态全部钉住。
func TestDebugSectionTolerantParsing(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantEnable bool
		wantFull   bool
	}{
		{"正常对象", `{"enabled": true, "full_requests": true}`, true, true},
		{"camelCase 别名", `{"enabled": true, "fullRequests": true}`, true, true},
		{"null", `null`, false, false},
		{"空对象", `{}`, false, false},
		{"空字符串", `""`, false, false},
		{"数字 1", `1`, false, false},
		{"true 字符串", `{"enabled": "true", "full_requests": "on"}`, true, true},
		{"数字开关", `{"enabled": 1, "full_requests": 0}`, true, false},
		{"未知字段忽略", `{"enabled": true, "unknown": {"nested": 1}}`, true, false},
		{"非对象数组", `[]`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d debugFileConfig
			if err := json.Unmarshal([]byte(tc.raw), &d); err != nil {
				t.Fatalf("容错解析不应返回错误: %v", err)
			}
			if d.Enabled != tc.wantEnable || d.FullRequests != tc.wantFull {
				t.Fatalf("解析结果 enabled=%v full=%v，期望 enabled=%v full=%v",
					d.Enabled, d.FullRequests, tc.wantEnable, tc.wantFull)
			}
		})
	}
}

// 整份 config.json 带一个写坏的 debug 段时，必须还能解析出其余配置。
func TestRuntimeFileToleratesBadDebugSection(t *testing.T) {
	raw := `{"debug": "", "prompt": {"mode": "custom"}}`
	var f runtimeFile
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("debug 段写坏不应导致整份配置解析失败: %v", err)
	}
	if f.Prompt.Mode != "custom" {
		t.Fatalf("其余配置应正常解析，实际 prompt.mode=%q", f.Prompt.Mode)
	}
	if f.Debug.Enabled || f.Debug.FullRequests {
		t.Fatalf("写坏的 debug 段应按全 false 处理，实际 %+v", f.Debug)
	}
}

// 完全没有 debug 段时（最常见的手写配置），两个开关都应是默认关闭。
func TestRuntimeFileMissingDebugSection(t *testing.T) {
	var f runtimeFile
	if err := json.Unmarshal([]byte(`{"prompt": {"mode": "append"}}`), &f); err != nil {
		t.Fatalf("缺少 debug 段不应报错: %v", err)
	}
	if f.Debug.Enabled || f.Debug.FullRequests {
		t.Fatalf("缺少 debug 段时应为全 false，实际 %+v", f.Debug)
	}
}
