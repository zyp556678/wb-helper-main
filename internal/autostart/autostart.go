// Package autostart 管理「开机自启动」这一系统级设置。
//
// # 为什么不放在桌面壳里
//
// 自启项（Windows 注册表 / macOS plist / Linux .desktop）是**系统级事实**，
// 任何进程都能读写。把它做进 Go 网关而不是 Tauri 壳，换来三件事：
//
//  1. 面板在任何部署形态下语义一致 —— 桌面版、命令行版、Docker 里看到的
//     都是同一份真实状态，不会出现「桌面版面板说开着、命令行版面板说关着」。
//  2. 托盘开关和面板开关操作的是同一个落点，天然同步，不需要壳↔网关的新通道。
//  3. 命令行安装包既有的 `-AutoStart`（计划任务）与这里的读写能对上同一份记录。
//
// # 唯一需要判断的事：让谁来开机启动
//
// 网关自己不能被写成自启项 —— 它需要宿主（桌面壳）来管生命周期、开窗口、驻托盘。
// 直接自启 `workbuddy-gateway.exe serve` 会得到一个没有界面、用户无处关闭的后台进程。
//
// 所以自启项指向的是**宿主**：
//
//   - 桌面壳启动的网关（环境变量 `WB_GATEWAY_PARENT_WATCH=1`）→ 指向桌面壳可执行文件
//   - 命令行/安装包启动的网关 → 指向安装目录下的 `launch-hidden.vbs`
//
// 桌面壳的路径从**父进程**取（见 hostBinary），拿不到就如实报错而不是写一个错的路径。
package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Kind 是自启项的宿主类型。
type Kind string

const (
	// KindDesktop 桌面壳托管（Tauri 应用）。
	KindDesktop Kind = "desktop"
	// KindCLI 命令行安装包托管（计划任务 + launch-hidden.vbs）。
	KindCLI Kind = "cli"
	// KindUnknown 无法判定宿主，此时拒绝写入自启项。
	KindUnknown Kind = "unknown"
)

// Windows 注册表位置与值名。
const (
	winRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	// winValue 是**本开关**唯一使用的自启值名。
	//
	// 只有一个值名，无论宿主是桌面壳还是 launch-hidden.vbs —— 面板开关和托盘开关
	// 操作的是同一条记录，不存在「两处都开了、两个自启项抢端口」的状态。
	// 这一点很关键：两个值名看着更"干净"，实际会制造出开关管不住的那一份。
	//
	// 与命令行安装包的**计划任务** `WorkBuddyGateway` 是两套机制，刻意错开命名，
	// 卸载其中一个不会误伤另一个。正因为错开，才必须在启用前做冲突检测。
	winValue = "WorkBuddyGatewayDesktop"
	// cliTaskName 是命令行安装包用的计划任务名（installer/windows/install.ps1）。
	cliTaskName = "WorkBuddyGateway"
)

// macAgentLabel 是桌面版 LaunchAgent 的标签。
const macAgentLabel = "com.workbuddy.gateway.desktop"

// Status 是一次自启状态查询的结果。
type Status struct {
	// Supported 表示当前平台已实现自启管理。
	Supported bool `json:"supported"`
	// Enabled 表示系统里确实存在我们写的自启项。
	Enabled bool `json:"enabled"`
	// Kind 是自启项的宿主类型（desktop / cli / unknown）。
	Kind Kind `json:"kind"`
	// Host 是自启项里记录的可执行文件路径（便于用户核对）。
	Host string `json:"host,omitempty"`
	// Location 是自启项所在的系统位置（注册表键 / plist 路径 / .desktop 路径）。
	// 面板把它显示出来，用户想手动改时知道该去哪。
	Location string `json:"location,omitempty"`
	// Conflict 表示检测到**另一形态**的自启项会占用同一端口。
	//
	// 桌面版和命令行版共用 8317 端口，两个同时自启必然有一个起不来，
	// 且表现为「有时能连上、有时连不上」这种最难排查的故障。所以不只是提示，
	// 开启前要拦住（见 Enable）。
	Conflict *Conflict `json:"conflict,omitempty"`
	// Detail 是给用户看的一句话说明（失败原因 / 当前形态）。
	Detail string `json:"detail,omitempty"`
}

// Conflict 描述一次端口冲突。
type Conflict struct {
	// Kind 是冲突方的宿主类型。
	Kind Kind `json:"kind"`
	// Name 是冲突方在系统里的名字（计划任务名 / 值名）。
	Name string `json:"name"`
	// Message 是给用户看的说明。
	Message string `json:"message"`
}

// Enable 写入自启项，指向当前网关的宿主。
//
// 返回的 Status 是**写入后重新查询**的真实状态 —— 调用方据此渲染，不必自己推断。
func Enable() (Status, error) {
	host, kind := hostBinary()
	if kind == KindUnknown {
		return Query(), errors.New(
			"无法确定应该自启哪个程序：请从桌面版设置，或使用安装包自带的开机自启选项")
	}
	if c := detectConflict(kind); c != nil {
		st := Query()
		st.Conflict = c
		return st, fmt.Errorf("%s", c.Message)
	}

	var err error
	switch runtime.GOOS {
	case "windows":
		err = winWrite(winValue, host)
	case "darwin":
		err = macWrite(host)
	default:
		err = linuxWrite(host)
	}
	if err != nil {
		return Query(), err
	}
	return Query(), nil
}

// Disable 移除自启项。幂等：本来就没有也算成功。
func Disable() (Status, error) {
	var err error
	switch runtime.GOOS {
	case "windows":
		err = winDelete(winValue)
	case "darwin":
		err = macDelete()
	default:
		err = linuxDelete()
	}
	if err != nil {
		return Query(), err
	}
	return Query(), nil
}

// Query 读取当前自启状态，不修改任何东西。
func Query() Status {
	switch runtime.GOOS {
	case "windows":
		return winQuery()
	case "darwin":
		return macQuery()
	default:
		return linuxQuery()
	}
}

// ---------------------------------------------------------------------------
// 宿主判定
// ---------------------------------------------------------------------------

// hostBinary 判断自启项应该指向哪个可执行文件。
//
// 判定顺序：
//  1. `WB_GATEWAY_PARENT_WATCH=1`（桌面壳拉起的网关，见 desktop/backend.rs 的
//     spawn_gateway）→ 取父进程的可执行文件路径
//  2. 网关同目录（或上一级）存在桌面壳可执行文件 → 用它
//  3. 落到命令行形态：安装目录下的 launch-hidden.vbs
//
// 为什么优先取父进程而不是「找同目录下有没有桌面壳」：一个用户可能同时装了
// 桌面版和命令行版，同目录探测会选错；父进程是**确凿的运行时事实**。
func hostBinary() (string, Kind) {
	return resolveHost(hostProbes{
		parentWatched: os.Getenv("WB_GATEWAY_PARENT_WATCH") == "1",
		parent:        parentExecutable,
		desktopShell:  findDesktopShell,
		launchScript:  findLaunchScript,
	})
}

// hostProbes 把判定所需的三个外部查询收成一组，便于测试注入。
//
// 不直接把 hostBinary 写成读全局状态的样子：这三件事（读环境变量、查父进程、
// 扫文件系统）在测试里都很难造，而它们恰恰是「自启项指错程序」这类事故的来源。
type hostProbes struct {
	parentWatched bool
	parent        func() (string, error)
	desktopShell  func() string
	launchScript  func() string
}

// resolveHost 是 hostBinary 的纯逻辑部分。
func resolveHost(p hostProbes) (string, Kind) {
	if p.parentWatched {
		if exe, err := p.parent(); err == nil && exe != "" {
			return exe, KindDesktop
		}
	}
	if exe := p.desktopShell(); exe != "" {
		return exe, KindDesktop
	}
	if script := p.launchScript(); script != "" {
		return script, KindCLI
	}
	return "", KindUnknown
}

// findDesktopShell 在网关可执行文件附近找桌面壳。
//
// 布局（见 desktop/scripts/prepare-resources.mjs）：安装后网关在
// `<install>/bin/`，桌面壳在 `<install>/`。因此先看上一级，再看同级。
func findDesktopShell() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(self)
	for _, cand := range desktopShellNames() {
		for _, base := range []string{dir, filepath.Dir(dir)} {
			p := filepath.Join(base, cand)
			if isFile(p) {
				return p
			}
		}
	}
	return ""
}

func desktopShellNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"workbuddy-gateway-desktop.exe"}
	}
	return []string{"workbuddy-gateway-desktop"}
}

// findLaunchScript 找命令行安装包留下的隐藏启动脚本。
func findLaunchScript() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(self)
	// 安装目录下直接放；也看上一级（网关在 bin/ 里的布局）。
	for _, base := range []string{dir, filepath.Dir(dir)} {
		p := filepath.Join(base, "launch-hidden.vbs")
		if isFile(p) {
			return p
		}
	}
	return ""
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// ---------------------------------------------------------------------------
// Windows
// ---------------------------------------------------------------------------

// kindOfHost 从自启项记录的可执行文件路径反推宿主形态。
//
// 因为只用一个值名（见 winValue 的注释），形态信息不在值名里，而在路径里：
// launch-hidden.vbs 是命令行安装包的启动器，其余按桌面壳算。
func kindOfHost(host string) Kind {
	if strings.Contains(strings.ToLower(host), "launch-hidden") {
		return KindCLI
	}
	if strings.TrimSpace(host) == "" {
		return KindUnknown
	}
	return KindDesktop
}

func winQuery() Status {
	st := Status{Supported: true, Location: winRunKey}
	path, ok := winRead(winValue)
	if !ok {
		return st
	}
	st.Enabled = true
	st.Host = path
	st.Kind = kindOfHost(path)
	if c := detectConflict(st.Kind); c != nil {
		st.Conflict = c
	}
	return st
}

// winRead 读一个自启值，返回它记录的路径。
func winRead(name string) (string, bool) {
	out, err := runHidden("reg", "query", winRunKey, "/v", name)
	if err != nil {
		return "", false
	}
	// 输出形如：`    WorkBuddyGatewayDesktop    REG_SZ    "C:\...\app.exe"`
	return parseRegValue(out, name), true
}

func parseRegValue(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		// 名称后必须紧跟空白才算这个值。
		//
		// 只用 HasPrefix 会让 `WorkBuddyGatewayDesktopOld` 也命中 —— reg query
		// 精确查询时不会出现这种行，但解析函数不该依赖调用方保证输入干净。
		if !strings.HasPrefix(trimmed, name) {
			continue
		}
		rest := trimmed[len(name):]
		if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		// 类型之后的部分整体是值（用 Fields 会在含空格的路径上切碎）。
		idx := strings.Index(rest, "REG_SZ")
		if idx < 0 {
			continue
		}
		return strings.Trim(strings.TrimSpace(rest[idx+len("REG_SZ"):]), `"`)
	}
	return ""
}

func winWrite(name, host string) error {
	// 含空格的路径必须带引号，否则登录时会被拆成「程序 + 参数」。
	value := `"` + host + `"`
	_, err := runHidden("reg", "add", winRunKey, "/v", name, "/t", "REG_SZ", "/d", value, "/f")
	return err
}

func winDelete(name string) error {
	// 值不存在时 reg delete 返回非零 —— 那是正常情况，不算失败。
	if _, err := runHidden("reg", "delete", winRunKey, "/v", name, "/f"); err != nil {
		if _, ok := winRead(name); !ok {
			return nil
		}
		return err
	}
	return nil
}

// detectConflict 检查另一种形态是否已经设置了自启（两者抢同一端口）。
func detectConflict(self Kind) *Conflict {
	if runtime.GOOS != "windows" {
		return nil
	}
	return resolveConflict(self, conflictProbes{
		taskPresent: taskExists,
	})
}

// conflictProbes 把冲突检测要查的事收起来，便于测试注入。
//
// 查的是真实系统状态（计划任务），在 CI/开发机上造不出来；而「安装包已经设了自启
// 时会不会漏报」正是这段逻辑唯一值得测的地方。
type conflictProbes struct {
	taskPresent func(name string) bool
}

// resolveConflict 是 detectConflict 的纯逻辑部分。
//
// 因为本开关只用一个注册表值名（见 winValue），注册表侧不存在「自己和自己
// 冲突」的可能。冲突的**唯一**来源是命令行安装包的计划任务 —— 它也会在登录时
// 拉起一个网关，和我们的自启项抢 8317 端口。
func resolveConflict(self Kind, p conflictProbes) *Conflict {
	// 自己就是注册表值，计划任务一旦存在就必然冲突，与 self 是桌面版还是
	// 命令行版无关（两者最终都会去占 8317）。
	_ = self
	if p.taskPresent(cliTaskName) {
		return &Conflict{Kind: KindCLI, Name: cliTaskName,
			Message: "安装包已设置开机自启（计划任务 " + cliTaskName + "）。" +
				"两者共用 8317 端口，同时自启会互相抢端口。请先关闭其中之一。"}
	}
	return nil
}

func taskExists(name string) bool {
	_, err := runHidden("schtasks", "/Query", "/TN", name)
	return err == nil
}

// ---------------------------------------------------------------------------
// macOS
// ---------------------------------------------------------------------------

func macPlistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", macAgentLabel+".plist")
}

func macQuery() Status {
	p := macPlistPath()
	st := Status{Supported: true, Location: p}
	if !isFile(p) {
		return st
	}
	st.Enabled = true
	st.Kind = KindDesktop
	// plist 里没有单独的 Host 字段可读时留空 —— 面板会退化成只显示「已开启」。
	st.Host = p
	return st
}

func macWrite(host string) error {
	p := macPlistPath()
	if p == "" {
		return errors.New("无法定位 ~/Library/LaunchAgents")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// RunAtLoad 让登录时自动拉起。**不加** KeepAlive：壳自己管着网关子进程，
	// 若让 launchd 守护「壳」，用户从托盘点「退出」后会被立刻拉起来。
	body := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + macAgentLabel + `</string>
    <key>ProgramArguments</key>
    <array>
        <string>` + host + `</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return err
	}
	// 立即装载，不重登也生效。先 bootout 是因为 plist 已存在时 bootstrap 会报错。
	uid := fmt.Sprint(os.Getuid())
	_, _ = runHidden("launchctl", "bootout", "gui/"+uid, p)
	if _, err := runHidden("launchctl", "bootstrap", "gui/"+uid, p); err != nil {
		// 写文件已成功，装载失败只影响「本次是否立刻生效」，下次登录仍会生效。
		// 因此不当成失败返回。
		return nil
	}
	return nil
}

func macDelete() error {
	p := macPlistPath()
	if p == "" {
		return errors.New("无法定位 ~/Library/LaunchAgents")
	}
	uid := fmt.Sprint(os.Getuid())
	_, _ = runHidden("launchctl", "bootout", "gui/"+uid, p)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Linux
// ---------------------------------------------------------------------------

func linuxDesktopPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if strings.TrimSpace(base) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "autostart", "workbuddy-gateway-desktop.desktop")
}

func linuxQuery() Status {
	p := linuxDesktopPath()
	st := Status{Supported: true, Location: p}
	if isFile(p) {
		st.Enabled = true
		st.Kind = KindDesktop
		st.Host = p
	}
	return st
}

func linuxWrite(host string) error {
	p := linuxDesktopPath()
	if p == "" {
		return errors.New("无法定位 XDG autostart 目录")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	body := "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=WorkBuddy 网关\n" +
		"Comment=开机自动启动 WorkBuddy 网关\n" +
		"Exec=" + host + "\n" +
		"Terminal=false\n" +
		"X-GNOME-Autostart-enabled=true\n"
	return os.WriteFile(p, []byte(body), 0o644)
}

func linuxDelete() error {
	p := linuxDesktopPath()
	if p == "" {
		return errors.New("无法定位 XDG autostart 目录")
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// 进程工具
// ---------------------------------------------------------------------------

// runHidden 跑一条命令并返回 stdout（失败时附 stderr 便于排查），不弹窗口。
func runHidden(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	prepareHidden(cmd)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			return "", err
		}
		return stdout.String(), fmt.Errorf("%s: %s", name, msg)
	}
	return stdout.String(), nil
}
