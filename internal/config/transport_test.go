package config

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// get 打一次真实请求（走外壳的 RoundTrip），用来触发「比对超时 → 必要时克隆」。
func hotTransportGet(t *testing.T, hot *HotTransport, url string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := hot.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip 失败: %v", err)
	}
	_ = resp.Body.Close()
}

// TestHotTransportFollowsTimeout 验证响应头等待上限热改的三条性质：
//  1. 值没变时**不**克隆 —— 每次请求都克隆会白白丢掉连接池；
//  2. 值变了才换一份 Transport —— 不能原地改活跃请求正在读的字段；
//  3. 新值确实落到新的那份上，之后不再重复克隆。
func TestHotTransportFollowsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cur := 5 * time.Second
	hot := NewHotTransport(&http.Transport{ResponseHeaderTimeout: cur}, func() time.Duration { return cur })
	first := hot.Base()

	hotTransportGet(t, hot, srv.URL)
	if hot.Base() != first {
		t.Fatal("超时没变时不应克隆 Transport（会丢连接池）")
	}

	cur = 30 * time.Second
	hotTransportGet(t, hot, srv.URL)
	if hot.Base() == first {
		t.Fatal("超时变了应当换一份 Transport，而不是原地改")
	}
	if got := hot.Base().ResponseHeaderTimeout; got != cur {
		t.Fatalf("新 Transport 的超时未更新，期望 %v 实际 %v", cur, got)
	}

	swapped := hot.Base()
	hotTransportGet(t, hot, srv.URL)
	if hot.Base() != swapped {
		t.Fatal("超时没再变，不应重复克隆")
	}
}

// TestHotTransportWithoutSource 没有配置来源时沿用装配值：
// 把「没接入配置」误当成「超时 0（不限制）」会让上游卡死的请求永远挂着。
func TestHotTransportWithoutSource(t *testing.T) {
	hot := NewHotTransport(&http.Transport{ResponseHeaderTimeout: 5 * time.Second}, nil)
	if got := hot.Base().ResponseHeaderTimeout; got != 5*time.Second {
		t.Fatalf("未注入来源时超时应保持装配值，实际 %v", got)
	}
	hot.SetTimeoutSource(nil)
	if got := hot.Base().ResponseHeaderTimeout; got != 5*time.Second {
		t.Fatalf("清空来源后超时应保持装配值，实际 %v", got)
	}
	hot.CloseIdleConnections()
}

// TestConfigHotTimeoutSharesWrapper 验证配置层接线：
// Control / Chat 共用同一个外壳（连接池只有一套），且钩子读的是**当前快照**——
// 换了配置快照之后，下一次请求就该用新超时。
func TestConfigHotTimeoutSharesWrapper(t *testing.T) {
	c := &Config{HeaderTimeout: 5 * time.Second, IdleTimeout: DefaultUpstreamIdleTimeout}
	if err := c.buildClients(); err != nil {
		t.Fatalf("装配客户端失败: %v", err)
	}
	if c.Control.Transport != c.Chat.Transport {
		t.Fatal("Control 与 Chat 应当共用同一个 Transport 外壳")
	}
	// 保存配置走的是「克隆 → 改克隆 → 换指针」，克隆体必须共享同一份外壳，
	// 否则每次保存都会凭空多出一套连接池，热改也就无从谈起。
	next := c.Clone()
	if next.hotTransport != c.hotTransport {
		t.Fatal("Clone 应当共享 Transport 外壳（连接池只有一套）")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hot := c.hotTransport
	if got := hot.Base().ResponseHeaderTimeout; got != 5*time.Second {
		t.Fatalf("装配值应为 5s，实际 %v", got)
	}

	// 模拟保存后「快照已换」：钩子读 next，而不是把值固定在 old 上。
	next.HeaderTimeout = 42 * time.Second
	c.SetHeaderTimeoutSource(func() time.Duration { return next.HeaderTimeout })

	hotTransportGet(t, hot, srv.URL)
	if got := hot.Base().ResponseHeaderTimeout; got != 42*time.Second {
		t.Fatalf("换快照后应当用新超时，实际 %v", got)
	}
}
