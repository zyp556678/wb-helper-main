package tokenstats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeJSONL 在 root/<dir>/<name>.jsonl 写一份会话日志，返回根目录。
func writeJSONL(t *testing.T, root, dir, name string, lines []map[string]any) {
	t.Helper()
	full := filepath.Join(root, dir)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(full, name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, line := range lines {
		if err := enc.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
}

// scanOne 对单个根跑一次扫描，返回 workbuddy 这个来源。
func scanOne(t *testing.T, root string, days int) SourceJSON {
	t.Helper()
	stats := collectFrom([]rootSpec{{source: "workbuddy", label: "WorkBuddy", path: root}}, days)
	for _, s := range stats.Sources {
		if s.Source == "workbuddy" {
			return s
		}
	}
	t.Fatal("没有 workbuddy 来源")
	return SourceJSON{}
}

// nowMS 当前毫秒时间戳。
func nowMS() int64 { return time.Now().UnixMilli() }

// usageLine 造一条带 usage 的记录（结构与真实日志一致：message.usage + providerData）。
func usageLine(ts int64, model string, usage map[string]any, raw map[string]any) map[string]any {
	pd := map[string]any{"model": model}
	if raw != nil {
		pd["rawUsage"] = raw
	}
	return map[string]any{
		"type":         "message",
		"timestamp":    ts,
		"cwd":          `C:\work\workbuddy-gateway`,
		"message":      map[string]any{"usage": usage},
		"providerData": pd,
	}
}

// ── 字段别名 ────────────────────────────────────────────────────────────────

func TestUsageFieldAliases(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]any
		want  Usage
	}{
		{"下划线拼写", map[string]any{"input_tokens": 10, "output_tokens": 3, "cache_read_input_tokens": 4}, Usage{Input: 10, Output: 3, Read: 4}},
		{"驼峰拼写", map[string]any{"inputTokens": 11, "outputTokens": 5, "cacheReadInputTokens": 6}, Usage{Input: 11, Output: 5, Read: 6}},
		{"OpenAI 拼写", map[string]any{"prompt_tokens": 12, "completion_tokens": 7, "cached_tokens": 8}, Usage{Input: 12, Output: 7, Read: 8}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parseUsage(tc.usage, nil)
			if got != tc.want {
				t.Fatalf("期望 %+v，实际 %+v", tc.want, got)
			}
		})
	}
}

// 真实日志里 `cache_read_input_tokens` 恒为 0，真值只在 `prompt_cache_hit_tokens`。
// 若按"取第一个存在的键"实现，命中率会被算成 0%。
func TestCacheReadZeroAliasDoesNotMaskHitTokens(t *testing.T) {
	u := map[string]any{
		"input_tokens":            100,
		"output_tokens":           5,
		"cache_read_input_tokens": 0,  // 陈旧/未填充的别名
		"prompt_cache_hit_tokens": 80, // 真值
	}
	got, _ := parseUsage(u, nil)
	if got.Read != 80 {
		t.Fatalf("应当取到 prompt_cache_hit_tokens=80，实际 %d（0 值别名把它盖住了）", got.Read)
	}
}

// 缓存写入只存在于 providerData.rawUsage，usage 对象里没有。
func TestCacheWriteComesFromRawUsage(t *testing.T) {
	u := map[string]any{"input_tokens": 50, "output_tokens": 10}
	raw := map[string]any{"cache_creation_input_tokens": 30}
	got, _ := parseUsage(u, raw)
	if got.Write != 30 {
		t.Fatalf("期望从 rawUsage 取到写入 30，实际 %d", got.Write)
	}
}

// `prompt_cache_miss_tokens` 表示"新算的未缓存输入"，不是缓存写入。
// 当成写入会让写入量虚高 —— 这是上游字段语义的坑，用测试钉住。
func TestPromptCacheMissIsNotWrite(t *testing.T) {
	u := map[string]any{"input_tokens": 50, "prompt_cache_miss_tokens": 40}
	got, _ := parseUsage(u, nil)
	if got.Write != 0 {
		t.Fatalf("prompt_cache_miss_tokens 不该被当成缓存写入，实际 %d", got.Write)
	}
}

// ── 汇总口径 ────────────────────────────────────────────────────────────────

// total 不能再加一次 cacheRead（input 已包含它），否则总量虚高。
func TestTotalsDoNotDoubleCountCacheRead(t *testing.T) {
	got := totalsJSON(totals{usage: Usage{Input: 100, Output: 20, Read: 80, Write: 5}, records: 1})
	if got.Total != 125 {
		t.Fatalf("总量应当是 input+output+write=125，实际 %d", got.Total)
	}
	if got.UncachedInput != 20 {
		t.Fatalf("非缓存输入应当是 100-80=20，实际 %d", got.UncachedInput)
	}
	if got.CacheHitRate == nil || *got.CacheHitRate < 0.79 || *got.CacheHitRate > 0.81 {
		t.Fatalf("命中率应当是 0.8，实际 %v", got.CacheHitRate)
	}
}

// input 为 0 时命中率必须是 null 而不是 0 —— 前者表示"无意义"，后者表示"命中率真的是 0"。
func TestCacheHitRateNullWhenNoInput(t *testing.T) {
	got := totalsJSON(totals{usage: Usage{Output: 5}})
	if got.CacheHitRate != nil {
		t.Fatalf("没有输入时命中率应当是 null，实际 %v", *got.CacheHitRate)
	}
}

// ── 去重 ────────────────────────────────────────────────────────────────────

// 复制/分叉的会话会把父会话历史重放一遍。相同指纹在多个文件里只应计一次，
// 且归属 mtime 更早的那个文件。
func TestDedupAcrossFilesPrefersOldest(t *testing.T) {
	root := t.TempDir()
	line := usageLine(nowMS(), "m1", map[string]any{"input_tokens": 100, "output_tokens": 1}, nil)

	writeJSONL(t, root, "proj", "original", []map[string]any{line})
	writeJSONL(t, root, "proj", "fork", []map[string]any{line})

	// 让 original 的 mtime 更早，去重应当保留它。
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(filepath.Join(root, "proj", "original.jsonl"), old, old)

	src := scanOne(t, root, 30)
	if src.Summary.Records != 1 {
		t.Fatalf("重放记录应当只计一次，实际 %d 条", src.Summary.Records)
	}
	if src.Summary.Input != 100 {
		t.Fatalf("输入应当只累加一次=100，实际 %d", src.Summary.Input)
	}
}

// subagents 是子代理日志，会把父会话上下文重写一遍，必须跳过。
func TestSubagentsSkipped(t *testing.T) {
	root := t.TempDir()
	line := usageLine(nowMS(), "m1", map[string]any{"input_tokens": 500}, nil)
	writeJSONL(t, root, "proj", "main", []map[string]any{line})
	writeJSONL(t, root, filepath.Join("proj", "subagents"), "child", []map[string]any{line})

	src := scanOne(t, root, 30)
	if src.FilesScanned != 1 {
		t.Fatalf("subagents 目录应当被跳过，实际扫了 %d 个文件", src.FilesScanned)
	}
	if src.Summary.Input != 500 {
		t.Fatalf("输入应当只有主会话的 500，实际 %d", src.Summary.Input)
	}
}

// ── 会话标题与思考 token ────────────────────────────────────────────────────

// aiTitle 优先于 summary，且标题可能出现在用量记录**之后**，必须回填。
func TestTitleFromAiTitleBackfilled(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, root, "proj", "sess", []map[string]any{
		usageLine(nowMS(), "m1", map[string]any{"input_tokens": 10}, nil),
		{"type": "message", "timestamp": nowMS(), "summary": "摘要标题"},
		{"type": "ai-title", "timestamp": nowMS(), "aiTitle": "AI 生成的标题"},
	})

	src := scanOne(t, root, 30)
	if len(src.Sessions) != 1 {
		t.Fatalf("应当有 1 个会话，实际 %d", len(src.Sessions))
	}
	title := src.Sessions[0].Title
	if title == nil || *title != "AI 生成的标题" {
		t.Fatalf("标题应当是 aiTitle（且回填到会话上），实际 %v", title)
	}
	for _, r := range src.Requests {
		if r.Title == nil || *r.Title != "AI 生成的标题" {
			t.Fatalf("明细行也应当回填标题，实际 %v", r.Title)
		}
	}
}

func TestThinkingTokensFromRawUsage(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, root, "proj", "sess", []map[string]any{
		usageLine(nowMS(), "m1", map[string]any{"input_tokens": 10, "output_tokens": 100},
			map[string]any{"completion_thinking_tokens": 60}),
	})
	src := scanOne(t, root, 30)
	if len(src.Requests) != 1 || src.Requests[0].Thinking != 60 {
		t.Fatalf("思考 token 应当是 60，实际 %+v", src.Requests)
	}
}

// ── 大行（缓冲区）────────────────────────────────────────────────────────────

// 工具结果会写进日志，实测单行可达数 MB；bufio.Scanner 默认上限 64KB，
// 超了会静默停止扫描。这条测试构造一个 200KB 的行，后面再跟一条 usage，
// 若缓冲区没抬高，后面的用例会整条丢失。
func TestHugeLineDoesNotAbortScan(t *testing.T) {
	root := t.TempDir()
	huge := map[string]any{
		"type":      "function_call_result",
		"timestamp": nowMS(),
		"output":    strings.Repeat("x", 200*1024),
	}
	usage := usageLine(nowMS(), "m1", map[string]any{"input_tokens": 42, "output_tokens": 1}, nil)
	writeJSONL(t, root, "proj", "sess", []map[string]any{huge, usage})

	src := scanOne(t, root, 30)
	if src.Summary.Records != 1 || src.Summary.Input != 42 {
		t.Fatalf("大行之后的那条 usage 应当仍被统计，实际 records=%d input=%d", src.Summary.Records, src.Summary.Input)
	}
}

// ── 时间归属 ────────────────────────────────────────────────────────────────

// 按天聚合必须用**本地日**：官方账本按本地日切，网关天桶也已改成本地日，
// 这里若用 UTC，同一天的用量会被劈到两天上。
func TestDailyUsesLocalDate(t *testing.T) {
	root := t.TempDir()
	// 本地 00:30 —— UTC 下属于前一天（GMT+8 时 UTC 是前一天 16:30）。
	now := time.Now()
	local := time.Date(now.Year(), now.Month(), now.Day(), 0, 30, 0, 0, time.Local)
	if local.After(now) {
		local = local.AddDate(0, 0, -1)
	}
	writeJSONL(t, root, "proj", "sess", []map[string]any{
		usageLine(local.UnixMilli(), "m1", map[string]any{"input_tokens": 7}, nil),
	})

	src := scanOne(t, root, 30)
	want := local.Format("2006-01-02")
	found := false
	for _, d := range src.Daily {
		if d.Key == want && d.Total > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("应当落在本地日 %s，实际按天为 %+v", want, src.Daily)
	}
}

// ── 解析失败 ────────────────────────────────────────────────────────────────

func TestParseErrorsCountedNotFatal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "{\"type\":\"message\"}\n这不是 JSON\n" +
		`{"timestamp":` + itoa64(nowMS()) + `,"cwd":"D:\\x","message":{"usage":{"input_tokens":5}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "sess.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	src := scanOne(t, root, 30)
	if src.ParseErrors != 1 {
		t.Fatalf("应当记 1 条解析失败，实际 %d", src.ParseErrors)
	}
	if src.Summary.Input != 5 {
		t.Fatalf("坏行不该影响后续行，实际 input=%d", src.Summary.Input)
	}
}

func itoa64(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// 按天序列必须是**时间序**，不能按量倒序。
//
// 真实踩过：daily 复用了排行榜的「按总量倒序」，于是折线图的 X 轴变成
// 「09/24 → 09/25 → 09/23」；量级小的那天被排到末尾，光看形状还以为是缺数据。
// 这条测试刻意让**较早的一天量更大**，两种排法会给出不同结果。
func TestDailyIsChronologicalNotByVolume(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	d0 := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local).AddDate(0, 0, -2)
	d1 := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local).AddDate(0, 0, -1)

	writeJSONL(t, root, "proj", "sess", []map[string]any{
		// 较早的一天：量更大（按量倒序时会排到最前）
		usageLine(d0.UnixMilli(), "m1", map[string]any{"input_tokens": 9000}, nil),
		// 较晚的一天：量很小
		usageLine(d1.UnixMilli(), "m1", map[string]any{"input_tokens": 10}, nil),
	})

	src := scanOne(t, root, 30)
	if len(src.Daily) != 2 {
		t.Fatalf("应当有 2 个天桶，实际 %d", len(src.Daily))
	}
	if src.Daily[0].Key >= src.Daily[1].Key {
		t.Fatalf("按天序列必须是时间升序：实际 %s → %s", src.Daily[0].Key, src.Daily[1].Key)
	}
	if src.Daily[0].Key != d0.Format("2006-01-02") {
		t.Fatalf("首项应当是最早的那天 %s，实际 %s", d0.Format("2006-01-02"), src.Daily[0].Key)
	}
	// 交叉维度（模型 × 天）同样要按日期排。
	for model, series := range src.DailyByModel {
		for i := 1; i < len(series); i++ {
			if series[i-1].Key >= series[i].Key {
				t.Fatalf("dailyByModel[%s] 也必须时间升序：%s → %s", model, series[i-1].Key, series[i].Key)
			}
		}
	}
}

// ── 项目归属 ────────────────────────────────────────────────────────────────

// 目录名常编码了完整绝对路径（c-Users-foo-…），直接展示会泄漏用户名。
func TestEncodedPathNotShownAsProject(t *testing.T) {
	for _, in := range []string{
		"c-Users-foo-WorkBuddy-2026-09-23-19-00-55",
		"d-software-workbuddy-gateway",
		"Users-foo-bar",
		"home-foo-bar",
		"sess.jsonl",
	} {
		if got := sanitizeFallback(in); got != unknownProject {
			t.Fatalf("%q 不该作为项目名展示，实际 %q", in, got)
		}
	}
	if got := sanitizeFallback("workbuddy-gateway"); got != "workbuddy-gateway" {
		t.Fatalf("普通目录名应当保留，实际 %q", got)
	}
}

// cwd 的 basename 是首选的项目来源。
func TestProjectFromCwdBasename(t *testing.T) {
	rec := map[string]any{"cwd": `C:\work\workbuddy-gateway`}
	if got := projectOf(rec, "fallback"); got != "workbuddy-gateway" {
		t.Fatalf("应当取 cwd 的 basename，实际 %q", got)
	}
}
