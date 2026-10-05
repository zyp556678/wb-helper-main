package extsessions

// -----------------------------------------------------------------------------
// 会话枚举与内容身份（对照 vscode_session.rs 的枚举部分 + vscode_session_link.rs）
//
// **记录单位是「一条消息」**（不是 JSONL 的一行）：
//   - 记录序列 = 会话 `index.json` 的 `messages[]` **顺序**（目录顺序不可靠，不用 ReadDir）；
//   - 单条摘要 = `messages/<id>.json` 的**规范化 JSON** 取 SHA-256；
//   - 规范化 = 把「副本特异的 id」换成占位符：`messages[].id` → `m0/m1…`、
//     `requests[].id` → `r0/r1…`（各按索引首次出现顺序编号）、会话自身 id → 固定标记。
//
// 只替换**已知 id**，其它 32-hex（如 `extra.traceId`）原样保留 —— 沿用保守哲学：
// 未知差异宁可作为差异呈现，也不猜。同一逻辑会话的源与副本因此得到逐条相同的摘要，
// 这是「可快进 / 分叉 / 一致」判定的地基。
//
// 反直觉但已实测：`isComplete: false` 是助手消息的**常态**，**不得**用它判断
// 「消息是否写完」—— 摘要只按字节算。
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

// SessionIDMarker 是归一化时替代「本副本会话 id」的固定标记（与 WorkBuddy 侧同一取值）。
const SessionIDMarker = "__wb_switch_session_id__"

// ExtSession 是一条可复制的会话（来自工作区索引）。
type ExtSession struct {
	ID            string `json:"id"`
	WorkspaceHash string `json:"workspace_hash"`
	Title         string `json:"title"`
	UpdatedAt     int64  `json:"updated_at"`
	Type          string `json:"type"`
	HasHistory    bool   `json:"has_history"`
}

// ListResult 是「某账号可复制的会话」列表。
type ListResult struct {
	SourceUID string       `json:"source_uid"`
	Sessions  []ExtSession `json:"sessions"`
	// Skipped 是无法解析（损坏）的工作区索引数量。
	Skipped int `json:"skipped"`
	// DataRoot 是解析到的扩展数据根目录（找不到时为空）。
	DataRoot string `json:"data_root"`
}

// ListSessions 列出某账号可复制的会话（按工作区 hash 分桶）。
func ListSessions(spec Spec, root, uid string) ListResult {
	out := ListResult{SourceUID: uid, Sessions: []ExtSession{}, DataRoot: root}
	if root == "" || !IsSafeUID(uid) {
		return out
	}
	history := HistoryRoot(spec, root, uid)
	entries, err := os.ReadDir(history)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		wsDir := filepath.Join(history, entry.Name())
		workspaceHash := entry.Name()
		index, ok := ReadJSON(filepath.Join(wsDir, "index.json")).(map[string]any)
		if !ok {
			out.Skipped++
			continue
		}
		conversations := jsonArray(index["conversations"])
		if conversations == nil {
			out.Skipped++
			continue
		}
		for _, raw := range conversations {
			conv, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id := strings.TrimSpace(strOf(conv["id"]))
			if id == "" {
				continue
			}
			title := strings.TrimSpace(strOf(conv["name"]))
			if title == "" {
				title = "(无标题)"
			}
			updated := TimeToMs(conv["lastMessageAt"])
			if updated == 0 {
				updated = TimeToMs(conv["createdAt"])
			}
			out.Sessions = append(out.Sessions, ExtSession{
				ID: id, WorkspaceHash: workspaceHash, Title: title,
				UpdatedAt: updated, Type: strOf(conv["type"]),
				HasHistory: conversationHasHistory(wsDir, id),
			})
		}
	}
	sort.SliceStable(out.Sessions, func(i, j int) bool {
		return out.Sessions[i].UpdatedAt > out.Sessions[j].UpdatedAt
	})
	return out
}

// conversationHasHistory 判断会话是否含正文（索引有 messages，或磁盘 messages/ 下有文件）。
func conversationHasHistory(wsDir, convID string) bool {
	convDir := filepath.Join(wsDir, convID)
	if index, ok := ReadJSON(filepath.Join(convDir, "index.json")).(map[string]any); ok {
		if len(jsonArray(index["messages"])) > 0 {
			return true
		}
	}
	entries, err := os.ReadDir(filepath.Join(convDir, "messages"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return true
		}
	}
	return false
}

// NormalizedContent 是归一化后的有序内容身份。
type NormalizedContent struct {
	RecordCount int
	LineDigests []string
	TotalDigest string
}

// ContentSnapshot 是一次成功读取的会话快照。
type ContentSnapshot struct {
	// IndexBytes 是会话索引原文（VS Code 侧没有单一正文字段，写入以文件为单位）。
	IndexBytes []byte
	// FullDigest 覆盖索引与全部消息文件的原始字节（预览凭据的版本绑定用）。
	FullDigest string
	Normalized NormalizedContent
}

// ContentStateKind 是读取结果的三态。
type ContentStateKind int

const (
	// ContentMissing：会话目录下没有 index.json（会话不存在）。
	ContentMissing ContentStateKind = iota
	// ContentReady：读到了可判定的内容。
	ContentReady
	// ContentUnavailable：索引无法解析 / 缺少消息 / 消息文件缺失损坏 —— 一律不猜。
	ContentUnavailable
)

// ContentState 是内容读取结果。
type ContentState struct {
	Kind     ContentStateKind
	Snapshot *ContentSnapshot
	Reason   string
}

// ReadSessionContent 读取一条会话的内容状态。
func ReadSessionContent(convDir, conversationID string) ContentState {
	indexPath := filepath.Join(convDir, "index.json")
	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ContentState{Kind: ContentMissing}
		}
		return ContentState{Kind: ContentUnavailable, Reason: "会话索引无法读取：" + err.Error()}
	}
	var index map[string]any
	if json.Unmarshal(indexBytes, &index) != nil {
		return ContentState{Kind: ContentUnavailable, Reason: "会话索引无法解析，内容可能不完整"}
	}
	messages, hasMessages := index["messages"].([]any)
	if !hasMessages {
		return ContentState{Kind: ContentUnavailable, Reason: "会话索引缺少消息列表，内容可能不完整"}
	}
	if len(messages) == 0 {
		return ContentState{Kind: ContentUnavailable, Reason: "会话内容为空，无法确认"}
	}

	placeholders := placeholderMap(index, conversationID)
	digests := make([]string, 0, len(messages))
	var raw []byte
	raw = appendLenPrefixed(raw, indexBytes)
	for _, rawMsg := range messages {
		msg, _ := rawMsg.(map[string]any)
		id := strings.TrimSpace(strOf(msg["id"]))
		if id == "" {
			return ContentState{Kind: ContentUnavailable, Reason: "会话索引中的消息缺少 id，内容可能不完整"}
		}
		bytes, err := os.ReadFile(filepath.Join(convDir, "messages", id+".json"))
		if err != nil {
			if os.IsNotExist(err) {
				return ContentState{Kind: ContentUnavailable, Reason: "消息 " + id + " 的文件不存在，内容可能不完整"}
			}
			return ContentState{Kind: ContentUnavailable, Reason: "消息 " + id + " 的文件无法读取：" + err.Error()}
		}
		var value any
		if json.Unmarshal(bytes, &value) != nil {
			return ContentState{Kind: ContentUnavailable, Reason: "消息 " + id + " 的文件无法解析，内容可能不完整"}
		}
		normalized, err := json.Marshal(normalizeMessage(value, placeholders))
		if err != nil {
			return ContentState{Kind: ContentUnavailable, Reason: "消息 " + id + " 无法归一化"}
		}
		digests = append(digests, lineDigestOf(string(normalized)))
		raw = appendLenPrefixed(raw, bytes)
	}
	return ContentState{Kind: ContentReady, Snapshot: &ContentSnapshot{
		IndexBytes: indexBytes,
		FullDigest: fullDigestOf(raw),
		Normalized: NormalizedContent{
			RecordCount: len(digests),
			LineDigests: digests,
			TotalDigest: totalDigestOf(digests),
		},
	}}
}

// appendLenPrefixed 长度编码后拼接：避免「两段内容边界不同」产生同一个原始摘要。
func appendLenPrefixed(buffer, bytes []byte) []byte {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(bytes)))
	buffer = append(buffer, length[:]...)
	return append(buffer, bytes...)
}

// placeholderMap 构造规范化占位符表：按索引顺序给消息 / 请求 id 编号，
// 会话自身 id 用固定标记。同一逻辑会话的源与副本 id 不同但顺序一致，
// 因此两侧得到同一套占位符，逐条摘要可比。
func placeholderMap(index map[string]any, conversationID string) map[string]string {
	placeholders := map[string]string{}
	for i, raw := range jsonArray(index["messages"]) {
		msg, _ := raw.(map[string]any)
		id := strings.TrimSpace(strOf(msg["id"]))
		if id == "" {
			continue
		}
		if _, ok := placeholders[id]; !ok {
			placeholders[id] = "m" + itoa(i)
		}
	}
	for i, raw := range jsonArray(index["requests"]) {
		req, _ := raw.(map[string]any)
		id := strings.TrimSpace(strOf(req["id"]))
		if id == "" {
			continue
		}
		if _, ok := placeholders[id]; !ok {
			placeholders[id] = "r" + itoa(i)
		}
	}
	if own := strings.TrimSpace(conversationID); own != "" {
		placeholders[own] = SessionIDMarker
	}
	return placeholders
}

// normalizeMessage 归一化一条消息文件：顶层 `id` 与 `extra` 内的 id 引用换成占位符，
// 其余字段原样保留（与复制路径的写入范围严格一致：复制不改 message 正文，归一化同样不改）。
func normalizeMessage(value any, placeholders map[string]string) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	out := make(map[string]any, len(object))
	for k, v := range object {
		out[k] = v
	}
	if id, ok := out["id"].(string); ok {
		if marker, found := placeholders[id]; found {
			out["id"] = marker
		}
	}
	if extra, ok := out["extra"]; ok {
		out["extra"] = replaceIDsInExtra(extra, placeholders)
	}
	return out
}

// replaceIDsInExtra 把 `extra`（对象，或**被序列化成字符串的对象**）里的 id 引用换成占位符。
func replaceIDsInExtra(extra any, placeholders map[string]string) any {
	var object map[string]any
	stringified := false
	switch v := extra.(type) {
	case string:
		if json.Unmarshal([]byte(v), &object) != nil {
			return extra
		}
		stringified = true
	case map[string]any:
		object = v
	default:
		return extra
	}
	out := make(map[string]any, len(object))
	for k, v := range object {
		replaceExactIDs(v, placeholders)
		out[k] = v
	}
	value := any(out)
	if stringified {
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return value
}

// replaceExactIDs 递归遍历任意 JSON 值，把「恰好等于映射表中某个旧 id」的字符串替换掉。
func replaceExactIDs(value any, placeholders map[string]string) {
	switch v := value.(type) {
	case string:
		_ = v
	case []any:
		for i := range v {
			if s, ok := v[i].(string); ok {
				if marker, found := placeholders[s]; found {
					v[i] = marker
					continue
				}
			}
			replaceExactIDs(v[i], placeholders)
		}
	case map[string]any:
		for k := range v {
			if s, ok := v[k].(string); ok {
				if marker, found := placeholders[s]; found {
					v[k] = marker
					continue
				}
			}
			replaceExactIDs(v[k], placeholders)
		}
	}
}

// lineDigestOf 是单条记录摘要。
//
// 与 WorkBuddy 侧同一口径（SHA-256 十六进制）；长度编码由上层拼接完成
// （规范化后的 JSON 字符串直接取摘要，与 switch 的 `line_digest_of` 一致）。
func lineDigestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// totalDigestOf 由有序摘要推导总摘要（域分隔 + 长度前缀，防拼接歧义）。
//
// 取值与 WorkBuddy 侧完全一致 —— 两处共用同一套摘要口径，便于对照排查。
func totalDigestOf(digests []string) string {
	h := sha256.New()
	h.Write([]byte("wb-switch-lines-v1\x00"))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(digests)))
	h.Write(length[:])
	for _, d := range digests {
		h.Write([]byte(d))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fullDigestOf 是原始字节摘要（长度编码拼接后取 SHA-256）。
func fullDigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// EqualDigests / IsOrderedPrefix / IsStrictOrderedExtension 与判定层共用的三个谓词。
func EqualDigests(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func IsOrderedPrefix(prefix, full []string) bool {
	if len(prefix) > len(full) {
		return false
	}
	for i := range prefix {
		if prefix[i] != full[i] {
			return false
		}
	}
	return true
}

func IsStrictOrderedExtension(prefix, full []string) bool {
	return len(prefix) < len(full) && IsOrderedPrefix(prefix, full)
}

// strOf 取字符串字段（非字符串返回空串）。
func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// itoa 是 strconv.Itoa 的短别名（占位符编号用）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
