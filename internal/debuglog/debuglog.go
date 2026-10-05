// Package debuglog 是 `debug.enabled` 打开后的**逐请求事件流**（JSONL）。
//
// 它在补什么：`internal/reqlog` 记的是「一次请求的结局」（状态码、耗时、token），
// `internal/eventlog` 记的是「系统里发生了什么」（签到、刷新、开关）。两者都回答不了
// 「这次请求**为什么**走了这条路」—— 被哪个策略挡下、重试了几次、注入了什么提示词。
// 这个包就是那条链路的事件流，按行 JSON 落盘，一眼可 grep。
//
// 三条硬约束（与 reqlog 同口径）：
//
//  1. **不落敏感内容**：提示词、响应正文、Authorization 原文一律不写；
//     密钥只写 sha256 前缀（够区分「换没换 key」，不足以复原）。
//  2. **不阻塞请求**：写盘在锁内直接 Write（追加小行），失败只静默丢弃 ——
//     调试日志绝不该把模型请求拖慢或拖挂。
//  3. **默认关闭**：只有 `debug.enabled=true` 时才建 sink；未开启时所有调用都是空操作
//     （Enabled() 为假时直接 return，连 map 都不构造）。
package debuglog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Sink 是当前生效的事件流（nil 表示未开启）。
var (
	mu   sync.RWMutex
	sink *fileSink
	// instanceID 在启用时生成一次，用于区分同一台机器上的多次运行。
	instanceID string
	// version 由 main 注入（面板版本号），便于把日志与发布版本对上。
	version = "dev"
)

type fileSink struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	opened time.Time
}

// SetVersion 注入版本号（main 在启动时调用一次）。
func SetVersion(v string) {
	if strings.TrimSpace(v) != "" {
		version = v
	}
}

// Enable 打开事件流：在 dir 下建 `debug-YYYY-MM-DD.jsonl`，返回关闭函数。
//
// 同一天重复调用会复用同一个文件（追加），因为调试日志跨重启连续更有用。
func Enable(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "debug-"+time.Now().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	s := &fileSink{file: f, path: path, opened: time.Now()}
	mu.Lock()
	old := sink
	sink = s
	instanceID = newInstanceID()
	mu.Unlock()
	if old != nil {
		old.close()
	}
	return func() {
		mu.Lock()
		if sink == s {
			sink = nil
		}
		mu.Unlock()
		s.close()
	}, nil
}

// Enabled 报告事件流是否打开（未打开时调用方不必构造 fields）。
func Enabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return sink != nil
}

// Path 返回当前日志文件路径（未开启为空；面板展示用）。
func Path() string {
	mu.RLock()
	defer mu.RUnlock()
	if sink == nil {
		return ""
	}
	return sink.path
}

func (s *fileSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

// Event 写一条事件；r 可以为 nil（后台任务没有请求上下文）。
//
// level 取 debug/info/warn/error，与参考实现同口径，便于按级别 grep。
func Event(r *http.Request, level, event string, fields map[string]any) {
	if !Enabled() {
		return
	}
	mu.RLock()
	s := sink
	inst := instanceID
	mu.RUnlock()
	if s == nil {
		return
	}
	record := map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"level":     level,
		"event":     event,
		"service":   "workbuddy-gateway",
		"instance":  inst,
		"version":   version,
	}
	if r != nil {
		record["route"] = r.URL.Path
		record["method"] = r.Method
		record["request_id"] = strings.TrimSpace(r.Header.Get("X-Request-Id"))
		record["client_ip"] = clientIP(r)
		record["remote_addr"] = r.RemoteAddr
		record["user_agent"] = r.UserAgent()
		record["host"] = r.Host
		record["content_type"] = r.Header.Get("Content-Type")
		record["content_length"] = r.ContentLength
		record["protocol"] = r.Proto
		record["authorization_present"] = strings.TrimSpace(r.Header.Get("Authorization")) != "" ||
			strings.TrimSpace(r.Header.Get("x-api-key")) != ""
		record["api_key_fingerprint"] = fingerprint(firstNonEmpty(
			r.Header.Get("Authorization"), r.Header.Get("x-api-key")))
	}
	for k, v := range fields {
		record[k] = v
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	s.mu.Lock()
	if s.file != nil {
		_, _ = s.file.Write(append(data, '\n'))
	}
	s.mu.Unlock()
}

// BodyFingerprint 返回请求体的「大小 + sha256 前缀」——够判断两次请求是不是同一份内容，
// 又不足以还原提示词。
func BodyFingerprint(body []byte) map[string]any {
	sum := sha256.Sum256(body)
	return map[string]any{
		"body_bytes":     len(body),
		"body_sha256_8":  hex.EncodeToString(sum[:])[:8],
		"body_truncated": false,
	}
}

// fingerprint 是密钥的不可逆短标识（空值返回空串）。
func fingerprint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.TrimPrefix(value, "Bearer ")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}

// clientIP 取客户端地址（优先 X-Forwarded-For 的第一段）。
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// newInstanceID 生成实例标识（进程内随机，不暴露任何用户信息）。
func newInstanceID() string {
	sum := sha256.Sum256([]byte(time.Now().UTC().Format(time.RFC3339Nano) + os.Args[0]))
	return hex.EncodeToString(sum[:])[:8]
}
