package extsessions

// -----------------------------------------------------------------------------
// 关联登记与同步（对照 vscode_session_sync.rs / codebuddy_ide_session_sync.rs）
//
// 与 WorkBuddy 侧**分层同构但不共用编排**：内核（关联表 / 配对基线 / 判定 / 预览凭据）
// 复用 `internal/localsessions` 暴露出来的那套（见 localsessions/kernel.go），
// 存储按命名空间与 WorkBuddy 完全隔离；内容身份与写入形态则是扩展专属的
//（一条消息一个文件、按记录顺序摘要、整体重建）。
//
// 触发时机：只在编辑器**已关闭**之后执行；顺序固定为「复制 → 登记关联 → 执行同步」。
// 覆盖模式靠整目录备份收场。
// -----------------------------------------------------------------------------

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"workbuddy-gateway/internal/localsessions"
)

// Store 是一个扩展数据仓的会话侧句柄。
type Store struct {
	Spec Spec
	// Root 是扩展数据根（`...\CodeBuddyExtension\Data`）。
	Root string
	// StateDir 是本工具的状态目录（关联登记表落点）。
	StateDir string
	// BackupRoot 是备份根（`<StateDir>/backups/<BackupKind>/<utc>`）。
	BackupRoot string
}

// NewStore 构造句柄（Root 为空表示没找到数据目录）。
func NewStore(spec Spec, stateDir string) *Store {
	return &Store{
		Spec:     spec,
		Root:     DataRoot(spec),
		StateDir: stateDir,
	}
}

// linkStore 返回该命名空间的登记表句柄（只用到 StateDir 与 Namespace）。
func (s *Store) linkStore() *localsessions.Store {
	return &localsessions.Store{
		StateDir:  s.StateDir,
		Namespace: localsessions.Namespace(s.Spec.Namespace),
	}
}

// Available 表示数据目录存在（否则界面上不该给出入口）。
func (s *Store) Available() bool { return s.Root != "" }

// conversationDir 定位一条会话的目录。
func (s *Store) conversationDir(uid, workspaceHash, conversationID string) string {
	return filepath.Join(HistoryRoot(s.Spec, s.Root, uid), workspaceHash, conversationID)
}

// LinkError 是一条登记失败（登记失败不回滚复制，只并入报告）。
type LinkError struct {
	WorkspaceHash  string `json:"workspace_hash"`
	ConversationID string `json:"conversation_id"`
	Error          string `json:"error"`
}

// RegisterCopiedSessions 为本次复制成功的会话登记「源 ↔ 副本」关联。
//
// **登记失败不回滚复制**：复制已完成、文件已落盘；失败原因由调用方并入报告。
func (s *Store) RegisterCopiedSessions(variant string, report *CopyReport) []LinkError {
	if report == nil {
		return nil
	}
	store := s.linkStore()
	if err := store.EnsureLinkStoreReady(); err != nil {
		return []LinkError{{Error: err.Error()}}
	}
	var linkErrors []LinkError
	for _, item := range report.Copied {
		if err := s.registerOne(store, variant, report.SourceUID, report.TargetUID, item); err != nil {
			linkErrors = append(linkErrors, LinkError{
				WorkspaceHash: item.WorkspaceHash, ConversationID: item.OldID, Error: err.Error(),
			})
		}
	}
	return linkErrors
}

// registerOne 登记一条复制（对照 commit_links 的语义：组复用、成员 supersede、配对基线、继承）。
func (s *Store) registerOne(store *localsessions.Store, variant, sourceUID, targetUID string, item CopyResultItem) error {
	sourceContent := ReadSessionContent(s.conversationDir(sourceUID, item.WorkspaceHash, item.OldID), item.OldID)
	targetContent := ReadSessionContent(s.conversationDir(targetUID, item.WorkspaceHash, item.NewID), item.NewID)
	var digests []string
	if sourceContent.Kind == ContentReady {
		digests = sourceContent.Snapshot.Normalized.LineDigests
	} else if targetContent.Kind == ContentReady {
		digests = targetContent.Snapshot.Normalized.LineDigests
	}
	return store.WithLinkStoreWrite(func(file *localsessions.LinkStoreFile) error {
		group := localsessions.FindGroupForIdentity(file, variant, sourceUID, item.OldID)
		if group == nil {
			// 目标账号已有该会话的另一份副本时，按**目标身份**复用它的组（跨档组的身份基础）。
			group = localsessions.FindGroupForIdentity(file, variant, targetUID, item.NewID)
		}
		if group == nil {
			file.Groups = append(file.Groups, localsessions.LinkGroupR{
				ID: localsessions.MustNewUUID(), Variant: variant, CreatedAt: localsessions.NowMillis(),
				Members: []localsessions.LinkMember{}, PairBases: []localsessions.PairBase{},
			})
			group = &file.Groups[len(file.Groups)-1]
		}
		sourceMember := localsessions.FindMemberInGroup(group, sourceUID, item.OldID)
		if sourceMember == nil {
			localsessions.AddActiveMember(group, localsessions.LinkMember{
				MemberID: localsessions.MustNewUUID(), UID: sourceUID, SessionID: item.OldID,
				Variant: variant, State: localsessions.MemberStateActive, LinkedAt: localsessions.NowMillis(),
			})
			sourceMember = localsessions.FindMemberInGroup(group, sourceUID, item.OldID)
		}
		targetMember := localsessions.FindMemberInGroup(group, targetUID, item.NewID)
		if targetMember == nil {
			localsessions.AddActiveMember(group, localsessions.LinkMember{
				MemberID: localsessions.MustNewUUID(), UID: targetUID, SessionID: item.NewID,
				Variant: variant, State: localsessions.MemberStateActive, LinkedAt: localsessions.NowMillis(),
			})
			targetMember = localsessions.FindMemberInGroup(group, targetUID, item.NewID)
		} else {
			targetMember.State = localsessions.MemberStateActive
		}
		if len(digests) == 0 || sourceMember == nil || targetMember == nil {
			localsessions.SortPairBases(group)
			return nil
		}
		ref := localsessions.MustNewUUID()
		if err := store.SaveBaselineRecord(&localsessions.BaselineRecord{
			Version: localsessions.LinkStoreVersion, BaselineRef: ref,
			NormalizationVersion: localsessions.NormalizationVersion, CreatedAt: localsessions.NowMillis(),
			RecordCount: len(digests), TotalDigest: totalDigestOf(digests), LineDigests: digests,
		}); err != nil {
			return err
		}
		localsessions.SetPairBase(group, sourceMember.MemberID, targetMember.MemberID, ref)
		// 继承：源与组内其它成员已有的基线，只有在「新成员正文包含该基线」时才建立到新成员。
		for _, other := range group.Members {
			if other.MemberID == sourceMember.MemberID || other.MemberID == targetMember.MemberID {
				continue
			}
			if localsessions.FindPairBase(group, other.MemberID, targetMember.MemberID) != nil {
				continue
			}
			pair := localsessions.FindPairBase(group, sourceMember.MemberID, other.MemberID)
			if pair == nil {
				continue
			}
			if record := store.InheritableBaseline(*pair, digests); record != nil {
				localsessions.SetPairBase(group, other.MemberID, targetMember.MemberID, record.BaselineRef)
			}
		}
		localsessions.SortPairBases(group)
		return nil
	})
}

// -----------------------------------------------------------------------------
// 组视图（列表 / 详情）
// -----------------------------------------------------------------------------

// MemberView 是组内成员的展示视图（字段与 WorkBuddy 侧同名，前端复用同一套组件）。
type MemberView struct {
	MemberID      string                               `json:"member_id"`
	SessionID     string                               `json:"session_id"`
	UID           string                               `json:"uid"`
	Label         string                               `json:"label"`
	Variant       string                               `json:"variant"`
	WorkspaceHash string                               `json:"workspace_hash"`
	State         string                               `json:"state"`
	UpdatedAt     int64                                `json:"updated_at"`
	RecordCount   int                                  `json:"record_count"`
	Readable      bool                                 `json:"readable"`
	VersionStatus string                               `json:"version_status"`
	Reason        string                               `json:"reason"`
	Preview       []localsessions.PreviewMemberBinding `json:"-"`
}

// GroupView 是聚合后的组视图。
type GroupView struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Status      string       `json:"status"`
	SummaryText string       `json:"summary_text"`
	SafeSource  string       `json:"safe_source_member_id"`
	Members     []MemberView `json:"members"`
	UpdatedAt   int64        `json:"updated_at"`
}

// memberFacts 读成员内容并折算成判定输入。
func (s *Store) memberFacts(m localsessions.LinkMember) (localsessions.ContentFacts, *ContentSnapshot, string) {
	dir := s.conversationDir(m.UID, "", "")
	// 会话目录按「uid/history/<ws>/<conv>」组织，成员没记工作区 hash 时全量搜索。
	path := s.findConversationDir(m.UID, m.SessionID)
	_ = dir
	if path == "" {
		return localsessions.FactsMissing(), nil, ""
	}
	state := ReadSessionContent(path, m.SessionID)
	switch state.Kind {
	case ContentReady:
		return localsessions.FactsFromNormalized(&localsessions.NormalizedContent{
			RecordCount: state.Snapshot.Normalized.RecordCount,
			LineDigests: state.Snapshot.Normalized.LineDigests,
			TotalDigest: state.Snapshot.Normalized.TotalDigest,
		}), state.Snapshot, path
	case ContentUnavailable:
		return localsessions.FactsUnavailable(state.Reason), nil, path
	}
	return localsessions.FactsMissing(), nil, ""
}

// findConversationDir 在账号的历史根下按会话 id 找目录（工作区 hash 未知时全量搜索）。
func (s *Store) findConversationDir(uid, conversationID string) string {
	history := HistoryRoot(s.Spec, s.Root, uid)
	entries, err := os.ReadDir(history)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(history, entry.Name(), conversationID)
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
	}
	return ""
}

// memberSnapshotFor 读一份副本的预览绑定值（原始摘要 + 归一化摘要 + 记录数）。
func (s *Store) memberSnapshotFor(m localsessions.LinkMember) localsessions.PreviewMemberBinding {
	facts, snapshot, _ := s.memberFacts(m)
	raw := ""
	normalized := ""
	count := 0
	if snapshot != nil {
		raw = snapshot.FullDigest
		normalized = snapshot.Normalized.TotalDigest
		count = snapshot.Normalized.RecordCount
	}
	_ = facts
	return localsessions.NewPreviewMemberBinding(m.MemberID, m.AccountID, m.UID, m.SessionID, raw, normalized, count)
}

// ListGroups 聚合全部组（按更新时间倒序）。
//
// 状态与文案与 WorkBuddy 侧同一套口径：latest/behind/diverge/missing/unknown。
func (s *Store) ListGroups(labels map[string]string) ([]GroupView, error) {
	store := s.linkStore()
	file, _, err := store.LoadLinkStore()
	if err != nil {
		return nil, err
	}
	out := make([]GroupView, 0, len(file.Groups))
	for i := range file.Groups {
		out = append(out, s.buildGroupView(store, &file.Groups[i], labels))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// GetGroup 取单个组视图（不存在返回 nil）。
func (s *Store) GetGroup(groupID string, labels map[string]string) *GroupView {
	store := s.linkStore()
	file, _, err := store.LoadLinkStore()
	if err != nil {
		return nil
	}
	group := localsessions.FindGroupByID(file, groupID)
	if group == nil {
		return nil
	}
	view := s.buildGroupView(store, group, labels)
	return &view
}

// buildGroupView 聚合一个组（成员状态 + 安全源 + 摘要文案）。
func (s *Store) buildGroupView(store *localsessions.Store, group *localsessions.LinkGroupR, labels map[string]string) GroupView {
	view := GroupView{ID: group.ID, Members: []MemberView{}}
	facts := make([]localsessions.ContentFacts, len(group.Members))
	for i, m := range group.Members {
		f, _, path := s.memberFacts(m)
		facts[i] = f
		mv := MemberView{
			MemberID: m.MemberID, SessionID: m.SessionID, UID: m.UID,
			Variant: m.Variant, State: m.State,
		}
		if label, ok := labels[m.UID]; ok && label != "" {
			mv.Label = label
		} else {
			mv.Label = m.UID
		}
		if path != "" {
			mv.WorkspaceHash = filepath.Base(filepath.Dir(path))
			if st, err := os.Stat(filepath.Join(path, "index.json")); err == nil {
				mv.UpdatedAt = st.ModTime().UnixMilli()
			}
		}
		if f.Missing {
			mv.VersionStatus = "missing"
			mv.Reason = "内容不存在（该账号下找不到这条会话）"
		} else if f.Unavailable != "" {
			mv.VersionStatus = "unknown"
			mv.Reason = "内容无法确认：" + f.Unavailable
		} else {
			mv.Readable = true
			mv.RecordCount = f.RecordCount
		}
		view.Members = append(view.Members, mv)
	}

	// 逐对判定：谁是最全的那份、谁落后、谁分叉。
	active := []int{}
	for i, m := range group.Members {
		if m.State == localsessions.MemberStateActive && facts[i].Unavailable == "" && !facts[i].Missing {
			active = append(active, i)
		}
	}
	safeIdx := -1
	for _, a := range active {
		okAll := true
		for _, b := range active {
			if a == b {
				continue
			}
			baseline, bState, bUnusable := store.BaselineState(group, group.Members[a].MemberID, group.Members[b].MemberID)
			d := localsessions.DecideSyncFacts(facts[a], facts[b], baseline, bState, bUnusable)
			if d.Verdict != localsessions.VerdictIdentical && d.Verdict != localsessions.VerdictFastForward {
				okAll = false
				break
			}
		}
		if okAll {
			safeIdx = a
			break
		}
	}
	if safeIdx >= 0 {
		view.SafeSource = group.Members[safeIdx].MemberID
		for i, mv := range view.Members {
			if i == safeIdx || !mv.Readable {
				continue
			}
			baseline, bState, bUnusable := store.BaselineState(group, group.Members[safeIdx].MemberID, group.Members[i].MemberID)
			d := localsessions.DecideSyncFacts(facts[safeIdx], facts[i], baseline, bState, bUnusable)
			switch d.Verdict {
			case localsessions.VerdictIdentical:
				view.Members[i].VersionStatus = "latest"
				view.Members[i].Reason = d.Reason
			case localsessions.VerdictFastForward:
				view.Members[i].VersionStatus = "behind"
				view.Members[i].Reason = d.Reason
			default:
				view.Members[i].VersionStatus = "diverge"
				view.Members[i].Reason = d.Reason
			}
		}
		view.Members[safeIdx].VersionStatus = "latest"
		view.Members[safeIdx].Reason = fmt.Sprintf("内容最全（%d 条）", facts[safeIdx].RecordCount)
	} else {
		for i := range view.Members {
			if view.Members[i].VersionStatus == "" {
				view.Members[i].VersionStatus = "unknown"
				if view.Members[i].Reason == "" {
					view.Members[i].Reason = "组内没有可安全同步的来源（内容互有分歧或正文不可读）"
				}
			}
		}
	}

	// 组状态与摘要文案（与 WorkBuddy 侧逐字一致）。
	statuses := map[string]int{}
	for _, mv := range view.Members {
		if mv.State != localsessions.MemberStateActive {
			continue
		}
		statuses[mv.VersionStatus]++
	}
	switch {
	case len(active) == 0:
		view.Status = "unknown"
		view.SummaryText = "暂时无法确认副本状态"
	case statuses["diverge"] > 0:
		view.Status = "diverge"
		view.SummaryText = "多个副本有不同更新，需要选择来源"
	case statuses["missing"] > 0:
		view.Status = "missing"
		view.SummaryText = "有副本内容缺失"
	case statuses["behind"] > 0:
		view.Status = "behind"
		view.SummaryText = "有副本落后，可安全同步"
	case statuses["unknown"] > 0:
		view.Status = "unknown"
		view.SummaryText = "暂时无法确认副本状态"
	default:
		view.Status = "latest"
		view.SummaryText = "关联副本内容一致"
	}
	// 标题与更新时间取记录数最多的可读成员。
	best := -1
	for i, mv := range view.Members {
		if !mv.Readable {
			continue
		}
		if best < 0 || mv.RecordCount > view.Members[best].RecordCount {
			best = i
		}
		if mv.UpdatedAt > view.UpdatedAt {
			view.UpdatedAt = mv.UpdatedAt
		}
	}
	if best >= 0 {
		view.Title = conversationTitle(s, group.Members[best])
	}
	if view.Title == "" {
		view.Title = "(无标题)"
	}
	return view
}

// conversationTitle 读会话在工作区索引里的标题（找不到时退回空串）。
func conversationTitle(s *Store, m localsessions.LinkMember) string {
	path := s.findConversationDir(m.UID, m.SessionID)
	if path == "" {
		return ""
	}
	wsIndex, ok := ReadJSON(filepath.Join(filepath.Dir(path), "index.json")).(map[string]any)
	if !ok {
		return ""
	}
	if entry := findConversation(wsIndex, m.SessionID); entry != nil {
		if name := strings.TrimSpace(strOf(entry["name"])); name != "" {
			return name
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// 预览与同步
// -----------------------------------------------------------------------------

// PreviewResult 是一次成员对预览（判定 + 服务端凭据 id）。
type PreviewResult struct {
	Verdict        string   `json:"verdict"`
	Reason         string   `json:"reason"`
	SourceOnly     int      `json:"source_only"`
	TargetOnly     int      `json:"target_only"`
	AvailableModes []string `json:"available_modes"`
	PreviewToken   string   `json:"preview_token"`
}

// PreviewPair 预览一对成员的同步判定，并签发服务端凭据。
func (s *Store) PreviewPair(groupID, sourceMemberID, targetMemberID string) (*PreviewResult, error) {
	store := s.linkStore()
	file, _, err := store.LoadLinkStore()
	if err != nil {
		return nil, err
	}
	group := localsessions.FindGroupByID(file, groupID)
	if group == nil {
		return nil, errf("会话组不存在")
	}
	src, tgt := findMembers(group, sourceMemberID, targetMemberID)
	if src == nil || tgt == nil {
		return nil, errf("来源成员不在该会话组内")
	}
	srcFacts, _, _ := s.memberFacts(*src)
	tgtFacts, _, _ := s.memberFacts(*tgt)
	baseline, bState, bUnusable := store.BaselineState(group, src.MemberID, tgt.MemberID)
	decision := localsessions.DecideSyncFacts(srcFacts, tgtFacts, baseline, bState, bUnusable)
	binding := s.liveBinding(store, group, *src, *tgt, decision.Verdict)
	token, err := store.SavePreviewToken(binding)
	if err != nil {
		return nil, err
	}
	return &PreviewResult{
		Verdict: decision.Verdict, Reason: decision.Reason,
		SourceOnly: decision.SourceOnly, TargetOnly: decision.TargetOnly,
		AvailableModes: decision.AvailableModes, PreviewToken: token,
	}, nil
}

// liveBinding 用实时状态构造预览绑定（预览与执行时都调用它，两边可比）。
func (s *Store) liveBinding(store *localsessions.Store, group *localsessions.LinkGroupR, src, tgt localsessions.LinkMember, verdict string) localsessions.PreviewBinding {
	binding := localsessions.NewPreviewBinding(
		tgt.Variant, group.ID, localsessions.GroupFingerprint(group),
		s.memberSnapshotFor(src), s.memberSnapshotFor(tgt), "", "", 0, verdict,
	)
	if record, state, _ := store.BaselineState(group, src.MemberID, tgt.MemberID); state == "ready" && record != nil {
		binding = localsessions.NewPreviewBinding(
			tgt.Variant, group.ID, localsessions.GroupFingerprint(group),
			s.memberSnapshotFor(src), s.memberSnapshotFor(tgt),
			record.BaselineRef, record.TotalDigest, record.RecordCount, verdict,
		)
	}
	return binding
}

// SyncOutcome 是一次成员对同步的结果。
type SyncOutcome struct {
	MemberID   string   `json:"member_id"`
	Applied    bool     `json:"applied"`
	Reason     string   `json:"reason,omitempty"`
	ReasonCode string   `json:"reason_code,omitempty"`
	Error      string   `json:"error,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// SyncMemberPair 按模式把源成员同步到目标成员（执行前复核预览凭据）。
//
// 写入强度：会话文件与索引走原子写；覆盖前把目标会话整目录备份到
// `<BackupRoot>/overwrite/<utc>/<workspaceHash>/<targetConvId>-overwrite/`。
func (s *Store) SyncMemberPair(groupID, sourceMemberID, targetMemberID, mode, previewToken string) SyncOutcome {
	out := SyncOutcome{MemberID: targetMemberID}
	store := s.linkStore()
	file, _, err := store.LoadLinkStore()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	group := localsessions.FindGroupByID(file, groupID)
	if group == nil {
		out.Error = "会话组不存在"
		return out
	}
	src, tgt := findMembers(group, sourceMemberID, targetMemberID)
	if src == nil || tgt == nil {
		out.Error = "来源成员不在该会话组内"
		return out
	}
	if tgt.State != localsessions.MemberStateActive {
		out.Reason = "该成员不是当前接力成员，不再参与同步"
		return out
	}
	preview := store.LoadPreviewToken(previewToken)
	if preview == nil {
		out.Error = "检查结果不存在或已失效，请重新检查后再操作"
		return out
	}
	if preview.Binding.GroupID != groupID {
		out.Error = "检查结果与所选会话不匹配，已拒绝"
		return out
	}
	srcFacts, srcSnapshot, srcDir := s.memberFacts(*src)
	tgtFacts, _, tgtDir := s.memberFacts(*tgt)
	baseline, bState, bUnusable := store.BaselineState(group, src.MemberID, tgt.MemberID)
	decision := localsessions.DecideSyncFacts(srcFacts, tgtFacts, baseline, bState, bUnusable)
	live := s.liveBinding(store, group, *src, *tgt, decision.Verdict)
	if stale := localsessions.VerifyPreview(preview, &live); len(stale) > 0 {
		out.Reason = "检查结果已失效：" + strings.Join(stale, "；")
		out.ReasonCode = "previewStale"
		return out
	}
	if decision.Verdict == localsessions.VerdictIdentical {
		out.Reason = decision.Reason
		return out
	}
	if !modeAllows(decision.Verdict, mode) {
		out.Error = fmt.Sprintf("该副本判定为 %s，不允许以 %s 模式同步。%s", decision.Verdict, mode, decision.Reason)
		return out
	}
	if srcSnapshot == nil || srcDir == "" {
		out.Error = decision.Reason
		return out
	}
	if tgtDir == "" {
		out.Error = "目标内容不存在，未同步"
		return out
	}

	// 备份目标会话整目录（覆盖模式的安全网）。
	backupDir := filepath.Join(s.BackupRoot, "overwrite", UTCTimestamp(),
		filepath.Base(filepath.Dir(tgtDir)), tgt.SessionID+"-overwrite")
	if err := CopyDirRecursive(tgtDir, backupDir); err != nil {
		out.Error = "备份目标会话失败，已放弃同步：" + err.Error()
		return out
	}
	out.Notes = append(out.Notes, "已备份目标会话到 "+backupDir)

	// 覆盖 = 按源重建整份会话：消息与请求 id 用**确定性派生**（同一目标会话、同一序号、
	// 同一源内容 → 同一个 id），重复执行只覆盖同一批文件。
	if err := s.overwriteFromSource(srcDir, tgtDir, src.SessionID, tgt.SessionID); err != nil {
		out.Error = "写入目标会话失败（可用备份恢复）：" + err.Error()
		return out
	}
	after := ReadSessionContent(tgtDir, tgt.SessionID)
	if after.Kind != ContentReady {
		out.Error = "写后目标内容无法确认，未按成功处理"
		return out
	}
	if !EqualDigests(after.Snapshot.Normalized.LineDigests, srcSnapshot.Normalized.LineDigests) {
		out.Error = "写后校验失败：目标内容与来源归一化后仍不一致"
		return out
	}
	out.Applied = true
	out.Notes = append(out.Notes, "已重建目标会话并回读校验")

	// 写新的配对基线 + 更新 lastSyncedAt。
	digests := srcSnapshot.Normalized.LineDigests
	ref := localsessions.MustNewUUID()
	if err := store.SaveBaselineRecord(&localsessions.BaselineRecord{
		Version: localsessions.LinkStoreVersion, BaselineRef: ref,
		NormalizationVersion: localsessions.NormalizationVersion, CreatedAt: localsessions.NowMillis(),
		RecordCount: len(digests), TotalDigest: totalDigestOf(digests), LineDigests: digests,
	}); err != nil {
		out.Notes = append(out.Notes, "提示：同步记录写入失败："+err.Error())
		return out
	}
	if err := store.WithLinkStoreWrite(func(f *localsessions.LinkStoreFile) error {
		g := localsessions.FindGroupByID(f, groupID)
		if g == nil {
			return nil
		}
		localsessions.SetPairBase(g, src.MemberID, tgt.MemberID, ref)
		for i := range g.Members {
			if g.Members[i].MemberID == tgt.MemberID {
				now := localsessions.NowMillis()
				g.Members[i].LastSyncedAt = &now
			}
		}
		return nil
	}); err != nil {
		out.Notes = append(out.Notes, "提示：同步记录更新失败："+err.Error())
	}
	return out
}

// overwriteFromSource 用源会话整体重建目标会话（确定性派生 id + 盐重试）。
func (s *Store) overwriteFromSource(sourceDir, targetDir, sourceID, targetID string) error {
	sourceIndex, ok := ReadJSON(filepath.Join(sourceDir, "index.json")).(map[string]any)
	if !ok {
		return errf("源会话索引缺失或损坏")
	}
	// 盐重试：派生 id 与目标已有文件冲突时换一个盐（上限 8 次，与 switch 一致）。
	const saltLimit = 8
	for salt := 0; salt < saltLimit; salt++ {
		plan := s.derivePlan(sourceIndex, sourceDir, targetID, uint32(salt))
		if !planConflicts(plan, targetDir, targetID) {
			tmp := filepath.Join(filepath.Dir(targetDir), ".tmp-"+targetID)
			RemoveDirAllIfExists(tmp)
			if err := writeConversation(sourceDir, tmp, sourceIndex, plan); err != nil {
				RemoveDirAllIfExists(tmp)
				return err
			}
			if err := os.RemoveAll(targetDir); err != nil {
				RemoveDirAllIfExists(tmp)
				return err
			}
			if err := os.Rename(tmp, targetDir); err != nil {
				RemoveDirAllIfExists(tmp)
				return err
			}
			_ = sourceID
			return nil
		}
	}
	return errf("派生 id 连续冲突（已重试 %d 次），未写入", saltLimit)
}

// derivePlan 按确定性公式派生消息 / 请求 id（对照 derive_message_id / derive_request_id）。
func (s *Store) derivePlan(sourceIndex map[string]any, sourceDir, targetConversationID string, salt uint32) *RemapPlan {
	messageIDs := map[string]string{}
	requestIDs := map[string]string{}
	used := map[string]bool{}
	seq := 0
	for _, raw := range jsonArray(sourceIndex["messages"]) {
		msg, _ := raw.(map[string]any)
		id := strings.TrimSpace(strOf(msg["id"]))
		if id == "" {
			continue
		}
		digest := ""
		if bytes, err := os.ReadFile(filepath.Join(sourceDir, "messages", id+".json")); err == nil {
			digest = lineDigestOf(string(bytes))
		}
		newID := DeriveMessageID(targetUIDPlaceholder, targetConversationID, seq, digest, salt)
		if used[newID] {
			seq++
			continue
		}
		used[newID] = true
		if _, ok := messageIDs[id]; !ok {
			messageIDs[id] = newID
		}
		seq++
	}
	for i, raw := range jsonArray(sourceIndex["requests"]) {
		req, _ := raw.(map[string]any)
		id := strings.TrimSpace(strOf(req["id"]))
		if id == "" {
			continue
		}
		encoded, _ := json.Marshal(req)
		newID := DeriveRequestID(targetUIDPlaceholder, targetConversationID, i, lineDigestOf(string(encoded)), salt)
		if _, ok := requestIDs[id]; !ok {
			requestIDs[id] = newID
		}
	}
	return &RemapPlan{
		MessageIDs: messageIDs, RequestIDs: requestIDs,
		CombinedIDs: mergeMessageFirst(messageIDs, requestIDs), MessageTotal: len(messageIDs),
	}
}

// targetUIDPlaceholder 是派生公式里的目标 uid 段。
//
// 目标 uid 在同步入口由调用方注入（见 SetTargetUID）；派生公式必须把它算进去，
// 否则两个目标账号会派生出同一批 id（不同账号的会话互不相关，撞 id 只会带来风险）。
var targetUIDPlaceholder = ""

// SetTargetUID 设置派生公式用的目标账号 uid（同步入口调用一次）。
func SetTargetUID(uid string) { targetUIDPlaceholder = uid }

// planConflicts 判断派生出的 id 是否与目标目录已有文件冲突。
func planConflicts(plan *RemapPlan, targetDir, targetID string) bool {
	entries, err := os.ReadDir(filepath.Join(targetDir, "messages"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		stem, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !IsHex32(stem) {
			continue
		}
		if stem == targetID {
			continue
		}
		for _, newID := range plan.MessageIDs {
			if newID == stem {
				return true
			}
		}
	}
	return false
}

// modeAllows 与 WorkBuddy 侧同一套模式权限（unknown 不匹配任何模式）。
func modeAllows(verdict, mode string) bool {
	switch mode {
	case "fastForward":
		return verdict == localsessions.VerdictFastForward
	case "overwrite":
		return verdict == localsessions.VerdictDiverge
	case "unifyOverwrite":
		return verdict == localsessions.VerdictAhead
	}
	return false
}

// findMembers 按 member id 取出源与目标成员。
func findMembers(group *localsessions.LinkGroupR, sourceMemberID, targetMemberID string) (*localsessions.LinkMember, *localsessions.LinkMember) {
	var src, tgt *localsessions.LinkMember
	for i := range group.Members {
		if group.Members[i].MemberID == sourceMemberID {
			src = &group.Members[i]
		}
		if group.Members[i].MemberID == targetMemberID {
			tgt = &group.Members[i]
		}
	}
	return src, tgt
}
