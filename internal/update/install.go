package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// 本文件实现「把下载好的安装包装到本机，并让网关跑上新版本」。
//
// # 为什么这一步不做成全自动
//
// 检测与下载都是**只读**的（打一次 GitHub、往数据目录放一个文件）；而安装会：
// 覆盖已经装好的程序、结束正在跑的进程、必要时提权。它失败的样子也很难看 ——
// 可能留下一个「既起不来、也没有可回退版本」的现场，而用户的 CLI / IDE 全指着这个端口。
//
// 所以分界画在这里：**下载自动，安装必须由人在面板上点一下并确认**（确认框里展示
// 将要执行的命令，用户知道会发生什么）。配置里只有 `update.auto_download` 一个开关，
// 没有 `auto_install` —— 少一个能把自己搞挂的开关。
//
// # 各平台的做法与边界
//
//   - Linux：`pkexec dpkg -i <deb>`（polkit 弹系统授权框）。装完网关**自己重启**：
//     先走与 SIGTERM 完全相同的优雅关闭，再用 exec 换掉进程映像（PID 不变，
//     桌面壳的父子关系与 stdin 管道都还在）。systemd 代管时**不**这么做，
//     见 main.go 的 managedBySystemd。
//   - macOS：`hdiutil attach -mountpoint <临时目录>` → `ditto <挂载>/X.app /Applications/X.app`
//     → `hdiutil detach`。无签名，首次打开仍可能要右键「打开」；/Applications 不可写时
//     如实报错并让用户手动拖。
//   - Windows：启动安装向导（不静默）。安装包自带的钩子会先按路径结束占用文件的进程
//     （含本网关与桌面壳）再覆盖文件，静默安装装完不会拉起窗口，而这里没有可靠的
//     「装完再启动」通路 —— 所以交给向导，最后一步由用户点。UAC 也由它自己弹。

// InstallPlan 描述「在这台机器上装这个包」会做什么，供面板在确认框里如实展示。
//
// 面板不自己拼这些文案：平台差异（要不要提权、装完谁重启、有没有额外提醒）
// 只有后端知道，前端猜出来的话迟早与实现对不上。
type InstallPlan struct {
	// Supported 表示本平台支持「面板内一键安装」。
	Supported bool `json:"supported"`
	// Command 是将要执行的命令（多条时按行展示）。
	Command string `json:"command,omitempty"`
	// NeedsAuth 表示会弹系统授权框。
	NeedsAuth bool `json:"needs_auth,omitempty"`
	// Restart 表示安装成功后网关会自行重启到新版本。
	Restart bool `json:"restart,omitempty"`
	// Reason 是「本平台不支持」的原因（Supported=false 时展示）。
	Reason string `json:"reason,omitempty"`
	// Note 是给用户的额外提醒（例如 macOS 无签名、Windows 会弹向导）。
	Note string `json:"note,omitempty"`
}

// InstallResult 是一次安装尝试的结果。
type InstallResult struct {
	OK bool `json:"ok"`
	// Detail 是给用户看的一句话结果。
	Detail string `json:"detail"`
	// Restart 表示网关会随后自行重启（exec 换上新版本，面板等它回来即可）。
	Restart bool `json:"restart"`
	// Exit 表示网关会随后**退出**、且不自行回来。
	//
	// 只有 Windows 走这条：那边正在运行的 .exe 被系统锁住，而安装程序在**升级**时会
	// 先跑**旧版本**的卸载器 —— 它删不掉还被占用的 `bin\*.exe`，于是直接报
	// 「无法卸载！」并中止安装（Tauri 的 NSIS 模板里 unableToUninstall 就是这个文案）。
	// 我们自己的杀进程钩子挂在安装段里，跑到那一步已经太晚（卸载器早就返回了）。
	// 所以唯一可靠的办法是在启动向导之后立刻退出，把文件锁让出去。
	// 重新启动由安装向导最后一页的「运行」完成（见 installWindows）。
	Exit bool `json:"exit,omitempty"`
	// Output 是安装命令的输出（失败时最有价值：dpkg 会在这里说清缺什么）。
	Output string `json:"output,omitempty"`
}

// assetNamePattern 限制安装包文件名：只允许保守字符集。
//
// 文件名的来源是 Release 资产的 URL（不是客户端传参），但这层校验仍然要有 ——
// 这个名字会被拼进命令行交给 pkexec 执行，任何引号/空格/换行都是注入面。
var assetNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// commonInstallHint 是所有平台都要说的一句提醒：本页下到的是**系统级安装包**
// （deb / dmg / setup.exe），而 npm 全局安装的实例该走 npm 自己的升级路径。
// 两者装的是同一份网关，但落点不同 —— 混着装会出现「装完还显示旧版本」的困惑。
const commonInstallHint = "若这个网关是用 npm 全局安装的，请改用 npm i -g workbuddy-gateway@latest 升级，不要用这个安装包。"

// planFor 返回在 goos 上安装 path 的方案。纯函数（不碰文件系统），便于测试三个平台。
func planFor(goos, path string) InstallPlan {
	if strings.TrimSpace(path) == "" {
		path = "<安装包路径>"
	}
	var plan InstallPlan
	switch goos {
	case "linux":
		plan = InstallPlan{
			Supported: true,
			Command:   "pkexec dpkg -i " + path,
			NeedsAuth: true,
			Restart:   true,
			Note:      "会弹系统授权框（polkit）临时提权；装完网关自动重启到新版本，面板会在几秒内重连。",
		}
	case "darwin":
		plan = InstallPlan{
			Supported: true,
			Command: "hdiutil attach -nobrowse -readonly -mountpoint <临时目录> " + path +
				"\nditto <挂载点>/*.app /Applications/" +
				"\nhdiutil detach <临时目录>",
			Restart: true,
			Note:    "安装包未做代码签名：装好后首次打开可能要在「应用程序」里右键 →「打开」。装完网关自动重启。",
		}
	case "windows":
		plan = InstallPlan{
			Supported: true,
			Command:   path,
			Restart:   false,
			Note: "会打开安装向导，网关随即退出以让出 bin\\*.exe 的文件锁 —— " +
				"旧的卸载器删不掉被占用的文件，会直接报「无法卸载！」并中止安装。" +
				"装完请在向导最后一页确认「运行」；中途取消的话网关会停着，需要手动启动一次。",
		}
	default:
		return InstallPlan{Reason: "当前平台（" + goos + "）没有可用的自动安装方式，请到 Release 页手动下载安装。"}
	}
	plan.Note = plan.Note + commonInstallHint
	return plan
}

// Plan 返回本机当前的安装方案（path 为空时用占位符，便于面板在下载前就展示会执行什么）。
func (c *Checker) Plan(path string) InstallPlan { return planFor(c.goos, path) }

// localAsset 返回本地已就绪的安装包路径与大小；不存在或大小与 Release 不符时返回空。
//
// 为什么要校验大小：Release 上的同名资产可能被重新上传（内容变了、名字没变），
// 而「本地这份大小对不上」意味着它根本不是这一版 —— 装上去等于装了个不知道什么的东西。
func (c *Checker) localAsset(name string, want int64) (string, int64) {
	name = strings.TrimSpace(name)
	if name == "" || !assetNamePattern.MatchString(name) || strings.TrimSpace(c.downloadDir) == "" {
		return "", 0
	}
	path := filepath.Join(c.downloadDir, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", 0
	}
	if want > 0 && info.Size() != want {
		return "", 0
	}
	return path, info.Size()
}

// Ready 报告「现在能不能在这台机器上安装」，不能时给出可操作的原因。
//
// 与 Install 分开是为了让**状态码**也说对话：前提不满足（还没下载、没有新版本、
// 平台没有产物）是 409 + 一句「该做什么」，而不是 502 —— 后者会让人以为是网络抖动，
// 去重试、去查网络，而真正的原因是自己还没点下载。
func (c *Checker) Ready() error {
	st := c.State()
	if st.Error != "" {
		return errors.New(st.Error)
	}
	if !st.UpdateAvailable {
		return errors.New("当前没有可安装的新版本，请先检查更新")
	}
	if strings.TrimSpace(st.AssetName) == "" {
		return errors.New("本平台在 Release 上没有对应安装包，请到 Release 页手动下载")
	}
	if path, _ := c.localAsset(st.AssetName, st.AssetSize); path == "" {
		return errors.New("安装包尚未下载完成（或大小与 Release 不一致），请先下载")
	}
	if plan := c.Plan(""); !plan.Supported {
		return errors.New(plan.Reason)
	}
	return nil
}

// Install 把本地已下载的安装包装到本机。
//
// 路径**不接受调用方传入**：只装「当前检查结果匹配到的那个资产名」在下载目录里的那份，
// 因此面板无法让网关去执行任意文件。
func (c *Checker) Install(ctx context.Context) (InstallResult, error) {
	if err := c.Ready(); err != nil {
		return InstallResult{}, err
	}
	st := c.State()
	path, _ := c.localAsset(st.AssetName, st.AssetSize)
	c.logf("[更新] 开始安装 %s（版本 %s）", path, st.Latest)

	switch c.goos {
	case "linux":
		return c.installLinux(ctx, path)
	case "darwin":
		return c.installDarwin(ctx, path)
	case "windows":
		return c.installWindows(path)
	}
	return InstallResult{}, errors.New("当前平台不支持自动安装")
}

// installLinux 用 pkexec + dpkg 安装 deb。
func (c *Checker) installLinux(ctx context.Context, path string) (InstallResult, error) {
	if _, err := exec.LookPath("pkexec"); err != nil {
		return InstallResult{}, fmt.Errorf(
			"未找到 pkexec（polkit 未安装或不在 PATH 里）：请手动执行 sudo dpkg -i %s", path)
	}
	out, err := exec.CommandContext(ctx, "pkexec", "dpkg", "-i", path).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			// 用户没在授权框里确认（或超时）：这不是「安装失败」，如实说。
			return InstallResult{}, errors.New("已取消安装（未在授权框中确认）")
		}
		return InstallResult{}, fmt.Errorf("dpkg 安装失败：%v%s", err, formatOutput(text))
	}
	return InstallResult{
		OK:      true,
		Detail:  "安装完成，网关正在重启到新版本",
		Restart: true,
		Output:  text,
	}, nil
}

// installDarwin 挂载 dmg、把 .app 覆盖到 /Applications，再卸载镜像。
//
// 三步都用绝对路径的外部命令（hdiutil / ditto 是 macOS 自带的），不经 shell ——
// 因此路径里的空格与特殊字符不会被当成命令分隔符。
func (c *Checker) installDarwin(ctx context.Context, path string) (InstallResult, error) {
	mount, err := os.MkdirTemp("", "wb-update-*")
	if err != nil {
		return InstallResult{}, fmt.Errorf("创建临时挂载点失败：%w", err)
	}
	defer func() { _ = os.Remove(mount) }()

	if out, err := exec.CommandContext(ctx, "/usr/bin/hdiutil", "attach",
		"-nobrowse", "-readonly", "-mountpoint", mount, path).CombinedOutput(); err != nil {
		return InstallResult{}, fmt.Errorf("挂载安装镜像失败：%v%s", err, formatOutput(strings.TrimSpace(string(out))))
	}
	defer func() {
		// 无论后续成败都要卸载：留一个挂着的镜像会让用户下次插盘时看到莫名其妙的卷。
		_, _ = exec.Command("/usr/bin/hdiutil", "detach", mount).CombinedOutput()
	}()

	entries, err := os.ReadDir(mount)
	if err != nil {
		return InstallResult{}, fmt.Errorf("读取挂载点失败：%w", err)
	}
	app := ""
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".app") {
			app = e.Name()
			break
		}
	}
	if app == "" {
		return InstallResult{}, errors.New("安装镜像里没有找到 .app，请到 Release 页手动安装")
	}
	src := filepath.Join(mount, app)
	target := filepath.Join("/Applications", app)
	// ditto 而不是 cp -R：它会保留 bundle 的元数据与资源分叉，而 cp 会把
	// 签名/属性弄得七零八落（Gatekeeper 与后续升级都会因此出问题）。
	if out, err := exec.CommandContext(ctx, "/usr/bin/ditto", src, target).CombinedOutput(); err != nil {
		return InstallResult{}, fmt.Errorf(
			"覆盖 /Applications/%s 失败：%v%s（没有写权限时请手动把 .app 拖进「应用程序」覆盖）",
			app, err, formatOutput(strings.TrimSpace(string(out))))
	}
	return InstallResult{
		OK:      true,
		Detail:  "安装完成，网关正在重启到新版本",
		Restart: true,
	}, nil
}

// installWindows 启动安装向导（**不静默**），随后由调用方让网关退出。
//
// 为什么不像其它平台那样静默：Windows 上正在运行的 .exe 被锁，必须由一个提权的
// 安装程序先结束进程再覆盖 —— 安装包自带这个钩子，而静默装完之后没有任何可靠的
// 「重新启动应用」通路（.exe 无法像 Unix 那样 exec 自己）。
//
// 为什么要**退出**（而不是像 Linux/macOS 那样自己重启）：升级时安装向导会先跑
// **旧版本**的卸载器，它删不掉还被我们占着的 `bin\workbuddy-gateway.exe` 与
// `bin\wb-local-agent.exe`，于是 Tauri 的 NSIS 模板直接弹「无法卸载！」并中止 ——
// 我们自己的杀进程钩子挂在安装段里，跑到那一步已经太晚。
//
// 两道防线各管一段：新版安装包与卸载器里的钩子（desktop/src-tauri/windows/hooks.nsh）
// 负责「以后的升级」；这里的退出负责「从任意旧版本升上来」—— 旧版本的卸载器里
// 有没有那个钩子，我们无从判断。
func (c *Checker) installWindows(path string) (InstallResult, error) {
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		return InstallResult{}, fmt.Errorf("启动安装向导失败：%w", err)
	}
	// 不 Wait：向导会先把本进程结束掉（这是它覆盖文件的必要步骤），
	// 等它退出等于永远等不到。
	return InstallResult{
		OK: true,
		Detail: "安装向导已打开；网关随即退出以让出文件锁，" +
			"装完请在向导最后一页确认「运行」（或从开始菜单重新启动）",
		Exit: true,
	}, nil
}

// formatOutput 把命令输出拼成一小段可读文本（空输出返回空串）。
func formatOutput(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	// 提权框被取消时 pkexec 会输出一堆使用说明，取最后几行足够说明问题。
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return "：" + strings.Join(lines, " / ")
}
