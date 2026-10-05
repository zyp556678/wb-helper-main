//go:build !windows

package localapps

import (
	"strconv"
	"syscall"
)

// procAttrNoWindow 在非 Windows 上无对应概念（不会弹控制台窗口）。
func procAttrNoWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// procAttrDetached 让启动的 GUI 进程脱离当前会话，不随网关退出而被带走。
func procAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// wbKillCmd 返回结束某个 PID 的命令（优雅 SIGTERM / 强杀 SIGKILL）。
//
// 按 PID 结束而不是 pkill -f 模式匹配：模式匹配会误伤命令行里恰好含同名字符串的
// 无关进程（我们的网关二进制名里就带 workbuddy）。
func wbKillCmd(pid int, force bool) (string, []string) {
	sig := "-15"
	if force {
		sig = "-9"
	}
	return "kill", []string{sig, strconv.Itoa(pid)}
}
