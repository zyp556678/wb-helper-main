// Package metrics 采集网关卡口的运行指标：每模型请求数 / 失败数 / 累计输出 token /
// 首字耗时（TTFT）与总耗时的滚动窗口均值。
//
// 为什么要滚动窗口而不是全程累计：均值被历史样本拖住会失去诊断价值
// （启动两小时后仍显示一小时前的慢响应）。这里按小时分桶、只保留最近 5 小时，
// 超窗样本自动淘汰，与 wb-gateway 的 monitor 口径一致。
package metrics

import (
	"sort"
	"sync"
	"time"
)

// windowHours 是滚动窗口长度。
const windowHours = 5

// bucket 是一个小时内的样本聚合。
type bucket struct {
	hour  int64 // Unix 小时
	sumMs float64
	count int64
}

// modelStat 是单模型统计。
type modelStat struct {
	model    string
	requests int64
	success  int64
	failures int64
	tokens   int64
	lastAt   int64
	// lastStatus 是该模型**最近一次**请求的结果（成功 / 失败:<原因>）。
	//
	// 为什么单独记「最近一次」而不是只留计数：计数回答「一共坏了几次」，
	// 而模型卡片上真正要看的是「它现在还行不行」——一个两小时前失败过、
	// 之后一直正常的模型，和一个刚刚连续失败的模型，计数看起来可能一样。
	lastStatus string
	ttft       []bucket
	total      []bucket
}

// Registry 是指标注册表。
type Registry struct {
	mu     sync.Mutex
	models map[string]*modelStat
	start  time.Time
}

// New 构造注册表。
func New() *Registry {
	return &Registry{models: map[string]*modelStat{}, start: time.Now()}
}

// StartedAt 返回采集起始时刻。
func (r *Registry) StartedAt() time.Time { return r.start }

// ModelSnapshot 是给面板用的单模型指标。
type ModelSnapshot struct {
	Model         string   `json:"model"`
	Requests      int64    `json:"requests"`
	Success       int64    `json:"success"`
	Failures      int64    `json:"failures"`
	TokensTotal   int64    `json:"tokens_total"`
	AvgTTFTMs     *float64 `json:"avg_ttft_ms"`
	AvgTotalMs    *float64 `json:"avg_total_ms"`
	LastRequestAt int64    `json:"last_request_at"`
	// LastStatus 是最近一次请求的结果（成功 / 失败:<原因>）。
	LastStatus string `json:"last_status,omitempty"`
}

// stat 取（或创建）某模型的统计项。
func (r *Registry) stat(model string) *modelStat {
	s, ok := r.models[model]
	if !ok {
		s = &modelStat{model: model}
		r.models[model] = s
	}
	return s
}

// RecordRequest 记一次请求进入。
func (r *Registry) RecordRequest(model string) {
	if model == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stat(model)
	s.requests++
	s.lastAt = time.Now().Unix()
}

// RecordSuccess 记一次成功响应（进入最近状态；成功次数由 requests - failures 也能算，
// 但显式记一份让快照自洽，不必让每个读取方都去做减法）。
func (r *Registry) RecordSuccess(model string) {
	if model == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stat(model)
	s.success++
	s.lastStatus = "成功"
}

// RecordFailure 记一次请求失败；reason 为空时最近状态只显示「失败」。
func (r *Registry) RecordFailure(model, reason string) {
	if model == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stat(model)
	s.failures++
	if reason == "" {
		s.lastStatus = "失败"
	} else {
		s.lastStatus = "失败:" + reason
	}
}

// RecordTokens 记一次输出 token（取上游 usage）。
func (r *Registry) RecordTokens(model string, n int64) {
	if model == "" || n <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stat(model).tokens += n
}

// RecordTTFT 记一次首字耗时。
func (r *Registry) RecordTTFT(model string, d time.Duration) {
	if model == "" || d <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stat(model)
	s.ttft = appendSample(s.ttft, float64(d.Milliseconds()))
}

// RecordLatency 记一次总耗时。
func (r *Registry) RecordLatency(model string, d time.Duration) {
	if model == "" || d <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stat(model)
	s.total = appendSample(s.total, float64(d.Milliseconds()))
}

// appendSample 往滚动窗口里追加一个样本（按小时分桶，淘汰超窗桶）。
func appendSample(bs []bucket, ms float64) []bucket {
	hour := time.Now().Unix() / 3600
	// 淘汰超窗
	kept := bs[:0]
	for _, b := range bs {
		if hour-b.hour < windowHours {
			kept = append(kept, b)
		}
	}
	bs = kept
	if n := len(bs); n > 0 && bs[n-1].hour == hour {
		bs[n-1].sumMs += ms
		bs[n-1].count++
		return bs
	}
	return append(bs, bucket{hour: hour, sumMs: ms, count: 1})
}

// avg 计算滚动窗口内的均值；无样本返回 nil（面板显示 "-"）。
func avg(bs []bucket) *float64 {
	hour := time.Now().Unix() / 3600
	var sum float64
	var count int64
	for _, b := range bs {
		if hour-b.hour >= windowHours {
			continue
		}
		sum += b.sumMs
		count += b.count
	}
	if count == 0 {
		return nil
	}
	v := sum / float64(count)
	return &v
}

// Snapshot 返回全部模型的指标（按请求数降序，其次按模型名）。
func (r *Registry) Snapshot() []ModelSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ModelSnapshot, 0, len(r.models))
	for _, s := range r.models {
		out = append(out, ModelSnapshot{
			Model:         s.model,
			Requests:      s.requests,
			Success:       s.success,
			Failures:      s.failures,
			TokensTotal:   s.tokens,
			AvgTTFTMs:     avg(s.ttft),
			AvgTotalMs:    avg(s.total),
			LastRequestAt: s.lastAt,
			LastStatus:    s.lastStatus,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// RequestsFor 返回某模型的累计请求数（供价格探测判断「是否被实际请求过」）。
func (r *Registry) RequestsFor(model string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.models[model]; ok {
		return s.requests
	}
	return 0
}

// Totals 返回全局累计（请求数与失败数）。
type Totals struct {
	Requests int64 `json:"requests"`
	Failures int64 `json:"failures"`
	Tokens   int64 `json:"tokens"`
}

// Totals 汇总全局计数。
func (r *Registry) Totals() Totals {
	r.mu.Lock()
	defer r.mu.Unlock()
	var t Totals
	for _, s := range r.models {
		t.Requests += s.requests
		t.Failures += s.failures
		t.Tokens += s.tokens
	}
	return t
}
