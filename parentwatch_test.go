package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// withStdin 把 os.Stdin 临时换成管道读端，返回写端供测试模拟「父进程消亡」。
func withStdin(t *testing.T) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		r.Close()
		w.Close()
	})
	return w
}

// 开关打开 + 父进程（管道写端）消失 → 必须取消 ctx。
// 这条钉住的是用户实际遇到的问题：壳被强杀后网关变成孤儿占着二进制文件。
func TestWatchParentExitCancelsOnStdinEOF(t *testing.T) {
	t.Setenv(parentWatchEnv, "1")
	w := withStdin(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchParentExit(ctx, cancel)

	// 关闭写端 = 父进程消亡，操作系统回收句柄后读端读到 EOF。
	w.Close()

	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("stdin 关闭后 ctx 未被取消，孤儿进程防护失效")
	}
}

// 开关**没有**打开时必须完全不介入。
//
// 这条防的是回归成「自动检测 stdin」：命令行与容器场景下 stdin 常是
// /dev/null 或已关闭的句柄，一旦自动检测就会立刻退出，
// 等于把 `serve < /dev/null` 和 Docker 部署全部打挂。
func TestWatchParentExitInertWithoutEnv(t *testing.T) {
	t.Setenv(parentWatchEnv, "")
	w := withStdin(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchParentExit(ctx, cancel)

	w.Close()

	select {
	case <-ctx.Done():
		t.Fatal("未声明由代管方启动时不应因 stdin 关闭而退出")
	case <-time.After(600 * time.Millisecond):
		// 期望路径：ctx 保持存活。
	}
}

// 父进程往 stdin 里写了东西 → 说明这是交互式终端而不是代管管道，
// 之后的内容与父进程存活无关，看门狗应当收手。
func TestWatchParentExitStopsOnStdinInput(t *testing.T) {
	t.Setenv(parentWatchEnv, "1")
	w := withStdin(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchParentExit(ctx, cancel)

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("写入管道失败: %v", err)
	}
	// 给看门狗时间读到这段输入并退出。
	time.Sleep(300 * time.Millisecond)
	w.Close()

	select {
	case <-ctx.Done():
		t.Fatal("读到了 stdin 输入后不应再把它当作父进程退出信号")
	case <-time.After(600 * time.Millisecond):
	}
}
