package pool

import (
	"testing"
	"time"

	"workbuddy-gateway/internal/auth"
)

// 余额跌破 0 再恢复时，企业不限量账号也要走同一条解冻路径 ——
// 它的 Remaining 恒为 0，只按数值判断会让它永远解不了冻。
func TestUnlimitedAccountReviveNotGatedOnRemaining(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	acc := p.accounts[0]
	acc.mu.Lock()
	acc.quotaKnown = true
	acc.quotaUnlimited = true
	acc.mu.Unlock()

	if !acc.CreditsUnlimited() {
		t.Fatal("CreditsUnlimited 应为 true")
	}
	if acc.Credits() != 0 {
		t.Fatalf("不限量账号剩余按约定应为 0，实际 %v", acc.Credits())
	}

	// 真正要覆盖的路径：处在硬冷却时，即使剩余为 0 也必须能解冻。
	acc.mu.Lock()
	acc.coolKind = CoolHard
	acc.cooldownUntil = time.Now().Add(4 * time.Hour)
	acc.mu.Unlock()

	if !acc.ReviveIfCreditsRecovered(acc.Credits()) {
		t.Fatal("不限量账号应能靠「额度恢复」路径解冻（剩余恒为 0 不该挡住它）")
	}
	acc.mu.Lock()
	still := time.Now().Before(acc.cooldownUntil)
	acc.mu.Unlock()
	if still {
		t.Fatal("解冻后不应仍处于冷却")
	}
}
