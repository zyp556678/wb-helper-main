package main

import (
	"log"
	"os"
)

// 本文件实现「安装更新之后的自我重启」。
//
// # 为什么是自己 exec 自己
//
// 桌面壳把网关当子进程代管，而它**没有**「子进程退出了就自动拉起」的逻辑
//（只有托盘里的手动重启）—— 直接退出会让面板停在「网关进程已退出」，
// 用户得自己去托盘点一下。
//
// syscall.Exec 把当前进程映像换成新装好的二进制，**PID 不变**：壳手里的
// Child 句柄、那条 stdin 管道、「是不是我起的实例」这些判断全都继续有效，
// 面板也会在几秒内重连。这是 Unix 上唯一能做到「装完无缝跑上新版本」的办法。
//
// # 调用点的硬约束
//
// exec 会跳过所有 defer —— 连接池、sqlite、本机代理子进程、统计刷盘都得在它之前
// 收干净。所以调用点在 runServe 的收尾处（优雅关闭完成之后），而不是在
// HTTP 处理器里。

// managedBySystemd 报告本进程是不是 systemd 单元的主进程。
//
// 判据是 INVOCATION_ID：systemd 每次拉起服务都会注入一个唯一的 invocation id，
// 其它启动方式（命令行、桌面壳、Docker、nohup）都没有这个变量。
//
// 为什么必须区分：systemd 的 restart 语义是「给主进程发 SIGTERM → 等它退出 → 起新的」。
// exec 保持 PID 不变，systemd 会一直等不到「退出」这个事件，最后按 TimeoutStopSec
// （默认 90 秒）强杀 —— 比不重启还糟。而由 systemd 代管时本来也不需要我们做什么：
// deb/rpm 的 postinst 会 systemctl restart，之后新版本自然就起来了。
func managedBySystemd() bool { return os.Getenv("INVOCATION_ID") != "" }

// reExecSelf 用新装好的二进制替换当前进程映像（仅 Unix；Windows 由安装程序接管）。
func reExecSelf(exe string) {
	if managedBySystemd() {
		log.Printf("[更新] 本进程由 systemd 代管，服务会被 systemd 重启，这里不做自我重启")
		return
	}
	if exe == "" {
		log.Printf("[更新] 拿不到自身可执行文件路径，请手动重启网关以运行新版本")
		return
	}
	log.Printf("[更新] 正在用 %s 重启网关以运行新版本", exe)
	if err := execSelf(exe); err != nil {
		log.Printf("[更新] 自我重启失败（%v），请手动重启网关以运行新版本", err)
	}
}
