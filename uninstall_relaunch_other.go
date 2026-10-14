//go:build !windows

package main

// Unix 上不需要「转交临时副本」这套：
//
//  - inode 语义允许删除正在运行的文件，所以卸载器删得掉自己所在的目录；
//  - stopProcesses 虽然同样会筛到「位于安装目录下的自己」，但那里的实现会跳过
//    当前进程（见 process_unix.go），不存在把自己杀掉的问题。
//
// 这几个函数仍要存在：install_cmd.go 的卸载分支是平台无关的，跨平台编译需要它们。

import "io"

func selfInsideDir(string) bool { return false }

func relaunchSelfForUninstall(installFlags, io.Writer, io.Writer) int { return 1 }

func waitForProcessExit(int) {}

func cleanupSelfTempDir() {}
