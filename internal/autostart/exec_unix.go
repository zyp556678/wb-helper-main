//go:build !windows

package autostart

import (
	"os"
	"os/exec"
	"strings"
)

// parentExecutable 取父进程的可执行文件路径。
//
// 非 Windows 上没有 `${pid}exe` 可读，退到 `ps`：`-o comm=` 给的是命令名而非
// 绝对路径，因此再拼一次 LookPath 得到绝对路径。取不到就返回错误，
// 调用方（Enable）会如实报错而不是写一个错的自启路径。
func parentExecutable() (string, error) {
	out, err := runHidden("ps", "-p", itoa(os.Getppid()), "-o", "comm=")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", os.ErrNotExist
	}
	if abs, err := exec.LookPath(name); err == nil {
		return abs, nil
	}
	return name, nil
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
