package config

import (
	"net/http"
	"sync"
	"time"
)

// 本文件实现「上游响应头等待上限热改」（对照上游 wb-gateway v1.14.0 的 hotConfigTransport）。
//
// 为什么需要一层外壳：`http.Transport` 的字段不能在活跃请求期间原地改 —— 它正在被
// 并发读取，改它既是数据竞争，也会让「换超时」这件事在语义上变得不可预期。此前的做法
// 是把 `upstream.header_timeout_seconds` 当**装配期字段**：改了必须重启进程，面板里
// 也标成只读。而实践中「上游某个时段变慢」是临时现象，为此重启网关要断掉所有在途会话。
//
// 做法（与上游同样的取舍）：每次 RoundTrip 比对一次目标值，**只有真的变了**才克隆一份
// Transport 换上去。新请求走新的那份，正在读取的旧流继续用旧的那份读到底；被换下来的
// 那份立即回收空闲连接（不打断已建立的流）。
type HotTransport struct {
	mu sync.Mutex
	tr *http.Transport
	// timeout 返回当前应生效的响应头等待上限；nil 表示沿用 tr 里的值（没有配置源时）。
	timeout func() time.Duration
}

// NewHotTransport 用一份基础 Transport 构造热改外壳。
func NewHotTransport(tr *http.Transport, timeout func() time.Duration) *HotTransport {
	return &HotTransport{tr: tr, timeout: timeout}
}

// SetTimeoutSource 注入「当前响应头等待上限」的来源。
//
// 用钩子而不是在 Config 上存一份值：Transport 是**跨配置快照共享**的
// （Clone 刻意不复制 http.Client，连接池只有一套），而每次保存配置都会整体换掉快照 ——
// 钩子读的是「此刻生效的那份快照」，才不会跟着某一版冻结。
func (t *HotTransport) SetTimeoutSource(fn func() time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.timeout = fn
	t.mu.Unlock()
}

// RoundTrip 实现 http.RoundTripper。
func (t *HotTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	tr := t.tr
	if t.timeout != nil {
		if want := t.timeout(); tr.ResponseHeaderTimeout != want {
			old := tr
			tr = old.Clone()
			tr.ResponseHeaderTimeout = want
			t.tr = tr
			// 只关空闲连接：仍在读的流不受影响（这正是「旧流读到底、新请求用新值」的关键）。
			old.CloseIdleConnections()
		}
	}
	t.mu.Unlock()
	return tr.RoundTrip(r)
}

// CloseIdleConnections 让外壳满足 http.Client 认的那个接口（换号重试会用到它）。
func (t *HotTransport) CloseIdleConnections() {
	if t == nil {
		return
	}
	t.mu.Lock()
	tr := t.tr
	t.mu.Unlock()
	tr.CloseIdleConnections()
}

// Base 返回底层 Transport（只读用途：检查更新要 Clone 一份改代理，不能拿外壳去断言）。
func (t *HotTransport) Base() *http.Transport {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tr
}
