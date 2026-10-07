//go:build !windows

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveProcessNameRejectsTruncatedComm 是本文件存在的主要理由。
//
// Linux 内核把 /proc/<pid>/stat 的 comm 字段限制在 15 个字符（TASK_COMM_LEN-1），
// 所以 `/usr/bin/workbuddy-gateway-desktop` 经 `ps -o comm=` 出来是
// `workbuddy-gatew`。它既不是路径、PATH 里也找不到对应文件。
//
// 修复前 parentExecutable 会把这个裸名字原样返回，被写进自启项的 `Exec=`，
// 结果就是「面板显示已开启、开机什么都没发生，且全程无报错」。
// 这条断言把「解析不出来就必须报错」钉死，防止再退回去。
func TestResolveProcessNameRejectsTruncatedComm(t *testing.T) {
	// 15 字符截断后的真实产物：既不是路径，PATH 里也没有这个名字。
	got, err := resolveProcessName("workbuddy-gatew")
	if err == nil {
		t.Errorf("截断的进程名应报错，实际返回 %q —— 它会变成一个开机静默失败的自启项", got)
	}

	// 名字完整但根本不存在于 PATH 的，同样不能当路径交出去。
	// （注意别拿 `workbuddy-gateway-desktop` 当反例：装了桌面版 deb 的机器上它就在
	// /usr/bin 里，LookPath 解析得出来，那是**正确**行为 —— 这条断言曾经写错过。）
	if got, err := resolveProcessName("definitely-not-a-real-binary-xyz"); err == nil {
		t.Errorf("PATH 中不存在的裸名字应报错，实际返回 %q", got)
	}
}

func TestResolveProcessNameAcceptsAbsolutePath(t *testing.T) {
	// macOS 的 BSD ps 给的就是完整路径，这条必须原样通过，不能因为校验而回归。
	abs := "/usr/bin/workbuddy-gateway-desktop"
	got, err := resolveProcessName(abs)
	if err != nil {
		t.Fatalf("绝对路径应被接受，实际报错：%v", err)
	}
	if got != abs {
		t.Errorf("应原样返回 %q，实际 %q", abs, got)
	}
}

func TestResolveProcessNameRejectsEmpty(t *testing.T) {
	if _, err := resolveProcessName(""); err == nil {
		t.Error("空进程名应报错")
	}
}

// TestProcExePathReturnsAbsoluteExistingFile 覆盖主路径。
//
// 用自己这个测试进程当样本：/proc/self/exe 一定存在且可读，
// 而且**不受 15 字符限制** —— 这正是它与 `ps -o comm=` 的关键差别。
func TestProcExePathReturnsAbsoluteExistingFile(t *testing.T) {
	got, err := procExePath(os.Getpid())
	if err != nil {
		// 没有挂 /proc 的环境（部分容器）跳过，而不是判失败。
		t.Skipf("本环境读不到 /proc/<pid>/exe，跳过：%v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("应返回绝对路径，实际 %q", got)
	}
	if strings.HasSuffix(got, " (deleted)") {
		t.Errorf("应剥掉 \" (deleted)\" 后缀，实际 %q", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("返回的路径应真实存在：%v", err)
	}
}

// TestParentExecutableIsUsableAsAutostartTarget 是端到端的那条性质：
// parentExecutable 的结果必须能直接写进自启项 —— 绝对路径 + 真实存在。
//
// 它不会复现 15 字符截断（`go test` 的父进程名很短），所以只是兜底；
// 真正的回归防线是上面的 TestResolveProcessNameRejectsTruncatedComm。
func TestParentExecutableIsUsableAsAutostartTarget(t *testing.T) {
	got, err := parentExecutable()
	if err != nil {
		t.Skipf("本环境取不到父进程可执行文件，跳过：%v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("自启项目标必须是绝对路径，实际 %q", got)
	}
	if info, err := os.Stat(got); err != nil || info.IsDir() {
		t.Fatalf("自启项目标必须真实存在，实际 %q（%v）", got, err)
	}
}
