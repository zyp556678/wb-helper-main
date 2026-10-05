package extsessions

// -----------------------------------------------------------------------------
// 会话复制内核（对照 vscode_session.rs 的复制部分）
//
// 复制 = 整目录复制 + 合并目标工作区索引，两条路径由 id 策略决定：
//   - AlwaysNew（插件）：恒定取新会话 id，并按 RemapPlan 重写消息/请求 id；
//   - KeepUnlessConflict（IDE）：整目录按字节复制、沿用源 id，仅当目标工作区已存在
//     同 id（索引条目或磁盘目录）时重随机会话 id 并改写副本内的旧 id 引用。
//
// 单条原子性：先写 `<ws>/.tmp-<newId>\` 再 rename 成 `<ws>\<newId>\`；
// 索引合并用「读-改-写 + 原子写」，写前把将被改的索引备份到
// `<backup_root>/<workspaceHash>/`。
//
// 逐条隔离：单条失败只回退该条（删临时/成品目录 + 回退该工作区索引），
// 记入 errors[] 后继续处理其余条目，不整体中止。
// -----------------------------------------------------------------------------

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RemapPlan 是会话内 id 重映射计划。
type RemapPlan struct {
	// MessageIDs 是旧消息 id → 新消息 id。
	MessageIDs map[string]string
	// RequestIDs 是旧请求 id → 新请求 id。
	RequestIDs map[string]string
	// CombinedIDs 是递归精确匹配用的合并表（消息优先于请求）。
	CombinedIDs map[string]string
	// MessageTotal 是参与复制的消息数量（报告用）。
	MessageTotal int
}

// BuildRemapPlan 依据会话索引与磁盘 messages/ 目录建立 id 重映射表。
func BuildRemapPlan(sourceIndex map[string]any, sourceDir string) *RemapPlan {
	messageIDs := map[string]string{}
	requestIDs := map[string]string{}
	used := map[string]bool{}

	// 消息 id：索引 messages[]（权威引用）与磁盘 messages/*.json 文件名
	// （只认 32 位小写 hex：目录里其它 json 如调试文件既非消息 id，也不改名）。
	for _, raw := range jsonArray(sourceIndex["messages"]) {
		msg, _ := raw.(map[string]any)
		insertNewID(messageIDs, used, strings.TrimSpace(strOf(msg["id"])))
	}
	if entries, err := os.ReadDir(filepath.Join(sourceDir, "messages")); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if stem, ok := strings.CutSuffix(name, ".json"); ok && IsHex32(stem) {
				insertNewID(messageIDs, used, stem)
			}
		}
	}
	// 请求 id：来自索引 requests[]。
	for _, raw := range jsonArray(sourceIndex["requests"]) {
		req, _ := raw.(map[string]any)
		id := strings.TrimSpace(strOf(req["id"]))
		if id == "" {
			continue
		}
		if _, ok := requestIDs[id]; !ok {
			requestIDs[id] = uniqueHex32(used)
		}
	}
	return &RemapPlan{
		MessageIDs:   messageIDs,
		RequestIDs:   requestIDs,
		CombinedIDs:  mergeMessageFirst(messageIDs, requestIDs),
		MessageTotal: len(messageIDs),
	}
}

// mergeMessageFirst 合并两张表：messages 优先（同一个旧 id 同时在两张表时以消息表为准）。
func mergeMessageFirst(messageIDs, requestIDs map[string]string) map[string]string {
	combined := make(map[string]string, len(messageIDs)+len(requestIDs))
	for k, v := range requestIDs {
		combined[k] = v
	}
	for k, v := range messageIDs {
		combined[k] = v
	}
	return combined
}

// insertNewID 为一个旧 id 生成并登记新 id（空串或已登记则跳过）。
func insertNewID(m map[string]string, used map[string]bool, oldID string) {
	if oldID == "" {
		return
	}
	if _, ok := m[oldID]; ok {
		return
	}
	m[oldID] = uniqueHex32(used)
}

// WorkspaceState 是目标工作区在本次复制期间的状态：磁盘备份 + 内存工作副本。
type WorkspaceState struct {
	dir string
	// original 是进入本次操作前的 index.json 原始字节（不存在为 nil），用于回退。
	original []byte
	// current 是当前工作副本（已合并成功条目）。
	current map[string]any
	// backupDir 是索引备份落盘目录（`<backup_root>/<workspaceHash>`）。
	backupDir string
}

// loadWorkspaceState 加载目标工作区：读取既有索引、备份将被修改的索引、建立内存工作副本。
//
// **延迟创建**：此处不创建目标工作区目录，只有确有会话写入时才创建 —— 避免
// 「该工作区所有条目最终都失败」时留下空目录。
func loadWorkspaceState(dir, backupRoot, workspaceHash string) (*WorkspaceState, error) {
	original, _ := os.ReadFile(filepath.Join(dir, "index.json"))
	originalBak, _ := os.ReadFile(filepath.Join(dir, ".index_bak.json"))
	workspaceBackup := filepath.Join(backupRoot, workspaceHash)
	if original != nil || originalBak != nil {
		if err := os.MkdirAll(workspaceBackup, 0o700); err != nil {
			return nil, err
		}
		if original != nil {
			if err := os.WriteFile(filepath.Join(workspaceBackup, "index.json"), original, 0o600); err != nil {
				return nil, err
			}
		}
		if originalBak != nil {
			_ = os.WriteFile(filepath.Join(workspaceBackup, ".index_bak.json"), originalBak, 0o600)
		}
	}
	current := map[string]any{}
	if original != nil {
		if parsed, ok := parseJSONObject(original); ok {
			current = parsed
		}
	}
	return &WorkspaceState{dir: dir, original: original, current: current, backupDir: workspaceBackup}, nil
}

// persist 把当前工作副本写回磁盘（原子写；插件侧同时写 .index_bak.json）。
func (s *WorkspaceState) persist(writeBackup bool) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	text, err := json.Marshal(s.current)
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(s.dir, "index.json"), text); err != nil {
		return err
	}
	if writeBackup {
		if err := WriteFileAtomic(filepath.Join(s.dir, ".index_bak.json"), text); err != nil {
			return err
		}
	}
	return nil
}

// restore 把工作区索引回退到进入本次操作前的状态（尽力而为）。
func (s *WorkspaceState) restore() {
	if s.original != nil {
		_ = os.WriteFile(filepath.Join(s.dir, "index.json"), s.original, 0o600)
		if backup, err := os.ReadFile(filepath.Join(s.backupDir, ".index_bak.json")); err == nil {
			_ = os.WriteFile(filepath.Join(s.dir, ".index_bak.json"), backup, 0o600)
		}
		return
	}
	_ = os.Remove(filepath.Join(s.dir, "index.json"))
	_ = os.Remove(filepath.Join(s.dir, ".index_bak.json"))
}

// CopyReport 是一次复制的结果。
type CopyReport struct {
	SourceUID string           `json:"source_uid"`
	TargetUID string           `json:"target_uid"`
	Copied    []CopyResultItem `json:"copied"`
	Errors    []CopyItemError  `json:"errors,omitempty"`
	Backup    string           `json:"backup"`
}

// CopyItemError 是一条失败的复制项。
type CopyItemError struct {
	WorkspaceHash  string `json:"workspace_hash"`
	ConversationID string `json:"conversation_id"`
	Error          string `json:"error"`
}

// CopyResultItem 是一条成功的复制项（新 id 与消息数）。
type CopyResultItem struct {
	WorkspaceHash string `json:"workspace_hash"`
	OldID         string `json:"old_id"`
	NewID         string `json:"new_id"`
	Messages      int    `json:"messages"`
}

// CopySessions 把勾选的会话复制到目标账号（逐条隔离）。
func CopySessions(spec Spec, options CopyOptions, root, backupRoot, sourceUID, targetUID string, items []CopyItem) (*CopyReport, error) {
	if !IsSafeUID(sourceUID) {
		return nil, errf("源账号 uid 非法")
	}
	if !IsSafeUID(targetUID) {
		return nil, errf("目标账号 uid 非法")
	}
	if sourceUID == targetUID {
		return nil, errf("源账号与目标账号相同，无需复制会话")
	}
	sourceHistory := HistoryRoot(spec, root, sourceUID)
	targetHistory := HistoryRoot(spec, root, targetUID)

	// 先为每个目标工作区建立「磁盘备份 + 内存工作副本」，保证逐条隔离与可回退。
	states := map[string]*WorkspaceState{}
	for _, workspaceHash := range distinctWorkspaces(items) {
		state, err := loadWorkspaceState(filepath.Join(targetHistory, workspaceHash), backupRoot, workspaceHash)
		if err != nil {
			return nil, errf("备份目标工作区索引失败（%s）：%v", workspaceHash, err)
		}
		states[workspaceHash] = state
	}

	report := &CopyReport{SourceUID: sourceUID, TargetUID: targetUID, Copied: []CopyResultItem{}, Backup: backupRoot}
	for _, item := range items {
		workspaceHash := strings.TrimSpace(item.WorkspaceHash)
		conversationID := strings.TrimSpace(item.ConversationID)
		if !IsHex32(workspaceHash) || !IsHex32(conversationID) {
			report.Errors = append(report.Errors, CopyItemError{
				WorkspaceHash: item.WorkspaceHash, ConversationID: item.ConversationID,
				Error: "工作区或会话 id 非法",
			})
			continue
		}
		sourceDir := filepath.Join(sourceHistory, workspaceHash, conversationID)
		if st, err := os.Stat(sourceDir); err != nil || !st.IsDir() {
			report.Errors = append(report.Errors, CopyItemError{
				WorkspaceHash: workspaceHash, ConversationID: conversationID, Error: "源会话目录不存在",
			})
			continue
		}
		state, ok := states[workspaceHash]
		if !ok {
			report.Errors = append(report.Errors, CopyItemError{
				WorkspaceHash: workspaceHash, ConversationID: conversationID, Error: "工作区状态缺失",
			})
			continue
		}
		outcome, err := copyOneConversation(options, sourceDir, filepath.Join(targetHistory, workspaceHash), workspaceHash, conversationID, state)
		if err != nil {
			report.Errors = append(report.Errors, CopyItemError{
				WorkspaceHash: workspaceHash, ConversationID: conversationID, Error: err.Error(),
			})
			continue
		}
		report.Copied = append(report.Copied, CopyResultItem{
			WorkspaceHash: workspaceHash, OldID: conversationID, NewID: outcome.newID, Messages: outcome.messages,
		})
	}
	return report, nil
}

type copyOutcome struct {
	newID    string
	messages int
}

// copyOneConversation 复制单个会话：写临时目录 → 原子提交 → 合并目标工作区索引。失败时回退。
func copyOneConversation(options CopyOptions, sourceDir, targetWsDir, workspaceHash, conversationID string, state *WorkspaceState) (copyOutcome, error) {
	sourceIndex, ok := ReadJSON(filepath.Join(sourceDir, "index.json")).(map[string]any)
	if !ok {
		return copyOutcome{}, errf("源会话 index.json 缺失或损坏")
	}

	var plan *RemapPlan
	if options.IDPolicy == IDAlwaysNew {
		plan = BuildRemapPlan(sourceIndex, sourceDir)
	}
	newConversationID := ""
	switch {
	case plan != nil:
		newConversationID = GenHex32()
	case conversationExists(targetWsDir, state.current, conversationID):
		// 冲突判定把「索引条目」与「磁盘目录」都算上：既不覆盖目标既有会话，也不产生重复条目。
		used := usedConversationIDs(targetWsDir, state.current)
		newConversationID = uniqueHex32(used)
	default:
		newConversationID = conversationID
	}

	// 1) 先写临时目录，避免出现「半个会话」。
	tmpDir := filepath.Join(targetWsDir, ".tmp-"+newConversationID)
	RemoveDirAllIfExists(tmpDir)
	var err error
	if plan != nil {
		err = writeConversation(sourceDir, tmpDir, sourceIndex, plan)
	} else {
		err = copyConversationKeep(sourceDir, tmpDir, conversationID, newConversationID)
	}
	if err != nil {
		RemoveDirAllIfExists(tmpDir)
		return copyOutcome{}, errf("写入临时会话目录失败：%v", err)
	}

	// 2) 原子提交：rename tmp → <newId>。
	finalDir := filepath.Join(targetWsDir, newConversationID)
	RemoveDirAllIfExists(finalDir)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		RemoveDirAllIfExists(tmpDir)
		return copyOutcome{}, errf("提交会话目录失败：%v", err)
	}

	// 3) 合并目标工作区索引；失败则回退目录与索引（仅本条）。
	sourceWsIndex, _ := ReadJSON(filepath.Join(filepath.Dir(sourceDir), "index.json")).(map[string]any)
	entry := conversationEntry(findConversation(sourceWsIndex, conversationID), newConversationID)
	before := cloneObject(state.current)
	mergeWorkspaceIndex(state.current, entry, newConversationID)
	if err := state.persist(options.WriteWorkspaceIndexBackup); err != nil {
		state.current = before
		if state.persist(options.WriteWorkspaceIndexBackup) != nil {
			// 二次写入仍失败：磁盘一致性已受损，从备份整体恢复该工作区索引（最后手段）。
			state.restore()
		}
		RemoveDirAllIfExists(finalDir)
		return copyOutcome{}, errf("合并工作区索引失败：%v", err)
	}

	messages := 0
	if plan != nil {
		messages = plan.MessageTotal
	} else {
		messages = len(jsonArray(sourceIndex["messages"]))
	}
	_ = workspaceHash
	return copyOutcome{newID: newConversationID, messages: messages}, nil
}

// copyConversationKeep 是「沿用 id」路径的写入器：整目录按字节复制；仅当会话 id 变更
// （冲突重随机）时，改写副本内真正引用旧会话 id 的 JSON 文件，其余文件保持字节不变。
func copyConversationKeep(sourceDir, destDir, oldConversationID, newConversationID string) error {
	if err := CopyDirRecursive(sourceDir, destDir); err != nil {
		return err
	}
	if oldConversationID != newConversationID {
		rewriteConversationIDReferences(destDir, oldConversationID, newConversationID)
	}
	return nil
}

// conversationExists 判断目标工作区是否已存在该会话（索引里有条目，或磁盘上有同名目录）。
func conversationExists(targetWsDir string, index map[string]any, conversationID string) bool {
	if st, err := os.Stat(filepath.Join(targetWsDir, conversationID)); err == nil && st.IsDir() {
		return true
	}
	return findConversation(index, conversationID) != nil
}

// usedConversationIDs 是目标工作区已占用的 id 集合（索引条目 id + 磁盘目录名）。
func usedConversationIDs(targetWsDir string, index map[string]any) map[string]bool {
	used := map[string]bool{}
	for _, raw := range jsonArray(index["conversations"]) {
		entry, _ := raw.(map[string]any)
		if id := strings.TrimSpace(strOf(entry["id"])); id != "" {
			used[id] = true
		}
	}
	if entries, err := os.ReadDir(targetWsDir); err == nil {
		for _, entry := range entries {
			used[entry.Name()] = true
		}
	}
	return used
}

// rewriteConversationIDReferences 改写副本内「恰好等于旧会话 id」的引用。
//
// 只重写确有引用的 JSON 文件：未命中的文件保持字节不变（实测会话索引与消息文件
// 都不含自身会话 id 时即零改写）。
func rewriteConversationIDReferences(destDir, oldID, newID string) {
	mapping := map[string]string{oldID: newID}
	for _, path := range jsonFiles(destDir) {
		text, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var value any
		if json.Unmarshal(text, &value) != nil {
			continue
		}
		// 先判定「确有引用」再改写：改写会把文件重新序列化为紧凑格式，不该波及无关文件。
		if !containsExactID(value, oldID) {
			continue
		}
		if encoded, err := json.Marshal(remapJSONReferences(value, mapping)); err == nil {
			_ = os.WriteFile(path, encoded, 0o600)
		}
	}
}

// containsExactID 递归判断某个 JSON 值里是否存在「恰好等于目标 id」的字符串
// （含字符串化 JSON 内部）。
func containsExactID(value any, target string) bool {
	switch v := value.(type) {
	case string:
		if v == target {
			return true
		}
		var inner any
		if json.Unmarshal([]byte(v), &inner) == nil {
			if _, ok := inner.(map[string]any); ok {
				return containsExactID(inner, target)
			}
		}
		return false
	case []any:
		for _, item := range v {
			if containsExactID(item, target) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if containsExactID(item, target) {
				return true
			}
		}
	}
	return false
}

// jsonFiles 递归收集目录下的 .json 文件（会话索引、消息文件与其备份）。
func jsonFiles(dir string) []string {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			out = append(out, jsonFiles(path)...)
			continue
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// remapJSONReferences 与 remapMessageFile 同范围的引用改写：
// 对象各层递归精确匹配 + extra（兼容字符串化 JSON），不改动正文与其它无关字符串。
func remapJSONReferences(value any, mapping map[string]string) any {
	object, ok := value.(map[string]any)
	if !ok {
		out := value
		replaceExactIDs(out, mapping)
		return out
	}
	out := cloneObject(object)
	for k, v := range out {
		replaceExactIDs(v, mapping)
		out[k] = v
	}
	if extra, ok := out["extra"]; ok {
		out["extra"] = replaceIDsInExtra(extra, mapping)
	}
	return out
}

// writeConversation 在目标目录写全一个会话：重映射后的 index.json + messages/* +
// 其余文件/目录原样复制（覆盖同步复用同一个写入器，只是 id 来源换成确定性派生）。
func writeConversation(sourceDir, destDir string, sourceIndex map[string]any, plan *RemapPlan) error {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return err
	}
	// 1) 重映射后的会话索引。
	remapped, err := json.Marshal(remapSessionIndex(sourceIndex, plan))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(destDir, "index.json"), remapped, 0o600); err != nil {
		return err
	}

	// 2) messages/<oldId>.json → messages/<newId>.json，并重写 id / extra 内的 id 引用。
	sourceMessages := filepath.Join(sourceDir, "messages")
	if st, err := os.Stat(sourceMessages); err == nil && st.IsDir() {
		targetMessages := filepath.Join(destDir, "messages")
		if err := os.MkdirAll(targetMessages, 0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(sourceMessages)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(sourceMessages, entry.Name())
			name := entry.Name()
			stem, _ := strings.CutSuffix(name, ".json")
			// 只重写 32 位小写 hex 的消息文件；messages/ 下的其它 json 一律按字节原样复制。
			if !IsHex32(stem) {
				if err := copyFile(path, filepath.Join(targetMessages, name)); err != nil {
					return err
				}
				continue
			}
			newName := name
			if newID, ok := plan.MessageIDs[stem]; ok {
				newName = newID + ".json"
			}
			text, err := os.ReadFile(path)
			if err != nil {
				if err := copyFile(path, filepath.Join(targetMessages, newName)); err != nil {
					return err
				}
				continue
			}
			if err := os.WriteFile(filepath.Join(targetMessages, newName), []byte(remapMessageFile(string(text), stem, plan)), 0o600); err != nil {
				return err
			}
		}
	}

	// 2b) 会话级 .index_bak.json（若存在，结构与 index.json 同）：同样重映射。
	sourceBak := filepath.Join(sourceDir, ".index_bak.json")
	if st, err := os.Stat(sourceBak); err == nil && !st.IsDir() {
		if bak, ok := ReadJSON(sourceBak).(map[string]any); ok {
			encoded, err := json.Marshal(remapSessionIndex(bak, plan))
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(destDir, ".index_bak.json"), encoded, 0o600); err != nil {
				return err
			}
		} else if err := copyFile(sourceBak, filepath.Join(destDir, ".index_bak.json")); err != nil {
			return err
		}
	}

	// 3) 其余顶层文件 / 目录原样复制（跳过已单独处理的索引与 messages；附件保持原文件名）。
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "index.json" || name == ".index_bak.json" || name == "messages" {
			continue
		}
		path := filepath.Join(sourceDir, name)
		if entry.IsDir() {
			if err := CopyDirRecursive(path, filepath.Join(destDir, name)); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(path, filepath.Join(destDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// remapSessionIndex 重映射会话索引：messages[].id、requests[].id、requests[].messages[]。
func remapSessionIndex(sourceIndex map[string]any, plan *RemapPlan) map[string]any {
	out := cloneObject(sourceIndex)
	if messages, ok := out["messages"].([]any); ok {
		for _, raw := range messages {
			msg, _ := raw.(map[string]any)
			if msg == nil {
				continue
			}
			if id, ok := msg["id"].(string); ok {
				if newID, found := plan.MessageIDs[id]; found {
					msg["id"] = newID
				}
			}
		}
	}
	if requests, ok := out["requests"].([]any); ok {
		for _, raw := range requests {
			req, _ := raw.(map[string]any)
			if req == nil {
				continue
			}
			if id, ok := req["id"].(string); ok {
				if newID, found := plan.RequestIDs[id]; found {
					req["id"] = newID
				}
			}
			if messages, ok := req["messages"].([]any); ok {
				for i := range messages {
					if id, ok := messages[i].(string); ok {
						if newID, found := plan.MessageIDs[id]; found {
							messages[i] = newID
						}
					}
				}
			}
		}
	}
	return out
}

// remapMessageFile 重映射单条消息文件：id 与 extra 内的 id 引用（保留其余字段原样）。
func remapMessageFile(text, oldStem string, plan *RemapPlan) string {
	var value map[string]any
	if json.Unmarshal([]byte(text), &value) != nil {
		return text
	}
	// 文件已按映射表重命名，内部 id 同步为新 id。
	if newID, ok := plan.MessageIDs[oldStem]; ok {
		value["id"] = newID
	}
	if extra, ok := value["extra"]; ok {
		value["extra"] = replaceIDsInExtra(extra, plan.CombinedIDs)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return text
	}
	return string(encoded)
}

// mergeWorkspaceIndex 把新条目并入工作区索引，并保证 current 指向真实存在的会话。
//
// 实测（macOS，扩展 4.12）缺 current 的索引会被扩展判定为损坏 —— 改名为
// `index.json.corrupted.<ms>` 并重建，同时多出一条垃圾空会话。目标原本的 current
// 仍有效则保留，缺失或悬空时指向本次并入的会话。
func mergeWorkspaceIndex(index map[string]any, entry map[string]any, newID string) {
	conversations, ok := index["conversations"].([]any)
	if !ok {
		conversations = []any{}
	}
	conversations = append(conversations, entry)
	index["conversations"] = conversations

	current, _ := index["current"].(string)
	currentOK := false
	if current != "" {
		for _, raw := range conversations {
			item, _ := raw.(map[string]any)
			if strOf(item["id"]) == current {
				currentOK = true
				break
			}
		}
	}
	if !currentOK {
		index["current"] = newID
	}
}

// findConversation 从工作区索引中查找指定会话条目。
func findConversation(index map[string]any, conversationID string) map[string]any {
	for _, raw := range jsonArray(index["conversations"]) {
		entry, _ := raw.(map[string]any)
		if strOf(entry["id"]) == conversationID {
			return entry
		}
	}
	return nil
}

// conversationEntry 构造并入目标索引的会话条目：优先复用源条目（保留 name/type/时间等），
// 仅换 id。
func conversationEntry(sourceEntry map[string]any, newID string) map[string]any {
	if sourceEntry != nil {
		entry := cloneObject(sourceEntry)
		entry["id"] = newID
		return entry
	}
	return map[string]any{
		"id": newID, "type": "craft", "name": "",
		"createdAt": nil, "lastMessageAt": nil,
	}
}

// distinctWorkspaces 返回待复制项涉及的工作区集合（仅保留合法 32 位 hex，避免建出无关目录）。
func distinctWorkspaces(items []CopyItem) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range items {
		hash := strings.TrimSpace(item.WorkspaceHash)
		if !IsHex32(hash) || seen[hash] {
			continue
		}
		seen[hash] = true
		out = append(out, hash)
	}
	sort.Strings(out)
	return out
}

// cloneObject 浅拷贝一层对象（值本身仍是共享引用，调用方按需再拷）。
func cloneObject(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// parseJSONObject 解析成对象（失败返回 false）。
func parseJSONObject(data []byte) (map[string]any, bool) {
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return nil, false
	}
	return out, true
}

// DeriveMessageID 派生副本消息 id：
// `sha256("vscode-session-link:v1" + 目标 uid + 目标会话 id + 序号 + 源记录摘要 + salt)` 前 32 位。
//
// 同一目标会话、同一序号、同一源内容 → 同一个 id，因此重复执行只会覆盖同一批文件；
// 序号进公式是为了让「内容完全相同的重复消息」不互相撞 id。
func DeriveMessageID(targetUID, targetConversationID string, seqIndex int, sourceDigest string, salt uint32) string {
	return deriveID([]string{targetUID, targetConversationID, itoa(seqIndex), sourceDigest, itoa(int(salt))})
}

// DeriveRequestID 派生副本请求 id：与消息 id 同公式，用请求序号与请求摘要，
// 并加 `req` 段区分。
func DeriveRequestID(targetUID, targetConversationID string, requestIndex int, sourceDigest string, salt uint32) string {
	return deriveID([]string{targetUID, targetConversationID, "req", itoa(requestIndex), sourceDigest, itoa(int(salt))})
}

// deriveID 各段长度编码后拼接再取 SHA-256 前 32 位（段内容含分隔符也不会歧义）。
func deriveID(parts []string) string {
	h := sha256.New()
	h.Write([]byte("vscode-session-link:v1"))
	h.Write([]byte{0})
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		h.Write(length[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
