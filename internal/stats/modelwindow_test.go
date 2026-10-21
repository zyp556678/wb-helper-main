package stats

import (
	"testing"
	"time"
)

// ModelWindow 是模型页 range=5h 的数据源。它存在的理由只有一个：
// 进程内指标重启即清零，而「近 5 小时这个模型跑了多少」在重启后仍然要能显示。
// 这组测试钉住三件事：窗口外的桶不能算进来、成功数由请求减失败推出、均值按样本数平均。
func TestModelWindowAggregatesOnlyWindow(t *testing.T) {
	s := New("")
	nowHour := time.Now().Unix() / 3600

	// 当前小时：m1 两次请求（一成功一失败），带首字与总耗时样本。
	s.Record(RecordInput{Model: "m1", OK: true, TotalTokens: 10, TTFTMs: 100, LatencyMs: 1000})
	s.Record(RecordInput{Model: "m1", OK: false, TotalTokens: 5, TTFTMs: 300, LatencyMs: 3000})
	// 当前小时：m2 一次成功。
	s.Record(RecordInput{Model: "m2", OK: true, TotalTokens: 7})

	// 构造一个**窗口外**的桶：5 小时窗口是 [nowHour-4, nowHour]，
	// 放进 nowHour-6 应该被排除。
	s.mu.Lock()
	s.doc.Hours[key(nowHour-6)] = &HourBucket{
		Hour: nowHour - 6,
		Models: map[string]*Counter{
			"m1": {Requests: 100, Failures: 0, Tokens: 9999},
		},
	}
	s.mu.Unlock()

	got := s.ModelWindow(5)
	m1, ok := got["m1"]
	if !ok {
		t.Fatal("窗口内应包含 m1")
	}
	if m1.Requests != 2 {
		t.Errorf("m1 请求数应为 2（窗口外的 100 次不能算进来），实际 %d", m1.Requests)
	}
	if m1.Success != 1 || m1.Failures != 1 {
		t.Errorf("m1 成功/失败应为 1/1，实际 %d/%d", m1.Success, m1.Failures)
	}
	if m1.Tokens != 15 {
		t.Errorf("m1 token 应为 15，实际 %d", m1.Tokens)
	}
	if m1.AvgTTFTMs == nil || *m1.AvgTTFTMs != 200 {
		t.Errorf("m1 平均首字应为 (100+300)/2=200，实际 %v", m1.AvgTTFTMs)
	}
	if m1.AvgLatencyMs == nil || *m1.AvgLatencyMs != 2000 {
		t.Errorf("m1 平均总耗时应为 (1000+3000)/2=2000，实际 %v", m1.AvgLatencyMs)
	}
	if m2, ok := got["m2"]; !ok || m2.Requests != 1 || m2.AvgLatencyMs != nil {
		t.Errorf("m2 应只有 1 次请求且无耗时样本，实际 %+v（存在=%v）", m2, ok)
	}
}

// 无样本的模型不会出现在结果里：模型页据此显示「-」，而不是 0。
func TestModelWindowEmpty(t *testing.T) {
	s := New("")
	if got := s.ModelWindow(5); len(got) != 0 {
		t.Fatalf("没有任何记录时结果应为空，实际 %d 个模型", len(got))
	}
}
