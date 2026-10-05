package server

// -----------------------------------------------------------------------------
// 扩展数据仓会话（VS Code 内 CodeBuddy 插件 / CodeBuddy IDE）的 API
//
// 与 WorkBuddy 侧同一套语义（复制 = 复制 + 登记、预览签发凭据、执行前复核），
// 差别只在会话形态与生命周期守卫：
//   - 写入前编辑器必须完全退出（运行中写入会被它的退出回写覆盖）；
//   - 会话按「工作区 hash → 会话」组织，复制项是 (workspace_hash, conversation_id)。
//
// **与 switch 的一处已知差异**：源账号由前端显式指定，而不是从扩展的登录态推导 ——
// 读扩展登录态要解 VS Code 的 Safe Storage 密文，本项目没有实现那条读取路径。
// 用户在对话框里选来源账号，行为等价、可见性更好（不必依赖「当前登录」的隐式假设）。
// -----------------------------------------------------------------------------

import (
	"errors"
	"net/http"
	"strings"

	"workbuddy-gateway/internal/extsessions"
)

// extStore 按客户端标识构造句柄（未知标识返回 nil）。
func (s *Server) extStore(client string) *extsessions.Store {
	var spec extsessions.Spec
	switch strings.TrimSpace(client) {
	case "vscode":
		spec = extsessions.VSCodeStore
	case "codebuddy-ide", "ide":
		spec = extsessions.IDEStore
	default:
		return nil
	}
	return extsessions.NewStore(spec, s.config().WorkDir)
}

// extBackupRoot 是扩展会话备份根（`<工作目录>/backups/<kind>/<utc>`）。
func extBackupRoot(st *extsessions.Store) string {
	return st.BackupRoot
}

// handleExtSessions 列出某扩展数据仓的状态、可复制会话与关联组
// （GET /panel/api/local-sessions/ext?client=vscode&uid=<源账号>）。
func (s *Server) handleExtSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	client := r.URL.Query().Get("client")
	st := s.extStore(client)
	if st == nil {
		writeJSON(w, http.StatusBadRequest, errBody("未知的会话客户端（只支持 vscode / codebuddy-ide）"))
		return
	}
	if !st.Available() {
		writeJSON(w, http.StatusOK, map[string]any{
			"client": client, "available": false, "data_root": "",
			"running": false, "groups": []any{}, "sessions": []any{},
			"note": "未找到 " + st.Spec.Label + " 的数据目录（可能未安装或从未登录过）",
		})
		return
	}
	labels := map[string]string{}
	for uid, label := range s.sessionAccountLabels() {
		labels[uid] = label.Label
	}
	groups, err := st.ListGroups(labels)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"client": client, "available": true, "data_root": st.Root,
			"running": extsessions.IsEditorRunning(st.Spec),
			"groups":  []any{}, "sessions": []any{},
			"store_status": "unavailable", "store_error": err.Error(),
		})
		return
	}
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	sessions := extsessions.ListSessions(st.Spec, st.Root, uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"client": client, "available": true, "data_root": st.Root,
		"running": extsessions.IsEditorRunning(st.Spec),
		"groups":  groups, "sessions": sessions.Sessions, "skipped": sessions.Skipped,
		"store_status": "ok", "note": "",
	})
}

// handleExtSessionsCopy 复制并登记（POST /panel/api/local-sessions/ext/copy）。
//
// body: {client, source_uid, target_uid, items:[{workspace_hash, conversation_id}], restart}
func (s *Server) handleExtSessionsCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Client    string `json:"client"`
		SourceUID string `json:"source_uid"`
		TargetUID string `json:"target_uid"`
		Restart   bool   `json:"restart"`
		Items     []struct {
			WorkspaceHash  string `json:"workspace_hash"`
			ConversationID string `json:"conversation_id"`
		} `json:"items"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	st := s.extStore(req.Client)
	if st == nil {
		writeJSON(w, http.StatusBadRequest, errBody("未知的会话客户端（只支持 vscode / codebuddy-ide）"))
		return
	}
	items := make([]extsessions.CopyItem, 0, len(req.Items))
	for _, item := range req.Items {
		items = append(items, extsessions.CopyItem{
			WorkspaceHash: item.WorkspaceHash, ConversationID: item.ConversationID,
		})
	}
	items = extsessions.NormalizeItems(items)
	if len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody("没有可复制的会话（工作区或会话 id 非法）"))
		return
	}
	report, linkErrors, notes, err := st.CopyWithGuard(
		extsessions.VariantForSpec(st.Spec), req.SourceUID, req.TargetUID, items, req.Restart)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"code": 502, "message": err.Error()},
			"notes": notes,
		})
		return
	}
	if n := len(report.Copied); n > 0 {
		s.logf("[会话] %s 复制完成：%d 个会话（源 %s → 目标 %s）", st.Spec.Label, n, req.SourceUID, req.TargetUID)
	}
	payload := map[string]any{"ok": true, "report": report, "notes": notes}
	if len(linkErrors) > 0 {
		payload["link_errors"] = linkErrors
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleExtSessionGroupPreviewPair 预览一对成员（POST /ext/groups/{id}/preview-pair）。
func (s *Server) handleExtSessionGroupPreviewPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Client         string `json:"client"`
		SourceMemberID string `json:"source_member_id"`
		TargetMemberID string `json:"target_member_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	st := s.extStore(req.Client)
	if st == nil {
		writeJSON(w, http.StatusBadRequest, errBody("未知的会话客户端"))
		return
	}
	groupID := strings.TrimSpace(r.PathValue("id"))
	preview, err := st.PreviewPair(groupID, req.SourceMemberID, req.TargetMemberID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// handleExtSessionGroupUnify 整组统一（POST /ext/groups/{id}/unify）。
//
// body: {client, source_member_id, targets:{<memberId>: <mode>}, restart}
func (s *Server) handleExtSessionGroupUnify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Client         string            `json:"client"`
		SourceMemberID string            `json:"source_member_id"`
		Targets        map[string]string `json:"targets"`
		Restart        bool              `json:"restart"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	st := s.extStore(req.Client)
	if st == nil {
		writeJSON(w, http.StatusBadRequest, errBody("未知的会话客户端"))
		return
	}
	groupID := strings.TrimSpace(r.PathValue("id"))
	outcomes, notes, err := st.SyncWithGuard(groupID, req.SourceMemberID, req.Targets, req.Restart)
	if err != nil {
		if errors.Is(err, extsessions.ErrGroupMissing) {
			writeJSON(w, http.StatusNotFound, errBody(err.Error()))
			return
		}
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	synced := 0
	for _, out := range outcomes {
		if out.Applied {
			synced++
		}
	}
	if synced > 0 {
		s.logf("[会话] %s 组 %s 统一完成：%d 项写入", st.Spec.Label, groupID, synced)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "outcomes": outcomes, "notes": notes, "synced": synced,
	})
}
