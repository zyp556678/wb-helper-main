package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"workbuddy-gateway/internal/creditwatch"
	"workbuddy-gateway/internal/reqlog"
)

// 本文件是「配置」页的只读后台查询接口（批次 5）：
//
//	GET /panel/api/checkin/logs     签到日志（读本地台账，不打上游）
//	GET /panel/api/logs/paths       日志落点（请求归档目录 / 数据目录的真实路径）
//	GET /panel/api/logs/download    下载最近一个（或指定）请求日志归档
//
// 三者的共同点是**只读**：不写盘、不触发任何上游请求 —— 面板读它们只为排障。
// 本项目是 Web 面板，「打开系统文件管理器」在浏览器场景不成立（仅 Tauri 壳内可行），
// 因此这里只提供路径展示 / 复制 / 下载，不假装能打开资源管理器。

// handleCheckinLogs 返回最近 days 天的签到记录（GET /panel/api/checkin/logs）。
//
// days 默认 30、最大 30（更早的记录在写入台账时已被裁剪，这里如实按上限处理）；
// 非法值/非正数按默认处理。记录按时间倒序（最新在前）。
func (s *Server) handleCheckinLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	days := creditwatch.CheckinKeepDays
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > creditwatch.CheckinKeepDays {
		days = creditwatch.CheckinKeepDays
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logs":           s.creditWatch.CheckinLogs(days, time.Now()),
		"days":           days,
		"retention_days": creditwatch.CheckinKeepDays,
		"max_records":    creditwatch.CheckinLogCap,
	})
}

// requestLogDir 是请求 JSONL 归档的落点（与 main 装配 reqlog 时的约定一致）。
func (s *Server) requestLogDir() string {
	return filepath.Join(s.config().WorkDir, "request-logs")
}

// isRequestLogName 判断文件名是否符合请求归档的命名约定
// （复用 reqlog.ArchiveFileGlob，避免第三处再抄一遍模式）。
func isRequestLogName(name string) bool {
	ok, _ := filepath.Match(reqlog.ArchiveFileGlob, name)
	return ok
}

// latestRequestLogFile 返回目录里最近写入的归档文件名；没有归档时返回空串。
//
// 按修改时间取最新而不是按文件名排序：同一天滚动时会出现
// `requests-2026-10-04-1.jsonl`，字典序并不等于时间序。
func latestRequestLogFile(dir string) string {
	matches, err := filepath.Glob(filepath.Join(dir, reqlog.ArchiveFileGlob))
	if err != nil || len(matches) == 0 {
		return ""
	}
	latest, latestMod := "", time.Time{}
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		name := filepath.Base(path)
		if latest == "" || info.ModTime().After(latestMod) ||
			(info.ModTime().Equal(latestMod) && name > latest) {
			latest, latestMod = name, info.ModTime()
		}
	}
	return latest
}

// handleLogPaths 返回日志落点（GET /panel/api/logs/paths）。
//
// 路径**直接来自配置**（数据目录），即使目录/文件不存在也照实返回：
// 前端据此显示「目录还没生成」，而不是显示一个假地址或干脆报错。
// request_log_dir_exists / latest_request_log 是给「下载最近日志」入口用的。
func (s *Server) handleLogPaths(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	dataDir := s.config().WorkDir
	reqDir := s.requestLogDir()
	exists := false
	if st, err := os.Stat(reqDir); err == nil && st.IsDir() {
		exists = true
	}
	latest := ""
	if name := latestRequestLogFile(reqDir); name != "" {
		latest = filepath.Join(reqDir, name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data_dir":               dataDir,
		"request_log_dir":        reqDir,
		"request_log_dir_exists": exists,
		"latest_request_log":     latest,
	})
}

// handleRequestLogDownload 下载最近一个（或指定 name 的）请求日志归档。
//
// 只读：文件来自请求归档目录，下载不写任何东西。安全上**不拼任意路径** ——
// name 必须精确命中目录里的一项，因此 `../` 之类的路径穿越没有落脚点；
// 缺省时取最近写入的那个文件。
func (s *Server) handleRequestLogDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	dir := s.requestLogDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errBody("请求日志目录不存在或不可读: "+dir))
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = latestRequestLogFile(dir)
	}
	found := false
	if isRequestLogName(name) {
		for _, entry := range entries {
			if entry.Name() == name && !entry.IsDir() {
				found = true
				break
			}
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errBody("请求日志文件不存在: "+name))
		return
	}
	path := filepath.Join(dir, name)
	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("读取请求日志失败: "+err.Error()))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("读取请求日志失败: "+err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, info.ModTime(), f)
}
