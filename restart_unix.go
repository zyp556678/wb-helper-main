//go:build !windows

package main

import (
	"os"
	"syscall"
)

// execSelf 用 exe 替换当前进程映像。**成功时不返回** —— 调用方之后的所有代码都不会执行。
//
// argv[0] 用新的路径而不是 os.Args[0]：老路径在安装之后可能已经指向被替换掉的旧
// inode（dpkg 是「删旧文件再写新的」），留着它会让进程自己的 /proc/PID/exe 与 argv
// 对不上，排查时凭空多一层困惑。
func execSelf(exe string) error {
	argv := append([]string{exe}, os.Args[1:]...)
	return syscall.Exec(exe, argv, os.Environ())
}
