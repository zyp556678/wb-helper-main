package server

import (
	"testing"
	"time"
)

// 登录会话的保留窗口是 v1.14.20 那个真实缺陷的修复点：
// 会话一进终态就删除 → 前端最后一次轮询拿到「会话不存在」，
// 表现成「链接失效了但页面还在等」。这里把三条边界钉住。
func TestLoginSessionKeptAfterTerminalState(t *testing.T) {
	store := newLoginStore()
	now := time.Now()
	sess := &loginSession{
		ID:        "s1",
		ExpiresAt: now.Add(5 * time.Minute),
		Status:    loginPending,
	}
	store.put(sess)

	sess.finish(loginExpired, "登录已超时，请重新发起")

	// 刚结束：必须还在，轮询要能读到 expired。
	store.gc(now.Add(time.Second))
	if got := store.get("s1"); got == nil {
		t.Fatal("会话刚进入终态就被清理，前端最后一次轮询会读到「不存在」")
	} else if got.Status != loginExpired {
		t.Fatalf("终态被改写：%s", got.Status)
	}

	// 保留窗口内：仍在。
	store.gc(now.Add(loginSessionRetention - time.Second))
	if store.get("s1") == nil {
		t.Fatal("保留窗口内会话被提前清理")
	}

	// 超过保留窗口：清理。
	store.gc(now.Add(loginSessionRetention + time.Second))
	if store.get("s1") != nil {
		t.Fatal("超过保留窗口后会话应被清理")
	}
}

// finish 必须幂等：重复调用不能把第一次的结束时刻覆盖掉，
// 否则会话会被无限续命，内存只增不减。
func TestLoginSessionFinishIsIdempotent(t *testing.T) {
	sess := &loginSession{Status: loginPending}
	sess.finish(loginError, "第一次")
	first := sess.EndedAt
	time.Sleep(time.Millisecond)
	sess.finish(loginOK, "")
	if !sess.EndedAt.Equal(first) {
		t.Fatalf("EndedAt 被重复刷新：%v → %v", first, sess.EndedAt)
	}
	if sess.Status != loginOK {
		t.Fatalf("状态应可更新，实际 %s", sess.Status)
	}
}

// remainingSeconds 是前端倒计时的唯一来源：不能为负，终态必须为 0。
func TestLoginSessionRemainingSeconds(t *testing.T) {
	now := time.Now()
	sess := &loginSession{ExpiresAt: now.Add(90 * time.Second)}
	if got := sess.remainingSeconds(now); got != 90 {
		t.Fatalf("剩余秒数应为 90，实际 %d", got)
	}
	if got := sess.remainingSeconds(now.Add(2 * time.Minute)); got != 0 {
		t.Fatalf("过期后应为 0 而不是负数，实际 %d", got)
	}
	sess.finish(loginOK, "")
	if got := sess.remainingSeconds(now); got != 0 {
		t.Fatalf("终态会话的剩余秒数必须为 0，实际 %d", got)
	}
}

// 进行中的会话如果一直没人轮询，也要在有效期后 30 分钟被清掉，
// 否则每发起一次登录就永久多一条内存记录。
func TestLoginStoreGCPendingSessions(t *testing.T) {
	store := newLoginStore()
	now := time.Now()
	store.put(&loginSession{ID: "p1", ExpiresAt: now.Add(time.Minute), Status: loginPending})
	store.gc(now.Add(20 * time.Minute))
	if store.get("p1") == nil {
		t.Fatal("仍在宽限期内的进行中会话不应被清理")
	}
	store.gc(now.Add(40 * time.Minute))
	if store.get("p1") != nil {
		t.Fatal("超过宽限期的进行中会话应被清理")
	}
}
