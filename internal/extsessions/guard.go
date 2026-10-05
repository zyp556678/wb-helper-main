package extsessions

// -----------------------------------------------------------------------------
// 编辑器生命周期守卫（对照 vscode_ext.rs 的 is_vscode_running / close_vscode_for_switch
// / relaunch_closed_editor，以及 codebuddy_ide.rs 的同名部分）
//
// **为什么写之前必须退出编辑器**：编辑器把工作区索引与会话索引缓存在内存里，退出时
// 回写磁盘。运行中写入会被它整份覆盖 —— 用户看到的现象是「复制完的会话不见了」。
// 与 WorkBuddy 客户端同一个道理，所以这里也是「关闭 → 写 → 重开」的固定顺序。
//
// 关闭策略：**先优雅（taskkill 不带 /F / osascript quit / SIGTERM），超时再强杀**；
// 强杀后仍等不到退出就报错，调用方必须放弃写入。
//
// 探针与动作都是包级变量：单测替换后断言「命令序列」，绝不真去关跑测试那台机器上的编辑器。
// -----------------------------------------------------------------------------

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// editorCloseTimeout 是等待编辑器退出的总预算。
const editorCloseTimeout = 20 * time.Second

// editorPollInterval 是等待进程消失的轮询间隔。
const editorPollInterval = 400 * time.Millisecond

// editorProcess 是一个编辑器进程。
type editorProcess struct {
	PID int
	Exe string
	// Main 表示这是主进程（不是渲染/插件宿主等子进程）。
	Main bool
}

// 包级探针（单测替换）。
var (
	runEditorCmd = func(timeout time.Duration, name string, args ...string) (string, error) {
		cmd := exec.Command(name, args...)
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
			return "", errf("%s 超时（%s）", name, timeout)
		}
	}
	spawnEditor = func(exe string, args ...string) error {
		cmd := exec.Command(exe, args...)
		cmd.Dir = filepath.Dir(exe)
		cmd.SysProcAttr = editorProcAttr()
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	listEditorProcs = listEditorProcsOS
	editorProcCache struct {
		at    time.Time
		byKey map[string][]editorProcess
	}
)

// editorImages 返回某数据仓对应的进程映像名（Windows 精确匹配）。
//
// VS Code 的主进程与子进程都叫 Code.exe，靠命令行里的 `--type=` 区分：
// 只有**没有** `--type=` 的才是主进程（关它才会真正退出）。
func editorImages(spec Spec) []string {
	if spec.ClientDir == IDEStore.ClientDir {
		// CodeBuddy IDE 与 VS Code 同源（Electron），主进程映像名见客户端安装目录。
		return []string{"CodeBuddy.exe", "CodeBuddy CN.exe"}
	}
	return []string{"Code.exe"}
}

// editorAppNames 返回 macOS 上的应用名。
func editorAppNames(spec Spec) []string {
	if spec.ClientDir == IDEStore.ClientDir {
		return []string{"CodeBuddy CN", "CodeBuddy"}
	}
	return []string{"Visual Studio Code"}
}

// editorLinuxPattern 返回 Linux 上 pgrep 用的命令行模式。
func editorLinuxPattern(spec Spec) string {
	if spec.ClientDir == IDEStore.ClientDir {
		return "codebuddy"
	}
	return "(^|/)code( |$)"
}

// IsEditorRunning 报告编辑器是否在运行（对照 is_vscode_running）。
func IsEditorRunning(spec Spec) bool {
	return len(listEditorProcsCached(spec)) > 0
}

// listEditorProcsCached 是进程枚举的短时缓存（一次操作里会问好几次）。
func listEditorProcsCached(spec Spec) []editorProcess {
	key := spec.ClientDir
	editorProcCache.at = time.Now()
	if editorProcCache.byKey == nil {
		editorProcCache.byKey = map[string][]editorProcess{}
	}
	procs := listEditorProcs(spec)
	editorProcCache.byKey[key] = procs
	return procs
}

// invalidateEditorProcCache 丢弃缓存（关闭/启动后必须调用）。
func invalidateEditorProcCache() {
	editorProcCache.byKey = nil
}

func listEditorProcsOS(spec Spec) []editorProcess {
	switch runtime.GOOS {
	case "windows":
		self := os.Getpid()
		var out []editorProcess
		for _, img := range editorImages(spec) {
			script := `$ErrorActionPreference='SilentlyContinue'; ` +
				`Get-CimInstance Win32_Process -Filter "Name='` + img + `'" | ` +
				`ForEach-Object { "$($_.ProcessId)|$($_.ExecutablePath)|$($_.CommandLine)" }`
			text, err := runEditorCmd(8*time.Second, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
			if err != nil && text == "" {
				continue
			}
			for _, line := range strings.Split(text, "\n") {
				line = strings.TrimSpace(line)
				if line == "" || !strings.Contains(line, "|") {
					continue
				}
				parts := strings.SplitN(line, "|", 3)
				pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
				if err != nil || pid <= 0 || pid == self {
					continue
				}
				exe := ""
				cmdline := ""
				if len(parts) > 1 {
					exe = strings.TrimSpace(parts[1])
				}
				if len(parts) > 2 {
					cmdline = strings.TrimSpace(parts[2])
				}
				// 只有没有 --type= 的才是主进程（子进程关掉不影响编辑器本体）。
				main := !strings.Contains(cmdline, "--type=")
				if main {
					out = append(out, editorProcess{PID: pid, Exe: exe, Main: true})
				}
			}
		}
		return out
	case "darwin":
		var out []editorProcess
		for _, app := range editorAppNames(spec) {
			if text, err := runEditorCmd(5*time.Second, "pgrep", "-f", app); err == nil || text != "" {
				for _, line := range strings.Split(text, "\n") {
					pid, err := strconv.Atoi(strings.TrimSpace(line))
					if err != nil || pid <= 0 {
						continue
					}
					out = append(out, editorProcess{PID: pid, Main: true})
				}
			}
		}
		return out
	default:
		var out []editorProcess
		if text, err := runEditorCmd(5*time.Second, "pgrep", "-f", editorLinuxPattern(spec)); err == nil || text != "" {
			for _, line := range strings.Split(text, "\n") {
				pid, err := strconv.Atoi(strings.TrimSpace(line))
				if err != nil || pid <= 0 {
					continue
				}
				out = append(out, editorProcess{PID: pid, Main: true})
			}
		}
		return out
	}
}

// ClosedEditor 是本次被守卫关闭的编辑器（重开时要还原）。
type ClosedEditor struct {
	Spec Spec
	Exe  string
}

// CloseEditor 关闭编辑器：未运行返回 (nil, nil)；关不掉返回错误（调用方必须放弃写入）。
func CloseEditor(spec Spec) (*ClosedEditor, error) {
	procs := listEditorProcsCached(spec)
	if len(procs) == 0 {
		return nil, nil
	}
	exe := ""
	pids := make([]int, 0, len(procs))
	for _, p := range procs {
		if exe == "" && p.Exe != "" {
			exe = p.Exe
		}
		pids = append(pids, p.PID)
	}

	// ---- 1. 优雅关闭 ----
	if runtime.GOOS == "darwin" {
		for _, app := range editorAppNames(spec) {
			_, _ = runEditorCmd(10*time.Second, "osascript", "-e", `tell application "`+app+`" to quit`)
		}
	}
	for _, pid := range pids {
		name, args := editorKillCmd(pid, false)
		_, _ = runEditorCmd(10*time.Second, name, args...)
	}
	if waitEditorGone(spec, pids, editorCloseTimeout/2) {
		invalidateEditorProcCache()
		return &ClosedEditor{Spec: spec, Exe: exe}, nil
	}

	// ---- 2. 超时强杀 ----
	for _, pid := range aliveEditorProcs(spec, pids) {
		name, args := editorKillCmd(pid, true)
		_, _ = runEditorCmd(10*time.Second, name, args...)
	}
	if waitEditorGone(spec, pids, editorCloseTimeout/2) {
		invalidateEditorProcCache()
		return &ClosedEditor{Spec: spec, Exe: exe}, nil
	}
	invalidateEditorProcCache()
	return nil, errf("编辑器无法关闭（仍有 %d 个进程存活）。已放弃写入：编辑器退出时会把内存里的"+
		"索引回写磁盘，此时写入会被它覆盖。请手动完全退出编辑器后重试", len(aliveEditorProcs(spec, pids)))
}

// editorKillCmd 返回关闭/强杀某 PID 的命令（Windows 用 taskkill，其它平台用 kill）。
func editorKillCmd(pid int, force bool) (string, []string) {
	if runtime.GOOS == "windows" {
		args := []string{"/PID", strconv.Itoa(pid), "/T"}
		if force {
			args = append(args, "/F")
		}
		return "taskkill", args
	}
	signal := "-15"
	if force {
		signal = "-9"
	}
	return "kill", []string{signal, strconv.Itoa(pid)}
}

// waitEditorGone 轮询等待这些 PID 全部消失。
func waitEditorGone(spec Spec, pids []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if len(aliveEditorProcs(spec, pids)) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(editorPollInterval)
	}
}

// aliveEditorProcs 返回这些 PID 里仍在运行的（按当前枚举，避免 PID 复用误判）。
func aliveEditorProcs(spec Spec, pids []int) []int {
	live := map[int]bool{}
	for _, p := range listEditorProcs(spec) {
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

// LaunchEditor 重新打开编辑器（尽力而为：失败只报告，不改变写入结果）。
func LaunchEditor(closed *ClosedEditor) error {
	if closed == nil {
		return nil
	}
	defer invalidateEditorProcCache()
	switch runtime.GOOS {
	case "darwin":
		app := editorAppNames(closed.Spec)[0]
		if _, err := runEditorCmd(10*time.Second, "open", "-a", app); err != nil {
			return errf("启动 %s 失败：%v", app, err)
		}
		return nil
	case "windows":
		candidates := []string{}
		if closed.Exe != "" {
			candidates = append(candidates, closed.Exe)
		}
		candidates = append(candidates, editorExeCandidates(closed.Spec)...)
		for _, exe := range candidates {
			if _, err := os.Stat(exe); err != nil {
				continue
			}
			if err := spawnEditor(exe); err != nil {
				return errf("启动 %s 失败：%v", exe, err)
			}
			return nil
		}
		return errf("找不到编辑器可执行文件（尝试了 %d 个路径）。请手动打开，"+
			"会话写入已经完成，不影响结果", len(candidates))
	default:
		name := "code"
		if closed.Spec.ClientDir == IDEStore.ClientDir {
			name = "codebuddy"
		}
		if err := spawnEditor(name); err != nil {
			return errf("启动 %s 失败：%v（请手动打开）", name, err)
		}
		return nil
	}
}

// editorExeCandidates 返回编辑器的可执行文件候选路径（Windows）。
func editorExeCandidates(spec Spec) []string {
	var out []string
	images := editorImages(spec)
	dirs := []string{}
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		dirs = append(dirs, filepath.Join(v, "Programs"), v)
	}
	if v := os.Getenv("ProgramFiles"); v != "" {
		dirs = append(dirs, v)
	}
	if v := os.Getenv("ProgramFiles(x86)"); v != "" {
		dirs = append(dirs, v)
	}
	subdirs := []string{"Microsoft VS Code"}
	if spec.ClientDir == IDEStore.ClientDir {
		subdirs = []string{"CodeBuddy", "CodeBuddy CN"}
	}
	for _, dir := range dirs {
		for _, sub := range subdirs {
			for _, img := range images {
				out = append(out, filepath.Join(dir, sub, img))
			}
		}
	}
	return out
}

// guardUnavailableMsg 是「没有进程探针可用」时的说明（受限环境）。
func guardUnavailableMsg(spec Spec) string {
	return fmt.Sprintf("无法确认 %s 是否在运行（进程枚举不可用），已按保守策略处理", spec.Label)
}
