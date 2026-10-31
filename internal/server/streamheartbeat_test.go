package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 首字节前心跳（对照 wb-gateway v1.14.24）。
//
// 这组用例守的是三条性质：
//  1. 没有数据时确实周期性下发 SSE 注释帧；
//  2. Stop 之后**一个字节都不再写**（handler 返回后写 ResponseWriter 是未定义行为）；
//  3. interval <= 0 时整体禁用，且 nil 上调用 Stop 安全。

func TestFirstByteHeartbeatWritesComments(t *testing.T) {
	rec := httptest.NewRecorder()
	hb := startFirstByteHeartbeat(rec, rec, 5*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	hb.Stop()

	body := rec.Body.String()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("没有下发心跳帧，实际 body=%q", body)
	}
	// SSE 注释必须以冒号开头、以空行结束，否则客户端会当成坏帧。
	if !strings.Contains(body, ": keepalive\n\n") {
		t.Errorf("心跳帧不是合法的 SSE 注释形态: %q", body)
	}
}

func TestFirstByteHeartbeatStopsWriting(t *testing.T) {
	rec := httptest.NewRecorder()
	hb := startFirstByteHeartbeat(rec, rec, 5*time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	hb.Stop()

	after := rec.Body.Len()
	time.Sleep(40 * time.Millisecond)
	if rec.Body.Len() != after {
		t.Fatalf("Stop 之后仍在写：%d -> %d 字节", after, rec.Body.Len())
	}
}

func TestFirstByteHeartbeatDisabled(t *testing.T) {
	rec := httptest.NewRecorder()
	if hb := startFirstByteHeartbeat(rec, rec, 0); hb != nil {
		t.Fatal("interval=0 时应返回 nil（禁用）")
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("禁用后不该写任何字节，实际 %q", body)
	}
	// nil 上调用 Stop 必须是空操作。
	var nilHB *firstByteHeartbeat
	nilHB.Stop()
}

func TestFirstByteHeartbeatStopIsIdempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	hb := startFirstByteHeartbeat(rec, rec, 5*time.Millisecond)
	time.Sleep(15 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		hb.Stop()
		hb.Stop() // 第二次必须立刻返回，不能阻塞
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("重复调用 Stop 阻塞了 —— 调用点会在首个数据帧处卡死")
	}
}
