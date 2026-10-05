//go:build !windows

package extsessions

import "syscall"

// editorProcAttr 让新进程脱离当前进程组（网关重启不会把它一起带走）。
func editorProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
