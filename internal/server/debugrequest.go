package server

import (
	"bytes"
	"net/http"

	"workbuddy-gateway/internal/debuglog"
)

// 本文件是「完整请求诊断」的服务端接线（对照 wb-gateway v1.14.21 的 debugrequest.go）。
//
// 记录点只有三个，刻意保持少而准：
//
//	incoming        请求刚读进内存（客户端实际发来的头 + 正文）
//	outbound        出站改写之后（真正发给上游的正文，附处理前后摘要）
//	upstream_error  上游 4xx/5xx 的错误正文（这是「为什么被拒」的唯一线索）
//
// 响应正文不记：成功响应是 SSE 流，逐帧复制既昂贵又会把答案内容写进日志；
// 而排障真正缺的从来是「我到底发出去什么」，不是「上游答了什么」。
// 记录入口还要求 debug.enabled 与 debug.full_requests 同时打开，见 FullRequestEnabled。

// fullRequestIncoming 记录入站请求头与正文。
func (s *Server) fullRequestIncoming(r *http.Request, body []byte) {
	if !debuglog.FullRequestEnabled(r) {
		return
	}
	fields := map[string]any{
		"stage":       "incoming",
		"trace_id":    traceOf(r),
		"headers":     debuglog.FullHeaderFields(r.Header),
		"body_digest": debuglog.DigestBytes(body),
	}
	for key, value := range debuglog.FullBodyFields(body) {
		fields[key] = value
	}
	debuglog.FullRequest(r, "full_request_incoming", fields)
}

// fullRequestOutbound 记录出站改写结果，并附处理前后的摘要用于比对。
//
// degrade 标记本次是「内容拦截后的降级重试」：同一个请求会写两条，
// 靠这个字段区分第一次与重试，否则看起来像重复记录。
func (s *Server) fullRequestOutbound(r *http.Request, modelName string, degrade bool, before, after []byte) {
	if !debuglog.FullRequestEnabled(r) {
		return
	}
	fields := map[string]any{
		"stage":            "outbound",
		"trace_id":         traceOf(r),
		"model":            modelName,
		"degraded_attempt": degrade,
		"incoming_digest":  debuglog.DigestBytes(before),
		"outbound_digest":  debuglog.DigestBytes(after),
		"body_changed":     !bytes.Equal(before, after),
	}
	for key, value := range debuglog.FullBodyFields(after) {
		fields[key] = value
	}
	debuglog.FullRequest(r, "full_request_outbound", fields)
}

// fullRequestUpstreamError 记录上游拒绝时返回的错误正文。
func (s *Server) fullRequestUpstreamError(r *http.Request, modelName string, status int, body []byte) {
	if !debuglog.FullRequestEnabled(r) {
		return
	}
	fields := map[string]any{
		"stage":       "upstream_error",
		"trace_id":    traceOf(r),
		"model":       modelName,
		"status_code": status,
	}
	for key, value := range debuglog.FullBodyFields(body) {
		fields[key] = value
	}
	debuglog.FullRequest(r, "full_request_upstream_error", fields)
}
