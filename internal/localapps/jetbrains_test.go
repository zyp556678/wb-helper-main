package localapps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 平台真实写盘形态（实测样本，照抄 wb-switch 的结论）。
//
// **外层 `<application><component name="SecretStorage"><Scores>` 是必需的**：
// SecretStorage 的 getState() 返回的 MapStorage 根会被平台展平，组件文件里
// 没有这一层。写成裸 `<MapStorage>` 顶层时平台读回会**整体丢弃**，
// 表现为「写入的 key 消失、插件显示未登录」且不报错 —— 最难自查的一类失败。
func TestJetBrainsXMLHasRequiredWrapper(t *testing.T) {
	got := serializeSecretEntries([]secretEntry{{Key: "k", Value: "v"}})

	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<application>`,
		`<component name="SecretStorage">`,
		`<Scores>`,
		`<Entry key="k" value="v" />`,
		`</Scores>`,
		`</component>`,
		`</application>`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("产物缺少 %q\n实际:\n%s", want, got)
		}
	}
	// 绝不能出现裸 MapStorage 顶层（那正是会被平台丢弃的写法）。
	if strings.Contains(got, "MapStorage") {
		t.Fatalf("出现了 MapStorage —— 平台会整体丢弃这个文件\n%s", got)
	}
	// 层级顺序也要对。
	if strings.Index(got, "<Scores>") < strings.Index(got, `<component name="SecretStorage">`) {
		t.Fatal("<Scores> 应在 component 之内")
	}
}

// 解析必须保持文件顺序（写回时要原位替换，顺序变了会放大与平台写盘的差异）。
func TestParseSecretEntriesKeepsOrder(t *testing.T) {
	content := serializeSecretEntries([]secretEntry{
		{Key: "a.first", Value: "1"},
		{Key: "b.second", Value: "2"},
		{Key: "c.third", Value: "3"},
	})
	got := parseSecretEntries(content)
	if len(got) != 3 {
		t.Fatalf("应解析出 3 条，实际 %d", len(got))
	}
	want := []string{"a.first", "b.second", "c.third"}
	for i, k := range want {
		if got[i].Key != k {
			t.Fatalf("第 %d 条应为 %q，实际 %q（顺序被打乱）", i, k, got[i].Key)
		}
	}
}

// 序列化 → 解析 必须往返一致（含需要转义的字符）。
func TestJetBrainsXMLRoundTrip(t *testing.T) {
	// 刻意放各种需要转义的字符：属性值里的引号、尖括号、&、换行、制表符。
	tricky := `a"b<c>d&e` + "\n" + "f\tg'h"
	entries := []secretEntry{
		{Key: jetbrainsSecretKey, Value: tricky},
		{Key: "other", Value: "plain"},
	}
	got := parseSecretEntries(serializeSecretEntries(entries))
	if len(got) != 2 {
		t.Fatalf("往返后应仍是 2 条，实际 %d", len(got))
	}
	if got[0].Value != tricky {
		t.Fatalf("往返后值不一致：\n  want %q\n  got  %q", tricky, got[0].Value)
	}
	if got[1].Value != "plain" {
		t.Fatalf("第二条被改动了: %q", got[1].Value)
	}
}

// **换行必须转成字符引用**：XML 属性值里的裸换行会被解析器规范化成空格，
// token 里一旦有这类字符就会被静默改坏。
func TestJetBrainsEscapeNewlineAsCharRef(t *testing.T) {
	esc := xmlEscapeAttr("line1\nline2\ttab\rcr")
	for _, raw := range []string{"\n", "\t", "\r"} {
		if strings.Contains(esc, raw) {
			t.Fatalf("转义后不该含裸控制字符 %q：%q", raw, esc)
		}
	}
	for _, ref := range []string{"&#10;", "&#9;", "&#13;"} {
		if !strings.Contains(esc, ref) {
			t.Fatalf("应含字符引用 %s，实际 %q", ref, esc)
		}
	}
	// 反转义要还原。
	if got := xmlUnescape(esc); got != "line1\nline2\ttab\rcr" {
		t.Fatalf("反转义不一致: %q", got)
	}
}

// upsert：已存在 → 原位替换；不存在 → 追加；其余 Entry 原样保留。
func TestUpsertSecret(t *testing.T) {
	base := serializeSecretEntries([]secretEntry{
		{Key: "keep.me", Value: "v1"},
		{Key: jetbrainsSecretKey, Value: "old"},
	})

	next, replaced, found := upsertSecret(base, jetbrainsSecretKey, "new")
	if !found || replaced != "old" {
		t.Fatalf("应识别出已存在并返回旧值，实际 found=%v replaced=%q", found, replaced)
	}
	got := parseSecretEntries(next)
	if len(got) != 2 {
		t.Fatalf("替换不该改变条目数，实际 %d", len(got))
	}
	if got[0].Key != "keep.me" || got[0].Value != "v1" {
		t.Fatalf("其它 Entry 被改动了: %+v", got[0])
	}
	if got[1].Key != jetbrainsSecretKey || got[1].Value != "new" {
		t.Fatalf("目标 Entry 未被替换: %+v", got[1])
	}

	// 不存在 → 追加。
	next2, replaced2, found2 := upsertSecret(serializeSecretEntries([]secretEntry{{Key: "x", Value: "y"}}),
		jetbrainsSecretKey, "new")
	if found2 || replaced2 != "" {
		t.Fatal("目标 key 不存在时不该报 found")
	}
	if n := len(parseSecretEntries(next2)); n != 2 {
		t.Fatalf("应追加成 2 条，实际 %d", n)
	}
}

// 造一个「装了 CodeBuddy 插件」的 JetBrains 配置目录。
func setupJetBrains(t *testing.T, dirName string, pluginDir string, secret string) string {
	t.Helper()
	root := filepath.Join(roamingAppData(), "JetBrains", dirName)
	if pluginDir != "" {
		if err := os.MkdirAll(filepath.Join(root, "plugins", pluginDir), 0o700); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if secret != "" {
		path := filepath.Join(root, "options")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "secret-storage.xml"), []byte(secret), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// 端到端：写入 → 校验 → 其它 Entry 保留 → 产生可恢复的备份。
func TestSwitchJetBrainsEndToEnd(t *testing.T) {
	setupHome(t)
	// 一个装了插件的 IDE + 一个没装插件的（后者不该被写）。
	setupJetBrains(t, "IntelliJIdea2026.2", "coding-copilot-1.0", serializeSecretEntries([]secretEntry{
		{Key: "keep.me", Value: "v1"},
		{Key: jetbrainsSecretKey, Value: "old-token"},
	}))
	setupJetBrains(t, "PyCharm2026.1", "", "")

	dirs := FindJetBrainsDirs()
	if len(dirs) != 1 {
		t.Fatalf("只应找到 1 个装了插件的目录，实际 %d（%+v）", len(dirs), dirs)
	}
	if dirs[0].Name != "IntelliJIdea2026.2" {
		t.Fatalf("应找到 IDEA 目录，实际 %s", dirs[0].Name)
	}
	if dirs[0].CurrentTokenMasked == "" {
		t.Fatal("应读出当前 token（脱敏）")
	}

	res, err := SwitchJetBrains(PoolAccount{
		ID: "a.json", UID: "u", Nickname: "甲", Site: "cn", AccessToken: "Bearer new-token",
	})
	if err != nil {
		t.Fatalf("切换失败: %v", err)
	}
	if res.Written != 1 {
		t.Fatalf("应写入 1 个目录，实际 %d", res.Written)
	}
	if res.BackupID == "" {
		t.Fatal("必须产生备份")
	}

	// 写后校验：token 已换（且去掉了 Bearer 前缀）。
	secret := filepath.Join(roamingAppData(), "JetBrains", "IntelliJIdea2026.2", "options", "secret-storage.xml")
	got, err := readJetBrainsSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if got != "new-token" {
		t.Fatalf("token 应为 new-token（去 Bearer），实际 %q", got)
	}
	// 其它 Entry 必须保留。
	entries := parseSecretEntries(string(mustRead(t, secret)))
	if len(entries) != 2 || entries[0].Key != "keep.me" || entries[0].Value != "v1" {
		t.Fatalf("其它 Entry 没保留: %+v", entries)
	}
	// 文件必须带完整外壳（否则平台丢弃）。
	if !strings.Contains(string(mustRead(t, secret)), `<component name="SecretStorage">`) {
		t.Fatal("产物缺少必需外壳")
	}
	// 提示里要说清需要重启 IDE。
	if !strings.Contains(strings.Join(res.Notes, " | "), "重启") {
		t.Fatalf("应提示需要重启 IDE，实际: %v", res.Notes)
	}

	// 备份可列出且内容 = 写入前的原文。
	list, err := ListBackups(20)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, b := range list {
		if b.ID == res.BackupID {
			found = true
			if b.Target != TargetJetBrains {
				t.Fatalf("备份目标应为 jetbrains，实际 %s", b.Target)
			}
		}
	}
	if !found {
		t.Fatalf("备份 %s 未出现在列表里", res.BackupID)
	}
	if _, err := RestoreBackup(res.BackupID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if back, _ := readJetBrainsSecret(secret); back != "old-token" {
		t.Fatalf("恢复后应为 old-token，实际 %q", back)
	}
}

// 首次写入（文件不存在）也要生成带完整外壳的文件，且恢复时把它删掉。
func TestSwitchJetBrainsCreatesFileWithWrapper(t *testing.T) {
	setupHome(t)
	setupJetBrains(t, "IntelliJIdea2026.2", "coding-copilot-1.0", "") // 无 secret 文件

	res, err := SwitchJetBrains(PoolAccount{ID: "a.json", UID: "u", Site: "cn", AccessToken: "tok"})
	if err != nil {
		t.Fatalf("切换失败: %v", err)
	}
	secret := filepath.Join(roamingAppData(), "JetBrains", "IntelliJIdea2026.2", "options", "secret-storage.xml")
	content := string(mustRead(t, secret))
	if !strings.Contains(content, `<component name="SecretStorage">`) ||
		!strings.Contains(content, `<Entry key="`+jetbrainsSecretKey+`" value="tok" />`) {
		t.Fatalf("新建的文件内容不对:\n%s", content)
	}

	// 恢复：文件原本不存在 → 应被删除，而不是留一个空文件。
	if _, err := RestoreBackup(res.BackupID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Fatal("恢复后文件应回到「不存在」状态")
	}
}

// 没装插件时必须拒绝并说清原因（而不是写到一个用户看不到的地方）。
func TestSwitchJetBrainsRequiresPlugin(t *testing.T) {
	setupHome(t)
	setupJetBrains(t, "IntelliJIdea2026.2", "", "")

	_, err := SwitchJetBrains(PoolAccount{ID: "a.json", UID: "u", Site: "cn", AccessToken: "tok"})
	if err == nil {
		t.Fatal("未装插件时应报错")
	}
	if !strings.Contains(err.Error(), "插件") {
		t.Fatalf("错误信息应说明是插件缺失，实际: %v", err)
	}
}

// 空 token 必须拒绝：写空值等于把插件登录态清掉。
func TestSwitchJetBrainsRejectsEmptyToken(t *testing.T) {
	setupHome(t)
	setupJetBrains(t, "IntelliJIdea2026.2", "coding-copilot-1.0", "")
	if _, err := SwitchJetBrains(PoolAccount{ID: "a.json", UID: "u", Site: "cn", AccessToken: "  "}); err == nil {
		t.Fatal("空 token 应被拒绝")
	}
}

// 探测：装了插件才可写；只装了 IDE 不算。
func TestProbeJetBrainsRequiresPlugin(t *testing.T) {
	setupHome(t)
	setupJetBrains(t, "IntelliJIdea2026.2", "", "")

	var jb TargetState
	for _, ts := range Probe(nil) {
		if ts.ID == TargetJetBrains {
			jb = ts
		}
	}
	if jb.Writable || jb.Installed {
		t.Fatalf("只装了 IDE 时不该判为可用: %+v", jb)
	}
	if len(jb.Blockers) == 0 {
		t.Fatal("应给出阻塞原因")
	}

	// 补上插件目录 → 变成可用。
	setupJetBrains(t, "IntelliJIdea2026.2", "coding-copilot-1.0", "")
	for _, ts := range Probe(nil) {
		if ts.ID == TargetJetBrains {
			jb = ts
		}
	}
	if !jb.Installed || !jb.Writable {
		t.Fatalf("装了插件后应可用: %+v", jb)
	}
	if len(jb.Blockers) != 0 {
		t.Fatalf("可用时不该有阻塞: %v", jb.Blockers)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return b
}
