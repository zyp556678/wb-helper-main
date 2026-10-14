package main

// -----------------------------------------------------------------------------
// 安装 / 升级 / 卸载子命令
//
// 这三个命令是「统一三端安装器」的执行端：安装包只负责把文件铺到磁盘，
// 真正的安装/卸载语义在这里。这样三端（Windows 自解压包 / Linux deb / macOS dmg）
// 的**行为由同一份 Go 代码决定**，不再取决于各平台打包工具生成的模板。
//
// 为什么要从框架手里把这件事拿回来（以及原子切换的取舍），见 internal/installer
// 的包注释；这里只放命令行接线。
//
// 与 -auth / -port 那些通用选项的关系：安装命令**不读 config.json**，
// 也不加载凭据池 —— 它要在「账号池还没配好」甚至「网关还没跑起来」的时候就能用，
// 而那正是安装的常态。
// -----------------------------------------------------------------------------

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"workbuddy-gateway/internal/installer"
)

func runInstall(args []string)   { os.Exit(cmdInstall(args, os.Stdout, os.Stderr)) }
func runUninstall(args []string) { os.Exit(cmdUninstall(args, os.Stdout, os.Stderr)) }
func runUpgrade(args []string)   { os.Exit(cmdUpgrade(args, os.Stdout, os.Stderr)) }

// installFlags 是三个安装命令共用的选项集合。
type installFlags struct {
	src        string
	installDir string
	dataDir    string
	port       int
	autoStart  bool
	jsonOut    bool
	dropData   bool
	waitPID    int
	noRegister bool
}

// newInstallFlagSet 建一个只注册**通用**选项的解析器。
//
// `-drop-data` 只在卸载时有意义，但它也在这里注册：三个命令共用同一个结构体与
// 同一份解析代码，比给卸载单独写一套更不容易漂移（代价是一个安装时不生效的开关，
// 已在帮助文本里说明）。
func newInstallFlagSet(name string, dst *installFlags, errOut io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&dst.src, "src", "", "安装源目录（安装包解压后的载荷位置）；默认取本可执行文件所在目录")
	fs.StringVar(&dst.installDir, "dir", "", "安装目录；默认按平台取（Windows: %LOCALAPPDATA%\\WorkBuddy Gateway）")
	fs.StringVar(&dst.dataDir, "data-dir", "", "数据目录；默认按平台取（账号池所在地，不要改）")
	fs.IntVar(&dst.port, "port", 8317, "网关监听端口（写进快捷方式与自启项）")
	fs.BoolVar(&dst.autoStart, "autostart", false, "安装完成后注册开机自启")
	fs.BoolVar(&dst.jsonOut, "json", false, "以 JSON 输出结果（供面板解析）")
	fs.BoolVar(&dst.dropData, "drop-data", false, "卸载时**一并删除数据目录**（里面是真实凭据，默认保留）")
	fs.IntVar(&dst.waitPID, "wait-pid", 0, "内部使用：等这个 PID 退出后再继续（卸载转交副本时用）")
	fs.BoolVar(&dst.noRegister, "no-register", false,
		"不写系统注册信息（A/R 项、快捷方式、自启）——绿色版/便携安装用；安装与卸载都生效")
	return fs
}

// resolveInstallFlags 补齐未指定时的默认值。
//
// src 默认取**本可执行文件所在目录**：安装包把网关与载荷解到同一个临时目录，
// 然后调用它自己带 install 参数 —— 所以「我在哪」就是「载荷在哪」，
// 不需要调用方额外传路径（少一个能传错的东西）。
func resolveInstallFlags(f installFlags) (installFlags, error) {
	if f.installDir == "" {
		f.installDir = installer.DefaultInstallDir()
		if f.installDir == "" {
			return f, errors.New("本平台无法推导默认安装目录，请用 -dir 指定")
		}
	}
	if f.dataDir == "" {
		f.dataDir = installer.DefaultDataDir()
	}
	if f.src == "" {
		self, err := os.Executable()
		if err != nil {
			return f, fmt.Errorf("无法确定自身位置以推导安装源：%w", err)
		}
		// 解析符号链接：Linux 上 /proc/self/exe 常常指向 /usr/bin 下的软链，
		// 而载荷在真实位置旁边。
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		f.src = filepath.Dir(self)
	}
	return f, nil
}

func cmdInstall(args []string, out, errOut io.Writer) int {
	var f installFlags
	fs := newInstallFlagSet("install", &f, errOut)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	f, err := resolveInstallFlags(f)
	if err != nil {
		fmt.Fprintf(errOut, "参数错误: %v\n", err)
		return 2
	}

	logf := func(format string, a ...any) {
		if !f.jsonOut {
			fmt.Fprintf(out, format+"\n", a...)
		}
	}
	if !f.jsonOut {
		fmt.Fprintf(out, "WorkBuddy 安装器\n  源:       %s\n  安装到:   %s\n  数据目录: %s\n\n",
			f.src, f.installDir, f.dataDir)
	}

	res, err := installer.Install(installer.Options{
		SrcDir:     f.src,
		InstallDir: f.installDir,
		DataDir:    f.dataDir,
		Port:       f.port,
		AutoStart:  f.autoStart,
		KeepData:   !f.dropData,
		NoRegister: f.noRegister,
		Log:        logf,
	})
	if err != nil {
		fmt.Fprintf(errOut, "安装失败: %v\n", err)
		return 1
	}

	if f.jsonOut {
		_ = json.NewEncoder(out).Encode(res)
		return 0
	}
	fmt.Fprintf(out, "\n  版本:     v%s\n  面板:     http://127.0.0.1:%d/panel/\n", res.Version, f.port)
	if res.Adopted {
		fmt.Fprintf(out, "  （已接管旧安装：%s）\n", res.AdoptedFrom)
	}
	return 0
}

// cmdUpgrade 就是「带默认数据目录的安装」—— 停进程、原子切换、重新注册一套走完。
//
// 单独给一个命令名是为了让面板与脚本的**意图**可读（升级 = 保数据），
// 而不是因为逻辑有分别；真写出两套实现才是将来分叉的来源。
func cmdUpgrade(args []string, out, errOut io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprintln(out, "用法: workbuddy-gateway upgrade [-src 载荷目录] [-dir 安装目录]：升级到本包版本，保留数据目录")
			return 0
		}
	}
	return cmdInstall(args, out, errOut)
}

func cmdUninstall(args []string, out, errOut io.Writer) int {
	var f installFlags
	fs := newInstallFlagSet("uninstall", &f, errOut)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	f, err := resolveInstallFlags(f)
	if err != nil {
		fmt.Fprintf(errOut, "参数错误: %v\n", err)
		return 2
	}

	// 卸载器位于待删目录之内时，Windows 上删不掉自己（映像被锁），
	// 而 stopProcesses 的「按目录筛选」还会把正在卸载的自己一起杀掉。
	// 转交给临时目录里的副本执行，本进程退出。
	//
	// 判据是「自身是否在目标目录下」而不是某个显式开关：副本在 %TEMP% 里，
	// 自然不满足这个条件，于是同一条命令在两种位置下各自走对的分支 ——
	// 不需要额外传「我是副本」这种可以被误传的标记。
	if selfInsideDir(f.installDir) {
		return relaunchSelfForUninstall(f, out, errOut)
	}

	// 副本路径：等原进程真正退出，否则它的映像仍锁着目录里的文件。
	if f.waitPID > 0 {
		waitForProcessExit(f.waitPID)
	}

	logf := func(format string, a ...any) {
		if !f.jsonOut {
			fmt.Fprintf(out, format+"\n", a...)
		}
	}
	res, err := installer.Uninstall(installer.Options{
		InstallDir: f.installDir,
		DataDir:    f.dataDir,
		KeepData:   !f.dropData,
		NoRegister: f.noRegister,
		Log:        logf,
	})
	if err != nil {
		fmt.Fprintf(errOut, "卸载失败: %v\n", err)
		return 1
	}

	if f.jsonOut {
		_ = json.NewEncoder(out).Encode(res)
		return 0
	}
	fmt.Fprintf(out, "\n  已卸载: %s\n", res.InstallDir)
	if res.DataKept {
		fmt.Fprintf(out, "  数据目录已保留: %s\n  （里面有凭据与配置；确认不再需要时用 -drop-data 再跑一次）\n", f.dataDir)
	}
	// 副本收尾：删掉自己所在的临时目录。
	cleanupSelfTempDir()
	return 0
}
