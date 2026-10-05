package tokenstats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// IDE 会话日志（CodeBuddy IDE / VS Code 插件）
// -----------------------------------------------------------------------------

// ideWorkspaceIndex 读出工作区级 index.json 里的会话元信息。
//
// 结构：`history/<ws>/index.json` 的 `conversations[]`，
// 每项含 id / name|title / selectedModelId|modelId|model。
// 会话级目录名即 id，用它去匹配。
type ideConversationMeta struct {
	title string
	model string
}

func ideWorkspaceMeta(convIndexPath string) map[string]ideConversationMeta {
	out := map[string]ideConversationMeta{}
	// 会话级 index.json 位于 history/<ws>/<conv-id>/index.json，
	// 工作区级 index.json 就在 history/<ws>/index.json。
	wsIndex := filepath.Join(filepath.Dir(filepath.Dir(convIndexPath)), "index.json")
	raw, err := os.ReadFile(wsIndex)
	if err != nil {
		return out
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return out
	}
	list, ok := doc["conversations"].([]any)
	if !ok {
		return out
	}
	for _, item := range list {
		conv := asObject(item)
		if conv == nil {
			continue
		}
		id := text(conv, "id")
		if id == "" {
			continue
		}
		out[id] = ideConversationMeta{
			title: text(conv, "name", "title"),
			model: textOr(text(conv, "selectedModelId"), text(conv, "modelId"), text(conv, "model")),
		}
	}
	return out
}

func textOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func scanIDE(c *collector, root string, files []fileEntry, cutoff int64, detail bool) {
	for _, f := range files {
		c.files++
		raw, err := os.ReadFile(f.path)
		if err != nil {
			c.parseErrs++
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			c.parseErrs++
			continue
		}

		convID := filepath.Base(filepath.Dir(f.path))
		meta := ideWorkspaceMeta(f.path)
		title := ""
		model := unknownModel
		if m, ok := meta[convID]; ok {
			title = m.title
			if m.model != "" {
				model = m.model
			}
		}
		project := sanitizeFallback(filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(f.path)))))

		reqs, _ := doc["requests"].([]any)
		var sess totals
		var pending []RequestRow

		for _, item := range reqs {
			req := asObject(item)
			if req == nil {
				continue
			}
			u := asObject(req["usage"])
			if u == nil {
				continue
			}
			// IDE 的字段名与 JSONL 不同：缓存命中是 `cacheTokens`，
			// 写入是 `cachedWriteTokens`。
			if _, ok := field(u, "inputTokens", "input_tokens", "prompt_tokens"); !ok {
				continue
			}
			var usage Usage
			usage.Input, _ = field(u, "inputTokens", "input_tokens", "prompt_tokens")
			usage.Output, _ = field(u, "outputTokens", "output_tokens", "completion_tokens")
			usage.Read, _ = positiveField(u, "cacheTokens", "cacheReadInputTokens", "cache_read_input_tokens")
			usage.Write, _ = positiveField(u, "cachedWriteTokens", "cacheWriteInputTokens",
				"cache_write_input_tokens", "cache_creation_input_tokens")

			ts, hasTS := timestampOf(req)
			if hasTS {
				fp := fingerprint{ts: ts, input: usage.Input, output: usage.Output,
					read: usage.Read, write: usage.Write, model: model}
				if _, dup := c.seen[fp]; dup {
					continue
				}
				c.seen[fp] = struct{}{}
			}
			if cutoff > 0 && hasTS && ts < cutoff {
				continue
			}

			c.total.add(usage)
			bump(c.models, model, usage)
			bump(c.projects, project, usage)
			sess.add(usage)
			if hasTS {
				bump(c.daily, dateOf(ts), usage)
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
						Timestamp: ts, Model: model, Project: project,
						SessionID: convID, Title: title, Usage: usage,
					})
				}
			}
		}

		if sess.records > 0 {
			c.sessions = append(c.sessions, sessionAgg{
				sessionID: convID, project: project, title: title, totals: sess,
			})
			if detail {
				c.requests = append(c.requests, pending...)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// 输出
// -----------------------------------------------------------------------------

func totalsJSON(t totals) TotalsJSON {
	out := TotalsJSON{
		Total:         t.usage.total(),
		Input:         t.usage.Input,
		Output:        t.usage.Output,
		CacheRead:     t.usage.Read,
		CacheWrite:    t.usage.Write,
		UncachedInput: maxInt64(0, t.usage.Input-t.usage.Read),
		Records:       t.records,
	}
	if t.usage.Input > 0 {
		rate := float64(t.usage.Read) / float64(t.usage.Input)
		out.CacheHitRate = &rate
	}
	return out
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// groups 把 map 转成按总量倒序的数组（用于模型 / 项目 / 会话这类"排行榜"）。
func groups(m map[string]*totals) []GroupJSON {
	out := make([]GroupJSON, 0, len(m))
	for k, t := range m {
		out = append(out, GroupJSON{Key: k, TotalsJSON: totalsJSON(*t)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// groupsByKey 按 key 升序（用于**时间序列**）。
//
// 时间序列绝不能走 groups() —— 那个是按总量倒序的，画到折线/柱状图上会变成
// 「09/24 → 09/25 → 09/23」这种乱序（真实踩过：X 轴顺序错乱，且量级小的那天
// 被排到末尾看不出异常）。按天维度是日期字符串，字典序即时间序。
func groupsByKey(m map[string]*totals) []GroupJSON {
	out := make([]GroupJSON, 0, len(m))
	for k, t := range m {
		out = append(out, GroupJSON{Key: k, TotalsJSON: totalsJSON(*t)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// sessionKey 生成会话展示键，重名时追加序号避免前端 key 冲突。
func sessionKey(project, sessionID string, used map[string]int) string {
	base := project + " · " + sessionID
	n := used[base]
	used[base] = n + 1
	if n == 0 {
		return base
	}
	return base + " · " + itoa(n+1)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// pruneDaily 只保留最近一年，避免热力图拿到无限长的历史。
func pruneDaily(m map[string]*totals) map[string]*totals {
	if len(m) <= 366 {
		return m
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys[:len(keys)-366] {
		delete(m, k)
	}
	return m
}

func (c *collector) toSource(source, label string) SourceJSON {
	used := map[string]int{}
	sessions := make([]GroupJSON, 0, len(c.sessions))
	for _, s := range c.sessions {
		var titlePtr *string
		if s.title != "" {
			t := s.title
			titlePtr = &t
		}
		sessions = append(sessions, GroupJSON{
			Key:        sessionKey(s.project, s.sessionID, used),
			Title:      titlePtr,
			Project:    s.project,
			SessionID:  s.sessionID,
			TotalsJSON: totalsJSON(s.totals),
		})
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Total != sessions[j].Total {
			return sessions[i].Total > sessions[j].Total
		}
		return sessions[i].Key < sessions[j].Key
	})

	reqs := make([]RequestRowJSON, 0, len(c.requests))
	sort.Slice(c.requests, func(i, j int) bool { return c.requests[i].Timestamp > c.requests[j].Timestamp })
	if len(c.requests) > requestWindow {
		c.requests = c.requests[:requestWindow]
	}
	for _, r := range c.requests {
		var titlePtr *string
		if r.Title != "" {
			t := r.Title
			titlePtr = &t
		}
		reqs = append(reqs, RequestRowJSON{
			Timestamp: r.Timestamp, Model: r.Model, Project: r.Project,
			SessionID: r.SessionID, Title: titlePtr,
			Input: r.Usage.Input, Output: r.Usage.Output,
			CacheRead: r.Usage.Read, CacheWrite: r.Usage.Write,
			UncachedInput: maxInt64(0, r.Usage.Input-r.Usage.Read),
			Thinking:      r.Thinking, Total: r.Usage.total(),
		})
	}

	// 每个模型的按天序列。只给量级靠前的 20 个模型：交叉维度是「模型 × 天」，
	// 模型一多这份 JSON 会膨胀得比主数据还大，而长尾模型也没人会逐个去看趋势。
	dailyByModel := map[string][]GroupJSON{}
	modelKeys := make([]string, 0, len(c.dailyByModel))
	for k := range c.dailyByModel {
		modelKeys = append(modelKeys, k)
	}
	sort.Slice(modelKeys, func(i, j int) bool {
		return daySeriesTotal(c.dailyByModel[modelKeys[i]]) > daySeriesTotal(c.dailyByModel[modelKeys[j]])
	})
	if len(modelKeys) > 20 {
		modelKeys = modelKeys[:20]
	}
	for _, k := range modelKeys {
		// 同样是时间序列，必须按日期排。
		dailyByModel[k] = groupsByKey(c.dailyByModel[k])
	}

	return SourceJSON{
		Source:          source,
		Kind:            KindLocalLog,
		Label:           label,
		Available:       c.files > 0,
		Summary:         totalsJSON(c.total),
		Models:          groups(c.models),
		Projects:        groups(c.projects),
		Sessions:        sessions,
		Daily:           groupsByKey(pruneDaily(c.daily)),
		DailyByModel:    dailyByModel,
		Requests:        reqs,
		FilesScanned:    c.files,
		ParseErrors:     c.parseErrs,
		CoverageStartAt: c.coverageStart,
		CoverageEndAt:   c.coverageEnd,
	}
}

// daySeriesTotal 求一组按天桶的 token 总量，用于排序。
func daySeriesTotal(m map[string]*totals) int64 {
	var sum int64
	for _, t := range m {
		sum += t.usage.total()
	}
	return sum
}

// -----------------------------------------------------------------------------
// 对外入口
// -----------------------------------------------------------------------------

var (
	cacheMu   sync.Mutex
	cacheKey  string
	cacheVal  Statistics
	cacheTime time.Time
)

// Collect 扫描本地日志并返回各来源的统计。
//
// days<=0 表示不限窗口。结果按 45 秒 TTL 缓存：本机日志有 100MB 量级，
// 面板还会定时刷新，逐次全量重扫会把面板拖垮。
// 这是与 wb-switch 的**有意偏离**（它每次调用都重扫），只影响耗时，不影响口径。
func Collect(days int) Statistics {
	key := itoa(days)
	cacheMu.Lock()
	if cacheKey == key && time.Since(cacheTime) < scanCacheTTL {
		val := cacheVal
		cacheMu.Unlock()
		return val
	}
	cacheMu.Unlock()

	val := collectUncached(days)

	cacheMu.Lock()
	cacheKey, cacheVal, cacheTime = key, val, time.Now()
	cacheMu.Unlock()
	return val
}

// InvalidateCache 丢弃缓存（面板手动刷新时用，让用户能立刻看到最新结果）。
func InvalidateCache() {
	cacheMu.Lock()
	cacheTime = time.Time{}
	cacheMu.Unlock()
}

func collectUncached(days int) Statistics {
	return collectFrom(localRoots(), days)
}

// collectFrom 是扫描主体。根目录由参数传入（而不是内部写死本机路径），
// 这样单元测试可以指向临时目录构造 fixture —— 否则这套解析规则完全无法回归测试。
func collectFrom(roots []rootSpec, days int) Statistics {
	var cutoff int64
	if days > 0 {
		start := time.Now().AddDate(0, 0, -(days - 1))
		local := start.Local()
		cutoff = time.Date(local.Year(), local.Month(), local.Day(),
			0, 0, 0, 0, time.Local).UnixMilli()
	}

	// 按 source 归并多个根：国际版的 projects/ 与 sessions/ 共享同一批去重指纹，
	// 否则同一会话在两处各记一次。
	type pendingSource struct {
		label string
		c     *collector
	}
	order := []string{"workbuddy", "workbuddy-ai", "codebuddy-cli", "codebuddy-ide"}
	bySource := map[string]*pendingSource{}

	for _, spec := range roots {
		info, err := os.Stat(spec.path)
		if err != nil || !info.IsDir() {
			continue
		}
		ps := bySource[spec.source]
		if ps == nil {
			ps = &pendingSource{label: spec.label, c: newCollector()}
			bySource[spec.source] = ps
		}

		var files []fileEntry
		if spec.ide {
			discoverIDE(spec.path, &files)
		} else {
			discoverJSONL(spec.path, &files)
		}
		sortByMtime(files)

		// 明细只在有页面消费的来源上收集：明细行会把整个窗口的请求都留在内存里，
		// 对不被展示的来源收它是纯粹的浪费。
		detail := true
		if spec.ide {
			scanIDE(ps.c, spec.path, files, cutoff, detail)
		} else {
			scanJSONL(ps.c, spec.path, files, cutoff, detail)
		}
	}

	sources := make([]SourceJSON, 0, len(order))
	for _, name := range order {
		ps := bySource[name]
		if ps == nil {
			sources = append(sources, SourceJSON{
				Source: name, Kind: KindLocalLog, Label: labelFor(name),
				Available:    false,
				Note:         "本机未发现该来源的会话日志",
				Models:       []GroupJSON{},
				Projects:     []GroupJSON{},
				Sessions:     []GroupJSON{},
				Daily:        []GroupJSON{},
				DailyByModel: map[string][]GroupJSON{},
				Requests:     []RequestRowJSON{},
			})
			continue
		}
		src := ps.c.toSource(name, ps.label)
		if src.Summary.Records == 0 {
			// 两种"空"必须分开说：一种是本机压根没这个产品（可能是没装），
			// 另一种是装了也扫到文件、但里面没有 usage。混成一句话会让人
			// 以为是同一个问题。
			if src.FilesScanned == 0 {
				src.Note = "本机未发现该来源的会话日志"
			} else {
				src.Note = "已扫描到 " + itoa(src.FilesScanned) + " 个会话文件，但其中没有可用的 usage 记录"
			}
		}
		sources = append(sources, src)
	}

	return Statistics{
		GeneratedAt: time.Now().UnixMilli(),
		RangeDays:   days,
		Sources:     sources,
	}
}

func labelFor(source string) string {
	switch source {
	case "workbuddy":
		return "WorkBuddy"
	case "workbuddy-ai":
		return "WorkBuddy 国际版"
	case "codebuddy-cli":
		return "CodeBuddy CLI"
	case "codebuddy-ide":
		return "CodeBuddy IDE"
	default:
		return source
	}
}

// -----------------------------------------------------------------------------
// 网关反代口径 → 同一 JSON 形状
// -----------------------------------------------------------------------------

// GatewayGroup 网关侧的一个分组。
//
// 注意能力差异：**模型维度只有总量**（网关按模型累加 tokens 一个数），
// **账号维度有输入/输出拆分**。因此 Input/Output 允许为 0 而 Tokens 有值，
// 此时 Total 取 Tokens —— 不能让「拆分未知」显示成「拆分为 0」。
type GatewayGroup struct {
	Key string
	// Title 是展示名（目前只有「账号」维度用：昵称，或按 display_field 选的备注）。
	//
	// **Key 是凭据文件名**（workbuddy-xxxx.json），直接显示等于让用户对着一串
	// uuid 找账号。解析显示名要用到账号池与备注存储，那是 server 层的事 ——
	// 本包不反向依赖，所以由调用方填好传进来；空表示没有更好的名字，用 Key。
	Title    string
	Requests int64
	Tokens   int64
	Input    int64
	Output   int64
}

func gatewayGroupTotals(m GatewayGroup) TotalsJSON {
	total := m.Tokens
	if total == 0 {
		total = m.Input + m.Output
	}
	return TotalsJSON{
		Total:         total,
		Input:         m.Input,
		Output:        m.Output,
		Records:       m.Requests,
		UncachedInput: m.Input,
	}
}

// GatewayDay 网关侧的一天。
type GatewayDay struct {
	Date     string
	Requests int64
	Input    int64
	Output   int64
	Tokens   int64
}

// GatewayInput 是网关统计里本页需要的那部分。
type GatewayInput struct {
	Requests        int64
	Failures        int64
	InputTokens     int64
	OutputTokens    int64
	Tokens          int64
	Models          []GatewayGroup
	Accounts        []GatewayGroup
	Daily           []GatewayDay
	CoverageStartAt int64
	CoverageEndAt   int64
}

// BuildGatewaySource 把网关自己的计数包装成与本地日志同构的来源。
//
// 必须诚实标注能力边界：网关只按请求累加输入/输出 token，
// **没有缓存读写、没有思考 token、也没有项目/会话维度**。
// 这些字段留零并配 Note，前端据此隐藏对应区块 —— 显示 0 会被误读成
// 「真实测到是 0」，那比不显示更糟。
func BuildGatewaySource(in GatewayInput) SourceJSON {
	// 网关自己的 Tokens 是权威总量（它按上游给的 total_tokens 累加）；
	// 输入/输出拆分可能因上游只给 total_tokens 而缺失（见 stats.Record 的兜底），
	// 那时不能拿 input+output 当总量（会偏小）。
	total := in.Tokens
	if total == 0 {
		total = in.InputTokens + in.OutputTokens
	}
	summary := TotalsJSON{
		Total:         total,
		Input:         in.InputTokens,
		Output:        in.OutputTokens,
		Records:       in.Requests,
		UncachedInput: in.InputTokens,
	}

	daily := make([]GroupJSON, 0, len(in.Daily))
	for _, d := range in.Daily {
		daily = append(daily, GroupJSON{
			Key: d.Date,
			TotalsJSON: TotalsJSON{
				Total:         d.Input + d.Output,
				Input:         d.Input,
				Output:        d.Output,
				Records:       d.Requests,
				UncachedInput: d.Input,
			},
		})
	}
	sort.Slice(daily, func(i, j int) bool { return daily[i].Key < daily[j].Key })

	models := make([]GroupJSON, 0, len(in.Models))
	for _, m := range in.Models {
		models = append(models, GroupJSON{Key: m.Key, TotalsJSON: gatewayGroupTotals(m)})
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Total != models[j].Total {
			return models[i].Total > models[j].Total
		}
		return models[i].Key < models[j].Key
	})

	accounts := make([]GroupJSON, 0, len(in.Accounts))
	for _, a := range in.Accounts {
		// Title 与 Key 相同（或为空）时不写 Title：前端据此把 Key 当副标题展示，
		// 避免出现「显示名 = 文件名」时上下两行一模一样。
		g := GroupJSON{Key: a.Key, TotalsJSON: gatewayGroupTotals(a)}
		if t := strings.TrimSpace(a.Title); t != "" && t != a.Key {
			g.Title = &t
		}
		accounts = append(accounts, g)
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Total != accounts[j].Total {
			return accounts[i].Total > accounts[j].Total
		}
		return accounts[i].Key < accounts[j].Key
	})

	var start, end *int64
	if in.CoverageStartAt > 0 {
		v := in.CoverageStartAt * 1000
		start = &v
	}
	if in.CoverageEndAt > 0 {
		v := in.CoverageEndAt * 1000
		end = &v
	}

	note := "网关按请求累加输入/输出 token：没有缓存读写与思考 token，也没有项目/会话维度"
	if in.Failures > 0 {
		note += "；含失败请求 " + itoa(int(in.Failures)) + " 次"
	}

	return SourceJSON{
		Source: "gateway",
		Kind:   KindGateway,
		Label:  "网关反代",
		// 网关来源恒为「存在」（它读的是我们自己的统计文件）。有没有数据看
		// summary.records —— 页面用 records==0 走空态，而不是把它标成不可用。
		Available: true,
		Note:      note,
		Summary:   summary,
		Models:    models,
		Projects:  []GroupJSON{}, // 网关不知道项目
		Sessions:  accounts,      // 复用会话槽位承载「账号」维度
		Daily:     daily,
		// 网关侧没有「模型 × 天」的交叉数据（天桶里只有总量与输入/输出），
		// 因此给空表而不是 null —— 前端按数组处理，null 会让整页崩。
		DailyByModel:    map[string][]GroupJSON{},
		Requests:        []RequestRowJSON{},
		CoverageStartAt: start,
		CoverageEndAt:   end,
	}
}
