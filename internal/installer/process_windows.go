//go:build windows

package installer

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// 旧形态安装留下的注册表位置。
//
// ## 为什么有一串要清理的历史键
//
// 这个项目先后用过两套互不相识的 Windows 安装器，各自写了**不同的** A/R 键：
//
//	NSIS（桌面版）    HKCU\...\Uninstall\WorkBuddy Gateway      ← 带空格
//	PowerShell（命令行版）HKCU\...\Uninstall\WorkBuddyGateway    ← 不带空格
//
// 两者的 DisplayName 都是「WorkBuddy Gateway」，所以在「设置 → 应用」里
// 看起来一模一样，实际却是两个独立条目 —— 这也正是老用户会看到重复条目的原因。
// 统一安装器接管时要把带空格那个删掉，否则新装的用户会在 A/R 里看到两个。
const (
	// legacyUninstKey 是 NSIS 写的 A/R 键（PRODUCTNAME 带空格）。
	legacyUninstKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\WorkBuddy Gateway`
	// legacyManuKey 是 NSIS 记录安装目录的地方，升级时它被当作 `$4` 传给旧卸载器。
	legacyManuKey = `HKCU\Software\workbuddy\WorkBuddy Gateway`
	// legacyRunValue 是 NSIS 卸载器试图删除的自启值名。
	//
	// 名字里**没有** "Desktop" 后缀，而实际写入的值名是 WorkBuddyGatewayDesktop
	// （见 internal/autostart 的 winValue），所以 NSIS 的卸载器删不掉它 ——
	// 这是旧版留下的自启残留，卸载后开机仍会被 Windows 尝试启动。
	legacyRunValue = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run\WorkBuddy Gateway`
)

// legacyInstall 描述一次被发现的旧形态安装（类型定义在 installer.go）。
// 这里只放与 Windows 相关的常量。

// stopProcesses 结束所有「映像位于安装目录下」的进程，并等到文件锁真正释放。
//
// ## 为什么不能只杀壳
//
// 真正占着待替换文件的是壳拉起的**子进程**（`bin\workbuddy-gateway.exe` 与
// `bin\wb-local-agent.exe`）。强杀壳时它的退出回调不执行，两个子进程会成为孤儿
// 继续持有文件句柄。不过网关侧有 parentwatch 兜底：壳一死，壳给它的 stdin 管道
// 关闭，网关读到 EOF 就自行优雅退出，并连带收掉本机代理。这条链路在实机日志里
// 得到过验证（47 次 `[父进程] stdin 已关闭` 记录）。
//
// 但仍要**显式**再扫一遍并按轮次杀：用户可能自己用命令行起过网关（那就不存在
// 父子关系，parentwatch 收不到 EOF）。
//
// ## 为什么按路径而不是按映像名
//
// 用户完全可能同时跑着仓库里的开发实例（同名、不同目录）。按映像名杀会误伤
// 与我们无关的进程；只处理安装目录下的才是职责范围内的事。
func stopProcesses(installDir, dataDir string, logf func(string, ...any)) (int, error) {
	stopped := 0

	// 分轮次：先杀壳，再杀网关与代理。顺序不能反 —— 网关的守护逻辑发现本机代理
	// 消失会把它重新拉起，先杀代理等于白杀。
	for round := 0; round < 6; round++ {
		procs := processesUnder(installDir)
		if len(procs) == 0 {
			break
		}
		for _, p := range procs {
			logf("停止 %s (PID %d)", p.name, p.pid)
			killProcess(p.pid)
			stopped++
		}
		time.Sleep(400 * time.Millisecond)
	}

	// 进程被杀不等于句柄立刻回收（内核要等清理完成）。这里等锁真正释放，
	// 而不是「杀完就往下走」—— 后者正是原来那个偶发失败的来源。
	//
	// 只探测**将要被替换**的文件，不去扫整个目录：目录里可能有用户自己放的东西
	// （凭据、日志），它们被占用不该阻断安装。
	files := []string{
		filepathJoin(installDir, "workbuddy-gateway-desktop.exe"),
		filepathJoin(installDir, "bin", "workbuddy-gateway.exe"),
		filepathJoin(installDir, "bin", "wb-local-agent.exe"),
		filepathJoin(installDir, "WebView2Loader.dll"),
	}
	for i := 0; i < 40; i++ {
		if locked := lockedFiles(files); len(locked) == 0 {
			return stopped, nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	if locked := lockedFiles(files); len(locked) > 0 {
		return stopped, fmt.Errorf(
			"以下文件仍被占用，安装中止：\n  %s\n\n"+
				"请在任务管理器中结束「WorkBuddy Gateway」相关进程后重试；"+
				"若仍失败，请重启电脑后再安装", strings.Join(locked, "\n  "))
	}
	return stopped, nil
}

// procInfo 是一个进程的最小描述。
type procInfo struct {
	name string
	pid  uint32
}

// processesUnder 返回映像路径位于 dir 下的所有进程。
//
// 用 `tasklist /FO CSV` 而不是 WMI：WMI 在部分精简系统上不可用，而 tasklist 是
// 每个 Windows 都有的。代价是它默认**不输出进程路径**，所以要用 `/M` 之外的办法 ——
// 这里改用 PowerShell 取 ExecutablePath（与旧 stop-installed-processes.ps1 同源），
// 因为按路径过滤是本函数的核心语义，不能退化成按名字。
func processesUnder(dir string) []procInfo {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	prefix := strings.TrimRight(dir, `\`) + `\`
	script := fmt.Sprintf(
		`Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and `+
			`$_.ExecutablePath.StartsWith('%s', [System.StringComparison]::OrdinalIgnoreCase) } | `+
			`ForEach-Object { "$($_.ProcessId)|$($_.Name)" }`,
		strings.ReplaceAll(prefix, "'", "''"))

	out, err := runHidden("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return nil
	}
	var procs []procInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		var pid uint32
		if _, err := fmt.Sscanf(parts[0], "%d", &pid); err != nil {
			continue
		}
		procs = append(procs, procInfo{name: strings.TrimSpace(parts[1]), pid: pid})
	}
	return procs
}

// lockedFiles 返回列表里仍被占用（无法独占打开）的文件。
//
// Share=None 的独占打开正是「复制文件所需」的权限，拿它当判据比看进程列表可靠：
// 进程列表可能与实际持有的句柄无关（比如进程已进入退出流程）。
func lockedFiles(paths []string) []string {
	var locked []string
	for _, p := range paths {
		if !fileExists(p) {
			continue
		}
		if !canOpenExclusive(p) {
			locked = append(locked, p)
		}
	}
	return locked
}

// canOpenExclusive 报告能否以独占方式打开文件。
//
// 用 syscall.CreateFile 而不是 os.OpenFile：Go 的 os 包在 Windows 上固定以
// FILE_SHARE_READ|FILE_SHARE_WRITE 打开，**永远拿不到独占语义**，
// 于是「文件被占用」这件事在 os 层面根本测不出来 —— 而它正是本函数的全部意义。
func canOpenExclusive(path string) bool {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // 不共享：只要还有别的句柄（哪怕只读）就会失败
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return false
	}
	_ = syscall.CloseHandle(h)
	return true
}

// killProcess 强杀一个进程，失败静默 —— 调用方按轮次重试并最终用文件锁判定成败。
func killProcess(pid uint32) {
	cmd := exec.Command("taskkill.exe", "/PID", fmt.Sprint(pid), "/T", "/F")
	// /T 连带结束子进程树：壳被杀时它的网关子进程还没读到 stdin EOF，
	// 连带结束可以少等一轮。
	runHiddenCmd(cmd)
}

// runHidden 跑一条命令并返回 stdout（隐藏窗口）。
//
// 隐藏是必须的：安装流程常常由面板在后台触发，弹出一个黑色控制台窗口
// 会让用户以为程序出了问题。
func runHidden(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	return string(out), err
}

func runHiddenCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}
