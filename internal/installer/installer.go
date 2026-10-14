// Package installer 实现三端统一的安装 / 升级 / 卸载逻辑。
//
// ## 它取代了什么
//
// 此前安装语义由 Tauri 生成的打包产物决定（Windows 的 NSIS、Linux 的 deb 维护脚本、
// macOS 的 dmg），我们只能挂钩子或换整套模板，改不动那一行。最直接的后果是
// Windows 上「升级时选安装前卸载」必然弹「无法卸载！」并中止：NSIS 模板在跑完
// **旧版本**的卸载器之后，用 `${FileExists} "$INSTDIR\工作程序.exe"` 判定失败 ——
// 而旧卸载器永远删不掉被运行中进程锁住的那个文件，所以这条岔路从设计上就走不通。
//
// 现在把逻辑收回来：真正的安装/卸载由 `workbuddy-gateway install|uninstall|upgrade`
// 三个子命令完成，安装包退化成「把文件铺到磁盘」的搬运工，零逻辑。好处是三端
// **同一份实现**、可 `go test`、改动不再依赖任何打包工具链。
//
// ## 原子切换（本包的核心不变量）
//
// 安装不是「把新文件覆盖上去」，而是：
//
//	1. 解包到 <目标>.new\
//	2. 删掉整个 <目标>\
//	3. rename(<目标>.new → <目标>)
//
// 为什么不原地覆盖：覆盖**只写同名文件**，某一版曾经有、后来删掉的文件会永久留在
// 安装目录里（新版清单里没有它，就没有任何一步会去删它）。原子切换让「装完的目录
// 内容 == 这一版的清单」成为结构性保证，而不是靠维护一份会漂移的文件清单。
//
// 代价是安装期间有一个短暂窗口，<目标> 不存在 —— 所以必须先停掉所有占用进程
// （见 stopProcesses），否则第 2 步会因文件被锁而失败，留下「旧的删了一半、
// 新的还没换上」的现场。
//
// ## 平台差异（唯一无法消除的部分）
//
// Windows 的安装目录是一堆平铺的文件，删掉重建即可；macOS 的 .app 是一个 **bundle
// 目录**，同样可用 rename 原子替换（见 swapDarwin）；Linux 走包管理器，文件由 dpkg
// 拥有，本包只负责在维护脚本里完成「停进程 → 落盘 → 注册」这几步。
package installer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Options 是一次安装的参数。
type Options struct {
	// SrcDir 是解包后的源目录（安装包把载荷解到哪里）。
	SrcDir string
	// InstallDir 是安装目标目录。
	InstallDir string
	// DataDir 是数据目录（凭据、配置、统计）。
	DataDir string
	// Port 是网关监听端口，写进快捷方式与自启项。
	Port int
	// AutoStart 表示安装完成后注册开机自启。
	AutoStart bool
	// KeepData 表示卸载时保留数据目录（默认 true：里面是真实凭据）。
	KeepData bool
	// NoRegister 跳过「注册」这一步（A/R 项、快捷方式、自启）。
	//
	// 两种用途：
	//   - 绿色版 / 便携安装（`-no-register`）：把文件铺到指定目录就结束，不往系统里
	//     写任何东西 —— 解压即用、删目录即卸载，这是「免安装压缩包」那类需求。
	//   - 测试：注册会写**真实的**系统位置（开始菜单、注册表、LaunchAgent），
	//     而单元测试必须能在任何机器上反复跑而不留痕迹 —— 测试里连装两次会在用户的
	//     开始菜单里留下快捷方式，那不是「测试通过了」而是「弄脏了用户的机器」。
	//     端到端验证同理：要能在一个临时目录里走完整流程，而不覆盖用户真实的
	//     A/R 项与快捷方式。
	NoRegister bool
	// TestGuard 是一道**硬护栏**：设成非空时，任何会改动系统状态的步骤都要求
	// 目标目录位于这个前缀之下，否则直接报错终止。
	//
	// 为什么需要它（这是踩过坑之后加的）：测试里的隔离此前只靠「每个调用都传
	// 正确的临时目录」这一约定，而约定的另一头 —— `adoptLegacy` 读注册表 ——
	// 能把传进来的目录悄悄换成用户真实的安装目录，于是 `go test` 把开发机上
	// 装好的三个可执行文件覆盖成了几字节的假数据，还顺手删掉了卸载注册表项。
	// 一次「跑个测试」不该有这种能力。
	//
	// 测试传 `t.TempDir()`，生产传空（不设即无限制）。
	TestGuard string
	// Log 是安装过程的输出回调（nil 时静默）。安装器把它接到 stdout 让用户看得见。
	Log func(format string, args ...any)
}

// guardTarget 在 TestGuard 生效时校验目标目录在其之下。
//
// 单独抽出来是为了让 Install 与 Uninstall **共用同一道判断** ——
// 两个入口都能删目录，只给其中一个加护栏等于没加。
func (o Options) guardTarget(dir string) error {
	if o.TestGuard == "" {
		return nil
	}
	clean := filepath.Clean(dir)
	guard := filepath.Clean(o.TestGuard)
	rel, err := filepath.Rel(guard, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf(
			"拒绝在测试护栏之外操作：目标目录 %q 不在允许的 %q 之下。"+
				"这通常意味着某个步骤把目标目录换成了真实路径 —— "+
				"先查 adoptLegacy 之类的「读系统状态决定落点」逻辑", clean, guard)
	}
	return nil
}

// Result 是一次安装的结果。
type Result struct {
	// InstallDir 是最终安装目录。
	InstallDir string `json:"install_dir"`
	// Version 是装上去的网关版本（从二进制自身读出，不写死）。
	Version string `json:"version"`
	// Adopted 表示这次是**接管**一个旧形态的安装（NSIS / 命令行版），而不是全新安装。
	Adopted bool `json:"adopted"`
	// AdoptedFrom 是接管前的安装目录（Adopted 为真时有意义）。
	AdoptedFrom string `json:"adopted_from,omitempty"`
	// Stopped 是本次被结束掉的进程数。
	Stopped int `json:"stopped"`
	// Removed 是原子切换时删掉的旧目录（为空表示全新安装）。
	Removed string `json:"removed,omitempty"`
}

// logger 把 nil 的 Log 归一成空操作，省掉每个调用点的判空。
func (o Options) logger() func(string, ...any) {
	if o.Log == nil {
		return func(string, ...any) {}
	}
	return o.Log
}

// legacyInstall 描述一次被发现的旧形态安装（平台实现见 adoptLegacy）。
type legacyInstall struct {
	// Found 表示确实发现并接管了一个旧安装。
	Found bool
	// Dir 是旧安装的目录。Install 会复用它作为目标目录 ——
	// 不复用会让老用户机器上出现第二个安装目录与重复的 A/R 条目。
	Dir string
}

// Install 执行一次完整安装：定目标目录 → 停进程 → 原子切换 → 注册。
//
// ## 目标目录怎么定（这里有个**曾经踩过的坑**）
//
// 优先级是「显式指定 > 接管旧安装 > 平台默认」，三者互斥：
//
//   - `opt.InstallDir` 非空 = 调用方明确指定了落点，**必须原样使用**。
//     早先的实现无条件让「接管旧安装」覆盖它，于是测试传进来的临时目录被
//     注册表里的真实路径顶掉，安装整个打到用户的真实安装目录上 ——
//     一行 `if adopted.Dir != "" { opt.InstallDir = adopted.Dir }` 造成的。
//     生产路径上这同样是 bug：用户说装到 D 盘，却被装回了 C 盘。
//   - 为空时才去读注册表、复用旧落点（这才是「平滑接管」该起作用的地方）。
//   - 都没有才回退平台默认目录。
//
// 因此 `adoptLegacy` **只在目录为空时**调用：那个函数会读真实注册表并删旧键，
// 在「调用方已经指明落点」的场景下既没必要，也会误删用户可能还想留着的旧安装记录。
func Install(opt Options) (Result, error) {
	if strings.TrimSpace(opt.SrcDir) == "" {
		return Result{}, errors.New("缺少安装源目录")
	}
	logf := opt.logger()

	var res Result

	// ---- 1. 定目标目录 ----
	explicit := strings.TrimSpace(opt.InstallDir) != ""
	// NoRegister（绿色版）不接管旧安装：接管会读注册表、删旧键、删旧计划任务，
	// 而绿色版的承诺是「只把文件铺到指定目录，不碰系统里任何东西」。
	// 调用方要么显式给了 -dir，要么就该落到平台默认目录 —— 让 adoptLegacy 去改
	// 落点，等于把它装到用户那条正规安装的头上。
	if !explicit && !opt.NoRegister {
		// 只在调用方没指定时才去接管旧安装。
		adopted, err := adoptLegacy(opt, logf)
		if err != nil {
			return Result{}, err
		}
		res.Adopted = adopted.Found
		res.AdoptedFrom = adopted.Dir
		if adopted.Dir != "" {
			// 复用旧落点，避免老用户机器上出现第二个安装目录与重复的 A/R 条目。
			opt.InstallDir = adopted.Dir
			logf("接管旧安装：%s", adopted.Dir)
		}
	}
	if strings.TrimSpace(opt.InstallDir) == "" {
		opt.InstallDir = DefaultInstallDir()
	}
	if strings.TrimSpace(opt.InstallDir) == "" {
		return Result{}, errors.New("无法确定安装目录：本平台没有默认值，请用 -dir 指定")
	}
	// 定完目标目录立刻校验：护栏要挡在所有写操作**之前**。
	if err := opt.guardTarget(opt.InstallDir); err != nil {
		return Result{}, err
	}

	// ---- 2. 停掉占用进程并等锁释放 ----
	stopped, err := stopProcesses(opt.InstallDir, opt.DataDir, logf)
	res.Stopped = stopped
	if err != nil {
		return Result{}, err
	}

	// ---- 3. 原子切换 ----
	removed, err := swap(opt, logf)
	res.Removed = removed
	if err != nil {
		return Result{}, err
	}

	// ---- 4. 注册（A/R 项、自启、快捷方式）----
	res.InstallDir = opt.InstallDir
	if opt.NoRegister {
		// 测试路径：注册会写真实的系统位置，跳过它才能让测试在任何机器上反复跑。
		res.Version = readInstalledVersion(opt.InstallDir)
		return res, nil
	}
	version, err := registerInstall(opt, logf)
	if err != nil {
		return Result{}, err
	}
	res.Version = version

	logf("安装完成：%s（v%s）", opt.InstallDir, res.Version)
	return res, nil
}

// UninstallResult 是一次卸载的结果。
type UninstallResult struct {
	// InstallDir 是被卸载的目录。
	InstallDir string `json:"install_dir"`
	// DataKept 表示数据目录被保留（默认行为：里面有真实凭据）。
	DataKept bool `json:"data_kept"`
	// Stopped 是本次被结束掉的进程数。
	Stopped int `json:"stopped"`
}

// Uninstall 卸载：停进程 → 删安装目录 → 清注册 → （可选）删数据目录。
//
// ## 数据目录默认保留
//
// 它装着 `workbuddy*.json` —— 真实的上游访问凭据。卸载程序**无权**替用户决定
// 删掉这些东西（重装一次就能用回来，删了却不可恢复）。所以默认保留，
// 只在调用方显式要求时才删。
//
// ## 为什么先停进程
//
// 直接删一个正在运行的程序的目录，在 Windows 上会失败（文件被锁），
// 在 Unix 上会「成功」但留下一个仍在运行的孤儿进程。两种都不是卸载该有的样子。
func Uninstall(opt Options) (UninstallResult, error) {
	if strings.TrimSpace(opt.InstallDir) == "" {
		return UninstallResult{}, errors.New("缺少安装目录")
	}
	// 与 Install 同一道护栏：卸载同样会删目录，只给安装加等于没加。
	if err := opt.guardTarget(opt.InstallDir); err != nil {
		return UninstallResult{}, err
	}
	// 数据目录也要挡：`-drop-data` 会把它整个删掉，而那是凭据所在地。
	// 这里允许数据目录为**空**（表示不处理），但非空时必须同样在护栏内。
	if strings.TrimSpace(opt.DataDir) != "" {
		if err := opt.guardTarget(opt.DataDir); err != nil {
			return UninstallResult{}, err
		}
	}
	logf := opt.logger()
	res := UninstallResult{InstallDir: opt.InstallDir}

	stopped, err := stopProcesses(opt.InstallDir, opt.DataDir, logf)
	res.Stopped = stopped
	if err != nil {
		return res, err
	}

	// 清注册（A/R 项、自启、快捷方式）必须在删目录**之前**：
	// 那些信息有一部分（卸载脚本路径）指向目录里的文件，目录没了就无从得知
	// 该删哪些键。
	//
	// NoRegister 时跳过：绿色版安装没写过系统注册信息，去删等于误删**另一个**
	// 正规安装的 A/R 项与快捷方式。
	if !opt.NoRegister {
		unregisterInstall(opt, logf)
	}

	logf("删除安装目录 %s", opt.InstallDir)
	if err := os.RemoveAll(opt.InstallDir); err != nil {
		return res, fmt.Errorf("删除安装目录失败：%w", err)
	}

	if opt.KeepData {
		res.DataKept = true
		if opt.DataDir != "" {
			logf("数据目录已保留：%s", opt.DataDir)
		}
	} else if strings.TrimSpace(opt.DataDir) != "" {
		logf("删除数据目录 %s", opt.DataDir)
		if err := os.RemoveAll(opt.DataDir); err != nil {
			return res, fmt.Errorf("删除数据目录失败：%w", err)
		}
	}
	return res, nil
}

// swap 做那个「解包到 .new → 删旧 → rename」的原子切换。
//
// 返回被删掉的旧目录（为空表示此前没有安装）。
func swap(opt Options, logf func(string, ...any)) (string, error) {
	target := filepath.Clean(opt.InstallDir)
	stage := target + ".new"

	// 上一次失败的安装可能留下 .new：先清掉，否则 rename 会因为目标非空而失败。
	if err := os.RemoveAll(stage); err != nil {
		return "", fmt.Errorf("清理上次残留的 %s 失败：%w", stage, err)
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return "", fmt.Errorf("创建暂存目录失败：%w", err)
	}

	logf("解包到 %s", stage)
	if err := copyPayload(opt.SrcDir, stage); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}

	// 记录旧目录是否真的存在，好如实回报「这是升级还是全新安装」。
	var removed string
	if _, err := os.Stat(target); err == nil {
		removed = target
		logf("删除旧目录 %s", target)
		if err := os.RemoveAll(target); err != nil {
			_ = os.RemoveAll(stage)
			return "", fmt.Errorf(
				"删除旧安装目录失败：%w\n"+
					"通常是仍有进程占用其中的文件（已尝试停止，仍失败则请重启后重试）", err)
		}
	}

	// rename 在同一卷内是原子的 —— 这一步要么完整生效，要么完全没发生。
	// 跨卷时 rename 会失败（EXDEV），但上面已经把 stage 建在 target 的同一父目录下，
	// 所以不跨卷。
	logf("切换到 %s", target)
	if err := os.Rename(stage, target); err != nil {
		return "", fmt.Errorf("启用新目录失败（%w）；新版本已就绪在 %s，可手工改名", err, stage)
	}
	return removed, nil
}

// extraFiles 是随包落盘的辅助文件（图标、启动器、说明、卸载脚本）。
//
// 刻意用显式清单而不是「把 srcDir 下的都拷过去」：清单是安装的契约，
// 将来加文件时会强迫作者想一次「它属于运行时还是构建期」；而「全拷」会把
// 构建产物（.pdb、obj 之类）一起带进用户的安装目录。
//
// 缺失即跳过 —— 这些是可选件（服务端形态没有图标与启动器），
// 不像网关二进制那样缺了就不能用。
var extraFiles = []string{
	"app.ico",
	"run.cmd",
	"launch-hidden.vbs",
	"uninstall.cmd",
	"INSTALL.txt",
	"WebView2Loader.dll",
}

// copyPayload 把安装源目录里的载荷按清单铺进暂存目录。
//
// 布局（三端一致，与 backend.rs::locate 和 internal/localagent 的查找顺序对齐）：
//
//	<安装目录>/workbuddy-gateway-desktop[.exe]   桌面壳
//	<安装目录>/bin/workbuddy-gateway[.exe]        网关
//	<安装目录>/bin/wb-local-agent[.exe]           本机代理（可选）
//
// 为什么网关要放在 bin/ 下而不是与壳同级：locate() 两个位置都找，但 bin/ 这个约定
// 从桌面版一落地就是它，改了会让已装的旧版本壳找不到新网关（反之亦然）。
func copyPayload(srcDir, stage string) error {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}

	type item struct {
		src  string // 源文件（绝对路径）
		dst  string // 目标（相对 stage 的路径）
		mode os.FileMode
	}

	var items []item
	// 用「候选源文件名」而不是写死一个名字：Windows 上带 .exe，Unix 上不带。
	// 两份候选都试，找不到再报错，免得因为后缀写错而静默漏装。
	gatewayNames := []string{"workbuddy-gateway" + exe, "workbuddy-gateway"}
	agentNames := []string{"wb-local-agent" + exe, "wb-local-agent"}
	shellNames := []string{"workbuddy-gateway-desktop" + exe, "workbuddy-gateway-desktop"}

	pick := func(names []string, optional bool, label string) (string, error) {
		for _, n := range names {
			p := filepath.Join(srcDir, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
		if optional {
			return "", nil
		}
		return "", fmt.Errorf("安装包里缺少 %s（在 %s 下找过 %v）", label, srcDir, names)
	}

	gw, err := pick(gatewayNames, false, "网关 workbuddy-gateway")
	if err != nil {
		return err
	}
	items = append(items, item{src: gw, dst: filepath.Join("bin", "workbuddy-gateway"+exe), mode: 0o755})

	// 本机代理与桌面壳都是**可选**：服务端形态没有壳，命令行形态没有代理。
	// 缺了不报错是刻意的 —— 面板会自动隐藏对应页面（见 internal/localagent 的注释）。
	agent, err := pick(agentNames, true, "本机代理 wb-local-agent")
	if err != nil {
		return err
	}
	if agent != "" {
		items = append(items, item{src: agent, dst: filepath.Join("bin", "wb-local-agent"+exe), mode: 0o755})
	}

	shell, err := pick(shellNames, true, "桌面壳")
	if err != nil {
		return err
	}
	if shell != "" {
		items = append(items, item{src: shell, dst: "workbuddy-gateway-desktop" + exe, mode: 0o755})
	}

	// 辅助文件：源目录里有就带上，没有就跳过。
	for _, name := range extraFiles {
		src := filepath.Join(srcDir, name)
		if st, err := os.Stat(src); err != nil || st.IsDir() {
			continue
		}
		items = append(items, item{src: src, dst: name, mode: 0o644})
	}

	for _, it := range items {
		target := filepath.Join(stage, filepath.FromSlash(it.dst))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("创建目录失败：%w", err)
		}
		if err := copyFile(it.src, target, it.mode); err != nil {
			return fmt.Errorf("复制 %s 失败：%w", filepath.Base(it.src), err)
		}
	}
	return nil
}

// copyFile 复制单个文件，保留可执行位。
//
// 不用 io.Copy 的封装：这里必须显式 Close 并检查错误 —— 没关掉的写句柄会让紧随
// 其后的 rename 在某些文件系统上失败，而那个失败信息完全指不到根因。
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
