package localsessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildJSONL 造一份「会话正文」：首行 meta，之后每行带 sessionId。
func buildJSONL(sessionID string, texts ...string) string {
	var b strings.Builder
	b.WriteString(`{"type":"session-meta","sessionId":"` + sessionID + `","id":"m0"}` + "\n")
	for i, t := range texts {
		b.WriteString(`{"type":"user","sessionId":"` + sessionID + `","id":"m`)
		b.WriteByte(byte('1' + i))
		b.WriteString(`","text":"` + t + `"}` + "\n")
	}
	return b.String()
}

// **核心不变量**：同一段对话的两份副本（sessionId 不同）归一化后必须摘要完全相同。
//
// 整个同步机制都建立在这一条上：如果它不成立，「同源」就永远判不出来，
// 每次同步都会报分叉。

// 末尾换行不算记录；中间空行与非法 JSON 必须**拒绝**（内容可能不完整）。

// 总摘要必须对顺序敏感，且要防拼接歧义。

// 判定表逐格验证。

// -----------------------------------------------------------------------------
// 端到端（用临时库）
// -----------------------------------------------------------------------------

// setupSyncStore 造一个含两条「同源」会话的库。
func setupSyncStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	local := filepath.Join(root, "local")
	state := filepath.Join(root, "state")
	for _, d := range []string{home, local, state} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOCALAPPDATA", local)

	s := NewStore(home)
	s.StateDir = state
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, cwd TEXT, user_id TEXT, title TEXT, custom_title TEXT,
		status TEXT, created_at INTEGER, updated_at INTEGER, last_activity_at INTEGER,
		deleted_at INTEGER, is_playground INTEGER, source_mode TEXT,
		is_background_automation INTEGER, mode TEXT, model TEXT, expert_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	now := int64(1759000000000)
	for _, id := range []string{"src", "tgt"} {
		if _, err := db.Exec(`INSERT INTO sessions VALUES (?,?,'uid','t',NULL,'active',?,?,?,0,0,'cli',0,'code','m','')`,
			id, `D:\ws`, now, now, now); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	dir := filepath.Join(s.Root, "projects", "d-ws")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("src.jsonl", buildJSONL("src", "a", "b", "c"))
	write("tgt.jsonl", buildJSONL("tgt", "a"))
	return s, "src", "tgt"
}

// 快进：目标只有前缀 → 直接追加同步；源不变；基线被记录；再跑一次变 Identical。

// 目标在同步后又新增 → 判 Ahead，不该往回同步（那会覆盖目标的新内容）。

// 两边都改 → Diverge；不 force 必须拒绝，force 才执行且覆盖前备份。

// 「以此为准」对**从未经本工具同步过**的分叉副本也要生效。
//
// 这是真实用户路径：手动复制出来的副本会自动进组（内容分组的意义所在），
// 而它们没有基线 —— 早期实现把「没有基线」与「内容不可读」混为一谈，
// 于是点「以此为准」永远 0 成功、全跳过，确认框承诺的覆盖一条也没发生。

// 「以此为准」时目标更新（ahead）也要被覆盖 —— 对照 wb-switch 的 unifyOverwrite 模式。

// 内容不可读时，**确认也不能做**：没有可复制的内容。

// 基线文件必须落在 StateDir，且**不能污染 ~/.workbuddy**。

// 未配置 StateDir 时：同步仍可用（快进不依赖基线），但基线相关判定退化为 Unknown。

// **identical 必须落基线** —— 否则下一步判定必然退化成 Unknown。
//
// 实测踩过：复制完两边一致（identical），之后目标新增一条，
// 本该判 ahead 却报「这一对会话还没有同步过，找不到上次同步的记录」。
// 基线就是「此刻两边一致到哪里」的记录，identical 正是该记它的时刻。
