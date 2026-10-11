package localsessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// 本文件守住「归档状态在关联会话之间传导」。
//
// 要解决的问题：一份会话在多个账号间有副本、正文已经一致（identical/ahead）时，
// 用户在来源归档了它，目标却永远同步不到 —— 归档不是正文动作，正文一致时没有
// 任何可勾选项。这里钉住传导规则的每一条边界，尤其是**不授权**的那几类
// （活跃目标、未知状态、缺列）—— 放宽它们的代价是把一个还在写的会话标成归档。

// setStatus 直接改夹具里的会话状态。
func setStatus(t *testing.T, s *Store, sessionID, status string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE sessions SET status = ? WHERE id = ?`, status, sessionID); err != nil {
		t.Fatal(err)
	}
}

// statusOf 读夹具里的会话状态。
func statusOf(t *testing.T, s *Store, sessionID string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v sql.NullString
	if err := db.QueryRow(`SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v.String
}

// archiveFixture 造「来源归档 + 目标 completed + 正文一致」的一对成员。
//
// 正文一致是归档传导的前提：正文不一致时用户本来就该先同步正文。
// 用 CopySession 复制成一致副本（复制出的内容与源逐字节同源，判定即 identical）。
func archiveFixture(t *testing.T) (s *Store, groupID, srcSessionID, tgtSessionID string) {
	t.Helper()
	s, _, src := setupSyncStore(t)
	res, err := s.CopySession(src, CopyOptions{TargetUID: "uid-T", TargetVariant: "cn"})
	if err != nil {
		t.Fatalf("复制并登记失败: %v", err)
	}
	// 复制出的副本与源同源 → 判定 identical。
	setStatus(t, s, src, SessionStatusArchived)
	setStatus(t, s, res.NewID, "completed")
	return s, res.GroupID, src, res.NewID
}

// memberIDs 从组里取出两个成员的 MemberID 与快照。
func memberPair(t *testing.T, s *Store, groupID, srcSessionID, tgtSessionID string) (*LinkGroupR, LinkMember, LinkMember) {
	t.Helper()
	store, _, err := s.loadLinkStore()
	if err != nil {
		t.Fatal(err)
	}
	g := findGroupR(store, groupID)
	if g == nil {
		t.Fatal("组不存在")
	}
	var srcM, tgtM *LinkMember
	for i := range g.Members {
		if g.Members[i].SessionID == srcSessionID {
			srcM = &g.Members[i]
		}
		if g.Members[i].SessionID == tgtSessionID {
			tgtM = &g.Members[i]
		}
	}
	if srcM == nil || tgtM == nil {
		t.Fatalf("组里找不到两个成员：%+v", g.Members)
	}
	return g, *srcM, *tgtM
}

// TestArchiveQualifiesRules 逐条钉住归档传导的授权规则。
func TestArchiveQualifiesRules(t *testing.T) {
	cases := []struct {
		name           string
		source, target string
		want           bool
	}{
		{"来源归档 + 目标 completed", "archived", "completed", true},
		{"来源归档 + 目标 error", "archived", "error", true},
		{"来源归档 + 目标 terminated", "archived", "terminated", true},
		{"来源归档 + 目标 archived（已归档，无需再传）", "archived", "archived", false},
		{"来源归档 + 目标 active（活跃保护）", "archived", "active", false},
		{"来源归档 + 目标 working（活跃保护）", "archived", "working", false},
		{"来源归档 + 目标未知值", "archived", "Pending", false},
		{"来源归档 + 目标空串（缺列/NULL）", "archived", "", false},
		{"来源非归档 + 目标 completed", "completed", "completed", false},
		{"来源空串 + 目标 completed", "", "completed", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := archiveQualifies(c.source, c.target); got != c.want {
				t.Fatalf("archiveQualifies(%q,%q)=%t，期望 %t", c.source, c.target, got, c.want)
			}
		})
	}
}

// TestWriteMemberStatusOnlyTouchesTerminalRows 条件 UPDATE 只动终态行、只改 status 列。
func TestWriteMemberStatusOnlyTouchesTerminalRows(t *testing.T) {
	s, _, tgt := setupSyncStore(t)
	lib := s.libByVariant("cn")

	// 目标是 active（夹具默认）时不该被写。
	setStatus(t, s, tgt, "active")
	if ok, _ := s.writeMemberStatus(lib, tgt, "uid", SessionStatusArchived); ok {
		t.Fatal("活跃会话不该被写入归档")
	}
	if got := statusOf(t, s, tgt); got != "active" {
		t.Fatalf("活跃会话状态被改动，实际 %q", got)
	}

	// 目标在终态白名单里才写得进去。
	setStatus(t, s, tgt, "completed")
	if ok, after := s.writeMemberStatus(lib, tgt, "uid", SessionStatusArchived); !ok || after != SessionStatusArchived {
		t.Fatalf("终态会话应可写入归档，ok=%t after=%q", ok, after)
	}
	if got := statusOf(t, s, tgt); got != SessionStatusArchived {
		t.Fatalf("写入后状态应为 archived，实际 %q", got)
	}

	// 其它列必须原样（写入不能顺手改了标题之类）。
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title string
	if err := db.QueryRow(`SELECT title FROM sessions WHERE id = ?`, tgt).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "t" {
		t.Fatalf("写入归档不该改动其它列，title 实际 %q", title)
	}
}

// TestWriteMemberStatusRejectsWrongUser user_id 不匹配时不得写入（防串号）。
func TestWriteMemberStatusRejectsWrongUser(t *testing.T) {
	s, _, tgt := setupSyncStore(t)
	lib := s.libByVariant("cn")
	setStatus(t, s, tgt, "completed")

	if ok, _ := s.writeMemberStatus(lib, tgt, "wrong-uid", SessionStatusArchived); ok {
		t.Fatal("user_id 不匹配时不应写入")
	}
	if got := statusOf(t, s, tgt); got != "completed" {
		t.Fatalf("状态不该被改动，实际 %q", got)
	}
}

// TestMemberStatusMissingColumnReturnsEmpty 老库没有 status 列时安静返回空串
// （不授权、不报错），这是「缺列一律不授权」的实现基础。
func TestMemberStatusMissingColumnReturnsEmpty(t *testing.T) {
	s, _, _ := setupSyncStore(t)
	lib := &library{Variant: "cn", Root: t.TempDir(), DBPath: filepath.Join(t.TempDir(), "old.db")}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(lib.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions VALUES ('x', 'D:\ws')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if got := s.memberStatus(lib, "x"); got != "" {
		t.Fatalf("缺列时应返回空串，实际 %q", got)
	}
	if ok, _ := s.writeMemberStatus(lib, "x", "uid", SessionStatusArchived); ok {
		t.Fatal("缺列时不应写入")
	}
}

// TestPreviewExposesArchiveAction 正文一致 + 来源归档 + 目标终态 → 预览下发归档动作。
func TestPreviewExposesArchiveAction(t *testing.T) {
	s, groupID, src, tgt := archiveFixture(t)
	_, srcM, tgtM := memberPair(t, s, groupID, src, tgt)

	preview, err := s.PreviewPair(groupID, srcM.MemberID, tgtM.MemberID)
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if preview.Verdict != VerdictIdentical {
		t.Fatalf("前提不成立：正文应一致，实际判定 %s", preview.Verdict)
	}
	if preview.ArchiveAction != ModeStatusOnly {
		t.Fatalf("应下发归档动作，实际 %q", preview.ArchiveAction)
	}
	if preview.SourceStatus != SessionStatusArchived || preview.TargetStatus != "completed" {
		t.Fatalf("预览应带双方状态，实际 source=%q target=%q", preview.SourceStatus, preview.TargetStatus)
	}
}

// TestSyncStatusOnlyEndToEnd 端到端：只改归档，不动正文。
func TestSyncStatusOnlyEndToEnd(t *testing.T) {
	s, groupID, src, tgt := archiveFixture(t)
	g, srcM, tgtM := memberPair(t, s, groupID, src, tgt)

	preview, err := s.PreviewPair(groupID, srcM.MemberID, tgtM.MemberID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ArchiveAction != ModeStatusOnly {
		t.Fatalf("应下发归档动作，实际 %q", preview.ArchiveAction)
	}

	out := s.SyncMemberPair(SyncPairInput{
		Group: g, Source: srcM, Target: tgtM,
		Mode: ModeStatusOnly, PreviewToken: preview.PreviewToken,
	})
	if !out.OK || out.Error != "" {
		t.Fatalf("归档传导应成功，error=%q reason=%q", out.Error, out.Reason)
	}
	if got := statusOf(t, s, tgt); got != SessionStatusArchived {
		t.Fatalf("目标应变为 archived，实际 %q", got)
	}
}

// TestSyncStatusOnlySkipsActiveTarget 预览之后目标被重新打开成活跃 → 执行必须拦下。
func TestSyncStatusOnlySkipsActiveTarget(t *testing.T) {
	s, groupID, src, tgt := archiveFixture(t)
	g, srcM, tgtM := memberPair(t, s, groupID, src, tgt)

	preview, err := s.PreviewPair(groupID, srcM.MemberID, tgtM.MemberID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ArchiveAction == "" {
		t.Fatal("前提不成立：本应下发归档动作")
	}

	setStatus(t, s, tgt, "active")
	out := s.SyncMemberPair(SyncPairInput{
		Group: g, Source: srcM, Target: tgtM,
		Mode: ModeStatusOnly, PreviewToken: preview.PreviewToken,
	})
	if out.OK {
		t.Fatal("目标已变活跃，不该写入归档")
	}
	if got := statusOf(t, s, tgt); got != "active" {
		t.Fatalf("活跃目标状态被改动，实际 %q", got)
	}
}

// TestSyncStatusOnlyNoOpWhenSourceNotArchived 来源没归档 → 不下发动作、执行也拒绝。
func TestSyncStatusOnlyNoOpWhenSourceNotArchived(t *testing.T) {
	s, groupID, src, tgt := archiveFixture(t)
	setStatus(t, s, src, "completed") // 来源取消归档
	_, srcM, tgtM := memberPair(t, s, groupID, src, tgt)

	preview, err := s.PreviewPair(groupID, srcM.MemberID, tgtM.MemberID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ArchiveAction != "" {
		t.Fatalf("来源未归档时不该下发归档动作，实际 %q", preview.ArchiveAction)
	}
}

// TestStatusOnlyLeavesBodyUntouched statusOnly 绝不改正文（这是它区别于正文模式的核心）。
func TestStatusOnlyLeavesBodyUntouched(t *testing.T) {
	s, groupID, src, tgt := archiveFixture(t)
	g, srcM, tgtM := memberPair(t, s, groupID, src, tgt)

	before, err := os.ReadFile(s.bodyPathForTest(tgtM))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewPair(groupID, srcM.MemberID, tgtM.MemberID)
	if err != nil {
		t.Fatal(err)
	}
	out := s.SyncMemberPair(SyncPairInput{
		Group: g, Source: srcM, Target: tgtM,
		Mode: ModeStatusOnly, PreviewToken: preview.PreviewToken,
	})
	if !out.OK {
		t.Fatalf("归档传导应成功，error=%q", out.Error)
	}
	after, err := os.ReadFile(s.bodyPathForTest(tgtM))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("statusOnly 不该改动正文")
	}
}

// bodyPathForTest 定位某成员的正文文件。
func (s *Store) bodyPathForTest(m LinkMember) string {
	lib := s.libByVariant(m.Variant)
	p, _ := s.bodyPathIn(lib, m.SessionID, "")
	return p
}
