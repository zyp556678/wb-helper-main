package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"

	"workbuddy-gateway/internal/localsessions"
)

// -----------------------------------------------------------------------------
// 本机会话库（切片 15）
//
// WorkBuddy 客户端的会话存在本机：
//
//	~/.workbuddy/workbuddy.db                SQLite（sessions / session_usage / workspaces）
//	~/.workbuddy/projects/<工作区>/<id>.jsonl  正文
//
// 本组接口提供「看会话」与「把某条会话复制给另一个账号」。
//
// **只做新增，不改不删**：删用户的历史是不可逆的，本工具没有理由碰它。
// -----------------------------------------------------------------------------

// sessionsStore 取当前用户的会话库句柄（取不到 home 时返回 nil）。
//
// StateDir 指向**网关自己的工作目录**：同步基线存在那里，
// 不写进 `~/.workbuddy/`（那是客户端的目录，不该多出我们的文件）。
func (s *Server) sessionsStore() *localsessions.Store {
	home := userHome()
	if home == "" {
		return nil
	}
	st := localsessions.NewStore(home)
	st.StateDir = s.config().WorkDir
	return st
}

// handleLocalSessions 列出会话（GET /panel/api/local-sessions?uid=&limit=）。
//
// 同时把账号池里的账号与该库中出现的 user_id 对应起来，
// 界面才能显示「这条会话属于池里的哪个账号」而不是一串裸 uuid。
func (s *Server) handleLocalSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}
	if !st.Exists() {
		// 未安装/未使用过客户端 —— 这是正常状态，不是错误。
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"sessions":  []localsessions.Session{},
			"user_ids":  []string{},
			"note":      "未找到本机会话库（" + st.DBPath + "），可能未安装或未使用过 WorkBuddy 客户端",
		})
		return
	}

	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	list, err := st.ListSessions(uid, 200)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody("读取会话库失败: "+err.Error()))
		return
	}
	uids, err := st.ListUserIDs()
	if err != nil {
		uids = []string{}
	}

	// 把 user_id 映射到账号池里的账号（界面据此显示账号名而不是裸 uuid）。
	type ownerInfo struct {
		Account string `json:"account"`
		Label   string `json:"label"`
		Known   bool   `json:"known"`
	}
	owners := map[string]ownerInfo{}
	for _, u := range uids {
		info := ownerInfo{}
		for _, a := range s.pool.Accounts() {
			if a.Cred.UID == u {
				info.Account = a.Cred.AccountID()
				info.Label = a.Cred.Nickname
				if strings.TrimSpace(info.Label) == "" {
					info.Label = u
				}
				info.Known = true
				break
			}
		}
		if !info.Known {
			info.Label = u
		}
		owners[u] = info
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"available": true,
		"db_path":   st.DBPath,
		"sessions":  list,
		"user_ids":  uids,
		"owners":    owners,
	})
}

// handleLocalSessionsCopy 把一条会话复制给某个账号
// （POST /panel/api/local-sessions/copy，body {session_id, account_id, dry_run}）。
func (s *Server) handleLocalSessionsCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		// AccountID 是账号池里的账号（凭据文件名），用它换出 user_id。
		AccountID string `json:"account_id"`
		// TargetUID 允许直接指定 user_id（账号池里没有那个账号时用）。
		TargetUID string `json:"target_uid"`
		DryRun    bool   `json:"dry_run"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少 session_id"))
		return
	}

	// 目标账号：优先用 account_id 换 uid（这样能顺带校验账号存在）。
	targetUID := strings.TrimSpace(req.TargetUID)
	// TargetVariant 决定副本写进哪个库（国际站账号 → ~/.workbuddy-ai）。
	// **必须传**：写错库的副本目标客户端看不见，面板却显示「已复制」。
	targetVariant := ""
	if strings.TrimSpace(req.AccountID) != "" {
		acc, err := s.pool.Find(strings.TrimSpace(req.AccountID))
		if err != nil {
			writeJSON(w, http.StatusNotFound, errBody("账号不存在: "+req.AccountID))
			return
		}
		targetUID = acc.Cred.UID
		targetVariant = acc.Site()
	}
	if targetUID == "" {
		writeJSON(w, http.StatusBadRequest, errBody(
			"必须指定目标账号（account_id 或 target_uid）。会话按账号隔离，没有目标账号就没有归属"))
		return
	}
	if targetVariant == "" {
		// 只给了 uid：按账号池反查档位；查不到就留空（CopySession 退回「与源同库」）。
		if lbl, ok := s.sessionAccountLabels()[targetUID]; ok {
			targetVariant = lbl.Site
		}
	}

	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}

	res, err := st.CopySession(strings.TrimSpace(req.SessionID), localsessions.CopyOptions{
		TargetUID:     targetUID,
		TargetVariant: targetVariant,
		DryRun:        req.DryRun,
	})
	if err != nil {
		// 失败时把已经做到的步骤（通常是「已备份」）一并回传。
		payload := map[string]any{"error": map[string]any{"code": 502, "message": err.Error()}}
		if res != nil {
			payload["notes"] = res.Notes
			payload["backup_id"] = res.BackupID
		}
		writeJSON(w, http.StatusBadGateway, payload)
		return
	}

	s.logf("[会话] 已把 %s 复制给账号 %s → 新会话 %s（重写 %d 行）",
		res.SourceID, targetUID, res.NewID, res.BodyLines)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"new_id":     res.NewID,
		"body_path":  res.BodyPath,
		"body_lines": res.BodyLines,
		"backup_id":  res.BackupID,
		"notes":      res.Notes,
	})
}

// handleLocalSessionsSync 把来源会话的内容同步到目标会话
// （POST /panel/api/local-sessions/sync，body {source_id, target_id, force}）。
//
// 判定语义见 localsessions.decideSync：
//   - identical  → 无需动作（200，applied=false）
//   - fast_forward → 目标无独有改动，可直接追加（**零覆盖**）
//   - ahead      → 只有目标新增，不该往回同步（200，applied=false）
//   - diverge    → 两边都改了，必须 force 才执行（覆盖会替换目标完整内容）
//   - unknown    → 内容不可读或缺基线，拒绝
func (s *Server) handleLocalSessionsSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		SourceID string `json:"source_id"`
		TargetID string `json:"target_id"`
		// Force 允许覆盖分叉的目标（会替换目标完整内容）。
		Force bool `json:"force"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	if strings.TrimSpace(req.SourceID) == "" || strings.TrimSpace(req.TargetID) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("必须同时指定 source_id 与 target_id"))
		return
	}

	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}

	res, err := st.SyncBySessionIDs(strings.TrimSpace(req.SourceID), strings.TrimSpace(req.TargetID), req.Force)
	if err != nil {
		payload := map[string]any{"error": map[string]any{"code": 502, "message": err.Error()}}
		if res != nil {
			payload["notes"] = res.Notes
		}
		writeJSON(w, http.StatusBadGateway, payload)
		return
	}

	if res.OK {
		s.logf("[会话] 已同步 %s → %s", req.SourceID, req.TargetID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"applied": res.OK,
		"notes":   res.Notes,
	})
}

// handleLocalSessionGroups 把会话按「同源副本」聚成组（GET /panel/api/local-sessions/groups）。
//
// 分组的依据是**内容本身**（归一化后的有序行摘要），不是登记表 ——
// 这样用户手动复制出来的副本也能进组。状态判定（一致 / 待同步 / 有分叉）
// 与 decideSync 同一套口径。
//
// 会话库读不出来时不再返回 502，而是 200 + `store_status=unavailable` +
// `store_error`：界面据此显示琥珀横幅与「重试」，而不是把整页打成错误态。
func (s *Server) handleLocalSessionGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available":    false,
			"groups":       []localsessions.RegistryGroupView{},
			"counts":       localsessions.GroupCounts{},
			"store_status": "unavailable",
			"store_error":  "无法确定用户主目录",
			"note":         "无法确定用户主目录，找不到本机会话库",
		})
		return
	}
	if !st.Exists() {
		// 未安装/未使用过客户端 —— 这是正常状态，不是错误。
		// store_error 留空：空态文案已经解释了原因，不需要再叠一条琥珀横幅。
		writeJSON(w, http.StatusOK, map[string]any{
			"available":    false,
			"groups":       []localsessions.RegistryGroupView{},
			"counts":       localsessions.GroupCounts{},
			"store_status": "unavailable",
			"store_error":  "",
			"note":         "未找到本机会话库（" + st.DBPath + "），可能未安装或未使用过 WorkBuddy 客户端",
		})
		return
	}

	if err := st.BootstrapRegistryIfNeeded(s.sessionAccountLabels()); err != nil {
		s.logf("[会话] 登记表初始化失败: %v", err)
	}
	groups, err := st.ListRegistryGroups(s.sessionAccountLabels())
	if err != nil {
		s.logf("[会话] 读取关联记录失败: %v", err)
		writeJSON(w, http.StatusOK, map[string]any{
			"available":    true,
			"db_path":      st.DBPath,
			"groups":       []localsessions.RegistryGroupView{},
			"counts":       localsessions.GroupCounts{},
			"store_status": "unavailable",
			"store_error":  "读取关联记录失败: " + err.Error(),
			"note":         "",
		})
		return
	}
	counts := localsessions.GroupCounts{All: len(groups)}
	for _, g := range groups {
		switch g.Status {
		case localsessions.GroupStatusBehind:
			counts.Behind++
		case localsessions.GroupStatusDiverge:
			counts.Diverge++
		case localsessions.GroupStatusLatest:
			counts.Latest++
		case localsessions.GroupStatusMissing:
			counts.Missing++
		case localsessions.GroupStatusUnknown:
			counts.Unknown++
		}
	}
	// 未完成操作只做**只读**汇总：真正执行恢复在复制/同步（已过写入门禁）或
	// /recover 端点（会先确认客户端没在运行）里做。
	writeJSON(w, http.StatusOK, map[string]any{
		"available":        true,
		"db_path":          st.DBPath,
		"groups":           groups,
		"counts":           counts,
		"store_status":     "ok",
		"store_error":      "",
		"note":             "",
		"pending_recovery": st.PendingRecovery(),
	})
}

// handleLocalSessionsRecover 恢复未完成的会话写入
// （POST /panel/api/local-sessions/recover，body {variants?: ["cn","intl"]}）。
//
// **必须客户端已退出**：会话写入会被运行中客户端的退出回写覆盖，所以运行中直接
// 拒绝并如实说明，而不是「假装恢复」。不传 variants 时恢复全部有未完成操作的档位。
func (s *Server) handleLocalSessionsRecover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Variants []string `json:"variants"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}
	variants := req.Variants
	if len(variants) == 0 {
		variants = st.PendingRecovery().Variants
	}
	if len(variants) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "reports": map[string]any{},
			"note": "没有待恢复的会话写入",
		})
		return
	}
	reports, err := st.RecoverVariants(variants)
	payload := map[string]any{"ok": err == nil, "reports": reports}
	if err != nil {
		payload["error"] = err.Error()
	}
	for variant, report := range reports {
		if len(report.Recovered) > 0 || len(report.Abandoned) > 0 {
			s.logf("[会话] %s 恢复完成：补齐 %d 项，放弃 %d 项", variant, len(report.Recovered), len(report.Abandoned))
		}
		if len(report.NeedsRecovery) > 0 {
			s.logf("[会话] %s 恢复发现 %d 项需要人工处理", variant, len(report.NeedsRecovery))
		}
	}
	if err != nil {
		writeJSON(w, http.StatusConflict, payload)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// sessionAccountLabels 把账号池的 uid 映射成账号标签（界面显示昵称而不是裸 uuid）。
func (s *Server) sessionAccountLabels() map[string]localsessions.AccountLabel {
	labels := map[string]localsessions.AccountLabel{}
	for _, a := range s.pool.Accounts() {
		uid := a.Cred.UID
		if uid == "" {
			continue
		}
		label := a.Cred.Nickname
		if strings.TrimSpace(label) == "" {
			label = uid
		}
		labels[uid] = localsessions.AccountLabel{Account: a.Cred.AccountID(), Label: label, Site: a.Site()}
	}
	return labels
}

// sessionAddTarget 是「关联新账号」可选的目标账号。
type sessionAddTarget struct {
	// ID 是账号在池里的标识（凭据文件名）。
	ID string `json:"id"`
	// UID 是复制时的会话归属。
	UID   string `json:"uid"`
	Label string `json:"label"`
	Site  string `json:"site"`
}

// sessionAddTargets 列出可以「复制并关联」进该组的账号：账号池里还没有该组副本的账号。
func (s *Server) sessionAddTargets(g *localsessions.RegistryGroupView) []sessionAddTarget {
	inGroup := map[string]bool{}
	for _, m := range g.Members {
		if m.UID != "" {
			inGroup[m.UID] = true
		}
	}
	out := make([]sessionAddTarget, 0)
	for _, a := range s.pool.Accounts() {
		uid := strings.TrimSpace(a.Cred.UID)
		if uid == "" || inGroup[uid] {
			continue
		}
		label := strings.TrimSpace(a.Cred.Nickname)
		if label == "" {
			label = uid
		}
		out = append(out, sessionAddTarget{ID: a.Cred.AccountID(), UID: uid, Label: label, Site: a.Cred.Site()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// handleLocalSessionGroup 取单个关联组（GET /panel/api/local-sessions/groups/{id}）。
//
// 详情弹窗的「重新检查」与同步后的刷新都走它；未命中返回 404。
// 响应 = 组本身 + 可关联账号列表（add_targets）。
func (s *Server) handleLocalSessionGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	groupID := strings.TrimSpace(r.PathValue("id"))
	if groupID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少会话组 id"))
		return
	}
	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}
	g := st.GetRegistryGroup(groupID, s.sessionAccountLabels())
	if g == nil {
		writeJSON(w, http.StatusNotFound, errBody("会话组不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group":       g,
		"add_targets": s.sessionAddTargetsRegistry(g),
	})
}

// sessionAddTargetsRegistry 是「关联新账号」的候选：账号池里**尚未在该组登记**
// 的账号（active 成员占用的 uid 排除；stale/superseded 的账号允许重新关联）。
func (s *Server) sessionAddTargetsRegistry(g *localsessions.RegistryGroupView) []sessionAddTarget {
	used := map[string]bool{}
	for _, m := range g.Members {
		if m.State == localsessions.MemberStateActive {
			used[m.UID] = true
		}
	}
	out := []sessionAddTarget{}
	for _, a := range s.pool.Accounts() {
		if a.IsDisabled() || used[a.Cred.UID] {
			continue
		}
		out = append(out, sessionAddTarget{
			ID: a.Cred.AccountID(), UID: a.Cred.UID,
			Label: a.Cred.Nickname, Site: a.Site(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// handleLocalSessionGroupUnify 组级统一
// （POST /panel/api/local-sessions/groups/{id}/unify，
//
//	body {source_member_id, targets: [{member_id, mode, preview_token}]}）。
//
// 每个目标的写入模式与**预览凭据**由前端在预览后带回（fastForward/overwrite/
// unifyOverwrite）；执行前用同一套算法重算绑定并逐字段复核，任何版本变化都进
// skipped（reason_code=previewStale），判定不允许该模式则报错 —— 预览过期/内容变化
// 绝不会被静默执行。同组写操作串行化：批量执行期间重复点击拿到 409。
//
// 生命周期窗口：校验全部通过后关闭「运行中且当前登录是写入目标」的客户端，
// 写完全部目标再重新打开（恢复不了时暂停重开并如实报告）。
func (s *Server) handleLocalSessionGroupSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	groupID := strings.TrimSpace(r.PathValue("id"))
	if groupID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少会话组 id"))
		return
	}
	var req struct {
		SourceMemberID string `json:"source_member_id"`
		// Targets 是每个目标成员的写入模式与预览凭据（预览后由前端带回）。
		Targets []struct {
			MemberID string `json:"member_id"`
			Mode     string `json:"mode"`
			// PreviewToken 是预览接口签发的凭据 id：执行时必须原样带回。
			PreviewToken string `json:"preview_token"`
		} `json:"targets"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}
	release, ok := s.groupLocks.tryLock(groupID)
	if !ok {
		writeJSON(w, http.StatusConflict, errBody("该会话组正在同步中，请稍候再试"))
		return
	}
	defer release()

	targets := map[string]localsessions.UnifyTarget{}
	for _, t := range req.Targets {
		targets[t.MemberID] = localsessions.UnifyTarget{Mode: t.Mode, PreviewToken: t.PreviewToken}
	}
	report, err := st.UnifyGroup(groupID, req.SourceMemberID, targets)
	if err != nil {
		switch {
		case errors.Is(err, localsessions.ErrGroupNotFound):
			writeJSON(w, http.StatusNotFound, errBody(err.Error()))
		case errors.Is(err, localsessions.ErrSourceMemberNotFound):
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		default:
			writeJSON(w, http.StatusBadGateway, errBody("组同步失败: "+err.Error()))
		}
		return
	}
	if n := len(report.Synced); n > 0 {
		s.logf("[会话] 组 %s 统一完成：%d 项写入（来源 %s）", groupID, n, report.SourceMemberID)
	}
	writeJSON(w, http.StatusOK, report)
}

// handleLocalSessionGroupSafeBatch 批量同步落后账号
// （POST /panel/api/local-sessions/groups/{id}/safe-batch）。
//
// 后端重算安全源并对每个落后副本**重新复核** fast-forward；复核不再通过的项
// 进 skipped（reason=重新检查后该副本不再属于安全快进范围）。
func (s *Server) handleLocalSessionGroupSafeBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	groupID := strings.TrimSpace(r.PathValue("id"))
	if groupID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少会话组 id"))
		return
	}
	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}
	release, ok := s.groupLocks.tryLock(groupID)
	if !ok {
		writeJSON(w, http.StatusConflict, errBody("该会话组正在同步中，请稍候再试"))
		return
	}
	defer release()

	report, err := st.SafeBatchSync(groupID, s.sessionAccountLabels())
	if err != nil {
		if errors.Is(err, localsessions.ErrGroupNotFound) {
			writeJSON(w, http.StatusNotFound, errBody(err.Error()))
			return
		}
		writeJSON(w, http.StatusBadGateway, errBody("批量同步失败: "+err.Error()))
		return
	}
	if n := len(report.Synced); n > 0 {
		s.logf("[会话] 组 %s 批量同步完成：%d 项写入", groupID, n)
	}
	writeJSON(w, http.StatusOK, report)
}

// handleLocalSessionGroupPreviewPair 预览一对成员的同步判定
// （POST /panel/api/local-sessions/groups/{id}/preview-pair，
//
//	body {source_member_id, target_member_id}）。
//
// 前端「以此为准」确认框据此列出每个目标的 verdict/可用模式/原因，
// 并把判定不允许的目标列为阻断项（对照 switch 的 preview 流程）。
func (s *Server) handleLocalSessionGroupPreviewPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	groupID := strings.TrimSpace(r.PathValue("id"))
	var req struct {
		SourceMemberID string `json:"source_member_id"`
		TargetMemberID string `json:"target_member_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}
	decision, err := st.PreviewPair(groupID, req.SourceMemberID, req.TargetMemberID)
	if err != nil {
		switch {
		case errors.Is(err, localsessions.ErrGroupNotFound),
			errors.Is(err, localsessions.ErrSourceMemberNotFound):
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		default:
			writeJSON(w, http.StatusBadGateway, errBody("预览失败: "+err.Error()))
		}
		return
	}
	writeJSON(w, http.StatusOK, decision)
}

// handleLocalSessionGroupAdd 关联新账号
// （POST /panel/api/local-sessions/groups/{id}/add，body {source_member_id, target_uid}）。
//
// 语义 = copy + link：复制出的副本内容与来源同源，下一次分组自动进组，
// 不需要额外的登记表（这是本项目与 wb-switch 数据模型最本质的区别）。
func (s *Server) handleLocalSessionGroupAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	groupID := strings.TrimSpace(r.PathValue("id"))
	if groupID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少会话组 id"))
		return
	}
	var req struct {
		SourceMemberID string `json:"source_member_id"`
		TargetUID      string `json:"target_uid"`
		// AccountID 是兼容写法：给账号池 id 时服务端换出 uid（与 /copy 一致）。
		AccountID string `json:"account_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	sourceMemberID := strings.TrimSpace(req.SourceMemberID)
	if sourceMemberID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少 source_member_id"))
		return
	}
	targetUID := strings.TrimSpace(req.TargetUID)
	targetVariant := ""
	if targetUID == "" && strings.TrimSpace(req.AccountID) != "" {
		acc, err := s.pool.Find(strings.TrimSpace(req.AccountID))
		if err != nil {
			writeJSON(w, http.StatusNotFound, errBody("账号不存在: "+req.AccountID))
			return
		}
		targetUID = acc.Cred.UID
		targetVariant = acc.Site()
	}
	if targetUID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("必须指定目标账号（target_uid 或 account_id）"))
		return
	}
	if targetVariant == "" {
		// 只给了 uid：按账号池反查档位（副本必须写进目标档位自己的库）。
		if lbl, ok := s.sessionAccountLabels()[targetUID]; ok {
			targetVariant = lbl.Site
		}
	}
	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}
	release, ok := s.groupLocks.tryLock(groupID)
	if !ok {
		writeJSON(w, http.StatusConflict, errBody("该会话组正在同步中，请稍候再试"))
		return
	}
	defer release()

	res, err := st.CopySession(sourceMemberID, localsessions.CopyOptions{
		TargetUID:     targetUID,
		TargetVariant: targetVariant,
		AccountID:     req.AccountID,
	})
	if err != nil {
		// alreadyLinked 是幂等结果（switch 同样按成功返回），不是失败。
		var linked *localsessions.AlreadyLinkedError
		if errors.As(err, &linked) {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "status": "alreadyLinked", "member_id": linked.MemberID,
			})
			return
		}
		switch {
		case errors.Is(err, localsessions.ErrGroupNotFound):
			writeJSON(w, http.StatusNotFound, errBody(err.Error()))
		default:
			writeJSON(w, http.StatusBadGateway, errBody("复制并关联失败: "+err.Error()))
		}
		return
	}
	s.logf("[会话] 组 %s 已复制成员 %s → 账号 %s（新会话 %s）",
		res.GroupID, sourceMemberID, targetUID, res.NewID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"status":    "linked",
		"new_id":    res.NewID,
		"group_id":  res.GroupID,
		"backup_id": res.BackupID,
		"notes":     res.Notes,
	})
}

// keyedLocks 是按 key 串行化的互斥集合（同 key 同时只允许一个执行者）。
//
// 组同步会连续写多份正文；前端在批量执行期间很容易被重复点击，
// 没有这层保护，两轮执行会并发写同一批文件（备份互相覆盖、回读校验互相干扰）。
// 不同组互不影响，所以按组 ID 分锁而不是一把全局大锁。
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// tryLock 尝试占住 key；失败返回 ok=false（调用方回 409，而不是排队等）。
// 返回的 release 必须调用（延迟解锁 + 引用计数清理）。
func (k *keyedLocks) tryLock(key string) (release func(), ok bool) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	entry := k.locks[key]
	if entry == nil {
		entry = &keyedLock{}
		k.locks[key] = entry
	}
	if !entry.mu.TryLock() {
		k.mu.Unlock()
		return nil, false
	}
	entry.refs++
	k.mu.Unlock()

	return func() {
		// 先摘引用再解锁：摘除期间 entry.mu 仍被持有，
		// 新来者不会「拿到新 entry 与旧持有者并发」。
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
		entry.mu.Unlock()
	}, true
}

// handleLocalSessionGroupUnlink 取消某个成员与组的关联
// （POST /panel/api/local-sessions/groups/{id}/unlink，body {member_id}）。
//
// **只记排除项，不碰会话内容**（对照 wb-switch 的 remove_member：
// 「账号里的会话内容不会被删除」）。本项目的组是按内容推导的，没有登记项可删，
// 所以「解除」= 记一条排除项，让这条会话不再参与分组（见 localsessions/unlink.go）。
func (s *Server) handleLocalSessionGroupUnlink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	groupID := strings.TrimSpace(r.PathValue("id"))
	if groupID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少会话组 id"))
		return
	}
	var req struct {
		MemberID string `json:"member_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}
	release, ok := s.groupLocks.tryLock(groupID)
	if !ok {
		writeJSON(w, http.StatusConflict, errBody("该会话组正在同步中，请稍候再试"))
		return
	}
	defer release()

	res, err := st.UnlinkMember(groupID, req.MemberID)
	if err != nil {
		switch {
		case errors.Is(err, localsessions.ErrGroupNotFound):
			writeJSON(w, http.StatusNotFound, errBody(err.Error()))
		case errors.Is(err, localsessions.ErrSourceMemberNotFound):
			writeJSON(w, http.StatusBadRequest, errBody("该成员不在这个会话组里（可能已被解除或内容已变化）"))
		default:
			writeJSON(w, http.StatusBadGateway, errBody("取消关联失败: "+err.Error()))
		}
		return
	}
	s.logf("[会话] 组 %s 已解除成员 %s 的关联（剩余 %d）", groupID, res.MemberID, res.Remaining)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"member_id":     res.MemberID,
		"group_removed": res.GroupRemoved,
		"remaining":     res.Remaining,
		"notes":         res.Notes,
	})
}

// handleLocalSessionGroupDelete 删除整个会话组（组内成员全部解除关联）
// （POST /panel/api/local-sessions/groups/{id}/delete）。
//
// 语义与 wb-switch 的 delete_group 一致：**只解除关联，不删会话内容**。
func (s *Server) handleLocalSessionGroupDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	groupID := strings.TrimSpace(r.PathValue("id"))
	if groupID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少会话组 id"))
		return
	}
	st := s.sessionsStore()
	if st == nil {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录"))
		return
	}
	release, ok := s.groupLocks.tryLock(groupID)
	if !ok {
		writeJSON(w, http.StatusConflict, errBody("该会话组正在同步中，请稍候再试"))
		return
	}
	defer release()

	n, err := st.DeleteGroup(groupID)
	if err != nil {
		if errors.Is(err, localsessions.ErrGroupNotFound) {
			writeJSON(w, http.StatusNotFound, errBody(err.Error()))
			return
		}
		writeJSON(w, http.StatusBadGateway, errBody("删除关联失败: "+err.Error()))
		return
	}
	s.logf("[会话] 组 %s 的关联已删除（解除 %d 个账号）", groupID, n)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"removed": n,
		"notes":   []string{"已解除关联", "账号里的会话内容不会被删除"},
	})
}
