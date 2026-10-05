package main

import (
	"context"
	"io"
	"log"
	"os"
)

// parentWatchEnv 是「本次进程由代管方（桌面壳）拉起」的标记。
//
// 为什么必须显式开启而不是自动判断 stdin 是不是管道：命令行与容器场景下
// stdin 经常是 /dev/null、已关闭的句柄、或指向空文件 —— 那些情况读到 EOF
// 会**立刻退出**，等于把 `./workbuddy-gateway serve < /dev/null` 和
// Docker 部署全部打挂。只有代管方明确声明「stdin 是我给的管道」时才启用。
const parentWatchEnv = "WB_GATEWAY_PARENT_WATCH"

// watchParentExit 在父进程关闭 stdin 管道时取消 ctx。
//
// ## 解决什么
//
// 桌面壳退出时会主动 kill 网关子进程，但壳**自己被强制结束**时那条 kill
// 根本不会执行 —— 触发场景包括：安装程序的 taskkill、任务管理器「结束任务」、
// 壳自身崩溃。此时网关变成孤儿进程，继续占着
// `bin\workbuddy-gateway.exe`，于是用户遇到的现象是
// 「安装程序提示先退出软件、点了确定，还是报文件被占用装不上」。
//
// ## 为什么 stdin EOF 能代表「父进程没了」
//
// 管道由父进程创建，写端只在父进程手里。父进程无论以何种方式消亡，
// 操作系统都会关闭它的句柄，读端随即读到 EOF。这比轮询父进程 PID 可靠，
// 也不依赖 Windows 专有 API。
//
// 读到 EOF 后走的是与收到 SIGTERM **完全相同**的退出路径：HTTP 优雅关闭、
// 统计刷盘、并收掉本机代理子进程（`Supervisor.Stop`）。所以孤儿不会残留。
func watchParentExit(ctx context.Context, cancel context.CancelFunc) {
	if os.Getenv(parentWatchEnv) == "" {
		return
	}
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				// 有人在往 stdin 里写：这是交互式终端而不是代管管道，
				// 之后的内容与「父进程是否存活」无关，就此收手。
				return
			}
			if err != nil {
				if err == io.EOF {
					log.Printf("[父进程] stdin 已关闭，判定代管进程已退出，开始优雅退出")
				} else {
					log.Printf("[父进程] 读取 stdin 失败（%v），按代管进程已退出处理", err)
				}
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
}
