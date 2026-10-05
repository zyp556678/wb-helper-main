package extsessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// -----------------------------------------------------------------------------
// 扩展数据仓会话子系统：夹具端到端（复制 / 登记 / 预览 / 同步）
//
// 全部在临时目录里构造数据仓，绝不读写真实的 %LOCALAPPDATA%\CodeBuddyExtension。
// -----------------------------------------------------------------------------

const (
	wsHash   = "0123456789abcdef0123456789abcdef"
	convID   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	msgID1   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	msgID2   = "cccccccccccccccccccccccccccccccc"
	srcUID   = "11111111-1111-1111-1111-111111111111"
	tgtUID   = "22222222-2222-2222-2222-222222222222"
	otherUID = "33333333-3333-3333-3333-333333333333"
)

// setupExtFixture 造一个含两条同源会话的扩展数据仓（源账号 + 目标账号）。
func setupExtFixture(t *testing.T, spec Spec) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(root, "ext-data")
	testDataRootOverride = &override
	t.Cleanup(func() { testDataRootOverride = nil })
	if err := os.MkdirAll(override, 0o700); err != nil {
		t.Fatal(err)
	}

	writeConversationFixture(t, spec, override, srcUID, wsHash, convID, "排查后台网关命令失败问题", []string{"第一条：网关报 502", "第二条：先看上游响应头"})
	// 目标账号先有一条**无关**会话，用于验证「沿用 id」路径不会误伤它。
	writeConversationFixture(t, spec, override, tgtUID, wsHash, "dddddddddddddddddddddddddddddddd", "目标账号原有会话", []string{"原有内容"})

	st := &Store{Spec: spec, Root: override, StateDir: state, BackupRoot: filepath.Join(state, "backups", spec.BackupKind)}
	return st, override
}

// writeConversationFixture 在工作区索引 + 会话目录里写一条会话。
func writeConversationFixture(t *testing.T, spec Spec, root, uid, workspaceHash, conversationID, title string, texts []string) {
	t.Helper()
	wsDir := filepath.Join(HistoryRoot(spec, root, uid), workspaceHash)
	if err := os.MkdirAll(filepath.Join(wsDir, conversationID, "messages"), 0o700); err != nil {
		t.Fatal(err)
	}
	messages := []any{}
	requests := []any{}
	for i, text := range texts {
		id := msgID1
		if i == 1 {
			id = msgID2
		}
		if i > 1 {
			id = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" + string(rune('0'+i))
		}
		messages = append(messages, map[string]any{"id": id})
		msg := map[string]any{
			"id": id, "role": "user", "message": text,
			"extra": map[string]any{"requestId": "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"},
		}
		encoded, _ := json.Marshal(msg)
		if err := os.WriteFile(filepath.Join(wsDir, conversationID, "messages", id+".json"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	requests = append(requests, map[string]any{"id": "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr", "messages": []any{msgID1, msgID2}})
	index := map[string]any{
		"conversations": []any{map[string]any{
			"id": conversationID, "type": "craft", "name": title, "createdAt": "2026-10-01T00:00:00Z",
			"lastMessageAt": "2026-10-02T00:00:00Z",
		}},
		"current": conversationID,
	}
	wsIndexPath := filepath.Join(wsDir, "index.json")
	existing, _ := ReadJSON(wsIndexPath).(map[string]any)
	if existing != nil {
		index = existing
		conversations, _ := index["conversations"].([]any)
		index["conversations"] = append(conversations, map[string]any{
			"id": conversationID, "type": "craft", "name": title, "createdAt": "2026-10-01T00:00:00Z",
			"lastMessageAt": "2026-10-02T00:00:00Z",
		})
	}
	wsIndex, _ := json.Marshal(index)
	if err := os.WriteFile(wsIndexPath, wsIndex, 0o600); err != nil {
		t.Fatal(err)
	}
	convIndex := map[string]any{"messages": messages, "requests": requests, "current": conversationID}
	encoded, _ := json.Marshal(convIndex)
	if err := os.WriteFile(filepath.Join(wsDir, conversationID, "index.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 内容身份：同源的两份副本（消息 id 不同）归一化后必须逐条摘要相同。
func TestExtContentIdentityIgnoresIDs(t *testing.T) {
	spec := VSCodeStore
	root := t.TempDir()
	testDataRootOverride = &root
	t.Cleanup(func() { testDataRootOverride = nil })

	writeConversationFixture(t, spec, root, srcUID, wsHash, convID, "会话", []string{"甲", "乙"})
	// 目标账号的同源副本：消息 id 与请求 id 全不同，内容相同。
	copyRoot := t.TempDir()
	writeConversationFixture(t, spec, copyRoot, tgtUID, wsHash, convID, "会话", []string{"甲", "乙"})

	src := ReadSessionContent(filepath.Join(HistoryRoot(spec, root, srcUID), wsHash, convID), convID)
	dst := ReadSessionContent(filepath.Join(HistoryRoot(spec, copyRoot, tgtUID), wsHash, convID), convID)
	if src.Kind != ContentReady || dst.Kind != ContentReady {
		t.Fatalf("两份都应可读：%+v %+v", src, dst)
	}
	if !EqualDigests(src.Snapshot.Normalized.LineDigests, dst.Snapshot.Normalized.LineDigests) {
		t.Fatal("同源副本（仅 id 不同）归一化后摘要必须相同 —— 否则同步判定全部失效")
	}
	if src.Snapshot.Normalized.TotalDigest != dst.Snapshot.Normalized.TotalDigest {
		t.Fatal("总摘要也必须相同")
	}
}

// 复制（插件策略）：副本取新 id、消息文件改名、索引合并、current 保持有效。
func TestExtCopyAlwaysNewIDs(t *testing.T) {
	spec := VSCodeStore
	st, root := setupExtFixture(t, spec)

	report, err := CopySessions(spec, VSCodeCopy, root, st.BackupRoot, srcUID, tgtUID, []CopyItem{
		{WorkspaceHash: wsHash, ConversationID: convID},
	})
	if err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	if len(report.Copied) != 1 || len(report.Errors) != 0 {
		t.Fatalf("应有 1 条成功、0 条失败，实际 %+v", report)
	}
	newID := report.Copied[0].NewID
	if !IsHex32(newID) || newID == convID {
		t.Fatalf("副本应取新的 32 位 hex id，实际 %q", newID)
	}
	// 源目录**一个字节都不能变**。
	srcIndex := ReadJSON(filepath.Join(HistoryRoot(spec, root, srcUID), wsHash, convID, "index.json"))
	if srcIndex == nil {
		t.Fatal("源会话索引不见了")
	}
	// 目标索引里应有两条（原有 + 副本），且 current 有效。
	wsIndex, _ := ReadJSON(filepath.Join(HistoryRoot(spec, root, tgtUID), wsHash, "index.json")).(map[string]any)
	conversations := jsonArray(wsIndex["conversations"])
	if len(conversations) != 2 {
		t.Fatalf("目标工作区索引应有 2 条，实际 %d", len(conversations))
	}
	current := strOf(wsIndex["current"])
	found := false
	for _, raw := range conversations {
		entry, _ := raw.(map[string]any)
		if strOf(entry["id"]) == current {
			found = true
		}
	}
	if !found {
		t.Fatal("current 必须指向索引内真实存在的会话（缺 current 的索引会被扩展判定为损坏）")
	}
	// 副本内容应与源同源（归一化后一致）。
	src := ReadSessionContent(filepath.Join(HistoryRoot(spec, root, srcUID), wsHash, convID), convID)
	dst := ReadSessionContent(filepath.Join(HistoryRoot(spec, root, tgtUID), wsHash, newID), newID)
	if !EqualDigests(src.Snapshot.Normalized.LineDigests, dst.Snapshot.Normalized.LineDigests) {
		t.Fatal("副本与源必须同源（归一化摘要一致）")
	}
	// 原有会话未被改动。
	original := ReadSessionContent(filepath.Join(HistoryRoot(spec, root, tgtUID), wsHash, "dddddddddddddddddddddddddddddddd"), "dddddddddddddddddddddddddddddddd")
	if original.Kind != ContentReady {
		t.Fatal("目标账号原有会话被破坏了")
	}
}

// 复制（IDE 策略）：沿用源 id；冲突时才重随机并改写引用。
func TestExtCopyKeepUnlessConflict(t *testing.T) {
	spec := IDEStore
	st, root := setupExtFixture(t, spec)

	// 无冲突：沿用源 id。
	report, err := CopySessions(spec, IDECopy, root, st.BackupRoot, srcUID, otherUID, []CopyItem{
		{WorkspaceHash: wsHash, ConversationID: convID},
	})
	if err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	if len(report.Copied) != 1 || report.Copied[0].NewID != convID {
		t.Fatalf("无冲突时应沿用源 id，实际 %+v", report.Copied)
	}

	// 有冲突：同一个目标账号再复制一次（它已经有同 id 的会话）→ 重随机，且不覆盖既有内容。
	before := ReadSessionContent(filepath.Join(HistoryRoot(spec, root, otherUID), wsHash, convID), convID)
	report2, err := CopySessions(spec, IDECopy, root, st.BackupRoot, srcUID, otherUID, []CopyItem{
		{WorkspaceHash: wsHash, ConversationID: convID},
	})
	if err != nil {
		t.Fatalf("冲突复制失败: %v", err)
	}
	if len(report2.Copied) != 1 || report2.Copied[0].NewID == convID {
		t.Fatalf("冲突时应重随机会话 id，实际 %+v", report2.Copied)
	}
	after := ReadSessionContent(filepath.Join(HistoryRoot(spec, root, otherUID), wsHash, convID), convID)
	if !EqualDigests(before.Snapshot.Normalized.LineDigests, after.Snapshot.Normalized.LineDigests) {
		t.Fatal("目标账号既有会话被覆盖了")
	}
}

// 登记 + 预览 + 同步：复制后登记关联，落后副本可快进同步，凭据失配时跳过。
func TestExtRegisterPreviewAndSync(t *testing.T) {
	spec := VSCodeStore
	st, root := setupExtFixture(t, spec)

	report, err := CopySessions(spec, VSCodeCopy, root, st.BackupRoot, srcUID, tgtUID, []CopyItem{
		{WorkspaceHash: wsHash, ConversationID: convID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if linkErrors := st.RegisterCopiedSessions("cn", report); len(linkErrors) != 0 {
		t.Fatalf("登记失败: %+v", linkErrors)
	}
	groups, err := st.ListGroups(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Members) != 2 {
		t.Fatalf("应有 1 个组、2 个成员，实际 %+v", groups)
	}
	if groups[0].Status != "latest" {
		t.Fatalf("刚复制完应为内容一致，实际 %s（%s）", groups[0].Status, groups[0].SummaryText)
	}

	// 把目标副本改成落后（删掉一条消息）→ 组状态应为 behind，且可快进。
	newID := report.Copied[0].NewID
	targetConvDir := filepath.Join(HistoryRoot(spec, root, tgtUID), wsHash, newID)
	trimConversation(t, targetConvDir, 1)

	groups, _ = st.ListGroups(nil)
	if groups[0].Status != "behind" {
		t.Fatalf("目标少一条后应为 behind，实际 %s", groups[0].Status)
	}
	view := st.GetGroup(groups[0].ID, nil)
	srcMember := view.SafeSource
	if srcMember == "" {
		t.Fatal("应有安全源")
	}
	var tgtMember string
	for _, m := range view.Members {
		if m.MemberID != srcMember {
			tgtMember = m.MemberID
		}
	}
	preview, err := st.PreviewPair(view.ID, srcMember, tgtMember)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Verdict != "fastForward" || len(preview.AvailableModes) == 0 {
		t.Fatalf("应为可快进，实际 %+v", preview)
	}
	SetTargetUID(tgtUID)
	out := st.SyncMemberPair(view.ID, srcMember, tgtMember, "fastForward", preview.PreviewToken)
	if out.Error != "" || !out.Applied {
		t.Fatalf("快进应成功，实际 %+v", out)
	}
	// 同步后内容一致。
	after := ReadSessionContent(targetConvDir, newID)
	if after.Kind != ContentReady {
		t.Fatalf("同步后目标不可读：%+v", after)
	}
	groups, _ = st.ListGroups(nil)
	if groups[0].Status != "latest" {
		t.Fatalf("同步后应回到一致，实际 %s（%s）", groups[0].Status, groups[0].SummaryText)
	}

	// 用**过期凭据**再同步一次：必须跳过（不沿用旧结论）。
	trimConversation(t, targetConvDir, 1)
	stale := st.SyncMemberPair(view.ID, srcMember, tgtMember, "fastForward", preview.PreviewToken)
	if stale.Applied || stale.ReasonCode != "previewStale" {
		t.Fatalf("凭据失配应跳过并带 previewStale，实际 %+v", stale)
	}
	// 伪造凭据：直接拒绝。
	forged := st.SyncMemberPair(view.ID, srcMember, tgtMember, "fastForward", "11111111-2222-3333-4444-555555555555")
	if forged.Applied || forged.Error == "" {
		t.Fatalf("伪造凭据应被拒绝，实际 %+v", forged)
	}
}

// trimConversation 把会话改成只剩前 n 条消息（模拟「目标落后」）。
func trimConversation(t *testing.T, convDir string, n int) {
	t.Helper()
	index, _ := ReadJSON(filepath.Join(convDir, "index.json")).(map[string]any)
	messages := jsonArray(index["messages"])
	if len(messages) <= n {
		return
	}
	index["messages"] = messages[:n]
	encoded, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(convDir, "index.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 未找到数据目录时列表为空（不是报错）。
func TestExtListWithoutDataRoot(t *testing.T) {
	spec := VSCodeStore
	empty := filepath.Join(t.TempDir(), "nope")
	testDataRootOverride = &empty
	t.Cleanup(func() { testDataRootOverride = nil })
	st := &Store{Spec: spec, Root: "", StateDir: t.TempDir()}
	if st.Available() {
		t.Fatal("数据目录不存在时应报不可用")
	}
	groups, err := st.ListGroups(nil)
	if err != nil {
		t.Fatalf("没有数据目录不该报错：%v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("应为空，实际 %+v", groups)
	}
}
