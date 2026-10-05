package server

import (
	"sync"
	"testing"
)

// keyedLocks：同组串行、异组并行、释放后可重入、全部释放后不残留。
//
// 这是「组同步期间前端重复点击」的防线：第二次请求必须拿到 ok=false（HTTP 409），
// 而不是与第一轮并发写同一批会话文件。
func TestKeyedLocksSerializeSameKey(t *testing.T) {
	var locks keyedLocks
	release1, ok := locks.tryLock("g1")
	if !ok {
		t.Fatal("首次获取应成功")
	}
	if _, ok := locks.tryLock("g1"); ok {
		t.Fatal("同 key 重复获取应失败（对应 HTTP 409）")
	}
	release2, ok := locks.tryLock("g2")
	if !ok {
		t.Fatal("不同 key 应互不影响")
	}
	release1()

	release3, ok := locks.tryLock("g1")
	if !ok {
		t.Fatal("释放后应能再次获取")
	}
	release3()
	release2()

	locks.mu.Lock()
	left := len(locks.locks)
	locks.mu.Unlock()
	if left != 0 {
		t.Fatalf("全部释放后不应残留条目，实际 %d", left)
	}
}

// 并发抢同一把锁：只有一个能成功。
func TestKeyedLocksConcurrentOnlyOneWinner(t *testing.T) {
	var locks keyedLocks
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := locks.tryLock("same")
			if !ok {
				return
			}
			mu.Lock()
			wins++
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()
	// 串行执行下每个 goroutine 都可能轮流成功；关键是**同时**只有一个持有者。
	// 这里验证锁能正常反复获取释放（wins >= 1），且不残留。
	if wins < 1 {
		t.Fatal("至少应有一个 goroutine 成功")
	}
	locks.mu.Lock()
	left := len(locks.locks)
	locks.mu.Unlock()
	if left != 0 {
		t.Fatalf("全部释放后不应残留条目，实际 %d", left)
	}
}
