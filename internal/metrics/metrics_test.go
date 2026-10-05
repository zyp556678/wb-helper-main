package metrics

import (
	"testing"
	"time"
)

// 最近状态必须如实反映**最后一次**结果：先成功后失败要显示失败，
// 先失败后成功要显示成功。它不是「有没有失败过」的累计，而是「现在还行不行」。
func TestLastStatusReflectsLatestResult(t *testing.T) {
	r := New()
	r.RecordRequest("m")
	r.RecordFailure("m", "流中断")
	if got := snapshot(t, r).LastStatus; got != "失败:流中断" {
		t.Fatalf("最近状态应为失败:流中断，实际 %q", got)
	}
	r.RecordRequest("m")
	r.RecordSuccess("m")
	snap := snapshot(t, r)
	if snap.LastStatus != "成功" {
		t.Fatalf("成功后最近状态应回到成功，实际 %q", snap.LastStatus)
	}
	if snap.Requests != 2 || snap.Success != 1 || snap.Failures != 1 {
		t.Fatalf("计数不对：requests=%d success=%d failures=%d",
			snap.Requests, snap.Success, snap.Failures)
	}
}

// 没有原因时只显示「失败」，不能拼出一个带冒号尾巴的怪串。
func TestFailureWithoutReason(t *testing.T) {
	r := New()
	r.RecordRequest("m")
	r.RecordFailure("m", "")
	if got := snapshot(t, r).LastStatus; got != "失败" {
		t.Fatalf("无原因时最近状态应为「失败」，实际 %q", got)
	}
}

// 空模型名不进统计：请求还没解析出模型时不该污染任何一行。
func TestEmptyModelIgnored(t *testing.T) {
	r := New()
	r.RecordRequest("")
	r.RecordSuccess("")
	r.RecordFailure("", "x")
	r.RecordTokens("", 100)
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("空模型名不应产生统计项，实际 %+v", got)
	}
}

// token 与耗时只累加正数：上游没给 usage 时记 0 会把平均耗时/总量算歪。
func TestNonPositiveSamplesIgnored(t *testing.T) {
	r := New()
	r.RecordRequest("m")
	r.RecordTokens("m", 0)
	r.RecordTokens("m", -5)
	r.RecordTTFT("m", 0)
	r.RecordLatency("m", -1)
	snap := snapshot(t, r)
	if snap.TokensTotal != 0 {
		t.Fatalf("非正 token 不应计入，实际 %d", snap.TokensTotal)
	}
	if snap.AvgTTFTMs != nil || snap.AvgTotalMs != nil {
		t.Fatalf("无有效样本时应为 nil（面板显示 '-'），实际 %v / %v", snap.AvgTTFTMs, snap.AvgTotalMs)
	}
}

// 滚动窗口均值确实生效（这里只验「有样本就能算出均值」这条基本性质）。
func TestAveragesFromSamples(t *testing.T) {
	r := New()
	r.RecordTTFT("m", 120*time.Millisecond)
	r.RecordTTFT("m", 80*time.Millisecond)
	r.RecordLatency("m", time.Second)
	snap := snapshot(t, r)
	if snap.AvgTTFTMs == nil || *snap.AvgTTFTMs != 100 {
		t.Fatalf("平均首字应为 100ms，实际 %v", snap.AvgTTFTMs)
	}
	if snap.AvgTotalMs == nil || *snap.AvgTotalMs != 1000 {
		t.Fatalf("平均总耗时应为 1000ms，实际 %v", snap.AvgTotalMs)
	}
}

// Totals 汇总必须与逐模型计数一致。
func TestTotalsAggregate(t *testing.T) {
	r := New()
	r.RecordRequest("a")
	r.RecordSuccess("a")
	r.RecordTokens("a", 10)
	r.RecordRequest("b")
	r.RecordFailure("b", "x")
	r.RecordTokens("b", 5)
	tot := r.Totals()
	if tot.Requests != 2 || tot.Failures != 1 || tot.Tokens != 15 {
		t.Fatalf("汇总不对：%+v", tot)
	}
}

func snapshot(t *testing.T, r *Registry) ModelSnapshot {
	t.Helper()
	all := r.Snapshot()
	if len(all) == 0 {
		t.Fatal("没有任何统计项")
	}
	return all[0]
}
