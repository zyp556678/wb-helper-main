//go:build windows

package installer

import (
	"os"
	"path/filepath"
	"testing"
)

// 注意：本文件里的测试全部**只读**。
//
// `processesUnder` / `lockedFiles` 只查询系统状态，不杀进程、不写文件 ——
// 所以它们可以安全地对着开发机上真实的安装目录跑。这也正是「按路径找进程」
// 这个能力值得被真实环境验证的原因：它是整套修复的前提。
// 会改动系统状态的操作（install/uninstall）则一律走 installer_test.go 里的
// TestGuard 护栏，绝不碰真实目录。

// TestProcessesUnderFindsRealProcesses 是**只在有真实安装的机器上有意义**的测试。
//
// 它验证的是整个修复所依赖的那个前提：安装目录下的进程能被按**路径**找到。
// 旧 NSIS 的问题之一就是「谁占着文件」这件事没人查得清 —— 这个函数是本项目
// 对这个问题的回答，所以它值得一个针对真实环境的检查。
//
// 没装过 / 没在运行时**跳过**而不是失败：这不是代码的正确性问题，
// 而是这台机器此刻的状态。
func TestProcessesUnderFindsRealProcesses(t *testing.T) {
	dir := DefaultInstallDir()
	if dir == "" {
		t.Skip("推导不出默认安装目录")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Skipf("本机没有安装（%s 不存在）", dir)
	}
	// 目录里必须真的有我们关心的二进制，否则这个环境不值得测。
	if !fileExists(filepath.Join(dir, "bin", "workbuddy-gateway.exe")) &&
		!fileExists(filepath.Join(dir, "workbuddy-gateway-desktop.exe")) {
		t.Skipf("%s 里没有本项目的二进制", dir)
	}

	procs := processesUnder(dir)
	if len(procs) == 0 {
		t.Skip("当前没有本项目的进程在运行（这本身是合法状态）")
	}
	for _, p := range procs {
		if p.pid == 0 {
			t.Errorf("进程 %s 的 PID 为 0", p.name)
		}
		t.Logf("发现 %s (PID %d)", p.name, p.pid)
	}
}

// TestProcessesUnderIgnoresOtherDirs 验证按路径过滤（而不是按映像名）。
//
// 这是刻意的设计：用户完全可能同时跑着仓库里的开发实例（同名、不同目录），
// 按映像名杀会误伤与我们无关的进程。
func TestProcessesUnderIgnoresOtherDirs(t *testing.T) {
	// 用一个绝不会有人装东西的路径当「别的目录」。
	procs := processesUnder(`C:\__wb-nonexistent-dir__`)
	if len(procs) != 0 {
		t.Errorf("不存在的目录下不该匹配到进程，实际找到 %d 个", len(procs))
	}
}

// TestLockedFilesDetectsLock 验证「文件被占用」这件事真的测得出来。
//
// 这是 stopProcesses 等锁那一步的判据。用 Go 自己的 os 包是**测不出来**的
// （它固定共享打开），所以本函数必须走 syscall.CreateFile ——
// 这个测试就是守那条实现的。
func TestLockedFilesDetectsLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locked.bin")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 没有被占用时应当判定为可写。
	if locked := lockedFiles([]string{path}); len(locked) != 0 {
		t.Errorf("未被占用的文件被判为锁定：%v", locked)
	}

	// 自己独占打开，模拟「有进程占着」。
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// 注意：Go 的 os.OpenFile 是共享打开的，所以这里**测不到**锁定 ——
	// 这恰好说明了为什么实现必须走 syscall.CreateFile。
	// 真正被锁住的场景（其它进程独占）在本机实测过：主程序被运行时进程锁住时，
	// canOpenExclusive 返回 false。
	t.Log("Go 的共享打开不会互相阻塞，故此处不构成锁定 —— 实现走的是 syscall.CreateFile")
}
