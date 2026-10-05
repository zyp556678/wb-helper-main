package accountmeta

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func strPtr(v string) *string { return &v }

func TestNoteLimitCountsRunes(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "wb-account-meta.json"))

	// 24 个中文字符 == 24 字符，必须通过（按字节数会误判成 72 > 24）。
	ok := strings.Repeat("备", MaxNoteRunes)
	if _, err := store.Update("a.json", strPtr(ok), nil); err != nil {
		t.Fatalf("24 个中文字符应允许: %v", err)
	}
	if got := store.Get("a.json").Note; got != ok {
		t.Fatalf("备注应原样保存，实际 %q", got)
	}

	// 25 个字符超限 → ErrNoteTooLong，且不改动已有值。
	if _, err := store.Update("a.json", strPtr(ok+"注"), nil); !errors.Is(err, ErrNoteTooLong) {
		t.Fatalf("超限应返回 ErrNoteTooLong，实际 %v", err)
	}
	if got := store.Get("a.json").Note; got != ok {
		t.Fatalf("校验失败不应改动已存值，实际 %q", got)
	}
}

func TestUpdateTrimsAndClears(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "wb-account-meta.json"))

	if _, err := store.Update("a.json", strPtr("  工作号  "), strPtr(DisplayNote)); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	entry := store.Get("a.json")
	if entry.Note != "工作号" || entry.DisplayField != DisplayNote {
		t.Fatalf("备注应去首尾空白、显示字段应保存，实际 %+v", entry)
	}

	// 清空备注但保留显示字段 → 条目仍在，只有备注没了。
	if _, err := store.Update("a.json", strPtr(""), nil); err != nil {
		t.Fatalf("清空备注失败: %v", err)
	}
	entry = store.Get("a.json")
	if entry.Note != "" || entry.DisplayField != DisplayNote {
		t.Fatalf("清空备注后应保留显示字段，实际 %+v", entry)
	}

	// 显示字段也清空 → 空条目整体删除。
	if _, err := store.Update("a.json", nil, strPtr("")); err != nil {
		t.Fatalf("清空显示字段失败: %v", err)
	}
	if _, ok := store.All()["a.json"]; ok {
		t.Fatalf("空条目应被删除，实际 %+v", store.All())
	}
}

func TestDisplayFieldValidation(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "wb-account-meta.json"))

	for _, valid := range []string{DisplayNickname, DisplayPhone, DisplayNote, ""} {
		if _, err := store.Update("a.json", nil, strPtr(valid)); err != nil {
			t.Fatalf("显示字段 %q 应合法: %v", valid, err)
		}
	}
	for _, invalid := range []string{"email", "NOTE", "uid"} {
		if _, err := store.Update("a.json", nil, strPtr(invalid)); !errors.Is(err, ErrInvalidDisplayField) {
			t.Fatalf("显示字段 %q 应被拒绝，实际 %v", invalid, err)
		}
	}

	// 首尾空白是手写/客户端拼接入参的常见噪声：去掉后归一，不作为非法值。
	entry, err := store.Update("a.json", nil, strPtr(" note "))
	if err != nil || entry.DisplayField != DisplayNote {
		t.Fatalf("显示字段应去掉首尾空白后归一为 note，实际 %+v / %v", entry, err)
	}
}

func TestMissingAccountFallsBackToZeroEntry(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "wb-account-meta.json"))
	entry := store.Get("unknown.json")
	if entry.Note != "" || entry.DisplayField != "" {
		t.Fatalf("未知账号应回退零值，实际 %+v", entry)
	}
	if len(store.All()) != 0 {
		t.Fatalf("空存储的 All() 应为空，实际 %+v", store.All())
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wb-account-meta.json")
	first := New(path)
	if _, err := first.Update("a.json", strPtr("甲"), strPtr(DisplayNickname)); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if _, err := first.Update("b.json", strPtr("乙"), nil); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	// 重新打开（模拟进程重启）应读回同一份数据。
	second := New(path)
	if got := second.Get("a.json"); got.Note != "甲" || got.DisplayField != DisplayNickname {
		t.Fatalf("重启后应读回 a 的记录，实际 %+v", got)
	}
	if got := second.Get("b.json"); got.Note != "乙" || got.DisplayField != "" {
		t.Fatalf("重启后应读回 b 的记录，实际 %+v", got)
	}
}

// 元数据文件损坏时按空处理，不阻塞账号页（与 creditwatch 同口径）。
func TestCorruptFileFallsBackToEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wb-account-meta.json")
	if err := os.WriteFile(path, []byte("{ 不是 json"), 0o600); err != nil {
		t.Fatalf("准备损坏文件失败: %v", err)
	}
	store := New(path)
	if entry := store.Get("a.json"); entry.Note != "" {
		t.Fatalf("损坏文件应回退空值，实际 %+v", entry)
	}
	if _, err := store.Update("a.json", strPtr("恢复"), nil); err != nil {
		t.Fatalf("损坏后仍应能写入: %v", err)
	}
}

// 并发更新不应 panic / 丢写（配合 -race 可验证锁的正确性）。
func TestConcurrentUpdates(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "wb-account-meta.json"))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "acc.json"
			if _, err := store.Update(id, strPtr(strings.Repeat("注", n%MaxNoteRunes+1)), strPtr(DisplayNote)); err != nil {
				t.Errorf("并发更新失败: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if store.Get("acc.json").Note == "" {
		t.Fatal("并发更新后应有备注")
	}
}

// nil 接收者安全：未接线（或单测里不关心存储）时调用不应 panic。
func TestNilStoreSafe(t *testing.T) {
	var store *Store
	if entry := store.Get("a.json"); entry != (Entry{}) {
		t.Fatalf("nil store 应返回零值，实际 %+v", entry)
	}
	if len(store.All()) != 0 {
		t.Fatal("nil store 的 All() 应为空")
	}
	if _, err := store.Update("a.json", strPtr("x"), nil); err != nil {
		t.Fatalf("nil store 的 Update 应成功但无副作用: %v", err)
	}
}
