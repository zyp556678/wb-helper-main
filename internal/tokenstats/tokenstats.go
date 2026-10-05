// Package tokenstats 统计**本地会话日志**里的 Token 用量。
//
// 为什么需要它：网关自己的计数（internal/stats）只能看见**经过网关**的流量。
// 客户端直连官方 API 的那部分用量在网关里完全不存在，而官方账本只有积分与
// 请求数、没有 token 明细。要做「这些 token 是谁用的、用在哪」就必须读客户端
// 自己写的会话日志 —— 这也是 wb-switch 的做法。
//
// 数据来源（四个，互不合并）：
//
//	workbuddy      ~/.workbuddy/projects/**/*.jsonl
//	workbuddy-ai   ~/.workbuddy-ai/{projects,sessions}/**/*.jsonl
//	codebuddy-cli  ~/.codebuddy/projects/**/*.jsonl
//	codebuddy-ide  <LOCALAPPDATA>/CodeBuddyExtension/Data/**/history/**/index.json
//
// 国内外两版**分开统计**：它们是不同账号体系，混算会让用量无法归因。
//
// 三条硬性约束（都来自真实数据核对，改之前先读）：
//
//  1. **绝不读 `providerData.reasoning`**。那是模型思考过程的正文，属隐私红线。
//     只需要 `providerData.rawUsage` 里的**计数**（completion_thinking_tokens 等）。
//  2. **行的长度没有上限**。工具结果会写进日志，实测单行可达数 MB；而
//     `bufio.Scanner` 默认上限 64KB，超了会静默 stop（表现为"少统计了一大截"）。
//     必须显式抬高缓冲区。
//  3. **缓存写入不在 `usage` 里**。实测 `message.usage` / `providerData.usage`
//     只有 input/output/total/cache_read；`cache_creation_input_tokens` 与
//     `prompt_cache_write_tokens` 只在 `providerData.rawUsage` 下。
package tokenstats

import (
	"os"
	"path/filepath"
	"time"
)

const (
	// maxLineBytes 单行上限。实测日志里工具结果那几行很大，64KB 的默认值会把
	// 它们当解析失败（甚至直接终止扫描）。给到 16MB，超出才算异常行。
	maxLineBytes = 16 << 20

	// unknownModel / unknownProject 是缺省字面量，与前端展示约定一致。
	unknownModel   = "未知模型"
	unknownProject = "未知项目"

	// requestWindow 明细返回条数；requestLimit 是内存硬上限。
	requestWindow = 500
	requestLimit  = 4000

	// scanCacheTTL 扫描结果缓存时长。
	//
	// switch 每次调用都全量重扫（它没有面板轮询）。本项目的面板会定时刷新，
	// 而本机日志有 100MB 量级，每次轮询都重扫会把面板拖垮，因此加一层 TTL 缓存。
	// 这是与 switch 的**有意偏离**，只在性能上，聚合口径完全一致。
	scanCacheTTL = 45 * time.Second
)

// Usage 是一次请求的 token 用量。
//
// Read 是缓存命中（input 已包含它），Write 是缓存写入（不计入 input）。
// 因此展示用总量 = Input + Output + Write —— **不能**再加 Read，否则重复计数。
type Usage struct {
	Input  int64
	Output int64
	Read   int64
	Write  int64
}

// total 展示用总量：input 已含 cache read，只追加 output 与 cache write。
func (u Usage) total() int64 { return u.Input + u.Output + u.Write }

type totals struct {
	usage   Usage
	records int64
}

func (t *totals) add(u Usage) {
	t.usage.Input += u.Input
	t.usage.Output += u.Output
	t.usage.Read += u.Read
	t.usage.Write += u.Write
	t.records++
}

// RequestRow 是一条请求明细。
type RequestRow struct {
	Timestamp int64
	Model     string
	Project   string
	SessionID string
	Title     string
	Usage     Usage
	Thinking  int64
}

// -----------------------------------------------------------------------------
// 对外 JSON（字段名与 wb-switch 的 types.ts 逐字一致，前端才能 1:1 复用版式）
// -----------------------------------------------------------------------------

// TotalsJSON 一组聚合值。
type TotalsJSON struct {
	Total         int64    `json:"total"`
	Input         int64    `json:"input"`
	Output        int64    `json:"output"`
	CacheRead     int64    `json:"cacheRead"`
	CacheWrite    int64    `json:"cacheWrite"`
	UncachedInput int64    `json:"uncachedInput"`
	Records       int64    `json:"records"`
	CacheHitRate  *float64 `json:"cacheHitRate"`
}

// GroupJSON 带 key 的聚合项（模型 / 项目 / 会话 / 按日 / 按小时）。
type GroupJSON struct {
	Key       string  `json:"key"`
	Title     *string `json:"title,omitempty"`
	Project   string  `json:"project,omitempty"`
	SessionID string  `json:"sessionId,omitempty"`
	TotalsJSON
}

// RequestRowJSON 明细行。
type RequestRowJSON struct {
	Timestamp     int64   `json:"timestamp"`
	Model         string  `json:"model"`
	Project       string  `json:"project"`
	SessionID     string  `json:"sessionId"`
	Title         *string `json:"title"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheRead     int64   `json:"cacheRead"`
	CacheWrite    int64   `json:"cacheWrite"`
	UncachedInput int64   `json:"uncachedInput"`
	Thinking      int64   `json:"thinking"`
	Total         int64   `json:"total"`
}

// SourceKind 区分两类数据源，前端据此决定哪些区块可展示。
type SourceKind string

const (
	// KindLocalLog 本地会话日志：全字段可用（含缓存读写、思考、项目、会话）。
	KindLocalLog SourceKind = "local-log"
	// KindGateway 网关反代计数：只有请求数与输入/输出 token。
	// 没有缓存读写、没有思考、没有项目/会话维度 —— 前端必须据此降级而不是显示 0。
	KindGateway SourceKind = "gateway"
)

// SourceJSON 单个数据源。
type SourceJSON struct {
	Source string     `json:"source"`
	Kind   SourceKind `json:"kind"`
	Label  string     `json:"label"`
	// Available=false 表示本机没有该来源的日志（例如未装国际版）。
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`

	Summary  TotalsJSON  `json:"summary"`
	Models   []GroupJSON `json:"models"`
	Projects []GroupJSON `json:"projects"`
	Sessions []GroupJSON `json:"sessions"`
	Daily    []GroupJSON `json:"daily"`
	// DailyByModel：模型 → 该模型的按天序列。趋势图按模型筛选时用它 ——
	// 只看按模型总量看不出「这个模型这几天在涨还是在落」。只保留量级靠前的模型。
	DailyByModel map[string][]GroupJSON `json:"dailyByModel"`
	Requests     []RequestRowJSON       `json:"requests"`

	FilesScanned    int    `json:"filesScanned"`
	ParseErrors     int    `json:"parseErrors"`
	CoverageStartAt *int64 `json:"coverageStartAt"`
	CoverageEndAt   *int64 `json:"coverageEndAt"`
}

// Statistics 顶层返回。
type Statistics struct {
	GeneratedAt int64        `json:"generatedAt"`
	RangeDays   int          `json:"rangeDays"`
	Sources     []SourceJSON `json:"sources"`
}

// -----------------------------------------------------------------------------
// 采集
// -----------------------------------------------------------------------------

type collector struct {
	total    totals
	models   map[string]*totals
	projects map[string]*totals
	daily    map[string]*totals
	// dailyByModel 是「模型 × 天」的交叉维度，供趋势图按模型筛选。
	// 只有它能回答「这个模型的用量这几天在涨还是在落」——
	// 按模型总量只是一个数，看不出趋势。
	dailyByModel map[string]map[string]*totals

	sessions  []sessionAgg
	requests  []RequestRow
	seen      map[fingerprint]struct{}
	parseErrs int
	files     int

	coverageStart *int64
	coverageEnd   *int64
}

type sessionAgg struct {
	key       string
	sessionID string
	project   string
	title     string
	totals    totals
}

type fingerprint struct {
	ts     int64
	input  int64
	output int64
	read   int64
	write  int64
	model  string
}

func newCollector() *collector {
	return &collector{
		models:       map[string]*totals{},
		projects:     map[string]*totals{},
		daily:        map[string]*totals{},
		dailyByModel: map[string]map[string]*totals{},
		seen:         map[fingerprint]struct{}{},
	}
}

// rootSpec 描述一个要扫描的日志根。
type rootSpec struct {
	source string
	label  string
	path   string
	// ide 为 true 时按 IDE 的 index.json 规则筛选，否则收 .jsonl。
	ide bool
}

// localRoots 返回本机的日志根。不存在的根由调用方跳过
// —— 未安装某个产品是常态，不该报错，只该标成「该来源不可用」。
func localRoots() []rootSpec {
	home, _ := os.UserHomeDir()
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		local = filepath.Join(home, "AppData", "Local")
	}
	return []rootSpec{
		{source: "workbuddy", label: "WorkBuddy", path: filepath.Join(home, ".workbuddy", "projects")},
		{source: "workbuddy-ai", label: "WorkBuddy 国际版", path: filepath.Join(home, ".workbuddy-ai", "projects")},
		{source: "workbuddy-ai", label: "WorkBuddy 国际版", path: filepath.Join(home, ".workbuddy-ai", "sessions")},
		{source: "codebuddy-cli", label: "CodeBuddy CLI", path: filepath.Join(home, ".codebuddy", "projects")},
		{source: "codebuddy-ide", label: "CodeBuddy IDE", path: filepath.Join(local, "CodeBuddyExtension", "Data"), ide: true},
	}
}
