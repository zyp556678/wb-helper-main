package stats

import "testing"

func TestQueryDailyBySiteFiltersAccounts(t *testing.T) {
	s := New("")
	s.Record(RecordInput{Model: "m1", Account: "cn-1", Site: "cn", OK: true, TotalTokens: 10})
	s.Record(RecordInput{Model: "m1", Account: "intl-1", Site: "intl", OK: true, TotalTokens: 20})
	s.Record(RecordInput{Model: "m2", Account: "cn-1", Site: "cn", OK: true, TotalTokens: 5})
	s.SetAccountSites(map[string]string{"cn-1": "cn", "intl-1": "intl"})

	all := s.QueryDaily(7)
	if all.Totals.Requests != 3 || all.Totals.Tokens != 35 {
		t.Fatalf("全部站点合计错误：%+v", all.Totals)
	}
	cn := s.QueryDailyBySite(7, "cn")
	if cn.Totals.Requests != 2 || cn.Totals.Tokens != 15 {
		t.Fatalf("国内站合计错误：%+v", cn.Totals)
	}
	if len(cn.Accounts) != 1 || cn.Accounts[0].Account != "cn-1" || cn.Accounts[0].Site != "cn" {
		t.Fatalf("国内站账号维度错误：%+v", cn.Accounts)
	}
	// 站点过滤下模型维度来自账号桶，必须仍然是两个（m1、m2），且只含国内站的量。
	if len(cn.TopModels) != 2 {
		t.Fatalf("国内站模型维度错误：%+v", cn.TopModels)
	}
	intl := s.QueryDailyBySite(7, "intl")
	if intl.Totals.Tokens != 20 {
		t.Fatalf("国际站合计错误：%+v", intl.Totals)
	}
}

// TestQueryDailyBySiteKeepsAccountRollup 锁住账号维度：站点过滤下账号维度
// 只应出现该站的账号，且跨天累计不应因为某天没有数据而漏掉。
func TestQueryDailyBySiteKeepsAccountRollup(t *testing.T) {
	s := New("")
	s.Record(RecordInput{Model: "m1", Account: "cn-1", Site: "cn", OK: true})
	s.Record(RecordInput{Model: "m1", Account: "intl-1", Site: "intl", OK: true})
	s.SetAccountSites(map[string]string{"cn-1": "cn", "intl-1": "intl"})

	cn := s.QueryDailyBySite(7, "cn")
	if len(cn.Accounts) != 1 || cn.Accounts[0].Account != "cn-1" || cn.Accounts[0].Site != "cn" {
		t.Fatalf("站点过滤下账号维度错误：%+v", cn.Accounts)
	}
	all := s.QueryDailyBySite(7, "")
	for _, a := range all.Accounts {
		if a.Site == "" {
			t.Fatalf("未过滤时账号应带站点：%+v", a)
		}
	}
}

// 本功能上线前写入的天桶没有 Sites 字段；站点过滤必须退回账号维度反推，
// 而不是把真实历史显示成 0。
func TestQueryDailyBySiteFallsBackForLegacyBuckets(t *testing.T) {
	s := New("")
	s.Record(RecordInput{Model: "m1", Account: "cn-1", Site: "cn", OK: true, TotalTokens: 10})
	s.SetAccountSites(map[string]string{"cn-1": "cn"})

	// 模拟旧数据：把 Sites 抹掉（等价于升级前落盘的桶）。
	s.mu.Lock()
	for _, b := range s.doc.Days {
		b.Sites = nil
	}
	s.mu.Unlock()

	got := s.QueryDailyBySite(7, "cn")
	if got.Totals.Requests != 1 || got.Totals.Tokens != 10 {
		t.Fatalf("旧天桶应能按账号维度反推出站点总量，实际 %+v", got.Totals)
	}
}

// 账号被删除后，它的历史仍应归到正确站点 —— 靠写入时落盘的归属兜底。
func TestQueryDailyBySiteKeepsDeletedAccountHistory(t *testing.T) {
	s := New("")
	s.Record(RecordInput{Model: "m1", Account: "cn-1", Site: "cn", OK: true, TotalTokens: 10})
	s.Record(RecordInput{Model: "m1", Account: "intl-1", Site: "intl", OK: true, TotalTokens: 20})

	// 模拟账号已被删除：活动池里只剩 intl-1。
	s.SetAccountSites(map[string]string{"intl-1": "intl"})

	cn := s.QueryDailyBySite(7, "cn")
	if cn.Totals.Tokens != 10 {
		t.Fatalf("已删除账号的历史仍应归到国内站，实际 tokens=%d", cn.Totals.Tokens)
	}
	all := s.QueryDailyBySite(7, "")
	cnIntl := s.QueryDailyBySite(7, "cn").Totals.Tokens + s.QueryDailyBySite(7, "intl").Totals.Tokens
	if cnIntl != all.Totals.Tokens {
		t.Fatalf("国内站+国际站(%d) 应等于全部(%d)", cnIntl, all.Totals.Tokens)
	}
}
