package server

import (
	"net/http"
	"strconv"
	"strings"

	"workbuddy-gateway/internal/eventlog"
)

// handlePanelLogs 返回事件日志（新的在前），支持频道 / 级别 / 关键词筛选。
func (s *Server) handlePanelLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	res := s.events.Query(eventlog.Query{
		Channel: strings.TrimSpace(q.Get("channel")),
		Level:   strings.TrimSpace(q.Get("level")),
		Keyword: strings.TrimSpace(q.Get("q")),
		Limit:   limit,
	})
	writeJSON(w, http.StatusOK, res)
}

// handleLogsClear 清空事件日志。
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	n := s.events.Clear()
	s.logf("[日志] 面板清空了事件日志（%d 条）", n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cleared": n})
}
