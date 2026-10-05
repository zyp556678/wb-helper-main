package localapps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupHome 建一个隔离的 HOME + %LOCALAPPDATA%，并把包内路径解析指过去。
//
// 不这么做的话，跑一次单测就会往用户真实的 `%LOCALAPPDATA%\wb-gateway\local-app-backups`
// 里写备份，也会去读真实的 `~/.codebuddy/settings.json`。
func setupHome(t *testing.T) (home, localAppData string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	localAppData = filepath.Join(root, "local")
	for _, d := range []string{home, localAppData} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	prev := testPathOverride
	testPathOverride = &pathOverride{Home: home, LocalAppData: localAppData}
	t.Cleanup(func() { testPathOverride = prev })
	return home, localAppData
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s 不是有效 JSON: %v", path, err)
	}
	return doc
}

// CLI 切换必须同时写 token 与档位标记，并**保留 hooks 等我们不懂的键**。
// 丢掉 hooks 会静默破坏用户的既有配置 —— 这是最容易被忽略的一类破坏。
func TestSwitchCLIPreservesUnknownKeysAndWritesBothSites(t *testing.T) {
	cases := []struct {
		name           string
		site           string
		wantEnv        string
		wantBaseURL    string // 空串表示该键应当**不存在**
		wantBaseAbsent bool
	}{
		{"国内站", "cn", cliEnvCN, "", true},
		{"国际站", "intl", cliEnvAI, cliBaseURLAI, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home, _ := setupHome(t)
			settings := cliSettingsPathOf(home)

			// 先造一份带 hooks（模拟本机真实状态）和**上一个档位残留的 BASE_URL**。
			writeFile(t, settings, `{
  "hooks": {"Stop": [{"hooks": [{"command": "bash hook.sh", "type": "command"}]}]},
  "env": {"CODEBUDDY_BASE_URL": "https://www.codebuddy.ai/v2", "KEEP_ME": "1"}
}`)

			notes, err := SwitchCLI(home, PoolAccount{
				ID: "workbuddy-a.json", UID: "uid-a", Nickname: "甲",
				Site: c.site, AccessToken: "Bearer tok-abc123",
			})
			if err != nil {
				t.Fatalf("切换失败: %v", err)
			}
			if len(notes) == 0 {
				t.Fatal("应返回逐条说明")
			}

			doc := readJSON(t, settings)

			// hooks 必须原样在。
			if _, ok := doc["hooks"]; !ok {
				t.Fatal("hooks 被丢掉了 —— 这属于破坏用户既有配置")
			}
			env, _ := doc["env"].(map[string]any)
			if env == nil {
				t.Fatal("env 不存在")
			}
			// token 应写入，且**去掉 Bearer 前缀**（带前缀会让 CLI 认证失败）。
			if got, _ := env["CODEBUDDY_AUTH_TOKEN"].(string); got != "tok-abc123" {
				t.Fatalf("token 应为去前缀的 tok-abc123，实际 %q", got)
			}
			if got, _ := env["CODEBUDDY_INTERNET_ENVIRONMENT"].(string); got != c.wantEnv {
				t.Fatalf("档位标记应为 %q，实际 %q", c.wantEnv, got)
			}
			if got, _ := env["KEEP_ME"].(string); got != "1" {
				t.Fatal("env 里的其他键被丢掉了")
			}
			base, hasBase := env["CODEBUDDY_BASE_URL"]
			if c.wantBaseAbsent {
				if hasBase {
					t.Fatalf("国内站必须删除 CODEBUDDY_BASE_URL，实际残留 %v", base)
				}
			} else {
				if !hasBase || base != c.wantBaseURL {
					t.Fatalf("国际站 BASE_URL 应为 %q，实际 %v", c.wantBaseURL, base)
				}
			}
		})
	}
}

// 写之前必须留下备份，且备份内容 = 写入前的原文（否则恢复等于没恢复）。
func TestSwitchCLIBackupCapturesOriginal(t *testing.T) {
	home, _ := setupHome(t)
	settings := cliSettingsPathOf(home)
	original := `{"hooks":{"Stop":[]},"env":{"CODEBUDDY_AUTH_TOKEN":"OLD"}}`
	writeFile(t, settings, original)

	if _, err := SwitchCLI(home, PoolAccount{
		ID: "a.json", UID: "u", Site: "cn", AccessToken: "NEW",
	}); err != nil {
		t.Fatal(err)
	}

	backups, err := ListBackups(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("应产生 1 份备份，实际 %d", len(backups))
	}
	if backups[0].Target != TargetCLI {
		t.Fatalf("备份目标应为 CLI，实际 %s", backups[0].Target)
	}

	// 恢复后必须回到原文（含旧 token）。
	notes, err := RestoreBackup(backups[0].ID)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if len(notes) == 0 {
		t.Fatal("恢复应返回逐文件说明")
	}
	doc := readJSON(t, settings)
	env, _ := doc["env"].(map[string]any)
	if got, _ := env["CODEBUDDY_AUTH_TOKEN"].(string); got != "OLD" {
		t.Fatalf("恢复后 token 应为 OLD，实际 %q", got)
	}
}

// 备份时**不存在**的文件，恢复后必须仍然不存在。
//
// 这条容易写错：若恢复时无脑写回「空内容」，就会在原本不存在的路径留下
// 一个 0 字节文件，目标程序读到的是「空的登录态」—— 比没有更糟。
func TestRestoreDeletesFilesThatDidNotExist(t *testing.T) {
	home, _ := setupHome(t)
	settings := cliSettingsPathOf(home)
	// 刻意不创建 settings.json（模拟首次配置）。
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Fatal("前置条件错误：文件不该存在")
	}

	if _, err := SwitchCLI(home, PoolAccount{
		ID: "a.json", UID: "u", Site: "cn", AccessToken: "NEW",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settings); err != nil {
		t.Fatal("切换后应已创建 settings.json")
	}

	backups, _ := ListBackups(10)
	if len(backups) != 1 {
		t.Fatalf("应有 1 份备份，实际 %d", len(backups))
	}
	if _, err := RestoreBackup(backups[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Fatal("恢复后文件应回到「不存在」的状态，而不是留一个空文件")
	}
}

// 现有 settings.json 坏掉时必须**拒绝写入**而不是覆盖它 ——
// 覆盖会把用户原本还能手工修好的配置直接毁掉。
func TestSwitchCLIRefusesToOverwriteBrokenJSON(t *testing.T) {
	home, _ := setupHome(t)
	settings := cliSettingsPathOf(home)
	writeFile(t, settings, `{ this is not json `)

	_, err := SwitchCLI(home, PoolAccount{ID: "a.json", UID: "u", Site: "cn", AccessToken: "T"})
	if err == nil {
		t.Fatal("坏 JSON 时应报错")
	}
	if !strings.Contains(err.Error(), "有效 JSON") {
		t.Fatalf("错误信息应说明原因，实际: %v", err)
	}
	// 原文件必须**一个字节都没变**。
	raw, _ := os.ReadFile(settings)
	if string(raw) != `{ this is not json ` {
		t.Fatal("拒绝写入时不该改动原文件")
	}
}

// 站点未知时必须拒绝：写错档位会让 CLI 打错站点，而表面看起来「切换成功」。
func TestSwitchCLIRejectsUnknownSite(t *testing.T) {
	home, _ := setupHome(t)
	_, err := SwitchCLI(home, PoolAccount{ID: "a.json", UID: "u", Site: "", AccessToken: "T"})
	if err == nil {
		t.Fatal("站点为空时应报错")
	}
}

// 空 token 必须拒绝：写一个空 token 会让 CLI 静默变成未登录。
func TestSwitchCLIRejectsEmptyToken(t *testing.T) {
	home, _ := setupHome(t)
	_, err := SwitchCLI(home, PoolAccount{ID: "a.json", UID: "u", Site: "cn", AccessToken: "   "})
	if err == nil {
		t.Fatal("空 token 时应报错")
	}
}

// Bearer 前缀的比较口径：带前缀与不带前缀的**同一个 token** 必须判为同一个账号，
// 否则界面会把「明明就是池里那个账号」显示成「不在账号池中」。
func TestCleanTokenAndSameSecret(t *testing.T) {
	if cleanToken("Bearer abc") != "abc" {
		t.Fatal("应去掉 Bearer 前缀")
	}
	if cleanToken("  bearer   abc  ") != "abc" {
		t.Fatal("应忽略大小写与空白")
	}
	if cleanToken("abc") != "abc" {
		t.Fatal("无前缀时应原样返回")
	}
	if !sameSecret("Bearer abc", "abc") {
		t.Fatal("带前缀与不带前缀应判为相同")
	}
	if sameSecret("", "") {
		t.Fatal("两个空值不应判为相同（否则未配置会被误认为匹配）")
	}
	if sameSecret("abc", "abd") {
		t.Fatal("不同 token 不应判为相同")
	}
}

// WorkBuddy：目标账号在客户端账号列表里且有信封 → 复用，不写明文。
func TestSwitchWorkBuddyReusesEnvelope(t *testing.T) {
	home, local := setupHome(t)
	_ = home
	path := workBuddyAuthPath("cn")
	envelope := map[string]any{"$wbEncrypted": 1, "envelope": "AAA"}
	writeFile(t, path, `{
  "account": {"uid": "u1", "nickname": {"$wbEncrypted":1,"envelope":"NNN"}},
  "auth": {"accessToken": {"$wbEncrypted":1,"envelope":"AAA"}, "tokenType":"Bearer"},
  "accounts": [{"uid":"u2","accessToken":{"$wbEncrypted":1,"envelope":"BBB"}}],
  "allAccounts": [{"uid":"u2","accessToken":{"$wbEncrypted":1,"envelope":"BBB"}}]
}`)
	_ = envelope
	_ = local

	// 切到 u2：它在 allAccounts 里带信封 → 不该要求 allowPlaintext。
	notes, err := SwitchWorkBuddy("cn", PoolAccount{
		ID: "b.json", UID: "u2", Nickname: "乙", Site: "cn", AccessToken: "plaintext-token",
	}, false)
	if err != nil {
		t.Fatalf("有信封可复用时应成功，实际: %v", err)
	}
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "复用") {
		t.Fatalf("应说明复用了加密凭据，实际: %s", joined)
	}

	doc := readJSON(t, path)
	acc, _ := doc["account"].(map[string]any)
	if got, _ := acc["uid"].(string); got != "u2" {
		t.Fatalf("当前账号应为 u2，实际 %q", got)
	}
	// 复用时**不得**写入明文 token。
	auth, _ := doc["auth"].(map[string]any)
	if _, isStr := auth["accessToken"].(string); isStr {
		t.Fatal("复用了信封却写了明文 token —— 这会让客户端读不出凭据")
	}
}

// WorkBuddy：账号不在客户端列表里 → 必须显式允许明文，否则拒绝。
func TestSwitchWorkBuddyRequiresPlaintextConsent(t *testing.T) {
	_, _ = setupHome(t)
	path := workBuddyAuthPath("cn")
	writeFile(t, path, `{
  "account": {"uid": "u1"},
  "auth": {"accessToken": {"$wbEncrypted":1,"envelope":"AAA"}},
  "allAccounts": [{"uid":"u1","accessToken":{"$wbEncrypted":1,"envelope":"AAA"}}]
}`)

	// 未同意 → 拒绝，且**不得改动文件**。
	_, err := SwitchWorkBuddy("cn", PoolAccount{
		ID: "c.json", UID: "u3", Nickname: "丙", Site: "cn", AccessToken: "tok",
	}, false)
	if err == nil {
		t.Fatal("未允许明文时应拒绝")
	}
	if !strings.Contains(err.Error(), "明文") {
		t.Fatalf("错误信息应说明是明文风险，实际: %v", err)
	}
	doc := readJSON(t, path)
	if acc, _ := doc["account"].(map[string]any); acc["uid"] != "u1" {
		t.Fatal("拒绝时不该改动认证文件")
	}

	// 同意 → 成功写入明文，并明确提示风险与恢复方式。
	notes, err := SwitchWorkBuddy("cn", PoolAccount{
		ID: "c.json", UID: "u3", Nickname: "丙", Site: "cn", AccessToken: "tok",
	}, true)
	if err != nil {
		t.Fatalf("允许明文后应成功，实际: %v", err)
	}
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "明文") || !strings.Contains(joined, "恢复") {
		t.Fatalf("应提示明文风险与恢复方式，实际: %s", joined)
	}
	doc = readJSON(t, path)
	if acc, _ := doc["account"].(map[string]any); acc["uid"] != "u3" {
		t.Fatal("应已切到 u3")
	}
	// allAccounts 应含新账号，且**不含重复的旧同名项**。
	all, _ := doc["allAccounts"].([]any)
	if len(all) != 2 {
		t.Fatalf("allAccounts 应有 2 项（u1 + u3），实际 %d", len(all))
	}
}

// 探测必须把「装了但我们没实现」与「没装」区分开。
// 合成一个布尔值会把责任推给用户的环境。
func TestProbeDistinguishesInstalledFromWritable(t *testing.T) {
	_, local := setupHome(t)
	// 造一个 VS Code 的 globalStorage（装了），但我们要确认它仍不可写。
	writeFile(t, filepath.Join(local, "Code", "User", "globalStorage", "state.vscdb"), "x")

	targets := Probe(nil)
	byID := map[TargetID]TargetState{}
	for _, ts := range targets {
		byID[ts.ID] = ts
	}

	vs := byID[TargetVSCode]
	if !vs.Installed {
		t.Fatal("造了 state.vscdb，应判为已安装")
	}
	if vs.Writable {
		t.Fatal("VS Code 未实现写入，Writable 必须为 false")
	}
	if len(vs.Blockers) == 0 {
		t.Fatal("不可写时必须给出 Blockers 说明原因")
	}

	// 契约自检：Blockers 非空却不允许写，反之亦然 —— 不一致就是 bug。
	for _, ts := range targets {
		if len(ts.Blockers) > 0 && ts.Writable {
			t.Fatalf("%s：有 Blockers 却标为可写", ts.ID)
		}
	}

	// CLI 必须始终可写（未安装也能写，但要说清后果）。
	if !byID[TargetCLI].Writable {
		t.Fatal("CLI 应可写")
	}
	if !byID[TargetWorkBuddy].Writable && byID[TargetWorkBuddy].Installed {
		t.Fatal("WorkBuddy 已安装且有认证文件时应可写")
	}
}

// 当前账号识别：按 uid / token 反查账号池。
func TestProbeMatchesPoolAccounts(t *testing.T) {
	_, _ = setupHome(t)
	path := workBuddyAuthPath("cn")
	writeFile(t, path, `{"account":{"uid":"uid-match"},"auth":{"accessToken":"t"},"allAccounts":[{"uid":"uid-match"}]}`)

	pool := []PoolAccount{
		{ID: "workbuddy-x.json", UID: "uid-other", Nickname: "别的", Site: "cn"},
		{ID: "workbuddy-y.json", UID: "uid-match", Nickname: "匹配的", Site: "cn"},
	}
	byID := map[TargetID]TargetState{}
	for _, ts := range Probe(pool) {
		byID[ts.ID] = ts
	}
	wb := byID[TargetWorkBuddy]
	if wb.CurrentUID != "uid-match" {
		t.Fatalf("当前 uid 应为 uid-match，实际 %q", wb.CurrentUID)
	}
	if wb.CurrentAccount != "workbuddy-y.json" {
		t.Fatalf("应反查到 workbuddy-y.json，实际 %q", wb.CurrentAccount)
	}
	if len(wb.AllAccounts) != 1 {
		t.Fatalf("AllAccounts 应有 1 项，实际 %v", wb.AllAccounts)
	}
	if len(wb.CurrentAccounts) != 1 || wb.CurrentAccounts[0] != "workbuddy-y.json" {
		t.Fatalf("CurrentAccounts 应为 [workbuddy-y.json]，实际 %v", wb.CurrentAccounts)
	}
}

// 国内站与国际站是两个独立文件，各自登录的账号**都要**上报 ——
// 只报一个会让另一个站点的当前登录在面板上永远不显示绿色状态。
func TestProbeReportsBothSitesCurrentAccounts(t *testing.T) {
	_, _ = setupHome(t)
	writeFile(t, workBuddyAuthPath("cn"),
		`{"account":{"uid":"uid-cn"},"auth":{"accessToken":"t"},"allAccounts":[{"uid":"uid-cn"}]}`)
	writeFile(t, workBuddyAuthPath("intl"),
		`{"account":{"uid":"uid-intl"},"auth":{"accessToken":"t"},"allAccounts":[{"uid":"uid-intl"}]}`)

	pool := []PoolAccount{
		{ID: "workbuddy-cn.json", UID: "uid-cn", Site: "cn"},
		{ID: "workbuddy-intl.json", UID: "uid-intl", Site: "intl"},
	}
	byID := map[TargetID]TargetState{}
	for _, ts := range Probe(pool) {
		byID[ts.ID] = ts
	}
	wb := byID[TargetWorkBuddy]

	want := []string{"workbuddy-cn.json", "workbuddy-intl.json"}
	if len(wb.CurrentAccounts) != len(want) {
		t.Fatalf("CurrentAccounts 应为 %v，实际 %v", want, wb.CurrentAccounts)
	}
	for i, id := range want {
		if wb.CurrentAccounts[i] != id {
			t.Fatalf("CurrentAccounts[%d] 应为 %s，实际 %s（顺序：国内站在前）", i, id, wb.CurrentAccounts[i])
		}
	}
	// 旧字段保持单值语义：优先国内站。
	if wb.CurrentAccount != "workbuddy-cn.json" {
		t.Fatalf("CurrentAccount 应保持国内站优先，实际 %q", wb.CurrentAccount)
	}
}

// 只登录国际站（国内站文件不存在）时，CurrentAccount 仍要能反查到池内账号 ——
// 否则「只装了国际版客户端」的部署会被显示成未识别登录。
func TestProbeIntlOnlyStillMatchesPool(t *testing.T) {
	_, _ = setupHome(t)
	writeFile(t, workBuddyAuthPath("intl"),
		`{"account":{"uid":"uid-intl"},"auth":{"accessToken":"t"},"allAccounts":[{"uid":"uid-intl"}]}`)

	pool := []PoolAccount{{ID: "workbuddy-intl.json", UID: "uid-intl", Site: "intl"}}
	byID := map[TargetID]TargetState{}
	for _, ts := range Probe(pool) {
		byID[ts.ID] = ts
	}
	wb := byID[TargetWorkBuddy]
	if wb.CurrentAccount != "workbuddy-intl.json" {
		t.Fatalf("只登录国际站时也应反查到池内账号，实际 %q", wb.CurrentAccount)
	}
	if len(wb.CurrentAccounts) != 1 || wb.CurrentAccounts[0] != "workbuddy-intl.json" {
		t.Fatalf("CurrentAccounts 应为 [workbuddy-intl.json]，实际 %v", wb.CurrentAccounts)
	}
}

// 备份 ID 不得被用来穿越目录。
func TestRestoreBackupRejectsPathTraversal(t *testing.T) {
	_, _ = setupHome(t)
	for _, id := range []string{"../../etc/passwd", `..\..\x`, "a/b", ""} {
		if _, err := RestoreBackup(id); err == nil {
			t.Fatalf("备份 ID %q 应被拒绝", id)
		}
	}
}

// 环境变量冲突必须是**告警**而不是阻塞。
//
// 我们读到的是网关自己进程的 env，而 CLI 由用户从别处启动、继承的未必是同一套。
// 把它当阻塞会让这个功能在本机永远不可用（实测本机就带着
// CODEBUDDY_INTERNET_ENVIRONMENT），而实际上多半没问题。
func TestEnvConflictIsWarningNotBlocker(t *testing.T) {
	home, _ := setupHome(t)
	writeFile(t, cliSettingsPathOf(home), `{"env":{}}`)

	t.Setenv("CODEBUDDY_INTERNET_ENVIRONMENT", "external")
	t.Setenv("CODEBUDDY_AUTH_TOKEN", "some-token")

	var cli TargetState
	for _, ts := range Probe(nil) {
		if ts.ID == TargetCLI {
			cli = ts
		}
	}
	if !cli.Writable {
		t.Fatalf("环境变量冲突不该把 CLI 判为不可写；blockers=%v", cli.Blockers)
	}
	if len(cli.Blockers) != 0 {
		t.Fatalf("环境变量冲突不该进 blockers，实际 %v", cli.Blockers)
	}
	if !strings.Contains(cli.Warning, "CODEBUDDY_INTERNET_ENVIRONMENT") {
		t.Fatalf("应在 warning 里说明环境变量冲突，实际: %q", cli.Warning)
	}
	if !strings.Contains(cli.Warning, "CODEBUDDY_AUTH_TOKEN") {
		t.Fatalf("应在 warning 里说明 token 环境变量，实际: %q", cli.Warning)
	}
}

// CodeBuddy IDE 的探测**不得**把 WorkBuddy 客户端的数据目录算作自己。
//
// 曾经的实现把 `%LOCALAPPDATA%\CodeBuddyExtension\Data` 当候选路径，
// 而那正是 WorkBuddy 客户端认证文件所在的目录 —— 结果是本机永远报
// 「已检测到 CodeBuddy IDE」。**把别人的目录算成自己的战绩，
// 比老实说「未检测到」更糟。**
func TestCodeBuddyIDEProbeDoesNotClaimWorkBuddyDataDir(t *testing.T) {
	_, local := setupHome(t)
	// 造出 WorkBuddy 客户端的数据目录（认证文件就在它下面）。
	writeFile(t, filepath.Join(local, "CodeBuddyExtension", "Data", "Public", "auth", "workbuddy-desktop.info"),
		`{"account":{"uid":"u"},"auth":{"accessToken":"t"}}`)

	for _, ts := range Probe(nil) {
		if ts.ID == TargetCodeBuddyI {
			if ts.Installed {
				t.Fatalf("不该把 WorkBuddy 客户端的数据目录判成 CodeBuddy IDE；path=%s", ts.Path)
			}
			if ts.Writable {
				t.Fatal("未检测到时不该标为可写")
			}
			return
		}
	}
	t.Fatal("未找到 codebuddy-ide 目标")
}

// 可写状态必须真的能走通：CLI 在「装了但没配 token」时应可写。
func TestCLIWritableWhenInstalledButUnconfigured(t *testing.T) {
	home, _ := setupHome(t)
	writeFile(t, cliSettingsPathOf(home), `{"hooks":{"Stop":[]}}`)

	for _, ts := range Probe(nil) {
		if ts.ID != TargetCLI {
			continue
		}
		if !ts.Installed || !ts.Writable {
			t.Fatalf("装了但未配置 token 时应 installed+writable，实际 %+v", ts)
		}
		if len(ts.Blockers) != 0 {
			t.Fatalf("不该有阻塞，实际 %v", ts.Blockers)
		}
		return
	}
	t.Fatal("未找到 CLI 目标")
}

// 列表字段**永远不能是 null**。
//
// Go 的 nil slice 会序列化成 `null`，前端拿到后 `.map()` / `.length`
// 直接抛异常 —— 表现是**整个页面白屏**，而根因只是一个字段是 null。
// 实测踩过：账号页因为 blockers 为 null 整页崩掉。
func TestListFieldsAreNeverNil(t *testing.T) {
	_, _ = setupHome(t)

	raw, err := json.Marshal(Probe(nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("探测结果里出现了 null（前端会崩）: %s", raw)
	}

	// CLI 状态同理。
	home, _ := setupHome(t)
	writeFile(t, cliSettingsPathOf(home), `{}`)
	st, err := ReadCLIStatus(home)
	if err != nil {
		t.Fatal(err)
	}
	raw2, _ := json.Marshal(st)
	if strings.Contains(string(raw2), "null") {
		t.Fatalf("CLI 状态里出现了 null: %s", raw2)
	}
}

// 共存冲突检测：wb-switch 的 hook 挂在 settings.json 里才算「会动手」。
//
// 只检测到数据目录是不够的 —— 目录在只说明装过，
// **hook 挂在配置里才说明它现在还会改写登录状态**。
func TestDetectSwitchConflict(t *testing.T) {
	home, _ := setupHome(t)

	// 无冲突时：Present=false、Contended 是空数组（不能是 nil，前端会崩）。
	none := DetectSwitchConflict()
	if none.Present {
		t.Fatal("什么都没造，不该报冲突")
	}
	if none.Contended == nil {
		t.Fatal("Contended 不能是 nil（会序列化成 null 让前端崩）")
	}
	raw, _ := json.Marshal(none)
	if strings.Contains(string(raw), "null") {
		t.Fatalf("冲突信息里出现 null: %s", raw)
	}

	// 造出 wb-switch 的目录与 hook。
	writeFile(t, filepath.Join(home, ".wb-switch", "accounts.json"), `{}`)
	writeFile(t, cliSettingsPathOf(home), `{"hooks":{"Stop":[{"hooks":[`+
		`{"command":"bash '/c/Users/x/.wb-switch/hook.sh'","type":"command"}]}]}}`)
	// 再造一个 WorkBuddy 认证文件，验证「争抢文件」列表。
	writeFile(t, workBuddyAuthPath("cn"), `{"account":{"uid":"u"},"auth":{"accessToken":"t"}}`)

	c := DetectSwitchConflict()
	if !c.Present {
		t.Fatal("应检测到 wb-switch")
	}
	if !c.HookActive {
		t.Fatalf("hooks 指向 wb-switch 时应判为会动手，实际 commands=%v", c.HookCommands)
	}
	if len(c.HookCommands) == 0 {
		t.Fatal("应把命中的 hook 命令原文带出来便于用户确认")
	}
	// 争抢文件里必须包含 settings.json 与 WorkBuddy 认证文件。
	joined := strings.Join(c.Contended, "|")
	if !strings.Contains(joined, "settings.json") {
		t.Fatalf("争抢文件应含 settings.json，实际 %v", c.Contended)
	}
	if !strings.Contains(joined, "workbuddy-desktop.info") {
		t.Fatalf("争抢文件应含 WorkBuddy 认证文件，实际 %v", c.Contended)
	}
	// 说明里必须给出可执行的下一步，不能只说「有冲突」。
	if !strings.Contains(c.Detail, "二选一") || !strings.Contains(c.Detail, "hooks") {
		t.Fatalf("说明应给出处置建议，实际: %s", c.Detail)
	}
}

// 只有目录、没有 hook 时：Present=true 但 HookActive=false。
func TestDetectSwitchConflictWithoutHook(t *testing.T) {
	home, _ := setupHome(t)
	writeFile(t, filepath.Join(home, ".wb-switch", "accounts.json"), `{}`)
	writeFile(t, cliSettingsPathOf(home), `{"hooks":{"Stop":[{"hooks":[{"command":"echo hi"}]}]}}`)

	c := DetectSwitchConflict()
	if !c.Present {
		t.Fatal("目录存在应判为 Present")
	}
	if c.HookActive {
		t.Fatal("hook 不指向 wb-switch 时不该判为 HookActive")
	}
	// 非 wb-switch 的 hook 不该被误当成冲突。
	for _, cmd := range c.HookCommands {
		if strings.Contains(cmd, "wb-switch") {
			t.Fatalf("不该把普通 hook 当冲突: %v", c.HookCommands)
		}
	}
}

// 深度遍历：未知事件名下的 hook 也要捞到（硬编码键名会静默漏检）。
func TestCollectHookCommandsUnknownEventNames(t *testing.T) {
	doc := map[string]any{
		"hooks": map[string]any{
			"SomeFutureEvent": []any{
				map[string]any{"hooks": []any{
					map[string]any{"command": "run a"},
					map[string]any{"nested": map[string]any{"command": "run b"}},
				}},
			},
		},
	}
	got := collectHookCommands(doc)
	if len(got) != 2 {
		t.Fatalf("应捞到 2 条命令（含嵌套），实际 %v", got)
	}
}

// =============================================================================
// 退出标记（对照 wb-switch a657dc8）
// =============================================================================

// 标记路径是**在完整认证文件名后追加** `.logged-out`，不是替换 `.info`。
//
// 写成 `workbuddy-desktop.logged-out` 客户端根本不会读 —— 静默无效，
// 而「切换成功但客户端仍显示未登录」是最难自查的一类现象。
func TestLogoutMarkerPathAppendsToFullName(t *testing.T) {
	p := workBuddyAuthPath("cn") // .../workbuddy-desktop.info
	got := logoutMarkerPath(p)
	want := p + ".logged-out"
	if got != want {
		t.Fatalf("标记路径应为 %q，实际 %q", want, got)
	}
	if strings.HasSuffix(strings.TrimSuffix(got, ".logged-out"), ".logged-out") {
		t.Fatal("不该出现重复后缀")
	}
	// 必须仍然以 .info 结尾（说明是追加而不是替换扩展名）。
	if !strings.Contains(filepath.Base(got), ".info.logged-out") {
		t.Fatalf("应为 `xxx.info.logged-out` 形态，实际 %q", filepath.Base(got))
	}
}

// 切换时必须清掉退出标记，否则「切换成功」是假的。
func TestSwitchWorkBuddyClearsLogoutMarker(t *testing.T) {
	setupHome(t)
	auth := workBuddyAuthPath("cn")
	writeFile(t, auth, `{"account":{"uid":"u1"},"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"AAA"}},`+
		`"allAccounts":[{"uid":"u1","accessToken":{"$wbEncrypted":1,"envelope":"AAA"}}]}`)
	// 客户端处于「已退出登录」状态。
	marker := logoutMarkerPath(auth)
	writeFile(t, marker, "")

	if !HasLogoutMarker(auth) {
		t.Fatal("前置条件错误：标记应存在")
	}

	notes, err := SwitchWorkBuddy("cn", PoolAccount{
		ID: "a.json", UID: "u1", Nickname: "甲", Site: "cn", AccessToken: "tok",
	}, true)
	if err != nil {
		t.Fatalf("切换失败: %v", err)
	}
	// 标记必须被删掉。
	if HasLogoutMarker(auth) {
		t.Fatal("切换后退出标记仍在 —— 客户端会无视刚写入的凭据，切换等于没做")
	}
	if !strings.Contains(strings.Join(notes, " | "), "退出标记") {
		t.Fatalf("应说明清理了退出标记，实际: %v", notes)
	}
}

// 标记本来不存在时，不该在说明里写「已清理」（那会让用户以为发生过什么）。
func TestSwitchWorkBuddyNoMarkerNoNote(t *testing.T) {
	setupHome(t)
	auth := workBuddyAuthPath("cn")
	writeFile(t, auth, `{"account":{"uid":"u1"},"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"AAA"}},`+
		`"allAccounts":[{"uid":"u1","accessToken":{"$wbEncrypted":1,"envelope":"AAA"}}]}`)

	notes, err := SwitchWorkBuddy("cn", PoolAccount{
		ID: "a.json", UID: "u1", Nickname: "甲", Site: "cn", AccessToken: "tok",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(notes, " | "), "已清理退出标记") {
		t.Fatalf("标记不存在时不该报「已清理」，实际: %v", notes)
	}
}

// **顺序要求**：写入/校验失败时标记必须**保留**（退出状态不能被半途改掉）。
func TestSwitchWorkBuddyKeepsMarkerOnFailure(t *testing.T) {
	setupHome(t)
	auth := workBuddyAuthPath("cn")
	writeFile(t, auth, `{"account":{"uid":"u1"},"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"AAA"}},`+
		`"allAccounts":[{"uid":"u1","accessToken":{"$wbEncrypted":1,"envelope":"AAA"}}]}`)
	marker := logoutMarkerPath(auth)
	writeFile(t, marker, "")

	// 目标账号不在客户端列表里且未允许明文 → 切换被拒。
	if _, err := SwitchWorkBuddy("cn", PoolAccount{
		ID: "b.json", UID: "u3", Nickname: "丙", Site: "cn", AccessToken: "tok",
	}, false); err == nil {
		t.Fatal("应被拒绝")
	}
	if !HasLogoutMarker(auth) {
		t.Fatal("切换失败时不该动退出标记（退出状态不能被半途改掉）")
	}
}

// 探测：标记存在时必须提示，且**不能覆盖**加密信封那条告警。
//
// 第一版用 `st.Warning = ...` 写第二条，直接把第一条覆盖掉了 —— 两条都得在。
func TestProbeWarnsLogoutMarkerWithoutOverwriting(t *testing.T) {
	setupHome(t)
	auth := workBuddyAuthPath("cn")
	// 同时满足两个条件：凭据是加密信封 + 存在退出标记。
	writeFile(t, auth, `{"account":{"uid":"u1"},"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"AAA"}},`+
		`"allAccounts":[{"uid":"u1","accessToken":{"$wbEncrypted":1,"envelope":"AAA"}}]}`)
	writeFile(t, logoutMarkerPath(auth), "")

	var wb TargetState
	for _, ts := range Probe(nil) {
		if ts.ID == TargetWorkBuddy {
			wb = ts
		}
	}
	if !strings.Contains(wb.Warning, "退出标记") {
		t.Fatalf("应提示存在退出标记，实际: %q", wb.Warning)
	}
	if !strings.Contains(wb.Warning, "加密信封") {
		t.Fatalf("加密信封那条告警被覆盖了，实际: %q", wb.Warning)
	}
}

// 退出标记本身不该被当成「凭据有问题」而阻止写入 —— 它是可被自动清理的状态。
func TestLogoutMarkerDoesNotBlockWritable(t *testing.T) {
	setupHome(t)
	auth := workBuddyAuthPath("cn")
	writeFile(t, auth, `{"account":{"uid":"u1"},"auth":{"accessToken":{"$wbEncrypted":1,"envelope":"AAA"}},`+
		`"allAccounts":[{"uid":"u1","accessToken":{"$wbEncrypted":1,"envelope":"AAA"}}]}`)
	writeFile(t, logoutMarkerPath(auth), "")

	for _, ts := range Probe(nil) {
		if ts.ID == TargetWorkBuddy {
			if !ts.Writable {
				t.Fatalf("退出标记不该让目标变成不可写: %+v", ts)
			}
			if len(ts.Blockers) != 0 {
				t.Fatalf("不该有阻塞: %v", ts.Blockers)
			}
			return
		}
	}
	t.Fatal("未找到 workbuddy-desktop 目标")
}
