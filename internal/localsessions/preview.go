package localsessions

// -----------------------------------------------------------------------------
// 预览凭据（对照 wb-switch 的 session_link.rs 预览部分 + session.rs 的
// preview_resolved_pair / parse_selection / verify_preview）
//
// **为什么需要它**：面板的「以此为准」是「先预览、用户确认、再执行」的两段式流程。
// 两次请求之间用户可能在别处改了会话、加了成员、或客户端写入了新内容 —— 直接沿用
// 用户看到的那次判定去写，等于用**过期的结论**覆盖真实数据。
//
// 做法：预览时服务端把「判定所依赖的全部版本信息」（组结构指纹、双方成员身份、
// 双方原始正文摘要、归一化摘要、基线引用与内容、判定结论）算成一个绑定，存进
// `previews/{id}.json`，只把 **id** 返回前端。执行时：
//   - 前端拿不到绑定内容，伪造/篡改参数不能扩大权限（凭据只认服务端存的 id）；
//   - 执行前用**同一套算法**重算实时绑定并逐字段比较，任何一项变化都跳过该项
//     （reasonCode = previewStale），绝不沿用旧选择。
//
// 与 switch 的差异只有摘要口径（见 sync.go 顶部：本项目行摘要不含长度前缀）——
// 凭据只在本工具内部消费，不与 switch 互换，因此不影响用户可见行为。
// -----------------------------------------------------------------------------

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// PreviewTokenVersion 是预览凭据格式版本；读到其它版本一律视为过期。
	PreviewTokenVersion = 1
	// KeepPreviewTokens 是每档位保留的历史预览凭据条数（凭据一次性，不做长期保留）。
	KeepPreviewTokens = 200
)

// PreviewMemberBinding 是预览时记录的单个成员绑定：执行前逐项核对，任何一项变化都算过期。
type PreviewMemberBinding struct {
	MemberID  string `json:"memberId"`
	AccountID string `json:"accountId,omitempty"`
	UID       string `json:"uid"`
	SessionID string `json:"sessionId"`
	// RawDigest 是原始正文摘要（含空白）：预览之后正文有任何改动都会失配。
	RawDigest string `json:"rawDigest"`
	// NormalizedDigest 是归一化总摘要：判定的依据。
	NormalizedDigest string `json:"normalizedDigest"`
	// RecordCount 是记录数（不称消息数）。
	RecordCount int `json:"recordCount"`
}

// PreviewBinding 是预览凭据绑定的全部版本信息。
type PreviewBinding struct {
	Variant string `json:"variant"`
	GroupID string `json:"groupId"`
	// GroupFingerprint 是组结构指纹（成员身份/状态 + 配对基线引用）。
	GroupFingerprint    string               `json:"groupFingerprint"`
	Source              PreviewMemberBinding `json:"source"`
	Target              PreviewMemberBinding `json:"target"`
	BaselineRef         string               `json:"baselineRef,omitempty"`
	BaselineTotalDigest string               `json:"baselineTotalDigest,omitempty"`
	BaselineRecordCount int                  `json:"baselineRecordCount,omitempty"`
	Verdict             string               `json:"verdict"`
}

// PreviewToken 是服务端保存的预览凭据。
type PreviewToken struct {
	Version   int            `json:"version"`
	PreviewID string         `json:"previewId"`
	CreatedAt int64          `json:"createdAt"`
	Binding   PreviewBinding `json:"binding"`
}

// groupFingerprint 计算组结构指纹：成员（id/身份/状态）与配对基线引用，顺序无关。
//
// 组内任何成员替换或基线变化都会改变指纹，因此可用作预览凭据的「组版本」；
// 其它组的变化不影响本指纹，避免同一次切换里的复制误伤无关组的预览。
func groupFingerprint(g *LinkGroupR) string {
	members := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		members = append(members, m.MemberID+"|"+m.UID+"|"+m.SessionID+"|"+m.State)
	}
	sort.Strings(members)
	pairs := make([]string, 0, len(g.PairBases))
	for _, p := range g.PairBases {
		pairs = append(pairs, p.MemberIDs[0]+"|"+p.MemberIDs[1]+"|"+p.BaselineRef+"|"+itoaInt(p.NormalizationVersion))
	}
	sort.Strings(pairs)

	h := sha256.New()
	h.Write([]byte("wb-switch-group-v1\x00"))
	h.Write([]byte(g.Variant))
	h.Write([]byte{0})
	h.Write([]byte(g.ID))
	h.Write([]byte{0})
	for _, entry := range members {
		h.Write([]byte(entry))
		h.Write([]byte{0})
	}
	for _, entry := range pairs {
		h.Write([]byte(entry))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fullDigestOf 是原始字节摘要（含空白与换行）—— 与归一化摘要无关：
// 归一化只看语义，原始摘要看「一个字节都不许变」。
func fullDigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// validPreviewID 判断凭据 id 是不是本工具生成的 UUID（拒绝路径穿越等构造值）。
func validPreviewID(id string) bool {
	id = strings.TrimSpace(id)
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

func (s *Store) previewFile(previewID string) string {
	return filepath.Join(s.previewsDir(), strings.TrimSpace(previewID)+".json")
}

// savePreviewToken 保存一份预览绑定并返回凭据 id。
func (s *Store) savePreviewToken(binding PreviewBinding) (string, error) {
	previewID := mustNewUUID()
	token := PreviewToken{
		Version:   PreviewTokenVersion,
		PreviewID: previewID,
		CreatedAt: nowMillis(),
		Binding:   binding,
	}
	if err := os.MkdirAll(s.previewsDir(), 0o700); err != nil {
		return "", err
	}
	// 先清理再写入：清理按时间排序，刚保存的凭据不会被自己的清理删掉。
	s.prunePreviewTokens(KeepPreviewTokens - 1)
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return "", err
	}
	if err := durableWriteFile(s.previewFile(previewID), append(data, '\n')); err != nil {
		return "", err
	}
	return previewID, nil
}

// loadPreviewToken 读取预览凭据；id 非法、文件缺失/损坏、版本不符一律返回 nil（视为过期）。
func (s *Store) loadPreviewToken(previewID string) *PreviewToken {
	if !validPreviewID(previewID) {
		return nil
	}
	raw, err := os.ReadFile(s.previewFile(previewID))
	if err != nil {
		return nil
	}
	var token PreviewToken
	if err := json.Unmarshal(raw, &token); err != nil {
		return nil
	}
	if token.Version != PreviewTokenVersion || token.PreviewID != strings.TrimSpace(previewID) {
		return nil
	}
	return &token
}

// prunePreviewTokens 清理历史预览凭据，保留最近 keep 条。
func (s *Store) prunePreviewTokens(keep int) int {
	entries, err := os.ReadDir(s.previewsDir())
	if err != nil {
		return 0
	}
	type fileInfo struct {
		mod  int64
		path string
	}
	var files []fileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{mod: info.ModTime().UnixNano(), path: filepath.Join(s.previewsDir(), e.Name())})
	}
	if len(files) <= keep {
		return 0
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod > files[j].mod })
	removed := 0
	for _, f := range files[keep:] {
		if os.Remove(f.path) == nil {
			removed++
		}
	}
	return removed
}

// verifyPreview 核对预览凭据与实时状态，返回不一致项的说明（空表示一致，可以继续）。
//
// 逐字段比较而不是整体比较，是为了给用户「哪一项变了」的可读原因。任何不一致都必须
// 跳过该项，不得沿用用户旧选择。
func verifyPreview(preview *PreviewToken, live *PreviewBinding) []string {
	var stale []string
	if preview.Version != PreviewTokenVersion {
		stale = append(stale, "检查结果已过期，请重新检查")
	}
	expected := &preview.Binding
	if expected.Variant != live.Variant {
		stale = append(stale, "当前应用已变化")
	}
	if expected.GroupID != live.GroupID {
		stale = append(stale, "会话的关联关系已变化")
	}
	if expected.GroupFingerprint != live.GroupFingerprint {
		stale = append(stale, "会话的关联关系或同步记录已变化")
	}
	if expected.Source != live.Source {
		stale = append(stale, "来源账号的内容已变化")
	}
	if expected.Target != live.Target {
		stale = append(stale, "目标账号的内容已变化")
	}
	if expected.BaselineRef != live.BaselineRef {
		stale = append(stale, "上次同步的记录已变化")
	} else if expected.BaselineTotalDigest != live.BaselineTotalDigest ||
		expected.BaselineRecordCount != live.BaselineRecordCount {
		stale = append(stale, "上次同步的内容已变化")
	}
	if expected.Verdict != live.Verdict {
		stale = append(stale, "检查结果已变化，请重新检查")
	}
	return stale
}

// PreviewStaleReasonCode 是「预览凭据失配」的原因码（前端据此区分跳过与失败）。
const PreviewStaleReasonCode = "previewStale"

// memberSnapshot 读一份副本的原始摘要 + 归一化摘要 + 记录数。
//
// 返回的第三个值为空表示可读；非空时是「不可验证」的原因（判定侧会得到 Unknown）。
func (s *Store) memberSnapshot(lib *library, sessionID, cwd string) (rawDigest, normDigest string, count int, reason string) {
	if lib == nil || !libExists(lib) {
		return "", "", 0, "内容不存在"
	}
	path, ok := s.bodyPathIn(lib, sessionID, cwd)
	if !ok {
		return "", "", 0, "内容不存在"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", 0, "内容不存在"
	}
	norm, err := normalizeJSONL(string(data), sessionID)
	if err != nil {
		return "", "", 0, err.Error()
	}
	return fullDigestOf(data), norm.TotalDigest, norm.RecordCount, ""
}

// memberBinding 构造成员的预览绑定。
func (s *Store) memberBinding(m LinkMember) PreviewMemberBinding {
	lib := s.libByVariant(m.Variant)
	cwd := ""
	if lib != nil && libExists(lib) {
		if db, err := s.openLib(lib, true); err == nil {
			if row, _ := readSessionRow(db, m.SessionID); row != nil {
				cwd = str(row["cwd"])
			}
			db.Close()
		}
	}
	raw, normDigest, count, _ := s.memberSnapshot(lib, m.SessionID, cwd)
	return PreviewMemberBinding{
		MemberID: m.MemberID, AccountID: m.AccountID, UID: m.UID, SessionID: m.SessionID,
		RawDigest: raw, NormalizedDigest: normDigest, RecordCount: count,
	}
}

// livePreviewBinding 用**实时状态**构造绑定（预览与执行时都调用它，两边可比）。
func (s *Store) livePreviewBinding(g *LinkGroupR, srcMember, tgtMember LinkMember, verdict string) PreviewBinding {
	binding := PreviewBinding{
		Variant:          tgtMember.Variant,
		GroupID:          g.ID,
		GroupFingerprint: groupFingerprint(g),
		Source:           s.memberBinding(srcMember),
		Target:           s.memberBinding(tgtMember),
		Verdict:          verdict,
	}
	if record, state, _ := s.baselineState(g, srcMember.MemberID, tgtMember.MemberID); state == "ready" && record != nil {
		binding.BaselineRef = record.BaselineRef
		binding.BaselineTotalDigest = record.TotalDigest
		binding.BaselineRecordCount = record.RecordCount
	}
	return binding
}

// PairPreview 是一次成员对预览的结果（判定 + 服务端凭据 id）。
//
// 前端只拿得到 `preview_token`；执行时服务端据此重算并逐项复核。
type PairPreview struct {
	Verdict    string `json:"verdict"`
	Reason     string `json:"reason"`
	SourceOnly int    `json:"source_only"`
	TargetOnly int    `json:"target_only"`
	// AvailableModes 是前端可勾选的写入模式（空表示不可写入）。
	AvailableModes []string `json:"available_modes"`
	// PreviewToken 是服务端凭据 id（执行时必须原样带回）。
	PreviewToken string `json:"preview_token"`
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
