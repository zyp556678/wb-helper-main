package localsessions

// -----------------------------------------------------------------------------
// 关联登记表（对照 wb-switch 的 session_link.rs 存储部分，格式逐字段对齐）
//
// **数据模型与 switch 完全一致**：组/成员/配对基线全部登记在
// `session_links.json`（camelCase 字段、version/revision、成员状态 active/stale/
// superseded），基线本体按引用存放在 `session-links/baselines/{ref}.json`。
// 组只包含通过本工具复制/登记过的会话 —— 手动复制的副本不再自动进组
//（那是旧「内容推导分组」模型的行为，已按用户要求废弃）。
//
// 与 switch 唯一的差别是**存储根**：它写在 ~/.wb-switch/，我们写在网关工作目录的
// session-links/ 下 —— 两个工具并存时绝不能共写同一份登记表（成员 id、revision
// 各自独立，共写会互相损坏），格式本身保持逐字段一致。
//
// 读写规则（对照 with_link_store_write）：读改写全程持锁；损坏/版本不符时
// **报错而不是降级成空表覆盖**；写入前与读取后都跑不变量校验，revision 每次 +1。
// -----------------------------------------------------------------------------

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// LinkStoreVersion / NormalizationVersion 与 wb-switch 的常量一致（格式互认的前提）。
const (
	LinkStoreVersion     = 1
	NormalizationVersion = 1
)

// LinkStoreFile 是登记表主文件的内容。
type LinkStoreFile struct {
	Version  int          `json:"version"`
	Revision uint64       `json:"revision"`
	Groups   []LinkGroupR `json:"groups"`
}

// LinkGroupR 是一个逻辑会话的接力组（同一逻辑会话的各账号副本归入同一组）。
//
// 命名带 R 是为了与旧的「内容推导 Group」区分；JSON 字段与 switch 逐字段一致。
type LinkGroupR struct {
	ID        string       `json:"id"`
	Variant   string       `json:"variant"`
	CreatedAt int64        `json:"createdAt"`
	Members   []LinkMember `json:"members"`
	PairBases []PairBase   `json:"pairBases"`
}

// 成员状态：active=当前接力成员；stale=已判定失效（保留记录不自动复活）；
// superseded=已被同账号的更新成员替换（保留记录）。
const (
	MemberStateActive     = "active"
	MemberStateStale      = "stale"
	MemberStateSuperseded = "superseded"
)

// LinkMember 是组内成员：某个账号上的某一个会话副本。
type LinkMember struct {
	MemberID string `json:"memberId"`
	// AccountID 是工具内账号 id（可空：身份判定以 uid 为准）。
	AccountID string `json:"accountId,omitempty"`
	UID       string `json:"uid"`
	SessionID string `json:"sessionId"`
	// Variant 是该副本所属档位（跨档组成员各自不同）。
	Variant      string `json:"variant"`
	State        string `json:"state"`
	LinkedAt     int64  `json:"linkedAt"`
	LastSyncedAt *int64 `json:"lastSyncedAt,omitempty"`
}

// PairBase 是配对基线引用（成员对无序存储，同一对至多一条）。
type PairBase struct {
	MemberIDs            [2]string `json:"memberIds"`
	BaselineRef          string    `json:"baselineRef"`
	NormalizationVersion int       `json:"normalizationVersion"`
}

// BaselineRecord 是基线本体：有序归一化行摘要 + 总摘要 + 记录数。
type BaselineRecord struct {
	Version              int      `json:"version"`
	BaselineRef          string   `json:"baselineRef"`
	NormalizationVersion int      `json:"normalizationVersion"`
	CreatedAt            int64    `json:"createdAt"`
	RecordCount          int      `json:"recordCount"`
	TotalDigest          string   `json:"totalDigest"`
	LineDigests          []string `json:"lineDigests"`
}

// 自洽性：版本、记录数、总摘要都能从行摘要重算出来。
func (r *BaselineRecord) selfConsistent() bool {
	if r.NormalizationVersion != NormalizationVersion || r.RecordCount != len(r.LineDigests) {
		return false
	}
	return totalDigestOf(r.LineDigests) == r.TotalDigest
}

// -----------------------------------------------------------------------------
// 存储路径
// -----------------------------------------------------------------------------

// Namespace 决定登记表与关联目录的落点（对照 switch 的 LinkNamespace）。
//
// **三个客户端各一份、互不可见**：WorkBuddy 客户端、VS Code 插件、CodeBuddy IDE
// 的会话形态完全不同（行/消息/整目录），把它们的成员登记在同一张表里会让
// 「同一会话的身份唯一性」校验互相打架。文件命名与 switch 逐字一致。
type Namespace string

const (
	NamespaceWorkBuddy    Namespace = "workbuddy"
	NamespaceVscodeExt    Namespace = "vscode-ext"
	NamespaceCodeBuddyIDE Namespace = "codebuddy-ide"
)

// linkRoot 返回该命名空间的关联目录（基线/操作日志/预览凭据的父目录）。
func (s *Store) linkRoot() string {
	switch s.Namespace {
	case NamespaceVscodeExt:
		return filepath.Join(s.StateDir, "vscode-session-links")
	case NamespaceCodeBuddyIDE:
		return filepath.Join(s.StateDir, "codebuddy-ide-session-links")
	default:
		return filepath.Join(s.StateDir, "session-links")
	}
}

// linkFile 返回该命名空间的主表文件名。
func (s *Store) linkFile() string {
	name := "session_links.json"
	switch s.Namespace {
	case NamespaceVscodeExt:
		name = "vscode_session_links.json"
	case NamespaceCodeBuddyIDE:
		name = "codebuddy_ide_session_links.json"
	}
	return filepath.Join(s.StateDir, name)
}

func (s *Store) baselinesDir() string  { return filepath.Join(s.linkRoot(), "baselines") }
func (s *Store) operationsDir() string { return filepath.Join(s.linkRoot(), "operations") }
func (s *Store) previewsDir() string   { return filepath.Join(s.linkRoot(), "previews") }

// hasUnfinishedTraces 判断是否存在未完成操作的痕迹（对照 switch 的同名函数）：
// 基线文件同样算痕迹（基线代表既有的关联关系，主文件缺失只能是异常现场），
// 操作日志里只有**未完成或无法解析**的才算。
func (s *Store) hasUnfinishedTraces() bool {
	if entries, err := os.ReadDir(s.baselinesDir()); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				return true
			}
		}
	}
	entries, err := os.ReadDir(s.operationsDir())
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.operationsDir(), e.Name()))
		if err != nil {
			// 读不了的残片同样算痕迹：不能当成空表覆盖。
			return true
		}
		var op Operation
		if err := json.Unmarshal(raw, &op); err != nil {
			return true
		}
		if op.Phase.Unfinished() {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// 读取 / 写入（对照 load_store / with_link_store_write）
// -----------------------------------------------------------------------------

// errLinkUnavailable 表示登记表不可用（损坏/版本不符/主文件缺失但有残留）。
// 调用方必须把这个原因原样告诉用户，**不得**降级成空表继续写。
var errLinkUnavailable = errors.New("会话关联记录不可用")

func linkUnavailable(reason string) error { return fmt.Errorf("%w：%s", errLinkUnavailable, reason) }

// loadLinkStore 读取登记表；区分首次缺失与不可用。
func (s *Store) loadLinkStore() (*LinkStoreFile, bool, error) {
	if strings.TrimSpace(s.StateDir) == "" {
		// 没有状态目录时**必须拒绝**：空 StateDir 会让路径退化成相对路径
		// （写到进程 CWD），那是「看起来能用、实际把登记表扔在随机目录」的隐性错误。
		return nil, false, linkUnavailable("未配置状态目录，会话关联功能不可用")
	}
	raw, err := os.ReadFile(s.linkFile())
	if err != nil {
		if os.IsNotExist(err) {
			if s.hasUnfinishedTraces() {
				return nil, false, linkUnavailable("同步记录主文件缺失但存在未完成的痕迹（操作记录），已保留现场")
			}
			return &LinkStoreFile{Version: LinkStoreVersion, Groups: []LinkGroupR{}}, false, nil
		}
		return nil, false, linkUnavailable("同步记录无法读取：" + err.Error())
	}
	var store LinkStoreFile
	if err := json.Unmarshal(raw, &store); err != nil {
		return nil, false, linkUnavailable("同步记录已损坏，原文件已保留")
	}
	if store.Version != LinkStoreVersion {
		return nil, false, linkUnavailable(fmt.Sprintf("同步记录版本 %d 不受支持（当前支持 %d），原文件已保留", store.Version, LinkStoreVersion))
	}
	backfillMemberVariants(&store)
	if reason := validateLinkStore(&store); reason != "" {
		return nil, false, linkUnavailable("同步记录内容不一致：" + reason)
	}
	return &store, true, nil
}

// 旧存储兼容：成员缺 variant 时用组级 variant 回填。
func backfillMemberVariants(store *LinkStoreFile) {
	for gi := range store.Groups {
		for mi := range store.Groups[gi].Members {
			if store.Groups[gi].Members[mi].Variant == "" {
				store.Groups[gi].Members[mi].Variant = store.Groups[gi].Variant
			}
		}
	}
}

// validateLinkStore 是不变量校验（写入前与读取后都执行），返回空串表示通过：
//   - 组 id 唯一；
//   - 同一 (variant, uid, sessionId) 只属于一个组；
//   - 每组每个账号至多一个 active 成员；
//   - 配对基线双方都在组内且不重复。
func validateLinkStore(store *LinkStoreFile) string {
	groupIDs := map[string]bool{}
	identities := map[string]bool{}
	for _, group := range store.Groups {
		if groupIDs[group.ID] {
			return fmt.Sprintf("组记录重复：%s", group.ID)
		}
		groupIDs[group.ID] = true
		memberIDs := map[string]bool{}
		activeByUID := map[string]bool{}
		for _, member := range group.Members {
			if memberIDs[member.MemberID] {
				return fmt.Sprintf("成员记录重复：%s", member.MemberID)
			}
			memberIDs[member.MemberID] = true
			identity := member.Variant + "\x00" + member.UID + "\x00" + member.SessionID
			if identities[identity] {
				return fmt.Sprintf("会话 %s 同时属于多个同步组", member.SessionID)
			}
			identities[identity] = true
			if member.State == MemberStateActive && activeByUID[member.UID] {
				return fmt.Sprintf("账号 %s 在同一组内出现多条有效成员记录", member.UID)
			}
		}
		pairs := map[string]bool{}
		for _, pair := range group.PairBases {
			id := pairID(pair.MemberIDs[0], pair.MemberIDs[1])
			if pairs[id] {
				return fmt.Sprintf("同一对成员重复保存了同步记录：%s", pair.BaselineRef)
			}
			pairs[id] = true
			for _, id := range pair.MemberIDs {
				if !memberIDs[id] {
					return fmt.Sprintf("同步记录引用了不存在的成员：%s", id)
				}
			}
		}
	}
	return ""
}

// pairKey 是规范化的成员对 key（无序成员对唯一）。
func pairKey(a, b string) (string, string) {
	if a <= b {
		return a, b
	}
	return b, a
}

// pairID 是成员对的唯一键（无序）。
func pairID(a, b string) string {
	k0, k1 := pairKey(a, b)
	return k0 + "\x00" + k1
}

var linkWriteMu sync.Mutex

// withLinkStoreWrite 是关联存储的读改写：锁内重读、修改、校验、原子写回并递增
// revision。存储不可用（损坏/未知版本）时直接失败，绝不降级成空表覆盖。
func (s *Store) withLinkStoreWrite(mutate func(store *LinkStoreFile) error) error {
	linkWriteMu.Lock()
	defer linkWriteMu.Unlock()
	store, existed, err := s.loadLinkStore()
	if err != nil {
		return err
	}
	if err := mutate(store); err != nil {
		return err
	}
	if reason := validateLinkStore(store); reason != "" {
		return fmt.Errorf("拒绝保存不一致的同步记录：%s", reason)
	}
	store.Version = LinkStoreVersion
	store.Revision++
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.linkRoot(), 0o700); err != nil {
		return err
	}
	tmp := s.linkFile() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		if !existed {
			return fmt.Errorf("同步记录首次保存失败：%w", err)
		}
		return fmt.Errorf("同步记录保存失败：%w", err)
	}
	if err := os.Rename(tmp, s.linkFile()); err != nil {
		if !existed {
			return fmt.Errorf("同步记录首次保存失败：%w", err)
		}
		return fmt.Errorf("同步记录保存失败：%w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// 组/成员/配对的操作原语（都在 withLinkStoreWrite 的 mutate 闭包里使用）
// -----------------------------------------------------------------------------

// findGroupForIdentity 按 (variant, uid, sessionId) 找所属组（身份唯一归组）。
func findGroupForIdentity(store *LinkStoreFile, variant, uid, sessionID string) *LinkGroupR {
	for gi := range store.Groups {
		g := &store.Groups[gi]
		for _, m := range g.Members {
			if m.Variant == variant && m.UID == uid && m.SessionID == sessionID {
				return g
			}
		}
	}
	return nil
}

// findMember 按 (uid, sessionId) 在组内找成员。
func findMember(g *LinkGroupR, uid, sessionID string) *LinkMember {
	for i := range g.Members {
		if g.Members[i].UID == uid && g.Members[i].SessionID == sessionID {
			return &g.Members[i]
		}
	}
	return nil
}

// addActiveMember 追加 active 成员；同账号已有 active 成员时显式转 superseded
// （保留记录，不自动复活）。
func addActiveMember(g *LinkGroupR, member LinkMember) {
	for i := range g.Members {
		if g.Members[i].UID == member.UID && g.Members[i].State == MemberStateActive {
			g.Members[i].State = MemberStateSuperseded
		}
	}
	g.Members = append(g.Members, member)
}

// findPairBase 取成员对的基线引用。
func findPairBase(g *LinkGroupR, a, b string) *PairBase {
	k0, k1 := pairKey(a, b)
	for i := range g.PairBases {
		if g.PairBases[i].MemberIDs[0] == k0 && g.PairBases[i].MemberIDs[1] == k1 {
			return &g.PairBases[i]
		}
	}
	return nil
}

// setPairBase 写成员对基线（已存在则覆盖引用）。
func setPairBase(g *LinkGroupR, a, b, ref string) {
	k0, k1 := pairKey(a, b)
	for i := range g.PairBases {
		if g.PairBases[i].MemberIDs[0] == k0 && g.PairBases[i].MemberIDs[1] == k1 {
			g.PairBases[i].BaselineRef = ref
			g.PairBases[i].NormalizationVersion = NormalizationVersion
			return
		}
	}
	g.PairBases = append(g.PairBases, PairBase{
		MemberIDs:            [2]string{k0, k1},
		BaselineRef:          ref,
		NormalizationVersion: NormalizationVersion,
	})
}

// removeMemberFromGroup 从组内删除成员及其全部配对基线（对照 remove_member）。
// 返回删除后组内剩余成员数。
func removeMemberFromGroup(g *LinkGroupR, memberID string) int {
	kept := g.Members[:0]
	for _, m := range g.Members {
		if m.MemberID != memberID {
			kept = append(kept, m)
		}
	}
	g.Members = kept
	bases := g.PairBases[:0]
	for _, p := range g.PairBases {
		if p.MemberIDs[0] != memberID && p.MemberIDs[1] != memberID {
			bases = append(bases, p)
		}
	}
	g.PairBases = bases
	return len(g.Members)
}

// -----------------------------------------------------------------------------
// 基线文件（baselines/{ref}.json）
// -----------------------------------------------------------------------------

// saveBaselineRecord 把基线本体写成按引用寻址的文件。
func (s *Store) saveBaselineRecord(record *BaselineRecord) error {
	if s.StateDir == "" {
		return fmt.Errorf("未配置状态目录")
	}
	if err := os.MkdirAll(s.baselinesDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.baselinesDir(), record.BaselineRef+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// baselineState 读取成员对的基线：Ready / Missing / Unverifiable（对照 BaselineState）。
// 成员对没有登记引用 → Missing；有引用但文件缺失/损坏/版本不符 → Unverifiable。
func (s *Store) baselineState(g *LinkGroupR, memberA, memberB string) (*BaselineRecord, string, string) {
	pair := findPairBase(g, memberA, memberB)
	if pair == nil {
		return nil, "missing", "找不到双方上次一致的内容，暂时无法同步"
	}
	if pair.NormalizationVersion != NormalizationVersion {
		return nil, "unverifiable", fmt.Sprintf("上次一致的内容不可用（归一化版本 %d 不受支持），暂时无法同步", pair.NormalizationVersion)
	}
	raw, err := os.ReadFile(filepath.Join(s.baselinesDir(), pair.BaselineRef+".json"))
	if err != nil {
		return nil, "unverifiable", fmt.Sprintf("上次一致的内容不可用（记录文件缺失），暂时无法同步")
	}
	var record BaselineRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, "unverifiable", "上次一致的内容不可用（记录损坏），暂时无法同步"
	}
	if !record.selfConsistent() {
		return nil, "unverifiable", "上次一致的内容不可用（记录自洽性校验失败），暂时无法同步"
	}
	return &record, "ready", ""
}

// inheritableBaseline 判断旧配对基线能否被新成员继承：
// 基线自洽、归一化版本一致，且**基线内容被新成员正文（有序前缀）包含**。
func (s *Store) inheritableBaseline(pair PairBase, newDigests []string) *BaselineRecord {
	raw, err := os.ReadFile(filepath.Join(s.baselinesDir(), pair.BaselineRef+".json"))
	if err != nil {
		return nil
	}
	var record BaselineRecord
	if json.Unmarshal(raw, &record) != nil || !record.selfConsistent() {
		return nil
	}
	if !isStrictOrderedExtension(newDigests, record.LineDigests) {
		return nil
	}
	return &record
}

// nowMillis 取当前毫秒时间戳（与客户端会话库的时间戳单位一致）。
func nowMillis() int64 { return time.Now().UnixMilli() }

// sortPairBases 让配对基线顺序稳定（测试与 diff 友好）。
func sortPairBases(g *LinkGroupR) {
	sort.Slice(g.PairBases, func(i, j int) bool {
		a, b := g.PairBases[i].MemberIDs, g.PairBases[j].MemberIDs
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		return a[1] < b[1]
	})
}

// stringsTrimSpaceAll 去除切片每项空白（小工具，避免在调用点重复写循环）。
func stringsTrimSpaceAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, strings.TrimSpace(v))
	}
	return out
}
