package localsessions

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// -----------------------------------------------------------------------------
// 关联会话：把「同一段对话的多份副本」聚成一组
//
// 这是切片 16 那套摘要机制的直接应用：
//   - 两份副本只有 sessionId 不同 → 归一化后行摘要完全相同；
//   - 一份是另一份的**有序前缀** → 落后的那份可安全追加同步（fast-forward）；
//   - 互为前缀都不成立 → 两边各自改过（diverge，必须人工确认）。
//
// 所以**分组不需要额外的登记表** —— 「是不是同一组」由内容本身决定。
// 这一点很重要：靠登记表的话，用户**手动**复制出来的副本
//（不经过本工具）就永远进不了组。
// -----------------------------------------------------------------------------

// GroupStatus 是一组的同步状态。
// -----------------------------------------------------------------------------
// 组视图（登记表聚合结果；状态取值与 wb-switch 一致）
// -----------------------------------------------------------------------------

// 组状态（对照 switch 的 summary_status；筛选 tab 用同一套值）。
const (
	GroupStatusLatest  = "latest"
	GroupStatusBehind  = "behind"
	GroupStatusDiverge = "diverge"
	GroupStatusMissing = "missing"
	GroupStatusUnknown = "unknown"
)

// AccountLabel 是 uid → 账号的展示映射（昵称/账号 id/档位）。
type AccountLabel struct {
	Account string
	Label   string
	// Site 是账号档位（cn/intl）。用于「会话所在库 vs 账号档位」的一致性校验：
	// 国际站账号的副本只可能在国际站的库里，出现在国内站库里的都是旧版本
	// 「复制写错库」留下的幻影，必须排除（见 loadSessionItems）。
	Site string
}

// GroupDivergence 是分叉组的形状描述：共同旧版 + 若干独立更新。
type GroupDivergence struct {
	// CommonMemberIDs 是内容为共同旧版的**成员 id**。
	CommonMemberIDs []string `json:"common_member_ids"`
	// BranchMemberIDs 是每条独立更新的成员（互不为前缀的版本各一条分支）。
	BranchMemberIDs [][]string `json:"branch_member_ids"`
	// Branches 是分支数（= len(BranchMemberIDs)）。
	Branches int `json:"branches"`
}

// GroupCounts 是各状态的组数（筛选段用）。
type GroupCounts struct {
	All     int `json:"all"`
	Behind  int `json:"behind"`
	Diverge int `json:"diverge"`
	Latest  int `json:"latest"`
	Missing int `json:"missing"`
	Unknown int `json:"unknown"`
}

type sessionItem struct {
	sess   Session
	norm   *NormalizedContent
	errMsg string
	// unlinked 表示这条会话已被用户「解除关联」，不再参与分组（见 unlink.go）。
	unlinked bool
	// variant 是这条会话所在会话库的档位（cn/intl）。
	variant string
}

// -----------------------------------------------------------------------------
// 摘要缓存
// -----------------------------------------------------------------------------

// digestCache 按 (size, mtime) 缓存归一化结果。
//
// **为什么必须缓存**：归一化要读整份正文（实测有 77MB 的会话），
// 每次请求重算是不可接受的。失效判据用 **size+mtime 而不是只按 id** ——
// 会话在客户端里继续写时二者会变，只按 id 缓存会一直返回旧摘要，
// 表现为「刚聊完，状态还是旧的」。
//
// **只放内存、不落盘**：落盘要存全量行摘要（19 个会话约 3MB），
// 而收益只是「进程重启后第一次请求快一点」。用 3MB 常驻状态文件
// 换一次 1~3 秒的读盘，不划算。
type digestCache struct {
	mu      sync.Mutex
	entries map[string]digestEntry
}

type digestEntry struct {
	size  int64
	mtime int64
	norm  *NormalizedContent
	err   string
}

var globalDigestCache = &digestCache{entries: map[string]digestEntry{}}

// normalizeCached 带缓存的归一化。size/mtime 与缓存一致时直接命中。
func (c *digestCache) normalize(id, path string, size, mtime int64) (*NormalizedContent, error) {
	c.mu.Lock()
	if e, ok := c.entries[id]; ok && e.size == size && e.mtime == mtime {
		c.mu.Unlock()
		if e.err != "" {
			return nil, stringError(e.err)
		}
		return e.norm, nil
	}
	c.mu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		c.store(id, size, mtime, nil, err.Error())
		return nil, err
	}
	norm, err := normalizeJSONL(string(raw), id)
	if err != nil {
		c.store(id, size, mtime, nil, err.Error())
		return nil, err
	}
	c.store(id, size, mtime, norm, "")
	return norm, nil
}

func (c *digestCache) store(id string, size, mtime int64, norm *NormalizedContent, errMsg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 简单上限：会话数不会很大（实测 19 个），超过就整体清空重来 ——
	// 做 LRU 的复杂度换不回收益。
	if len(c.entries) > 512 {
		c.entries = map[string]digestEntry{}
	}
	c.entries[id] = digestEntry{size: size, mtime: mtime, norm: norm, err: errMsg}
}

// -----------------------------------------------------------------------------
// 分组
// -----------------------------------------------------------------------------

// loadSessionItems 读取全部会话（**聚合国内站与国际站两个库**）并归一化
// （带摘要缓存），返回不可读条数与「库↔档位不符」的幻影副本条数。
//
// **幻影副本必须排除**：国际站账号的会话只可能在国际站的库里；在国内站库里
// 出现的国际站 uid 会话，是旧版本「复制写进错库」留下的脏数据 —— 该客户端
// 永远不会显示它，把它算进组会让面板显示「内容一致」而 switch 显示「待同步」
// （2026-10-05 实测踩过）。uid 不在账号池里时无从判断，保留（宁可多看不可错杀）。
func (s *Store) loadSessionItems(uidToAccount map[string]AccountLabel) ([]sessionItem, int, int, error) {
	sessions, err := s.ListSessions("", 0)
	if err != nil {
		return nil, 0, 0, err
	}
	items := make([]sessionItem, 0, len(sessions))
	unreadable, mismatched := 0, 0
	for _, sess := range sessions {
		if lbl, ok := uidToAccount[sess.UserID]; ok && lbl.Site != "" && sess.Variant != "" && lbl.Site != sess.Variant {
			mismatched++
			continue
		}
		it := sessionItem{sess: sess, variant: sess.Variant}
		path, ok := s.bodyPathIn(s.libByVariant(sess.Variant), sess.ID, sess.Cwd)
		if !ok {
			it.errMsg = "正文文件不存在"
			unreadable++
			items = append(items, it)
			continue
		}
		mtime := fileMtime(path)
		norm, err := globalDigestCache.normalize(sess.ID, path, sess.BodyBytes, mtime)
		if err != nil {
			it.errMsg = err.Error()
			unreadable++
			items = append(items, it)
			continue
		}
		it.norm = norm
		items = append(items, it)
	}
	return items, unreadable, mismatched, nil
}

// clusterItems 用并查集把「内容同源」的会话归入同一簇，返回每簇的下标集合。
//
// 合并判据：两份内容**相等或互为有序前缀**（relatedContent）。
// 比较的是行摘要切片（纯字符串比较），首个不同处短路。
func clusterItems(items []sessionItem) [][]int {
	parent := make([]int, len(items))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	for i := 0; i < len(items); i++ {
		if items[i].norm == nil || items[i].unlinked {
			continue // 不可读的不参与合并（单独成组，状态 missing）；已解除关联的不再入组
		}
		for j := i + 1; j < len(items); j++ {
			if items[j].norm == nil || items[j].unlinked {
				continue
			}
			if relatedContent(items[i].norm.LineDigests, items[j].norm.LineDigests) {
				ra, rb := find(i), find(j)
				if ra != rb {
					parent[rb] = ra
				}
			}
		}
	}

	buckets := map[int][]int{}
	for i := range items {
		r := find(i)
		buckets[r] = append(buckets[r], i)
	}
	out := make([][]int, 0, len(buckets))
	for _, idxs := range buckets {
		out = append(out, idxs)
	}
	return out
}

// relatedContent 判断两份内容是否同源（相等，或一方是另一方的有序前缀）。
func relatedContent(a, b []string) bool {
	return equalDigests(a, b) ||
		isStrictOrderedExtension(a, b) ||
		isStrictOrderedExtension(b, a)
}

// projectName 取工作区名（cwd 末段）。
//
// 界面上显示为「项目 X」。cwd 为空时给中性名字而不是空串 ——
// 那会让卡片看起来像坏了。
func projectName(cwd string) string {
	c := strings.TrimSpace(cwd)
	if c == "" {
		return "（未记录工作区）"
	}
	c = strings.TrimRight(c, `\/`)
	if i := strings.LastIndexAny(c, `\/`); i >= 0 {
		if seg := strings.TrimSpace(c[i+1:]); seg != "" {
			return seg
		}
	}
	return c
}

// fileMtime 取文件修改时间（纳秒）；取不到返回 0。
//
// 0 会让缓存每次都失效（不会返回错数据），这是**安全的降级方向**。
func fileMtime(path string) int64 {
	st, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return 0
	}
	return st.ModTime().UnixNano()
}

// stringError 把缓存的错误文本还原成 error。
//
// 不保留原始 error 类型：调用方只把它当文案展示（判定表里也是这么用的），
// 缓存里存 error 接口反而会拖住一次读盘失败的全部上下文。
type stringError string

func (e stringError) Error() string { return string(e) }
