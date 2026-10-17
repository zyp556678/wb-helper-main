package server

import (
	"testing"
	"time"

	"workbuddy-gateway/internal/pool"
)

func stateFor(id string, mutate func(*pool.AccountState)) pool.AccountState {
	st := pool.AccountState{ID: id, TokenValid: true}
	mutate(&st)
	return st
}

func TestAccountAlertKinds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name string
		st   pool.AccountState
		want []string
	}{
		{"健康账号无告警", stateFor("a", func(*pool.AccountState) {}), nil},
		{"禁用", stateFor("a", func(s *pool.AccountState) { s.Disabled = true }), []string{"disabled"}},
		{"令牌过期", stateFor("a", func(s *pool.AccountState) { s.TokenValid = false }), []string{"expired"}},
		{"额度用尽", stateFor("a", func(s *pool.AccountState) {
			s.Quota = &pool.QuotaView{Exhausted: true}
		}), []string{"exhausted"}},
		{"不限量不算用尽", stateFor("a", func(s *pool.AccountState) {
			s.Quota = &pool.QuotaView{Exhausted: false, Unlimited: true}
		}), nil},
		{"冷却中", stateFor("a", func(s *pool.AccountState) {
			s.CooldownUntil = now.Unix() + 60
		}), []string{"cooldown"}},
		{"冷却已过期不算", stateFor("a", func(s *pool.AccountState) {
			s.CooldownUntil = now.Unix() - 60
		}), nil},
		{"暂停", stateFor("a", func(s *pool.AccountState) { s.Paused = true }), []string{"paused"}},
		{"禁用账号不重复报暂停", stateFor("a", func(s *pool.AccountState) {
			s.Disabled = true
			s.Paused = true
		}), []string{"disabled"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := accountAlertKinds(tc.st, now)
			if len(got) != len(tc.want) {
				t.Fatalf("得到 %v，期望 %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("得到 %v，期望 %v", got, tc.want)
				}
			}
		})
	}
}

func TestAlertTrackerKeepsFirstSeenAndClearsRecovered(t *testing.T) {
	tr := newAlertTracker()
	start := time.Unix(1_700_000_000, 0)
	states := []pool.AccountState{stateFor("a", func(s *pool.AccountState) { s.Disabled = true })}

	first := tr.observe(states, start)
	if len(first) != 1 || first[0].Since != start.Unix() {
		t.Fatalf("首次观察结果异常：%+v", first)
	}
	// 时间推进后再观察，Since 必须保持首次出现时刻（不是「最近一次」）。
	later := tr.observe(states, start.Add(10*time.Minute))
	if later[0].Since != start.Unix() {
		t.Fatalf("首次出现时刻被刷新：%d != %d", later[0].Since, start.Unix())
	}
	// 恢复后立刻消失。
	if got := tr.observe([]pool.AccountState{stateFor("a", func(*pool.AccountState) {})}, start.Add(11*time.Minute)); len(got) != 0 {
		t.Fatalf("恢复后仍有告警：%+v", got)
	}
	// 再次出现时重新计时 —— 否则界面会显示「坏了很久了」，而其实刚刚才坏。
	again := tr.observe(states, start.Add(20*time.Minute))
	if again[0].Since != start.Add(20*time.Minute).Unix() {
		t.Fatalf("恢复后再出现应从当前时刻重新计时，实际 %d", again[0].Since)
	}
}

func TestSortAlertsSeverityFirst(t *testing.T) {
	list := []AccountAlert{
		{Account: "b", State: "paused", Severity: "notice", Since: 100},
		{Account: "a", State: "disabled", Severity: "urgent", Since: 10},
		{Account: "c", State: "cooldown", Severity: "warning", Since: 200},
	}
	sortAlerts(list)
	if list[0].State != "disabled" || list[1].State != "cooldown" || list[2].State != "paused" {
		t.Fatalf("排序不符合「严重度优先」：%+v", list)
	}
}

func TestSummarizeAlertsCountsAccountsOnce(t *testing.T) {
	list := []AccountAlert{
		{Account: "a", Severity: "urgent"},
		{Account: "a", Severity: "warning"},
		{Account: "b", Severity: "warning"},
	}
	sum := summarizeAlerts(list)
	if sum.Total != 3 || sum.Urgent != 1 || sum.AffectedAccounts != 2 {
		t.Fatalf("汇总异常：%+v", sum)
	}
}
