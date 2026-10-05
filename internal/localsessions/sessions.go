// Package localsessions 读写本机 WorkBuddy 的会话库。
//
// 数据布局（实测，见下）：
//
//	~/.workbuddy/workbuddy.db                 SQLite（Drizzle 管理）
//	    sessions(id, cwd, user_id, title, custom_title, status, created_at,
//	             updated_at, last_activity_at, deleted_at, is_playground,
//	             source_mode, is_background_automation, mode, model, expert_id, ...)
//	    session_usage(session_id, used, size, updated_at, credit_json)
//	    workspaces(path, last_opened_at)
//	~/.workbuddy/projects/<workspace>/<sessionId>.jsonl   会话正文
//	~/.workbuddy/edge-sync-mapping-v4.db     云端会话映射（edge_sync_mapping）
//
// 正文格式（实测 19 个文件）：JSONL，**首行 type="session-meta"**，
// 之后约 90% 的行都带 `sessionId` 字段。
// **所以复制会话必须重写每一行的 sessionId** —— 只复制文件不改内容，
// 客户端会看到一个「文件名叫新 id、内容里写着旧 id」的会话。
//
// 安全约定（**这是本包最重要的部分**）：
//
//  1. 读一律 `mode=ro` + `query_only`。实时库正被 WorkBuddy 客户端使用，
//     以读写方式打开可能让它写入失败。
//  2. 写之前**必须备份**（DB + 正文），复用 localapps 的备份设施。
//  3. 写后**必须回读校验**。
//  4. 只做「复制/新增」，**绝不改动或删除已有会话** —— 删用户的历史是不可逆的，
//     本工具没有理由碰它。
package localsessions

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动（无 cgo），可交叉编译
)

// Session 是一条会话的元数据（来自 sessions 表）。
type Session struct {
	ID      string `json:"id"`
	Cwd     string `json:"cwd"`
	UserID  string `json:"user_id"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Model   string `json:"model"`
	Mode    string `json:"mode"`
	Created int64  `json:"created_at"`
	Updated int64  `json:"updated_at"`
	Deleted int64  `json:"deleted_at"`
	// HasBody 表示正文 JSONL 是否存在（缺正文的会话在界面上要区别对待）。
	HasBody bool `json:"has_body"`
	// BodyBytes 是正文大小（0 表示缺失或空）。
	BodyBytes int64 `json:"body_bytes"`
	// Lines 是正文行数（-1 表示未统计，统计大文件有代价）。
	Lines int64 `json:"lines"`
	// CustomTitle 是用户在客户端里改过的标题（非空时优先作为展示名）。
	CustomTitle string `json:"custom_title"`
	// IsPlayground 对应客户端侧栏的「任务」；其余按 cwd 归入「空间」。
	IsPlayground bool `json:"is_playground"`
	// Variant 是这条会话所在**会话库的档位**（cn=国内站 ~/.workbuddy，
	// intl=国际站 ~/.workbuddy-ai）。国内站与国际站是两个隔离的客户端，
	// 副本必须写进目标账号自己档位的库，跨库分组时成员要标明来自哪个库。
	Variant string `json:"variant"`
}

// library 是一个档位的会话库（数据根 + workbuddy.db）。
type library struct {
	Variant string
	Root    string
	DBPath  string
}

// Store 是会话库的访问句柄 —— **同时管国内站与国际站两个库**。
//
// 为什么必须是两个：国内站客户端用 ~/.workbuddy，国际站客户端用 ~/.workbuddy-ai，
// 两者互相隔离（认证文件、workbuddy.db、projects/ 都各一套）。
// 只读其一的后果是实打实的：往国际站账号「复制」时副本被写进国内站的库，
// 国际站客户端永远看不到它 —— 面板显示「内容一致」，switch 读两个库，
// 如实显示「2 个账号待同步」（2026-10-05 实测踩过）。
type Store struct {
	// Libraries 是全部档位的会话库（顺序固定 cn → intl）。
	Libraries []library
	// StateDir 是本工具自己的状态目录（同步基线等）。
	//
	// **由调用方注入，不默认写进 ~/.workbuddy/** —— 那是客户端的目录，
	// 往里塞我们自己的文件会让它多出看不懂的东西。
	// 为空时同步基线功能不可用（判定会退化成 Unknown，属安全降级）。
	StateDir string
	// Namespace 决定关联登记表的落点（WorkBuddy / VS Code 插件 / CodeBuddy IDE）。
	// 空值按 WorkBuddy 处理（兼容既有调用）。
	Namespace Namespace

	// Root / DBPath 是**国内站**库的根与 DB（兼容字段：既有测试与调用点引用）。
	Root   string
	DBPath string
}

// NewStore 用默认路径构造：国内站 ~/.workbuddy + 国际站 ~/.workbuddy-ai。
func NewStore(home string) *Store {
	cn := library{Variant: "cn", Root: filepath.Join(home, ".workbuddy"), DBPath: filepath.Join(home, ".workbuddy", "workbuddy.db")}
	intl := library{Variant: "intl", Root: filepath.Join(home, ".workbuddy-ai"), DBPath: filepath.Join(home, ".workbuddy-ai", "workbuddy.db")}
	return &Store{
		Libraries: []library{cn, intl},
		Root:      cn.Root,
		DBPath:    cn.DBPath,
	}
}

// libByVariant 按档位取库；未知档位返回 nil（调用方按「写不了」处理）。
func (s *Store) libByVariant(variant string) *library {
	for i := range s.Libraries {
		if s.Libraries[i].Variant == variant {
			return &s.Libraries[i]
		}
	}
	return nil
}

// libExists 表示该库存在（客户端跑过才会有 workbuddy.db）。
func libExists(lib *library) bool {
	_, err := os.Stat(lib.DBPath)
	return err == nil
}

// Exists 表示**任一**会话库可用（面板据此决定显示数据还是空态）。
func (s *Store) Exists() bool {
	for i := range s.Libraries {
		if libExists(&s.Libraries[i]) {
			return true
		}
	}
	return false
}

// open 打开某个档位的数据库。
//
// **读一律只读**：实时库正被 WorkBuddy 客户端使用。用 `mode=ro` 打开时
// SQLite 连 journal 都不会创建，从根上避免干扰它。
// busy_timeout 是给写路径用的：客户端正在写时我们要等它，而不是直接失败。
func (s *Store) openLib(lib *library, readOnly bool) (*sql.DB, error) {
	mode := "rw"
	pragmas := "&_pragma=busy_timeout(5000)"
	if readOnly {
		mode = "ro"
		pragmas = "&_pragma=query_only(1)"
	}
	dsn := fmt.Sprintf("file:%s?mode=%s%s", filepath.ToSlash(lib.DBPath), mode, pragmas)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接：SQLite 的写并发靠 busy_timeout 串行化，
	// 多连接只会让「database is locked」更容易出现。
	db.SetMaxOpenConns(1)
	return db, nil
}

// ListSessions 列出会话（按更新时间倒序，**聚合国内站与国际站两个库**）。
//
// uid 非空时只列该账号的；空则列全部。
// **默认排除已删除**（deleted_at 非 0）—— 那是客户端自己的回收站语义。
//
// 展示名口径与 wb-switch 一致：**自定义标题优先**，其次自动标题，最后「(无标题)」。
// `custom_title` / `is_playground` 是客户端较新版本才有的列，老库上没有 ——
// 缺列时按「没有自定义标题 / 都是空间会话」处理，而不是让整个查询失败。
//
// 某个库不存在（该档位客户端没跑过）不算错误：跳过即可。
func (s *Store) ListSessions(uid string, limit int) ([]Session, error) {
	out := []Session{}
	for i := range s.Libraries {
		lib := &s.Libraries[i]
		if !libExists(lib) {
			continue
		}
		rows, err := s.listSessionsIn(lib, uid)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// listSessionsIn 列出单个库里的会话。
func (s *Store) listSessionsIn(lib *library, uid string) ([]Session, error) {
	db, err := s.openLib(lib, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	hasCustom := hasColumn(db, "sessions", "custom_title")
	hasPlayground := hasColumn(db, "sessions", "is_playground")

	customExpr := "''"
	if hasCustom {
		customExpr = "COALESCE(custom_title,'')"
	}
	playExpr := "0"
	if hasPlayground {
		playExpr = "COALESCE(is_playground,0)"
	}

	q := `SELECT id, COALESCE(cwd,''), COALESCE(user_id,''), COALESCE(title,''),
	             COALESCE(status,''), COALESCE(model,''), COALESCE(mode,''),
	             COALESCE(created_at,0), COALESCE(updated_at,0), COALESCE(deleted_at,0),
	             ` + customExpr + `, ` + playExpr + `
	      FROM sessions WHERE COALESCE(deleted_at,0) = 0`
	args := []any{}
	if strings.TrimSpace(uid) != "" {
		q += ` AND user_id = ?`
		args = append(args, uid)
	}
	q += ` ORDER BY COALESCE(updated_at,0) DESC`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		var it Session
		var customTitle string
		var playground int
		if err := rows.Scan(&it.ID, &it.Cwd, &it.UserID, &it.Title, &it.Status,
			&it.Model, &it.Mode, &it.Created, &it.Updated, &it.Deleted,
			&customTitle, &playground); err != nil {
			return nil, err
		}
		it.CustomTitle = strings.TrimSpace(customTitle)
		it.IsPlayground = playground != 0
		it.Title = sessionDisplayTitle(it.Title, it.CustomTitle)
		it.Variant = lib.Variant
		// 正文路径：projects/<workspace>/<sessionId>.jsonl（只在本库找 ——
		// 跨库搜同名正文没有意义，还能避免把幻影副本的正文误配给别的档位）。
		if p, ok := s.bodyPathIn(lib, it.ID, it.Cwd); ok {
			if st, err := os.Stat(p); err == nil {
				it.HasBody = true
				it.BodyBytes = st.Size()
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// sessionDisplayTitle 取会话的展示名（自定义标题优先，与 wb-switch 一致）。
func sessionDisplayTitle(title, customTitle string) string {
	if t := strings.TrimSpace(customTitle); t != "" {
		return t
	}
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	return "(无标题)"
}

// hasColumn 判断表里有没有某一列（客户端升级会加列，老库上没有）。
func hasColumn(db *sql.DB, table, col string) bool {
	cols, err := tableColumns(db, table)
	if err != nil {
		return false
	}
	return cols[strings.ToLower(col)]
}

// tableColumns 返回表的列名集合（小写键）。供「按实际 schema 裁剪写入列」用。
func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[strings.ToLower(name)] = true
	}
	return out, rows.Err()
}

// ListUserIDs 列出库里出现过的账号（user_id），供界面分组（聚合两个库）。
func (s *Store) ListUserIDs() ([]string, error) {
	set := map[string]bool{}
	for i := range s.Libraries {
		lib := &s.Libraries[i]
		if !libExists(lib) {
			continue
		}
		db, err := s.openLib(lib, true)
		if err != nil {
			return nil, err
		}
		rows, err := db.Query(`SELECT DISTINCT COALESCE(user_id,'') FROM sessions`)
		if err != nil {
			db.Close()
			return nil, err
		}
		for rows.Next() {
			var u string
			if err := rows.Scan(&u); err != nil {
				rows.Close()
				db.Close()
				return nil, err
			}
			if strings.TrimSpace(u) != "" {
				set[u] = true
			}
		}
		err = rows.Err()
		rows.Close()
		db.Close()
		if err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out, nil
}

// resolveBody 在**全部库**里定位会话正文，返回它所在的库（跨库分组/同步要用）。
func (s *Store) resolveBody(sessionID, cwd string) (*library, string, bool) {
	for i := range s.Libraries {
		lib := &s.Libraries[i]
		if !libExists(lib) {
			continue
		}
		if p, ok := s.bodyPathIn(lib, sessionID, cwd); ok {
			return lib, p, true
		}
	}
	return nil, "", false
}

// bodyPath 定位会话正文（任意库）。
//
// 目录名不是 sessionId，而是**工作区路径的扁平化形式**
// （实测形如 `c-Users-x-WorkBuddy-2026-09-23-19-00-55`），
// 所以只能按 cwd 推不出来 —— 直接全目录搜同名文件，命中即返回。
//
// 搜不到时返回 ok=false（正文可能被客户端清过，属正常）。
func (s *Store) bodyPath(sessionID, cwd string) (string, bool) {
	_, p, ok := s.resolveBody(sessionID, cwd)
	return p, ok
}

// bodyPathIn 在**指定库**里定位正文（列表/写入都必须限定库，不能跨库乱配）。
func (s *Store) bodyPathIn(lib *library, sessionID, cwd string) (string, bool) {
	base := sessionID + ".jsonl"
	projects := filepath.Join(lib.Root, "projects")

	// 先按 cwd 猜测目录（常见情形，省一次遍历）。
	if cwd != "" {
		if guess := flattenWorkspace(cwd); guess != "" {
			p := filepath.Join(projects, guess, base)
			if _, err := os.Stat(p); err == nil {
				return p, true
			}
		}
	}
	// 回退：遍历 projects 下的一层子目录。
	entries, err := os.ReadDir(projects)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(projects, e.Name(), base)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// flattenWorkspace 把工作区路径扁平化成客户端用的目录名。
//
// 实测规则：`D:\software\workbuddy-gateway` → `d-software-workbuddy-gateway`
// （盘符后的**冒号消失**、分隔符变 `-`、全小写）。**这只是「先猜一下」的优化**，
// 猜不中会回退到全目录搜索，所以规则不完全精确也不影响正确性。
func flattenWorkspace(cwd string) string {
	c := strings.TrimSpace(cwd)
	if c == "" {
		return ""
	}
	c = strings.ReplaceAll(c, ":", "")
	c = strings.ReplaceAll(c, `\`, "-")
	c = strings.ReplaceAll(c, "/", "-")
	// 折叠连续减号（`a//b` 这类路径会出现）。
	for strings.Contains(c, "--") {
		c = strings.ReplaceAll(c, "--", "-")
	}
	c = strings.Trim(c, "-")
	return strings.ToLower(c)
}

// -----------------------------------------------------------------------------
// 复制
// -----------------------------------------------------------------------------

// CopyResult 是一次复制的产物。
type CopyResult struct {
	SourceID string `json:"source_id"`
	NewID    string `json:"new_id"`
	UserID   string `json:"user_id"`
	// GroupID 是登记组 id（新组为预分配的 UUID；已有组沿用）。
	GroupID string `json:"group_id"`
	// Status：linked=已登记；alreadyLinked=目标已有有效副本（幂等跳过）。
	Status string `json:"status"`
	// BodyPath 是新的正文路径（空表示源会话没有正文）。
	BodyPath string `json:"body_path"`
	// BodyLines 是重写了 sessionId 的行数。
	BodyLines int `json:"body_lines"`
	// BackupID 是备份标识，出问题可据此恢复。
	BackupID string `json:"backup_id"`
	// Notes 是逐条说明。
	Notes []string `json:"notes"`
}

// CopyOptions 是复制选项。
type CopyOptions struct {
	// TargetUID 是目标账号的 user_id（会话归属）。必填。
	TargetUID string
	// TargetVariant 是目标账号的档位（cn/intl）。**决定副本写进哪个库**：
	// 国际站账号必须写 ~/.workbuddy-ai，写进国内站库客户端根本看不见。
	// 为空时按「与源会话同库」处理（兼容旧调用）。
	TargetVariant string
	// AccountID 是目标账号在池里的 id（登记成员用，可空）。
	AccountID string
	// TitleSuffix 追加到标题末尾，便于区分副本（默认「（副本）」）。
	TitleSuffix string
	// DryRun 为真时只做检查与备份，不写任何东西。
	DryRun bool
}

// targetBodyPath 计算副本正文在**目标库**里的路径。
//
// 规则（对照 wb-switch 的 `target_body_path`，跨档实验已验证）：**沿用源正文在
// 源库 projects/ 下的相对目录**。客户端按会话行的 cwd 推导工作区目录名，而 cwd
// 复制时原样保留，所以目录名必须逐字一致 —— 不能用扁平化规则去猜（实测客户端
// 目录保留大小写与空格，`c-Users-x-WorkBuddy AI-2026-09-28-10-04-08`，
// 猜出来是小写连字符形式，客户端不认）。同库复制时自然退化为「与源同目录」。
func (s *Store) targetBodyPath(srcLib *library, sourceBody string, targetLib *library, newID string) (string, error) {
	srcProjects := filepath.Join(srcLib.Root, "projects")
	rel, err := filepath.Rel(srcProjects, filepath.Dir(sourceBody))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		// 源正文不在源库 projects/ 下（不该发生）：退到目标库 projects 根。
		return filepath.Join(targetLib.Root, "projects", newID+".jsonl"), nil
	}
	return filepath.Join(targetLib.Root, "projects", rel, newID+".jsonl"), nil
}

// copyJSONLRewriteSessionID 复制正文并把 sessionId 改成 newID。
//
// **用精准的字节替换，而不是「逐行解析 → 重新序列化」。**
// 后者（第一版）虽然安全，但代价很大，实测在 71MB 的会话上暴露得很清楚：
//
//	json.Marshal(map[string]any) 会 **按键名排序**、并默认把 `<` `>` `&`
//	转成 \u003c 等，于是产出的文件**每一个字节都和原文不同**（71.0MB → 71.8MB）。
//	对客户端解析而言语义等价，但：文件白白变大、慢（71MB 要全量解析+序列化）、
//	而且**任何基于内容比较的后续功能（如同步的差异判定）都会失效**。
//
// 精准替换为什么是安全的：JSON 字符串值里的引号**必须写成 \"**，
// 所以文件里出现**未被反斜杠转义**的 `"sessionId":"` 序列时，
// 它只可能是**真正的字段名** —— 消息正文里就算原样贴着这段文本，
// 在文件里也是 `\"sessionId\":\"`，不会被误伤。
// 这也正是本函数只额外检查「前一个字节不是反斜杠」的原因。
func copyJSONLRewriteSessionID(src, dst, oldID, newID string) (int, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return 0, err
	}
	out, n := replaceSessionIDBytes(data, oldID, newID)
	if err := writeFileAtomic(dst, out); err != nil {
		return 0, err
	}
	return n, nil
}

// replaceSessionIDBytes 把 `"sessionId":"<oldID>"` 精准替换成新 id。
//
// 返回替换次数。只认**未被转义**的字段名，且只替换**值恰好等于 oldID** 的那些。
func replaceSessionIDBytes(data []byte, oldID, newID string) ([]byte, int) {
	field := `"sessionId":"`
	needle := field + oldID + `"`

	var out []byte
	count := 0
	pos := 0
	for {
		i := indexFrom(data, needle, pos)
		if i < 0 {
			break
		}
		// 前一个字节是反斜杠 → 这是**字符串值内部**被转义的引号，
		// 即消息正文里恰好贴着这段文本，不能碰。
		if i > 0 && data[i-1] == '\\' {
			pos = i + 1
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(data)+64)
		}
		out = append(out, data[pos:i]...)
		out = append(out, field...)
		out = append(out, newID...)
		out = append(out, '"')
		count++
		pos = i + len(needle)
	}
	if out == nil {
		// 一次都没命中：**原样返回原切片**，不做任何拷贝（大文件下这很重要）。
		return data, 0
	}
	out = append(out, data[pos:]...)
	return out, count
}

// indexFrom 是 bytes.Index 的「从 pos 开始」版本。
func indexFrom(data []byte, needle string, pos int) int {
	if pos >= len(data) {
		return -1
	}
	i := strings.Index(string(data[pos:]), needle)
	if i < 0 {
		return -1
	}
	return pos + i
}

// writeFileAtomic 原子写入（同目录 tmp + rename），权限 0600。
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	// fsync 再 rename：会话正文属于「业务完成门禁」——掉电后 rename 元数据可能已落盘
	// 而数据还在缓存里，留下一个内容为空的正文（对照 switch 的 durable_write_str）。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// readSessionRow 读一整行成 map（列名 → 值）。
//
// 用 `SELECT *` 拿全部列：**客户端升级加列时，写死的列清单会静默丢数据**，
// 而复制语义上应当是「照着源行复制，只改身份相关的几列」。
func readSessionRow(db *sql.DB, id string) (map[string]any, error) {
	rows, err := db.Query(`SELECT * FROM sessions WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, nil // 不存在
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		switch v := vals[i].(type) {
		case []byte:
			out[c] = string(v)
		default:
			out[c] = v
		}
	}
	return out, nil
}

// insertSessionRow 按源行构造 INSERT，只覆盖身份相关的列。
func insertSessionRow(db *sql.DB, src map[string]any, newID string, opts CopyOptions) error {
	now := time.Now().UnixMilli()
	// **列集合必须先与目标表的实际 schema 求交集**：客户端不同版本的 sessions 表
	// 列不一样（老库没有 last_activity_at / custom_title，未来可能加新列）。
	// 不求交集的后果：老库上直接报「table sessions has no column named …」，
	// 整个复制失败（实测抓到过）。这也正是本函数用「读到的整行 + 覆盖少数几列」
	// 的原因 —— 那张「整行」本身也必须按 schema 裁剪。
	existing, err := tableColumns(db, "sessions")
	if err != nil {
		return fmt.Errorf("读取 sessions 表结构失败: %w", err)
	}
	for c := range src {
		if !existing[strings.ToLower(c)] {
			delete(src, c)
		}
	}

	// 时间戳单位实测是**毫秒**（如 1759... 13 位）；写秒会让新会话排到最前/最后。
	src["id"] = newID
	src["user_id"] = opts.TargetUID
	src["created_at"] = now
	src["updated_at"] = now
	if existing["last_activity_at"] {
		src["last_activity_at"] = now
	}
	if t, ok := src["title"].(string); ok {
		src["title"] = t + opts.TitleSuffix
	}
	if _, ok := src["custom_title"]; ok {
		src["custom_title"] = nil
	}
	// 副本不该继承「云同步/自动化」这类身份，否则会以源会话的名义继续同步。
	if _, ok := src["is_background_automation"]; ok {
		src["is_background_automation"] = 0
	}
	if _, ok := src["deleted_at"]; ok {
		src["deleted_at"] = 0
	}

	// 列顺序固定（按 map 遍历会是随机的，必须排序保证可复现）。
	cols := make([]string, 0, len(src))
	for c := range src {
		cols = append(cols, c)
	}
	sort.Strings(cols)

	ph := make([]string, len(cols))
	args := make([]any, len(cols))
	for i, c := range cols {
		ph[i] = "?"
		args[i] = src[c]
	}
	q := fmt.Sprintf(`INSERT INTO sessions (%s) VALUES (%s)`,
		strings.Join(quoteAll(cols), ", "), strings.Join(ph, ", "))
	if _, err := db.Exec(q, args...); err != nil {
		return err
	}
	return nil
}

func quoteAll(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// bodyDigests 读一条会话的正文并返回归一化行摘要（供复制时落基线）。
func (s *Store) bodyDigests(sessionID string) ([]string, error) {
	row, err := s.sessionCwd(sessionID)
	if err != nil {
		return nil, err
	}
	path, ok := s.bodyPath(sessionID, row)
	if !ok {
		return nil, fmt.Errorf("没有正文文件")
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	norm, err := normalizeJSONL(string(text), sessionID)
	if err != nil {
		return nil, err
	}
	return norm.LineDigests, nil
}

// newUUID 生成 UUID v4（不引第三方包，格式固定 8-4-4-4-12）。
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
