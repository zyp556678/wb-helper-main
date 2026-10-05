// Package session 实现会话粘性：同一会话尽量复用同一账号，保证多轮对话不跳号。
//
// 会话键提取顺序（与 wb2api-panel 一致）：
//
//	metadata.conversation_id → metadata.conversationId → metadata.user_id
//	→ 顶层 conversation_id → 顶层 conversationId
//
// 客户端完全不提供时，用「system + 首条 user 内容」的哈希派生会话键（d- 前缀），
// 这样通用 OpenAI 客户端也能享受粘性。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Binding 是一次会话到账号的绑定。
type Binding struct {
	AccountID string
	ExpiresAt time.Time
}

// Store 是会话粘性表。
type Store struct {
	mu       sync.RWMutex
	bindings map[string]Binding

	enabled bool
	ttl     time.Duration
	gcEvery time.Duration
	lastGC  time.Time
}

// New 构造粘性表。
func New(enabled bool, ttl, gcEvery time.Duration) *Store {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if gcEvery <= 0 {
		gcEvery = 5 * time.Minute
	}
	return &Store{
		bindings: map[string]Binding{},
		enabled:  enabled,
		ttl:      ttl,
		gcEvery:  gcEvery,
		lastGC:   time.Now(),
	}
}

// Configure 热更新参数（面板保存配置时调用）。
func (s *Store) Configure(enabled bool, ttl, gcEvery time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = enabled
	if ttl > 0 {
		s.ttl = ttl
	}
	if gcEvery > 0 {
		s.gcEvery = gcEvery
	}
	if !enabled {
		s.bindings = map[string]Binding{}
	}
}

// Config 返回当前参数。
func (s *Store) Config() (enabled bool, ttl, gcEvery time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled, s.ttl, s.gcEvery
}

// Enabled 报告粘性是否启用。
func (s *Store) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// Lookup 查会话绑定；命中即滚动续期（TTL 从本次访问重新计算）。
// 返回空串表示未绑定或未启用。
func (s *Store) Lookup(key string) string {
	if key == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return ""
	}
	s.maybeGC()
	b, ok := s.bindings[key]
	if !ok {
		return ""
	}
	if time.Now().After(b.ExpiresAt) {
		delete(s.bindings, key)
		return ""
	}
	b.ExpiresAt = time.Now().Add(s.ttl) // 滚动续期
	s.bindings[key] = b
	return b.AccountID
}

// Bind 绑定会话到账号（成功后调用）。
func (s *Store) Bind(key, accountID string) {
	if key == "" || accountID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return
	}
	s.maybeGC()
	s.bindings[key] = Binding{AccountID: accountID, ExpiresAt: time.Now().Add(s.ttl)}
}

// Unbind 解绑（请求失败后调用，让下次重新分配）。
func (s *Store) Unbind(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bindings, key)
}

// Count 返回有效绑定数（面板展示用）。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, b := range s.bindings {
		if now.Before(b.ExpiresAt) {
			n++
		}
	}
	return n
}

// maybeGC 惰性清理过期绑定（调用方需持有写锁）。
func (s *Store) maybeGC() {
	now := time.Now()
	if now.Sub(s.lastGC) < s.gcEvery {
		return
	}
	s.lastGC = now
	for k, b := range s.bindings {
		if now.After(b.ExpiresAt) {
			delete(s.bindings, k)
		}
	}
}

// -----------------------------------------------------------------------------
// 会话键提取
// -----------------------------------------------------------------------------

// Key 从请求体里提取会话键。
func Key(req map[string]any) string {
	if k := keyFromRequest(req); k != "" {
		return k
	}
	return derivedKey(req)
}

// keyFromRequest 按约定顺序查找客户端显式提供的会话标识。
func keyFromRequest(req map[string]any) string {
	candidates := []struct {
		container string
		key       string
	}{
		{"metadata", "conversation_id"},
		{"metadata", "conversationId"},
		{"metadata", "user_id"},
		{"", "conversation_id"},
		{"", "conversationId"},
	}
	for _, c := range candidates {
		var src map[string]any
		if c.container == "" {
			src = req
		} else {
			m, ok := req[c.container].(map[string]any)
			if !ok {
				continue
			}
			src = m
		}
		if v, ok := src[c.key].(string); ok && strings.TrimSpace(v) != "" {
			return "c-" + strings.TrimSpace(v)
		}
	}
	return ""
}

// derivedKey 客户端没给会话标识时，用 system + 首条 user 内容派生。
//
// 注意：这会让「同一系统提示 + 同一句开场白」的不同会话被视为同一会话。
// 这是有意的取舍——通用 OpenAI 客户端没有任何会话标识，只能靠内容近似；
// 真要对齐语义，客户端应显式传 conversation_id。
func derivedKey(req map[string]any) string {
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	var sys, firstUser string
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system", "developer":
			if sys == "" {
				sys = flattenContent(msg["content"])
			}
		case "user":
			if firstUser == "" {
				firstUser = flattenContent(msg["content"])
			}
		}
		if sys != "" && firstUser != "" {
			break
		}
	}
	if sys == "" && firstUser == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sys + "\x1f" + firstUser))
	return "d-" + hex.EncodeToString(sum[:8])
}

// flattenContent 把 content 归一成字符串（兼容纯字符串与多模态数组两种形态）。
func flattenContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			if m, ok := part.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		if v == nil {
			return ""
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}
