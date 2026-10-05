package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy-gateway/internal/accountmeta"
	"workbuddy-gateway/internal/tokenstats"
)

// handleTokenStats 返回 Token 统计（GET /panel/api/token-stats?days=30）。
//
// 两类来源一起返给前端，构成页面的「数据源」切换：
//
//   - **本地会话日志**（tokenstats）：客户端自己写的会话记录，覆盖**所有**客户端流量
//     —— 包括不经网关的直连。有缓存读写、思考 token、项目与会话维度。
//   - **网关反代**（stats）：网关自己的请求级计数。只有请求数与输入/输出 token。
//
// 两类来源的能力边界不同，前端必须按 Kind 决定哪些区块可展示 ——
// 把网关侧缺失的字段显示成 0，会被读成「测到就是 0」。
//
// 扫描是同步 IO（本机日志可达 100MB 量级，实测一次约 0.75 秒），
// HTTP 处理本身已在独立 goroutine 里，这里不再另起；重复请求由
// tokenstats 内部的 45 秒 TTL 缓存兜住。
func (s *Server) handleTokenStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}

	days := parseTokenStatsDays(r.URL.Query().Get("days"))

	local := tokenstats.Collect(days)

	// 网关来源排在最前：它是本项目的自有能力，用户也最常先看它。
	sources := make([]tokenstats.SourceJSON, 0, len(local.Sources)+1)
	sources = append(sources, tokenstats.BuildGatewaySource(s.gatewayTokenInput(days)))
	sources = append(sources, local.Sources...)

	writeJSON(w, http.StatusOK, tokenstats.Statistics{
		GeneratedAt: time.Now().UnixMilli(),
		RangeDays:   days,
		Sources:     sources,
	})
}

// parseTokenStatsDays 解析窗口天数。
//
// 与 parseDays 分开：这里的取值是 tokenstats 扫描的窗口，语义是「最近 N 天」，
// 不接受小时粒度。非法值回落到 30 天而不是报错 —— 页面永远要能渲染。
func parseTokenStatsDays(raw string) int {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "30d":
		return 30
	case "today", "1d":
		return 1
	case "7d":
		return 7
	case "90d":
		return 90
	case "365d", "1y":
		return 365
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), "d")); err == nil && n > 0 {
		if n > 365 {
			n = 365
		}
		return n
	}
	return 30
}

// gatewayTokenInput 把网关自己的按天统计映射成 tokenstats 需要的形状。
func (s *Server) gatewayTokenInput(days int) tokenstats.GatewayInput {
	snapshot := s.stats.QueryDaily(days)

	in := tokenstats.GatewayInput{
		Requests:     snapshot.Totals.Requests,
		Failures:     snapshot.Totals.Failures,
		InputTokens:  snapshot.Totals.InputTokens,
		OutputTokens: snapshot.Totals.OutputTokens,
		Tokens:       snapshot.Totals.Tokens,
	}

	for _, m := range snapshot.TopModels {
		in.Models = append(in.Models, tokenstats.GatewayGroup{
			Key: m.Model, Requests: m.Requests, Tokens: m.Tokens,
		})
	}
	for _, a := range snapshot.Accounts {
		in.Accounts = append(in.Accounts, tokenstats.GatewayGroup{
			Key: a.Account, Title: s.accountDisplayName(a.Account),
			Requests: a.Requests, Tokens: a.Tokens,
			Input: a.InputTokens, Output: a.OutputTokens,
		})
	}
	for _, d := range snapshot.Series {
		// 天桶的 ts 是「本地日索引 × 86400」，用 UTC 格式化取回那个日历日
		// （与 stats.dayKey 同一套口径）。
		in.Daily = append(in.Daily, tokenstats.GatewayDay{
			Date:     time.Unix(d.TS, 0).UTC().Format("2006-01-02"),
			Requests: d.Requests,
			Input:    d.InputTokens,
			Output:   d.OutputTokens,
			Tokens:   d.Tokens,
		})
	}

	if snapshot.SinceAt > 0 {
		in.CoverageStartAt = snapshot.SinceAt
	}
	if len(snapshot.Series) > 0 {
		in.CoverageEndAt = snapshot.GeneratedAt
	}
	return in
}

// accountDisplayName 是「这个账号在面板上叫什么」的**服务端口径**。
//
// 必须与前端 accountDisplayName（web/src/lib/format.ts）逐字对齐，
// 否则同一个账号在账号页与用量统计页会叫两个名字：
//
//	display_field = note 且备注非空 → 备注
//	否则                            → 昵称 → uid → 凭据文件名
//
// （phone 没有数据源，前端会回退昵称，这里同样不处理。）
//
// 账号不在池里（凭据被删、但统计里还留着历史计数）时回退文件名 ——
// 用量统计是历史账本，不能因为账号被删就把这一行变成空白。
func (s *Server) accountDisplayName(id string) string {
	if entry := s.accountMeta.Get(id); entry.DisplayField == accountmeta.DisplayNote {
		if note := strings.TrimSpace(entry.Note); note != "" {
			return note
		}
	}
	if s.pool != nil {
		if acc, err := s.pool.Find(id); err == nil {
			if name := strings.TrimSpace(acc.Cred.Nickname); name != "" {
				return name
			}
			if uid := strings.TrimSpace(acc.Cred.UID); uid != "" {
				return uid
			}
		}
	}
	return id
}
