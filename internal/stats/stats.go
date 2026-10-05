// Package stats 是按小时聚合的**持久化**用量统计。
//
// 为什么不能复用 internal/metrics：那个包是滚动窗口（最近 5 小时）的内存指标，
// 用途是「诊断当前是否变慢」，均值被历史样本拖住会失去意义，所以它天然不该保留历史。
// 而趋势图要的是「过去 7 天每天用了多少」——两个需求对时间维度的要求正好相反
// （一个要短窗口新样本、一个要长历史），所以分成两个包，各司其职：
//
//	metrics —— 滚动窗口均值（TTFT / 总耗时），给监控页诊断用
//	stats   —— 按小时归档的计数与余额采样，给用量页画趋势用
//
// 持久化到工作目录的 wb-stats.json。设计取舍：
//   - **按小时聚合**而不是逐请求落盘：逐请求会在高频使用下写出巨大文件，
//     而这些数据（每模型请求数/token/首字合计）本来也只需要小时粒度。
//   - 只保留最近 30 天桶，超期淘汰；余额采样按小时保留最后一个点。
//   - 文件损坏时静默从空开始（统计是可再生的诊断数据，不值得为它阻塞启动）。
package stats

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// schemaVersion 3：天桶的日期键从 **UTC 日** 改为 **本地日历日**
	// （见 localDayIndex 的注释：UTC 日会让「今天」从本地 08:00 起算，
	// 与官方账本的本地零点错开 8 小时，做差集时系统性失真）。
	// 2：新增按天桶、账号维度、输入/输出 token 拆分。
	// 旧版（1、2）的数据依然能读 —— 缺的字段按零值处理，不会丢历史。
	//
	// 关于 2 → 3 的既有数据：天桶的键是「自纪元起的天数」，改口径后同一个
	// 数字对应的日期可能平移一天，因此历史天桶的**日期标签**在跨版本时可能
	// 偏移一天（桶里的累计值本身不丢）。本次升级时实测两份数据目录的天桶
	// 都是空的，所以没有实际影响；若日后还有旧数据，这一点需要人工核对。
	schemaVersion = 3
	// retentionHours 是小时桶保留时长（30 天）。小时桶只服务 24h/7d 这类近期视图。
	retentionHours = 30 * 24
	// retentionDays 是天桶保留时长（365 天）。趋势与活跃度要跨月跨季看，
	// 按天归档必须留够一年，否则「过去一年用了多少」无从谈起。
	retentionDays = 365
	// maxCreditPoints 是余额采样点上限（30 天 × 24 点 = 720，留些余量）。
	maxCreditPoints = 800
)

// RecordInput 是一次已完成请求的统计输入。
type RecordInput struct {
	Model   string
	Account string // 账号标识，空表示未知（不影响其它维度统计）
	OK      bool
	// InputTokens / OutputTokens 是上游 usage 里的输入与输出拆分。
	InputTokens  int64
	OutputTokens int64
	// TotalTokens 是上游给出的总量。**有的上游只给 total_tokens 而不分输入输出**，
	// 那种情况下上面两个字段为 0，但总量必须照记 —— 否则统计会凭空少一块，
	// 表现为「请求数对得上、token 数偏低」。
	TotalTokens int64
	// TTFTMs <= 0 表示没有首字样本（非流式聚合取不到），不计入均值而不计为 0。
	TTFTMs int64
}

// tokens 返回本次请求应计入的总 token。
func (in RecordInput) tokens() int64 {
	if in.TotalTokens > 0 {
		return in.TotalTokens
	}
	return in.InputTokens + in.OutputTokens
}

// Counter 是一组可累加的计量字段。
//
// 抽出来是为了让「小时桶 / 天桶 / 模型维度 / 账号维度」四处共用同一份累加实现 ——
// 否则同一套加法要抄四遍，日后新增指标必然漏改其中一两处。
type Counter struct {
	Requests     int64 `json:"requests"`
	Failures     int64 `json:"failures"`
	Tokens       int64 `json:"tokens"`
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	TTFTSumMs    int64 `json:"ttft_sum_ms,omitempty"`
	TTFTCount    int64 `json:"ttft_count,omitempty"`
}

// add 累加一次请求。
func (c *Counter) add(in RecordInput) {
	c.Requests++
	if !in.OK {
		c.Failures++
	}
	if in.InputTokens > 0 {
		c.InputTokens += in.InputTokens
	}
	if in.OutputTokens > 0 {
		c.OutputTokens += in.OutputTokens
	}
	if n := in.tokens(); n > 0 {
		c.Tokens += n
	}
	if in.TTFTMs > 0 {
		c.TTFTSumMs += in.TTFTMs
		c.TTFTCount++
	}
}

// AvgTTFTMs 返回平均首字延迟；无样本时返回 nil（前端显示「-」而不是 0）。
func (c *Counter) AvgTTFTMs() *float64 {
	if c.TTFTCount == 0 {
		return nil
	}
	v := float64(c.TTFTSumMs) / float64(c.TTFTCount)
	return &v
}

// ModelHour 是单模型聚合。保留为 Counter 的别名，兼容既有引用。
type ModelHour = Counter

// HourBucket 是一个小时的聚合。
type HourBucket struct {
	Hour int64 `json:"hour"` // Unix 小时（秒 / 3600）
	Counter
	Models   map[string]*Counter `json:"models,omitempty"`
	Accounts map[string]*Counter `json:"accounts,omitempty"`
}

// DayBucket 是一天的聚合。与小时桶同构，只有粒度不同。
type DayBucket struct {
	Day int64 `json:"day"` // 本地日历日索引（见 localDayIndex），不是 UTC 天
	Counter
	Models   map[string]*Counter `json:"models,omitempty"`
	Accounts map[string]*Counter `json:"accounts,omitempty"`
}

// CreditPoint 是一次余额采样。
type CreditPoint struct {
	TS       int64              `json:"ts"`
	Total    float64            `json:"total"`
	Accounts map[string]float64 `json:"accounts,omitempty"`
}

type document struct {
	Schema  int                    `json:"schema"`
	Hours   map[string]*HourBucket `json:"hours"`
	Days    map[string]*DayBucket  `json:"days"`
	Credits []CreditPoint          `json:"credits"`
	Updated int64                  `json:"updated_at"`
	SinceAt int64                  `json:"since_at"`
}

// Store 是统计仓库。
type Store struct {
	path string

	mu    sync.Mutex
	doc   document
	dirty bool
	Logf  func(format string, args ...any)
}

// New 构造仓库（不读文件，需另调 Load）。
func New(path string) *Store {
	return &Store{
		path: path,
		doc: document{
			Schema:  schemaVersion,
			Hours:   map[string]*HourBucket{},
			Days:    map[string]*DayBucket{},
			SinceAt: time.Now().Unix(),
		},
	}
}

func (s *Store) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Load 读入磁盘上的统计文件。文件不存在或损坏时从空开始（不返回错误）。
func (s *Store) Load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.logf("[统计] 读取 %s 失败，本次从空统计开始: %v", filepath.Base(s.path), err)
		}
		return
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 统计是可再生数据，损坏时重建比报错中止更有价值
		s.logf("[统计] %s 解析失败（将重建）: %v", filepath.Base(s.path), err)
		return
	}
	// 只要小时桶在就接受：schema 1 → 2 只是**新增**字段（按天桶、账号维度、
	// 输入/输出拆分），缺失部分按零值处理即可。为一次版本号变化丢掉全部历史统计
	// 不划算 —— 这些数据一旦丢就不可再生。
	if doc.Hours == nil {
		s.logf("[统计] %s 缺少小时桶，将重建", filepath.Base(s.path))
		return
	}
	if doc.Schema != schemaVersion {
		s.logf("[统计] %s schema %d → %d，保留既有数据并补齐新字段", filepath.Base(s.path), doc.Schema, schemaVersion)
		doc.Schema = schemaVersion
	}
	if doc.Days == nil {
		doc.Days = map[string]*DayBucket{}
	}
	s.mu.Lock()
	s.doc = doc
	s.pruneLocked()
	s.mu.Unlock()
	s.logf("[统计] 已载入 %d 个小时桶、%d 个天桶、%d 个余额采样点",
		len(doc.Hours), len(doc.Days), len(doc.Credits))
}

// Save 落盘（原子替换）。
func (s *Store) Save() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.doc.Updated = time.Now().Unix()
	data, err := json.Marshal(s.doc)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// FlushIfDirty 在有改动时落盘（供定时器周期调用）。
func (s *Store) FlushIfDirty() {
	if err := s.Save(); err != nil {
		s.logf("[统计] 写盘失败: %v", err)
	}
}

// Record 记一次已完成的请求。
//
// 一次写入同时更新**小时桶与天桶**，两者都维护「总量 / 按模型 / 按账号」三个维度。
// 为什么两份都留而不是查时聚合：天桶若由小时桶推导，就受限于小时桶 30 天的保留期，
// 「过去一年」永远算不出来；小时桶若由天桶推导，近期视图又会丢掉小时分辨率。
// 各自保留各自精度的历史，代价只是一次请求多累加几个计数器。
func (s *Store) Record(in RecordInput) {
	if in.Model == "" {
		return
	}
	now := time.Now().Unix()

	s.mu.Lock()
	defer s.mu.Unlock()

	h := s.bucketLocked(now / 3600)
	addToBucket(&h.Counter, h.Models, h.Accounts, in)

	d := s.dayBucketLocked(localDayIndex(time.Unix(now, 0)))
	addToBucket(&d.Counter, d.Models, d.Accounts, in)

	s.dirty = true
}

// addToBucket 把一个请求同时累加到「时间片总量」以及它的模型/账号维度。
//
// 账号为空时跳过账号维度：与其造一个 unknown 桶污染占比图，不如让它不计入 ——
// 账号维度本来就是给「按账号看消耗」用的，未知来源混进来只会误导。
func addToBucket(total *Counter, models, accounts map[string]*Counter, in RecordInput) {
	total.add(in)

	m := models[in.Model]
	if m == nil {
		m = &Counter{}
		models[in.Model] = m
	}
	m.add(in)

	if in.Account == "" {
		return
	}
	a := accounts[in.Account]
	if a == nil {
		a = &Counter{}
		accounts[in.Account] = a
	}
	a.add(in)
}

// RecordCredits 记录一次余额采样（同一小时内只保留最后一个点）。
//
// 只留最后一个点而不是求平均：余额是**单调消耗**的存量指标，
// 求平均会掩盖掉「这一小时用了多少」，而画趋势图正是要看这个变化。
func (s *Store) RecordCredits(accounts map[string]float64) {
	if len(accounts) == 0 {
		return
	}
	total := 0.0
	cp := CreditPoint{TS: time.Now().Unix(), Total: total, Accounts: map[string]float64{}}
	for k, v := range accounts {
		cp.Accounts[k] = v
		cp.Total += v
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	hour := cp.TS / 3600
	if n := len(s.doc.Credits); n > 0 && s.doc.Credits[n-1].TS/3600 == hour {
		s.doc.Credits[n-1] = cp
	} else {
		s.doc.Credits = append(s.doc.Credits, cp)
	}
	if len(s.doc.Credits) > maxCreditPoints {
		drop := len(s.doc.Credits) - maxCreditPoints
		s.doc.Credits = append(s.doc.Credits[:0], s.doc.Credits[drop:]...)
	}
	s.dirty = true
}

// bucketLocked 取（或创建）某小时的桶，顺带淘汰超期数据。
func (s *Store) bucketLocked(hour int64) *HourBucket {
	if b, ok := s.doc.Hours[key(hour)]; ok {
		if b.Models == nil {
			b.Models = map[string]*Counter{}
		}
		if b.Accounts == nil {
			b.Accounts = map[string]*Counter{}
		}
		return b
	}
	s.pruneLocked()
	b := &HourBucket{Hour: hour, Models: map[string]*Counter{}, Accounts: map[string]*Counter{}}
	s.doc.Hours[key(hour)] = b
	return b
}

// dayBucketLocked 取（或创建）某天的桶。
func (s *Store) dayBucketLocked(day int64) *DayBucket {
	if b, ok := s.doc.Days[dayKey(day)]; ok {
		if b.Models == nil {
			b.Models = map[string]*Counter{}
		}
		if b.Accounts == nil {
			b.Accounts = map[string]*Counter{}
		}
		return b
	}
	s.pruneLocked()
	b := &DayBucket{Day: day, Models: map[string]*Counter{}, Accounts: map[string]*Counter{}}
	s.doc.Days[dayKey(day)] = b
	return b
}

func (s *Store) pruneLocked() {
	hourCutoff := time.Now().Unix()/3600 - retentionHours
	for k, b := range s.doc.Hours {
		if b.Hour < hourCutoff {
			delete(s.doc.Hours, k)
		}
	}
	// 天桶按天淘汰：它要留够一年，不能跟着小时桶的 30 天走。
	dayCutoff := localDayIndex(time.Now()) - retentionDays
	for k, b := range s.doc.Days {
		if b.Day < dayCutoff {
			delete(s.doc.Days, k)
		}
	}
	if n := len(s.doc.Credits); n > 0 {
		kept := s.doc.Credits[:0]
		for _, p := range s.doc.Credits {
			if p.TS/3600 >= hourCutoff {
				kept = append(kept, p)
			}
		}
		s.doc.Credits = kept
	}
}

func key(hour int64) string {
	return time.Unix(hour*3600, 0).UTC().Format("2006010215")
}

// localDayIndex 把一个时刻映射到**本地日历日**的稳定整数键。
//
// 为什么不能沿用 `t.Unix()/86400`：那是 **UTC** 日边界。本机在 GMT+8，于是
// 「今天」这个桶实际从本地 08:00 才开始，而官方账本（`startTime` 取本地
// 00:00）是从本地零点切的 —— 两边错开 8 小时。
//
// 这个错位在**做差集时是致命的**：用量统计要把「官方直连」算成
// 官方账本 − 反代计数，窗口起点差 8 小时会让短周期（尤其「今天」）的差集
// 系统性偏大，且偏多少取决于当天的流量分布，不是能靠文案遮掉的误差。
//
// 实现取本地年月日、再按 UTC 重新构造零点，于是同一本地日内取值恒定；
// 刻意不用 `Add(-offset)` 那种写法，是因为它跨夏令时会漂
// （本地日可能变成 23 或 25 小时），而这里只关心「哪一天」。
func localDayIndex(t time.Time) int64 {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

// dayKey 把本地日索引格式化成 yyyymmdd。
//
// 索引本身已经是「某本地日的零点按 UTC 表示」的整数，所以这里必须用 UTC 格式化
// 才能取回那个日期；用本地格式化会在负时区（如 UTC-5）渲染成前一天。
func dayKey(day int64) string {
	return time.Unix(day*86400, 0).UTC().Format("20060102")
}

// -----------------------------------------------------------------------------
// 查询
// -----------------------------------------------------------------------------

// Point 是时间序列上的一个点（已补齐缺失小时）。
type Point struct {
	TS       int64    `json:"ts"`
	Label    string   `json:"label"`
	Requests int64    `json:"requests"`
	Failures int64    `json:"failures"`
	Tokens   int64    `json:"tokens"`
	Credits  *float64 `json:"credits"`
}

// Snapshot 是统计查询结果。
type Snapshot struct {
	RangeHours      int           `json:"range_hours"`
	GeneratedAt     int64         `json:"generated_at"`
	SinceAt         int64         `json:"since_at"`
	Totals          Totals        `json:"totals"`
	Series          []Point       `json:"series"`
	TopModels       []ModelRollup `json:"top_models"`
	HourlyAvgTokens float64       `json:"hourly_avg_tokens"`
}

// Totals 是窗口内合计。
type Totals struct {
	Requests    int64    `json:"requests"`
	Failures    int64    `json:"failures"`
	Tokens      int64    `json:"tokens"`
	CreditsNow  *float64 `json:"credits_now"`
	CreditsUsed *float64 `json:"credits_used"`
	// Hours 是窗口内**有请求**的小时数，用于计算平均值时不被空小时摊薄。
	Hours int `json:"hours"`
}

// ModelRollup 是窗口内的模型排行项。
type ModelRollup struct {
	Model     string   `json:"model"`
	Requests  int64    `json:"requests"`
	Failures  int64    `json:"failures"`
	Tokens    int64    `json:"tokens"`
	AvgTTFTMs *float64 `json:"avg_ttft_ms"`
}

// Query 返回最近 hours 小时的统计。
//
// 缺失的小时会被补成 0 点：图表必须连续，否则 recharts 会把不连续的日期连成一条斜线，
// 让人误以为那段时间有流量。
func (s *Store) Query(hours int) Snapshot {
	if hours <= 0 {
		hours = 24
	}
	if hours > retentionHours {
		hours = retentionHours
	}
	now := time.Now()
	nowHour := now.Unix() / 3600
	from := nowHour - int64(hours) + 1

	s.mu.Lock()
	defer s.mu.Unlock()

	snap := Snapshot{
		RangeHours:  hours,
		GeneratedAt: now.Unix(),
		SinceAt:     s.doc.SinceAt,
		Series:      make([]Point, 0, hours),
		TopModels:   []ModelRollup{},
	}

	creditsByHour := map[int64]float64{}
	for _, c := range s.doc.Credits {
		creditsByHour[c.TS/3600] = c.Total
	}

	rollup := map[string]*ModelRollup{}
	for h := from; h <= nowHour; h++ {
		p := Point{TS: h * 3600, Label: time.Unix(h*3600, 0).Format("01-02 15:00")}
		if h == nowHour {
			p.Label = "本小时"
		}
		if b, ok := s.doc.Hours[key(h)]; ok {
			p.Requests = b.Requests
			p.Failures = b.Failures
			p.Tokens = b.Tokens
			snap.Totals.Requests += b.Requests
			snap.Totals.Failures += b.Failures
			snap.Totals.Tokens += b.Tokens
			if b.Requests > 0 {
				snap.Totals.Hours++
			}
			for name, m := range b.Models {
				r := rollup[name]
				if r == nil {
					r = &ModelRollup{Model: name}
					rollup[name] = r
				}
				r.Requests += m.Requests
				r.Failures += m.Failures
				r.Tokens += m.Tokens
				if m.TTFTCount > 0 {
					if r.AvgTTFTMs == nil {
						v := 0.0
						r.AvgTTFTMs = &v
					}
					// 用「合计 / 计数」累计，最后再除，避免逐点平均引入的加权误差
					*r.AvgTTFTMs += float64(m.TTFTSumMs)
				}
			}
		}
		if v, ok := creditsByHour[h]; ok {
			val := v
			p.Credits = &val
		}
		snap.Series = append(snap.Series, p)
	}

	// 首尾余额点算窗口内消耗
	var first, last *float64
	for _, p := range snap.Series {
		if p.Credits != nil {
			if first == nil {
				v := *p.Credits
				first = &v
			}
			v := *p.Credits
			last = &v
		}
	}
	if last != nil {
		snap.Totals.CreditsNow = last
	}
	if first != nil && last != nil {
		used := *first - *last
		snap.Totals.CreditsUsed = &used
	}

	// 模型排行：ttft 需要重新按计数平均
	for name, r := range rollup {
		var sumMs int64
		var count int64
		for h := from; h <= nowHour; h++ {
			if b, ok := s.doc.Hours[key(h)]; ok {
				if m, ok := b.Models[name]; ok {
					sumMs += m.TTFTSumMs
					count += m.TTFTCount
				}
			}
		}
		if count > 0 {
			v := float64(sumMs) / float64(count)
			r.AvgTTFTMs = &v
		} else {
			r.AvgTTFTMs = nil
		}
		snap.TopModels = append(snap.TopModels, *r)
	}
	sort.Slice(snap.TopModels, func(i, j int) bool {
		if snap.TopModels[i].Requests != snap.TopModels[j].Requests {
			return snap.TopModels[i].Requests > snap.TopModels[j].Requests
		}
		return snap.TopModels[i].Model < snap.TopModels[j].Model
	})
	if len(snap.TopModels) > 20 {
		snap.TopModels = snap.TopModels[:20]
	}
	if snap.Totals.Hours > 0 {
		snap.HourlyAvgTokens = float64(snap.Totals.Tokens) / float64(snap.Totals.Hours)
	}
	return snap
}

// Purge 清空全部统计（面板上的「重置统计」用）。
func (s *Store) Purge() error {
	s.mu.Lock()
	s.doc = document{
		Schema:  schemaVersion,
		Hours:   map[string]*HourBucket{},
		Days:    map[string]*DayBucket{},
		SinceAt: time.Now().Unix(),
	}
	s.dirty = true
	s.mu.Unlock()
	return s.Save()
}

// -----------------------------------------------------------------------------
// 按天查询（用量统计 / 积分统计两个页面用）
// -----------------------------------------------------------------------------

// DayPoint 是按天序列上的一个点。
type DayPoint struct {
	TS           int64    `json:"ts"`
	Label        string   `json:"label"`
	Requests     int64    `json:"requests"`
	Failures     int64    `json:"failures"`
	Tokens       int64    `json:"tokens"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	Credits      *float64 `json:"credits"` // 当天最后一个余额采样
}

// AccountRollup 是账号维度的聚合项。
type AccountRollup struct {
	Account      string   `json:"account"`
	Requests     int64    `json:"requests"`
	Failures     int64    `json:"failures"`
	Tokens       int64    `json:"tokens"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	AvgTTFTMs    *float64 `json:"avg_ttft_ms"`
}

// DailyTotals 是窗口内按天口径的合计。
type DailyTotals struct {
	Requests     int64    `json:"requests"`
	Failures     int64    `json:"failures"`
	Tokens       int64    `json:"tokens"`
	InputTokens  int64    `json:"input_tokens"`
	OutputTokens int64    `json:"output_tokens"`
	CreditsNow   *float64 `json:"credits_now"`
	CreditsUsed  *float64 `json:"credits_used"`
	// ActiveDays 是窗口内**有请求**的天数。算日均时必须用它做分母：
	// 一个月里只用过 3 天，除以 30 会得出一个毫无意义的「日均」。
	ActiveDays int `json:"active_days"`
}

// DailySnapshot 是按天统计的查询结果。
type DailySnapshot struct {
	RangeDays   int             `json:"range_days"`
	GeneratedAt int64           `json:"generated_at"`
	SinceAt     int64           `json:"since_at"`
	Totals      DailyTotals     `json:"totals"`
	Series      []DayPoint      `json:"series"`
	TopModels   []ModelRollup   `json:"top_models"`
	Accounts    []AccountRollup `json:"accounts"`
}

// mergeCounter 把一个计数器的值累加进另一个（把按天的维度汇总到整个窗口）。
func mergeCounter(dst, src *Counter) {
	dst.Requests += src.Requests
	dst.Failures += src.Failures
	dst.Tokens += src.Tokens
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.TTFTSumMs += src.TTFTSumMs
	dst.TTFTCount += src.TTFTCount
}

// dailyWindow 返回按天视图的窗口 [from, nowDay]，两端都是**本地日索引**（闭区间）。
//
// 抽成函数是为了让它可测：QueryDaily 内部取 time.Now()，测试没法构造
// 「本地零点刚过」这种边界时刻；而窗口口径恰恰是这次修时区 bug 的落点。
func dailyWindow(now time.Time, days int) (int64, int64) {
	nowDay := localDayIndex(now)
	return nowDay - int64(days) + 1, nowDay
}

// QueryDaily 返回最近 days 天的统计，并补齐缺失的天。
//
// 补齐的理由与 Query 一致：图表必须连续，否则 recharts 会把断点连成斜线，
// 让人误以为那几天有流量。余额口径也与小时视图保持一致 —— 存量指标取**当天最后一个**
// 采样点，而不是求平均，因为平均会掩盖掉「这天到底消耗了多少」。
func (s *Store) QueryDaily(days int) DailySnapshot {
	if days <= 0 {
		days = 30
	}
	if days > retentionDays {
		days = retentionDays
	}
	now := time.Now()
	from, nowDay := dailyWindow(now, days)

	s.mu.Lock()
	defer s.mu.Unlock()

	snap := DailySnapshot{
		RangeDays:   days,
		GeneratedAt: now.Unix(),
		SinceAt:     s.doc.SinceAt,
		Series:      make([]DayPoint, 0, days),
		TopModels:   []ModelRollup{},
		Accounts:    []AccountRollup{},
	}

	creditsByDay := map[int64]float64{}
	for _, c := range s.doc.Credits {
		creditsByDay[localDayIndex(time.Unix(c.TS, 0))] = c.Total
	}

	modelAgg := map[string]*Counter{}
	accountAgg := map[string]*Counter{}
	var firstCredit, lastCredit *float64

	for d := from; d <= nowDay; d++ {
		p := DayPoint{TS: d * 86400, Label: time.Unix(d*86400, 0).UTC().Format("01-02")}
		if d == nowDay {
			p.Label = "今天"
		}
		if v, ok := creditsByDay[d]; ok {
			v := v
			p.Credits = &v
			if firstCredit == nil {
				firstCredit = &v
			}
			lastCredit = &v
		}
		if b, ok := s.doc.Days[dayKey(d)]; ok {
			p.Requests = b.Requests
			p.Failures = b.Failures
			p.Tokens = b.Tokens
			p.InputTokens = b.InputTokens
			p.OutputTokens = b.OutputTokens

			snap.Totals.Requests += b.Requests
			snap.Totals.Failures += b.Failures
			snap.Totals.Tokens += b.Tokens
			snap.Totals.InputTokens += b.InputTokens
			snap.Totals.OutputTokens += b.OutputTokens
			if b.Requests > 0 {
				snap.Totals.ActiveDays++
			}
			for name, m := range b.Models {
				agg := modelAgg[name]
				if agg == nil {
					agg = &Counter{}
					modelAgg[name] = agg
				}
				mergeCounter(agg, m)
			}
			for id, a := range b.Accounts {
				agg := accountAgg[id]
				if agg == nil {
					agg = &Counter{}
					accountAgg[id] = agg
				}
				mergeCounter(agg, a)
			}
		}
		snap.Series = append(snap.Series, p)
	}

	snap.Totals.CreditsNow = lastCredit
	if firstCredit != nil && lastCredit != nil {
		used := *firstCredit - *lastCredit
		snap.Totals.CreditsUsed = &used
	}

	for name, m := range modelAgg {
		snap.TopModels = append(snap.TopModels, ModelRollup{
			Model:     name,
			Requests:  m.Requests,
			Failures:  m.Failures,
			Tokens:    m.Tokens,
			AvgTTFTMs: m.AvgTTFTMs(),
		})
	}
	sort.Slice(snap.TopModels, func(i, j int) bool {
		if snap.TopModels[i].Tokens != snap.TopModels[j].Tokens {
			return snap.TopModels[i].Tokens > snap.TopModels[j].Tokens
		}
		return snap.TopModels[i].Requests > snap.TopModels[j].Requests
	})
	if len(snap.TopModels) > 20 {
		snap.TopModels = snap.TopModels[:20]
	}

	for id, a := range accountAgg {
		snap.Accounts = append(snap.Accounts, AccountRollup{
			Account:      id,
			Requests:     a.Requests,
			Failures:     a.Failures,
			Tokens:       a.Tokens,
			InputTokens:  a.InputTokens,
			OutputTokens: a.OutputTokens,
			AvgTTFTMs:    a.AvgTTFTMs(),
		})
	}
	sort.Slice(snap.Accounts, func(i, j int) bool {
		if snap.Accounts[i].Tokens != snap.Accounts[j].Tokens {
			return snap.Accounts[i].Tokens > snap.Accounts[j].Tokens
		}
		return snap.Accounts[i].Requests > snap.Accounts[j].Requests
	})

	return snap
}
