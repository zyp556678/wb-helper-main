package localapps

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// -----------------------------------------------------------------------------
// WorkBuddy 客户端进程控制（关闭 / 启动）
//
// **为什么切换必须先关闭客户端**：客户端在退出时会把内存里的登录态回写认证文件。
// 先写文件再关它，写进去的账号会被它退出时的回写覆盖 —— 用户看到的现象是
// 「切了又变回去」。所以顺序固定为：**关闭 → 写认证文件 → 启动**（对照 wb-switch
// 的 modules/process.rs：close_workbuddy → write_account_to_auth_file → launch_workbuddy）。
//
// 国内站与国际站是**两个独立的应用**（WorkBuddy / WorkBuddy AI），进程名与安装
// 路径都不同，所以这里按站点分支，不共用一套名字。
//
// 平台差异全部收敛在本文件：Windows 用 PowerShell CIM 取 PID+路径、taskkill 关闭；
// macOS 用 pgrep/osascript/open；Linux 用 pgrep/kill + 直接启动二进制。
// 命令执行器与启动器是包级变量，单测替换后即可断言「命令序列」而不用真关进程。
// -----------------------------------------------------------------------------

// wbCloseTimeout 是等待客户端退出的总预算（与 wb-switch 的 20 秒一致）。
const wbCloseTimeout = 20 * time.Second

// wbPollInterval 是等待进程消失的轮询间隔。
const wbPollInterval = 400 * time.Millisecond

// runProcCmd 执行命令并返回 stdout。可在单测中替换。
var runProcCmd = func(timeout time.Duration, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = procAttrNoWindow()
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
		return string(out), err
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return "", fmt.Errorf("%s 超时（%s）", name, timeout)
	}
}

// spawnDetached 启动一个 GUI 进程（不等待退出）。可在单测中替换。
//
// 必须**脱离**当前进程组：网关是被桌面壳拉起来的常驻进程，
// 如果新进程挂在同一作业/控制台下，网关重启会把它一起带走。
var spawnDetached = func(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = procAttrDetached()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// wbClientImages 返回某站点客户端在 Windows 上的进程映像名（精确匹配，不用子串）。
func wbClientImages(site string) []string {
	if site == "intl" {
		return []string{"WorkBuddyAI.exe"}
	}
	return []string{"WorkBuddy.exe"}
}

// wbClientApps 返回某站点客户端在 macOS 上的应用名。
func wbClientApps(site string) []string {
	if site == "intl" {
		return []string{"WorkBuddy AI.app"}
	}
	return []string{"WorkBuddy.app"}
}

// wbClientLinuxExe 返回某站点客户端在 Linux 上的可执行文件路径。
func wbClientLinuxExe(site string) string {
	if site == "intl" {
		return "/usr/bin/workbuddy-ai"
	}
	return "/usr/bin/workbuddy"
}

// wbLinuxPattern 返回 Linux 上 pgrep 用的命令行模式。
//
// 国内站要**排除**国际站：`workbuddy` 是 `workbuddy-ai` 的子串，直接子串匹配会把
// 国际版进程也算进来（切国内站时误关国际站客户端）。
func wbLinuxPattern(site string) string {
	if site == "intl" {
		return "workbuddy-ai"
	}
	// ERE：(行首或斜杠)workbuddy(空格或行尾) —— 我们的网关二进制叫
	// workbuddy-gateway，后面跟的是 `-`，不会自匹配。
	return `(^|/)workbuddy( |$)`
}

// wbProc 是一个客户端进程。
type wbProc struct {
	PID int
	// Exe 是进程映像的完整路径，取不到时为空（tasklist 兜底路径下就没有）。
	Exe string
}

// listWorkBuddyProcs 列出指定站点客户端当前运行的进程。
//
// 做成包级变量：单测替换后即可断言「命令序列」与失败路径，
// 而不必真去枚举、真去结束跑测试那台机器上的客户端。
var listWorkBuddyProcs = func(site string) []wbProc {
	switch runtime.GOOS {
	case "windows":
		return listWorkBuddyProcsWindows(site)
	case "darwin":
		return listProcsByPgrep(wbClientApps(site)[0])
	default:
		return listProcsByPgrep(wbLinuxPattern(site))
	}
}

func listWorkBuddyProcsWindows(site string) []wbProc {
	self := os.Getpid()
	var out []wbProc
	for _, img := range wbClientImages(site) {
		// 首选 PowerShell CIM：能同时拿到 PID 与映像路径（路径用于之后重新启动）。
		script := `$ErrorActionPreference='SilentlyContinue'; ` +
			`Get-CimInstance Win32_Process -Filter "Name='` + img + `'" | ` +
			`ForEach-Object { "$($_.ProcessId)|$($_.ExecutablePath)" }`
		if text, err := runProcCmd(8*time.Second, "powershell",
			"-NoProfile", "-NonInteractive", "-Command", script); err == nil {
			out = append(out, parseCIMRows(text)...)
		}
		// CIM 拿不到（权限/被裁剪的系统）时退回 tasklist：只有 PID，没有路径。
		if len(out) == 0 {
			if text, err := runProcCmd(5*time.Second, "tasklist",
				"/FI", "IMAGENAME eq "+img, "/FO", "CSV", "/NH"); err == nil {
				out = append(out, parseTasklistRows(text)...)
			}
		}
	}
	// 排除自己（理论上映像名不会撞上，防御性保留）。
	kept := out[:0]
	for _, p := range out {
		if p.PID > 0 && p.PID != self {
			kept = append(kept, p)
		}
	}
	return kept
}

// parseCIMRows 解析 `PID|ExePath` 行。
func parseCIMRows(text string) []wbProc {
	var out []wbProc
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "|") {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || pid <= 0 {
			continue
		}
		out = append(out, wbProc{PID: pid, Exe: strings.TrimSpace(parts[1])})
	}
	return out
}

// parseTasklistRows 解析 tasklist 的 CSV 输出（第 2 列是 PID）。
func parseTasklistRows(text string) []wbProc {
	var out []wbProc
	for _, line := range strings.Split(text, "\n") {
		fields := parseCSVLine(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil || pid <= 0 {
			continue
		}
		out = append(out, wbProc{PID: pid})
	}
	return out
}

// parseCSVLine 解析一行 CSV（tasklist 的字段可能带逗号，所以不能简单 split）。
func parseCSVLine(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var fields []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
			if inQuote && i+1 < len(line) && line[i+1] == '"' {
				cur.WriteByte('"')
				i++
				continue
			}
			inQuote = !inQuote
		case c == ',' && !inQuote:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	fields = append(fields, cur.String())
	return fields
}

// listProcsByPgrep 用 pgrep -f 列出匹配的 PID（macOS / Linux）。
func listProcsByPgrep(pattern string) []wbProc {
	text, err := runProcCmd(5*time.Second, "pgrep", "-f", pattern)
	if err != nil && text == "" {
		return nil
	}
	self := os.Getpid()
	var out []wbProc
	for _, line := range strings.Split(text, "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		out = append(out, wbProc{PID: pid})
	}
	return out
}

// closeWorkBuddyClient 关闭指定站点的客户端，返回是否原本在运行、以及进程映像路径
// （用于之后重新启动）。
//
// 关闭策略对照 wb-switch：**先优雅（taskkill 不带 /F / SIGTERM），超时再强杀**，
// 强杀后仍等不到退出就报错 —— 报错时调用方必须放弃写入，否则写进去的账号会被
// 客户端退出时的回写覆盖。
func closeWorkBuddyClient(site string, timeout time.Duration) (notes []string, wasRunning bool, exeHint string, err error) {
	defer invalidateProcCache()
	procs := listWorkBuddyProcs(site)
	if len(procs) == 0 {
		return []string{"客户端未在运行，跳过关闭"}, false, "", nil
	}
	wasRunning = true
	for _, p := range procs {
		if exeHint == "" && p.Exe != "" {
			exeHint = p.Exe
		}
	}
	pids := make([]int, 0, len(procs))
	for _, p := range procs {
		pids = append(pids, p.PID)
	}

	// ---- 1. 优雅关闭 ----
	//
	// macOS 额外先走一次「正常退出」请求：GUI 应用对 SIGTERM 的处理不一致，
	// 让系统替我们发退出事件最接近用户点关闭的行为（对照 wb-switch 的
	// close_workbuddy_macos：先 graceful quit，再清杀残留）。
	if runtime.GOOS == "darwin" {
		for _, app := range wbClientApps(site) {
			_, _ = runProcCmd(10*time.Second, "osascript", "-e",
				`tell application "`+strings.TrimSuffix(app, ".app")+`" to quit`)
		}
	}
	for _, pid := range pids {
		name, args := wbKillCmd(pid, false)
		_, _ = runProcCmd(10*time.Second, name, args...)
	}
	if waitProcsGone(site, pids, timeout/2) {
		return append(notes, fmt.Sprintf("已关闭客户端（%d 个进程）", len(pids))), wasRunning, exeHint, nil
	}

	// ---- 2. 超时强杀 ----
	left := aliveProcs(site, pids)
	for _, pid := range left {
		name, args := wbKillCmd(pid, true)
		_, _ = runProcCmd(10*time.Second, name, args...)
	}
	if waitProcsGone(site, pids, timeout/2) {
		return append(notes, fmt.Sprintf("客户端未响应优雅退出，已强制结束（%d 个进程）", len(pids))), wasRunning, exeHint, nil
	}
	return notes, wasRunning, exeHint, fmt.Errorf(
		"WorkBuddy 客户端无法关闭（仍有 %d 个进程存活）。已放弃写入：客户端退出时会把"+
			"内存里的登录态回写认证文件，此时写入会被它覆盖。请手动完全退出客户端后重试",
		len(aliveProcs(site, pids)))
}

// waitProcsGone 轮询等待这些 PID 全部消失。
func waitProcsGone(site string, pids []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if len(aliveProcs(site, pids)) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(wbPollInterval)
	}
}

// aliveProcs 返回这些 PID 里仍在运行的（按当前站点重新枚举，避免 PID 复用误判）。
func aliveProcs(site string, pids []int) []int {
	live := map[int]bool{}
	for _, p := range listWorkBuddyProcs(site) {
		live[p.PID] = true
	}
	var out []int
	for _, pid := range pids {
		if live[pid] {
			out = append(out, pid)
		}
	}
	return out
}

// workBuddyExeCachePath 是「上次用过的客户端可执行文件」缓存路径。
//
// 为什么要缓存：CIM 在部分机器上取不到 ExecutablePath，而默认安装路径有十几种可能。
// 成功启动过一次就把路径记下来，下次即使用户装了非默认目录也能直接启动。
func workBuddyExeCachePath() string {
	return filepath.Join(localAppData(), "wb-gateway", "workbuddy-exe.json")
}

func readWorkBuddyExeCache(site string) string {
	data, err := os.ReadFile(workBuddyExeCachePath())
	if err != nil {
		return ""
	}
	var m map[string]string
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return m[site]
}

func writeWorkBuddyExeCache(site, exe string) {
	m := map[string]string{}
	if data, err := os.ReadFile(workBuddyExeCachePath()); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	if m[site] == exe {
		return
	}
	m[site] = exe
	path := workBuddyExeCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(data, '\n'), 0o600)
}

// workBuddyExeCandidates 返回某站点客户端的可执行文件候选路径（Windows）。
func workBuddyExeCandidates(site string) []string {
	images := wbClientImages(site)
	var out []string
	dirs := []string{}
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		dirs = append(dirs, filepath.Join(v, "Programs"))
	}
	if v := os.Getenv("ProgramFiles"); v != "" {
		dirs = append(dirs, v)
	}
	if v := os.Getenv("ProgramFiles(x86)"); v != "" {
		dirs = append(dirs, v)
	}
	for _, dir := range dirs {
		for _, img := range images {
			out = append(out, filepath.Join(dir, strings.TrimSuffix(img, ".exe"), img))
		}
	}
	return out
}

// launchWorkBuddyClient 启动指定站点的客户端。
//
// 做成包级变量：单测替换后即可断言「关闭 → 写入 → 启动」的顺序，
// 而不会在跑测试的机器上真的弹出客户端。
var launchWorkBuddyClient = func(site, exeHint string) (string, error) {
	defer invalidateProcCache()
	return launchWorkBuddyClientOS(site, exeHint)
}

// launchWorkBuddyClientOS 是各平台的启动实现。
//
// exeHint 是关闭前从运行进程里读到的映像路径，优先用它 —— 用户可能装在非默认目录，
// 猜路径必然猜不到。取不到时依次尝试：缓存 → 默认安装目录。
func launchWorkBuddyClientOS(site, exeHint string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		app := wbClientApps(site)[0]
		if _, err := runProcCmd(10*time.Second, "open", "-a", app); err != nil {
			return "", fmt.Errorf("启动 %s 失败: %w", app, err)
		}
		return "已重新打开 " + strings.TrimSuffix(app, ".app"), nil
	case "windows":
		candidates := []string{}
		if exeHint != "" {
			candidates = append(candidates, exeHint)
		}
		if cached := readWorkBuddyExeCache(site); cached != "" {
			candidates = append(candidates, cached)
		}
		candidates = append(candidates, workBuddyExeCandidates(site)...)
		for _, exe := range candidates {
			if _, err := os.Stat(exe); err != nil {
				continue
			}
			if err := spawnDetached(exe); err != nil {
				return "", fmt.Errorf("启动 %s 失败: %w", exe, err)
			}
			writeWorkBuddyExeCache(site, exe)
			return "已重新打开 " + filepath.Base(exe), nil
		}
		return "", fmt.Errorf("找不到客户端可执行文件（尝试了 %d 个路径）。请手动打开客户端，"+
			"登录态已经写入，不影响账号切换结果", len(candidates))
	default:
		exe := wbClientLinuxExe(site)
		if _, err := os.Stat(exe); err != nil {
			return "", fmt.Errorf("找不到客户端可执行文件 %s，请手动打开", exe)
		}
		if err := spawnDetached(exe); err != nil {
			return "", fmt.Errorf("启动 %s 失败: %w", exe, err)
		}
		return "已重新打开 " + filepath.Base(exe), nil
	}
}

// -----------------------------------------------------------------------------
// 会话侧门禁用的导出接口
//
// 会话复制/同步与账号切换**共用同一套进程控制**：账号切换是「关闭 → 写认证文件 →
// 启动」，会话操作是「关闭 → 写会话库 → 启动」。分成两套实现会出现「切换能关掉、
// 会话操作关不掉」这类只有用户才会遇到的偏差（同一台机器、同一个客户端）。
// -----------------------------------------------------------------------------

// IsWorkBuddyRunning 报告某站点客户端是否在运行（对照 process::is_workbuddy_running）。
//
// 带 2 秒缓存：一次会话操作里会问好几次「客户端在不在跑」（门禁判定 + 逐目标复核），
// 而每次枚举在 Windows 上要起一次 PowerShell（实测 0.5–8 秒）。缓存让一次操作只枚举
// 一次；关闭/启动客户端后立即失效，避免读到过期结果。
func IsWorkBuddyRunning(site string) bool {
	return len(listWorkBuddyProcsCached(site)) > 0
}

// procCacheTTL 是进程枚举结果的缓存时长。
const procCacheTTL = 2 * time.Second

var procCache struct {
	mu   sync.Mutex
	at   time.Time
	byID map[string][]wbProc
}

// listWorkBuddyProcsCached 是 listWorkBuddyProcs 的短时缓存包装。
func listWorkBuddyProcsCached(site string) []wbProc {
	procCache.mu.Lock()
	defer procCache.mu.Unlock()
	if procCache.byID != nil && time.Since(procCache.at) < procCacheTTL {
		if procs, ok := procCache.byID[site]; ok {
			return procs
		}
	}
	procs := listWorkBuddyProcs(site)
	if procCache.byID == nil {
		procCache.byID = map[string][]wbProc{}
	}
	procCache.byID[site] = procs
	procCache.at = time.Now()
	return procs
}

// invalidateProcCache 丢弃缓存（关闭/启动客户端后必须调用）。
func invalidateProcCache() {
	procCache.mu.Lock()
	procCache.byID = nil
	procCache.mu.Unlock()
}

// CurrentLoginUID 读该站点认证文件里的 `account.uid`（对照 session::current_user_uid）。
//
// 第二个返回值为 false 表示**读不到**（文件缺失/损坏/没有 uid）—— 调用方必须把
// 「读不到」与「读到别的账号」分开处理：读不到时保守拦截写入，而不是当作没登录。
func CurrentLoginUID(site string) (string, bool) {
	doc, err := readJSONMap(workBuddyAuthPath(site))
	if err != nil {
		return "", false
	}
	acc, ok := doc["account"].(map[string]any)
	if !ok {
		return "", false
	}
	uid, _ := acc["uid"].(string)
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "", false
	}
	return uid, true
}

// CloseWorkBuddyClient 关闭指定站点的客户端（导出给会话侧门禁使用）。
func CloseWorkBuddyClient(site string, timeout time.Duration) (notes []string, wasRunning bool, exeHint string, err error) {
	return closeWorkBuddyClient(site, timeout)
}

// LaunchWorkBuddyClient 启动指定站点的客户端（导出给会话侧门禁使用）。
func LaunchWorkBuddyClient(site, exeHint string) (string, error) {
	return launchWorkBuddyClient(site, exeHint)
}

// WorkBuddySiteLabel 返回站点在会话报告里用的客户端名（对照 workbuddy_variant_label）。
func WorkBuddySiteLabel(site string) string {
	if site == "intl" {
		return "WorkBuddy 国际版"
	}
	return "WorkBuddy 国内版"
}
