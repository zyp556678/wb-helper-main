package localsessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// -----------------------------------------------------------------------------
// 登记表模型的测试（对照 wb-switch 的语义：复制=登记、按模式同步、解除=删登记项）
// -----------------------------------------------------------------------------

// setupLinkStore 造出「国内站库里三份同源副本 + 已登记一组」的夹具：
//   - src(a,b,c) 与 third(a,b,c) 内容相同（登记为同组）
//   - tgt(a) 是它们的前缀（落后）
//
// 返回 store 与组 id。
func setupLinkStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, _, _ := setupSyncStore(t)

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	now := int64(1759000000000)
	for _, id := range []string{"third"} {
		if _, err := db.Exec(`INSERT INTO sessions VALUES
			(?,?,'uid-C','t',NULL,'active',?,?,?,0,0,'cli',0,'code','m','')`,
			id, `D:\ws`, now, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE sessions SET user_id='uid-A' WHERE id IN ('src','tgt')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	dir := filepath.Join(s.Root, "projects", "d-ws")
	if err := os.WriteFile(filepath.Join(dir, "third.jsonl"), []byte(buildJSONL("third", "a", "b", "c")), 0o600); err != nil {
		t.Fatal(err)
	}

	// 登记：把 src 复制给 uid-C（副本 id 由复制生成）——这是 switch 的组来源。
	res, err := s.CopySession("src", CopyOptions{TargetUID: "uid-C", TargetVariant: "cn"})
	if err != nil {
		t.Fatalf("复制并登记失败: %v", err)
	}
	return s, res.GroupID
}

// 复制 = copy + 登记：组/成员/配对基线一次建好；同源身份再次复制复用同一个组。
func TestCopyRegistersGroupMembersAndBaseline(t *testing.T) {
	s, groupID := setupLinkStore(t)

	store, existed, err := s.loadLinkStore()
	if err != nil || !existed {
		t.Fatalf("登记表应已写入: existed=%v err=%v", existed, err)
	}
	if len(store.Groups) != 1 {
		t.Fatalf("应有 1 个组，实际 %d", len(store.Groups))
	}
	g := store.Groups[0]
	if g.ID != groupID {
		t.Fatalf("组 id 应为 %s，实际 %s", groupID, g.ID)
	}
	if len(g.Members) != 2 {
		t.Fatalf("组内应有 2 个成员（源 + 副本），实际 %d", len(g.Members))
	}
	if len(g.PairBases) != 1 {
		t.Fatalf("应有 1 条配对基线，实际 %d", len(g.PairBases))
	}
	for _, m := range g.Members {
		if m.State != MemberStateActive {
			t.Fatalf("新登记的成员都应是 active，实际 %s", m.State)
		}
		if m.MemberID == "" || m.SessionID == "" || m.Variant != "cn" {
			t.Fatalf("成员字段不完整：%+v", m)
		}
	}

	// 再复制给第三个账号：**复用同一个组**（按源身份找组）。
	res2, err := s.CopySession("src", CopyOptions{TargetUID: "uid-D", TargetVariant: "cn"})
	if err != nil {
		t.Fatalf("第二次复制失败: %v", err)
	}
	if res2.GroupID != groupID {
		t.Fatalf("同源复制应复用同一个组：期望 %s，实际 %s", groupID, res2.GroupID)
	}
	store2, _, _ := s.loadLinkStore()
	if len(store2.Groups) != 1 || len(store2.Groups[0].Members) != 3 {
		t.Fatalf("应仍是 1 个组、3 个成员，实际 %+v", store2.Groups)
	}

	// 幂等：目标账号已有有效副本 → alreadyLinked，不再复制。
	_, err = s.CopySession("src", CopyOptions{TargetUID: "uid-C", TargetVariant: "cn"})
	var linked *AlreadyLinkedError
	if err == nil || !asAlreadyLinked(err, &linked) {
		t.Fatalf("重复复制应返回 alreadyLinked，实际 %v", err)
	}
}

func asAlreadyLinked(err error, target **AlreadyLinkedError) bool {
	e, ok := err.(*AlreadyLinkedError)
	if ok {
		*target = e
	}
	return ok
}

// 组聚合：安全源 + 成员状态 + summary（对照 switch 的 aggregate_group_state）。
func TestRegistryGroupAggregation(t *testing.T) {
	s, groupID := setupLinkStore(t)
	// 再造一份**落后**副本：复制 src 给 uid-D，然后把副本正文截成前缀
	//（模拟「另一个账号还停在旧版本」）。
	res, err := s.CopySession("src", CopyOptions{TargetUID: "uid-D", TargetVariant: "cn"})
	if err != nil {
		t.Fatalf("复制落后副本失败: %v", err)
	}
	if err := os.WriteFile(res.BodyPath, []byte(buildJSONL(res.NewID, "a")), 0o600); err != nil {
		t.Fatal(err)
	}
	labels := map[string]AccountLabel{
		"uid-A": {Account: "a.json", Label: "甲", Site: "cn"},
		"uid-C": {Account: "c.json", Label: "丙", Site: "cn"},
		"uid-D": {Account: "d.json", Label: "丁", Site: "cn"},
	}
	view := s.GetRegistryGroup(groupID, labels)
	if view == nil {
		t.Fatal("组应存在")
	}
	if len(view.Members) != 3 {
		t.Fatalf("应有 3 个成员，实际 %d", len(view.Members))
	}
	// src/third 内容相同且最全 → 安全源是其中之一（按 accountKey 排序取第一个）。
	if view.SafeSourceMemberID == "" {
		t.Fatal("应有安全源")
	}
	// tgt 的副本落后 → behind；summary=behind（有落后副本）。
	statuses := map[string]string{}
	for _, m := range view.Members {
		statuses[m.Label] = m.VersionStatus
	}
	if statuses["丁"] != "behind" {
		t.Fatalf("丁（落后副本）应判 behind，实际 %v", statuses)
	}
	if view.Status != GroupStatusBehind {
		t.Fatalf("组状态应为 behind，实际 %s", view.Status)
	}
	if view.SummaryText != "有副本落后，可安全同步" {
		t.Fatalf("summary 文案应为 switch 的原文，实际 %q", view.SummaryText)
	}
}

// 按模式同步：fastForward 允许；diverge 必须以 overwrite 显式覆盖；ahead 需要 unifyOverwrite。
func TestSyncMemberPairModes(t *testing.T) {
	s, groupID := setupLinkStore(t)

	// 造一个落后成员：复制 src 给 uid-D，再把副本正文截成前缀。
	res, err := s.CopySession("src", CopyOptions{TargetUID: "uid-D", TargetVariant: "cn"})
	if err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	behindSessionID := res.NewID
	if err := os.WriteFile(res.BodyPath, []byte(buildJSONL(behindSessionID, "a")), 0o600); err != nil {
		t.Fatal(err)
	}
	store2, _, _ := s.loadLinkStore()
	g2 := findGroupR(store2, groupID)
	var srcM, behindM *LinkMember
	for i := range g2.Members {
		if g2.Members[i].SessionID == "src" {
			srcM = &g2.Members[i]
		}
		if g2.Members[i].SessionID == behindSessionID {
			behindM = &g2.Members[i]
		}
	}
	if srcM == nil || behindM == nil {
		t.Fatalf("复制后应能在组里找到两个成员：%+v", g2.Members)
	}

	// 0) 没有预览凭据 → 直接拒绝（不静默跳过、不无凭据写入）。
	if out := s.SyncMemberPair(SyncPairInput{Group: g2, Source: *srcM, Target: *behindM, Mode: ModeFastForward}); out.Error == "" {
		t.Fatalf("缺少预览凭据时必须拒绝，实际 %+v", out)
	}

	// 1) 落后副本 + fastForward（带预览凭据）→ 写入成功。
	preview1 := mustPreviewToken(t, s, groupID, srcM.MemberID, behindM.MemberID)
	out := s.SyncMemberPair(SyncPairInput{Group: g2, Source: *srcM, Target: *behindM, Mode: ModeFastForward, PreviewToken: preview1})
	if out.Error != "" || !out.Applied {
		t.Fatalf("快进应成功，实际 %+v", out)
	}
	// 2) 再同步一次 → identical，不写。
	preview2 := mustPreviewToken(t, s, groupID, srcM.MemberID, behindM.MemberID)
	out2 := s.SyncMemberPair(SyncPairInput{Group: g2, Source: *srcM, Target: *behindM, Mode: ModeFastForward, PreviewToken: preview2})
	if out2.Applied || out2.Reason == "" {
		t.Fatalf("已一致时应跳过并给出原因，实际 %+v", out2)
	}
	// 3) 目标单方面新增 → ahead；fastForward 不允许，unifyOverwrite 允许。
	dir := filepath.Join(s.Root, "projects", "d-ws")
	bodyPath := filepath.Join(dir, behindM.SessionID+".jsonl")
	cur, _ := os.ReadFile(bodyPath)
	extra := `{"type":"user","sessionId":"` + behindM.SessionID + `","id":"mZ","text":"目标独有"}` + "\n"
	if err := os.WriteFile(bodyPath, append([]byte(extra), cur...), 0o600); err != nil {
		t.Fatal(err)
	}
	store3, _, _ := s.loadLinkStore()
	g3 := findGroupR(store3, groupID)
	// 3a) 内容在预览之后变了 → 旧凭据必须失配（不沿用旧结论）。
	out3 := s.SyncMemberPair(SyncPairInput{Group: g3, Source: *srcM, Target: *behindM, Mode: ModeFastForward, PreviewToken: preview2})
	if out3.Applied {
		t.Fatal("预览过期时不该写")
	}
	if out3.ReasonCode != PreviewStaleReasonCode || out3.Reason == "" {
		t.Fatalf("预览过期应带 previewStale 原因码与说明，实际 %+v", out3)
	}
	// 3b) 重新预览：ahead 时 fastForward 被拒。
	preview3 := mustPreviewToken(t, s, groupID, srcM.MemberID, behindM.MemberID)
	out4 := s.SyncMemberPair(SyncPairInput{Group: g3, Source: *srcM, Target: *behindM, Mode: ModeFastForward, PreviewToken: preview3})
	if out4.Applied || out4.Error == "" {
		t.Fatalf("ahead 时 fastForward 应被拒并说明原因，实际 %+v", out4)
	}
	// 3c) 同一份凭据 + unifyOverwrite（ahead 的显式覆盖入口）→ 覆盖成功。
	//     凭据里记的判定是 ahead，与实时判定一致，因此不会失配。
	out5 := s.SyncMemberPair(SyncPairInput{Group: g3, Source: *srcM, Target: *behindM, Mode: ModeUnifyOverwrite, PreviewToken: preview3})
	if out5.Error != "" || !out5.Applied {
		t.Fatalf("unifyOverwrite 应覆盖 ahead 目标，实际 %+v", out5)
	}
}

// mustPreviewToken 预览一对成员并返回服务端签发的凭据 id。
func mustPreviewToken(t *testing.T, s *Store, groupID, srcMemberID, tgtMemberID string) string {
	t.Helper()
	p, err := s.PreviewPair(groupID, srcMemberID, tgtMemberID)
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if p.PreviewToken == "" {
		t.Fatal("预览必须签发凭据 id")
	}
	return p.PreviewToken
}

// 解除/删除关联：只删登记项，内容不动；成员清空时组被删除。
func TestUnlinkAndDeleteGroup(t *testing.T) {
	s, groupID := setupLinkStore(t)
	store, _, _ := s.loadLinkStore()
	g := findGroupR(store, groupID)
	if len(g.Members) != 2 {
		t.Fatalf("前置：应有 2 个成员，实际 %d", len(g.Members))
	}
	memberID := g.Members[0].MemberID
	sessionID := g.Members[0].SessionID
	bodyBefore, err := os.ReadFile(filepath.Join(s.Root, "projects", "d-ws", sessionID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.UnlinkMember(groupID, memberID)
	if err != nil {
		t.Fatalf("取消关联失败: %v", err)
	}
	if res.Remaining != 1 || res.GroupRemoved {
		t.Fatalf("解除一个后应剩 1 个且组仍在，实际 %+v", res)
	}
	// 内容未动。
	bodyAfter, _ := os.ReadFile(filepath.Join(s.Root, "projects", "d-ws", sessionID+".jsonl"))
	if string(bodyBefore) != string(bodyAfter) {
		t.Fatal("解除关联不该改动会话内容")
	}
	// 配对基线随成员一起删（不变量：pairBases 引用的成员必须存在）。
	store2, _, _ := s.loadLinkStore()
	g2 := findGroupR(store2, groupID)
	for _, p := range g2.PairBases {
		if p.MemberIDs[0] == memberID || p.MemberIDs[1] == memberID {
			t.Fatal("解除成员后不该残留引用它的配对基线")
		}
	}

	// 删除整组：组消失。
	rest := g2.Members[0].MemberID
	if _, err := s.UnlinkMember(groupID, rest); err != nil {
		t.Fatalf("解除最后一个成员失败: %v", err)
	}
	store3, _, _ := s.loadLinkStore()
	if findGroupR(store3, groupID) != nil {
		t.Fatal("成员清空后组应被删除")
	}
}

// 登记表损坏时必须报错，绝不降级成空表（否则会覆盖用户的关联记录）。
func TestLinkStoreCorruptionFailsLoudly(t *testing.T) {
	s, _ := setupLinkStore(t)
	if err := os.WriteFile(s.linkFile(), []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListRegistryGroups(nil); err == nil {
		t.Fatal("登记表损坏时应报错")
	}
	if _, err := s.CopySession("src", CopyOptions{TargetUID: "uid-D"}); err == nil {
		t.Fatal("登记表损坏时复制也应拒绝（不能拿空表继续写）")
	}
}

// 一次性迁移：既有同源副本（内容推导时代留下的组）登记进表，之后以登记表为准。
func TestBootstrapRegistryFromExistingCopies(t *testing.T) {
	s, _, _ := setupSyncStore(t)
	dir := filepath.Join(s.Root, "projects", "d-ws")
	if err := os.WriteFile(filepath.Join(dir, "third.jsonl"), []byte(buildJSONL("third", "a", "b", "c")), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	now := int64(1759000000000)
	if _, err := db.Exec(`INSERT INTO sessions VALUES
		(?,?,'uid-C','t',NULL,'active',?,?,?,0,0,'cli',0,'code','m','')`,
		"third", `D:\ws`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET user_id='uid-A' WHERE id IN ('src','tgt')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	labels := map[string]AccountLabel{
		"uid-A": {Account: "a.json", Label: "甲", Site: "cn"},
		"uid-C": {Account: "c.json", Label: "丙", Site: "cn"},
	}
	if err := s.BootstrapRegistryIfNeeded(labels); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	groups, err := s.ListRegistryGroups(labels)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("应登记 1 个组（src/third 同源；tgt 只有前缀但会并进同一簇），实际 %d", len(groups))
	}
	if len(groups[0].Members) < 2 {
		t.Fatalf("组内应至少 2 个成员，实际 %+v", groups[0].Members)
	}
	// 迁移只跑一次：删掉登记表后 marker 仍在 → 不再重建。
	if err := os.Remove(s.linkFile()); err != nil {
		t.Fatal(err)
	}
	if err := s.BootstrapRegistryIfNeeded(labels); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.linkFile()); err == nil {
		t.Fatal("marker 已存在时不该再次迁移")
	}
}
