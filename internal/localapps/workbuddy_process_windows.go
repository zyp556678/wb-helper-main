//go:build windows

package localapps

import (
	"strconv"
	"syscall"
)

// procAttrNoWindow 让命令执行不弹控制台窗口。
func procAttrNoWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// procAttrDetached 让启动的 GUI 进程脱离当前进程组/控制台。
//
// DETACHED_PROCESS：不继承控制台；CREATE_NEW_PROCESS_GROUP：不被网关收到的
// Ctrl 类事件波及。两者都不设时，客户端会挂在网关的控制台下 —— 网关重启
// （桌面壳拉起/退出）时可能把它一起带走。
func procAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200}
}

// wbKillCmd 返回结束某个 PID 的命令。
//
// **按 PID 结束，不按映像名子串**（对照 wb-switch：`pgrep -f` 式子串匹配会误伤
// 命令行里恰好含同名字符串的无关进程）。`/T` 连子进程一起结束 —— 客户端是多进程的
// （主进程 + 渲染/工具子进程），只杀主进程会留下孤儿。
// force=false 是优雅关闭（不带 /F），进程有机会自己收尾；超时才升级为 /F。
func wbKillCmd(pid int, force bool) (string, []string) {
	args := []string{"/PID", strconv.Itoa(pid), "/T"}
	if force {
		args = append(args, "/F")
	}
	return "taskkill", args
}
