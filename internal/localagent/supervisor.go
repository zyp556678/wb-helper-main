// Package localagent 代管「本机代理」子进程：发现二进制、拉起、探活、日志归集、
// 异常重启、优雅退出。
//
// 为什么由网关代管而不是让用户自己起：
//   - 两者必须版本一致（本机代理改动往往跟着网关契约走），代管才能启动时校验；
//   - 面板需要知道「本机能力现在有没有」，代管才能给出可信的状态；
//   - 退出时网关能先请它优雅收尾（会话复制是多阶段写入，直接杀会留下半成品），
//     用户手工起两个进程就没人负责这件事。
//
// 找不到二进制时的行为刻意是「静默跳过」：服务端部署本来就不该有本机代理，
// 那种环境下 capabilities.local=false、前端隐藏本机页，这是正常路径而不是故障。
package localagent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Config 是代管参数。
type Config struct {
	// Enabled 为假时完全不启动（服务端部署可显式关掉）。
	Enabled bool
	// BinName 是二进制名（不含扩展名），默认 wb-local-agent。
	BinName string
	// DataDir 透传给代理的客户端数据目录（留空由代理自己推断）。
	DataDir string
	// Logf 是日志出口（默认丢弃）。
	Logf func(format string, args ...any)
	// Events 是事件出口（可为 nil）。
	Events EventSink
}

// EventSink 是事件日志的最小接口（避免本包依赖 eventlog 的具体实现）。
type EventSink interface {
	Info(channel, event, message string, fields map[string]any)
	Warn(channel, event, message string, fields map[string]any)
}

// 运行状态。
const (
	StateDisabled = "disabled" // 配置关闭
	StateMissing  = "missing"  // 找不到二进制（服务端部署的正常形态）
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopped  = "stopped"
	StateFailed   = "failed"
)

// Supervisor 代管本机代理进程。
type Supervisor struct {
	cfg Config

	mu         sync.Mutex
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	port       int
	token      string
	state      string
	lastErr    string
	startedAt  int64
	restarts   int
	backoff    time.Duration
	binPath    string
	lastProbe  int64
	lastHealth map[string]any

	// exited 在子进程 Wait 返回时关闭。用它判断「是否已退出」而不是
	// 轮询进程存活性：Windows 上 os.Process.Signal(syscall.Signal(0)) 不可靠。
	exited chan struct{}

	stopCh  chan struct{}
	stopped bool
}

// New 构造代管器（不启动）。
func New(cfg Config) *Supervisor {
	if strings.TrimSpace(cfg.BinName) == "" {
		cfg.BinName = "wb-local-agent"
	}
	return &Supervisor{
		cfg:     cfg,
		state:   StateStarting,
		backoff: time.Second,
		stopCh:  make(chan struct{}),
	}
}

func (s *Supervisor) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

func (s *Supervisor) emit(level, event, message string, fields map[string]any) {
	if s.cfg.Events == nil {
		return
	}
	if level == "warn" {
		s.cfg.Events.Warn("system", event, message, fields)
		return
	}
	s.cfg.Events.Info("system", event, message, fields)
}

// binFileName 返回带平台扩展名的二进制名。
func (s *Supervisor) binFileName() string {
	if runtime.GOOS == "windows" {
		return s.cfg.BinName + ".exe"
	}
	return s.cfg.BinName
}

// candidatePaths 列出查找二进制的位置，按优先级排列。
//
// 顺序有讲究：**先同目录**，因为「随主程序打包、解压即用」是分发的默认形态，
// 同目录命中率最高；再 ./bin 与用户空间目录。用户空间放最后是因为
// macOS 的完全磁盘访问按二进制路径授权，路径越稳定越好，而我们无法保证那里一定存在。
func (s *Supervisor) candidatePaths() []string {
	name := s.binFileName()
	out := []string{}
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), name))
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(wd, name), filepath.Join(wd, "bin", name))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".wb-gateway", "bin", name))
	}
	// 去重：exe 目录与当前工作目录常常是同一个（本地开发就是），
	// 不去重的话报错信息里会出现两条一模一样的路径，看起来像 bug。
	seen := map[string]bool{}
	uniq := make([]string, 0, len(out))
	for _, p := range out {
		if !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq
}

// findBinary 返回可用的二进制路径。
func (s *Supervisor) findBinary() (string, error) {
	tried := []string{}
	for _, p := range s.candidatePaths() {
		tried = append(tried, p)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到本机代理二进制（查找过 %s）", strings.Join(tried, "、"))
}

// Start 启动代管循环（异步、可重启）。ctx 结束即停止。
func (s *Supervisor) Start(ctx context.Context) {
	if !s.cfg.Enabled {
		s.mu.Lock()
		s.state = StateDisabled
		s.mu.Unlock()
		s.logf("[本机代理] 已在配置中关闭，不启动")
		return
	}

	bin, err := s.findBinary()
	if err != nil {
		s.mu.Lock()
		s.state = StateMissing
		s.lastErr = err.Error()
		s.mu.Unlock()
		// 这是服务端部署的正常路径，用 info 而不是 warn，避免误导运维去「修」
		s.logf("[本机代理] %v —— 本机相关页面将自动隐藏", err)
		return
	}

	s.mu.Lock()
	s.binPath = bin
	s.mu.Unlock()
	s.logf("[本机代理] 使用二进制 %s", bin)

	go s.supervise(ctx)
}

// supervise 是「启动 → 探活 → 挂了就退避重启」的主循环。
func (s *Supervisor) supervise(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case <-s.stopCh:
			s.shutdown()
			return
		default:
		}

		startedAt := time.Now()
		if err := s.spawn(ctx); err != nil {
			s.setFailed(err)
			if !s.waitBackoff(ctx) {
				return
			}
			continue
		}

		// 探活直到进程退出或 ctx 结束
		exited := s.probeLoop(ctx)

		// 跑够久说明上一次是稳定运行，重置退避（否则一次偶发崩溃会让退避一直停在很长）
		if time.Since(startedAt) > time.Minute {
			s.mu.Lock()
			s.backoff = time.Second
			s.mu.Unlock()
		}

		if ctx.Err() != nil {
			s.shutdown()
			return
		}
		if !exited {
			// 探活循环因 stopCh 退出
			s.shutdown()
			return
		}

		s.mu.Lock()
		s.restarts++
		count := s.restarts
		backoff := s.backoff
		s.backoff = min(backoff*2, time.Minute)
		s.mu.Unlock()

		s.emit("warn", "local_agent_restart", "本机代理异常退出，准备重启",
			map[string]any{"restarts": count, "backoff_seconds": int(backoff.Seconds())})
		s.logf("[本机代理] 异常退出，第 %d 次重启，等待 %v", count, backoff)
		if !s.waitBackoff(ctx) {
			return
		}
	}
}

// spawn 拉起子进程并等待它就绪。
func (s *Supervisor) spawn(ctx context.Context) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.state = StateStarting
	s.lastErr = ""
	s.token = token
	s.port = 0
	s.mu.Unlock()

	cmd := exec.Command(s.binPath)
	// 传入 127.0.0.1:0 让代理自己挑空闲端口，再通过 stdout 的 LISTENING 行告诉我们。
	// 由网关先挑端口再传参会有「挑完到子进程绑定之间被别人占用」的竞态。
	cmd.Env = append(os.Environ(),
		"WBG_LOCAL_ADDR=127.0.0.1:0",
		"WBG_LOCAL_TOKEN="+token,
	)
	if s.cfg.DataDir != "" {
		cmd.Env = append(cmd.Env, "WBG_LOCAL_DATA_DIR="+s.cfg.DataDir)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动本机代理失败: %w", err)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.stdin = stdin
	s.startedAt = time.Now().Unix()
	s.mu.Unlock()

	ready := make(chan int, 1)
	exited := make(chan struct{})
	s.mu.Lock()
	s.exited = exited
	s.mu.Unlock()

	go s.consumeStdout(stdout, ready)
	go s.consumeStderr(stderr)
	go func() {
		// 等进程退出，把状态置为 stopped，让探活循环与 shutdown 感知
		err := cmd.Wait()
		s.mu.Lock()
		if err != nil && s.state == StateRunning {
			s.lastErr = err.Error()
		}
		if s.state != StateStopped {
			s.state = StateStopped
		}
		s.mu.Unlock()
		close(exited)
	}()

	// 等待就绪行（最多 10 秒）
	select {
	case port := <-ready:
		s.mu.Lock()
		s.port = port
		s.state = StateRunning
		s.backoff = time.Second
		s.mu.Unlock()
		s.logf("[本机代理] 已就绪，监听 127.0.0.1:%d（pid=%d）", port, cmd.Process.Pid)
		s.emit("info", "local_agent_started", "本机代理已启动",
			map[string]any{"port": port, "pid": cmd.Process.Pid, "bin": s.binPath})
		return nil
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		return fmt.Errorf("本机代理启动超时：10 秒内没有报告监听地址")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return ctx.Err()
	}
}

// consumeStdout 解析就绪行并把后续输出归集到网关日志。
func (s *Supervisor) consumeStdout(r io.Reader, ready chan<- int) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 32*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if port, ok := parseListening(line); ok {
			select {
			case ready <- port:
			default:
			}
			continue
		}
		if strings.TrimSpace(line) != "" {
			s.logf("[本机代理] %s", line)
		}
	}
}

func (s *Supervisor) consumeStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 32*1024), 1<<20)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			s.logf("[本机代理:err] %s", line)
		}
	}
}

// parseListening 解析 "LISTENING 127.0.0.1:8318"。
func parseListening(line string) (int, bool) {
	const prefix = "LISTENING "
	if !strings.HasPrefix(line, prefix) {
		return 0, false
	}
	addr := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return 0, false
	}
	return port, port > 0
}

// probeLoop 每 10 秒探活一次；返回 true 表示进程已退出（需要重启）。
func (s *Supervisor) probeLoop(ctx context.Context) bool {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// 先立刻探一次，避免「刚起来 10 秒内状态还是 starting」
	s.probeOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return false
		case <-s.stopCh:
			return false
		case <-ticker.C:
			if s.procExited() {
				return true
			}
			s.probeOnce(ctx)
		}
	}
}

func (s *Supervisor) procExited() bool {
	s.mu.Lock()
	state := s.state
	s.mu.Unlock()
	return state == StateStopped || state == StateFailed
}

// probeOnce 发一次健康检查。
func (s *Supervisor) probeOnce(ctx context.Context) {
	s.mu.Lock()
	port, token := s.port, s.token
	s.mu.Unlock()
	if port == 0 {
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/api/health", port), nil)
	if err != nil {
		return
	}
	req.Header.Set("X-WBG-Token", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.mu.Lock()
		s.lastProbe = time.Now().Unix()
		s.lastErr = "探活失败: " + err.Error()
		s.mu.Unlock()
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	s.mu.Lock()
	s.lastProbe = time.Now().Unix()
	if resp.StatusCode == http.StatusOK {
		var health map[string]any
		if json.Unmarshal(body, &health) == nil {
			s.lastHealth = health
		}
		s.lastErr = ""
	} else {
		s.lastErr = fmt.Sprintf("探活返回 HTTP %d", resp.StatusCode)
	}
	s.mu.Unlock()
}

func (s *Supervisor) setFailed(err error) {
	s.mu.Lock()
	s.state = StateFailed
	s.lastErr = err.Error()
	s.mu.Unlock()
	s.logf("[本机代理] %v", err)
}

// waitBackoff 等待退避时长；ctx 结束返回 false。
func (s *Supervisor) waitBackoff(ctx context.Context) bool {
	s.mu.Lock()
	d := s.backoff
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return false
	case <-s.stopCh:
		return false
	case <-time.After(d):
		return true
	}
}

// Stop 请求代管器停止（幂等）。
func (s *Supervisor) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()
	close(s.stopCh)
	s.shutdown()
}

// shutdown 结束子进程：先关 stdin 请它优雅收尾，超时再强杀。
//
// 为什么必须先优雅：本机代理上的写操作（如会话复制）是多阶段写入，
// 直接杀会留下未完成状态，而这种残留用户很难发现、更难清理。
func (s *Supervisor) shutdown() {
	s.mu.Lock()
	cmd, stdin, exited := s.cmd, s.stdin, s.exited
	s.stdin = nil
	s.cmd = nil
	if s.state != StateDisabled && s.state != StateMissing {
		s.state = StateStopped
	}
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}
	if stdin != nil {
		_ = stdin.Close() // 关 stdin = 请它退出
	}
	if exited == nil {
		_ = cmd.Process.Kill()
		return
	}

	select {
	case <-exited:
		s.logf("[本机代理] 已优雅退出（pid=%d）", cmd.Process.Pid)
	case <-time.After(5 * time.Second):
		s.logf("[本机代理] 优雅退出超时，强制结束 pid=%d", cmd.Process.Pid)
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
		}
	}
}

// Restart 手动重启（面板按钮）。
func (s *Supervisor) Restart(ctx context.Context) error {
	if !s.cfg.Enabled {
		return fmt.Errorf("本机代理已在配置中关闭")
	}
	s.shutdown()
	bin, err := s.findBinary()
	if err != nil {
		s.mu.Lock()
		s.state = StateMissing
		s.lastErr = err.Error()
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	s.binPath = bin
	s.backoff = time.Second
	s.mu.Unlock()
	if err := s.spawn(ctx); err != nil {
		s.setFailed(err)
		return err
	}
	go s.probeLoop(context.WithoutCancel(ctx))
	return nil
}

// -----------------------------------------------------------------------------
// 对外状态与转发
// -----------------------------------------------------------------------------

// BaseURL 返回本机代理的回环地址（未运行时返回空串）。
func (s *Supervisor) BaseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateRunning || s.port == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", s.port)
}

// Token 返回一次性令牌。
func (s *Supervisor) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// Available 报告本机能力是否可用（前端据此决定是否显示本机页）。
func (s *Supervisor) Available() bool {
	return s.BaseURL() != ""
}

// Capabilities 返回给面板的状态快照。
//
// 刻意把 state / last_error / 查找过的路径都暴露出来：本机代理「没起来」有四种原因
// （配置关了、没装、崩了、探活失败），不区分的话用户只能干瞪眼。
func (s *Supervisor) Capabilities() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	caps := map[string]any{
		"enabled":    s.cfg.Enabled,
		"state":      s.state,
		"available":  s.state == StateRunning && s.port > 0,
		"local":      s.state == StateRunning && s.port > 0,
		"restarts":   s.restarts,
		"last_probe": s.lastProbe,
	}
	if s.port > 0 {
		caps["port"] = s.port
	}
	if s.startedAt > 0 {
		caps["started_at"] = s.startedAt
	}
	if s.lastErr != "" {
		caps["last_error"] = s.lastErr
	}
	if s.lastHealth != nil {
		caps["agent_version"] = s.lastHealth["version"]
		caps["agent_pid"] = s.lastHealth["pid"]
		caps["mode"] = s.lastHealth["mode"]
		caps["agent_capabilities"] = s.lastHealth["capabilities"]
		caps["unimplemented"] = s.lastHealth["unimplemented"]
	}
	if s.binPath != "" {
		caps["bin"] = s.binPath
	}
	if s.state == StateMissing {
		// 把查找路径告诉用户，他才知道该把二进制放哪
		caps["searched_paths"] = s.candidatePaths()
	}
	return caps
}

// ProxyPath 返回反代挂载前缀（与设计文档一致）。
const ProxyPath = "/panel/local"

// NewProxy 构造 /panel/local/* 的反向代理。
//
// 两个安全要点：
//  1. **剥掉外部凭据**：进来的 Authorization 一律删除，换成一次性令牌后转发。
//     否则同一个令牌会被外部请求方看到/复用（它只能网关知道）。
//  2. 目标固定为 127.0.0.1:<按需查询的端口>，不接受外部传入目标地址——
//     否则这个端点会变成一个开放代理。
func (s *Supervisor) NewProxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := s.BaseURL()
		if base == "" {
			writeLocalError(w, http.StatusServiceUnavailable, "local_agent_unavailable",
				"本机代理未运行：请把 "+s.binFileName()+" 放到网关同目录，或在配置中启用")
			return
		}

		// /panel/local/api/health → http://127.0.0.1:PORT/api/health
		rel := strings.TrimPrefix(r.URL.Path, ProxyPath)
		if rel == "" {
			rel = "/"
		}
		target := base + rel
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}

		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
		if err != nil {
			writeLocalError(w, http.StatusBadGateway, "proxy_request_error", err.Error())
			return
		}
		// 只透传内容协商相关的头，其余（尤其 Authorization/Cookie）一律不带
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-WBG-Token", s.Token())

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			writeLocalError(w, http.StatusBadGateway, "proxy_upstream_error", err.Error())
			return
		}
		defer resp.Body.Close()

		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 8<<20))
	})
}

func writeLocalError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": status, "type": code, "message": msg},
	})
}

// randomToken 生成 32 位十六进制令牌。
func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
