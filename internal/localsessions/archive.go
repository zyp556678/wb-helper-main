package localsessions

import "database/sql"

// -----------------------------------------------------------------------------
// 会话归档状态（对照 wb-switch 的 session.rs 归档传导）
//
// **要解决的问题**：一份会话在多个账号间有副本、且正文已经一致（identical / ahead）时，
// 用户在来源账号把它归档了，目标账号却永远同步不到 —— 因为归档不是「正文动作」，
// 而现有的同步模式（fastForward / overwrite / unifyOverwrite）全都以正文为中心，
// 正文一致时没有任何可勾选的项，归档这件事就卡在那里。
//
// **传导规则**（逐条对照上游，不要凭直觉放宽）：
//
//   - 单向：只把 archived 传导出去，不反向取消归档。用户手动取消归档是另一个动作。
//   - 粘滞白名单：仅当**目标处于终态**（completed / error / terminated）时才授权。
//     不使用「非 active/working」这种开放条件 —— 目标状态为 NULL / 未知值 / 缺列时
//     一律**不授权**。把未知当终态，等于把一个可能还在写的会话标成归档。
//   - 活跃态保护：目标为 active / working 时绝不写入。
//
// 之所以不放进 modeAllows：它不是被判定结论授权的正文动作，而是由归档资格
// （archiveQualifies）单独授权。混进 availableModes 会让前端把它当成一种「写入模式」，
// 而它根本不写正文。
// -----------------------------------------------------------------------------

// SessionStatusArchived 是归档态的状态值。
const SessionStatusArchived = "archived"

// ModeStatusOnly 是「只同步归档状态、不写正文」的写入模式。
//
// 它**不**出现在 availableModes 里，也不被 modeAllows 认可 —— 由前端在
// 「正文已一致但可传导归档」时单独提交（见 preview 的 archive_action）。
const ModeStatusOnly = "statusOnly"

// archiveTargetTerminal 是允许被写入归档的目标状态白名单。
//
// 三个值来自上游对真实库的实测；用**精确相等**而非「非活跃」这类开放条件。
// 缺列 / NULL / 空串一律不在其中 —— 未知状态不授权（见文件头「粘滞白名单」）。
var archiveTargetTerminal = map[string]bool{
	"completed":  true,
	"error":      true,
	"terminated": true,
}

// archiveQualifies 判断这对成员是否应传导归档：来源已归档、且目标仍在终态白名单内。
//
// 两个参数都是**从库里实时读出**的状态字符串（可能为空 = 缺列 / NULL / 查不到）。
// 任一不满足即返回 false，调用方据此不下发 archiveAction。
func archiveQualifies(sourceStatus, targetStatus string) bool {
	if sourceStatus != SessionStatusArchived {
		return false
	}
	return archiveTargetTerminal[targetStatus]
}

// -----------------------------------------------------------------------------
// 状态读写
// -----------------------------------------------------------------------------

// memberStatus 读某成员的 status 列。缺列 / 行不存在 / 值非字符串一律返回空串
// （在 archiveQualifies 里等同「不授权」，正是我们要的保守行为）。
//
// 用独立的函数而不是复用 readSessionRow：后者下拉整行，而这里只需要一列，
// 且必须能在**缺列**的老库上安静地返回空串而不是报错。
func (s *Store) memberStatus(lib *library, sessionID string) string {
	if lib == nil || !libExists(lib) || sessionID == "" {
		return ""
	}
	db, err := s.openLib(lib, true)
	if err != nil {
		return ""
	}
	defer db.Close()
	if !hasColumn(db, "sessions", "status") {
		return "" // 老库没有 status 列：不当作任何已知状态
	}
	var status sql.NullString
	if err := db.QueryRow(`SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&status); err != nil {
		return ""
	}
	if !status.Valid {
		return ""
	}
	return status.String
}

// writeMemberStatus 把目标会话的 status 写成归档态。
//
// **条件 UPDATE + affected 必须为 1**：把「不覆盖活跃会话」这条规则写进 SQL 本身，
// 而不是先读后写 —— 先读后写在「读」与「写」之间客户端可能刚好改了状态，
// 那正是这条保护要防的场景。affected 不为 1 说明条件不再成立（状态已变 / 行不存在 /
// 被软删），调用方必须按失败处理。
//
// 返回 (写入是否成功, 写后回读到的状态)。回读用于确认写入真的生效
// （affected=1 只说明行被匹配，不保证值对）。
func (s *Store) writeMemberStatus(lib *library, sessionID, userID, want string) (bool, string) {
	if lib == nil || !libExists(lib) || sessionID == "" {
		return false, ""
	}
	db, err := s.openLib(lib, false)
	if err != nil {
		return false, ""
	}
	defer db.Close()
	if !hasColumn(db, "sessions", "status") {
		return false, "" // 缺列不写：无法表达归档态
	}

	// 条件 UPDATE：只动终态行、只动未软删的行。
	// user_id 一并约束，避免 sessionID 撞车时改到别的账号的行。
	//
	// 软删口径用 COALESCE(deleted_at,0)=0 而不是 deleted_at IS NULL：
	// 客户端把「未删除」写成 0（见 sessions.go 的列表查询），NULL 只出现在
	// 新插入还没来得及赋值的极端情况。用 IS NULL 会让**绝大多数正常行**都不匹配 ——
	// 条件 UPDATE 静默不生效，affected 恒为 0。
	res, err := db.Exec(
		`UPDATE sessions SET status = ?, updated_at = ?
		 WHERE id = ? AND user_id = ? AND COALESCE(deleted_at, 0) = 0
		   AND status IN ('completed', 'error', 'terminated')`,
		want, nowMillis(), sessionID, userID,
	)
	if err != nil {
		return false, ""
	}
	affected, err := res.RowsAffected()
	if err != nil || affected != 1 {
		return false, ""
	}
	// 回读确认。
	var after sql.NullString
	if err := db.QueryRow(`SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&after); err != nil {
		return false, ""
	}
	return after.Valid && after.String == want, after.String
}
