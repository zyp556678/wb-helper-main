package update

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// regQueryOutput 是 `reg query` 在真实机器上的输出形态（缩进、值里的空格、
// 以及一条我们不需要的 (Default) 行）。
const regQueryOutput = `
HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Uninstall\WorkBuddy Gateway
    (Default)    REG_SZ    CurrentUser
    DisplayName    REG_SZ    WorkBuddy Gateway
    MainBinaryName    REG_SZ    workbuddy-gateway-desktop.exe
    DisplayVersion    REG_SZ    0.13.2
    InstallLocation    REG_SZ    "C:\Users\u\AppData\Local\WorkBuddy Gateway"
    UninstallString    REG_SZ    "C:\Users\u\AppData\Local\WorkBuddy Gateway\uninstall.exe"
`

// TestParseRegQuery 解析要吃得下真实输出：缩进、值里的空格、以及不关心的行。
func TestParseRegQuery(t *testing.T) {
	e := parseRegQuery(regQueryOutput)
	if e.DisplayName != "WorkBuddy Gateway" {
		t.Fatalf("DisplayName 解析错误：%q", e.DisplayName)
	}
	if e.MainBinaryName != "workbuddy-gateway-desktop.exe" {
		t.Fatalf("MainBinaryName 解析错误：%q", e.MainBinaryName)
	}
	if e.InstallLocation != `"C:\Users\u\AppData\Local\WorkBuddy Gateway"` {
		t.Fatalf("InstallLocation 应原样保留（含引号）：%q", e.InstallLocation)
	}
	if got := e.uninstallerPath(); got != `C:\Users\u\AppData\Local\WorkBuddy Gateway\uninstall.exe` {
		t.Fatalf("卸载器路径应去掉引号，实际 %q", got)
	}
}

// TestUninstallerPath 取值要同时吃得住带引号、不带引号、以及带参数三种写法。
func TestUninstallerPath(t *testing.T) {
	cases := map[string]string{
		`"C:\a b\uninstall.exe"`:         `C:\a b\uninstall.exe`,
		`C:\a\uninstall.exe`:             `C:\a\uninstall.exe`,
		` "C:\a b\uninstall.exe" /S`:     `C:\a b\uninstall.exe`,
		`C:\a\uninstall.exe _?=C:\a`:     `C:\a\uninstall.exe`,
		`"C:\a b\uninstall.exe" _?=C:\a`: `C:\a b\uninstall.exe`,
		``:                               ``,
		`"C:\a b\uninstall.exe`:          `C:\a b\uninstall.exe`, // 只有前半引号
	}
	for in, want := range cases {
		e := uninstallEntry{UninstallString: in}
		if got := e.uninstallerPath(); got != want {
			t.Fatalf("uninstallerPath(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestStaleReason 判据只有一条：卸载器还在就绝不判残（那是向导要用的东西）。
func TestStaleReason(t *testing.T) {
	e := parseRegQuery(regQueryOutput)
	un := e.uninstallerPath()

	if got := e.staleReason(func(string) bool { return true }); got != "" {
		t.Fatalf("卸载器在时不该判残迹，实际 %q", got)
	}
	got := e.staleReason(func(string) bool { return false })
	if got == "" || !strings.Contains(got, un) {
		t.Fatalf("卸载器不在时应判残迹并点出路径，实际 %q", got)
	}

	// 读不到卸载器路径时判不了：宁可不动。
	empty := uninstallEntry{}
	if got := empty.staleReason(func(string) bool { return false }); got != "" {
		t.Fatalf("没有卸载器路径时不该判残迹，实际 %q", got)
	}
}

// fakeReg 记录调用并按时序返回预置结果，模拟 reg.exe。
type fakeReg struct {
	queryOut  string
	queryErr  error
	calls     [][]string
	deleteErr error
}

func (f *fakeReg) run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "query":
		return f.queryOut, f.queryErr
	case "delete":
		if f.deleteErr != nil {
			return "", f.deleteErr
		}
		return "操作成功完成。\n", nil
	}
	return "", errors.New("unexpected: " + strings.Join(args, " "))
}

// TestCleanStaleUninstallEntry 三条路径：键不存在不动、卸载器还在不动、
// 卸载器不在则两条键都删掉。
func TestCleanStaleUninstallEntry(t *testing.T) {
	dir := t.TempDir()
	uninstaller := filepath.Join(dir, "uninstall.exe")
	const tmpl = `
HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Uninstall\WorkBuddy Gateway
    DisplayName    REG_SZ    WorkBuddy Gateway
    UninstallString    REG_SZ    "%s"
`
	query := func() string { return strings.Replace(tmpl, "%s", uninstaller, 1) }

	// 1) 键不存在：什么都不做。
	f := &fakeReg{queryErr: errors.New("exit status 1")}
	if removed, reason := cleanStaleUninstallEntry(f.run, nil); removed || reason != "" {
		t.Fatalf("键不存在时不该有任何动作，实际 removed=%v reason=%q", removed, reason)
	}
	if len(f.calls) != 1 {
		t.Fatalf("键不存在时只该查一次，实际 %v", f.calls)
	}

	// 2) 卸载器还在：不碰。
	if err := os.WriteFile(uninstaller, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f = &fakeReg{queryOut: query()}
	if removed, reason := cleanStaleUninstallEntry(f.run, nil); removed || reason != "" {
		t.Fatalf("卸载器在时不该清理，实际 removed=%v reason=%q", removed, reason)
	}
	if len(f.calls) != 1 {
		t.Fatalf("卸载器在时只该查一次，实际 %v", f.calls)
	}

	// 3) 卸载器已不在（本机真实踩到的那种残键）：两条键都删。
	if err := os.Remove(uninstaller); err != nil {
		t.Fatal(err)
	}
	var logs []string
	f = &fakeReg{queryOut: query()}
	removed, reason := cleanStaleUninstallEntry(f.run, func(format string, args ...any) {
		logs = append(logs, format)
	})
	if !removed || reason == "" {
		t.Fatalf("残键应被清掉，实际 removed=%v reason=%q", removed, reason)
	}
	if len(f.calls) != 3 {
		t.Fatalf("应有一次查询 + 两次删除，实际 %v", f.calls)
	}
	for i, key := range []string{updateUninstallKey, updateVendorKey} {
		got := f.calls[i+1]
		if len(got) != 3 || got[0] != "delete" || got[1] != key || got[2] != "/f" {
			t.Fatalf("第 %d 次删除应为 delete %s /f，实际 %v", i+1, key, got)
		}
	}
	if len(logs) == 0 {
		t.Fatal("清理成功要留下一条日志，便于在面板日志里回溯")
	}

	// 4) 删除失败：如实返回「没清掉」，不能让调用方以为干净了。
	f = &fakeReg{queryOut: query(), deleteErr: errors.New("拒绝访问")}
	if removed, _ := cleanStaleUninstallEntry(f.run, nil); removed {
		t.Fatal("删除失败时不该报告已清理")
	}
}
