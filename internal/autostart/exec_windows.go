//go:build windows

package autostart

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// parentExecutable 取父进程的可执行文件完整路径。
//
// 为什么需要它：桌面壳启动网关时会设 `WB_GATEWAY_PARENT_WATCH=1`，
// 此时自启项要指向**壳**而不是网关自己 —— 直接自启 `workbuddy-gateway.exe serve`
// 会得到一个没界面、用户无处关闭的后台进程。而「壳在哪」最可靠的来源就是父进程。
//
// 为什么不用 `os.Getppid()` 再查进程表：Windows 上没有 /proc，查父进程需要
// Toolhelp32 快照或 WMI，而 `GetModuleFileNameEx(父进程句柄)` 一次调用就能拿到，
// 也不用引第三方库（syscall 里就有）。
func parentExecutable() (string, error) {
	// PROCESS_QUERY_LIMITED_INFORMATION：只要能查信息即可，不要求完整权限。
	// 拿完整权限在受保护进程上会失败，而这个权限几乎总能拿到。
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(os.Getppid()))
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(h)

	var buf [syscall.MAX_PATH * 4]uint16
	size := uint32(len(buf))
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")
	r1, _, e := proc.Call(
		uintptr(h),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 {
		return "", e
	}
	return syscall.UTF16ToString(buf[:size]), nil
}

// prepareHidden 让子进程不弹控制台窗口。
//
// 网关自己没有控制台（桌面版是托盘应用、命令行版则在后台），调 reg.exe /
// schtasks.exe 时若不设这个标志，用户会看到一个黑框一闪而过。
func prepareHidden(cmd *exec.Cmd) {
	const createNoWindow = 0x0800_0000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
