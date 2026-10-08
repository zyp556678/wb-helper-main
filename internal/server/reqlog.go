package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy-gateway/internal/debuglog"
	"workbuddy-gateway/internal/reqlog"
)

// 本文件把 internal/reqlog 接到 HTTP 层。
//
// ## 为什么挂在 logRequest 上而不是各分支各写一遍
//
// `logRequest` 已经在本项目**每一个**请求完成分支被调用（chat 流式 / chat 聚合 /
// responses 流式 / responses 聚合 / 早期失败），且参数里恰好有模型、账号、首字、
// 总耗时、token、是否失败。把它当作唯一挂载点，就不会出现「新加一条返回路径忘了记账」
// 的漏记 —— 那种漏记的表现是「成功率偏高」，而且没人会发现。
//
// ## 为什么中间件包 ResponseWriter 要保留 Flusher/Unwrap
//
// 我们的对话是 SSE 逐帧刷新的，包一层会把 http.Flusher 断言打断，
// 结果是整条流退化成「全部缓冲到最后一次性发出」——功能还能用，
// 但首字延迟和流式体验全没了，且不会有任何报错。

type requestTraceKey struct{}

// requestTrace 在一次请求内累积要写进 reqlog 的字段。
//
// 分两段填：中间件开请求时建好并放进 context；处理过程中由 logRequest 补齐
// 模型/账号/耗时等；中间件在请求结束时统一落账（含它自己观测到的状态码）。
type requestTrace struct {
	ID      string
	Started time.Time

	mu      sync.Mutex
	model   string
	account string
	mode    string
	ttfb    time.Duration
	tokens  int64
	// output 是**输出** token。速率必须用它做分子：用总量会把 prompt 也算进
	// "每秒生成多少"，长上下文请求会显得快得离谱。
	output   int64
	credit   float64
	hasCred  bool
	attempts int
	failed   bool
	noted    bool
}

// traceFrom 取当前请求的 reqlog 追踪对象；未启用时返回 nil。
func traceFrom(r *http.Request) *requestTrace {
	if r == nil {
		return nil
	}
	tr, _ := r.Context().Value(requestTraceKey{}).(*requestTrace)
	return tr
}

// noteStats 由 logRequest 调用，把这一次请求的统计记进追踪对象。
func (t *requestTrace) noteStats(model, account, mode string, ttft time.Duration, tokens, output int64, failed bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.model = model
	t.account = account
	t.mode = mode
	t.ttfb = ttft
	t.tokens = tokens
	t.output = output
	t.failed = failed
	t.noted = true
}

// event 把追踪对象折算成一条 reqlog 记录。
func (t *requestTrace) event(path string, status int) reqlog.Event {
	t.mu.Lock()
	defer t.mu.Unlock()

	e := reqlog.Event{
		Time:      t.Started,
		RequestID: t.ID,
		Path:      path,
		Status:    status,
		Model:     t.model,
		Account:   t.account,
		TTFBMs:    t.ttfb.Milliseconds(),
		Attempts:  t.attempts,
		Credit:    t.credit,
		HasCredit: t.hasCred,
	}
	d := time.Since(t.Started)
	e.DurationMs = d.Milliseconds()
	if e.DurationMs < 1 {
		e.DurationMs = 1
	}
	if t.tokens > 0 {
		// 上游只给总数时也记一条，别让 total 为空 —— 面板按它排序。
		e.TotalTokens = t.tokens
	}
	if t.output > 0 {
		e.TokensPerSec = tokensPerSecond(t.output, d, t.ttfb)
	}
	switch {
	case t.failed:
		e.Outcome = reqlog.OutcomeStreamError
		e.OK = false
	case status >= 200 && status < 300:
		e.Outcome = reqlog.OutcomeSuccess
		e.OK = true
	default:
		e.Outcome = reqlog.OutcomeHTTPError
		e.OK = false
	}
	return e
}

// responseObserver 捕获 handler 实际写出的 HTTP 状态码。
//
// 必须保留 Flusher 与 Unwrap：前者是 SSE 逐帧刷新的前提，
// 后者让 http.ResponseController 之类的调用能拿回原始 writer。
type responseObserver struct {
	http.ResponseWriter
	status int
}

func (o *responseObserver) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	return o.ResponseWriter.Write(p)
}

func (o *responseObserver) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *responseObserver) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// withRequestLog 给对话类端点挂上请求级观测。
//
// 只挂对话端点：面板 API 是内部调用、量大且无 token/账号语义，记进来只会淹没真正的数据。
func (s *Server) withRequestLog(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.reqLog == nil {
			next(w, r)
			return
		}
		tr := &requestTrace{ID: reqlog.NewRequestID(), Started: time.Now()}
		w.Header().Set("X-Request-Id", tr.ID)
		// 调试事件流：请求进入/返回两条（只在 debug.enabled 时真正落盘）。
		debuglog.Event(r, "info", "request_received", map[string]any{
			"stream": strings.Contains(r.Header.Get("Accept"), "text/event-stream"),
		})

		obs := &responseObserver{ResponseWriter: w}
		s.reqLog.Begin()
		next(obs, r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, tr)))

		status := obs.status
		if status == 0 {
			status = http.StatusOK
		}
		debuglog.Event(r, "info", "response_returned", map[string]any{
			"status_code": status,
			"elapsed_ms":  time.Since(tr.Started).Milliseconds(),
		})
		s.reqLog.Record(tr.event(r.URL.Path, status))
	}
}

// -----------------------------------------------------------------------------
// 面板端点
// -----------------------------------------------------------------------------

// handleRequestMetrics 返回进程内请求指标、最近 100 条与归档状态。
func (s *Server) handleRequestMetrics(w http.ResponseWriter, r *http.Request) {
	if s.reqLog == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("请求记录器不可用"))
		return
	}
	writeJSON(w, http.StatusOK, s.reqLog.Snapshot())
}

// handleRequestLogs 从 JSONL 归档读取最近请求；limit 默认 200、最大 1000。
func (s *Server) handleRequestLogs(w http.ResponseWriter, r *http.Request) {
	if s.reqLog == nil {
		writeJSON(w, http.StatusNotImplemented, errBody("请求记录器不可用"))
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.reqLog.ReadArchive(limit, reqlog.Filter{
		Outcome: r.URL.Query().Get("outcome"),
		Account: r.URL.Query().Get("account"),
		Model:   r.URL.Query().Get("model"),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit})
}
