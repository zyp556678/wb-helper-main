package localsessions

// -----------------------------------------------------------------------------
// 登记表内核的**对外**接口（供其它会话形态复用：VS Code 插件 / CodeBuddy IDE）
//
// 三种客户端的会话形态完全不同（JSONL 行 / 一条消息一个文件），但**登记表与判定语义
// 必须是同一套**：组/成员/配对基线/预览凭据的格式、判定的分支顺序与文案，
// 三处各写一份必然会漂移（「同样的两个副本，在 A 页说可以快进、在 B 页说分叉」）。
//
// 所以这里只把**内核**暴露出去：登记表读写、组/成员/基线原语、预览凭据、判定表。
// 内容层（怎么读一条记录、怎么算摘要）由各客户端自己实现 —— 那是它们真正的差异。
// -----------------------------------------------------------------------------

// ContentFacts 是一份内容的判定输入（不绑定任何存储形态）。
//
// 与内部的 contentState 一一对应，只是把字段暴露出来供其它包构造。
type ContentFacts struct {
	// Missing 表示内容不存在（正文/会话目录缺失）。
	Missing bool
	// Unavailable 非空表示读到了但无法验证（损坏、消息文件缺失…）——判定必须按 Unknown。
	Unavailable string
	// Digests 是有序记录摘要（判定与快进的依据）。
	Digests []string
	// RecordCount 是记录数（只用于文案里的「各 N 条」）。
	RecordCount int
}

// FactsFromNormalized 由归一化内容构造判定输入。
func FactsFromNormalized(n *NormalizedContent) ContentFacts {
	if n == nil {
		return ContentFacts{Missing: true}
	}
	return ContentFacts{Digests: n.LineDigests, RecordCount: n.RecordCount}
}

// FactsMissing / FactsUnavailable 是两个非 Ready 形态的构造器。
func FactsMissing() ContentFacts { return ContentFacts{Missing: true} }
func FactsUnavailable(reason string) ContentFacts {
	return ContentFacts{Unavailable: reason}
}

// DecideSyncFacts 是判定表的**唯一实现**（ContentFacts 版）。
//
// 分支顺序不可调换（与 switch 的 decide_sync 逐条对应，详见 decide.go 顶部）。
func DecideSyncFacts(source, target ContentFacts, baseline *BaselineRecord, baselineState, baselineUnusable string) SyncDecision2 {
	return decideSync2(
		contentState{ready: factsToNormalized(source), miss: source.Missing, reason: source.Unavailable},
		contentState{ready: factsToNormalized(target), miss: target.Missing, reason: target.Unavailable},
		baseline, baselineState, baselineUnusable,
	)
}

// factsToNormalized 把判定输入还原成内部形态（空内容返回 nil）。
func factsToNormalized(f ContentFacts) *NormalizedContent {
	if f.Missing || f.Unavailable != "" || f.Digests == nil {
		return nil
	}
	return &NormalizedContent{RecordCount: f.RecordCount, LineDigests: f.Digests, TotalDigest: totalDigestOf(f.Digests)}
}

// -----------------------------------------------------------------------------
// 登记表内核（读改写 / 组与成员原语 / 基线）
// -----------------------------------------------------------------------------

// LoadLinkStore 读取登记表（区分首次缺失与不可用）。
//
// 返回的 bool 表示主文件**是否已存在**（false 且 err==nil 表示首次使用）。
func (s *Store) LoadLinkStore() (*LinkStoreFile, bool, error) { return s.loadLinkStore() }

// WithLinkStoreWrite 是登记表的读改写（锁内重读、校验、原子写回、revision+1）。
func (s *Store) WithLinkStoreWrite(mutate func(store *LinkStoreFile) error) error {
	return s.withLinkStoreWrite(mutate)
}

// EnsureLinkStoreReady 保证主文件已存在（首次使用时先落地空表）。
func (s *Store) EnsureLinkStoreReady() error { return s.ensureLinkStoreReady() }

// FindGroupForIdentity 按 (variant, uid, sessionId) 找所属组。
func FindGroupForIdentity(store *LinkStoreFile, variant, uid, sessionID string) *LinkGroupR {
	return findGroupForIdentity(store, variant, uid, sessionID)
}

// FindGroupByID 按组 id 找组。
func FindGroupByID(store *LinkStoreFile, groupID string) *LinkGroupR {
	return findGroupR(store, groupID)
}

// FindMemberInGroup 按 (uid, sessionId) 在组内找成员。
func FindMemberInGroup(g *LinkGroupR, uid, sessionID string) *LinkMember {
	return findMember(g, uid, sessionID)
}

// ActiveMemberForUID 取组内该账号的 active 成员（没有则 nil）。
func ActiveMemberForUID(g *LinkGroupR, uid string) *LinkMember { return activeMemberForUID(g, uid) }

// AddActiveMember 追加 active 成员；同账号已有 active 成员时显式转 superseded。
func AddActiveMember(g *LinkGroupR, member LinkMember) { addActiveMember(g, member) }

// RemoveMemberFromGroup 从组内删除成员及其全部配对基线，返回剩余成员数。
func RemoveMemberFromGroup(g *LinkGroupR, memberID string) int {
	return removeMemberFromGroup(g, memberID)
}

// FindPairBase 取成员对的基线引用。
func FindPairBase(g *LinkGroupR, a, b string) *PairBase { return findPairBase(g, a, b) }

// SetPairBase 写成员对基线（已存在则覆盖引用）。
func SetPairBase(g *LinkGroupR, a, b, ref string) { setPairBase(g, a, b, ref) }

// SortPairBases 让配对基线顺序稳定。
func SortPairBases(g *LinkGroupR) { sortPairBases(g) }

// SaveBaselineRecord 把基线本体写成按引用寻址的文件。
func (s *Store) SaveBaselineRecord(record *BaselineRecord) error { return s.saveBaselineRecord(record) }

// BaselineState 读取成员对的基线（Ready/Missing/Unverifiable 三态 + 不可用原因）。
func (s *Store) BaselineState(g *LinkGroupR, memberA, memberB string) (*BaselineRecord, string, string) {
	return s.baselineState(g, memberA, memberB)
}

// InheritableBaseline 判断旧配对基线能否被新成员继承。
func (s *Store) InheritableBaseline(pair PairBase, newDigests []string) *BaselineRecord {
	return s.inheritableBaseline(pair, newDigests)
}

// NowMillis 取当前毫秒时间戳（各客户端写登记表时统一用它）。
func NowMillis() int64 { return nowMillis() }

// NewUUID 生成 UUID v4（成员 id / 组 id / 基线引用）。
func NewUUID() (string, error) { return newUUID() }

// MustNewUUID 生成 UUID v4，失败直接 panic（没有它就无法登记）。
func MustNewUUID() string { return mustNewUUID() }

// -----------------------------------------------------------------------------
// 预览凭据（版本绑定）
// -----------------------------------------------------------------------------

// GroupFingerprint 计算组结构指纹（预览凭据的「组版本」）。
func GroupFingerprint(g *LinkGroupR) string { return groupFingerprint(g) }

// SavePreviewToken 保存一份预览绑定并返回凭据 id。
func (s *Store) SavePreviewToken(binding PreviewBinding) (string, error) {
	return s.savePreviewToken(binding)
}

// LoadPreviewToken 读取预览凭据（非法 id / 缺失 / 损坏 / 版本不符一律 nil）。
func (s *Store) LoadPreviewToken(previewID string) *PreviewToken {
	return s.loadPreviewToken(previewID)
}

// VerifyPreview 核对预览凭据与实时状态，返回不一致项的说明（空表示一致）。
func VerifyPreview(preview *PreviewToken, live *PreviewBinding) []string {
	return verifyPreview(preview, live)
}

// MemberSnapshot 读一份副本的原始摘要 + 归一化摘要 + 记录数（预览绑定用）。
//
// 各客户端的内容形态不同，但绑定需要的三个值相同，所以由调用方传入。
func NewPreviewMemberBinding(memberID, accountID, uid, sessionID, rawDigest, normalizedDigest string, recordCount int) PreviewMemberBinding {
	return PreviewMemberBinding{
		MemberID: memberID, AccountID: accountID, UID: uid, SessionID: sessionID,
		RawDigest: rawDigest, NormalizedDigest: normalizedDigest, RecordCount: recordCount,
	}
}

// NewPreviewBinding 构造一份预览绑定（字段与 switch 的 PreviewBinding 一致）。
func NewPreviewBinding(variant, groupID, fingerprint string, source, target PreviewMemberBinding, baselineRef, baselineTotalDigest string, baselineRecordCount int, verdict string) PreviewBinding {
	return PreviewBinding{
		Variant: variant, GroupID: groupID, GroupFingerprint: fingerprint,
		Source: source, Target: target,
		BaselineRef: baselineRef, BaselineTotalDigest: baselineTotalDigest,
		BaselineRecordCount: baselineRecordCount, Verdict: verdict,
	}
}

// FullDigestOf 是原始字节摘要（预览绑定的版本绑定用）。
func FullDigestOf(data []byte) string { return fullDigestOf(data) }
