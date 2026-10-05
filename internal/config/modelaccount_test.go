package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfig 在临时目录写一份 config.json 并加载。
func writeConfig(t *testing.T, body string) *Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("写 config.json 失败: %v", err)
	}
	cfg, err := Load(&Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return cfg
}

// TestModelAccountRulesParsed 守住 models.accounts 的解析。
func TestModelAccountRulesParsed(t *testing.T) {
	cfg := writeConfig(t, `{
	  "models": {
	    "blocklist": [],
	    "allowlist": [],
	    "accounts": {
	      "deepseek-v4.1-flash": {
	        "allowlist": ["intl-a.json", "intl-b.json"],
	        "blocklist": ["intl-b.json"]
	      },
	      "hy3": { "blocklist": ["old-account.json"] }
	    }
	  }
	}`)

	rule, ok := cfg.ModelAccountRuleFor("deepseek-v4.1-flash")
	if !ok {
		t.Fatal("未解析出 deepseek-v4.1-flash 的账号名单")
	}
	if !rule.Allow["intl-a.json"] || !rule.Allow["intl-b.json"] {
		t.Fatalf("白名单解析错误: %#v", rule.Allow)
	}
	if !rule.Block["intl-b.json"] {
		t.Fatalf("黑名单解析错误: %#v", rule.Block)
	}

	hy, ok := cfg.ModelAccountRuleFor("hy3")
	if !ok {
		t.Fatal("未解析出 hy3 的账号名单")
	}
	if len(hy.Allow) != 0 {
		t.Fatalf("hy3 未配白名单，应为空: %#v", hy.Allow)
	}
	if !hy.Block["old-account.json"] {
		t.Fatalf("hy3 黑名单解析错误: %#v", hy.Block)
	}
}

// TestModelAccountRuleModelNameNormalized 模型名归一化：大小写与首尾空格都要认。
//
// 严格区分大小写会让「写了规则却不生效」变成静默故障 —— 用户看到的是
// 「我明明配了，怎么没起作用」，而不是任何报错。
func TestModelAccountRuleModelNameNormalized(t *testing.T) {
	cfg := writeConfig(t, `{"models":{"accounts":{"DeepSeek-V4.1-Flash":{"blocklist":["a.json"]}}}}`)

	for _, probe := range []string{"deepseek-v4.1-flash", "DEEPSEEK-V4.1-FLASH", "  DeepSeek-V4.1-Flash  "} {
		if _, ok := cfg.ModelAccountRuleFor(probe); !ok {
			t.Fatalf("模型名 %q 未命中规则", probe)
		}
	}
}

// TestModelAccountRuleEmptyEntryIgnored 两个名单都空的条目视为「没配」。
//
// 若把空条目当成「配了但谁都不允许」，会把该模型的账号全挡掉 ——
// 用户只是留了个空壳，却得到一个模型完全不可用的结果。
func TestModelAccountRuleEmptyEntryIgnored(t *testing.T) {
	cfg := writeConfig(t, `{"models":{"accounts":{"hy3":{"allowlist":[],"blocklist":[]}}}}`)

	if _, ok := cfg.ModelAccountRuleFor("hy3"); ok {
		t.Fatal("空名单条目不应进规则表")
	}
	if len(cfg.ModelAccountRules) != 0 {
		t.Fatalf("规则表应为空，实际 %d 条", len(cfg.ModelAccountRules))
	}
}

// TestModelAccountRuleBlankNamesDropped 名单里的空白项被丢弃，不会变成空串键。
func TestModelAccountRuleBlankNamesDropped(t *testing.T) {
	cfg := writeConfig(t, `{"models":{"accounts":{"hy3":{"blocklist":["  ","real.json",""]}}}}`)

	rule, ok := cfg.ModelAccountRuleFor("hy3")
	if !ok {
		t.Fatal("应解析出规则")
	}
	if len(rule.Block) != 1 || !rule.Block["real.json"] {
		t.Fatalf("空白项应被丢弃，实际 %#v", rule.Block)
	}
}

// TestNoModelAccountRulesIsNil 未配置时不建表（保持 nil，便于调用方零成本判断）。
func TestNoModelAccountRulesIsNil(t *testing.T) {
	cfg := writeConfig(t, `{"models":{"blocklist":[],"allowlist":[]}}`)
	if cfg.ModelAccountRules != nil {
		t.Fatalf("未配置 models.accounts 时不应建表: %#v", cfg.ModelAccountRules)
	}
	if _, ok := cfg.ModelAccountRuleFor("hy3"); ok {
		t.Fatal("未配置时不应命中任何规则")
	}
}

// TestTransientRetriesExplicitZeroKept 显式 0 表示禁用重试，不能被默认值覆盖。
//
// 这条与「保留显式 0」的 firstPtr 配套：用户明确关掉的东西又被打开，
// 且没有任何提示，是最难排查的一类「配置不生效」。
func TestTransientRetriesExplicitZeroKept(t *testing.T) {
	cfg := writeConfig(t, `{"upstream":{"transientRetries":0}}`)
	if cfg.TransientRetries != 0 {
		t.Fatalf("显式 0 应禁用重试，实际 %d", cfg.TransientRetries)
	}

	// 未配置时落到默认值
	def := writeConfig(t, `{}`)
	if def.TransientRetries != DefaultTransientRetries {
		t.Fatalf("未配置时应为默认 %d，实际 %d", DefaultTransientRetries, def.TransientRetries)
	}

	// snake_case 同样生效（面板保存写 snake_case）
	snake := writeConfig(t, `{"upstream":{"transient_retries":5}}`)
	if snake.TransientRetries != 5 {
		t.Fatalf("snake_case 应生效，实际 %d", snake.TransientRetries)
	}
}
