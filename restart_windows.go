//go:build windows

package main

import "errors"

// execSelf 在 Windows 上不存在可用的等价物：正在运行的 .exe 被系统锁住，
// 覆盖安装与随后重新启动都由安装程序（NSIS）完成 —— 见
// internal/update/install.go 的 installWindows 与 desktop/src-tauri/windows/hooks.nsh。
func execSelf(string) error {
	return errors.New("Windows 上没有 exec 语义，安装与重启由安装程序接管")
}
