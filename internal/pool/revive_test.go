package pool

import (
	"testing"
	"time"

	"workbuddy-gateway/internal/auth"
)

// Revive 是运维口径的**无条件恢复**，必须把治理层的四种阻挡一次清干净：
// 禁用、冷却（软）、熔断、连败降权。
//
// 少清任何一项的表现都是「点了复活，账号还是不出现在候选里」——
// 而按钮已经变绿了，用户只会以为功能坏了。
func TestReviveClearsAllGovernanceState(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	acc := p.accounts[0]

	// 造出四种阻挡同时存在的局面。
	if err := p.SetDisabled(acc, true, "手工禁用"); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	acc.CooldownSoft(time.Minute, time.Time{}, "限流")
	acc.breakerUntil = time.Now().Add(time.Hour)
	acc.degradeUntil = time.Now().Add(time.Hour)
	acc.degradeFails = 5

	if err := p.Revive(acc); err != nil {
		t.Fatalf("复活失败: %v", err)
	}

	if acc.IsDisabled() {
		t.Fatal("禁用未清除")
	}
	if acc.disabledReason != "" {
		t.Fatalf("禁用原因未清除: %q", acc.disabledReason)
	}
	if !acc.breakerUntil.IsZero() {
		t.Fatalf("熔断未清除: %v", acc.breakerUntil)
	}
	if !acc.degradeUntil.IsZero() || acc.degradeFails != 0 {
		t.Fatalf("连败降权未清除: until=%v fails=%d", acc.degradeUntil, acc.degradeFails)
	}
	if !acc.cooldownUntil.IsZero() || acc.coolKind != CoolNone {
		t.Fatalf("软冷却未清除: until=%v kind=%q", acc.cooldownUntil, acc.coolKind)
	}

	// 复活后必须真的能入选（清状态但选号仍排除它，等于没复活）。
	got, err := p.PickAccount(PickOptions{})
	if err != nil {
		t.Fatalf("复活后应能被选到: %v", err)
	}
	got.ReleaseInFlight()
}

// **余额类硬冷却不能清**：它的依据是「余额为 0」这个客观事实，
// 点一下按钮并不会让余额回来。清了只会让它立刻又被冷却一次，
// 而用户会以为复活没用。
func TestReviveKeepsHardCooldown(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	acc := p.accounts[0]

	acc.CooldownHard("余额耗尽")
	before := acc.cooldownUntil
	if before.IsZero() {
		t.Fatal("前置条件错误：硬冷却应已设置")
	}

	if err := p.Revive(acc); err != nil {
		t.Fatal(err)
	}

	if acc.coolKind != CoolHard {
		t.Fatalf("硬冷却类别被改掉了: %q", acc.coolKind)
	}
	if acc.cooldownUntil.IsZero() {
		t.Fatal("余额类硬冷却不该被复活清掉（依据是余额为 0，不是失败状态）")
	}
}

// Revive 必须落盘「解除禁用」标记：否则重启后账号又变回禁用，
// 表现是「复活了，一重启又没了」。
func TestReviveWritesEnableMarker(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	acc := p.accounts[0]

	if err := p.SetDisabled(acc, true, "手工禁用"); err != nil {
		t.Fatal(err)
	}
	if err := p.Revive(acc); err != nil {
		t.Fatal(err)
	}

	// 用一份新的池重新扫描同一批凭据，禁用状态应已消失。
	p2 := newTestPool(t, auth.SiteCN)
	if p2.accounts[0].IsDisabled() {
		t.Fatal("标记未落盘 → 重启后会回到禁用态")
	}
}

// nil 账号不该 panic（面板的路径参数可能指向一个已被移除的账号）。
func TestReviveNilAccount(t *testing.T) {
	p := newTestPool(t, auth.SiteCN)
	if err := p.Revive(nil); err == nil {
		t.Fatal("nil 账号应返回错误而不是静默成功")
	}
}
