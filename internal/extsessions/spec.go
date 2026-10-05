// Package extsessions 负责「扩展数据仓」的会话复制与关联同步 ——
// VS Code 内 CodeBuddy 插件与 CodeBuddy IDE 桌面客户端两棵**同根同构**的会话树。
//
// 对照 wb-switch 的 `vscode_session.rs` / `vscode_session_link.rs` /
// `vscode_session_sync.rs` / `codebuddy_ide_session.rs`。与 WorkBuddy 侧的差别：
//
//   - 记录单位是**一条消息**（不是 JSONL 的一行）：`index.json` 的 `messages[]` 顺序
//     即记录顺序（目录顺序不可靠，不用 read_dir）；
//   - 会话 = 一个目录（`index.json` + `messages/*.json` + `assets/*`），
//     复制 = 整目录复制 + 合并工作区索引，没有数据库；
//   - id 一律 **32 位小写 hex**（扩展 `generateId()` 产物），不是带连字符的 UUID；
//   - 写之前 VS Code / IDE **必须完全退出**：运行中的编辑器会把内存里的索引与消息
//     回写磁盘，覆盖我们的写入（与 WorkBuddy 客户端同理，理由一致）。
//
// 存储布局（Windows 实测）：
//
//	%LOCALAPPDATA%\CodeBuddyExtension\Data\<uid>\VSCode\<uid>\
//	  history\<md5(工作区)>\
//	    index.json              # 工作区级会话索引 {conversations:[...], current}
//	    .index_bak.json         # 同结构备份（插件侧同步写；IDE 侧不动）
//	    <conversationId>\       # 32 位小写 hex
//	      index.json            # {messages:[...], requests:[...]}
//	      messages\<messageId>.json
//	      assets\*
//
// 只写目标 uid 目录；源 uid 目录只读，绝不修改或删除。显式排除 `default\` /
// `Public\`（结构不同），uid 只能来自账号库/扩展登录态，不接受用户任意输入。
package extsessions

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HistoryDir 是会话历史目录名。
const HistoryDir = "history"

// Spec 是扩展数据仓的分段与备份命名（区分插件与 IDE 两棵同构的会话树）。
type Spec struct {
	// AppDir 是应用数据目录名（`CodeBuddyExtension`）。
	AppDir string
	// ClientDir 是客户端段目录名（`VSCode` / `CodeBuddyIDE`）。
	ClientDir string
	// BackupKind 是会话索引备份的根目录名（`<工具存储根>/backups/<BackupKind>/<utc_iso>/`）。
	BackupKind string
	// Label 是给用户看的客户端名。
	Label string
	// Namespace 是关联登记表的命名空间（与 WorkBuddy 侧完全隔离）。
	Namespace string
}

// VSCodeStore 是 VS Code 内 CodeBuddy 插件（`tencent-cloud.coding-copilot`）的数据仓。
var VSCodeStore = Spec{
	AppDir: "CodeBuddyExtension", ClientDir: "VSCode",
	BackupKind: "vscode-sessions", Label: "VS Code 插件", Namespace: "vscode-ext",
}

// IDEStore 是 CodeBuddy IDE 桌面客户端的数据仓：与插件同根，只有客户端段不同。
var IDEStore = Spec{
	AppDir: "CodeBuddyExtension", ClientDir: "CodeBuddyIDE",
	BackupKind: "codebuddy-ide-sessions", Label: "CodeBuddy IDE", Namespace: "codebuddy-ide",
}

// CopyItem 是待复制的会话引用（工作区 hash + 会话 id）。
type CopyItem struct {
	// WorkspaceHash 是工作区目录名 = md5(工作区)（32 位小写 hex）。
	WorkspaceHash string `json:"workspaceHash"`
	// ConversationID 是会话 id（32 位小写 hex）。
	ConversationID string `json:"conversationId"`
}

// IDPolicy 决定复制体沿用源 id 还是取新 id。
type IDPolicy int

const (
	// IDAlwaysNew：副本一律取新 id（VS Code 插件：扩展按 id 归类，复制体必须与源区分）。
	IDAlwaysNew IDPolicy = iota
	// IDKeepUnlessConflict：默认沿用源 id，仅当目标工作区已存在同 id 时取新 id 并重写引用
	// （CodeBuddy IDE：真机实测沿用 id 的目标账号可直接加载；冲突重随机避免覆盖既有会话）。
	IDKeepUnlessConflict
)

// CopyOptions 是复制策略：id 生成方式与目标工作区索引的写入范围。
type CopyOptions struct {
	IDPolicy IDPolicy
	// WriteWorkspaceIndexBackup 是否把合并后的索引同步写入目标工作区的 `.index_bak.json`。
	// 插件侧写（扩展自用的索引备份需要与主索引一致）；IDE 侧不写。
	WriteWorkspaceIndexBackup bool
}

// VSCodeCopy / IDECopy 是两侧的复制策略（与 switch 的两个常量逐字段一致）。
var (
	VSCodeCopy = CopyOptions{IDPolicy: IDAlwaysNew, WriteWorkspaceIndexBackup: true}
	IDECopy    = CopyOptions{IDPolicy: IDKeepUnlessConflict, WriteWorkspaceIndexBackup: false}
)

// -----------------------------------------------------------------------------
// 路径解析
// -----------------------------------------------------------------------------

// testDataRootOverride 仅供测试注入（生产为 nil）。
var testDataRootOverride *string

// dataRootCandidates 返回扩展数据根候选目录（按优先级去重）。
//
// Windows：`%LOCALAPPDATA%\<app>\Data`（实测）；macOS / Linux 按同一相对布局推导
// （**未实测**，仅作 best-effort 兜底）。
func dataRootCandidates(spec Spec) []string {
	if testDataRootOverride != nil && *testDataRootOverride != "" {
		return []string{*testDataRootOverride}
	}
	bases := []string{}
	switch {
	case os.Getenv("LOCALAPPDATA") != "":
		bases = append(bases, os.Getenv("LOCALAPPDATA"))
	case os.Getenv("XDG_CACHE_HOME") != "":
		bases = append(bases, os.Getenv("XDG_CACHE_HOME"))
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		bases = append(bases, filepath.Join(h, ".cache"), filepath.Join(h, "Library", "Caches"))
	}
	var out []string
	seen := map[string]bool{}
	for _, base := range bases {
		candidate := filepath.Join(base, spec.AppDir, "Data")
		if !seen[candidate] {
			seen[candidate] = true
			out = append(out, candidate)
		}
	}
	return out
}

// DataRoot 解析扩展数据根目录：返回第一个真实存在的候选；都不存在返回空串。
func DataRoot(spec Spec) string {
	for _, c := range dataRootCandidates(spec) {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

// UIDDataDir 是账号 uid 的数据目录：`<root>/<uid>/<client_dir>/<uid>`。
func UIDDataDir(spec Spec, root, uid string) string {
	return filepath.Join(root, uid, spec.ClientDir, uid)
}

// HistoryRoot 是账号 uid 的会话历史根：`<root>/<uid>/<client_dir>/<uid>/history`。
func HistoryRoot(spec Spec, root, uid string) string {
	return filepath.Join(UIDDataDir(spec, root, uid), HistoryDir)
}

// IsSafeUID 是 uid 白名单校验：非空、不含路径分隔符 / `..`、且不是 `default` / `Public`。
//
// 用于杜绝路径穿越与误碰结构不同的兜底目录。
func IsSafeUID(uid string) bool {
	uid = strings.TrimSpace(uid)
	if uid == "" || uid == "." || uid == ".." {
		return false
	}
	if strings.ContainsAny(uid, `/\`) {
		return false
	}
	if strings.EqualFold(uid, "default") || strings.EqualFold(uid, "public") {
		return false
	}
	return true
}

// IsHex32 判断是否为 32 位小写 hex id。
func IsHex32(text string) bool {
	if len(text) != 32 {
		return false
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// GenHex32 生成 32 位小写 hex id（对齐扩展 `generateId()` 产物，非带连字符 UUID）。
func GenHex32() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, 32)
	for _, by := range b {
		out = append(out, hexDigits[by>>4], hexDigits[by&0x0f])
	}
	return string(out)
}

// uniqueHex32 生成一个此前未出现的 32 位 hex id，并登记进 used。
func uniqueHex32(used map[string]bool) string {
	for {
		candidate := GenHex32()
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

// -----------------------------------------------------------------------------
// 通用小工具
// -----------------------------------------------------------------------------

// ReadJSON 读取并解析 JSON 文件；文件缺失或内容损坏时返回 nil（不 panic）。
func ReadJSON(path string) any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// TimeToMs 把索引里的时间字段换算成 epoch 毫秒；无法识别时返回 0。
//
// 支持 RFC3339 字符串与数字（>1e12 视为毫秒，否则按秒）。
func TimeToMs(value any) int64 {
	switch v := value.(type) {
	case string:
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return t.UnixMilli()
	case float64:
		raw := int64(v)
		if raw > 1_000_000_000_000 {
			return raw
		}
		return raw * 1000
	case int64:
		if v > 1_000_000_000_000 {
			return v
		}
		return v * 1000
	}
	return 0
}

// CopyDirRecursive 递归复制目录（保持相对结构与文件名，含附件中文名）。
func CopyDirRecursive(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			if err := CopyDirRecursive(from, to); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(from, to); err != nil {
			return err
		}
	}
	return nil
}

// copyFile 复制单个文件（0600）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// RemoveDirAllIfExists 尽力删除目录树（忽略不存在 / 失败）。
func RemoveDirAllIfExists(path string) {
	if _, err := os.Stat(path); err == nil {
		_ = os.RemoveAll(path)
	}
}

// WriteFileAtomic 原子写入（同目录 tmp + fsync + rename）。
func WriteFileAtomic(path string, data []byte) error {
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
	return nil
}

// UTCTimestamp 返回备份目录用的 UTC 时间戳（`20060102T150405Z`）。
func UTCTimestamp() string {
	return time.Now().UTC().Format("20060102T150405Z")
}

// jsonObject 把解析结果当对象用（不是对象时返回空对象）。
func jsonObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// jsonArray 把解析结果当数组用（不是数组时返回 nil）。
func jsonArray(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

// sortedKeys 返回 map 的键（排序后，保证输出可复现）。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// errf 是 fmt.Errorf 的短别名（本文件里错误文案较多）。
func errf(format string, args ...any) error { return fmt.Errorf(format, args...) }
