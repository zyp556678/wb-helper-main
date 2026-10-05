package localsessions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// -----------------------------------------------------------------------------
// 会话内容归一化与同步判定
//
// 移植自 wb-switch 的 `session_link.rs`（内容层）。核心思想：
//
//   两条「同源」的会话（同一段对话复制到两个账号）**只有 sessionId 不同**。
//   所以把各自的 sessionId 都替换成同一个占位符后逐行取摘要，
//   两边的**有序行摘要序列**就能直接比较：
//     - 完全相同            → Identical
//     - 目标 ⊂ 来源（有序前缀）→ FastForward（纯追加，零覆盖，最安全）
//     - 来源 = 上次同步基线   → Ahead（只有目标新增，不该往回同步）
//     - 其余                → Diverge（两边都有改动，必须显式确认）
//
// **为什么用有序行摘要而不是整文件哈希**：整文件哈希只能回答「一样/不一样」，
// 无法区分「目标只是少了尾部几条」（可以安全追加）与「两边各改了一半」
// （覆盖会丢数据）。而这两个场景对用户的后果完全不同。
// -----------------------------------------------------------------------------

// sessionIDMarker 是归一化时用来替换各自 sessionId 的占位符。
//
// 取值与 wb-switch 一致：**不要改**。改了两边算出的摘要就不再可比，
// 已有基线全部失效（表现为「明明同源却报分叉」）。
const sessionIDMarker = "__wb_switch_session_id__"

// NormalizedContent 是一份正文的归一化结果。
type NormalizedContent struct {
	RecordCount int      `json:"record_count"`
	LineDigests []string `json:"-"`
	TotalDigest string   `json:"total_digest"`
}

// normalizeJSONL 逐行归一化并取摘要。
//
// 规则（与 wb-switch 一致）：
//   - 末尾的空行不算记录（文件以换行结尾是常态）
//   - 中间的空行是**错误**：内容可能不完整，宁可拒绝也不要按「少一条」处理
//   - 每行必须是合法 JSON，否则拒绝（同上）
//   - 行内把**本会话的 id** 替换成占位符后再取摘要
func normalizeJSONL(text, ownSessionID string) (*NormalizedContent, error) {
	ownSessionID = strings.TrimSpace(ownSessionID)
	if ownSessionID == "" {
		return nil, fmt.Errorf("缺少会话 id，无法读取内容")
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("内容为空")
	}

	lines := strings.Split(text, "\n")
	// 末尾换行（含多个）不算记录。
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}

	digests := make([]string, 0, len(lines))
	for i, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if strings.TrimSpace(line) == "" {
			return nil, fmt.Errorf("第 %d 行为空，内容可能不完整", i+1)
		}
		if !json.Valid([]byte(line)) {
			return nil, fmt.Errorf("第 %d 行的格式无法识别，内容可能不完整", i+1)
		}
		// **只替换 sessionId 字段的值，不做朴素全串替换。**
		//
		// wb-switch 用的是 `line.replace(own_session_id, MARKER)`（全串替换）。
		// 真实会话 id 是 36 字符 UUID，全串替换几乎不会误伤；但这是**运气好**
		// 而不是设计好 —— id 短一点（比如测试里的 `s`）就会把
		// `"type":"session-meta"` 里的字母也换掉，于是**两份副本反而不可比**，
		// 整个同步机制的前提就不成立了。
		//
		// 这里用与复制路径同一套精准替换：只认**未被反斜杠转义**的
		// `"sessionId":"<id>"`，与 id 长短无关。
		digests = append(digests, lineDigestOf(replaceSessionIDInLine(line, ownSessionID, sessionIDMarker)))
	}

	return &NormalizedContent{
		RecordCount: len(digests),
		LineDigests: digests,
		TotalDigest: totalDigestOf(digests),
	}, nil
}

// lineDigestOf 是单行摘要（SHA-256 十六进制）。
func lineDigestOf(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])
}

// totalDigestOf 由有序行摘要推导总摘要。
//
// **带长度前缀与固定域分隔串**：否则 [ab, c] 与 [a, bc] 这类拼接歧义会算出同一摘要。
// 前缀（而非整表）也能自洽重算，便于校验基线是否被篡改。
func totalDigestOf(digests []string) string {
	h := sha256.New()
	h.Write([]byte("wb-switch-lines-v1\x00"))
	// 长度用 8 字节大端（与 wb-switch 一致，保证跨实现可比）。
	n := uint64(len(digests))
	var lenBuf [8]byte
	for i := 7; i >= 0; i-- {
		lenBuf[i] = byte(n)
		n >>= 8
	}
	h.Write(lenBuf[:])
	for _, d := range digests {
		h.Write([]byte(d))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// isOrderedPrefix 判断 prefix 是否为 full 的有序前缀。
func isOrderedPrefix(prefix, full []string) bool {
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

// isStrictOrderedExtension 判断 prefix 是否**严格**是 full 的有序前缀（确实更短）。
//
// 「严格更短」是快进的前提之一：等长且前缀成立就是「相同」，那是 Identical 的事。
func isStrictOrderedExtension(prefix, full []string) bool {
	return len(prefix) < len(full) && isOrderedPrefix(prefix, full)
}
func equalDigests(a, b []string) bool {
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

// countExclusive 数两边各有多少条是对方没有的（多重集口径）。
//
// 用于解释差异规模，**不参与自动判定** —— 行级摘要无法表达「顺序」，
// 拿它做「能自动合并多少条」的依据会得出错误结论。
func countExclusive(source, target []string) (sourceOnly, targetOnly int) {
	sc := map[string]int{}
	for _, d := range source {
		sc[d]++
	}
	tc := map[string]int{}
	for _, d := range target {
		tc[d]++
	}
	for d, n := range sc {
		if m := tc[d]; n > m {
			sourceOnly += n - m
		}
	}
	for d, n := range tc {
		if m := sc[d]; n > m {
			targetOnly += n - m
		}
	}
	return sourceOnly, targetOnly
}

// -----------------------------------------------------------------------------
// 基线与同步执行
// -----------------------------------------------------------------------------

func (s *Store) sessionCwd(id string) (string, error) {
	for i := range s.Libraries {
		lib := &s.Libraries[i]
		if !libExists(lib) {
			continue
		}
		db, err := s.openLib(lib, true)
		if err != nil {
			return "", err
		}
		var cwd string
		err = db.QueryRow(`SELECT COALESCE(cwd,'') FROM sessions WHERE id = ?`, id).Scan(&cwd)
		db.Close()
		if err == nil {
			return cwd, nil
		}
	}
	return "", fmt.Errorf("会话 %s 不存在", id)
}

// replaceSessionIDInLine 把行内 `"sessionId":"<id>"` 的值替换成 marker。
//
// 复用 `replaceSessionIDBytes` 的判据（只认未被反斜杠转义的字段名），
// 所以与 id 的长短无关，也不会误伤消息正文里出现的同一串文本。
func replaceSessionIDInLine(line, ownID, marker string) string {
	out, _ := replaceSessionIDBytes([]byte(line), ownID, marker)
	return string(out)
}
