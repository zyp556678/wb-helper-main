package debuglog

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 完整请求日志的价值在正文，风险也在正文。这一组钉住「必须脱敏」的几类：
// 凭据字段、token 形态、URL userinfo / 敏感 query、私钥。
func TestFullBodyFieldsRedactsSecrets(t *testing.T) {
	body := []byte(`{
		"model": "m1",
		"api_key": "sk-abcdefghijklmnopqrstuv",
		"messages": [{"role":"user","content":"我的 key 是 sk-abcdefghijklmnopqrstuv 请记住"}],
		"metadata": {"password": "p@ss", "note": "正常内容"}
	}`)
	fields := FullBodyFields(body)
	raw, err := json.Marshal(fields["body"])
	if err != nil {
		t.Fatalf("脱敏结果无法序列化: %v", err)
	}
	text := string(raw)
	for _, banned := range []string{"sk-abcdefghijklmnopqrstuv", "p@ss"} {
		if strings.Contains(text, banned) {
			t.Fatalf("脱敏后仍出现敏感值 %q: %s", banned, text)
		}
	}
	if !strings.Contains(text, "正常内容") {
		t.Fatalf("普通内容被误删: %s", text)
	}
	if fields["body_bytes"].(int) != len(body) {
		t.Fatalf("body_bytes 应记录原始大小 %d", len(body))
	}
	if _, ok := fields["body_sha256_8"]; !ok {
		t.Fatal("应带正文摘要前缀")
	}
}

func TestRedactTextPatterns(t *testing.T) {
	cases := []string{
		"github_pat_ABCDEFGHIJKLMNOPQRSTUVWX",
		"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345",
		"sk-proj-abcdefghijklmnop",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
	}
	for _, secret := range cases {
		got := RedactText("前缀 " + secret + " 后缀")
		if strings.Contains(got, secret) {
			t.Errorf("未脱敏 %q → %q", secret, got)
		}
		if !strings.HasPrefix(got, "前缀 ") || !strings.HasSuffix(got, " 后缀") {
			t.Errorf("脱敏不应改动无关文本：%q", got)
		}
	}
}

func TestRedactURL(t *testing.T) {
	got := RedactURL("https://user:pass@example.com/v1?token=abc123&q=hello")
	for _, banned := range []string{"user:pass", "abc123"} {
		if strings.Contains(got, banned) {
			t.Errorf("URL 未脱敏 %q：%s", banned, got)
		}
	}
	if !strings.Contains(got, "q=hello") {
		t.Errorf("非敏感 query 应保留：%s", got)
	}
}

func TestRedactValueNested(t *testing.T) {
	var parsed any
	if err := json.Unmarshal([]byte(`{"a":[{"authorization":"Bearer x","v":2}]}`), &parsed); err != nil {
		t.Fatal(err)
	}
	redacted := RedactValue(parsed).(map[string]any)
	list := redacted["a"].([]any)
	item := list[0].(map[string]any)
	if item["authorization"] != "[REDACTED]" {
		t.Fatalf("嵌套 authorization 未被遮蔽: %v", item["authorization"])
	}
	if item["v"] != float64(2) {
		t.Fatalf("普通数值被改动: %v", item["v"])
	}
}

// 开关判定：必须 (a) 打开 full_requests，(b) sink 已启用，(c) 只限模型 API。
func TestFullRequestEnabledGate(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{}"))

	SetFullRequests(false)
	if FullRequestEnabled(r) {
		t.Fatal("full_requests 关闭时不应记录")
	}

	SetFullRequests(true)
	if FullRequestEnabled(r) {
		t.Fatal("debug.enabled（sink）未打开时不应记录")
	}

	dir := t.TempDir()
	closeFn, err := Enable(dir)
	if err != nil {
		t.Fatalf("打开调试事件流失败: %v", err)
	}
	defer closeFn()
	defer SetFullRequests(false)

	if !FullRequestEnabled(r) {
		t.Fatal("两个开关都打开时应记录模型 API")
	}
	if FullRequestEnabled(httptest.NewRequest("POST", "/panel/api/config", strings.NewReader("{}"))) {
		t.Fatal("管理配置接口不应记录正文")
	}
	if FullRequestEnabled(httptest.NewRequest("GET", "/v1/chat/completions", nil)) {
		t.Fatal("非 POST 请求不应记录")
	}
}

// 摘要必须稳定且不是明文：同一个值两次摘要相同，不同值不同。
func TestDigestStableAndOpaque(t *testing.T) {
	a := DigestBytes([]byte(`{"x":1}`))
	b := DigestBytes([]byte(`{"x":1}`))
	c := DigestBytes([]byte(`{"x":2}`))
	if a != b {
		t.Fatal("同一内容摘要应一致")
	}
	if a == c {
		t.Fatal("不同内容摘要应不同")
	}
	if strings.Contains(a, "x") || len(a) != 16 {
		t.Fatalf("摘要形态异常: %q", a)
	}
}

// Apply 是配置热更新调用入口：必须幂等，重复调用不能反复关闭/重开日志文件
// （每次配置保存都会调它一次，重开会让日志句柄与写入位置不断抖动）。
func TestApplyIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	Apply(false, dir)
	if Enabled() {
		t.Fatal("关闭状态应为空")
	}
	if err := Apply(true, dir); err != nil {
		t.Fatalf("开启失败: %v", err)
	}
	defer Apply(false, dir)
	if !Enabled() {
		t.Fatal("开启后应处于启用状态")
	}
	first := Path()
	if err := Apply(true, dir); err != nil {
		t.Fatalf("重复开启失败: %v", err)
	}
	if Path() != first {
		t.Fatalf("重复 Apply 不应重开日志文件：%s → %s", first, Path())
	}
	Apply(false, dir)
	if Enabled() {
		t.Fatal("关闭后应处于停用状态")
	}
}

