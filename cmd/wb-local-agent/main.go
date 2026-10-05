// Command wb-local-agent 是 WorkBuddy 网关的**最小本机代理**。
//
// 定位必须说清楚，否则很容易被误解成「这就是 switch 的本机代理」：
//
//	switch 的 Rust 本机代理（crates/wb-switch-server，约 4.2 万行）提供的是
//	会话复制、进程检测、SQLite 读写、跨编辑器配置注入、权限引导这类**读写本机**的能力。
//	本文件**不是**它的替代品，而是一个契约相同的**最小只读实现**，用途有两个：
//
//	  1. 让 Go 侧的编排框架（internal/localagent：拉起、探活、日志归集、异常重启、优雅退出）
//	     有一个真实对端可以做端到端验证——没有它，编排代码只能靠读代码假装正确；
//	  2. 给只想要「看一眼本机客户端状态」的用户一个开箱可用的形态。
//
// 契约：监听 127.0.0.1 上的指定端口，所有 /api/* 都要求 X-WBG-Token 头，
// 路径命名与 switch 的 /api/* 保持一致，因此 Rust 版可以直接替换本二进制。
//
// 安全边界（刻意为之）：
//   - 只绑回环地址，绝不监听 0.0.0.0；
//   - 所有请求校验一次性令牌（由网关通过环境变量注入）；
//   - **全部能力都是只读**：读目录、数文件、看日志尾部。不做任何写入、不读凭据正文、
//     不执行子进程。写操作留给 Rust 版——在有完整幂等安装/备份/还原那套保护之前，
//     用 Go 重写一遍只会把「安全」这件事做薄。
//   - 日志与文件列表都有条数与字节上限，避免一个巨大日志把内存吃光。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const agentVersion = "0.1.0"

var started = time.Now()

func main() {
	var (
		addr   = flag.String("addr", "", "监听地址（默认 127.0.0.1:8318，端口 0 表示自动选择）")
		dataIn = flag.String("data-dir", "", "本机客户端数据目录（默认 ~/.workbuddy）")
	)
	flag.Parse()

	listenAddr := strings.TrimSpace(*addr)
	if listenAddr == "" {
		listenAddr = strings.TrimSpace(os.Getenv("WBG_LOCAL_ADDR"))
	}
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8318"
	}
	token := strings.TrimSpace(os.Getenv("WBG_LOCAL_TOKEN"))

	dataDir := strings.TrimSpace(*dataIn)
	if dataDir == "" {
		dataDir = strings.TrimSpace(os.Getenv("WBG_LOCAL_DATA_DIR"))
	}
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("[本机代理] 无法定位用户主目录: %v", err)
		}
		dataDir = filepath.Join(home, ".workbuddy")
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("[本机代理] 监听 %s 失败: %v", listenAddr, err)
	}
	// 拒绝非回环连接：即使有人把 addr 配成 0.0.0.0，这里也会挡住。
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok && !tcpAddr.IP.IsLoopback() {
		log.Fatalf("[本机代理] 拒绝监听非回环地址 %s（本机代理持有读本机数据的能力，只允许回环）", ln.Addr())
	}

	a := &agent{dataDir: dataDir, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", a.auth(a.handleHealth))
	mux.HandleFunc("/api/local/overview", a.auth(a.handleOverview))
	mux.HandleFunc("/api/local/logs", a.auth(a.handleLogs))
	mux.HandleFunc("/api/local/hooks", a.auth(a.handleHooks))
	mux.HandleFunc("/api/local/paths", a.auth(a.handlePaths))

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// 就绪信号：网关按这一行判断可以开始探活，避免用固定 sleep 猜时间。
	fmt.Printf("LISTENING %s\n", ln.Addr().String())
	os.Stdout.Sync()
	log.Printf("[本机代理] v%s 已启动，数据目录 %s，令牌校验=%v", agentVersion, dataDir, token != "")

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[本机代理] 服务异常退出: %v", err)
		}
	}()

	// 退出路径一：stdin 关闭。网关用「关掉子进程的 stdin」请求优雅退出，
	// 这比发信号更可靠——Windows 上 os.Interrupt 对无控制台的子进程经常无效。
	stdinClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(stdinClosed)
	}()

	// 退出路径二：信号（手工前台运行时用 Ctrl+C）。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case <-stdinClosed:
		log.Printf("[本机代理] 父进程已关闭 stdin，开始退出")
	case s := <-sigCh:
		log.Printf("[本机代理] 收到信号 %v，开始退出", s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Printf("[本机代理] 已停止")
}

// agent 持有运行状态。
type agent struct {
	dataDir string
	token   string

	mu      sync.Mutex
	cacheSz int64
	cacheAt time.Time
}

// auth 是令牌校验中间件。
//
// 为什么必须校验：本代理读的是用户自己的客户端数据，虽然都是只读元信息，
// 但同机上任何进程都能访问回环端口——不校验等于把「本机有哪些会话/日志」暴露给所有本地进程。
func (a *agent) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.token != "" {
			if r.Header.Get("X-WBG-Token") != a.token {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": map[string]any{"code": 401, "message": "缺少或错误的 X-WBG-Token"},
				})
				return
			}
		}
		next(w, r)
	}
}

func (a *agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"version":  agentVersion,
		"pid":      os.Getpid(),
		"uptime":   int64(time.Since(started).Seconds()),
		"data_dir": a.dataDir,
		"mode":     "readonly",
		"capabilities": []string{
			"overview", "logs", "hooks", "paths",
		},
		// unimplemented 明示「契约里有哪些能力本实现没做」，
		// 让网关与面板能如实展示，而不是让人以为功能缺失是 bug。
		"unimplemented": []string{
			"session_copy", "process_scan", "sqlite_read", "editor_inject", "hooks_install",
		},
	})
}

// handleOverview 汇总本机客户端状态（全部只读）。
func (a *agent) handleOverview(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"data_dir":        a.dataDir,
		"data_dir_exists": dirExists(a.dataDir),
		"scanned_at":      time.Now().Unix(),
	}
	if !dirExists(a.dataDir) {
		writeJSON(w, http.StatusOK, info)
		return
	}

	info["counts"] = map[string]any{
		"sessions": countFiles(filepath.Join(a.dataDir, "sessions"), ".json", 1),
		"projects": countDirs(filepath.Join(a.dataDir, "projects")),
		"skills":   countDirs(filepath.Join(a.dataDir, "skills")),
		"plugins":  countDirs(filepath.Join(a.dataDir, "plugins")),
	}

	// 客户端版本：不同版本放置位置不同，逐个尝试并如实标注来源。
	// 实测 app-config.json 里往往没有 version，而 AppStartup.log 的第一行有
	// "appName=WorkBuddy appVersion=5.6.2 build=..."，所以日志是更可靠的来源。
	info["client"] = map[string]any{
		"settings_present": fileExists(filepath.Join(a.dataDir, "settings.json")),
		"app_config":       readAppConfig(filepath.Join(a.dataDir, "app", "app-config.json")),
		"from_log":         clientVersionFromLog(filepath.Join(a.dataDir, "logs", "AppStartup.log")),
	}

	// 数据库：只报存在性与大小，不打开（打开会加锁，可能干扰正在运行的客户端）。
	dbs := []map[string]any{}
	for _, name := range []string{"workbuddy.db", "edge-sync-mapping-v4.db"} {
		p := filepath.Join(a.dataDir, name)
		if st, err := os.Stat(p); err == nil {
			dbs = append(dbs, map[string]any{"name": name, "size": st.Size(), "mtime": st.ModTime().Unix()})
		}
	}
	info["databases"] = dbs

	info["logs"] = a.logSummary()
	info["storage"] = a.storageUsage()
	writeJSON(w, http.StatusOK, info)
}

// handlePaths 返回本机代理能访问到的关键路径（面板展示 + 排障用）。
func (a *agent) handlePaths(w http.ResponseWriter, r *http.Request) {
	paths := []map[string]any{}
	for _, rel := range []string{
		"settings.json", "sessions", "projects", "logs", "skills", "plugins",
		"workbuddy.db", "edge-sync-mapping-v4.db", "app/app-config.json",
	} {
		full := filepath.Join(a.dataDir, filepath.FromSlash(rel))
		st, err := os.Stat(full)
		entry := map[string]any{"path": full, "rel": rel, "exists": err == nil}
		if err == nil {
			entry["is_dir"] = st.IsDir()
			entry["size"] = st.Size()
			entry["mtime"] = st.ModTime().Unix()
		}
		paths = append(paths, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data_dir": a.dataDir, "paths": paths})
}

// handleLogs 列出日志文件；带 file 参数时返回该文件尾部若干行。
//
// 上限刻意压得很低：日志文件可能有几十 MB，全量读回网关没有任何意义，
// 面板要看的就是「最后发生了什么」。
func (a *agent) handleLogs(w http.ResponseWriter, r *http.Request) {
	const maxTailBytes = 256 << 10
	tail := 100
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := parseInt(v); err == nil && n > 0 && n <= 2000 {
			tail = n
		}
	}

	logsDir := filepath.Join(a.dataDir, "logs")
	files := listLogFiles(logsDir, 50)

	if name := strings.TrimSpace(r.URL.Query().Get("file")); name != "" {
		// 只允许访问 logs 目录下的文件：拒绝任何路径成分，防目录穿越
		if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{"code": 400, "message": "非法文件名"},
			})
			return
		}
		content, err := tailFile(filepath.Join(logsDir, filepath.Base(name)), maxTailBytes, tail)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": map[string]any{"code": 404, "message": err.Error()},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"file": name, "lines": content})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs_dir": logsDir, "files": files})
}

// handleHooks 读客户端 settings.json 里的 hooks 段（只读展示）。
//
// 为什么只看不写：switch 会往这里注册 Stop / FinalStop 钩子来做限额台账，
// 那是**写用户客户端配置**——需要幂等安装、写前备份、逐字节还原这一整套保护。
// 本代理处于只读形态，先把「看得到」做出来，改动交给 Rust 版。
func (a *agent) handleHooks(w http.ResponseWriter, r *http.Request) {
	p := filepath.Join(a.dataDir, "settings.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"present": false,
			"message": "未找到 settings.json：客户端可能尚未启动过",
		})
		return
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"present": true,
			"error":   "settings.json 解析失败: " + err.Error(),
		})
		return
	}
	hooks, _ := doc["hooks"].(map[string]any)
	summary := []map[string]any{}
	for event, v := range hooks {
		arr, _ := v.([]any)
		summary = append(summary, map[string]any{
			"event":   event,
			"entries": len(arr),
			// 只回显匹配器与命令的前若干字符，避免把命令全文（可能含路径中的用户名）整段透出
			"preview": previewHooks(arr),
		})
	}
	sort.Slice(summary, func(i, j int) bool {
		return summary[i]["event"].(string) < summary[j]["event"].(string)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"present":       true,
		"hooks":         summary,
		"hook_count":    len(hooks),
		"settings_keys": sortedKeys(doc),
	})
}

// -----------------------------------------------------------------------------
// 本机信息采集（只读，全部带边界）
// -----------------------------------------------------------------------------

func (a *agent) logSummary() map[string]any {
	logsDir := filepath.Join(a.dataDir, "logs")
	out := map[string]any{"dir": logsDir, "file_count": 0, "latest_date": ""}
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return out
	}
	count := 0
	dates := []string{}
	for _, e := range entries {
		if e.IsDir() {
			// 日期目录形如 2026-09-24
			if len(e.Name()) == 10 && e.Name()[4] == '-' {
				dates = append(dates, e.Name())
			}
			continue
		}
		count++
	}
	sort.Strings(dates)
	if len(dates) > 0 {
		out["latest_date"] = dates[len(dates)-1]
	}
	out["file_count"] = count
	out["dates"] = dates
	return out
}

// storageUsage 计算数据目录总占用。结果缓存 5 分钟——
// 1.3 GB 的目录遍历在冷缓存时要几秒，面板刷新一次就重扫会拖慢整个代理。
func (a *agent) storageUsage() map[string]any {
	a.mu.Lock()
	if time.Since(a.cacheAt) < 5*time.Minute && a.cacheAt.Unix() > 0 {
		size := a.cacheSz
		at := a.cacheAt
		a.mu.Unlock()
		return map[string]any{"size_bytes": size, "computed_at": at.Unix(), "cached": true}
	}
	a.mu.Unlock()

	size := dirSize(a.dataDir, 6)
	a.mu.Lock()
	a.cacheSz = size
	a.cacheAt = time.Now()
	a.mu.Unlock()
	return map[string]any{"size_bytes": size, "computed_at": time.Now().Unix(), "cached": false}
}

// dirSize 统计目录大小；maxDepth 限制深度，避免在巨大的嵌套目录上跑太久。
func dirSize(root string, maxDepth int) int64 {
	var total int64
	var walk func(path string, depth int)
	walk = func(path string, depth int) {
		entries, err := os.ReadDir(path)
		if err != nil {
			return
		}
		for _, e := range entries {
			full := filepath.Join(path, e.Name())
			if e.IsDir() {
				if depth < maxDepth {
					walk(full, depth+1)
				}
				continue
			}
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	walk(root, 0)
	return total
}

func listLogFiles(logsDir string, limit int) []map[string]any {
	out := []map[string]any{}
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"name": e.Name(), "size": info.Size(), "mtime": info.ModTime().Unix(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["mtime"].(int64) > out[j]["mtime"].(int64)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// tailFile 读文件尾部：最多 maxBytes，再从后往前取 tail 行。
func tailFile(path string, maxBytes int64, tail int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := int64(0)
	if st.Size() > maxBytes {
		offset = st.Size() - maxBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return lines, nil
}

// clientVersionFromLog 从 AppStartup.log 里解析客户端版本（只读前 64KB，够取到首行）。
func clientVersionFromLog(path string) map[string]any {
	f, err := os.Open(path)
	if err != nil {
		return map[string]any{"present": false}
	}
	defer f.Close()
	raw, _ := io.ReadAll(io.LimitReader(f, 64<<10))
	text := string(raw)
	out := map[string]any{"present": true}
	for _, key := range []string{"appName", "appVersion", "build"} {
		if v := extractKV(text, key); v != "" {
			out[key] = v
		}
	}
	return out
}

// extractKV 从文本里取 "key=value" 的 value。
//
// 关键细节：日志里同一个键可能出现多次，且**早期行可能是空值**
// （实测 AppStartup.log 第一条 appVersion= 就是空的，5.6.2 在后面的行里）。
// 所以不能只取首次出现，必须跳过空值继续找——否则版本号永远解析不出来。
func extractKV(text, key string) string {
	needle := key + "="
	rest := text
	for {
		idx := strings.Index(rest, needle)
		if idx < 0 {
			return ""
		}
		rest = rest[idx+len(needle):]
		end := len(rest)
		if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
			end = i
		}
		if v := strings.TrimSpace(rest[:end]); v != "" {
			return v
		}
	}
}

func readAppConfig(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{"present": false}
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return map[string]any{"present": true, "error": err.Error()}
	}
	out := map[string]any{"present": true}
	// 只挑与版本/渠道相关的键；其余整包内容不往外传（可能含用户标识）
	for _, key := range []string{"version", "appVersion", "channel", "buildNumber", "edition"} {
		if v, ok := doc[key]; ok {
			out[key] = v
		}
	}
	return out
}

func previewHooks(arr []any) []map[string]any {
	out := []map[string]any{}
	for _, v := range arr {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		entry := map[string]any{}
		if matcher, ok := m["matcher"].(string); ok {
			entry["matcher"] = truncate(matcher, 60)
		}
		if hooks, ok := m["hooks"].([]any); ok {
			cmds := []string{}
			for _, hv := range hooks {
				if hm, ok := hv.(map[string]any); ok {
					if cmd, ok := hm["command"].(string); ok {
						cmds = append(cmds, truncate(cmd, 80))
					}
				}
			}
			entry["commands"] = cmds
		}
		out = append(out, entry)
	}
	return out
}

// -----------------------------------------------------------------------------
// 小工具
// -----------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func countFiles(dir, suffix string, maxDepth int) int {
	n := 0
	var walk func(string, int)
	walk = func(path string, depth int) {
		entries, err := os.ReadDir(path)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				if depth < maxDepth {
					walk(filepath.Join(path, e.Name()), depth+1)
				}
				continue
			}
			if suffix == "" || strings.HasSuffix(e.Name(), suffix) {
				n++
			}
		}
	}
	walk(dir, 0)
	return n
}

func countDirs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}
