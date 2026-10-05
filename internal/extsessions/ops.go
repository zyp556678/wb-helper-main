package extsessions

// -----------------------------------------------------------------------------
// 高层编排：复制 + 登记 + 同步（编辑器生命周期窗口）
//
// 顺序固定（对照 switch 的 switch_vscode_ext_with_copy / sync_unify_batch_vscode）：
//
//	校验 → 关闭编辑器（restart 时才关，关不掉即放弃）→ 复制 → 登记关联 →
//	执行勾选的同步 → 重开编辑器
//
// **校验全部通过再关编辑器**：为注定失败的请求打断用户是无谓的。
// 会话写入没有 WorkBuddy 侧的 needsRecovery 语义（没有操作日志），中断后靠「重跑收敛」，
// 覆盖模式靠整目录备份收场。
// -----------------------------------------------------------------------------

import (
	"errors"
	"path/filepath"
	"strings"

	"workbuddy-gateway/internal/localsessions"
)

// ErrGroupMissing 是「会话组不存在」的哨兵错误（HTTP 层据此给 404）。
var ErrGroupMissing = errors.New("会话组不存在")

// CopyAndRegister 复制给定会话并登记关联（不关编辑器 —— 调用方负责生命周期窗口）。
func (s *Store) CopyAndRegister(variant string, sourceUID, targetUID string, items []CopyItem) (*CopyReport, []LinkError, error) {
	if !s.Available() {
		return nil, nil, errf("未找到 %s 的数据目录，无法复制会话", s.Spec.Label)
	}
	if !IsSafeUID(sourceUID) {
		return nil, nil, errf("源账号 uid 非法，拒绝写入")
	}
	if !IsSafeUID(targetUID) {
		return nil, nil, errf("目标账号 uid 非法，拒绝写入")
	}
	options := VSCodeCopy
	if s.Spec.ClientDir == IDEStore.ClientDir {
		options = IDECopy
	}
	report, err := CopySessions(s.Spec, options, s.Root, s.BackupRoot, sourceUID, targetUID, items)
	if err != nil {
		return nil, nil, err
	}
	linkErrors := s.RegisterCopiedSessions(variant, report)
	return report, linkErrors, nil
}

// CopyWithGuard 是「带编辑器生命周期窗口」的复制：校验 → 关闭 → 复制登记 → 重开。
//
// restart=false 且编辑器在运行时**直接拒绝**（不「假装写入」）。
func (s *Store) CopyWithGuard(variant string, sourceUID, targetUID string, items []CopyItem, restart bool) (*CopyReport, []LinkError, []string, error) {
	if !s.Available() {
		return nil, nil, nil, errf("未找到 %s 的数据目录，无法复制会话", s.Spec.Label)
	}
	running := IsEditorRunning(s.Spec)
	if running && !restart {
		return nil, nil, nil, errf("检测到 %s 正在运行，请先完全退出后再复制会话（或勾选「重启编辑器」）", s.Spec.Label)
	}
	var closed *ClosedEditor
	if running {
		var err error
		closed, err = CloseEditor(s.Spec)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	notes := []string{}
	report, linkErrors, err := s.CopyAndRegister(variant, sourceUID, targetUID, items)
	if err != nil {
		if closed != nil {
			if reopenErr := LaunchEditor(closed); reopenErr != nil {
				notes = append(notes, reopenErr.Error())
			}
		}
		return nil, nil, notes, err
	}
	if closed != nil {
		if reopenErr := LaunchEditor(closed); reopenErr != nil {
			// 重开失败只降级为提示：会话已经写好了。
			notes = append(notes, "会话已复制，但编辑器重新打开失败："+reopenErr.Error())
		} else {
			notes = append(notes, "已重新打开 "+s.Spec.Label)
		}
	}
	return report, linkErrors, notes, nil
}

// SyncWithGuard 是「带编辑器生命周期窗口」的整组统一：校验 → 关闭 → 逐目标同步 → 重开。
func (s *Store) SyncWithGuard(groupID, sourceMemberID string, targets map[string]string, restart bool) ([]SyncOutcome, []string, error) {
	if !s.Available() {
		return nil, nil, errf("未找到 %s 的数据目录", s.Spec.Label)
	}
	// 校验阶段：凭据与模式先全部过一遍，避免为注定失败的请求打断用户。
	store := s.linkStore()
	file, _, err := store.LoadLinkStore()
	if err != nil {
		return nil, nil, err
	}
	group := localsessions.FindGroupByID(file, groupID)
	if group == nil {
		return nil, nil, ErrGroupMissing
	}
	type prepared struct{ memberID, mode, token string }
	var preparedTargets []prepared
	for _, m := range group.Members {
		mode, ok := targets[m.MemberID]
		if !ok {
			continue
		}
		// 服务端现算预览并签发凭据：执行时用同一套算法复核（不沿用任何旧结论）。
		preview, err := s.PreviewPair(groupID, sourceMemberID, m.MemberID)
		if err != nil {
			return nil, nil, err
		}
		preparedTargets = append(preparedTargets, prepared{memberID: m.MemberID, mode: mode, token: preview.PreviewToken})
	}
	running := IsEditorRunning(s.Spec)
	if running && !restart {
		return nil, nil, errf("检测到 %s 正在运行，请先完全退出后再同步会话", s.Spec.Label)
	}
	var closed *ClosedEditor
	if running {
		closed, err = CloseEditor(s.Spec)
		if err != nil {
			return nil, nil, err
		}
	}
	notes := []string{}
	outcomes := make([]SyncOutcome, 0, len(preparedTargets))
	for _, target := range preparedTargets {
		outcomes = append(outcomes, s.SyncMemberPair(groupID, sourceMemberID, target.memberID, target.mode, target.token))
	}
	if closed != nil {
		if reopenErr := LaunchEditor(closed); reopenErr != nil {
			notes = append(notes, "会话已同步，但编辑器重新打开失败："+reopenErr.Error())
		} else {
			notes = append(notes, "已重新打开 "+s.Spec.Label)
		}
	}
	return outcomes, notes, nil
}

// VariantForSpec 返回该数据仓在登记表里的档位标记。
//
// 插件侧的会话树与 WorkBuddy 档位无关（一个账号一条树），统一记 `cn`；
// CodeBuddy IDE 的国内版/国际版共用同一棵树，档位由调用方按登录态决定。
func VariantForSpec(spec Spec) string {
	if spec.ClientDir == IDEStore.ClientDir {
		return "cn"
	}
	return "cn"
}

// NormalizeItems 过滤掉非法的工作区/会话 id（前端传参的第一道闸）。
func NormalizeItems(items []CopyItem) []CopyItem {
	out := make([]CopyItem, 0, len(items))
	for _, item := range items {
		if !IsHex32(strings.TrimSpace(item.WorkspaceHash)) || !IsHex32(strings.TrimSpace(item.ConversationID)) {
			continue
		}
		out = append(out, CopyItem{
			WorkspaceHash:  strings.TrimSpace(item.WorkspaceHash),
			ConversationID: strings.TrimSpace(item.ConversationID),
		})
	}
	return out
}

// ConversationDirFor 返回某账号某会话的目录（供上层做存在性检查与展示）。
func (s *Store) ConversationDirFor(uid, workspaceHash, conversationID string) string {
	return filepath.Join(HistoryRoot(s.Spec, s.Root, uid), workspaceHash, conversationID)
}
