//go:build windows

package installer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// adoptLegacy 发现并接管旧形态的安装。
//
// ## 为什么必须做
//
// 老用户机器上装着 NSIS 装的桌面版（注册表键带空格、安装目录由 NSIS 推导）。
// 如果不接管，新装会落在另一个目录，于是：
//   - 「设置 → 应用」里出现两个「WorkBuddy Gateway」
//   - 两个自启项抢 8317 端口（表现为「有时能连上、有时连不上」）
//   - 用户不知道哪个该删
//
// ## 做法
//
// 读旧键拿到 **安装目录并复用它**（不改落点），然后删掉旧键、旧自启残留。
// 目录里的文件不在这里删 —— 稍后的原子切换会整个删掉重建，那一步更彻底。
//
// ## 顺序约束
//
// 必须在注册新键**之前**做完，否则删除旧键可能连带删掉刚要写的东西；
// 也必须在停进程之前，因为复用的目标目录此时就要定下来。
func adoptLegacy(opt Options, logf func(string, ...any)) (legacyInstall, error) {
	dir := readRegValue(legacyManuKey, "")
	if strings.TrimSpace(dir) == "" {
		// 没有 NSIS 记录，再看 A/R 键的 InstallLocation（两种写法都可能缺）。
		dir = readRegValue(legacyUninstKey, "InstallLocation")
	}
	dir = strings.Trim(strings.TrimSpace(dir), `"`)
	if dir == "" {
		return legacyInstall{}, nil
	}

	// 只接管「确实存在且看着像我们的安装目录」的情况。
	// 注册表可能指向一个已被用户手工删掉的目录 —— 那时不该复用，
	// 否则会把新版本装到一个用户以为已经清掉的位置。
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		logf("旧注册表指向的目录不存在（%s），按全新安装处理", dir)
		cleanupLegacyRegistry(logf)
		return legacyInstall{}, nil
	}

	res := legacyInstall{Found: true, Dir: dir}
	cleanupLegacyRegistry(logf)
	return res, nil
}

// cleanupLegacyRegistry 删掉旧安装器写的注册表项。
//
// 三处都要删，缺一处就是残留：
//   - A/R 键：不删会在「设置 → 应用」里留下一个点了没反应的死条目
//     （它指向的旧卸载器已被原子切换删掉了）
//   - 厂商键：NSIS 用它记录安装位置，留着会让下一次接管误判
//   - Run 值：NSIS 卸载器尝试删的值名与它自己写的不符，所以一直没被清掉
func cleanupLegacyRegistry(logf func(string, ...any)) {
	for _, key := range []string{
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\WorkBuddy Gateway`,
		`HKCU\Software\workbuddy\WorkBuddy Gateway`,
		`HKCU\Software\workbuddy`,
	} {
		if regKeyExists(key) {
			logf("清理旧注册表项 %s", key)
			deleteRegKey(key)
		}
	}
	if regValueExists(`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "WorkBuddy Gateway") {
		logf("清理旧自启值 WorkBuddy Gateway")
		deleteRegValue(`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "WorkBuddy Gateway")
	}
}

// registerInstall 注册 A/R 项、快捷方式与（可选）自启，返回装上去的版本号。
func registerInstall(opt Options, logf func(string, ...any)) (string, error) {
	shellPath := filepath.Join(opt.InstallDir, "workbuddy-gateway-desktop.exe")
	gatewayPath := filepath.Join(opt.InstallDir, "bin", gatewayBinaryName())

	// 版本号从二进制自身读，不写死：写死的话升版本时忘了改就会出现
	// 「附件文件名是新版、A/R 里显示旧版」这种自相矛盾。
	version := readBinaryVersion(gatewayPath)
	logf("装上去的网关版本 %s", version)

	// ---- 快捷方式 ----
	if err := createShortcuts(opt, shellPath, logf); err != nil {
		return version, err
	}

	// ---- 自启 ----
	//
	// 只在用户要求时注册；**不主动删除**用户已有选择（AutoStart=false 时保持现状）。
	if opt.AutoStart {
		if err := enableAutoStart(shellPath, logf); err != nil {
			logf("注册开机自启失败（不影响安装）：%v", err)
		}
	}

	// ---- A/R 项 ----
	if err := writeUninstallEntry(opt, shellPath, version); err != nil {
		return version, err
	}
	return version, nil
}

// writeUninstallEntry 写 A/R 项。
//
// DisplayName 带「（桌面版）」后缀 —— 命令行版与桌面版共用 8317 端口，
// 两者在 A/R 里同名会让人分不清该卸载哪个（旧 NSIS 正是这个问题）。
func writeUninstallEntry(opt Options, shellPath, version string) error {
	uninst := filepath.Join(opt.InstallDir, "uninstall.cmd")
	if !fileExists(uninst) {
		// 没有独立卸载脚本时，用网关自己的 uninstall 子命令 —— 它本来就能做这件事。
		uninst = filepath.Join(opt.InstallDir, "bin", "workbuddy-gateway"+exeSuffix())
	}
	uninstStr := fmt.Sprintf(`"%s" uninstall --dir "%s"`, uninst, opt.InstallDir)

	icon := filepath.Join(opt.InstallDir, "app.ico")
	if !fileExists(icon) {
		icon = shellPath
	}

	values := map[string]string{
		"DisplayName":     "WorkBuddy Gateway（桌面版）",
		"DisplayVersion":  version,
		"Publisher":       "workbuddy-gateway",
		"InstallLocation": opt.InstallDir,
		"DisplayIcon":     icon,
		"UninstallString": uninstStr,
	}
	if err := writeRegKeyValues(arKey(), values); err != nil {
		return fmt.Errorf("写卸载项失败：%w", err)
	}
	return nil
}

// arKey 是新形态使用的 A/R 键。
//
// 沿用命令行版的键名（不带空格），这样两者的历史键位规整成一个：
// 桌面版接管时会删掉 NSIS 的带空格键，最终系统里只剩这一个。
func arKey() string {
	return `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\WorkBuddyGateway`
}

// createShortcuts 建开始菜单与桌面快捷方式，指向网关本体。
//
// 与命令行版不同，桌面版**不需要** run.cmd 之类的中转：壳自己就是那个窗口，
// 双击快捷方式等于启动壳，壳再去拉起网关。多一层批处理只会多一个黑窗口。
func createShortcuts(opt Options, shellPath string, logf func(string, ...any)) error {
	if !fileExists(shellPath) {
		return nil // 命令行形态没有壳，快捷方式由它自己的安装器负责
	}
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return nil
	}
	startMenu := filepath.Join(appData, `Microsoft\Windows\Start Menu\Programs`)

	targets := []string{
		filepath.Join(startMenu, "WorkBuddy Gateway.lnk"),
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		desktop := filepath.Join(home, "Desktop")
		if st, err := os.Stat(desktop); err == nil && st.IsDir() {
			targets = append(targets, filepath.Join(desktop, "WorkBuddy Gateway.lnk"))
		}
	}

	for _, lnk := range targets {
		if err := os.MkdirAll(filepath.Dir(lnk), 0o755); err != nil {
			continue
		}
		if err := createShortcut(lnk, shellPath, opt.InstallDir, opt.DataDir); err != nil {
			logf("建快捷方式失败（%s）：%v", filepath.Base(lnk), err)
		}
	}
	return nil
}

// readBinaryVersion 从二进制自身读出 `vX.Y.Z`。
//
// 读不到返回 "0.0.0" 而不是报错：A/R 里的版本号只是个展示字段，
// 不值得因为读不到它就让整个安装失败。
func readBinaryVersion(bin string) string {
	if !fileExists(bin) {
		return "0.0.0"
	}
	out, err := runHidden(bin, "version")
	if err != nil {
		return "0.0.0"
	}
	// 输出形如 `WorkBuddy Gateway v0.13.2`。
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "v") {
			return strings.TrimPrefix(field, "v")
		}
	}
	return "0.0.0"
}

// ---------------------------------------------------------------- 注册表薄封装

// 这些封装走 reg.exe 而不是 golang.org/x/sys/windows/registry：
// 项目现在只有一个外部依赖（modernc.org/sqlite），为了写几个键值再引一个不划算，
// 而 reg.exe 每个 Windows 都有、上面这套键位也都是 HKCU 下的个人设置。

func regKeyExists(key string) bool {
	_, err := runHidden("reg.exe", "query", key)
	return err == nil
}

func readRegValue(key, name string) string {
	args := []string{"query", key}
	if name != "" {
		args = append(args, "/v", name)
	}
	out, err := runHidden("reg.exe", args...)
	if err != nil {
		return ""
	}
	return parseRegValue(out, name)
}

// parseRegValue 从 `reg query` 的输出里取值。
//
// 输出形如：
//
//	HKEY_CURRENT_USER\...\Run
//	    WorkBuddyGatewayDesktop    REG_SZ    "C:\...\shell.exe" --silent
//
// 按空白切成三段，第三段是值（可能含空格，所以要 Join 回剩余的）。
func parseRegValue(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if name != "" && !strings.EqualFold(fields[0], name) {
			continue
		}
		// fields[1] 是类型（REG_SZ / REG_EXPAND_SZ / REG_DWORD）
		if !strings.HasPrefix(strings.ToUpper(fields[1]), "REG_") {
			continue
		}
		return strings.Join(fields[2:], " ")
	}
	return ""
}

func regValueExists(key, name string) bool {
	return readRegValue(key, name) != ""
}

func deleteRegKey(key string) {
	_, _ = runHidden("reg.exe", "delete", key, "/f")
}

func deleteRegValue(key, name string) {
	_, _ = runHidden("reg.exe", "delete", key, "/v", name, "/f")
}

// writeRegKeyValues 批量写一个键下的字符串值。
func writeRegKeyValues(key string, values map[string]string) error {
	for name, val := range values {
		if _, err := runHidden("reg.exe", "add", key, "/v", name, "/t", "REG_SZ", "/d", val, "/f"); err != nil {
			return fmt.Errorf("写 %s\\%s 失败：%w", key, name, err)
		}
	}
	return nil
}

// createShortcut 用 PowerShell 的 WScript.Shell 建 .lnk。
func createShortcut(lnk, target, workDir, dataDir string) error {
	script := fmt.Sprintf(
		`$w = New-Object -ComObject WScript.Shell; `+
			`$s = $w.CreateShortcut('%s'); `+
			`$s.TargetPath = '%s'; $s.WorkingDirectory = '%s'; `+
			`$s.Description = 'WorkBuddy Gateway'; $s.Save()`,
		psQuote(lnk), psQuote(target), psQuote(workDir))
	_, err := runHidden("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return err
	}
	// 数据目录靠环境变量传给壳，快捷方式里不需要额外参数（见 backend.rs 读
	// WB_GATEWAY_DATA_DIR）。这里只在有自定义数据目录时设一次用户级环境变量，
	// 避免每次启动都要用户配。
	if strings.TrimSpace(dataDir) != "" {
		setUserEnv("WB_GATEWAY_DATA_DIR", dataDir)
	}
	return nil
}

// setUserEnv 设一个用户级环境变量（失败静默）。
func setUserEnv(name, value string) {
	_, _ = runHidden("setx.exe", name, value)
}

// psQuote 把字符串转义成 PowerShell 单引号字面量里的安全形式。
func psQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// unregisterInstall 清理安装期写下的注册信息（卸载路径）。
//
// ⚠️ 必须清理自启项：旧版 `installer/windows/uninstall.ps1` 从不删它，
// 于是卸载后 `HKCU\...\Run\WorkBuddyGatewayDesktop` 还留着，开机时 Windows 会
// 尝试启动一个已经不存在的程序（CLI 版的 launch-hidden.vbs）。这在事件查看器里
// 表现为每次登录一条错误，用户根本不会联想到是卸载残留。
func unregisterInstall(opt Options, logf func(string, ...any)) {
	// 自启项：走网关自己的 autostart 能力，确保与面板/托盘操作的是**同一条记录**
	// （值名与落点的唯一定义在 internal/autostart，这里不复制一份）。
	if gateway := filepath.Join(opt.InstallDir, "bin", gatewayBinaryName()); fileExists(gateway) {
		if _, err := runHidden(gateway, "autostart", "off"); err != nil {
			// 二进制已经不在了或调用失败时兜底直接删 —— 卸载流程不能因为
			// 「清理自启」这一步而半途而废。
			deleteRegValue(`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "WorkBuddyGatewayDesktop")
		}
	} else {
		deleteRegValue(`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "WorkBuddyGatewayDesktop")
	}

	if regKeyExists(arKey()) {
		logf("移除卸载项")
		deleteRegKey(arKey())
	}
	// 顺带清掉旧形态的键，避免「先装新版、再卸载」之后旧键又冒出来。
	cleanupLegacyRegistry(logf)

	// 快捷方式。
	appData := os.Getenv("APPDATA")
	if appData != "" {
		_ = os.Remove(filepath.Join(appData, `Microsoft\Windows\Start Menu\Programs`, "WorkBuddy Gateway.lnk"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		_ = os.Remove(filepath.Join(home, "Desktop", "WorkBuddy Gateway.lnk"))
	}
}

// enableAutoStart 注册开机自启。
//
// 直接调网关自己的 autostart 能力（`workbuddy-gateway autostart on`），
// 而不是在这里重写一遍落点逻辑 —— 面板开关、托盘开关、安装器三条路必须操作
// **同一条记录**，否则会出现「安装器写了一个、面板说没开」这种解释不清的状态。
// 落点的唯一定义在 internal/autostart。
func enableAutoStart(gatewayPath string, logf func(string, ...any)) error {
	if !fileExists(gatewayPath) {
		return errors.New("网关二进制不存在")
	}
	out, err := runHidden(gatewayPath, "autostart", "on")
	if err != nil {
		return fmt.Errorf("%v（输出：%s）", err, strings.TrimSpace(out))
	}
	logf("已注册开机自启")
	return nil
}
