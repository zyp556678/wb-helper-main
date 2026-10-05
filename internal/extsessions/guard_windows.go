//go:build windows

package extsessions

import "syscall"

// editorProcAttr 让新进程脱离当前进程组（网关重启不会把它一起带走）。
func editorProcAttr() *syscall.SysProcAttr {
	const (
		detachedProcess       = 0x00000008
		createNewProcessGroup = 0x00000200
		createNoWindow        = 0x08000000
	)
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup | createNoWindow}
}
