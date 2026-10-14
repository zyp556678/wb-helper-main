package main

// install / uninstall 命令行的参数与「卸载转交」判定。
//
// 这里只测**纯逻辑**（参数解析、自身位置判定）：真正的转交要启动进程、写 %TEMP%，
// 那部分靠实机端到端验证（见 PROGRESS.md 的「Windows 卸载」一节），单元测试里重复
// 一遍只会得到一个脆弱的替身。
//
// 转交本身没有测试，但它的**触发条件**有 —— 而这个条件判错的两个方向都很贵：
// 该转交时不转交，卸载会因「删不掉自己」失败；不该转交时转交，绿色版卸载会去动
// 系统里另一个正规安装。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// discard 吞掉 flag 集合的错误输出，测试里不需要看到它。
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// mustExecutable 返回当前测试进程的可执行文件路径。
func mustExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// TestSelfInsideDir 自身位于目标目录下时才需要转交。
func TestSelfInsideDir(t *testing.T) {
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}

	// 空目录永远不触发转交：命令行上没给 -dir 时不能凭空认定「自己在里面」，
	// 否则会启动一个删不到东西的副本，而真正的卸载从未发生。
	if selfInsideDir("") {
		t.Fatal("目标目录为空时不该判定为「自身在目录内」")
	}

	// 完全无关的目录同理。
	if selfInsideDir(filepath.Join(self, "no-such-dir-xyz")) {
		t.Fatal("不存在的目录不该判定为「自身在目录内」")
	}

	// 正向用例只在 Windows 上有意义：Unix 侧的 selfInsideDir 固定返回 false
	// （那边 inode 语义允许删除正在运行的文件，不需要转交）。
	if runtime.GOOS != "windows" {
		return
	}

	// 当前进程的可执行文件必然位于它自己的目录之下 —— 这正是「从安装目录里
	// 执行卸载」的形态，必须是 true。
	exe, err := filepath.EvalSymlinks(mustExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	if !selfInsideDir(filepath.Dir(exe)) {
		t.Fatalf("自身可执行文件所在目录应判定为「在目录内」：%s", filepath.Dir(exe))
	}
}

// TestUninstallFlagParsing 解析 -no-register 与 -wait-pid。
//
// 这两个开关都直接决定行为：on 与否决定要不要写系统状态、要不要等前一个进程退出。
// 解析漏一个的后果不是报错，而是「安静地做了错的事」。
func TestUninstallFlagParsing(t *testing.T) {
	var f installFlags
	fs := newInstallFlagSet("uninstall", &f, discard{})
	if err := fs.Parse([]string{"-dir", `C:\x`, "-no-register", "-wait-pid", "4321", "-drop-data"}); err != nil {
		t.Fatal(err)
	}
	if f.installDir != `C:\x` {
		t.Fatalf("-dir 未解析：%q", f.installDir)
	}
	if !f.noRegister {
		t.Fatal("-no-register 未解析")
	}
	if f.waitPID != 4321 {
		t.Fatalf("-wait-pid 未解析：%d", f.waitPID)
	}
	if !f.dropData {
		t.Fatal("-drop-data 未解析")
	}

	// 默认值：不设开关时必须是「会注册、不等任何进程」——绿色版的语义只有在
	// 显式传 -no-register 时才生效，不能是默认。
	var g installFlags
	fs2 := newInstallFlagSet("install", &g, discard{})
	if err := fs2.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if g.noRegister {
		t.Fatal("默认不该是 no-register")
	}
	if g.waitPID != 0 {
		t.Fatalf("默认 wait-pid 应为 0，实际 %d", g.waitPID)
	}
}
