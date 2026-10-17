package update

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// 本文件处理「注册表里还留着旧安装的影子」这一种现场。
//
// # 现象
//
// Tauri 的 NSIS 安装向导**只看注册表**就判定「已安装」：只要
// `HKCU\...\Uninstall\WorkBuddy Gateway` 里还有内容，它就会先摆出一页
// 「安装前卸载 / 请勿卸载」。而安装目录被手工删掉、或卸载/更新中途被掐断
// （断电、结束进程、用户取消）时，这个键会留下来 —— 于是「安装前卸载」必然
// 失败（旧卸载器已不在磁盘上），向导只弹一句「无法卸载！」再退回同一页。
// 用户多半不知道还能改选「请勿卸载」，看起来就是版本装不上去了。
//
// # 这里做什么
//
// 在拉起向导**之前**体检一次：键在、但它指向的卸载器已经不在了，就把这条残键
// 删掉（连同厂商键）并按全新安装继续。判据刻意保守：卸载器还在就一律不动 ——
// 那是向导「安装前卸载」要用的东西，真正正常的旧安装必须保持原样。
//
// 安装包里的模板守卫是同一件事的另一半（`desktop/src-tauri/nsis/installer.nsi`）：
// 这里管面板点的「安装并重启」，那里管用户自己双击安装包。
//
// 全程只在 Windows 的安装路径上被调用（installWindows），因此不需要构建标签；
// 跨平台放着也不会跑 —— 与 installLinux / installDarwin 同一套组织方式。

const (
	// updateUninstallKey 是 NSIS 安装器写的「应用和功能」条目（PRODUCTNAME 带空格）。
	//
	// 必须与安装包侧一致：`desktop/src-tauri/nsis/installer.nsi` 的 UNINSTKEY
	// 由 ${PRODUCTNAME} 拼出，而 productName 是 tauri.conf.json 里的
	// "WorkBuddy Gateway"。改产品名就要三处一起改（这里、模板、unified 安装器）。
	updateUninstallKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\WorkBuddy Gateway`

	// updateVendorKey 是 NSIS 记录安装位置的厂商键；安装导向导会用它的默认值
	// 恢复上一次的安装目录。残键一起清掉，否则下一次安装会读到一个旧路径。
	updateVendorKey = `HKCU\Software\workbuddy\WorkBuddy Gateway`
)

// uninstallEntry 是从注册表读出来的一条卸载记录（只取我们关心的字段）。
type uninstallEntry struct {
	DisplayName     string
	MainBinaryName  string
	InstallLocation string
	UninstallString string
}

// regValueLine 匹配 `reg query` 输出的值行：
//
//	DisplayName    REG_SZ    WorkBuddy Gateway
//
// 值名前有两个以上空格缩进，数据部分可有空格（路径就是典型），因此按
// 「名字 + 类型 + 行内余下全部」切。`(Default)` 这类带括号的名字不匹配 ——
// 我们不需要它。
var regValueLine = regexp.MustCompile(`^\s{2,}([A-Za-z0-9_]+)\s+REG_[A-Z_]+\s+(.*)$`)

// parseRegQuery 从 `reg query <键>` 的输出里取出各值。
func parseRegQuery(out string) uninstallEntry {
	var e uninstallEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		m := regValueLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		value := strings.TrimSpace(m[2])
		switch strings.ToLower(m[1]) {
		case "displayname":
			e.DisplayName = value
		case "mainbinaryname":
			e.MainBinaryName = value
		case "installlocation":
			e.InstallLocation = value
		case "uninstallstring":
			e.UninstallString = value
		}
	}
	return e
}

// uninstallerPath 从 UninstallString 里取出可执行文件路径。
//
// NSIS 写的是带引号的完整路径（`"C:\...\uninstall.exe"`）；不带引号时按空格截断，
// 因为引号之外的部分可能跟参数（旧版本曾出现过 ` _?=$INSTDIR` 这种写法）。
func (e uninstallEntry) uninstallerPath() string {
	s := strings.TrimSpace(e.UninstallString)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		if end := strings.Index(s[1:], `"`); end >= 0 {
			return s[1 : 1+end]
		}
		return strings.TrimPrefix(s, `"`) // 只有前半引号：当作整段路径
	}
	if i := strings.IndexAny(s, " \t"); i > 0 {
		return s[:i]
	}
	return s
}

// staleReason 返回「这条记录只是残迹」的原因；不是残迹时返回空串。
//
// 唯一的判据是「卸载器文件还在不在」：它不在，「安装前卸载」这条选项必然失败，
// 而这正是用户会踩的那条路。反过来，卸载器在就一律不动 —— 哪怕安装目录看着不对，
// 那也是向导自己能处理的情形（它有「卸载成功但文件还在」的兜底检查）。
//
// exists 由调用方注入，便于在非 Windows 上跑测试。
func (e uninstallEntry) staleReason(exists func(string) bool) string {
	un := e.uninstallerPath()
	if un == "" {
		return "" // 读不到卸载器路径：判不了，宁可不碰
	}
	if exists(un) {
		return ""
	}
	return "旧卸载器已不存在（" + un + "）"
}

// regRunner 执行一次 reg.exe 并返回合并输出。
type regRunner func(args ...string) (string, error)

// regExec 是生产用的 regRunner。
func regExec(args ...string) (string, error) {
	out, err := exec.Command("reg.exe", args...).CombinedOutput()
	return string(out), err
}

// cleanStaleUninstallEntry 体检并清理残键，返回是否真的删了、以及给人看的原因。
//
// 失败一律不改变现场：清理只是「顺手把路铺平」，铺不动也不该让安装本身失败 ——
// 用户仍然可以在向导里改选「请勿卸载」。
func cleanStaleUninstallEntry(run regRunner, logf func(string, ...any)) (bool, string) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	out, err := run("query", updateUninstallKey)
	if err != nil {
		return false, "" // 键不存在（最常见）或 reg.exe 不可用：都没什么可做
	}
	reason := parseRegQuery(out).staleReason(func(p string) bool {
		_, statErr := os.Stat(p)
		return statErr == nil
	})
	if reason == "" {
		return false, ""
	}
	if _, err := run("delete", updateUninstallKey, "/f"); err != nil {
		logf("[更新] 旧的卸载记录已经失效（%s），但删除失败：%v", reason, err)
		return false, ""
	}
	// 厂商键只记安装位置；删失败不影响安装，如实记日志。
	if _, err := run("delete", updateVendorKey, "/f"); err != nil {
		logf("[更新] 清理厂商键失败（不影响安装）：%v", err)
	}
	logf("[更新] 清理了失效的旧卸载记录：%s", reason)
	return true, reason
}
