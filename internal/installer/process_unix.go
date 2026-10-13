//go:build !windows

package installer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// adoptLegacy 在 Unix 上目前没有需要接管的旧形态。
//
// Windows 需要它是因为历史上存在两套安装器写了两套注册表键；macOS/Linux 的
// 安装落点一直由 Tauri 的 productName 唯一决定，不存在分叉。保留这个函数
// 是为了让 Install 的流程在三端保持同构 —— 将来若有落点迁移，接管逻辑有地方放。
func adoptLegacy(opt Options, logf func(string, ...any)) (legacyInstall, error) {
	return legacyInstall{}, nil
}

// stopProcesses 结束「映像位于安装目录下」的进程，并等到文件锁释放。
//
// ## 为什么在 Unix 上也要做
//
// 与 Windows 不同，Unix 允许**替换正在运行的文件**（inode 语义：删掉旧 inode、
// 新文件用新 inode，运行中的进程继续持有旧的那个）。所以覆盖安装**不会报错** ——
// 但那正是更隐蔽的问题：旧进程会一直跑着旧版本，用户看到「升级成功了但版本没变」，
// 直到某次重启才生效。所以这里显式停掉，让新版本立刻生效。
func stopProcesses(installDir, dataDir string, logf func(string, ...any)) (int, error) {
	stopped := 0
	// 先停壳：壳一死，它给网关的 stdin 管道关闭，网关读到 EOF 自行优雅退出，
	// 并连带收掉本机代理（见 parentwatch.go）。这条链路比逐个杀可靠。
	for round := 0; round < 6; round++ {
		procs := processesUnder(installDir)
		if len(procs) == 0 {
			break
		}
		for _, p := range procs {
			logf("停止 %s (PID %d)", p.name, p.pid)
			if proc, err := os.FindProcess(int(p.pid)); err == nil {
				_ = proc.Signal(syscall.SIGTERM)
			}
			stopped++
		}
		time.Sleep(400 * time.Millisecond)
	}
	// SIGTERM 之后仍活着的（网关有多阶段写入，可能需要更久）再给一次机会，
	// 然后才强杀 —— 直接 SIGKILL 会让会话复制这类多阶段写入留下半成品。
	time.Sleep(500 * time.Millisecond)
	for _, p := range processesUnder(installDir) {
		if proc, err := os.FindProcess(int(p.pid)); err == nil {
			_ = proc.Signal(syscall.SIGKILL)
		}
	}
	return stopped, nil
}

// procInfo 是一个进程的最小描述。
type procInfo struct {
	name string
	pid  uint32
}

// processesUnder 返回可执行文件位于 dir 下的进程。
//
// Linux 读 /proc（无外部依赖）；macOS 用 ps。两者都不用 pgrep -f —— 那是按
// **命令行**匹配，一个恰好在参数里提到该路径的无关进程也会被误杀。
func processesUnder(dir string) []procInfo {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = dir
	}
	prefix := strings.TrimRight(real, "/") + "/"

	var procs []procInfo
	// Linux：/proc/<pid>/exe 是指向真实可执行文件的符号链接。
	entries, err := os.ReadDir("/proc")
	if err == nil {
		for _, e := range entries {
			pid, err := strconv.ParseUint(e.Name(), 10, 32)
			if err != nil {
				continue
			}
			link, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
			if err != nil {
				continue
			}
			if !strings.HasPrefix(link, prefix) {
				continue
			}
			name := filepath.Base(link)
			// 不杀自己：安装过程本身就是这个二进制在跑，杀掉等于中途退出。
			if uint32(pid) == uint32(os.Getpid()) {
				continue
			}
			procs = append(procs, procInfo{name: name, pid: uint32(pid)})
		}
		return procs
	}

	// macOS：ps 输出 pid 与完整可执行路径。
	out, err := exec.Command("ps", "-Ao", "pid=,comm=").Output()
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		pid, err := strconv.ParseUint(parts[0], 10, 32)
		if err != nil {
			continue
		}
		exe := strings.TrimSpace(parts[1])
		if !strings.HasPrefix(exe, prefix) {
			continue
		}
		if uint32(pid) == uint32(os.Getpid()) {
			continue
		}
		procs = append(procs, procInfo{name: filepath.Base(exe), pid: uint32(pid)})
	}
	return procs
}

// registerInstall 在 Unix 上注册图标/自启等。
//
// Linux 的 .desktop 条目、图标、文件归属都由 dpkg 负责（我们只覆盖维护脚本），
// macOS 的 .app 里已经带了 Info.plist，所以这里只处理自启。
func registerInstall(opt Options, logf func(string, ...any)) (string, error) {
	gatewayPath := filepath.Join(opt.InstallDir, "bin", gatewayBinaryName())
	version := readBinaryVersion(gatewayPath)
	logf("装上去的网关版本 %s", version)

	if opt.AutoStart {
		if err := enableAutoStart(gatewayPath, logf); err != nil {
			logf("注册开机自启失败（不影响安装）：%v", err)
		}
	}
	return version, nil
}

// readBinaryVersion 从二进制自身读出 `vX.Y.Z`（读不到返回 0.0.0）。
func readBinaryVersion(bin string) string {
	if !fileExists(bin) {
		return "0.0.0"
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		return "0.0.0"
	}
	for _, field := range strings.Fields(string(out)) {
		if strings.HasPrefix(field, "v") {
			return strings.TrimPrefix(field, "v")
		}
	}
	return "0.0.0"
}

// unregisterInstall 清理安装期写下的注册信息（卸载路径）。
//
// Unix 侧只有自启要清：Linux 的 .desktop / macOS 的 LaunchAgent 都是**用户级**的
// 文件（不由 dpkg 拥有），卸载时不会自动消失 —— 留着的话开机仍会尝试拉起一个
// 已经不存在的程序。图标与桌面条目由包管理器负责，不在这里删。
func unregisterInstall(opt Options, logf func(string, ...any)) {
	gateway := filepath.Join(opt.InstallDir, "bin", gatewayBinaryName())
	if fileExists(gateway) {
		if out, err := exec.Command(gateway, "autostart", "off").CombinedOutput(); err != nil {
			logf("关闭开机自启失败（%v）：%s", err, strings.TrimSpace(string(out)))
		} else {
			logf("已关闭开机自启")
		}
		return
	}
	// 二进制已不在（比如用户手工删过 bin/）：退化成直接删两处已知落点。
	// 这里写死路径是可以接受的 —— 它们是平台惯例位置，且失败无副作用。
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		_ = os.Remove(filepath.Join(home, ".config", "autostart", "workbuddy-gateway-desktop.desktop"))
		_ = os.Remove(filepath.Join(home, "Library", "LaunchAgents", "com.workbuddy.gateway.desktop.plist"))
	}
}

// enableAutoStart 调网关自己的 autostart 能力，不在这里重写落点逻辑 ——
// 面板开关、托盘开关、安装器必须操作同一条记录（见 internal/autostart）。
func enableAutoStart(gatewayPath string, logf func(string, ...any)) error {
	if !fileExists(gatewayPath) {
		return fmt.Errorf("网关二进制不存在：%s", gatewayPath)
	}
	out, err := exec.Command(gatewayPath, "autostart", "on").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v（输出：%s）", err, strings.TrimSpace(string(out)))
	}
	logf("已注册开机自启")
	return nil
}
