package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConvertResponsesToolOutputKeepsOldBehaviourWhenNotMultimodal 守住保守边界。
//
// 只有「数组里带已知类型标签」才当多模态处理。普通字符串、普通对象数组
// （例如 [{a:1},{b:2}]）必须原样走老的序列化路径 —— 否则一个业务上本来就是
// JSON 数组的工具结果会被拆得面目全非。
func TestConvertResponsesToolOutputKeepsOldBehaviourWhenNotMultimodal(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string
	}{
		{"字符串", "纯文本结果", "纯文本结果"},
		{"空值", nil, ""},
		{
			"普通对象数组（无类型标签）",
			[]any{map[string]any{"a": float64(1)}, map[string]any{"b": float64(2)}},
			`[{"a":1},{"b":2}]`,
		},
		{
			"普通对象",
			map[string]any{"ok": true},
			`{"ok":true}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, images := convertResponsesToolOutput(c.input)
			if len(images) != 0 {
				t.Errorf("不该产出图片块，实际 %d 个", len(images))
			}
			if text != c.want {
				t.Errorf("文本应为 %q，实际 %q", c.want, text)
			}
		})
	}
}

// TestConvertResponsesToolOutputLiftsImages 是这次修复的核心。
//
// 老实现把整个 output 一律 stringify，于是图片的 Base64 被当成普通文本塞进
// tool 消息：模型看不到图，token 却按 Base64 的长度照算。
// 新实现把图片块提升成多模态内容，由调用方接一条 user 消息。
func TestConvertResponsesToolOutputLiftsImages(t *testing.T) {
	t.Run("图片以字符串给出", func(t *testing.T) {
		text, images := convertResponsesToolOutput([]any{
			map[string]any{"type": "input_text", "text": "这是截图"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
		})
		if text != "这是截图" {
			t.Errorf("文本应只保留文字块，实际 %q", text)
		}
		if len(images) != 1 {
			t.Fatalf("应提升 1 个图片块，实际 %d 个", len(images))
		}
		// Base64 不该出现在文本里 —— 那正是老实现的毛病。
		if strings.Contains(text, "base64") {
			t.Error("文本里不该出现 Base64 数据")
		}
		blob, _ := json.Marshal(images[0])
		if !strings.Contains(string(blob), "data:image/png;base64,AAAA") {
			t.Errorf("图片块应带上原始 url，实际 %s", blob)
		}
	})

	t.Run("图片以嵌套对象给出并带 detail", func(t *testing.T) {
		_, images := convertResponsesToolOutput([]any{
			map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "https://example.com/a.png", "detail": "high"},
			},
		})
		if len(images) != 1 {
			t.Fatalf("应提升 1 个图片块，实际 %d 个", len(images))
		}
		blob, _ := json.Marshal(images[0])
		for _, want := range []string{"https://example.com/a.png", "high"} {
			if !strings.Contains(string(blob), want) {
				t.Errorf("图片块应含 %q，实际 %s", want, blob)
			}
		}
	})

	t.Run("只有图片没有文字时给占位", func(t *testing.T) {
		text, images := convertResponsesToolOutput([]any{
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,BBBB"},
		})
		if text == "" {
			t.Error("content 为空串会被部分上游判成非法请求，应给占位文本")
		}
		if len(images) != 1 {
			t.Errorf("应提升 1 个图片块，实际 %d 个", len(images))
		}
	})
}

// TestConvertResponsesToolOutputDoesNotDropUnknownBlocks 守住「不静默丢数据」。
//
// 已知类型之外的块仍按 JSON 文本保留 —— 工具结果里出现没见过的块类型时，
// 悄悄丢掉比报错更糟：调用方拿不到任何线索。
func TestConvertResponsesToolOutputDoesNotDropUnknownBlocks(t *testing.T) {
	text, _ := convertResponsesToolOutput([]any{
		map[string]any{"type": "input_text", "text": "已知块"},
		map[string]any{"type": "未来才有的块", "payload": "别丢我"},
	})
	if !strings.Contains(text, "已知块") {
		t.Errorf("已知文本块应保留，实际 %q", text)
	}
	if !strings.Contains(text, "别丢我") {
		t.Errorf("未知块应保留成 JSON 文本，实际 %q", text)
	}
}
