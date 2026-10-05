package server

import (
	"strings"
	"testing"
)

// 导入解析必须「宽容」：用户拿着本项目导出的文件、拿着 wb-switch 导出的文件、
// 或者只导出了一个账号的对象，都应该能导进来。漏认一种的表现是
// 「导入后没有 token」，而用户完全无从判断是文件坏了还是工具不认。
func TestParseImportFile(t *testing.T) {
	t.Run("本项目导出格式（app + accounts）", func(t *testing.T) {
		raw := []byte(`{"app":"workbuddy-gateway","version":"0.9.0","count":1,
			"accounts":[{"id":"workbuddy-1001.json","uid":"1001","nickname":"甲",
			"site":"cn","domain":"codebuddy.cn","access_token":"at-1",
			"refresh_token":"rt-1","expiresAt":1800000000}]}`)
		items, err := parseImportFile(raw)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("应解析出 1 项，实际 %d", len(items))
		}
		it := items[0]
		if it.uid != "1001" || it.accessToken != "at-1" || it.refreshToken != "rt-1" {
			t.Fatalf("字段读取错误: %+v", it)
		}
		if it.expiresAt != 1800000000 {
			t.Fatalf("expiresAt 读取错误: %d", it.expiresAt)
		}
		if it.site == "" {
			t.Fatal("站点未归一化")
		}
		if it.parseErr != "" {
			t.Fatalf("不该有解析错误: %s", it.parseErr)
		}
	})

	t.Run("裸数组（wb-switch 的导出形态）", func(t *testing.T) {
		// wb-switch 的 AccountRecord：token 用 snake_case、expiresAt 用 camelCase，
		// 且完整凭据可能放在 auth_raw 里。
		raw := []byte(`[{"id":"x","uid":"2002","nickname":"乙","email":"a@b.c",
			"access_token":"at-2","refresh_token":"rt-2","token_type":"Bearer",
			"domain":"workbuddy.ai","expiresAt":1900000000,"createdAt":1700000000}]`)
		items, err := parseImportFile(raw)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(items) != 1 || items[0].accessToken != "at-2" || items[0].refreshToken != "rt-2" {
			t.Fatalf("wb-switch 形态未读出来: %+v", items)
		}
		if items[0].expiresAt != 1900000000 {
			t.Fatalf("camelCase expiresAt 未读出来: %d", items[0].expiresAt)
		}
	})

	t.Run("token 藏在 auth_raw 里", func(t *testing.T) {
		raw := []byte(`[{"uid":"3003","auth_raw":{"accessToken":"at-3","refreshToken":"rt-3","expiresAt":1850000000}}]`)
		items, err := parseImportFile(raw)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if items[0].accessToken != "at-3" {
			t.Fatalf("auth_raw 未作为回退来源: %+v", items[0])
		}
	})

	t.Run("嵌套 auth/account 形态", func(t *testing.T) {
		raw := []byte(`[{"auth":{"accessToken":"at-4","refreshToken":"rt-4","expiresAt":1810000000},
			"account":{"uid":"4004","nickname":"丁"},"realm":"cn"}]`)
		items, err := parseImportFile(raw)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		it := items[0]
		if it.accessToken != "at-4" || it.uid != "4004" || it.nickname != "丁" {
			t.Fatalf("嵌套形态未读出来: %+v", it)
		}
	})

	t.Run("单个对象也接受", func(t *testing.T) {
		raw := []byte(`{"uid":"5005","access_token":"at-5"}`)
		items, err := parseImportFile(raw)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(items) != 1 || items[0].uid != "5005" {
			t.Fatalf("单对象形态失败: %+v", items)
		}
	})

	t.Run("uid 是数字也要能读", func(t *testing.T) {
		raw := []byte(`[{"uid":6006,"access_token":"at-6"}]`)
		items, err := parseImportFile(raw)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if items[0].uid != "6006" {
			t.Fatalf("数字 uid 应转成字符串 6006，实际 %q", items[0].uid)
		}
		if items[0].parseErr != "" {
			t.Fatalf("不该有解析错误: %s", items[0].parseErr)
		}
	})

	t.Run("缺 token / 缺 uid 要标记而不是静默", func(t *testing.T) {
		raw := []byte(`[{"uid":"7007"},{"access_token":"at-8"}]`)
		items, err := parseImportFile(raw)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if items[0].parseErr == "" {
			t.Fatal("缺 token 的项必须带 parseErr")
		}
		if items[1].parseErr == "" {
			t.Fatal("缺 uid 的项必须带 parseErr")
		}
	})

	t.Run("字符串形式的时间戳也要认", func(t *testing.T) {
		raw := []byte(`[{"uid":"8008","access_token":"at-9","expires_at":"1830000000"}]`)
		items, err := parseImportFile(raw)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if items[0].expiresAt != 1830000000 {
			t.Fatalf("字符串时间戳未读出来: %d", items[0].expiresAt)
		}
	})

	t.Run("无法识别的输入要报错", func(t *testing.T) {
		for _, raw := range []string{`{"foo":"bar"}`, `[]`, `不是 json`, ``, `123`} {
			if _, err := parseImportFile([]byte(raw)); err == nil {
				t.Fatalf("输入 %q 应当报错", raw)
			}
		}
	})
}

// 站点必须归一化，否则 NewForLogin 会把账号写进错误的站点档位。
func TestParseImportItemResolvesSite(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"edition=intl", `{"uid":"1","access_token":"a","edition":"intl"}`},
		{"realm=global", `{"uid":"1","access_token":"a","realm":"global"}`},
		{"domain 后缀", `{"uid":"1","access_token":"a","domain":"workbuddy.ai"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items, err := parseImportFile([]byte("[" + c.raw + "]"))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if items[0].site == "" {
				t.Fatal("site 不应为空")
			}
			if items[0].site != "intl" {
				t.Fatalf("%s 应归一化为 intl，实际 %q", c.name, items[0].site)
			}
		})
	}
}

// 加密凭据（WorkBuddy 5.6 的 $wbEncrypted 信封）必须被识别并如实标记。
//
// 判据与 wb-switch 的 `account::is_envelope` 一致：token 值是对象且含
// "$wbEncrypted" 键。报成「缺少 access_token」会把用户引向「文件丢了 token」
// 这个错误方向；普通对象（没有该键）仍按缺 token 处理，不冒充加密。
func TestParseImportFileEncryptedEnvelope(t *testing.T) {
	envelope := `[{"uid":"9009","nickname":"戊","access_token":{"$wbEncrypted":1,"envelope":"a"},` +
		`"refresh_token":{"$wbEncrypted":1,"envelope":"r"}}]`
	items, err := parseImportFile([]byte(envelope))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !items[0].encrypted {
		t.Fatal("$wbEncrypted 信封应被标记为加密凭据")
	}
	if items[0].accessToken != "" {
		t.Fatalf("加密形态不应解析出明文 token，实际 %q", items[0].accessToken)
	}
	if !strings.Contains(items[0].parseErr, "加密") {
		t.Fatalf("parseErr 应说明是加密凭据，实际 %q", items[0].parseErr)
	}

	// 普通对象不是已知的加密形态：仍按「缺少 access_token」处理。
	plain := `[{"uid":"9009","access_token":{"foo":"bar"}}]`
	items, err = parseImportFile([]byte(plain))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if items[0].encrypted {
		t.Fatal("普通对象不应冒充加密凭据")
	}
	if !strings.Contains(items[0].parseErr, "access_token") {
		t.Fatalf("普通对象应报缺少 access_token，实际 %q", items[0].parseErr)
	}

	// 加密信封但同记录另有明文（旧版 auth_raw）：明文可用，不应标加密。
	mixed := `[{"uid":"9009","access_token":{"$wbEncrypted":1,"envelope":"a"},` +
		`"auth_raw":{"accessToken":"plain-token"}}]`
	items, err = parseImportFile([]byte(mixed))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if items[0].encrypted {
		t.Fatal("已解析出明文 token 时不应标加密")
	}
	if items[0].accessToken != "plain-token" {
		t.Fatalf("应读回 auth_raw 的明文，实际 %q", items[0].accessToken)
	}
}
