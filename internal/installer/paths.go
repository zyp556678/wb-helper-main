package installer

import (
	"os"
	"path/filepath"
	"runtime"
)

// fileExists 报告路径存在且不是目录。
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// exeSuffix 返回当前平台的可执行文件后缀。
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// DefaultInstallDir 返回当前平台的默认安装目录。
//
// 与旧安装器保持一致（**不能改**）：Windows 是 NSIS 用的
// `%LOCALAPPDATA%\WorkBuddy Gateway`，macOS 是 `/Applications/WorkBuddy Gateway.app`。
// 改了就等于让已装用户失去升级路径 —— 新版本会落到别处，旧版本留在原地。
//
// Linux 返回空串：那种形态由 dpkg 拥有文件，安装目录不是我们能选的。
func DefaultInstallDir() string {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return ""
		}
		return base + `\WorkBuddy Gateway`
	case "darwin":
		return "/Applications/WorkBuddy Gateway.app"
	default:
		return ""
	}
}

// DefaultDataDir 返回当前平台的默认数据目录。
//
// 这是**账号池所在地**，三个入口（桌面版、命令行版、npm）共用同一份，
// 所以绝对不能因为安装方式变化而改变它。
func DefaultDataDir() string {
	if runtime.GOOS == "windows" {
		if base := os.Getenv("LOCALAPPDATA"); base != "" {
			return base + `\wb-gateway`
		}
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return home + "/.wb-gateway"
}

// readInstalledVersion 读已装网关二进制里的版本号。
//
// 平台无关：readBinaryVersion 在两端都有实现（Windows 走 runHidden，
// Unix 走 exec.Command），这里只是在 NoRegister 路径上把它接出来。
func readInstalledVersion(installDir string) string {
	return readBinaryVersion(filepath.Join(installDir, "bin", gatewayBinaryName()))
}

// filepathJoin 是 filepath.Join 的包内别名，让平台文件少写一层 import。
func filepathJoin(parts ...string) string {
	return filepath.Join(parts...)
}

// gatewayBinaryName 返回网关可执行文件名（不含目录）。
func gatewayBinaryName() string {
	return "workbuddy-gateway" + exeSuffix()
}
