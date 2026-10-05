// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// ## 它在补什么
//
// 本项目原有的观测是**按模型聚合**的（internal/metrics：某模型请求数/失败数/TTFB），
// 以及**事件流**（internal/eventlog：发生了什么）。两者都回答不了
// 「刚才那次 502 是哪一次请求、走的哪个账号、卡了多久」—— 缺的是**请求级明细**。
//
// ## 两条硬约束
//
//  1. **不写敏感内容**：提示词、响应正文、Authorization、完整 UID 一律不落盘。
//     账号只存「昵称(uid8)」标签，够人辨认，不足以复原凭据。
//  2. **不阻塞请求**：归档走有界队列，满了**丢弃并计数**，绝不让日志写盘拖住模型请求。
//     指标本身（内存计数）始终可用，归档是可选增强。
//
// 内存指标有界保存最近 100 条；磁盘归档按天 + 按大小轮转，带保留天数与总量上限。
package reqlog

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	recentLimit    = 100
	defaultFileMax = int64(16 << 20)
	defaultQueue   = 1024
	defaultReadMax = 1000
	// ArchiveFileGlob 是归档文件名模式（requests-YYYY-MM-DD[-N].jsonl）。
	// 导出给面板的「错误日志」路径展示与最近日志下载复用，
	// 避免第三处再写一遍这个命名约定。
	ArchiveFileGlob = "requests-*.jsonl"
)

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

// 请求结局。区分 http_error 与 stream_error 是有用的：
// 前者是上游明确拒绝（可重试/需换号），后者是流读到一半断了（多为网络）。
const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
}

// Event 是一条脱敏请求记录。Account 只保存「昵称(uid8)」标签，不保存完整 UID。
type Event struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Path             string    `json:"path"`
	Account          string    `json:"account,omitempty"`
	Model            string    `json:"model,omitempty"`
	Status           int       `json:"status"`
	OK               bool      `json:"ok"`
	Outcome          string    `json:"outcome"`
	DurationMs       int64     `json:"duration_ms"`
	TTFBMs           int64     `json:"ttfb_ms,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	TotalTokens      int64     `json:"total_tokens,omitempty"`
	Credit           float64   `json:"credit,omitempty"`
	HasCredit        bool      `json:"credit_known"`
}

// Filter 用于从归档中筛选最近记录。
type Filter struct {
	Outcome string
	Account string
	Model   string
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled       bool   `json:"enabled"`
	Dir           string `json:"dir,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// Snapshot 是进程内指标的对外形态。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	recent      []Event
	archive     *archiveWriter
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	r := &Recorder{started: time.Now(), recent: make([]Event, 0, recentLimit)}
	r.archive = newArchiveWriter(cfg)
	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	// 头插 + 截断：recent 永远是「最近 N 条、新的在前」。
	r.recent = append([]Event{e}, r.recent...)
	if len(r.recent) > recentLimit {
		r.recent = r.recent[:recentLimit]
	}
	r.mu.Unlock()
	if r.archive != nil {
		r.archive.enqueue(e)
	}
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		// Recent 必须是 `[]` 而不是 nil：Go 的 nil slice 会序列化成 `null`，
		// 前端拿到后 `.length` / `.map` 直接抛异常 → **整页白屏**。
		// 实测踩过：刚启动、还没有任何请求时打开「请求流水」页就白屏。
		return Snapshot{Recent: []Event{}}
	}
	r.mu.Lock()
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
		// 同上：`append([]Event(nil), 空...)` 返回的仍是 nil，
		// 必须给一个非 nil 的基底切片。
		Recent: append(make([]Event, 0, len(r.recent)), r.recent...),
	}
	if r.completed > 0 {
		s.Failed = r.completed - r.succeeded
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		s.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	r.mu.Unlock()
	if r.archive != nil {
		s.Archive = r.archive.stats()
	}
	return s
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
//
// **永远返回非 nil 切片**（没有归档时返回空切片）：nil 会被序列化成 `null`，
// 前端 `entries.length` 抛异常整页白屏。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return []Event{}, nil
	}
	rows, err := r.archive.read(limit, filter)
	if err != nil {
		return []Event{}, err
	}
	if rows == nil {
		return []Event{}, nil
	}
	return rows, nil
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.close()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

// -----------------------------------------------------------------------------
// 归档写入
// -----------------------------------------------------------------------------

type archiveWriter struct {
	cfg       Config
	ch        chan Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
	lastErrMu sync.Mutex
	lastErr   string

	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64
}

func newArchiveWriter(cfg Config) *archiveWriter {
	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		return &archiveWriter{cfg: cfg}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 100 << 20
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	w := &archiveWriter{
		cfg:  cfg,
		ch:   make(chan Event, cfg.QueueSize),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		w.setErr(err)
		w.cfg.Enabled = false
		close(w.done)
		return w
	}
	go w.run()
	return w
}

func (w *archiveWriter) enabled() bool {
	return w != nil && w.cfg.Enabled && w.cfg.Dir != "" && w.done != nil
}

// enqueue 非阻塞投递：队列满就丢弃并计数。
//
// 这是整个模块最重要的性质 —— 日志写盘慢绝不能拖住模型请求。
// 宁可丢日志（有 dropped_writes 计数可查），不可让请求变慢。
func (w *archiveWriter) enqueue(e Event) {
	if !w.enabled() {
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

func (w *archiveWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		case <-ticker.C:
			w.flush()
			w.prune()
		case <-w.stop:
			// 关闭前把队列里剩的写完，避免丢掉最后一批
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(e)
				default:
					w.flush()
					w.closeFile()
					return
				}
			}
		}
	}
}

func (w *archiveWriter) writeEvent(e Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		w.setErr(err)
		return
	}
	now := e.Time
	if now.IsZero() {
		now = time.Now()
	}
	day := now.Format("2006-01-02")
	if w.file == nil || w.day != day {
		if err := w.openFile(now, false); err != nil {
			w.setErr(err)
			return
		}
	}
	if w.size >= w.cfg.FileMaxBytes {
		if err := w.openFile(now, true); err != nil {
			w.setErr(err)
			return
		}
	}
	n, err := w.buf.Write(append(raw, '\n'))
	if err != nil {
		w.setErr(err)
		return
	}
	w.size += int64(n)
}

func (w *archiveWriter) openFile(now time.Time, rotate bool) error {
	w.closeFile()
	day := now.Format("2006-01-02")
	path := ""
	if !rotate {
		path = latestArchiveForDay(w.cfg.Dir, day)
	}
	if path == "" {
		path = nextArchivePath(w.cfg.Dir, day)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.buf = bufio.NewWriter(f)
	w.path = path
	w.day = day
	w.size = info.Size()
	return nil
}

// latestArchiveForDay 找当天已有的归档文件（重启后接着写，不新开一个）。
func latestArchiveForDay(dir, day string) string {
	matches, err := filepath.Glob(filepath.Join(dir, "requests-"+day+"*.jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}

// nextArchivePath 生成当天下一个序号的文件名。
func nextArchivePath(dir, day string) string {
	prefix := filepath.Join(dir, "requests-"+day)
	for i := 0; ; i++ {
		p := prefix + "-" + strconv.Itoa(i) + ".jsonl"
		if i == 0 {
			p = prefix + ".jsonl"
		}
		if !fileExists(p) {
			return p
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (w *archiveWriter) flush() {
	if w.buf != nil {
		if err := w.buf.Flush(); err != nil {
			w.setErr(err)
		}
	}
}

func (w *archiveWriter) closeFile() {
	w.flush()
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
		w.buf = nil
	}
}

// prune 按保留天数与总量上限清理旧归档。
func (w *archiveWriter) prune() {
	entries, err := filepath.Glob(filepath.Join(w.cfg.Dir, ArchiveFileGlob))
	if err != nil || len(entries) == 0 {
		return
	}
	type fi struct {
		path string
		mod  time.Time
		size int64
	}
	items := make([]fi, 0, len(entries))
	var total int64
	for _, p := range entries {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		items = append(items, fi{path: p, mod: st.ModTime(), size: st.Size()})
		total += st.Size()
	}
	// 老的先删
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })

	cutoff := time.Now().AddDate(0, 0, -w.cfg.RetentionDays)
	for _, it := range items {
		if it.path == w.path {
			continue // 正在写的文件不删
		}
		if it.mod.Before(cutoff) {
			if os.Remove(it.path) == nil {
				total -= it.size
			}
		}
	}
	// 仍超总量上限就继续从最老的删
	if total <= w.cfg.MaxBytes {
		return
	}
	for _, it := range items {
		if total <= w.cfg.MaxBytes {
			break
		}
		if it.path == w.path {
			continue
		}
		if os.Remove(it.path) == nil {
			total -= it.size
		}
	}
}

// read 从归档里读最近 limit 条（时间倒序）。
func (w *archiveWriter) read(limit int, filter Filter) ([]Event, error) {
	if !w.enabled() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > defaultReadMax {
		limit = defaultReadMax
	}
	w.flush() // 让刚写入的记录可见

	entries, err := filepath.Glob(filepath.Join(w.cfg.Dir, ArchiveFileGlob))
	if err != nil {
		return nil, err
	}
	// 新的文件优先（文件名含日期，字典序即时间序）
	sort.Sort(sort.Reverse(sort.StringSlice(entries)))

	out := make([]Event, 0, limit)
	for _, p := range entries {
		if len(out) >= limit {
			break
		}
		rows, err := readFile(p, filter)
		if err != nil {
			continue
		}
		// 文件内也是倒序读取，保持「新的在前」
		for i := len(rows) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, rows[i])
		}
	}
	return out, nil
}

func readFile(path string, filter Filter) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var rows []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // 半截行（进程被 kill）直接跳过，不让整次读取失败
		}
		if filter.match(e) {
			rows = append(rows, e)
		}
	}
	return rows, sc.Err()
}

func (f Filter) match(e Event) bool {
	if f.Outcome != "" && !strings.EqualFold(e.Outcome, f.Outcome) {
		return false
	}
	if f.Account != "" && !strings.Contains(strings.ToLower(e.Account), strings.ToLower(f.Account)) {
		return false
	}
	if f.Model != "" && !strings.Contains(strings.ToLower(e.Model), strings.ToLower(f.Model)) {
		return false
	}
	return true
}

func (w *archiveWriter) stats() ArchiveStats {
	if !w.enabled() {
		return ArchiveStats{Enabled: false, DroppedWrites: w.dropped.Load()}
	}
	s := ArchiveStats{Enabled: true, Dir: w.cfg.Dir, DroppedWrites: w.dropped.Load()}
	entries, err := filepath.Glob(filepath.Join(w.cfg.Dir, ArchiveFileGlob))
	if err == nil {
		s.Files = len(entries)
		for _, p := range entries {
			if st, err := os.Stat(p); err == nil {
				s.Bytes += st.Size()
			}
		}
	}
	s.LastError = w.errString()
	return s
}

func (w *archiveWriter) close() {
	if w == nil || w.done == nil {
		return
	}
	w.closeOnce.Do(func() {
		if w.enabled() {
			close(w.stop)
			<-w.done
			return
		}
		<-w.done
	})
}

func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}
