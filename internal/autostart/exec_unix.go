//go:build !windows

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// parentExecutable 取父进程的可执行文件路径（用于把自启项指向宿主，即桌面壳）。
//
// 判定顺序：
//  1. Linux：读 `/proc/<ppid>/exe`。它是内核维护的符号链接，给的是**真实完整路径**。
//  2. 退到 `ps -p <ppid> -o comm=`，再解析成绝对路径（macOS 与没有 /proc 的场合）。
//
// # 为什么不能拿 `ps -o comm=` 当主路径
//
// Linux 上 `comm` 取的是 `/proc/<pid>/stat` 里的 comm 字段，而内核把它限制在
// **15 个字符**（`TASK_COMM_LEN - 1`）。于是
// `/usr/bin/workbuddy-gateway-desktop` 会变成 `workbuddy-gatew` —— 它既不是路径，
// `LookPath` 也找不到（PATH 里没有叫这个名字的文件），最后被原样写进自启项的
// `Exec=`。症状极具迷惑性：**面板显示「已开启」，开机却什么都没发生，且全程无任何报错**
// （GNOME 只会静默地执行失败）。macOS 的 BSD ps 不同，`comm` 给的就是完整路径，
// 所以第 2 步在那边依然可用。
func parentExecutable() (string, error) {
	ppid := os.Getppid()

	if exe, err := procExePath(ppid); err == nil {
		return exe, nil
	}

	out, err := runHidden("ps", "-p", itoa(ppid), "-o", "comm=")
	if err != nil {
		return "", err
	}
	return resolveProcessName(strings.TrimSpace(out))
}

// procExePath 读 `/proc/<pid>/exe`，返回可执行文件的真实路径。
//
// 已删除/已被替换的可执行文件会带上 `" (deleted)"` 后缀（升级覆盖安装时会出现），
// 去掉它才是可用的路径 —— 而且去掉后正好指向**新版本**的文件，这正是自启想要的。
func procExePath(pid int) (string, error) {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return "", err
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	// 极少数情况下（内核线程等）链接内容是空的，或形如 "pipe:[123]"，一律不认。
	if exe == "" || !filepath.IsAbs(exe) {
		return "", fmt.Errorf("/proc/%d/exe 不是绝对路径：%q", pid, exe)
	}
	return exe, nil
}

// resolveProcessName 把 `ps -o comm=` 的输出解析成可用的绝对路径。
//
// 硬约束：**解析不出来就返回错误，绝不把裸名字当路径交出去**。
// 这条约束是这次事故的直接产物 —— 交给调用方一个 `workbuddy-gatew`，
// 它会被写进自启项、在下次开机时静默失败，而排错时看到的只是一份「看起来正常」的
// desktop 文件。宁可在这里失败得响一点。
func resolveProcessName(name string) (string, error) {
	if name == "" {
		return "", os.ErrNotExist
	}
	if filepath.IsAbs(name) {
		return name, nil
	}
	if abs, err := exec.LookPath(name); err == nil && filepath.IsAbs(abs) {
		return abs, nil
	}
	return "", fmt.Errorf("进程名 %q 既不是绝对路径、也无法在 PATH 中解析为可执行文件", name)
}

// prepareHidden 在 Unix 上无需处理：这些平台本来就不会弹控制台窗口。
func prepareHidden(_ *exec.Cmd) {}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
