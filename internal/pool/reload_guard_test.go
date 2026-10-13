package pool

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy-gateway/internal/config"
)

// 本文件的用例都锚在同一个事故形态上：**凭据目录暂时读不到**（被删、改名、挂载丢失）
// 被当成「目录是空的」，于是周期热加载把账号池清空，面板显示 0 个账号、请求全部 503。
// 用户的凭据文件其实一直躺在原处，只是目录暂时不可达 —— 一次瞬时故障不该变成
// 「账号全没了」。

// writeCred 在 dir 下写一个合法凭据文件。
func writeCred(t *testing.T, dir, name, uid string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	path := filepath.Join(dir, name)
	body := `{"auth":{"accessToken":"TOKEN_` + uid + `","refreshToken":"R","expiresAt":4102444800},` +
		`"account":{"uid":"` + uid + `","nickname":"` + uid + `"},"edition":"cn"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写凭据失败: %v", err)
	}
	return path
}

// newAuthDirPool 构造一个走「凭据目录」来源的池，并完成首次加载。
func newAuthDirPool(t *testing.T) (*Pool, string) {
	t.Helper()
	work := t.TempDir()
	authDir := filepath.Join(work, "auths")
	writeCred(t, authDir, "workbuddy-a.json", "uid-a")
	writeCred(t, authDir, "workbuddy-b.json", "uid-b")

	cfg := &config.Config{WorkDir: work, AuthDir: authDir}
	cfg.ApplyDefaults()
	p := New(cfg, nil)
	if n, errs := p.Load(); n != 2 {
		t.Fatalf("首次加载应得到 2 个账号，实际 %d，errs=%v", n, errs)
	}
	return p, authDir
}

// TestReloadKeepsPoolWhenAuthDirUnreadable 目录读不到时保留原池。
//
// 这是本文件的核心断言：账号数不变、账号对象不换 —— 后者同样重要，
// 换对象会让额度、成功失败计数、冷却、粘性等运行状态全部丢失。
func TestReloadKeepsPoolWhenAuthDirUnreadable(t *testing.T) {
	p, authDir := newAuthDirPool(t)

	before := p.Accounts()
	// 把目录改名，模拟「挂载点掉了 / 目录被删」。
	if err := os.Rename(authDir, authDir+"-gone"); err != nil {
		t.Fatalf("改名目录失败: %v", err)
	}

	if changed := p.Reload(); changed {
		t.Errorf("目录不可读时不应报告变更（会让上层以为池子真的变了）")
	}

	after := p.Accounts()
	if len(after) != len(before) {
		t.Fatalf("目录不可读不该清空账号池：前 %d 个，后 %d 个", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("第 %d 个账号对象被换掉了，运行状态会丢失", i)
		}
	}
	// 必须把「目录读不到」记成加载错误，否则上层会渲染成正常的「0 个账号」。
	if p.LoadError() == "" {
		t.Errorf("目录不可读应记入加载错误，实际为空")
	}
}

// TestReloadRecoversAfterAuthDirReturns 目录恢复后正常收敛，且清掉上一轮的错误。
func TestReloadRecoversAfterAuthDirReturns(t *testing.T) {
	p, authDir := newAuthDirPool(t)

	if err := os.Rename(authDir, authDir+"-gone"); err != nil {
		t.Fatalf("改名目录失败: %v", err)
	}
	p.Reload()
	if p.LoadError() == "" {
		t.Fatalf("前置条件不成立：目录不可读时应已记入错误")
	}

	// 目录回来，并且多了一个新账号。
	if err := os.Rename(authDir+"-gone", authDir); err != nil {
		t.Fatalf("恢复目录失败: %v", err)
	}
	writeCred(t, authDir, "workbuddy-c.json", "uid-c")

	if changed := p.Reload(); !changed {
		t.Errorf("目录恢复且新增了凭据，应报告变更")
	}
	if n := len(p.Accounts()); n != 3 {
		t.Errorf("恢复后应重新见到 3 个账号（含新增），实际 %d", n)
	}
	if err := p.LoadError(); err != "" {
		t.Errorf("扫描成功后应清掉上一轮的目录错误，实际 %q", err)
	}
}

// TestReloadStillClearsWhenAuthDirEmptied 目录**可读但为空**时仍照常清空 ——
// 守卫只挡「读不到」，不挡「用户真的把凭据都删了」这个正当操作。
func TestReloadStillClearsWhenAuthDirEmptied(t *testing.T) {
	p, authDir := newAuthDirPool(t)

	entries, err := os.ReadDir(authDir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(authDir, e.Name())); err != nil {
			t.Fatalf("删凭据失败: %v", err)
		}
	}

	if changed := p.Reload(); !changed {
		t.Errorf("清空凭据后应报告变更")
	}
	if n := len(p.Accounts()); n != 0 {
		t.Errorf("目录可读且确实为空时应收敛到 0 个账号，实际 %d", n)
	}
	if err := p.LoadError(); err != "" {
		t.Errorf("目录可读时不该报目录错误，实际 %q", err)
	}
}

// TestLoadReportsUnreadableAuthDir Load 也要如实报错：
// 启动时目录就不可读，不能被显示成正常的「0 个账号」。
func TestLoadReportsUnreadableAuthDir(t *testing.T) {
	work := t.TempDir()
	authDir := filepath.Join(work, "auths")
	writeCred(t, authDir, "workbuddy-a.json", "uid-a")

	cfg := &config.Config{WorkDir: work, AuthDir: authDir}
	cfg.ApplyDefaults()
	p := New(cfg, nil)
	if n, _ := p.Load(); n != 1 {
		t.Fatalf("前置条件不成立：首次加载应得到 1 个账号")
	}

	if err := os.Rename(authDir, authDir+"-gone"); err != nil {
		t.Fatalf("改名目录失败: %v", err)
	}
	n, errs := p.Load()
	if n != 0 {
		t.Errorf("目录不可读时确实拿不到账号，实际 %d", n)
	}
	if len(errs) == 0 {
		t.Errorf("目录不可读必须作为加载错误返回，而不是静默的 0 个账号")
	}
	if p.LoadError() == "" {
		t.Errorf("加载错误应能被上层读到")
	}
}

// TestReloadUnaffectedWhenNoAuthDir 未配置凭据目录（自动发现 / 显式文件）时守卫不介入：
// 那是另一套语义，不该被这次的改动影响。
func TestReloadUnaffectedWhenNoAuthDir(t *testing.T) {
	work := t.TempDir()
	writeCred(t, work, "workbuddy-a.json", "uid-a")

	cfg := &config.Config{WorkDir: work}
	cfg.ApplyDefaults()
	p := New(cfg, nil)
	if n, errs := p.Load(); n != 1 {
		t.Fatalf("自动发现应得到 1 个账号，实际 %d，errs=%v", n, errs)
	}
	// 工作目录一直可读，重复 reload 不该报错也不该丢账号。
	if changed := p.Reload(); changed {
		t.Errorf("没有任何变化时不该报告变更")
	}
	if n := len(p.Accounts()); n != 1 {
		t.Errorf("账号数应保持 1，实际 %d", n)
	}
	if err := p.LoadError(); err != "" {
		t.Errorf("不该有加载错误，实际 %q", err)
	}
}
