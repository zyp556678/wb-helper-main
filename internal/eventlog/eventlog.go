// Package eventlog 是一个有界的事件环形缓冲，用于面板的「日志」页。
//
// 设计取舍：只保留最近 N 条（默认 500）且全部在内存里，**不落盘**。
// 理由是这个页面的用途是「看刚才发生了什么」（出站改写了什么、拦截重试了几次、
// 哪个账号进了冷却），属于排障现场信息；落盘会带来轮转、清理、权限一堆问题，
// 而且长期日志另有 stdout 归集（Docker/journald/systemd）接管，不该由面板重复承担。
//
// 并发：写入来自多个请求协程与后台任务协程，用一把互斥锁串行化。
// 事件量级是「每请求几条」，锁竞争可以忽略；换成无锁环会把代码复杂度抬得很高。
package eventlog

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Level 是事件级别。
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Channel 是事件频道（面板上按此分组筛选）。
const (
	ChannelRequest    = "request"    // 请求生命周期
	ChannelOutbound   = "outbound"   // 出站改写、拦截重试
	ChannelGovernance = "governance" // 账号冷却 / 熔断 / 禁用
	ChannelTask       = "task"       // 定时任务
	ChannelSystem     = "system"     // 启动 / 配置 / 目录
)

// Event 是一条事件记录。
type Event struct {
	ID      uint64         `json:"id"`
	TS      int64          `json:"ts"`
	Level   Level          `json:"level"`
	Channel string         `json:"channel"`
	Model   string         `json:"model,omitempty"`
	Account string         `json:"account,omitempty"`
	Event   string         `json:"event"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// Query 是事件检索条件。
type Query struct {
	Channel string
	Level   string
	Keyword string
	Limit   int
}

// Log 是环形缓冲。
type Log struct {
	mu       sync.Mutex
	buf      []Event
	capacity int
	nextID   uint64
}

// New 构造日志缓冲。capacity <= 0 时取默认 500。
func New(capacity int) *Log {
	if capacity <= 0 {
		capacity = 500
	}
	return &Log{buf: make([]Event, 0, capacity), capacity: capacity}
}

// Capacity 返回容量（面板展示用）。
func (l *Log) Capacity() int { return l.capacity }

// Add 写入一条事件。Fields 为 nil 时自动补空 map，保证 JSON 结构稳定。
func (l *Log) Add(level Level, channel, event, message string, fields map[string]any) {
	if l == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	l.buf = append(l.buf, Event{
		ID:      l.nextID,
		TS:      time.Now().Unix(),
		Level:   level,
		Channel: channel,
		Model:   str(fields["model"]),
		Account: str(fields["account"]),
		Event:   event,
		Message: message,
		Fields:  fields,
	})
	if len(l.buf) > l.capacity {
		// 一次性搬移，摊还成本 O(1)
		drop := len(l.buf) - l.capacity
		l.buf = append(l.buf[:0], l.buf[drop:]...)
	}
}

// Info / Warn / Error 是 Add 的便捷包装。
func (l *Log) Info(channel, event, message string, fields map[string]any) {
	l.Add(LevelInfo, channel, event, message, fields)
}
func (l *Log) Warn(channel, event, message string, fields map[string]any) {
	l.Add(LevelWarn, channel, event, message, fields)
}
func (l *Log) Error(channel, event, message string, fields map[string]any) {
	l.Add(LevelError, channel, event, message, fields)
}

// Result 是检索结果。
type Result struct {
	Events   []Event        `json:"events"`
	Channels []ChannelStat  `json:"channels"`
	Levels   map[string]int `json:"levels"`
	Capacity int            `json:"capacity"`
	Total    int            `json:"total"`
}

// ChannelStat 是某频道的事件计数。
type ChannelStat struct {
	Channel string `json:"channel"`
	Count   int    `json:"count"`
}

// Query 按条件检索（结果按时间倒序，新的在前）。
//
// 语义说明：Channel / Level 计数是**全量**（未按筛选截断）的，
// 因为它们的用途是「告诉用户当前缓冲里各频道有多少条」，若按筛选计算会自我指涉、
// 导致用户看到计数为 0 却无法理解为何还有数据。limit <= 0 取默认 200，上限 500。
func (l *Log) Query(q Query) Result {
	l.mu.Lock()
	defer l.mu.Unlock()

	res := Result{
		Capacity: l.capacity,
		Total:    len(l.buf),
		Levels:   map[string]int{},
	}
	counts := map[string]int{}
	for _, ev := range l.buf {
		counts[ev.Channel]++
		res.Levels[string(ev.Level)]++
	}
	for _, ch := range []string{ChannelRequest, ChannelOutbound, ChannelGovernance, ChannelTask, ChannelSystem} {
		res.Channels = append(res.Channels, ChannelStat{Channel: ch, Count: counts[ch]})
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	kw := strings.ToLower(strings.TrimSpace(q.Keyword))
	level := strings.ToLower(strings.TrimSpace(q.Level))
	channel := strings.ToLower(strings.TrimSpace(q.Channel))

	out := make([]Event, 0, limit)
	for i := len(l.buf) - 1; i >= 0 && len(out) < limit; i-- {
		ev := l.buf[i]
		if channel != "" && ev.Channel != channel {
			continue
		}
		if level != "" && string(ev.Level) != level {
			continue
		}
		if kw != "" && !matches(ev, kw) {
			continue
		}
		out = append(out, ev)
	}
	res.Events = out
	return res
}

// matches 做关键词匹配。除了四个常规字段，也扫 fields 里的值——
// 请求级 trace（trace_id）就在 fields 里，按 trace 查是日志页最常用的用法之一。
func matches(ev Event, kw string) bool {
	if strings.Contains(strings.ToLower(ev.Message), kw) ||
		strings.Contains(strings.ToLower(ev.Model), kw) ||
		strings.Contains(strings.ToLower(ev.Account), kw) ||
		strings.Contains(strings.ToLower(ev.Event), kw) {
		return true
	}
	for _, v := range ev.Fields {
		switch val := v.(type) {
		case string:
			if strings.Contains(strings.ToLower(val), kw) {
				return true
			}
		case float64, bool:
			if strings.Contains(strings.ToLower(fmt.Sprint(val)), kw) {
				return true
			}
		}
	}
	return false
}

// Clear 清空缓冲，返回被清掉的条数。
func (l *Log) Clear() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.buf)
	l.buf = l.buf[:0]
	return n
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
