package pool

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPausedExcludesFromPickButKeepsMaintenance 守住「暂停选号」的语义边界。
//
// 它是与「禁用」并列的**中间态**（对齐上游 panel@1b90f7f / issue #125）：
//
//	暂停 = 不参与选号（不出对话流量），但维护任务照常
//	禁用 = 完全不参与，连维护也停（除非开 schedule.include_disabled_in_tasks）
//
// 两者混成一个状态，用户就没法表达「这个号先别发请求、但签到保活别停」——
// 而那恰恰是让位防风控时最需要的中间态。
func TestPausedExcludesFromPickButKeepsMaintenance(t *testing.T) {
	p := newTestPool(t, "cn")
	acc := p.accounts[0]
	// newTestPool 给的 Path 是相对文件名，落盘标记会写进当前工作目录（仓库里）。
	// 换成临时目录下的绝对路径，测试不留痕。
	acc.Cred.Path = filepath.Join(t.TempDir(), "workbuddy-test.json")

	if acc.IsPaused() {
		t.Fatal("新账号不该是暂停态")
	}
	if err := p.SetPaused(acc, true); err != nil {
		t.Fatalf("暂停失败: %v", err)
	}
	if !acc.IsPaused() {
		t.Fatal("暂停后 IsPaused 应为真")
	}
	if _, err := os.Stat(acc.Cred.Path + pausedMarkerSuffix); err != nil {
		t.Errorf("应写入 .paused 标记文件：%v", err)
	}

	// 选号必须跳过它 —— 这是 paused 的全部意义。
	if got, _ := p.PickAccount(PickOptions{}); got != nil {
		t.Errorf("暂停号不该被选中，实际选中 %s", got.Cred.AccountID())
	}

	// 统计要单独一列：混进「可用」会让界面看不出它是暂停的。
	if s := p.Summary(); s.Paused != 1 || s.Active != 0 || s.Disabled != 0 {
		t.Errorf("统计应为 paused=1 active=0 disabled=0，实际 paused=%d active=%d disabled=%d",
			s.Paused, s.Active, s.Disabled)
	}

	// 恢复后一切照旧。
	if err := p.SetPaused(acc, false); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if _, err := os.Stat(acc.Cred.Path + pausedMarkerSuffix); !os.IsNotExist(err) {
		t.Error("恢复后应删除 .paused 标记文件")
	}
	if got, _ := p.PickAccount(PickOptions{}); got == nil {
		t.Error("恢复后应能被选中")
	}
	if s := p.Summary(); s.Active != 1 || s.Paused != 0 {
		t.Errorf("恢复后统计应为 active=1 paused=0，实际 active=%d paused=%d", s.Active, s.Paused)
	}
}

// TestDisabledWinsOverPaused 守住四类状态的互斥优先级：禁用 > 暂停 > 冷却 > 可用。
//
// 同时按下禁用与暂停时必须只算一类，否则 Summary 的「和恒等于 Total」这条契约就破了，
// 面板上的数字会互相矛盾。
func TestDisabledWinsOverPaused(t *testing.T) {
	p := newTestPool(t, "cn")
	acc := p.accounts[0]
	acc.Cred.Path = filepath.Join(t.TempDir(), "workbuddy-test.json")

	if err := p.SetPaused(acc, true); err != nil {
		t.Fatal(err)
	}
	if err := p.SetDisabled(acc, true, "测试用"); err != nil {
		t.Fatal(err)
	}

	s := p.Summary()
	if s.Disabled != 1 || s.Paused != 0 || s.Active != 0 {
		t.Errorf("禁用应优先于暂停：disabled=%d paused=%d active=%d", s.Disabled, s.Paused, s.Active)
	}
	if sum := s.Active + s.Disabled + s.Paused + s.Cooldown; sum != s.Total {
		t.Errorf("四类必须互斥且和等于 Total：和=%d total=%d", sum, s.Total)
	}
}

// TestPausedMarkerIsSeparateFromDisabled 守住两个标记文件互不干扰。
//
// 它们后缀不同（.paused / .disabled），这是"两种状态"的落盘表达 ——
// 共用一个标记就没法区分"别派发"与"别用了"。
func TestPausedMarkerIsSeparateFromDisabled(t *testing.T) {
	if markerSuffix == pausedMarkerSuffix {
		t.Fatalf("两个标记后缀不能相同：%q", markerSuffix)
	}
	p := newTestPool(t, "cn")
	acc := p.accounts[0]
	acc.Cred.Path = filepath.Join(t.TempDir(), "workbuddy-test.json")

	if err := p.SetPaused(acc, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(acc.Cred.Path + markerSuffix); !os.IsNotExist(err) {
		t.Error("只暂停时不该产生 .disabled 标记")
	}
	if err := p.SetDisabled(acc, true, "测试用"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(acc.Cred.Path + pausedMarkerSuffix); err != nil {
		t.Error("禁用不该清掉已有的 .paused 标记（两者独立）")
	}
}
