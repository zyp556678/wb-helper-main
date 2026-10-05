// Package clientlimits 从**客户端本机日志**还原「哪个账号的哪个模型被上游限流、
// 官方给出的恢复时刻」。
//
// ## 为什么需要它
//
// 网关自己只能看到**经过它的**请求的 429；而 WorkBuddy 桌面客户端是直连官方服务的，
// 它的限流不会出现在网关的观测里 —— 表现就是「客户端弹了限流提示、面板却毫无动静」。
// wb-switch 对此的做法是双通路：往客户端配置装 hook 实时归因为主、扫客户端日志兜底
// （见其 limits.rs 顶部注释）。本包实现的是**兜底那条**：只读客户端日志，不碰用户配置。
//
// ## 数据形态（实测，2026-10-04 客户端日志）
//
//	[2026/10/4 19:14:15.469] [Info] [pid=42048] [SessionManager] await result.completed ERROR —
//	  session=15e48553-…, error=429 您的使用量已超出频率限制，将在 2026-10-05 13:49:05 UTC+8
//	  重置，您也可以切换其他模型继续使用。 (01a1069f23ff7049abb5ed59e8b0c81e/15e48553-…)
//
// 文案里**没有模型名**，但尾部括号里的 requestId 与同文件更早的
// `[ModelProvider] Sending request: … model=deepseek-v4.1-flash, requestId=01a1069f…`
// 可以精确关联；账号则用同文件里的 `uid=<uuid>` / `userId changed: … -> <uuid>`，
// 昵称兜底取 `[auth_success: <昵称>]`。
//
// ## 保守原则（对齐 switch：宁可少显示，不可显示错账号）
//
//   - 只在最近 `scanWindowDays` 个日期目录里找（客户端日志按天分目录）；
//   - 解析不出重置时刻、或恢复时刻已过的条目直接丢弃；
//   - 账号归因失败（既无 uid 又无唯一昵称）的条目不返回；
//   - 模型归因失败时按 switch 的口径显示「未知模型」，不猜测具体名字。
package clientlimits

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// scanWindowDays 扫描窗口：最近 N 个日期目录（与 switch 的「最近 2 天」一致）。
	scanWindowDays = 2
	// CacheTTL 扫描缓存时长。面板每 5 分钟静默刷新一次账号列表，
	// 扫盘频率跟着它走即可；比这更频繁只会重复读同一批日志。
	CacheTTL = 5 * time.Minute
	// maxFileBytes 单文件读取上限：客户端日志偶有几十 MB 的大文件，
	// 超过上限就跳过（宁可漏，不把内存吃满）。
	maxFileBytes = 64 << 20
	// marker 是字节级粗筛关键字：整份文件不含它就直接跳过，不做逐行解析。
	marker = "超出频率限制"
	// UnknownModel 是模型归因失败时的展示名（与 switch 同口径）。
	UnknownModel = "未知模型"
)

var (
	dateDirPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	// resetTimePattern 取「将在 <YYYY-MM-DD HH:MM:SS> UTC+8 重置」。
	// 时区按原文声明固定为 UTC+8（客户端文案里写死 UTC+8），不自建窗口模型。
	resetTimePattern = regexp.MustCompile(
		`将在\s*(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})\s*UTC\+8`)
	// requestIDPattern 取限额文案尾部的 `(<requestId>/<sessionId>)`。
	requestIDPattern = regexp.MustCompile(`\(([0-9a-fA-F]{8,})/([0-9a-fA-F-]{8,})\)`)
	// modelPattern 取 `model=<名称>`（同一行里与 requestId 一起出现）。
	modelPattern = regexp.MustCompile(`model=([A-Za-z0-9._:\-]+)`)
	// uidPattern 取 `uid=<uuid>`；uidChangedPattern 取 `userId changed: … -> <uuid>`。
	uidPattern        = regexp.MustCompile(`uid=([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)
	uidChangedPattern = regexp.MustCompile(`userId changed:.*->\s*([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)
	// nicknamePattern 取 `[auth_success: <昵称>]`（账号归因的兜底）。
	nicknamePattern = regexp.MustCompile(`\[auth_success:\s*([^\]\n]+)\]`)
)

// Limit 是一条「账号 × 模型」的客户端观测限流。
type Limit struct {
	// UID 是客户端日志里出现的账号 uid（可能为空）。
	UID string
	// Nickname 是认证成功日志里的昵称（uid 缺失时用于兜底归因）。
	Nickname string
	Model    string
	Until    time.Time
	// Evidence 是命中的原文片段（截断），排障与展示用。
	Evidence string
}

// Scanner 扫描客户端日志并缓存结果。零值不可用；用 New 构造。
type Scanner struct {
	mu       sync.Mutex
	roots    []string
	cachedAt time.Time
	cached   []Limit
}

// New 创建扫描器。roots 是客户端日志根（如 ~/.workbuddy/logs、~/.workbuddy-ai/logs）；
// 不存在的根会被静默跳过 —— 本机没装那个客户端是正常路径。
func New(roots ...string) *Scanner {
	kept := make([]string, 0, len(roots))
	for _, root := range roots {
		if strings.TrimSpace(root) != "" {
			kept = append(kept, root)
		}
	}
	return &Scanner{roots: kept}
}

// Limits 返回当前仍有效的客户端观测限流（按恢复时间升序）。
// 结果按 CacheTTL 缓存；nil 接收者返回空。
func (s *Scanner) Limits(now time.Time) []Limit {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cachedAt.IsZero() && now.Sub(s.cachedAt) < CacheTTL {
		return append([]Limit(nil), s.cached...)
	}
	s.cached = scan(s.roots, now)
	s.cachedAt = now
	return append([]Limit(nil), s.cached...)
}

func scan(roots []string, now time.Time) []Limit {
	var out []Limit
	seen := map[string]int{} // uid|nickname + model -> out 下标（同键取更晚的恢复时刻）
	for _, root := range roots {
		for _, dir := range recentDateDirs(root, scanWindowDays) {
			_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return nil
				}
				if !strings.HasSuffix(strings.ToLower(entry.Name()), ".log") {
					return nil
				}
				info, err := entry.Info()
				if err != nil || info.Size() > maxFileBytes {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil || !bytes.Contains(data, []byte(marker)) {
					return nil
				}
				collectFile(string(data), now, seen, &out)
				return nil
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Until.Equal(out[j].Until) {
			return out[i].Until.Before(out[j].Until)
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// recentDateDirs 返回 root 下最近 n 个日期目录（新→旧）；目录不存在/无日期目录时为空。
func recentDateDirs(root string, n int) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && dateDirPattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	if len(names) > n {
		names = names[len(names)-n:]
	}
	out := make([]string, 0, len(names))
	for i := len(names) - 1; i >= 0; i-- { // 新目录优先
		out = append(out, filepath.Join(root, names[i]))
	}
	return out
}

func collectFile(text string, now time.Time, seen map[string]int, out *[]Limit) {
	lines := strings.Split(text, "\n")
	uid := firstSubmatch(uidPattern, text)
	if uid == "" {
		uid = firstSubmatch(uidChangedPattern, text)
	}
	nickname := strings.TrimSpace(firstSubmatch(nicknamePattern, text))
	onlyModel := soleModel(lines)

	for _, line := range lines {
		if !strings.Contains(line, marker) {
			continue
		}
		until, ok := parseResetTime(line)
		if !ok || !until.After(now) {
			continue
		}
		// 账号归因失败（既无 uid 又无昵称）的条目**不返回**：
		// 面板按账号展示，无主的限流提示只能挂到错误账号上（宁可少显示）。
		if uid == "" && nickname == "" {
			continue
		}
		model := ""
		if rid := requestIDPattern.FindStringSubmatch(line); rid != nil {
			model = modelForRequest(lines, rid[1])
		}
		if model == "" {
			model = onlyModel
		}
		if model == "" {
			model = UnknownModel
		}
		key := uid + "\x00" + nickname + "\x00" + model
		if idx, ok := seen[key]; ok {
			if until.After((*out)[idx].Until) {
				(*out)[idx].Until = until
			}
			continue
		}
		seen[key] = len(*out)
		*out = append(*out, Limit{
			UID:      uid,
			Nickname: nickname,
			Model:    model,
			Until:    until,
			Evidence: truncateRunes(strings.TrimSpace(line), 240),
		})
	}
}

// parseResetTime 从命中行里取重置时刻（UTC+8）。
func parseResetTime(line string) (time.Time, bool) {
	m := resetTimePattern.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	loc := time.FixedZone("UTC+8", 8*3600)
	nums := make([]int, 0, 6)
	for _, g := range m[1:] {
		n := 0
		for _, ch := range g {
			n = n*10 + int(ch-'0')
		}
		nums = append(nums, n)
	}
	return time.Date(nums[0], time.Month(nums[1]), nums[2], nums[3], nums[4], nums[5], 0, loc), true
}

// modelForRequest 用 requestId 关联同文件里 `Sending request: … model=…, requestId=…` 行。
func modelForRequest(lines []string, requestID string) string {
	needle := "requestId=" + requestID
	for _, line := range lines {
		if !strings.Contains(line, needle) {
			continue
		}
		if m := modelPattern.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// soleModel 返回整份文件里唯一出现过的 model=（多个不同模型时返回空，不猜测）。
func soleModel(lines []string) string {
	found := ""
	for _, line := range lines {
		m := modelPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if found == "" {
			found = m[1]
			continue
		}
		if found != m[1] {
			return ""
		}
	}
	return found
}

func firstSubmatch(re *regexp.Regexp, text string) string {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
