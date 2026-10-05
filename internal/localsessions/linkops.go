package localsessions

// -----------------------------------------------------------------------------
// 登记表上的操作层（对照 wb-switch 的 session.rs / session_groups.rs 用户可见行为）
//
//   - 复制 = copy + 登记（alreadyLinked 幂等、组复用、commit 基线与继承）；
//   - 组列表/详情从登记表聚合（safeSource、成员状态、summary、分叉形状）；
//   - 同步按**模式**执行（fastForward/overwrite/unifyOverwrite），执行前逐对复核；
//   - 取消/删除关联 = 删登记项（内容不动）；
//   - 首次初始化时从既有同源副本做一次性迁移（老版本复制过的不丢）。
//
// 与 switch 的差异只有一处：存储根在网关工作目录的 session-links/ 下（理由见
// linkstore.go 顶部）；其余结构、判定、文案逐字段对齐。
// -----------------------------------------------------------------------------

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"workbuddy-gateway/internal/localapps"
)

// -----------------------------------------------------------------------------
// 复制 = copy + 登记
// -----------------------------------------------------------------------------

// mustNewUUID 生成 UUID；失败（熵源不可用）直接 panic —— 没有它就无法登记，
// 这种失败不该被静默吞掉。
func mustNewUUID() string {
	id, err := newUUID()
	if err != nil {
		panic(err)
	}
	return id
}

// AlreadyLinkedError 表示目标账号在组内已有一份有效副本（幂等，不是失败）。
type AlreadyLinkedError struct{ MemberID string }

func (e *AlreadyLinkedError) Error() string {
	return "该账号已在会话组中，无需再次复制"
}

// CopySession 把一条会话复制给另一个账号并登记关联（对照 copy_one_session）。
//
// 流程：写侧门禁（目标账号正在客户端里用 → 拒绝）→ 主表就绪 → 恢复未完成操作 →
// 按源身份找组（复用）→ 目标已有有效副本则 alreadyLinked → 同一请求有未完成操作
// 则拒绝重复 → 预分配组 id 与目标会话 id → **先落盘操作日志** → 备份 → 写正文 →
// 插行 → 核验 → 提交登记（成员 supersede + 配对基线 + 继承）→ 操作完成。
func (s *Store) CopySession(sessionID string, opts CopyOptions) (*CopyResult, error) {
	if strings.TrimSpace(opts.TargetUID) == "" {
		return nil, fmt.Errorf("必须指定目标账号（user_id）")
	}
	if opts.TitleSuffix == "" {
		opts.TitleSuffix = "（副本）"
	}
	targetVariant := strings.TrimSpace(opts.TargetVariant)

	// ---- 0. 定位源会话所在库，读源行 ----
	var srcLib *library
	var srcRow map[string]any
	for i := range s.Libraries {
		if !libExists(&s.Libraries[i]) {
			continue
		}
		db, err := s.openLib(&s.Libraries[i], true)
		if err != nil {
			return nil, err
		}
		row, err := readSessionRow(db, sessionID)
		db.Close()
		if err != nil {
			return nil, err
		}
		if row != nil {
			srcLib = &s.Libraries[i]
			srcRow = row
			break
		}
	}
	if srcLib == nil {
		return nil, fmt.Errorf("会话 %s 不存在", sessionID)
	}
	srcUID, _ := srcRow["user_id"].(string)
	if srcUID == "" {
		return nil, fmt.Errorf("会话 %s 没有 user_id，无法判定归属", sessionID)
	}
	if targetVariant == "" {
		targetVariant = srcLib.Variant
	}
	if srcUID == opts.TargetUID && srcLib.Variant == targetVariant {
		return nil, fmt.Errorf("该会话本来就属于这个账号，无需复制")
	}

	// ---- 1. 目标库 ----
	targetLib := s.libByVariant(targetVariant)
	if targetLib == nil {
		return nil, fmt.Errorf("未知的目标档位 %q", targetVariant)
	}
	if !libExists(targetLib) {
		return nil, fmt.Errorf("目标账号的客户端还没有会话库（%s 不存在），"+
			"请先打开该档位的 WorkBuddy 客户端并登录一次，再复制会话", targetLib.DBPath)
	}

	// ---- 2. 写侧门禁：目标账号正在客户端里使用 → 拒绝（见 clientgate.go）----
	if blocked, reason := s.targetWriteBlocked(targetVariant, opts.TargetUID); blocked {
		return nil, errors.New(reason)
	}

	// ---- 3. 主表就绪 + 恢复未完成操作（恢复失败一律拒绝继续写）----
	if err := s.ensureLinkStoreReady(); err != nil {
		return nil, err
	}
	recovery := s.RecoverPendingOperations(targetVariant)
	if hasUnparseableIssue(recovery) {
		return nil, errors.New(recoveryBlockingDetail(recovery))
	}
	pending := s.pendingOperations(targetVariant)

	// ---- 4. 按源身份找组；目标已有**有效**副本 → alreadyLinked（幂等）----
	store, _, err := s.loadLinkStore()
	if err != nil {
		return nil, err
	}
	group := findGroupForIdentity(store, srcLib.Variant, srcUID, sessionID)
	if group != nil {
		if m := activeMemberForUID(group, opts.TargetUID); m != nil && s.memberIsValid(m) {
			return nil, &AlreadyLinkedError{MemberID: m.MemberID}
		}
	}

	// ---- 5. 同一（源会话 → 目标账号）已有未完成操作：只复用，不新建第二个副本 ----
	if op := findPendingOperation(pending, srcUID, sessionID, opts.TargetUID); op != nil {
		reason := op.LastError
		if reason == "" {
			reason = "等待恢复"
		}
		return nil, fmt.Errorf("上一次复制尚未完成（操作 %s）：%s，这次不会重复创建", op.OperationID, reason)
	}

	// ---- 6. 预分配身份（组 id 与目标会话 id 都在写入前定下，恢复时复用）----
	newID, err := newUUID()
	if err != nil {
		return nil, err
	}
	groupID := ""
	if group != nil {
		groupID = group.ID
	} else {
		groupID, err = newUUID()
		if err != nil {
			return nil, err
		}
	}

	var bodyPath string
	var hasBody bool
	if p, ok := s.bodyPathIn(srcLib, sessionID, str(srcRow["cwd"])); ok {
		bodyPath, hasBody = p, true
	}
	// 源内容快照：本次写入的正文即源内容（归一化后 sessionId 是占位符，两边同摘要）。
	var snapshot *NormalizedContent
	if hasBody {
		if text, err := os.ReadFile(bodyPath); err == nil {
			if norm, err := normalizeJSONL(string(text), sessionID); err == nil {
				snapshot = norm
			}
		}
	}

	res := &CopyResult{SourceID: sessionID, NewID: newID, UserID: opts.TargetUID, BackupID: "", GroupID: groupID}
	backupPaths := []string{targetLib.DBPath, targetLib.DBPath + "-wal", targetLib.DBPath + "-shm"}
	if hasBody {
		backupPaths = append(backupPaths, bodyPath)
	}
	if opts.DryRun {
		// 预演也备份：这样「看完预演直接真跑」时，出问题能立刻恢复
		// （预演不写任何业务数据，也不留操作日志）。
		entry, err := localappsBackupFor(fmt.Sprintf("预演复制会话 %s → 账号 %s（%s 库）", sessionID, opts.TargetUID, targetLib.Variant), backupPaths...)
		if err != nil {
			return res, fmt.Errorf("备份失败，已放弃复制（宁可不做也不能写坏会话库）: %w", err)
		}
		res.BackupID = entry
		res.Notes = append(res.Notes, "预演模式：未写入任何内容")
		return res, nil
	}

	// ---- 7. 操作日志先落盘，再动任何业务数据 ----
	op := &Operation{
		Version:     OperationVersion,
		OperationID: mustNewUUID(),
		Kind:        OperationKindCopy,
		Variant:     targetVariant,
		GroupID:     groupID,
		Source:      OperationMember{AccountID: "", UID: srcUID, SessionID: sessionID},
		Target:      OperationMember{AccountID: opts.AccountID, UID: opts.TargetUID, SessionID: newID},
		Phase:       PhasePrepared,
		CreatedAt:   nowMillis(),
		UpdatedAt:   nowMillis(),
	}
	if srcLib.Variant != targetVariant {
		op.SourceVariant = srcLib.Variant
	}
	if snapshot != nil {
		op.ExpectedContentDigest = snapshot.TotalDigest
		op.ExpectedRecordCount = snapshot.RecordCount
	}
	if err := s.saveOperation(op); err != nil {
		return res, fmt.Errorf("操作记录写入失败，已放弃复制（宁可不做也不能写坏会话库）: %w", err)
	}

	// ---- 8. 备份（目标库 DB + 源正文）----
	entry, err := localappsBackupFor(fmt.Sprintf("复制会话 %s → 账号 %s（%s 库）", sessionID, opts.TargetUID, targetLib.Variant), backupPaths...)
	if err != nil {
		s.abandonOperation(op, "备份失败，已放弃复制："+err.Error())
		return res, fmt.Errorf("备份失败，已放弃复制（宁可不做也不能写坏会话库）: %w", err)
	}
	op.Backup = entry
	_ = s.saveOperation(op)
	res.BackupID = entry
	res.Notes = append(res.Notes, "已备份 "+filepath.ToSlash(targetLib.DBPath)+
		func() string {
			if hasBody {
				return " 与正文"
			}
			return ""
		}()+" → "+entry)

	// ---- 9. 推进阶段：正文 → 数据库行 → 映射（交接）→ 关联与基线 ----
	if hasBody {
		lines, err := s.writeCopyBody(srcLib, targetLib, bodyPath, sessionID, newID, false)
		if err != nil {
			s.failOperation(op, err.Error())
			return res, fmt.Errorf("%s（会话库未改动，可用备份恢复）", err.Error())
		}
		if err := s.advanceOperation(op, PhaseBodyWritten); err != nil {
			s.failOperation(op, err.Error())
			return res, err
		}
		res.BodyPath, _ = s.targetBodyPath(srcLib, bodyPath, targetLib, newID)
		res.BodyLines = lines
		res.Notes = append(res.Notes, fmt.Sprintf("已复制正文到 %s（重写了 %d 行的 sessionId）", filepath.ToSlash(res.BodyPath), lines))
	} else {
		res.Notes = append(res.Notes, "源会话没有正文文件，只复制元数据行")
	}
	if err := s.insertSessionRowFromSource(srcLib, targetLib, sessionID, newID, opts.TargetUID, opts.TitleSuffix); err != nil {
		s.failOperation(op, err.Error())
		return res, fmt.Errorf("插入会话记录失败（正文已写入，可用备份 %s 恢复）: %w", entry, err)
	}
	if err := s.verifySessionRowOwner(targetLib, newID, opts.TargetUID); err != nil {
		s.failOperation(op, err.Error())
		return res, fmt.Errorf("%s（请用备份 %s 恢复）", err.Error(), entry)
	}
	res.Notes = append(res.Notes, fmt.Sprintf("已在 %s 库的 sessions 表插入新记录（归属 %s）", targetLib.Variant, opts.TargetUID))
	if err := s.advanceOperation(op, PhaseDbWritten); err != nil {
		s.failOperation(op, err.Error())
		return res, err
	}
	// 云端登记交接给客户端：不预写映射库（见 finishCopyFromBody 的说明）。
	if err := s.advanceOperation(op, PhaseMappingWritten); err != nil {
		s.failOperation(op, err.Error())
		return res, err
	}

	// ---- 10. 提交登记（成员 + 配对基线 + 继承；对照 commit_links）----
	// 本次复制的正文即源↔目标的共同基线。正文读不出来时基线缺失，
	// 之后判定会退化为「无法自动判断方向」——与 switch 的行为一致。
	var digests []string
	if snapshot != nil {
		digests = snapshot.LineDigests
	}
	if _, err := s.commitLinks(groupID, srcLib.Variant, srcUID, sessionID, "",
		opts.TargetUID, newID, targetVariant, opts.AccountID, digests); err != nil {
		s.failOperation(op, err.Error())
		return res, fmt.Errorf("登记关联失败（会话已复制，可重试登记或解除）: %w", err)
	}
	if len(digests) > 0 {
		res.Notes = append(res.Notes, "已记录同步基线（之后可直接快进同步）")
	}
	if err := s.advanceOperation(op, PhaseLinksCommitted); err != nil {
		s.failOperation(op, err.Error())
		return res, err
	}
	if err := s.advanceOperation(op, PhaseCompleted); err != nil {
		s.failOperation(op, err.Error())
		return res, err
	}
	op.CleanupState = CleanupStateCleaned
	_ = s.saveOperation(op)
	_ = s.pruneOperations(targetVariant, KeepCompletedOperations)
	res.GroupID = groupID
	return res, nil
}

// hasUnparseableIssue 报告恢复结果里是否存在「操作记录无法解析/无法读取」这类
// 必须阻断的项（继续写会绕过 pending 去重，可能写出第二个副本）。
func hasUnparseableIssue(report *RecoveryReport) bool {
	for _, issue := range report.NeedsRecovery {
		if !issue.Retryable && strings.Contains(issue.Reason, UnparseableOperationReason) {
			return true
		}
	}
	return false
}

// activeMemberForUID 取组内该账号的 active 成员（没有则 nil）。
func activeMemberForUID(g *LinkGroupR, uid string) *LinkMember {
	for i := range g.Members {
		if g.Members[i].UID == uid && g.Members[i].State == MemberStateActive {
			return &g.Members[i]
		}
	}
	return nil
}

// memberIsValid 判断组内成员是不是**真实有效**的副本：正文可验证 + 会话行归属正确
// （对照 switch 的 member_is_valid）。
//
// 只看正文存在是不够的：会话行可能被客户端删掉（软删除）或改成别的账号，
// 那时「已关联」是假的，重跑复制才是正确行为。
func (s *Store) memberIsValid(m *LinkMember) bool {
	lib := s.libByVariant(m.Variant)
	if lib == nil || !libExists(lib) {
		return false
	}
	state, _ := s.memberContent(lib, m.SessionID, "")
	if state.ready == nil {
		return false
	}
	owner, err := s.sessionRowOwner(lib, m.SessionID)
	return err == nil && owner == m.UID
}

// writeCopyBody 写副本正文并做写后校验（对照 write_copy_body）。
//
// **写前先算源摘要、写后比对**：避免「读源 → 写目标」之间源被改动导致的
// 「写进去的其实不是校验的那份」。
func (s *Store) writeCopyBody(srcLib, tgtLib *library, srcPath, srcSessionID, tgtSessionID string, allowExisting bool) (int, error) {
	if tgtLib == nil || !libExists(tgtLib) {
		return 0, fmt.Errorf("目标账号的客户端还没有会话库，请先打开该档位的 WorkBuddy 客户端并登录一次，再复制会话")
	}
	dst, err := s.targetBodyPath(srcLib, srcPath, tgtLib, tgtSessionID)
	if err != nil {
		return 0, fmt.Errorf("定位新正文路径失败: %w", err)
	}
	if !allowExisting {
		if _, err := os.Stat(dst); err == nil {
			return 0, fmt.Errorf("目标内容已存在同名文件，已停止复制")
		}
	}
	before, err := os.ReadFile(srcPath)
	if err != nil {
		return 0, fmt.Errorf("读取源正文失败: %w", err)
	}
	want, err := normalizeJSONL(string(before), srcSessionID)
	if err != nil {
		return 0, fmt.Errorf("源内容无法验证（%s），未复制", err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, fmt.Errorf("创建目标正文目录失败（会话库未改动）: %w", err)
	}
	lines, err := copyJSONLRewriteSessionID(srcPath, dst, srcSessionID, tgtSessionID)
	if err != nil {
		return 0, fmt.Errorf("写正文失败: %w", err)
	}
	after, err := os.ReadFile(dst)
	if err != nil {
		return 0, fmt.Errorf("复制后的内容保存后不存在，未按成功处理")
	}
	got, err := normalizeJSONL(string(after), tgtSessionID)
	if err != nil {
		return 0, fmt.Errorf("复制后的内容保存后无法确认：%s", err.Error())
	}
	if !equalDigests(got.LineDigests, want.LineDigests) {
		return 0, fmt.Errorf("复制后的内容保存后校验不一致，未按成功处理")
	}
	return lines, nil
}

// insertSessionRowFromSource 读源行并写入目标行（复制与恢复共用）。
func (s *Store) insertSessionRowFromSource(srcLib, tgtLib *library, srcSessionID, tgtSessionID, targetUID, titleSuffix string) error {
	if srcLib == nil || !libExists(srcLib) {
		return fmt.Errorf("源会话库不存在")
	}
	if tgtLib == nil || !libExists(tgtLib) {
		return fmt.Errorf("目标账号的客户端还没有会话库")
	}
	sdb, err := s.openLib(srcLib, true)
	if err != nil {
		return err
	}
	srcRow, err := readSessionRow(sdb, srcSessionID)
	sdb.Close()
	if err != nil {
		return err
	}
	if srcRow == nil {
		return fmt.Errorf("数据库中找不到源会话记录，未复制")
	}
	if titleSuffix == "" {
		titleSuffix = "（副本）"
	}
	tdb, err := s.openLib(tgtLib, false)
	if err != nil {
		return err
	}
	defer tdb.Close()
	return insertSessionRow(tdb, srcRow, tgtSessionID, CopyOptions{TargetUID: targetUID, TitleSuffix: titleSuffix})
}

// commitLinks 原子提交关联与配对基线（含失效成员替换与基线继承），返回组 id
// （对照 commit_links：整个读改写都在关联存储锁内完成，只有全部成功才推进 revision）。
func (s *Store) commitLinks(groupID, srcVariant, srcUID, srcSessionID, srcAccountID string,
	targetUID, targetSessionID, targetVariant, targetAccountID string, digests []string) (string, error) {
	err := s.withLinkStoreWrite(func(store *LinkStoreFile) error {
		idx := -1
		for i := range store.Groups {
			if store.Groups[i].ID == groupID {
				idx = i
				break
			}
		}
		if idx < 0 {
			store.Groups = append(store.Groups, LinkGroupR{
				ID: groupID, Variant: targetVariant, CreatedAt: nowMillis(),
				Members: []LinkMember{}, PairBases: []PairBase{},
			})
			idx = len(store.Groups) - 1
		}
		g := &store.Groups[idx]

		srcMemberID := memberIDOf(g, srcUID, srcSessionID)
		if srcMemberID == "" {
			srcMemberID = mustNewUUID()
			addActiveMember(g, LinkMember{
				MemberID: srcMemberID, AccountID: srcAccountID, UID: srcUID,
				SessionID: srcSessionID, Variant: srcVariant,
				State: MemberStateActive, LinkedAt: nowMillis(),
			})
		}

		tgtMemberID := memberIDOf(g, targetUID, targetSessionID)
		if tgtMemberID == "" {
			tgtMemberID = mustNewUUID()
			addActiveMember(g, LinkMember{
				MemberID: tgtMemberID, AccountID: targetAccountID, UID: targetUID,
				SessionID: targetSessionID, Variant: targetVariant,
				State: MemberStateActive, LinkedAt: nowMillis(),
			})
		} else if m := findMember(g, targetUID, targetSessionID); m != nil {
			// 已存在（恢复重放）：显式置回 active，保留 lastSyncedAt。
			m.State = MemberStateActive
		}

		// 本次复制的正文即源↔目标的共同基线（定向更新，不动其它配对）。
		if len(digests) > 0 {
			ref := mustNewUUID()
			if err := s.saveBaselineRecord(&BaselineRecord{
				Version: LinkStoreVersion, BaselineRef: ref,
				NormalizationVersion: NormalizationVersion, CreatedAt: nowMillis(),
				RecordCount: len(digests), TotalDigest: totalDigestOf(digests), LineDigests: digests,
			}); err != nil {
				return err
			}
			setPairBase(g, srcMemberID, tgtMemberID, ref)
		}

		// 继承：源与组内其它成员已有的基线，只有在「新成员正文包含该基线」时
		// 才建立到新成员的基线；已有配对不覆盖。
		for _, other := range g.Members {
			if other.MemberID == srcMemberID || other.MemberID == tgtMemberID {
				continue
			}
			if findPairBase(g, other.MemberID, tgtMemberID) != nil {
				continue
			}
			pair := findPairBase(g, srcMemberID, other.MemberID)
			if pair == nil {
				continue
			}
			if len(digests) == 0 {
				continue
			}
			if record := s.inheritableBaseline(*pair, digests); record != nil {
				setPairBase(g, other.MemberID, tgtMemberID, record.BaselineRef)
			}
		}
		sortPairBases(g)
		return nil
	})
	if err != nil {
		return "", err
	}
	return groupID, nil
}

func memberIDOf(g *LinkGroupR, uid, sessionID string) string {
	if m := findMember(g, uid, sessionID); m != nil {
		return m.MemberID
	}
	return ""
}

// contentPreviewOf 取正文里**最近 6 条可读内容**（对照 switch 的 content_preview）。
//
// 顺序：从尾部往前找有文本的记录（跳过没有文本的元数据行），取满 6 条后翻回时间顺序 ——
// 用户看到的是「最后说了什么」，而不是「最前面说了什么」。
// 文本截断到 240 字符：预览是让人确认「这是哪段对话」，不是用来读全文的。
func contentPreviewOf(text string) []ContentPreviewItem {
	lines := strings.Split(text, "\n")
	out := make([]ContentPreviewItem, 0, 6)
	for i := len(lines) - 1; i >= 0 && len(out) < 6; i-- {
		line := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
		if line == "" {
			continue
		}
		var record any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		content := strings.TrimSpace(recordTextOf(record))
		if content == "" {
			continue
		}
		out = append(out, ContentPreviewItem{Speaker: recordSpeakerOf(record), Text: truncateRunes(content, 240)})
	}
	// 翻回时间顺序（上面是倒着取的）。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// recordSpeakerOf 判定一条记录的说话人（对照 switch 的 role/type 映射）。
func recordSpeakerOf(record any) string {
	obj, ok := record.(map[string]any)
	if !ok {
		return "记录"
	}
	v, _ := obj["role"].(string)
	if v == "" {
		v, _ = obj["type"].(string)
	}
	switch v {
	case "user":
		return "用户"
	case "assistant":
		return "助手"
	}
	return "记录"
}

// recordTextOf 从一条记录里取正文（对照 switch 的 record_text）：
// 字符串直接用；数组拼接；对象按 text/content/message/parts 依次尝试。
func recordTextOf(record any) string {
	switch v := record.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if t := strings.TrimSpace(recordTextOf(item)); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		for _, key := range []string{"text", "content", "message", "parts"} {
			if raw, ok := v[key]; ok {
				if t := recordTextOf(raw); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// truncateRunes 按字符（不是字节）截断，避免把多字节汉字切成半个。
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

// -----------------------------------------------------------------------------
// 组级同步报告与哨兵错误
// -----------------------------------------------------------------------------

// 组级同步的哨兵错误，供 HTTP 层区分状态码。
var (
	ErrGroupNotFound        = errors.New("会话组不存在")
	ErrSourceMemberNotFound = errors.New("来源成员不在该会话组内")
)

// GroupSyncItem 是一条成功同步（真的写了目标）的成员结果。
type GroupSyncItem struct {
	MemberID string   `json:"member_id"`
	OK       bool     `json:"ok"`
	Notes    []string `json:"notes"`
}

// GroupSyncSkip 是一条被安全跳过的成员及原因（不是失败）。
type GroupSyncSkip struct {
	MemberID string `json:"member_id"`
	Reason   string `json:"reason"`
	// ReasonCode 是机器可读的原因码（如 previewStale / recheckNotFastForward），
	// 前端据此给出「重新检查」这类针对性提示，而不是解析文案。
	ReasonCode string `json:"reason_code,omitempty"`
}

// GroupSyncError 是一条执行失败的成员及错误。
type GroupSyncError struct {
	MemberID string `json:"member_id"`
	Error    string `json:"error"`
}

// GroupSyncReport 是组级同步的逐项报告（前端据此逐项展示）。
type GroupSyncReport struct {
	SourceMemberID string           `json:"source_member_id"`
	Synced         []GroupSyncItem  `json:"synced"`
	Skipped        []GroupSyncSkip  `json:"skipped"`
	Errors         []GroupSyncError `json:"errors"`
	// NeedsRecovery 为真表示本次写入留下了需要人工处理的现场（前端要显式提示，
	// 且客户端暂停重开）。
	NeedsRecovery bool `json:"needs_recovery"`
	// RestartedVariants 是本次会话操作后重新打开的客户端档位（cn/intl）。
	RestartedVariants []string `json:"restarted_variants"`
}

func newGroupSyncReport() *GroupSyncReport {
	return &GroupSyncReport{Synced: []GroupSyncItem{}, Skipped: []GroupSyncSkip{}, Errors: []GroupSyncError{}}
}

// -----------------------------------------------------------------------------
// 组列表 / 详情：从登记表聚合（对照 list_session_groups 的聚合规则）
// -----------------------------------------------------------------------------

// GroupMemberView 是组内成员的展示视图（JSON 字段是旧视图的加法，不改名）。
type GroupMemberView struct {
	MemberID         string `json:"member_id"`
	SessionID        string `json:"session_id"`
	UID              string `json:"uid"`
	Account          string `json:"account"`
	Label            string `json:"label"`
	Title            string `json:"title"`
	Variant          string `json:"variant"`
	Cwd              string `json:"cwd,omitempty"`
	State            string `json:"state"`
	UpdatedAt        int64  `json:"updated_at"`
	BodyBytes        int64  `json:"body_bytes"`
	RecordCount      int    `json:"record_count"`
	TotalDigest      string `json:"total_digest"`
	Readable         bool   `json:"readable"`
	VersionStatus    string `json:"version_status"`
	Reason           string `json:"reason"`
	OverwriteRecords int    `json:"overwrite_records"`
	// ContentPreview 是「最近 N 条可读内容」的预览（只有详情视图填充；
	// 列表视图不填 —— 组列表可能几十个组，每个成员都解析一遍正文不值得）。
	ContentPreview []ContentPreviewItem `json:"content_preview,omitempty"`
}

// ContentPreviewItem 是一条内容预览（说话人 + 截断后的文本）。
type ContentPreviewItem struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
}

// RegistryGroupView 是登记组的聚合视图（组列表与详情共用）。
type RegistryGroupView struct {
	ID                 string            `json:"id"`
	Project            string            `json:"project"`
	Title              string            `json:"title"`
	UpdatedAt          int64             `json:"updated_at"`
	Status             string            `json:"status"`
	Reason             string            `json:"reason"`
	SummaryText        string            `json:"summary_text"`
	SafeSourceMemberID string            `json:"safe_source_member_id"`
	Members            []GroupMemberView `json:"members"`
	Divergence         *GroupDivergence  `json:"divergence,omitempty"`
	Counts             int               `json:"-"`
}

// memberContent 读一份副本的正文状态（Ready/Missing/Unverifiable）。
func (s *Store) memberContent(lib *library, sessionID, cwd string) (contentState, string) {
	if lib == nil || !libExists(lib) {
		return contentMissing(), ""
	}
	path, ok := s.bodyPathIn(lib, sessionID, cwd)
	if !ok {
		return contentMissing(), ""
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return contentMissing(), ""
	}
	norm, err := normalizeJSONL(string(text), sessionID)
	if err != nil {
		return contentUnverifiable(err.Error()), ""
	}
	return contentReady(norm), path
}

// buildMemberViews 读登记组里每个成员的内容，做成员视图。
//
// withPreview 为真时额外填充「最近 N 条可读内容」预览（详情视图用）。
func (s *Store) buildMemberViews(g *LinkGroupR, uidToAccount map[string]AccountLabel, withPreview bool) ([]GroupMemberView, []contentState, []string) {
	views := make([]GroupMemberView, 0, len(g.Members))
	states := make([]contentState, 0, len(g.Members))
	libOf := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		lib := s.libByVariant(m.Variant)
		var row map[string]any
		if lib != nil && libExists(lib) {
			db, err := s.openLib(lib, true)
			if err == nil {
				row, _ = readSessionRow(db, m.SessionID)
				db.Close()
			}
		}
		view := GroupMemberView{
			MemberID: m.MemberID, SessionID: m.SessionID, UID: m.UID,
			Variant: m.Variant, State: m.State,
		}
		if lbl, ok := uidToAccount[m.UID]; ok && lbl.Label != "" {
			view.Account = lbl.Account
			view.Label = lbl.Label
		} else {
			view.Label = m.UID
		}
		cwd := ""
		updated := int64(0)
		if row != nil {
			cwd = str(row["cwd"])
			view.Cwd = cwd
			if t, ok := row["title"].(string); ok {
				view.Title = sessionDisplayTitle(t, str(row["custom_title"]))
			}
			if v, ok := row["updated_at"].(int64); ok {
				updated = v
			}
		}
		view.UpdatedAt = updated
		content, bodyPath := s.memberContent(lib, m.SessionID, cwd)
		if content.ready != nil {
			view.Readable = true
			view.RecordCount = content.ready.RecordCount
			view.TotalDigest = content.ready.TotalDigest
			if withPreview && bodyPath != "" {
				if raw, err := os.ReadFile(bodyPath); err == nil {
					view.ContentPreview = contentPreviewOf(string(raw))
				}
			}
		}
		views = append(views, view)
		states = append(states, content)
		libOf = append(libOf, m.Variant)
		_ = libOf
	}
	return views, states, libOf
}

// aggregateGroupState 聚合组状态（对照 session_groups.rs 的 aggregate_group_state）。
func (s *Store) aggregateGroupState(g *LinkGroupR, views []GroupMemberView, states []contentState) (string, string) {
	active := map[int]bool{}
	for i, m := range g.Members {
		active[i] = m.State == MemberStateActive
	}
	// 判定矩阵：active 成员两两之间（含基线）。
	verdictOf := map[string]SyncDecision2{}
	key := func(a, b int) string { return fmt.Sprintf("%d\x00%d", a, b) }
	for a := range g.Members {
		if !active[a] || states[a].ready == nil {
			continue
		}
		for b := range g.Members {
			if !active[b] || a == b || states[b].ready == nil {
				continue
			}
			baseline, bState, bUnusable := s.baselineState(g, g.Members[a].MemberID, g.Members[b].MemberID)
			d := decideSync2(states[a], states[b], baseline, bState, bUnusable)
			verdictOf[key(a, b)] = d
		}
	}

	// 安全源：内容可读，且对其余全部 active 成员的判定都是 identical/fastForward。
	// 并列按 accountKey → uid → memberId 排序取第一个。
	safeCandidates := []int{}
	for a := range g.Members {
		if !active[a] || states[a].ready == nil {
			continue
		}
		okAll := true
		for b := range g.Members {
			if !active[b] || a == b {
				continue
			}
			d, ok := verdictOf[key(a, b)]
			if !ok || (d.Verdict != VerdictIdentical && d.Verdict != VerdictFastForward) {
				okAll = false
				break
			}
		}
		if okAll {
			safeCandidates = append(safeCandidates, a)
		}
	}
	accountKey := func(i int) string {
		if lbl, ok := accountKeyLookup(g.Members[i].UID); ok {
			return lbl
		}
		return ""
	}
	sort.Slice(safeCandidates, func(x, y int) bool {
		l, r := safeCandidates[x], safeCandidates[y]
		if accountKey(l) != accountKey(r) {
			return accountKey(l) < accountKey(r)
		}
		if g.Members[l].UID != g.Members[r].UID {
			return g.Members[l].UID < g.Members[r].UID
		}
		return g.Members[l].MemberID < g.Members[r].MemberID
	})
	safeSource := -1
	if len(safeCandidates) > 0 {
		safeSource = safeCandidates[0]
	}

	hasBehind, hasDiverge, hasMissing := false, false, false
	for i := range views {
		m := g.Members[i]
		switch m.State {
		case MemberStateStale:
			views[i].VersionStatus = "stale"
			views[i].Reason = "该副本已标记为失效"
		case MemberStateSuperseded:
			views[i].VersionStatus = "superseded"
			views[i].Reason = "该副本已被更新成员替代"
		default:
			st := states[i]
			if st.miss {
				views[i].VersionStatus = "missing"
				views[i].Reason = "会话内容不存在，无法同步"
				hasMissing = true
			} else if st.reason != "" {
				views[i].VersionStatus = "unknown"
				views[i].Reason = st.reason
			} else if safeSource >= 0 {
				if i == safeSource {
					views[i].VersionStatus = "latest"
				} else if d, ok := verdictOf[key(safeSource, i)]; ok && d.Verdict == VerdictIdentical {
					views[i].VersionStatus = "latest"
					views[i].Reason = "内容与操作来源一致"
				} else if d, ok := verdictOf[key(safeSource, i)]; ok && d.Verdict == VerdictFastForward {
					views[i].VersionStatus = "behind"
					views[i].Reason = d.Reason
					hasBehind = true
				} else {
					views[i].VersionStatus = "unknown"
					views[i].Reason = "重新检查后无法确认此成员状态"
				}
			} else {
				foundDiverge := ""
				firstReason := ""
				for a := range g.Members {
					if !active[a] || a == i {
						continue
					}
					if d, ok := verdictOf[key(a, i)]; ok {
						if firstReason == "" {
							firstReason = d.Reason
						}
						if d.Verdict == VerdictDiverge && foundDiverge == "" {
							foundDiverge = d.Reason
						}
					}
				}
				if foundDiverge != "" {
					views[i].VersionStatus = "diverge"
					views[i].Reason = foundDiverge
					hasDiverge = true
				} else {
					views[i].VersionStatus = "unknown"
					views[i].Reason = firstReason
					if views[i].Reason == "" {
						views[i].Reason = "缺少可比较的关联成员"
					}
				}
			}
		}
	}

	summaryStatus := "unknown"
	if safeSource >= 0 {
		if hasBehind {
			summaryStatus = "behind"
		} else {
			summaryStatus = "latest"
		}
	} else if hasDiverge {
		summaryStatus = "diverge"
	} else if hasMissing {
		summaryStatus = "missing"
	}
	summaryText := map[string]string{
		"latest":  "关联副本内容一致",
		"behind":  "有副本落后，可安全同步",
		"diverge": "多个副本有不同更新，需要选择来源",
		"missing": "有副本内容缺失",
		"unknown": "暂时无法确认副本状态",
	}[summaryStatus]
	return summaryStatus, summaryText
}

// accountKeyLookup 由服务端注入（uid → 账号文件名），用于安全源排序。
var accountKeyLookup = func(uid string) (string, bool) { return "", false }

// SetAccountKeyLookup 注入 uid → 账号 id 的查询（安全源排序用）。
func SetAccountKeyLookup(fn func(uid string) (string, bool)) { accountKeyLookup = fn }

// ListRegistryGroups 列出登记表里的全部组（聚合状态）。
func (s *Store) ListRegistryGroups(uidToAccount map[string]AccountLabel) ([]RegistryGroupView, error) {
	store, _, err := s.loadLinkStore()
	if err != nil {
		return nil, err
	}
	out := make([]RegistryGroupView, 0, len(store.Groups))
	for i := range store.Groups {
		g := &store.Groups[i]
		out = append(out, s.buildRegistryGroupView(g, uidToAccount, false))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// buildRegistryGroupView 聚合单个组（列表与详情共用）。
//
// withPreview 只在详情视图为真：列表可能几十个组，逐成员解析正文取预览不值得。
func (s *Store) buildRegistryGroupView(g *LinkGroupR, uidToAccount map[string]AccountLabel, withPreview bool) RegistryGroupView {
	views, states, _ := s.buildMemberViews(g, uidToAccount, withPreview)
	status, summaryText := s.aggregateGroupState(g, views, states)

	view := RegistryGroupView{
		ID: g.ID, Members: views, Status: status, SummaryText: summaryText,
	}
	// 标题/项目/更新时间取记录数最多的可读成员（副本标题带「（副本）」后缀，
	// 用最全的那份更能代表这组对话）。
	richest := -1
	for i, v := range views {
		if !v.Readable {
			continue
		}
		if richest < 0 || v.RecordCount > views[richest].RecordCount {
			richest = i
		}
		if v.UpdatedAt > view.UpdatedAt {
			view.UpdatedAt = v.UpdatedAt
		}
	}
	if richest >= 0 {
		view.Title = views[richest].Title
		view.Project = projectName(views[richest].Cwd)
	}
	if view.Title == "" && len(views) > 0 {
		view.Title = views[0].Title
	}
	if safe := s.registrySafeSource(g, states, accountKeyOf(uidToAccount)); safe != "" {
		view.SafeSourceMemberID = safe
	}
	// 分叉形状（对照 common_base_branches：共同旧版 + 独立分支）。
	if status == "diverge" {
		if common, branches, ok := s.registryDivergence(g, states); ok {
			view.Divergence = &GroupDivergence{CommonMemberIDs: common, BranchMemberIDs: branches, Branches: len(branches)}
		}
	}
	return view
}

func accountKeyOf(uidToAccount map[string]AccountLabel) func(string) (string, bool) {
	return func(uid string) (string, bool) {
		if lbl, ok := uidToAccount[uid]; ok {
			return lbl.Account, true
		}
		return "", false
	}
}

// registrySafeSource：与 aggregateGroupState 的安全源同口径（重新独立计算，
// 因为那里没有返回成员 id）。
func (s *Store) registrySafeSource(g *LinkGroupR, states []contentState, accountKey func(string) (string, bool)) string {
	type cand struct {
		index int
		key   string
		uid   string
		mid   string
	}
	var cands []cand
	for a := range g.Members {
		if g.Members[a].State != MemberStateActive || states[a].ready == nil {
			continue
		}
		okAll := true
		for b := range g.Members {
			if a == b || g.Members[b].State != MemberStateActive || states[b].ready == nil {
				continue
			}
			baseline, bState, bUnusable := s.baselineState(g, g.Members[a].MemberID, g.Members[b].MemberID)
			d := decideSync2(states[a], states[b], baseline, bState, bUnusable)
			if d.Verdict != VerdictIdentical && d.Verdict != VerdictFastForward {
				okAll = false
				break
			}
		}
		if okAll {
			key := ""
			if accountKey != nil {
				key, _ = accountKey(g.Members[a].UID)
			}
			cands = append(cands, cand{a, key, g.Members[a].UID, g.Members[a].MemberID})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].key != cands[j].key {
			return cands[i].key < cands[j].key
		}
		if cands[i].uid != cands[j].uid {
			return cands[i].uid < cands[j].uid
		}
		return cands[i].mid < cands[j].mid
	})
	if len(cands) == 0 {
		return ""
	}
	return cands[0].mid
}

// registryDivergence：active 可读成员 ≥3、存在共同旧版且其余两两不互为前缀。
func (s *Store) registryDivergence(g *LinkGroupR, states []contentState) ([]string, [][]string, bool) {
	var versions []contentVersion
	for i, m := range g.Members {
		if m.State != MemberStateActive || states[i].ready == nil {
			continue
		}
		merged := false
		for vi := range versions {
			if equalDigests(versions[vi].digests, states[i].ready.LineDigests) {
				versions[vi].ids = append(versions[vi].ids, m.MemberID)
				merged = true
				break
			}
		}
		if !merged {
			versions = append(versions, contentVersion{ids: []string{m.MemberID}, digests: states[i].ready.LineDigests})
		}
	}
	common, branches, ok := findCommonBaseVersions(versions)
	return common, branches, ok
}

// contentVersion 是「同一份内容」的等价类（成员 id 集合 + 行摘要）。
type contentVersion struct {
	ids     []string
	digests []string
}

// findCommonBaseVersions 找共同旧版与独立分支（对照 switch 的 common_base_branches）。
func findCommonBaseVersions(versions []contentVersion) ([]string, [][]string, bool) {
	if len(versions) < 3 {
		return nil, nil, false
	}
	commonIdx := -1
	for vi := range versions {
		allExt := true
		for oi := range versions {
			if oi == vi {
				continue
			}
			if !isStrictOrderedExtension(versions[vi].digests, versions[oi].digests) {
				allExt = false
				break
			}
		}
		if allExt {
			if commonIdx >= 0 {
				return nil, nil, false
			}
			commonIdx = vi
		}
	}
	if commonIdx < 0 {
		return nil, nil, false
	}
	for a := range versions {
		if a == commonIdx {
			continue
		}
		for b := a + 1; b < len(versions); b++ {
			if b == commonIdx {
				continue
			}
			if isStrictOrderedExtension(versions[a].digests, versions[b].digests) ||
				isStrictOrderedExtension(versions[b].digests, versions[a].digests) {
				return nil, nil, false
			}
		}
	}
	out := make([][]string, 0, len(versions)-1)
	for vi := range versions {
		if vi == commonIdx {
			continue
		}
		out = append(out, versions[vi].ids)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return versions[commonIdx].ids, out, true
}

// GetRegistryGroup 取单个组（详情），未命中返回 nil。
func (s *Store) GetRegistryGroup(groupID string, uidToAccount map[string]AccountLabel) *RegistryGroupView {
	store, _, err := s.loadLinkStore()
	if err != nil {
		return nil
	}
	for i := range store.Groups {
		if store.Groups[i].ID == groupID {
			v := s.buildRegistryGroupView(&store.Groups[i], uidToAccount, true)
			return &v
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// 同步执行：按模式、逐对复核（对照 sync_sessions_cross 的执行语义）
// -----------------------------------------------------------------------------

// SyncPairOutcome 是一次「源成员 → 目标成员」按模式执行的原始结果。
type SyncPairOutcome struct {
	MemberID string
	OK       bool
	Applied  bool
	Reason   string
	// ReasonCode 是跳过原因码（previewStale 表示预览凭据失配，前端据此提示「重新检查」）。
	ReasonCode string
	Error      string
	Notes      []string
	BackupID   string
}

// SyncPairInput 是一次成员对同步的全部输入。
type SyncPairInput struct {
	// Group 是组成员快照（执行前的登记状态）。
	Group *LinkGroupR
	// Source / Target 是成员快照。
	Source, Target LinkMember
	// Mode 是写入模式（fastForward / overwrite / unifyOverwrite）。
	Mode string
	// PreviewToken 是预览时服务端签发的凭据 id：**执行时必须原样带回**，
	// 缺省或失配一律不写（见 preview.go 的说明）。
	PreviewToken string
}

// decideForPair 重新加载双方内容与基线并判定（预览与执行共用同一条路径）。
func (s *Store) decideForPair(g *LinkGroupR, srcMember, tgtMember LinkMember) SyncDecision2 {
	srcLib := s.libByVariant(srcMember.Variant)
	tgtLib := s.libByVariant(tgtMember.Variant)
	srcContent, _ := s.memberContent(srcLib, srcMember.SessionID, "")
	tgtCwd := ""
	if tgtLib != nil && libExists(tgtLib) {
		if db, err := s.openLib(tgtLib, true); err == nil {
			if row, _ := readSessionRow(db, tgtMember.SessionID); row != nil {
				tgtCwd = str(row["cwd"])
			}
			db.Close()
		}
	}
	tgtContent, _ := s.memberContent(tgtLib, tgtMember.SessionID, tgtCwd)
	baseline, bState, bUnusable := s.baselineState(g, srcMember.MemberID, tgtMember.MemberID)
	return decideSync2(srcContent, tgtContent, baseline, bState, bUnusable)
}

// SyncMemberPair 把源成员按指定模式同步到目标成员。
//
// **执行前重新判定 + 逐项复核预览凭据**：以当时的真实内容与登记基线重新 decide，
// 凭据里任何一项版本信息变化都跳过该项（不沿用用户旧选择）；判定不允许该模式
// 直接拒绝。写成功后更新目标成员 lastSyncedAt 并写新的配对基线
// （对照 commit_sync_baseline）。
func (s *Store) SyncMemberPair(in SyncPairInput) SyncPairOutcome {
	out := SyncPairOutcome{MemberID: in.Target.MemberID}
	if _, err := parseMode(in.Mode); err != nil {
		out.Error = err.Error()
		return out
	}
	if in.Target.State != MemberStateActive {
		out.Reason = "该成员不是当前接力成员，不再参与同步"
		return out
	}
	// 凭据必须是我们服务端保存过的：伪造的 id 读不到，直接拒绝（不静默跳过）。
	preview := s.loadPreviewToken(in.PreviewToken)
	if preview == nil {
		out.Error = "检查结果不存在或已失效，请重新检查后再操作"
		return out
	}
	if preview.Binding.GroupID != in.Group.ID {
		out.Error = "检查结果与所选会话不匹配，已拒绝"
		return out
	}
	if preview.Binding.Source.UID != in.Source.UID || preview.Binding.Target.UID != in.Target.UID {
		out.Reason = "账号已变化，检查结果已失效"
		out.ReasonCode = PreviewStaleReasonCode
		return out
	}
	// 重新加载内容、基线与判定，再与凭据逐项核对。
	decision := s.decideForPair(in.Group, in.Source, in.Target)
	live := s.livePreviewBinding(in.Group, in.Source, in.Target, decision.Verdict)
	if stale := verifyPreview(preview, &live); len(stale) > 0 {
		out.Reason = "检查结果已失效：" + strings.Join(stale, "；")
		out.ReasonCode = PreviewStaleReasonCode
		return out
	}
	// 判定已按当前内容重算：mode 必须仍然成立，unknown 不得被覆盖绕过。
	if !modeAllows(decision.Verdict, in.Mode) {
		out.Error = fmt.Sprintf("该副本判定为 %s，不允许以 %s 模式同步。%s", decision.Verdict, in.Mode, decision.Reason)
		return out
	}
	if decision.Verdict == VerdictIdentical {
		out.Reason = decision.Reason
		return out
	}
	srcLib := s.libByVariant(in.Source.Variant)
	tgtLib := s.libByVariant(in.Target.Variant)
	srcContent, _ := s.memberContent(srcLib, in.Source.SessionID, "")
	if srcContent.ready == nil {
		out.Error = decision.Reason
		return out
	}
	tgtCwd := ""
	if tgtLib != nil && libExists(tgtLib) {
		if db, err := s.openLib(tgtLib, true); err == nil {
			if row, _ := readSessionRow(db, in.Target.SessionID); row != nil {
				tgtCwd = str(row["cwd"])
			}
			db.Close()
		}
	}
	srcPath, ok := s.bodyPathIn(srcLib, in.Source.SessionID, "")
	if !ok {
		out.Error = "来源正文文件不存在"
		return out
	}
	tgtPath, ok := s.bodyPathIn(tgtLib, in.Target.SessionID, tgtCwd)
	if !ok {
		out.Error = "目标内容不存在，未同步"
		return out
	}

	// 同一目标上仍有未完成写入：本轮不得再写一次（等恢复完成）。
	if op := s.pendingOperationForTarget(in.Target.Variant, in.Target.SessionID); op != nil {
		reason := op.LastError
		if reason == "" {
			reason = "等待恢复"
		}
		out.Error = fmt.Sprintf("上一次会话保存尚未完成（操作 %s）：%s，本次未保存", op.OperationID, reason)
		return out
	}

	// 操作日志先落盘（预分配身份），再备份、写入。
	op := &Operation{
		Version:     OperationVersion,
		OperationID: mustNewUUID(),
		Kind:        OperationKindSync,
		Variant:     in.Target.Variant,
		SourceVariant: func() string {
			if in.Source.Variant != in.Target.Variant {
				return in.Source.Variant
			}
			return ""
		}(),
		GroupID:               in.Group.ID,
		Source:                OperationMember{AccountID: in.Source.AccountID, UID: in.Source.UID, SessionID: in.Source.SessionID},
		Target:                OperationMember{AccountID: in.Target.AccountID, UID: in.Target.UID, SessionID: in.Target.SessionID},
		ExpectedContentDigest: srcContent.ready.TotalDigest,
		ExpectedRecordCount:   srcContent.ready.RecordCount,
		Phase:                 PhasePrepared,
		CreatedAt:             nowMillis(),
		UpdatedAt:             nowMillis(),
	}
	if err := s.saveOperation(op); err != nil {
		out.Error = "操作记录写入失败，已放弃同步：" + err.Error()
		return out
	}

	// 备份目标正文 → 写入（重写 sessionId）→ 回读校验。
	entry, err := localappsBackupFor(fmt.Sprintf("同步会话 %s → %s", in.Source.SessionID, in.Target.SessionID), tgtPath)
	if err != nil {
		s.abandonOperation(op, "备份失败，已放弃同步："+err.Error())
		out.Error = fmt.Sprintf("备份失败，已放弃同步: %v", err)
		return out
	}
	op.Backup = entry
	_ = s.saveOperation(op)
	out.BackupID = entry
	if _, err := s.writeCopyBody(srcLib, tgtLib, srcPath, in.Source.SessionID, in.Target.SessionID, true); err != nil {
		s.failOperation(op, err.Error())
		out.Error = fmt.Sprintf("%s（可用备份 %s 恢复）", err.Error(), entry)
		return out
	}
	if err := s.advanceOperation(op, PhaseBodyWritten); err != nil {
		s.failOperation(op, err.Error())
		out.Error = err.Error()
		return out
	}
	out.Applied = true
	out.OK = true
	out.Notes = append(out.Notes, "已写入目标正文并回读校验")

	// 写新的配对基线 + 更新 lastSyncedAt（对照 commit_sync_baseline）。
	digests := srcContent.ready.LineDigests
	if err := s.commitSyncBaseline(in.Group.ID, in.Source.MemberID, in.Target.MemberID, digests); err != nil {
		s.failOperation(op, err.Error())
		out.Notes = append(out.Notes, "提示：同步记录更新失败："+err.Error())
		return out
	}
	if err := s.advanceOperation(op, PhaseLinksCommitted); err != nil {
		s.failOperation(op, err.Error())
		return out
	}
	if err := s.advanceOperation(op, PhaseCompleted); err != nil {
		s.failOperation(op, err.Error())
		return out
	}
	op.CleanupState = CleanupStateCleaned
	_ = s.saveOperation(op)
	_ = s.pruneOperations(in.Target.Variant, KeepCompletedOperations)
	return out
}

// commitSyncBaseline 写新的配对基线并更新目标成员的 lastSyncedAt。
func (s *Store) commitSyncBaseline(groupID, srcMemberID, tgtMemberID string, digests []string) error {
	if len(digests) == 0 {
		return fmt.Errorf("来源内容不可验证，未写同步记录")
	}
	ref := mustNewUUID()
	if err := s.saveBaselineRecord(&BaselineRecord{
		Version: LinkStoreVersion, BaselineRef: ref,
		NormalizationVersion: NormalizationVersion, CreatedAt: nowMillis(),
		RecordCount: len(digests), TotalDigest: totalDigestOf(digests), LineDigests: digests,
	}); err != nil {
		return err
	}
	return s.withLinkStoreWrite(func(store *LinkStoreFile) error {
		for gi := range store.Groups {
			if store.Groups[gi].ID != groupID {
				continue
			}
			setPairBase(&store.Groups[gi], srcMemberID, tgtMemberID, ref)
			for mi := range store.Groups[gi].Members {
				if store.Groups[gi].Members[mi].MemberID == tgtMemberID {
					now := nowMillis()
					store.Groups[gi].Members[mi].LastSyncedAt = &now
				}
			}
		}
		return nil
	})
}

// pendingOperationForTarget 找目标会话上未完成的写入操作。
func (s *Store) pendingOperationForTarget(variant, targetSessionID string) *Operation {
	for _, op := range s.pendingOperations(variant) {
		if op.Target.SessionID == targetSessionID {
			o := op
			return &o
		}
	}
	return nil
}

// UnifyTarget 是整组统一里一个目标的执行输入（模式 + 预览凭据）。
type UnifyTarget struct {
	Mode         string `json:"mode"`
	PreviewToken string `json:"preview_token"`
}

// UnifyGroup 以指定来源为准统一组内其它成员（对照整组统一：逐目标带模式与凭据）。
//
// 全部目标先校验（凭据存在、模式合法）——校验不通过就**不打断用户**（不关客户端）；
// 校验通过后关闭「运行中且当前登录是写入目标」的客户端，写完全部目标再重新打开。
func (s *Store) UnifyGroup(groupID, sourceMemberID string, targets map[string]UnifyTarget) (*GroupSyncReport, error) {
	store, _, err := s.loadLinkStore()
	if err != nil {
		return nil, err
	}
	g := findGroupR(store, groupID)
	if g == nil {
		return nil, ErrGroupNotFound
	}
	var srcMember *LinkMember
	for i := range g.Members {
		if g.Members[i].MemberID == sourceMemberID {
			srcMember = &g.Members[i]
			break
		}
	}
	if srcMember == nil {
		return nil, ErrSourceMemberNotFound
	}
	// 校验阶段：目标不重复、模式合法、凭据可读（对照 prepare_unify_targets）。
	type prepared struct {
		member LinkMember
		target UnifyTarget
	}
	var prep []prepared
	seen := map[string]bool{}
	for _, m := range g.Members {
		if m.MemberID == sourceMemberID {
			continue
		}
		t, ok := targets[m.MemberID]
		if !ok {
			continue
		}
		if seen[m.MemberID] {
			return nil, fmt.Errorf("目标副本重复，请重新检查会话")
		}
		seen[m.MemberID] = true
		if _, err := parseMode(t.Mode); err != nil {
			return nil, err
		}
		if s.loadPreviewToken(t.PreviewToken) == nil {
			return nil, fmt.Errorf("检查结果不存在或已失效，请重新检查后再操作")
		}
		prep = append(prep, prepared{member: m, target: t})
	}

	gCopy := *g
	report := newGroupSyncReport()
	report.SourceMemberID = sourceMemberID

	// 生命周期窗口：只关闭「运行中且当前登录是写入目标」的档位，只重开本次关闭的档位。
	closeTargets := make([]LinkMember, 0, len(prep))
	for _, p := range prep {
		closeTargets = append(closeTargets, p.member)
	}
	closed, err := closeClientsForTargets(closeTargets)
	if err != nil {
		return nil, err
	}

	for _, p := range prep {
		out := s.SyncMemberPair(SyncPairInput{Group: &gCopy, Source: *srcMember, Target: p.member, Mode: p.target.Mode, PreviewToken: p.target.PreviewToken})
		switch {
		case out.Error != "":
			report.Errors = append(report.Errors, GroupSyncError{MemberID: p.member.MemberID, Error: out.Error})
		case out.Applied:
			report.Synced = append(report.Synced, GroupSyncItem{MemberID: p.member.MemberID, OK: true, Notes: out.Notes})
		default:
			report.Skipped = append(report.Skipped, GroupSyncSkip{MemberID: p.member.MemberID, Reason: out.Reason, ReasonCode: out.ReasonCode})
		}
		if report.NeedsRecovery {
			break
		}
	}
	report.RestartedVariants = s.settleClosedClients(closed, report)
	return report, nil
}

// SafeBatchSync 自动挑安全源，把落后的副本追加上来（对照 sync_safe_batch）。
//
// 服务端自己重算预览并签发凭据：不再属于快进范围的项进 skipped
// （reasonCode：重新检查后该副本不再属于安全快进范围），不沿用任何旧结论。
func (s *Store) SafeBatchSync(groupID string, uidToAccount map[string]AccountLabel) (*GroupSyncReport, error) {
	view := s.GetRegistryGroup(groupID, uidToAccount)
	if view == nil {
		return nil, ErrGroupNotFound
	}
	store, _, err := s.loadLinkStore()
	if err != nil {
		return nil, err
	}
	g := findGroupR(store, groupID)
	if g == nil {
		return nil, ErrGroupNotFound
	}
	report := newGroupSyncReport()
	if view.SafeSourceMemberID == "" {
		for _, m := range view.Members {
			if m.State == MemberStateActive {
				report.Skipped = append(report.Skipped, GroupSyncSkip{MemberID: m.MemberID, Reason: "组内没有可安全同步的来源（内容互有分歧或正文不可读）"})
			}
		}
		return report, nil
	}
	report.SourceMemberID = view.SafeSourceMemberID
	var srcMember *LinkMember
	for i := range g.Members {
		if g.Members[i].MemberID == view.SafeSourceMemberID {
			srcMember = &g.Members[i]
			break
		}
	}
	if srcMember == nil {
		return nil, ErrSourceMemberNotFound
	}
	// 准备阶段：全部目标的复核与凭据都在关闭客户端之前完成。
	targets := map[string]UnifyTarget{}
	prep := make([]LinkMember, 0)
	for _, m := range view.Members {
		if m.MemberID == view.SafeSourceMemberID || m.State != MemberStateActive || m.VersionStatus != "behind" {
			continue
		}
		var member *LinkMember
		for i := range g.Members {
			if g.Members[i].MemberID == m.MemberID {
				member = &g.Members[i]
				break
			}
		}
		if member == nil {
			continue
		}
		decision := s.decideForPair(g, *srcMember, *member)
		if decision.Verdict != VerdictFastForward {
			report.Skipped = append(report.Skipped, GroupSyncSkip{
				MemberID: m.MemberID, ReasonCode: "recheckNotFastForward",
				Reason: "重新检查后该副本不再属于安全快进范围",
			})
			continue
		}
		binding := s.livePreviewBinding(g, *srcMember, *member, decision.Verdict)
		token, err := s.savePreviewToken(binding)
		if err != nil {
			return nil, fmt.Errorf("重新预览未返回可执行凭据：%w", err)
		}
		targets[m.MemberID] = UnifyTarget{Mode: ModeFastForward, PreviewToken: token}
		prep = append(prep, *member)
	}
	if len(targets) == 0 {
		return report, nil
	}
	// 复用 UnifyGroup 的生命周期窗口与执行语义。
	unified, err := s.UnifyGroup(groupID, view.SafeSourceMemberID, targets)
	if err != nil {
		return nil, err
	}
	// 合并准备阶段的跳过项与执行结果。
	unified.Skipped = append(report.Skipped, unified.Skipped...)
	return unified, nil
}

// PreviewPair 预览一对成员的同步判定（verdict/可用模式/记录数），并签发服务端凭据。
func (s *Store) PreviewPair(groupID, sourceMemberID, targetMemberID string) (*PairPreview, error) {
	store, _, err := s.loadLinkStore()
	if err != nil {
		return nil, err
	}
	g := findGroupR(store, groupID)
	if g == nil {
		return nil, ErrGroupNotFound
	}
	var src, tgt *LinkMember
	for i := range g.Members {
		if g.Members[i].MemberID == sourceMemberID {
			src = &g.Members[i]
		}
		if g.Members[i].MemberID == targetMemberID {
			tgt = &g.Members[i]
		}
	}
	if src == nil || tgt == nil {
		return nil, ErrSourceMemberNotFound
	}
	d := s.decideForPair(g, *src, *tgt)
	// ahead 的显式覆盖入口：组级统一可改用 unifyOverwrite（对照 preview_resolved_pair）。
	if d.Verdict == VerdictAhead {
		d.AvailableModes = []string{ModeUnifyOverwrite}
	}
	binding := s.livePreviewBinding(g, *src, *tgt, d.Verdict)
	token, err := s.savePreviewToken(binding)
	if err != nil {
		return nil, err
	}
	return &PairPreview{
		Verdict: d.Verdict, Reason: d.Reason,
		SourceOnly: d.SourceOnly, TargetOnly: d.TargetOnly,
		AvailableModes: d.AvailableModes,
		PreviewToken:   token,
	}, nil
}

// -----------------------------------------------------------------------------
// 取消 / 删除关联（对照 remove_member / delete_group：纯登记变更，内容不动）
// -----------------------------------------------------------------------------

// UnlinkMemberResult 是「取消关联」的结果。
type UnlinkMemberResult struct {
	MemberID     string `json:"member_id"`
	GroupRemoved bool   `json:"group_removed"`
	Remaining    int    `json:"remaining"`
	Notes        []string
}

// UnlinkMember 把成员从组里删除（组内已无成员时整组删除）。
func (s *Store) UnlinkMember(groupID, memberID string) (*UnlinkMemberResult, error) {
	memberID = strings.TrimSpace(memberID)
	if memberID == "" {
		return nil, fmt.Errorf("缺少要解除的成员（member_id）")
	}
	res := &UnlinkMemberResult{MemberID: memberID}
	err := s.withLinkStoreWrite(func(store *LinkStoreFile) error {
		g := findGroupR(store, groupID)
		if g == nil {
			return ErrGroupNotFound
		}
		found := false
		for _, m := range g.Members {
			if m.MemberID == memberID {
				found = true
				break
			}
		}
		if !found {
			return ErrSourceMemberNotFound
		}
		res.Remaining = removeMemberFromGroup(g, memberID)
		if res.Remaining == 0 {
			s.removeGroupFromStore(store, groupID)
			res.GroupRemoved = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Notes = append(res.Notes, "已解除关联（会话内容未改动）")
	if res.GroupRemoved {
		res.Notes = append(res.Notes, "该会话组已无成员，已删除")
	}
	return res, nil
}

// DeleteGroup 删除整个会话组（登记项删除，会话内容不受影响）。
// 返回解除的成员数。
func (s *Store) DeleteGroup(groupID string) (int, error) {
	n := 0
	err := s.withLinkStoreWrite(func(store *LinkStoreFile) error {
		g := findGroupR(store, groupID)
		if g == nil {
			return ErrGroupNotFound
		}
		n = len(g.Members)
		s.removeGroupFromStore(store, groupID)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) removeGroupFromStore(store *LinkStoreFile, groupID string) {
	kept := store.Groups[:0]
	for _, g := range store.Groups {
		if g.ID != groupID {
			kept = append(kept, g)
		}
	}
	store.Groups = kept
}

func findGroupR(store *LinkStoreFile, groupID string) *LinkGroupR {
	for i := range store.Groups {
		if store.Groups[i].ID == groupID {
			return &store.Groups[i]
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// 一次性迁移：把「内容推导分组」时代已经存在的同源副本登记进表，
// 并把旧的配对基线（session-sync-baselines.json）转成成员对基线。
// 只在登记表缺失且未跑过迁移时执行一次；之后以登记表为准。
// -----------------------------------------------------------------------------

const registryMigratedMarker = "session-links-registry-migrated"

func (s *Store) migratedMarkerPath() string {
	return filepath.Join(s.StateDir, registryMigratedMarker)
}

// BootstrapRegistryIfNeeded 做一次性迁移；登记表已存在时直接返回。
func (s *Store) BootstrapRegistryIfNeeded(uidToAccount map[string]AccountLabel) error {
	if _, err := os.Stat(s.linkFile()); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(s.migratedMarkerPath()); err == nil {
		return nil
	}
	// 旧内容分组：≥2 份副本、且**跨 uid**（同账号的两份不是「关联」）。
	items, _, _, err := s.loadSessionItems(uidToAccount)
	if err != nil {
		return err
	}
	oldBaselines := s.loadLegacyBaselines()
	err = s.withLinkStoreWrite(func(store *LinkStoreFile) error {
		if len(store.Groups) > 0 {
			return nil // 并发下别人才建过：不覆盖
		}
		clusters := clusterItems(items)
		for _, idxs := range clusters {
			if len(idxs) < 2 {
				continue
			}
			uids := map[string]bool{}
			for _, i := range idxs {
				uids[items[i].sess.UserID] = true
			}
			if len(uids) < 2 {
				continue
			}
			groupID := mustNewUUID()
			g := LinkGroupR{ID: groupID, Variant: items[idxs[0]].sess.Variant, CreatedAt: nowMillis(), Members: []LinkMember{}, PairBases: []PairBase{}}
			var memberIDs []string
			var memberBySession = map[string]string{}
			for _, i := range idxs {
				it := items[i]
				mid := mustNewUUID()
				memberBySession[it.sess.ID] = mid
				memberIDs = append(memberIDs, mid)
				lbl := uidToAccount[it.sess.UserID]
				g.Members = append(g.Members, LinkMember{
					MemberID: mid, AccountID: lbl.Account, UID: it.sess.UserID,
					SessionID: it.sess.ID, Variant: it.variant,
					State: MemberStateActive, LinkedAt: nowMillis(),
				})
			}
			// 两两建基线：有内容的一方为准；旧基线能对上的沿用旧摘要。
			for a := 0; a < len(memberIDs); a++ {
				for b := a + 1; b < len(memberIDs); b++ {
					sa, sb := s.sessionIDOfMember(&g, memberIDs[a]), s.sessionIDOfMember(&g, memberIDs[b])
					digests := s.pairBaselineDigests(sa, sb, oldBaselines)
					if digests == nil {
						continue
					}
					ref := mustNewUUID()
					if err := s.saveBaselineRecord(&BaselineRecord{
						Version: LinkStoreVersion, BaselineRef: ref,
						NormalizationVersion: NormalizationVersion, CreatedAt: nowMillis(),
						RecordCount: len(digests), TotalDigest: totalDigestOf(digests), LineDigests: digests,
					}); err != nil {
						return err
					}
					setPairBase(&g, memberIDs[a], memberIDs[b], ref)
				}
			}
			sortPairBases(&g)
			store.Groups = append(store.Groups, g)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.WriteFile(s.migratedMarkerPath(), []byte(strconv.FormatInt(nowMillis(), 10)), 0o600)
}

func (s *Store) sessionIDOfMember(g *LinkGroupR, memberID string) string {
	for _, m := range g.Members {
		if m.MemberID == memberID {
			return m.SessionID
		}
	}
	return ""
}

// pairBaselineDigests 取一对会话的旧基线摘要；没有旧基线时用「行数较少一方」
// 的全量摘要（两份当时一致的最保守假设）。
func (s *Store) pairBaselineDigests(a, b string, old map[string][]string) []string {
	if d, ok := old[pairID(a, b)]; ok {
		return d
	}
	if d, ok := old[a+"\x00"+b]; ok {
		return d
	}
	if d, ok := old[b+"\x00"+a]; ok {
		return d
	}
	da, errA := s.bodyDigests(a)
	db, errB := s.bodyDigests(b)
	if errA != nil || errB != nil {
		return nil
	}
	if len(da) <= len(db) {
		return da
	}
	return db
}

// loadLegacyBaselines 读取旧版配对基线（session-sync-baselines.json）。
func (s *Store) loadLegacyBaselines() map[string][]string {
	out := map[string][]string{}
	raw, err := os.ReadFile(filepath.Join(s.StateDir, "session-sync-baselines.json"))
	if err != nil {
		return out
	}
	var doc struct {
		Pairs map[string]struct {
			Digests []string `json:"digests"`
		} `json:"pairs"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return out
	}
	for k, v := range doc.Pairs {
		out[k] = v.Digests
	}
	return out
}

// localappsBackupFor 是 localapps.BackupFor 的包内小包装（避免每处写全前缀）。
func localappsBackupFor(reason string, paths ...string) (string, error) {
	entry, err := localapps.BackupFor(localapps.TargetSessions, paths, reason)
	if err != nil {
		return "", err
	}
	return entry.ID, nil
}

var _ = sql.Open // 保持 import（readSessionRow 的调用方在别处）

// SyncBySessionIDs 按会话 id 对（而不是成员 id）执行一次同步。
//
// 这是旧「/local-sessions/sync」端点的兼容入口：两个会话必须已经在同一个
// 登记组里（switch 的语义就是「只有关联过的副本才能互相同步」）。
// force 映射为 overwrite 模式（对 diverge 的显式覆盖）；ahead 需要
// unifyOverwrite，走关联会话页的「以此为准」。
func (s *Store) SyncBySessionIDs(sourceID, targetID string, force bool) (*GroupSyncItem, error) {
	store, _, err := s.loadLinkStore()
	if err != nil {
		return nil, err
	}
	groupID := ""
	var srcMember, tgtMember LinkMember
	found := false
	for gi := range store.Groups {
		g := &store.Groups[gi]
		var sm, tm *LinkMember
		for _, m := range g.Members {
			if m.SessionID == sourceID && m.State == MemberStateActive {
				sm = &m
			}
			if m.SessionID == targetID && m.State == MemberStateActive {
				tm = &m
			}
		}
		if sm != nil && tm != nil {
			groupID = g.ID
			srcMember, tgtMember = *sm, *tm
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("这两个会话还没有建立关联（只有通过本工具复制/登记过的副本才能互相同步）")
	}
	mode := ModeFastForward
	if force {
		mode = ModeOverwrite
	}
	g := findGroupR(store, groupID)
	// 兼容入口也要走预览凭据：服务端自己现算一次绑定并签发（不绕过版本复核）。
	decision := s.decideForPair(g, srcMember, tgtMember)
	token, err := s.savePreviewToken(s.livePreviewBinding(g, srcMember, tgtMember, decision.Verdict))
	if err != nil {
		return nil, fmt.Errorf("检查结果签发失败：%w", err)
	}
	out := s.SyncMemberPair(SyncPairInput{Group: g, Source: srcMember, Target: tgtMember, Mode: mode, PreviewToken: token})
	if out.Error != "" {
		return &GroupSyncItem{MemberID: tgtMember.MemberID, Notes: out.Notes}, fmt.Errorf("%s", out.Error)
	}
	return &GroupSyncItem{MemberID: tgtMember.MemberID, OK: out.Applied, Notes: out.Notes}, nil
}
