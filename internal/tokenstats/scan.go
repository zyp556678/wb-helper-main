package tokenstats

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// 数值提取
// -----------------------------------------------------------------------------

// number 把 JSON 里的数值宽容地转成 int64。
//
// 上游字段的类型会漂移（数字给成字符串、给成浮点、给成负数都见过），
// 严格断言会让整条记录被丢掉 —— 读用量的解析必须宽容。
//
// 这里把所有整数宽度都收进来：经 `json.Unmarshal` 到 `any` 的数字是 float64，
// 但同一个函数也会被直接构造的 map 调用（测试、以及将来可能的其他解码路径），
// 那些路径给的是 Go 原生整数类型。只认 float64 会让这类调用全部解析成 0 ——
// 而且不报错，只是数字悄悄变成 0。
func number(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, false
		}
		return int64(n), true
	case float32:
		if n < 0 {
			return 0, false
		}
		return int64(n), true
	case int:
		if n < 0 {
			return 0, false
		}
		return int64(n), true
	case int32:
		if n < 0 {
			return 0, false
		}
		return int64(n), true
	case int64:
		if n < 0 {
			return 0, false
		}
		return n, true
	case uint:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		return int64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil || f < 0 {
			return 0, false
		}
		return int64(f), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil || f < 0 {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}

// field 按候选键顺序取第一个能解析成正数的字段。
func field(obj map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			if n, ok := number(v); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// positiveField 只接受 > 0 的候选值。
//
// 为什么不能接受 0：缓存字段在同一份日志里可能有多个别名，其中一个陈旧的
// `cache_read_input_tokens: 0` 会掩盖另一个已填充的 `prompt_cache_hit_tokens`，
// 于是缓存命中率凭空掉到 0。取第一个正值才稳。
func positiveField(obj map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			if n, ok := number(v); ok && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}

func text(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := obj[k].(string); ok {
			if t := strings.TrimSpace(s); t != "" {
				return t
			}
		}
	}
	return ""
}

func asObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// cachedFromDetails 从嵌套的 details 结构里找缓存命中数。
//
// 实测两种形态都存在：
//
//	prompt_tokens_details.cached_tokens        （对象）
//	usage.inputTokensDetails[].cached_tokens   （数组）
func cachedFromDetails(u map[string]any) (int64, bool) {
	if d := asObject(u["prompt_tokens_details"]); d != nil {
		if n, ok := positiveField(d, "cached_tokens"); ok {
			return n, true
		}
	}
	for _, key := range []string{"inputTokensDetails", "input_tokens_details"} {
		if arr, ok := u[key].([]any); ok {
			for _, item := range arr {
				if d := asObject(item); d != nil {
					if n, ok := positiveField(d, "cached_tokens"); ok {
						return n, true
					}
				}
			}
		}
	}
	return 0, false
}

// -----------------------------------------------------------------------------
// 单行解析
// -----------------------------------------------------------------------------

// usageObject 按优先级挑出 usage 对象。
//
// 合法性判据是「至少有一个输入锚点键」而不是 `input > 0`：上游可能上报
// output-only 的重试，input 合法为 0，那种记录也是有效用量。
func usageObject(rec map[string]any) (map[string]any, string) {
	if msg := asObject(rec["message"]); msg != nil {
		if u := asObject(msg["usage"]); u != nil && hasInputAnchor(u) {
			return u, "message.usage"
		}
	}
	if pd := asObject(rec["providerData"]); pd != nil {
		if u := asObject(pd["usage"]); u != nil && hasInputAnchor(u) {
			return u, "providerData.usage"
		}
	}
	if u := asObject(rec["usage"]); u != nil && hasInputAnchor(u) {
		return u, "usage"
	}
	return nil, ""
}

func hasInputAnchor(u map[string]any) bool {
	_, ok := field(u, "input_tokens", "inputTokens", "prompt_tokens")
	return ok
}

// parseUsage 从 usage 对象 + 原始记录里提取完整用量。
//
// fallbacks 是 providerData.rawUsage —— 缓存写入与思考 token **只在那里**，
// `message.usage` / `providerData.usage` 都不带（已用真实日志核对）。
func parseUsage(u map[string]any, raw map[string]any) (Usage, int64) {
	var out Usage

	if n, ok := field(u, "input_tokens", "inputTokens", "prompt_tokens"); ok {
		out.Input = n
	}
	if n, ok := field(u, "output_tokens", "outputTokens", "completion_tokens"); ok {
		out.Output = n
	}

	if n, ok := positiveField(u,
		"cache_read_input_tokens", "cacheReadInputTokens",
		"prompt_cache_hit_tokens", "cached_tokens"); ok {
		out.Read = n
	} else if n, ok := cachedFromDetails(u); ok {
		out.Read = n
	}

	// 缓存写入：先在本层找，找不到再到 rawUsage。
	// 注意 `prompt_cache_miss_tokens` **不是**写入的别名 —— 它表示"新算的、
	// 未被缓存的输入"，把它当写入会让写入量虚高。
	if n, ok := positiveField(u,
		"cache_write_input_tokens", "cacheWriteInputTokens",
		"cache_creation_input_tokens", "prompt_cache_write_tokens"); ok {
		out.Write = n
	} else if raw != nil {
		if n, ok := positiveField(raw,
			"cache_creation_input_tokens", "cache_write_input_tokens",
			"prompt_cache_write_tokens", "cacheWriteInputTokens"); ok {
			out.Write = n
		}
	}

	var thinking int64
	if raw != nil {
		if n, ok := positiveField(raw, "completion_thinking_tokens"); ok {
			thinking = n
		} else if d := asObject(raw["completion_tokens_details"]); d != nil {
			if n, ok := field(d, "reasoning_tokens"); ok {
				thinking = n
			}
		}
	}
	return out, thinking
}

// timestampOf 取毫秒时间戳（实测 timestamp 是 13 位毫秒）。
func timestampOf(rec map[string]any) (int64, bool) {
	for _, k := range []string{"timestamp", "ts", "startedAt"} {
		if n, ok := number(rec[k]); ok && n > 0 {
			return n, true
		}
	}
	return 0, false
}

func modelOf(rec map[string]any) string {
	if pd := asObject(rec["providerData"]); pd != nil {
		if m := text(pd, "model", "modelName", "modelId"); m != "" {
			return m
		}
	}
	if m := text(rec, "model", "modelName", "modelId"); m != "" {
		return m
	}
	return unknownModel
}

func thinkingOnly(rec map[string]any) int64 {
	raw := asObject(rec["providerData"])
	if raw == nil {
		return 0
	}
	if ru := asObject(raw["rawUsage"]); ru != nil {
		if n, ok := positiveField(ru, "completion_thinking_tokens"); ok {
			return n
		}
		if d := asObject(ru["completion_tokens_details"]); d != nil {
			if n, ok := field(d, "reasoning_tokens"); ok {
				return n
			}
		}
	}
	return 0
}

// -----------------------------------------------------------------------------
// 时间归属
// -----------------------------------------------------------------------------

// dateOf 本地时区的 YYYY-MM-DD。
//
// 必须用本地时区：官方账本按本地日切，网关天桶也已改成本地日（见
// internal/stats 的 localDayIndex）；这里若用 UTC，同一天的用量会被劈成两天。
func dateOf(ms int64) string {
	return time.UnixMilli(ms).Local().Format("2006-01-02")
}

// -----------------------------------------------------------------------------
// 项目归属
// -----------------------------------------------------------------------------

// projectOf 优先取记录里的 cwd 的 basename，回退到目录名推断。
//
// 目录名常是**完整绝对路径的编码**（如 c-Users-foo-WorkBuddy-2026-09-23…），
// 直接展示会泄漏用户名与父目录，因此这类一律记成「未知项目」。
func projectOf(rec map[string]any, fallback string) string {
	if pd := asObject(rec["providerData"]); pd != nil {
		if cwd := text(pd, "cwd"); cwd != "" {
			return baseName(cwd)
		}
	}
	if cwd := text(rec, "cwd"); cwd != "" {
		return baseName(cwd)
	}
	return sanitizeFallback(fallback)
}

func baseName(p string) string {
	p = strings.TrimRight(strings.ReplaceAll(p, "\\", "/"), "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if p == "" || len(p) > 120 {
		return unknownProject
	}
	return p
}

func sanitizeFallback(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".json") {
		return unknownProject
	}
	// 产品把绝对路径编码进目录名（c-Users-… / d-software-… / home-…），
	// 带用户名或盘符的一律不展示。
	if strings.HasPrefix(name, "Users-") || strings.HasPrefix(name, "home-") ||
		strings.HasPrefix(name, "c-") || strings.HasPrefix(name, "d-") ||
		strings.HasPrefix(name, "C-") || strings.HasPrefix(name, "D-") {
		return unknownProject
	}
	return name
}

// -----------------------------------------------------------------------------
// 文件发现
// -----------------------------------------------------------------------------

type fileEntry struct {
	path  string
	mtime int64
}

// ideSkipDirs 是 IDE 数据目录下不递归的子树。
//
// messages/ 是消息正文（隐私），check-point/ 与 backups/ 与用量无关，
// Public/ 是共享桶。扫它们既没用又慢。
var ideSkipDirs = map[string]bool{
	"messages": true, "check-point": true, "backups": true, "Public": true,
}

// discoverJSONL 递归收集 .jsonl，跳过 subagents。
//
// subagents 是子代理会话，它会把父会话的上下文重新写一遍，
// 计进去就是重复用量。
func discoverJSONL(root string, out *[]fileEntry) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if e.IsDir() {
			if e.Name() == "subagents" {
				continue
			}
			discoverJSONL(p, out)
			continue
		}
		if strings.HasSuffix(e.Name(), ".jsonl") {
			appendFile(p, out)
		}
	}
}

// isIDEConversationIndex 判断是否为会话级 index.json：
// 文件名 index.json 且上溯三级目录名是 history。
func isIDEConversationIndex(p string) bool {
	if filepath.Base(p) != "index.json" {
		return false
	}
	gp := filepath.Dir(filepath.Dir(filepath.Dir(p)))
	return filepath.Base(gp) == "history"
}

func discoverIDE(root string, out *[]fileEntry) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if e.IsDir() {
			if ideSkipDirs[e.Name()] {
				continue
			}
			discoverIDE(p, out)
			continue
		}
		if isIDEConversationIndex(p) {
			appendFile(p, out)
		}
	}
}

func appendFile(p string, out *[]fileEntry) {
	info, err := os.Stat(p)
	if err != nil {
		return
	}
	*out = append(*out, fileEntry{path: p, mtime: info.ModTime().UnixMilli()})
}

// sortByMtime 按 mtime 升序。
//
// 复制/分叉的会话会把父会话的历史**重放**一遍，mtime 升序 + 指纹去重
// 能让这些重放的用量归到原始会话，而不是算在副本头上。
func sortByMtime(files []fileEntry) {
	sort.SliceStable(files, func(i, j int) bool { return files[i].mtime < files[j].mtime })
}

// -----------------------------------------------------------------------------
// 扫描
// -----------------------------------------------------------------------------

// scanJSONL 扫描一批 JSONL 文件并累加到 c。
//
// 会话维度是**按文件**聚合的：一个 .jsonl 就是一个会话，会话名取文件名。
// 因此这里先在本文件内累计，读完再把整份结果作为一个会话挂上去 ——
// 不能边读边往全局会话表里塞（那样每个用量点都会变成一个"会话"）。
func scanJSONL(c *collector, root string, files []fileEntry, cutoff int64, detail bool) {
	for _, f := range files {
		c.files++
		file, err := os.Open(f.path)
		if err != nil {
			c.parseErrs++
			continue
		}

		fallback := firstComponent(root, f.path)
		sessionID := strings.TrimSuffix(filepath.Base(f.path), ".jsonl")
		var sess totals
		title := ""
		project := ""
		var pending []RequestRow

		sc := bufio.NewScanner(file)
		// 必须抬高缓冲区：工具结果会写进日志，实测单行可达数 MB，
		// 而 Scanner 默认上限 64KB —— 超了会静默停止（表现为少统计一大截）。
		sc.Buffer(make([]byte, 0, 256<<10), maxLineBytes)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil {
				c.parseErrs++
				continue
			}
			if rec == nil {
				continue
			}

			// 标题事件不受时间窗限制：标题可能落在窗口之外，但它是窗内那条
			// 用量记录的人类可读标识。aiTitle 恒优先于 summary。
			if t := titleOf(rec); t != "" {
				if title == "" || rec["aiTitle"] != nil {
					title = t
				}
			}

			u, _ := usageObject(rec)
			if u == nil {
				continue
			}
			var raw map[string]any
			if pd := asObject(rec["providerData"]); pd != nil {
				raw = asObject(pd["rawUsage"])
			}
			usage, thinking := parseUsage(u, raw)

			ts, hasTS := timestampOf(rec)
			model := modelOf(rec)
			itemProject := projectOf(rec, fallback)
			if project == "" {
				project = itemProject
			}

			// 去重：内容指纹。只有带时间戳的记录才去重；无时间戳的照常统计，
			// 但因为它无法排序，不进明细。
			if hasTS {
				fp := fingerprint{ts: ts, input: usage.Input, output: usage.Output,
					read: usage.Read, write: usage.Write, model: model}
				if _, dup := c.seen[fp]; dup {
					continue
				}
				c.seen[fp] = struct{}{}
			}

			// 按天维度**不受时间窗限制**：热力图要展示最近一年，
			// 若跟着窗口一起截断，选「今天」时热力图就只剩一格。
			// 其余维度按窗口过滤（它们是"这段时间用了多少"的答案）。
			if hasTS {
				day := dateOf(ts)
				bump(c.daily, day, usage)
				perModel := c.dailyByModel[model]
				if perModel == nil {
					perModel = map[string]*totals{}
					c.dailyByModel[model] = perModel
				}
				bump(perModel, day, usage)
			}

			if cutoff > 0 && hasTS && ts < cutoff {
				continue
			}

			c.total.add(usage)
			bump(c.models, model, usage)
			bump(c.projects, itemProject, usage)
			sess.add(usage)
			if hasTS {
				if c.coverageStart == nil || ts < *c.coverageStart {
					v := ts
					c.coverageStart = &v
				}
				if c.coverageEnd == nil || ts > *c.coverageEnd {
					v := ts
					c.coverageEnd = &v
				}
				if detail {
					pending = append(pending, RequestRow{
						Timestamp: ts, Model: model, Project: itemProject,
						Usage: usage, Thinking: thinking,
					})
				}
			}
		}
		if err := sc.Err(); err != nil {
			c.parseErrs++
		}
		file.Close()

		if sess.records > 0 {
			if project == "" {
				project = sanitizeFallback(fallback)
			}
			// 标题可能出现在用量记录之后（aiTitle 往往写在会话末尾），
			// 所以读完整个文件再回填。
			for i := range pending {
				pending[i].Title = title
				pending[i].SessionID = sessionID
			}
			c.sessions = append(c.sessions, sessionAgg{
				sessionID: sessionID, project: project, title: title, totals: sess,
			})
			if detail {
				c.requests = append(c.requests, pending...)
			}
		}
	}
}

// firstComponent 取相对根的第一级目录名作为项目回退值。
func firstComponent(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return unknownProject
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 {
		return unknownProject
	}
	return sanitizeFallback(parts[0])
}

func titleOf(rec map[string]any) string {
	if t := text(rec, "aiTitle"); t != "" {
		return t
	}
	return text(rec, "summary")
}

func bump(m map[string]*totals, key string, u Usage) {
	t, ok := m[key]
	if !ok {
		t = &totals{}
		m[key] = t
	}
	t.add(u)
}
