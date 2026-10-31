package server

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// streamHeartbeatInterval 是首字节前 SSE 心跳的发送间隔（对照 wb-gateway v1.14.24）。
//
// 为什么需要它：上游收到请求后要先排队、预处理，再开始出字。长思考模型这段空窗
// 可能长达几十秒，而客户端（各家 SDK 与浏览器）普遍有「多久收不到任何字节就判定
// 连接死掉」的读超时。SSE 注释帧（以冒号开头）是协议里合法的空操作，客户端会忽略
// 内容但会刷新读超时计时器 —— 正好用来把这段空窗填上。
//
// 15 秒是个折中：比绝大多数客户端读超时（通常 30–60 秒）短一半以上，
// 又不会在正常快请求上产生无谓的帧。
const streamHeartbeatInterval = 15 * time.Second

// firstByteHeartbeat 在首个有效数据到达前周期性下发 SSE 注释帧。
//
// # 为什么不需要写锁
//
// 上游那版用 streamWriteGuard 串行化心跳与数据写入，是因为它的心跳 goroutine 与
// 主流读取可能**同时**持有 ResponseWriter。这里的约束更强也更简单：
//
//   - 心跳只在「还没有任何数据被写出」时运行；
//   - 调用方在写第一个数据帧**之前**调用 Stop()，而 Stop 会一直等到 goroutine 退出。
//
// 于是两者的写入在时间上严格不重叠，不需要 mutex。这个前提写在调用点上（见
// streamChatResponse），改那里时要一起看。
type firstByteHeartbeat struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// startFirstByteHeartbeat 启动心跳；interval <= 0 时返回 nil（调用方按空操作处理）。
func startFirstByteHeartbeat(w http.ResponseWriter, flusher http.Flusher, interval time.Duration) *firstByteHeartbeat {
	if interval <= 0 || w == nil || flusher == nil {
		return nil
	}
	h := &firstByteHeartbeat{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-ticker.C:
				_, _ = fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}()
	return h
}

// Stop 停止心跳并等待 goroutine 退出；可重复调用，nil 安全。
//
// 必须等待退出（而不是发个信号就走）：否则 handler 返回后 goroutine 仍可能写
// ResponseWriter，那是 net/http 明确禁止的用法。
func (h *firstByteHeartbeat) Stop() {
	if h == nil {
		return
	}
	h.once.Do(func() { close(h.stop) })
	<-h.done
}
