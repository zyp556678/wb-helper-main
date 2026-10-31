package config

import (
	"os"
	"path/filepath"
	"testing"
)

// cn_chat_base 覆盖国内站上游 base（对照 wb2api-panel 的 cn.chat_base）。
//
// 两种拼写都接受，snake 优先 —— 与 upstream 段其它字段的口径一致：面板保存写
// snake_case，人工手写的 config.json 历来是 camelCase，只认一种会让另一侧
// 「配了但没生效」。
func TestCNChatBaseParsed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"snake", `{"upstream":{"cn_chat_base":"https://cn.example.com"}}`, "https://cn.example.com"},
		{"camel", `{"upstream":{"cnChatBase":"https://cn2.example.com"}}`, "https://cn2.example.com"},
		{"两者都有时 snake 优先", `{"upstream":{"cn_chat_base":"https://snake.example.com","cnChatBase":"https://camel.example.com"}}`, "https://snake.example.com"},
		{"留空不覆盖", `{"upstream":{}}`, ""},
		{"空白按未配置", `{"upstream":{"cn_chat_base":"   "}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(&Config{WorkDir: dir})
			if err != nil {
				t.Fatalf("加载配置失败: %v", err)
			}
			if cfg.CNChatBase != tc.want {
				t.Errorf("CNChatBase = %q，期望 %q", cfg.CNChatBase, tc.want)
			}
		})
	}
}
