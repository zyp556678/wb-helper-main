package config

import (
	"crypto/sha256"
	"sync"
)

// Watch 是 config.json 的**内容摘要游标**：记住哪份内容已经被应用过。
//
// 为什么按内容摘要而不是 mtime / 文件大小：编辑器保存、容器里 cp、rsync 之后
// mtime 可能被刻意保留，「同一秒内写两次」也会让 mtime 的粒度不够用；而摘要按内容算，
// 改一个字节就会变。config.json 只有几 KB，每次读一遍再算摘要的开销可忽略 ——
// 这也是不做 inotify（需要自己处理重命名、临时文件、队列溢出与跨平台差异）的理由。
//
// 面板保存与文件监听**共用同一个游标**：面板保存后把刚写进磁盘的内容标记为「已应用」，
// 监听循环就不会把自己刚写的东西当成外部改动再重放一遍（重放无害，但会让
// 「这份配置是谁改的」这条排障线索变模糊，还会多一条日志）。
type Watch struct {
	mu         sync.Mutex
	applied    [32]byte
	hasApplied bool
	failed     [32]byte
	hasFailed  bool
}

// NewWatch 构造一个空游标（还没记录过任何内容）。
func NewWatch() *Watch { return &Watch{} }

// MarkApplied 记录一份已经生效的内容。
func (w *Watch) MarkApplied(data []byte) {
	if w == nil {
		return
	}
	sum := sha256.Sum256(data)
	w.mu.Lock()
	w.applied, w.hasApplied, w.hasFailed = sum, true, false
	w.mu.Unlock()
}

// Changed 报告 data 与已应用的内容是否不同；从未记录过内容时返回 true。
func (w *Watch) Changed(data []byte) bool {
	if w == nil {
		return false
	}
	sum := sha256.Sum256(data)
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.hasApplied || w.applied != sum
}

// MarkFailed 记下一份**被拒绝**的内容（校验没通过），并报告是不是第一次见到它。
//
// 用途是抑制告警刷屏：同一份坏配置会被监听循环每秒重试一次，
// 但不能每秒打一行日志 —— 用户真正需要看到的是「这份内容有问题」这一条。
// 内容再改一次（哪怕还是坏的）会被当成新的一份，重新告警。
func (w *Watch) MarkFailed(data []byte) (first bool) {
	if w == nil {
		return false
	}
	sum := sha256.Sum256(data)
	w.mu.Lock()
	defer w.mu.Unlock()
	first = !w.hasFailed || w.failed != sum
	w.failed, w.hasFailed = sum, true
	return first
}
