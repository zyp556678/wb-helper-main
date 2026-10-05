package localsessions

// -----------------------------------------------------------------------------
// 操作日志与未完成操作恢复（对照 wb-switch 的 session_link.rs 操作日志 +
// session.rs 的 recover_pending_session_operations）
//
// **为什么需要它**：复制/同步是「写正文 + 插数据库行 + 登记关联」的多步操作，
// 任何一步之间进程被杀都会留下半成品。没有操作日志时，下一次操作只能看到
// 「目标账号多了一条没有登记的会话」——既不敢删也不敢继续，用户只能手工收拾。
//
// 设计（与 switch 一致）：
//   - 每次复制/同步先预分配身份（目标会话 id、组 id）并**先落盘操作日志**，
//     再动任何业务数据；
//   - 阶段只前进（prepared → body_written → db_written → mapping_written →
//     links_committed → completed），已越过的阶段在恢复时只核验、不重放；
//   - 恢复先检查实际状态再决定下一步，**绝不覆盖无法确认的内容**：
//     中间产物被改动/丢失时只上报 needsRecovery（阻断客户端重开，等人处理）；
//   - 同一（源会话 → 目标账号）已有未完成操作时拒绝重复复制，避免写出第二个副本。
//
// 与 switch 的差异只有一处：备份回收。switch 的复制备份是事务性临时产物，成功后
// 自动回收；本项目的备份是**用户可见的还原点**（localapps 备份中心，一键恢复），
// 因此一律保留、不做自动清理（详见 linkstore.go 顶部的差异说明）。
// -----------------------------------------------------------------------------

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workbuddy-gateway/internal/localapps"
)

const (
	// OperationVersion 是操作日志格式版本（与 switch 的 OPERATION_VERSION 一致）。
	OperationVersion = 1
	// OperationKindCopy / OperationKindSync 是操作类型。
	OperationKindCopy = "copy"
	OperationKindSync = "sync"
	// CleanupStateCleaned / CleanupStateSafeTerminated 与 switch 的清理状态常量同名。
	CleanupStateCleaned        = "cleaned"
	CleanupStateSafeTerminated = "safeTerminated"
	// KeepCompletedOperations 是每档位保留的历史（已完成）操作条数。
	KeepCompletedOperations = 20
	// UnparseableOperationReason 是操作日志无法解析时的原因前缀。
	//
	// 解析失败的操作无法对应到具体会话：继续复制会绕过 pending 去重，
	// 可能对同一请求写出第二个副本 —— 所以它与「扫描不完整」一样必须阻断。
	UnparseableOperationReason = "操作记录无法解析"
)

// OpPhase 是操作阶段：只有走到 completed 才算完整成功。
type OpPhase string

const (
	PhasePrepared       OpPhase = "prepared"
	PhaseBodyWritten    OpPhase = "body_written"
	PhaseDbWritten      OpPhase = "db_written"
	PhaseMappingWritten OpPhase = "mapping_written"
	PhaseLinksCommitted OpPhase = "links_committed"
	PhaseCompleted      OpPhase = "completed"
	// PhaseAbandoned 表示未写入任何内容即放弃（例如源会话已被删除），不是成功。
	PhaseAbandoned OpPhase = "abandoned"
)

// phaseRank 是阶段的推进顺序（恢复时据此判断「是否已越过」）。
func phaseRank(p OpPhase) int {
	switch p {
	case PhasePrepared:
		return 1
	case PhaseBodyWritten:
		return 2
	case PhaseDbWritten:
		return 3
	case PhaseMappingWritten:
		return 4
	case PhaseLinksCommitted:
		return 5
	case PhaseCompleted:
		return 6
	}
	return 0
}

// Unfinished 表示该阶段尚未到达终态。
func (p OpPhase) Unfinished() bool {
	return p != PhaseCompleted && p != PhaseAbandoned
}

// OperationMember 是操作涉及的账号/会话身份。
type OperationMember struct {
	// AccountID 是账号库里的账号 id（仅作展示；身份判定以 uid 为准）。
	AccountID string `json:"accountId,omitempty"`
	UID       string `json:"uid"`
	SessionID string `json:"sessionId"`
}

// Operation 是一条操作日志（对照 switch 的 Operation，字段逐字段对齐）。
type Operation struct {
	Version     int    `json:"version"`
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	// Variant 是**目标档**（写侧）。
	Variant string `json:"variant"`
	// SourceVariant 只在跨档复制时记录（同档与旧记录为空）。
	SourceVariant string `json:"sourceVariant,omitempty"`
	// GroupID 在写入前预分配：恢复时复用同一个组，不会另起新组。
	GroupID string          `json:"groupId"`
	Source  OperationMember `json:"source"`
	Target  OperationMember `json:"target"`
	// ExpectedContentDigest / ExpectedRecordCount 是本次写入内容的快照：
	// 恢复时据此判断中间产物是否被改动。
	ExpectedContentDigest string  `json:"expectedContentDigest"`
	ExpectedRecordCount   int     `json:"expectedRecordCount"`
	Phase                 OpPhase `json:"phase"`
	// Backup 是本次操作的备份标识（本项目为备份中心的条目 id）。
	Backup string `json:"backup,omitempty"`
	// LifecycleVersion 是生命周期标记版本（与 switch 同名字段，本版恒为 1）。
	LifecycleVersion int    `json:"lifecycleVersion,omitempty"`
	CleanupState     string `json:"cleanupState,omitempty"`
	LastError        string `json:"lastError,omitempty"`
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
}

// OperationScan 是操作日志目录的扫描结果。
//
// Complete 为 false 表示目录不可读或枚举失败：调用方**不得**把结果当作
// 「不存在对应操作」来授权写入（与解析失败同口径阻断）。
type OperationScan struct {
	Operations []Operation
	Problems   []string
	Complete   bool
}

// RecoveryIssue 是一条需要人工处理的恢复问题。
type RecoveryIssue struct {
	OperationID string `json:"operationId"`
	Reason      string `json:"reason"`
	// Retryable 表示重试可能成功；false 表示需要人工处理，不得盲目重放。
	Retryable bool `json:"retryable"`
}

// RecoveryReport 是一次恢复的结果。
type RecoveryReport struct {
	Recovered     []string        `json:"recovered"`
	Abandoned     []string        `json:"abandoned"`
	NeedsRecovery []RecoveryIssue `json:"needsRecovery"`
}

// Clean 表示本次恢复没有留下需要人工处理的问题。
func (r *RecoveryReport) Clean() bool { return len(r.NeedsRecovery) == 0 }

// Empty 表示本次恢复什么都没做。
func (r *RecoveryReport) Empty() bool {
	return len(r.Recovered) == 0 && len(r.Abandoned) == 0 && len(r.NeedsRecovery) == 0
}

// -----------------------------------------------------------------------------
// 读写
// -----------------------------------------------------------------------------

func (s *Store) operationFile(id string) string {
	return filepath.Join(s.operationsDir(), id+".json")
}

// saveOperation 原子写入操作日志（临时文件 + rename；调用方保证 id 已生成）。
func (s *Store) saveOperation(op *Operation) error {
	if err := os.MkdirAll(s.operationsDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return err
	}
	return durableWriteFile(s.operationFile(op.OperationID), append(data, '\n'))
}

// durableWriteFile 写文件并尽量保证「要么旧内容、要么新内容」：
// 临时文件 → fsync → rename → 尽力同步父目录。
//
// Windows 上 rename 覆盖已存在文件是原子的（ReplaceFile 语义），无需先删。
func durableWriteFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir 尽力把目录项同步到磁盘（失败不影响结果：最坏情况是崩溃后丢一次 rename）。
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// scanOperations 扫描全部操作日志（不区分档位）；解析失败的文件作为问题上报。
func (s *Store) scanOperations() OperationScan {
	scan := OperationScan{Complete: true}
	entries, err := os.ReadDir(s.operationsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return scan
		}
		scan.Complete = false
		scan.Problems = append(scan.Problems, "操作日志目录不可读："+err.Error())
		return scan
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.operationsDir(), entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			scan.Problems = append(scan.Problems, entry.Name()+": "+err.Error())
			continue
		}
		var op Operation
		if err := json.Unmarshal(raw, &op); err != nil {
			scan.Problems = append(scan.Problems, entry.Name()+": "+err.Error())
			continue
		}
		scan.Operations = append(scan.Operations, op)
	}
	sort.Slice(scan.Operations, func(i, j int) bool {
		return scan.Operations[i].CreatedAt < scan.Operations[j].CreatedAt
	})
	return scan
}

// pendingOperations 收集某档位未完成的操作。
func (s *Store) pendingOperations(variant string) []Operation {
	var out []Operation
	for _, op := range s.scanOperations().Operations {
		if op.Variant == variant && op.Phase.Unfinished() {
			out = append(out, op)
		}
	}
	return out
}

// findPendingOperation 找与本次请求同一（源会话 → 目标账号）的未完成操作。
func findPendingOperation(ops []Operation, sourceUID, sourceSessionID, targetUID string) *Operation {
	for i := range ops {
		op := &ops[i]
		if op.Phase.Unfinished() &&
			op.Source.UID == sourceUID &&
			op.Source.SessionID == sourceSessionID &&
			op.Target.UID == targetUID {
			return op
		}
	}
	return nil
}

// pruneOperations 清理已完成/已放弃的历史操作日志，每档位保留最近 keep 条。
func (s *Store) pruneOperations(variant string, keep int) int {
	scan := s.scanOperations()
	if !scan.Complete {
		return 0
	}
	var finished []Operation
	for _, op := range scan.Operations {
		if op.Variant == variant && !op.Phase.Unfinished() {
			finished = append(finished, op)
		}
	}
	if len(finished) <= keep {
		return 0
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].UpdatedAt > finished[j].UpdatedAt })
	removed := 0
	for _, op := range finished[keep:] {
		if os.Remove(s.operationFile(op.OperationID)) == nil {
			removed++
		}
	}
	return removed
}

// ensureLinkStoreReady 保证主文件已存在（首次使用时先落地空表）。
//
// **顺序不能反**：操作日志出现必须晚于主文件存在，否则 loadLinkStore 会把
// 「主文件缺失 + 有未完成操作」判成异常现场而自锁（对照 switch 的同名函数）。
func (s *Store) ensureLinkStoreReady() error {
	_, existed, err := s.loadLinkStore()
	if err != nil {
		return fmt.Errorf("%w；已阻止复制", err)
	}
	if existed {
		return nil
	}
	return s.withLinkStoreWrite(func(*LinkStoreFile) error { return nil })
}

// -----------------------------------------------------------------------------
// 阶段推进
// -----------------------------------------------------------------------------

// advanceOperation 推进操作阶段：阶段只能前进，已越过时不回写。
//
// 先保存候选副本、成功后才替换内存状态：保存失败时内存阶段不变，
// 随后的 failOperation 不会把未落盘的阶段写回磁盘。
func (s *Store) advanceOperation(op *Operation, phase OpPhase) error {
	if phaseRank(op.Phase) >= phaseRank(phase) {
		return nil
	}
	candidate := *op
	candidate.Phase = phase
	candidate.UpdatedAt = nowMillis()
	if err := s.saveOperation(&candidate); err != nil {
		return err
	}
	*op = candidate
	return nil
}

// failOperation 记录失败原因（阶段不变；失败不改变「已越过」的判断）。
func (s *Store) failOperation(op *Operation, errText string) {
	op.LastError = errText
	op.UpdatedAt = nowMillis()
	_ = s.saveOperation(op)
}

// abandonOperation 放弃一个未写入任何会话内容的操作（不算成功）。
func (s *Store) abandonOperation(op *Operation, reason string) {
	op.Phase = PhaseAbandoned
	op.CleanupState = CleanupStateSafeTerminated
	op.LastError = reason
	op.UpdatedAt = nowMillis()
	_ = s.saveOperation(op)
}

// -----------------------------------------------------------------------------
// 恢复
// -----------------------------------------------------------------------------

// RecoverPendingOperations 恢复某档位全部未完成操作（对照
// recover_pending_session_operations）。
//
// 恢复先检查实际状态再决定下一步，不盲目重放；中间产物被改动或丢失时只上报
// needsRecovery，不覆盖未知内容。
func (s *Store) RecoverPendingOperations(variant string) *RecoveryReport {
	report := &RecoveryReport{Recovered: []string{}, Abandoned: []string{}, NeedsRecovery: []RecoveryIssue{}}
	scan := s.scanOperations()
	// 扫描不完整（目录不可读/枚举失败）不能按「没有未完成操作」继续写。
	if !scan.Complete {
		report.NeedsRecovery = append(report.NeedsRecovery, RecoveryIssue{
			OperationID: "operation-scan",
			Reason:      UnparseableOperationReason + "（操作记录无法读取），已停止恢复以免产生重复复制",
		})
	}
	for _, problem := range scan.Problems {
		report.NeedsRecovery = append(report.NeedsRecovery, RecoveryIssue{
			OperationID: problem,
			Reason:      UnparseableOperationReason + "（" + problem + "），已停止恢复以免产生重复复制",
		})
	}
	for _, op := range scan.Operations {
		if op.Variant != variant || !op.Phase.Unfinished() {
			continue
		}
		outcome, issue := s.recoverOperation(op)
		switch outcome {
		case "recovered":
			report.Recovered = append(report.Recovered, op.OperationID)
		case "abandoned":
			report.Abandoned = append(report.Abandoned, op.OperationID)
		default:
			report.NeedsRecovery = append(report.NeedsRecovery, *issue)
		}
	}
	return report
}

// recoverOperation 恢复单个操作：按「持久化阶段 + 实际状态」逐阶段判断。
func (s *Store) recoverOperation(op Operation) (string, *RecoveryIssue) {
	if op.Kind == OperationKindSync {
		return s.recoverSyncOperation(op)
	}
	return s.recoverCopyOperation(op)
}

// bodyCheck 是恢复第一步「目标正文现状」的判定结果。
type bodyCheck int

const (
	bodyAbsent bodyCheck = iota
	bodyVerified
	bodyNeedsRecovery
)

// checkTargetBody 核对目标正文与操作记录里的摘要。
func (s *Store) checkTargetBody(op *Operation) (bodyCheck, string, *NormalizedContent) {
	lib := s.libByVariant(op.Variant)
	if lib == nil || !libExists(lib) {
		if op.Phase.Unfinished() && phaseRank(op.Phase) >= phaseRank(PhaseBodyWritten) {
			return bodyNeedsRecovery, "目标内容丢失，已停止恢复", nil
		}
		return bodyAbsent, "", nil
	}
	path, ok := s.bodyPathIn(lib, op.Target.SessionID, "")
	if !ok {
		if phaseRank(op.Phase) >= phaseRank(PhaseBodyWritten) {
			return bodyNeedsRecovery, "目标内容丢失，已停止恢复", nil
		}
		return bodyAbsent, "", nil
	}
	text, err := os.ReadFile(path)
	if err != nil {
		if phaseRank(op.Phase) >= phaseRank(PhaseBodyWritten) {
			return bodyNeedsRecovery, "目标内容不可验证（读取失败），已停止恢复", nil
		}
		return bodyAbsent, "", nil
	}
	norm, err := normalizeJSONL(string(text), op.Target.SessionID)
	if err != nil {
		return bodyNeedsRecovery, "目标内容不可验证（" + err.Error() + "），已停止恢复", nil
	}
	if norm.TotalDigest != op.ExpectedContentDigest {
		return bodyNeedsRecovery, "目标内容与操作记录不一致（可能被其它程序改动），已停止恢复", nil
	}
	return bodyVerified, "", norm
}

// recoverCopyOperation 恢复一次复制。
func (s *Store) recoverCopyOperation(op Operation) (string, *RecoveryIssue) {
	needs := func(reason string, retryable bool) (string, *RecoveryIssue) {
		return "needs", &RecoveryIssue{OperationID: op.OperationID, Reason: reason, Retryable: retryable}
	}
	// 读侧库：跨档操作按 sourceVariant 解析；同档与旧记录用目标档。
	readVariant := op.Variant
	if op.SourceVariant != "" {
		readVariant = op.SourceVariant
	}
	srcLib := s.libByVariant(readVariant)
	tgtLib := s.libByVariant(op.Variant)

	// 1) 目标正文：已写成则直接复用；未写成则用当前源内容补写；被改动则停止。
	check, reason, normalized := s.checkTargetBody(&op)
	if check == bodyNeedsRecovery {
		return needs(reason, false)
	}
	if check == bodyAbsent {
		srcPath := ""
		if srcLib != nil && libExists(srcLib) {
			srcPath, _ = s.bodyPathIn(srcLib, op.Source.SessionID, "")
		}
		if srcPath == "" {
			s.abandonOperation(&op, "源会话已不可用，且没有复制出任何会话，已放弃该操作")
			return "abandoned", nil
		}
		text, err := os.ReadFile(srcPath)
		if err != nil {
			s.abandonOperation(&op, "源会话已不可用，且没有复制出任何会话，已放弃该操作")
			return "abandoned", nil
		}
		norm, err := normalizeJSONL(string(text), op.Source.SessionID)
		if err != nil {
			s.abandonOperation(&op, "源会话内容无法验证，且没有复制出任何会话，已放弃该操作")
			return "abandoned", nil
		}
		// 源内容在本机发生了变化：按当前内容继续（副本是快照复制，不是同步）。
		if norm.TotalDigest != op.ExpectedContentDigest {
			op.ExpectedContentDigest = norm.TotalDigest
			op.ExpectedRecordCount = norm.RecordCount
		}
		if _, err := s.writeCopyBody(srcLib, tgtLib, srcPath, op.Source.SessionID, op.Target.SessionID, op.Kind == OperationKindSync); err != nil {
			s.failOperation(&op, err.Error())
			return needs(err.Error(), true)
		}
		if err := s.advanceOperation(&op, PhaseBodyWritten); err != nil {
			return needs(err.Error(), true)
		}
		normalized = norm
	}

	// 2) 数据库行：缺失则补写，归属异常则停止。
	owner, rowErr := s.sessionRowOwner(tgtLib, op.Target.SessionID)
	if rowErr == nil && owner != "" && owner != op.Target.UID {
		return needs("目标会话记录归属异常，已停止恢复", false)
	}
	if rowErr == nil && owner == "" {
		if phaseRank(op.Phase) >= phaseRank(PhaseDbWritten) {
			return needs("目标会话记录丢失，已停止恢复", false)
		}
		if err := s.insertSessionRowFromSource(srcLib, tgtLib, op.Source.SessionID, op.Target.SessionID, op.Target.UID, ""); err != nil {
			s.failOperation(&op, err.Error())
			return needs(err.Error(), true)
		}
		if err := s.verifySessionRowOwner(tgtLib, op.Target.SessionID, op.Target.UID); err != nil {
			return needs(err.Error(), false)
		}
	}
	if err := s.advanceOperation(&op, PhaseDbWritten); err != nil {
		return needs(err.Error(), true)
	}
	// 3) 云端映射登记交接给客户端（不预写映射库，见 copy_one_session 的说明）。
	if err := s.advanceOperation(&op, PhaseMappingWritten); err != nil {
		return needs(err.Error(), true)
	}
	// 4) 关联与基线：已提交过就不再 commit，但同样核验产物仍在。
	if phaseRank(op.Phase) < phaseRank(PhaseLinksCommitted) {
		groupID, err := s.commitLinks(op.GroupID, readVariant, op.Source.UID, op.Source.SessionID, op.Source.AccountID,
			op.Target.UID, op.Target.SessionID, op.Variant, op.Target.AccountID, normalized.LineDigests)
		if err != nil {
			s.failOperation(&op, err.Error())
			return needs(err.Error(), true)
		}
		op.GroupID = groupID
		if err := s.advanceOperation(&op, PhaseLinksCommitted); err != nil {
			return needs(err.Error(), true)
		}
	} else if err := s.committedLinksPresent(&op); err != nil {
		return needs(err.Error(), false)
	}
	if err := s.advanceOperation(&op, PhaseCompleted); err != nil {
		return needs(err.Error(), true)
	}
	op.CleanupState = CleanupStateCleaned
	_ = s.saveOperation(&op)
	return "recovered", nil
}

// recoverSyncOperation 恢复一次同步：先校验现场，再补完，绝不覆盖未知内容。
func (s *Store) recoverSyncOperation(op Operation) (string, *RecoveryIssue) {
	needs := func(reason string, retryable bool) (string, *RecoveryIssue) {
		return "needs", &RecoveryIssue{OperationID: op.OperationID, Reason: reason, Retryable: retryable}
	}
	check, reason, normalized := s.checkTargetBody(&op)
	if check == bodyNeedsRecovery {
		return needs(reason, false)
	}
	if check == bodyAbsent {
		srcLib := s.libByVariant(op.SourceVariant)
		tgtLib := s.libByVariant(op.Variant)
		srcPath := ""
		if srcLib != nil && libExists(srcLib) {
			srcPath, _ = s.bodyPathIn(srcLib, op.Source.SessionID, "")
		}
		if srcPath == "" {
			s.abandonOperation(&op, "源会话已不可用，且没有写入任何内容，已放弃该操作")
			return "abandoned", nil
		}
		if _, err := s.writeCopyBody(srcLib, tgtLib, srcPath, op.Source.SessionID, op.Target.SessionID, true); err != nil {
			s.failOperation(&op, err.Error())
			return needs(err.Error(), true)
		}
		text, err := os.ReadFile(srcPath)
		if err != nil {
			return needs("源内容无法确认，已停止恢复", false)
		}
		norm, err := normalizeJSONL(string(text), op.Source.SessionID)
		if err != nil {
			return needs("源内容无法确认，已停止恢复", false)
		}
		normalized = norm
		if err := s.advanceOperation(&op, PhaseBodyWritten); err != nil {
			return needs(err.Error(), true)
		}
	}
	if phaseRank(op.Phase) < phaseRank(PhaseCompleted) {
		srcMemberID, tgtMemberID, memberErr := s.syncOperationMembers(&op)
		if memberErr != nil {
			return needs(memberErr.Error(), false)
		}
		if err := s.commitSyncBaseline(op.GroupID, srcMemberID, tgtMemberID, normalized.LineDigests); err != nil {
			s.failOperation(&op, err.Error())
			return needs(err.Error(), true)
		}
		if err := s.advanceOperation(&op, PhaseCompleted); err != nil {
			return needs(err.Error(), true)
		}
		op.CleanupState = CleanupStateCleaned
		_ = s.saveOperation(&op)
	}
	return "recovered", nil
}

// committedLinksPresent 核验「已提交关联」的操作产物仍在（不能把跳过重放当成
// 产物一定还在）。
func (s *Store) committedLinksPresent(op *Operation) error {
	store, _, err := s.loadLinkStore()
	if err != nil {
		return err
	}
	g := findGroupR(store, op.GroupID)
	if g == nil {
		return errors.New("关联组不存在（可能被其它操作删除），已停止恢复")
	}
	src := findMember(g, op.Source.UID, op.Source.SessionID)
	tgt := findMember(g, op.Target.UID, op.Target.SessionID)
	if src == nil || tgt == nil {
		return errors.New("关联记录缺失（可能被其它操作修改），已停止恢复")
	}
	return nil
}

// syncOperationMembers 从登记组里解析操作双方的成员 id（按 uid + 会话 id）。
func (s *Store) syncOperationMembers(op *Operation) (string, string, error) {
	store, _, err := s.loadLinkStore()
	if err != nil {
		return "", "", err
	}
	g := findGroupR(store, op.GroupID)
	if g == nil {
		return "", "", errors.New("关联组不存在，已停止恢复")
	}
	src := findMember(g, op.Source.UID, op.Source.SessionID)
	tgt := findMember(g, op.Target.UID, op.Target.SessionID)
	if src == nil || tgt == nil {
		return "", "", errors.New("关联记录缺失（可能被其它操作修改），已停止恢复")
	}
	return src.MemberID, tgt.MemberID, nil
}

// sessionRowOwner 读会话行的归属账号；行不存在返回空串。
func (s *Store) sessionRowOwner(lib *library, sessionID string) (string, error) {
	if lib == nil || !libExists(lib) {
		return "", errors.New("会话库不存在")
	}
	db, err := s.openLib(lib, true)
	if err != nil {
		return "", err
	}
	defer db.Close()
	row, err := readSessionRow(db, sessionID)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", nil
	}
	return str(row["user_id"]), nil
}

// verifySessionRowOwner 写后校验：目标行必须存在且归属正确。
func (s *Store) verifySessionRowOwner(lib *library, sessionID, uid string) error {
	owner, err := s.sessionRowOwner(lib, sessionID)
	if err != nil {
		return fmt.Errorf("写后无法回读新会话：%w", err)
	}
	if owner != uid {
		return fmt.Errorf("写后校验失败：新会话归属为 %q，期望 %q", owner, uid)
	}
	return nil
}

// hasPendingRecoveryBlocking 报告是否存在会阻断「重开客户端」的未完成操作。
//
// 返回 (是否阻断, 说明)：未完成操作本身不算问题（下次维护会补完），但**恢复不了**
// 的（中间产物被改动）必须先让人处理，否则重开的客户端会继续写一份不一致的数据。
func (s *Store) hasPendingRecoveryBlocking(variant string) (bool, string) {
	report := s.RecoverPendingOperations(variant)
	if report.Clean() && len(s.pendingOperations(variant)) == 0 {
		return false, ""
	}
	var parts []string
	for _, issue := range report.NeedsRecovery {
		parts = append(parts, issue.Reason)
	}
	if len(parts) == 0 {
		parts = append(parts, "仍有待恢复的会话写入")
	}
	return true, strings.Join(parts, "；")
}

// operationTimeString 便于日志与报告展示（本地时间）。
func operationTimeString(ms int64) string {
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

// PendingRecoverySummary 是未完成操作的只读汇总（面板据此提示「有会话写入待恢复」）。
type PendingRecoverySummary struct {
	// Count 是未完成操作条数（两个档位合计）。
	Count int `json:"count"`
	// Variants 是有未完成操作的档位（cn/intl）。
	Variants []string `json:"variants"`
	// Unparseable 表示存在无法解析/无法读取的操作记录：这类现场必须人工处理，
	// 且会阻断后续复制（避免绕过 pending 去重写出第二个副本）。
	Unparseable bool `json:"unparseable"`
	// Details 是逐条可读说明。
	Details []string `json:"details"`
}

// PendingRecovery 只读扫描未完成操作（不做任何写入）。
func (s *Store) PendingRecovery() PendingRecoverySummary {
	out := PendingRecoverySummary{Variants: []string{}, Details: []string{}}
	scan := s.scanOperations()
	if !scan.Complete {
		out.Unparseable = true
		out.Details = append(out.Details, UnparseableOperationReason+"（操作记录目录无法读取）")
	}
	for _, problem := range scan.Problems {
		out.Unparseable = true
		out.Details = append(out.Details, UnparseableOperationReason+"（"+problem+"）")
	}
	for _, variant := range []string{"cn", "intl"} {
		ops := s.pendingOperations(variant)
		if len(ops) == 0 {
			continue
		}
		out.Count += len(ops)
		out.Variants = append(out.Variants, variant)
		for _, op := range ops {
			what := "复制"
			if op.Kind == OperationKindSync {
				what = "同步"
			}
			detail := fmt.Sprintf("%s %s 的%s操作（%s）尚未完成", localappsSiteLabelOf(variant), op.Target.SessionID, what, string(op.Phase))
			if op.LastError != "" {
				detail += "：" + op.LastError
			}
			out.Details = append(out.Details, detail)
		}
	}
	return out
}

// RecoverVariants 恢复指定档位的未完成操作（对照 recover_pending_session_operations）。
//
// 调用方必须保证该档位客户端**没有在运行**（会话写入会被它的退出回写覆盖）；
// 运行中时直接拒绝，不「假装恢复」。
func (s *Store) RecoverVariants(variants []string) (map[string]*RecoveryReport, error) {
	out := map[string]*RecoveryReport{}
	for _, variant := range variants {
		if variant != "cn" && variant != "intl" {
			continue
		}
		if localappsIsWorkBuddyRunning(variant) {
			return out, fmt.Errorf("%s正在运行，请先退出客户端再恢复会话写入", localappsSiteLabelOf(variant))
		}
		out[variant] = s.RecoverPendingOperations(variant)
	}
	return out, nil
}

// localappsSiteLabelOf 是本包对 localapps.WorkBuddySiteLabel 的小包装。
func localappsSiteLabelOf(site string) string {
	return localapps.WorkBuddySiteLabel(site)
}

// localappsIsWorkBuddyRunning 是本包对门禁探针的小包装。
func localappsIsWorkBuddyRunning(site string) bool {
	return clientRunning(site)
}
