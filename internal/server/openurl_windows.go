//go:build windows

package server

import (
	"os/exec"
	"syscall"
)

// openInSystemBrowser 用系统默认浏览器打开链接。
//
// 走 rundll32 的 FileProtocolHandler 而不是 `cmd /C start`：
// exec.Command 直接传参、不经过 shell 解析，URL 里的 `&`、`?` 不会被 cmd 二次解释，
// 也不需要处理 `start` 把第一个参数当窗口标题的怪癖。
func openInSystemBrowserOS(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	// CREATE_NO_WINDOW：不闪一个控制台窗口（网关自身可能是无窗口进程）。
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
