package reqlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSnapshotCounts 进程级计数与成功率。
func TestSnapshotCounts(t *testing.T) {
	r := New(Config{}) // 不启归档
	r.Begin()
	r.Begin()
	r.Record(Event{Status: 200, OK: true, DurationMs: 100})
	r.Record(Event{Status: 500, OK: false, DurationMs: 300})

	s := r.Snapshot()
	if s.Completed != 2 || s.Succeeded != 1 || s.Failed != 1 {
		t.Fatalf("计数错误: %+v", s)
	}
	if s.InFlight != 0 {
		t.Fatalf("在途应归零，实际 %d", s.InFlight)
	}
	if s.SuccessRate != 50 {
		t.Fatalf("成功率应为 50，实际 %v", s.SuccessRate)
	}
	if s.HTTPSuccessRate != 50 {
		t.Fatalf("HTTP 成功率应为 50，实际 %v", s.HTTPSuccessRate)
	}
	if s.AvgDurationMs != 200 {
		t.Fatalf("平均耗时应为 200，实际 %v", s.AvgDurationMs)
	}
}

// TestRecentIsBounded 内存只保留最近 100 条，且新的在前。
//
// 有界是硬要求：无界增长会让长跑进程的内存随请求数线性上涨。
func TestRecentIsBounded(t *testing.T) {
	r := New(Config{})
	for i := 0; i < recentLimit+50; i++ {
		r.Record(Event{Status: 200, OK: true, RequestID: string(rune('a' + i%26))})
	}
	s := r.Snapshot()
	if len(s.Recent) != recentLimit {
		t.Fatalf("recent 应被截断到 %d 条，实际 %d", recentLimit, len(s.Recent))
	}
}

// TestOutcomeDefaults 未显式给 outcome 时按状态码推断。
func TestOutcomeDefaults(t *testing.T) {
	r := New(Config{})
	r.Record(Event{Status: 200, OK: true})
	r.Record(Event{Status: 502, OK: false})
	s := r.Snapshot()
	if s.Recent[1].Outcome != OutcomeSuccess {
		t.Fatalf("200+OK 应推断为 success，实际 %q", s.Recent[1].Outcome)
	}
	if s.Recent[0].Outcome != OutcomeHTTPError {
		t.Fatalf("502 应推断为 http_error，实际 %q", s.Recent[0].Outcome)
	}
}

// TestArchiveRoundTrip 归档写入后能读回来，且脱敏字段原样保留。
func TestArchiveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true})
	defer r.Close()

	r.Record(Event{
		RequestID: "req-1", Path: "/v1/chat/completions",
		Account: "测试(a1b2c3d4)", Model: "deepseek-v4.1-flash",
		Status: 200, OK: true, DurationMs: 1234, TTFBMs: 200, TotalTokens: 42,
	})
	r.Record(Event{
		RequestID: "req-2", Path: "/v1/chat/completions",
		Account: "测试(a1b2c3d4)", Model: "glm-5.3",
		Status: 502, OK: false, Outcome: OutcomeHTTPError, DurationMs: 50,
	})

	// 归档是异步的；Close 会把队列排空再返回
	r.Close()

	rows, err := r.ReadArchive(10, Filter{})
	if err != nil {
		t.Fatalf("读取归档失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应读到 2 条，实际 %d", len(rows))
	}
	// 倒序：新的在前
	if rows[0].RequestID != "req-2" {
		t.Fatalf("应按时间倒序，首条实际 %q", rows[0].RequestID)
	}

	// 过滤
	only, err := r.ReadArchive(10, Filter{Outcome: OutcomeHTTPError})
	if err != nil {
		t.Fatalf("按 outcome 过滤失败: %v", err)
	}
	if len(only) != 1 || only[0].RequestID != "req-2" {
		t.Fatalf("outcome 过滤结果不对: %+v", only)
	}
	byModel, _ := r.ReadArchive(10, Filter{Model: "glm"})
	if len(byModel) != 1 || byModel[0].RequestID != "req-2" {
		t.Fatalf("model 模糊过滤结果不对: %+v", byModel)
	}
}

// TestArchiveNeverWritesSecrets 归档文件里不得出现提示词、正文或凭据。
//
// 这是本模块最关键的安全约束：归档是落盘的、会被翻看的，
// 一旦把 Authorization 或提示词写进去，等于把凭据和用户内容长期留在磁盘上。
// Event 结构本身没有这些字段，这里用「写进去的整行文本」做一次兜底断言 ——
// 将来若有人往 Event 里加了敏感字段，这条会失败。
func TestArchiveNeverWritesSecrets(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true})
	r.Record(Event{RequestID: "req-1", Path: "/v1/chat/completions", Status: 200, OK: true})
	r.Close()

	entries, err := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("未生成归档文件: %v %v", entries, err)
	}
	raw, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatalf("读取归档失败: %v", err)
	}
	text := strings.ToLower(string(raw))
	for _, banned := range []string{"authorization", "bearer ", "access_token", "refreshtoken", "messages", "prompt\":"} {
		if strings.Contains(text, banned) {
			t.Fatalf("归档里出现了敏感字段 %q —— 归档是落盘的，绝不能写提示词/正文/凭据", banned)
		}
	}
}

// TestDisabledArchiveKeepsMetrics 关闭归档时内存指标仍工作。
func TestDisabledArchiveKeepsMetrics(t *testing.T) {
	r := New(Config{Dir: t.TempDir(), Enabled: false})
	defer r.Close()
	r.Record(Event{Status: 200, OK: true, DurationMs: 10})

	s := r.Snapshot()
	if s.Completed != 1 {
		t.Fatalf("归档关闭时指标仍应计数，实际 %+v", s)
	}
	if s.Archive.Enabled {
		t.Fatal("归档应报告为关闭")
	}
	if rows, _ := r.ReadArchive(10, Filter{}); len(rows) != 0 {
		t.Fatalf("归档关闭时不应读到记录，实际 %d 条", len(rows))
	}
}

// TestEnqueueNeverBlocks 队列满时丢弃而不是阻塞。
//
// 这条是整个模块的底线：日志写盘绝不能拖住模型请求。
// 用一个容量 1 的队列 + 大量写入来逼近：只要 enqueue 不阻塞，测试就不会超时。
func TestEnqueueNeverBlocks(t *testing.T) {
	dir := t.TempDir()
	// 故意用极小的队列，并让写入远多于容量
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 1})
	defer r.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			r.Record(Event{Status: 200, OK: true, DurationMs: 1})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("enqueue 阻塞了 —— 日志写盘绝不能拖住请求")
	}
}

// TestNewRequestIDUnique 请求 ID 不重复且带前缀。
func TestNewRequestIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewRequestID()
		if !strings.HasPrefix(id, "req-") {
			t.Fatalf("请求 ID 前缀不对: %q", id)
		}
		if seen[id] {
			t.Fatalf("请求 ID 重复: %q", id)
		}
		seen[id] = true
	}
}

// -----------------------------------------------------------------------------
// 列表字段**永远不能是 null**（这类 bug 已经复发过两次）
// -----------------------------------------------------------------------------
//
// Go 的 nil slice 序列化成 `null`，前端拿到后 `.length` / `.map` 直接抛异常 →
// **整页白屏**。实测踩过两次：
//
//  1. 本模块：刚启动、还没有任何请求时打开「请求流水」页白屏
//     （`Recent` 是 nil，`append([]Event(nil), 空...)` 返回的仍是 nil）；
//  2. 本机应用接入的 `Blockers`（见 internal/localapps 的同名用例）。
//
// 类型检查与单测都过，只有真跑一遍才暴露 —— 所以这里直接断言 JSON 里
// 不能出现该字段的 `null`。
func TestSnapshotListsAreNeverNull(t *testing.T) {
	// 关键场景：**完全空**的记录器（刚启动、零请求）—— 这正是用户截图里的状态。
	empty := New(Config{})
	raw, err := json.Marshal(empty.Snapshot())
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), `"recent":null`) {
		t.Fatalf("recent 序列化成了 null（前端 .length 会崩）: %s", raw)
	}
	if !strings.Contains(string(raw), `"recent":[]`) {
		t.Fatalf("空记录器的 recent 应为 []，实际: %s", raw)
	}

	// nil 记录器（服务未装配该模块）同样不能产出 null。
	rawNil, err := json.Marshal((*Recorder)(nil).Snapshot())
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(rawNil), `"recent":null`) {
		t.Fatalf("nil 记录器的 recent 也不能是 null: %s", rawNil)
	}

	// 无归档时 ReadArchive 必须返回空切片而不是 nil。
	rows, err := empty.ReadArchive(10, Filter{})
	if err != nil {
		t.Fatalf("ReadArchive 失败: %v", err)
	}
	if rows == nil {
		t.Fatal("无归档时应返回空切片（nil 会序列化成 null 让前端崩）")
	}

	// nil 记录器同理。
	rowsNil, err := (*Recorder)(nil).ReadArchive(10, Filter{})
	if err != nil {
		t.Fatalf("nil 记录器 ReadArchive 失败: %v", err)
	}
	if rowsNil == nil {
		t.Fatal("nil 记录器也应返回空切片")
	}
}
