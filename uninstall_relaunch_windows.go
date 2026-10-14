//go:build windows

package main

// Windows 上「卸载器删不掉自己」的兜底。
//
// ## 两个必须一起解决的问题
//
// A/R 里的卸载项指向 `<安装目录>\bin\workbuddy-gateway.exe uninstall --dir <安装目录>`，
// 于是卸载进程的可执行文件**就在它必须删掉的那个目录里**：
//
//  1. Windows 不允许删除正在运行的映像 —— os.RemoveAll 会在 `bin\workbuddy-gateway.exe`
//     上以 "Access is denied" 失败，目录删到一半留下残骸（实测确认，不是推测）。
//     Unix 没有这个问题：inode 语义允许 unlink 正在运行的文件。
//  2. stopProcesses 按「映像位于安装目录之下」筛选要结束的进程，这个判据会把
//     **正在执行卸载的自己**一起筛进去，于是 taskkill /F 把卸载器自己杀掉，
//     流程半途而废 —— 用户看到的现象是「卸载点了没反应」。
//
// ## 做法：把自己搬到临时目录，再让副本接手
//
// 检测到自身位于目标目录内时，把可执行文件复制到 %TEMP% 下的临时目录，用同一个
// `uninstall -dir <同一个目录>` 启动副本（外加 `-wait-pid <自己>`），本进程随即退出。
// 副本不在安装目录之下，上面两条同时消失：它删得掉整个目录，也不会被自己的筛选命中。
//
// ## 副本必须完全脱离父进程的 console（踩过的坑）
//
// 副本一开始是用 `HideWindow: true` 启动的，它因此**继承了父进程的 console**。
// 父进程为了尽快让出安装目录里的文件锁立刻退出，它一退，那个 console 就关闭，
// 副本随即收到 CTRL_CLOSE_EVENT 被终结 —— 症状非常隐蔽：安装目录确实删掉了
// （那一步在副本被杀之前就做完了），只有副本自己的临时目录永远留着，
// 每次卸载在 %TEMP% 里残留一份二十多 MB 的可执行文件。
//
// 所以副本用 DETACHED_PROCESS 启动：不继承 console，也就不受父进程退出影响。
// 输出改写到 %TEMP%\wb-uninstall.log（排查副本行为时有据可查），而不是一条
// 即将失效的管道。
//
// ## 副本自己也要能收尾
//
// 副本删不掉正在运行的自己，所以最后交给一个同样脱离的 cmd：等副本退出后再 rmdir。
// 那条 cmd 的工作目录必须设在待删目录**之外** —— Windows 拒绝删除「仍是某个进程
// 当前目录」的目录，这一条同样是实测得出的（见下）。

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// detachedProcess 让子进程不继承父进程的 console。
//
// 这不是「顺手加个隐藏窗口」：父进程退出会关闭它持有的 console，任何仍挂在上面的
// 子进程都会收到 CTRL_CLOSE_EVENT 而被终结。副本必须不受此影响。
const detachedProcess = 0x00000008

// selfInsideDir 报告当前可执行文件是否位于 dir 之下。
func selfInsideDir(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(self))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// relaunchSelfForUninstall 把自己复制到临时目录，交给副本继续卸载。返回退出码。
func relaunchSelfForUninstall(f installFlags, out, errOut io.Writer) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(errOut, "无法定位自身以转交卸载：%v\n", err)
		return 1
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}

	tmpDir, err := os.MkdirTemp("", "wb-uninstall-")
	if err != nil {
		fmt.Fprintf(errOut, "无法创建临时目录：%v\n", err)
		return 1
	}
	tmpExe := filepath.Join(tmpDir, "workbuddy-gateway-uninstall.exe")
	if err := copySelfExe(self, tmpExe); err != nil {
		_ = os.RemoveAll(tmpDir)
		fmt.Fprintf(errOut, "无法把卸载器复制到临时目录：%v\n", err)
		return 1
	}

	args := []string{"uninstall", "-dir", f.installDir, "-wait-pid", strconv.Itoa(os.Getpid())}
	if strings.TrimSpace(f.dataDir) != "" {
		args = append(args, "-data-dir", f.dataDir)
	}
	if f.dropData {
		args = append(args, "-drop-data")
	}
	// 开关必须原样带给副本。漏掉 -no-register 的后果不是「多写了一点东西」，
	// 而是绿色版（-no-register 装的）卸载时会去删**另一个正规安装**的 A/R 项与
	// 快捷方式 —— 副本在临时目录里，不满足 selfInsideDir，不会再次转交，
	// 于是一次误删就这么执行完了。
	if f.noRegister {
		args = append(args, "-no-register")
	}

	cmd := exec.Command(tmpExe, args...)
	// 副本的工作目录**不能**是待删的临时目录本身：Windows 拒绝删除「仍是某个进程
	// 当前目录」的目录，而副本要删的正是自己脚下这个目录 —— 于是 rmdir 永远失败、
	// 每次卸载残留一份二十多 MB 的副本。设在临时目录根上，副本的 CWD 就不在待删
	// 目录之内了（实测确认过这条因果，不是推测）。
	cmd.Dir = os.TempDir()

	// 输出落到日志文件而不是继承 stdout：父进程马上要退出，它那条管道随时会失效，
	// 把副本的关键步骤写进一个稳定的文件，出问题时才有据可查。
	if lf, err := os.OpenFile(filepath.Join(os.TempDir(), "wb-uninstall.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		cmd.Stdout = lf
		cmd.Stderr = lf
		defer lf.Close()
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(tmpDir)
		fmt.Fprintf(errOut, "无法启动卸载副本：%v\n", err)
		return 1
	}

	fmt.Fprintf(out, "卸载已转交临时副本继续执行\n  安装目录 %s 将在本进程退出后删除\n", f.installDir)
	return 0
}

// waitForProcessExit 等指定 PID 退出（或超时）。
//
// 副本必须先等原进程走完：原进程的映像还占着安装目录里的文件，不等它就删目录，
// 结果还是那句 Access is denied。
func waitForProcessExit(pid int) {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return // 已经退出了
	}
	defer syscall.CloseHandle(h)
	// 上限 30 秒：卡在这里不放，比留一个删不干净的目录更糟。
	_, _ = syscall.WaitForSingleObject(h, 30000)
	// 句柄可等待不等于映像已从文件系统脱开（退出码与句柄表还要回收一会儿），
	// 补一小段等待，避免紧接着的删除仍然撞上文件锁。
	time.Sleep(300 * time.Millisecond)
}

// cleanupSelfTempDir 删掉卸载副本自己所在的临时目录。
//
// 副本删不掉正在运行的自己，所以交给一个脱离的 cmd：等本进程退出后再 rmdir。
// 只在确实是我们自己的临时目录里才动手，避免误删。
//
// 两个细节都是实测踩出来的，不能说「看着对」：
//
//   - 用 SysProcAttr.CmdLine 而不是 exec.Command 传参数：Go 会把参数里**内嵌的
//     引号**反斜杠转义，而 cmd.exe 不认那种转义，于是那条 rmdir 静默不执行。
//   - 工作目录设在待删目录之外（见下）：Windows 不会删除仍是某进程当前目录的目录。
func cleanupSelfTempDir() {
	self, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(self)
	if !strings.HasPrefix(strings.ToLower(dir), strings.ToLower(os.TempDir())) {
		return
	}
	if !strings.HasPrefix(filepath.Base(dir), "wb-uninstall-") {
		return
	}

	// 延时用 ping（timeout 需要控制台，从隐藏进程里跑会立刻返回）。rmdir 要等副本
	// 进程完全退出、映像从文件系统脱开之后才能成功，所以先等够，失败再补一轮。
	// 这个删除只有一次机会 —— 失败就是目录永久留着。
	line := `/c ping -n 4 127.0.0.1 >nul & rmdir /s /q "` + dir + `" 2>nul & ` +
		`if exist "` + dir + `" (ping -n 4 127.0.0.1 >nul & rmdir /s /q "` + dir + `" 2>nul)`

	c := exec.Command("cmd.exe")
	c.Dir = os.TempDir()
	c.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess,
		CmdLine:       line,
	}
	_ = c.Start()
}

// copySelfExe 复制可执行文件。
func copySelfExe(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
