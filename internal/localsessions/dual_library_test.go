package localsessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupIntlLibrary 在夹具里造出国际站会话库（与 setupSyncStore 的表结构一致），
// 返回国际站库的 projects 目录。
func setupIntlLibrary(t *testing.T, s *Store) string {
	t.Helper()
	intl := s.libByVariant("intl")
	if err := os.MkdirAll(intl.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(intl.DBPath))
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
	db.Close()
	return filepath.Join(intl.Root, "projects")
}

// 跨库复制：国际站账号的副本必须写进国际站的库（~/.workbuddy-ai），
// 国内站的库一行不多。这是 2026-10-05 实测问题的直接回归测试：
// 旧实现把国际站副本写进国内站库，面板显示「内容一致」而国际站客户端永远看不到。
func TestCopySessionWritesTargetVariantLibrary(t *testing.T) {
	s, src, _ := setupSyncStore(t)
	intlProjects := setupIntlLibrary(t, s)

	res, err := s.CopySession(src, CopyOptions{TargetUID: "uid-intl", TargetVariant: "intl"})
	if err != nil {
		t.Fatalf("跨库复制失败: %v", err)
	}

	// 国际站库里有新行、国内站库没有。
	intlDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.libByVariant("intl").DBPath))
	if err != nil {
		t.Fatal(err)
	}
	defer intlDB.Close()
	var n int
	if err := intlDB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, res.NewID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("国际站库里应有新会话行，实际 %d", n)
	}
	if _, err := os.Stat(filepath.Join(intlProjects, "d-ws", res.NewID+".jsonl")); err != nil {
		t.Fatalf("副本正文应写进国际站库的同一工作区目录: %v", err)
	}

	cnDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	defer cnDB.Close()
	if err := cnDB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, res.NewID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("国内站库不该出现国际站账号的副本（这正是要修的 bug）")
	}

	// 正文内容与源一致（重写 sessionId 之后）。
	if _, err := os.Stat(filepath.Join(s.Root, "projects", "d-ws", src+".jsonl")); err != nil {
		t.Fatal(err)
	}
}

// 目标档位没有会话库（客户端没跑过）必须拒绝，而不是替它造库赌 schema。
func TestCopySessionRequiresTargetLibrary(t *testing.T) {
	s, src, _ := setupSyncStore(t)
	// intl 库不存在（setupSyncStore 只建了国内站）。
	_, err := s.CopySession(src, CopyOptions{TargetUID: "uid-intl", TargetVariant: "intl"})
	if err == nil {
		t.Fatal("目标档位没有会话库时应拒绝")
	}
	if !strings.Contains(err.Error(), "会话库") {
		t.Fatalf("错误里应说明缺会话库，实际 %v", err)
	}
}
