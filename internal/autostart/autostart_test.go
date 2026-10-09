package autostart

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// Query 在任何平台上都必须能跑通，且未配置时不该报 Enabled。
//
// 这条挡的是「读取逻辑写反」和「解析越界 panic」这两类问题 —— 自启状态是面板
// 每次打开配置页都要调的东西，崩在这里等于整页打不开。
func TestQueryDoesNotPanicAndDefaultsToDisabled(t *testing.T) {
	st := Query()
	if !st.Supported {
		t.Fatalf("当前平台 %s 应当支持自启查询", runtime.GOOS)
	}
	// 自动化环境里没人开过自启。若开发机恰好开着，请先关掉再跑。
	if st.Enabled {
		t.Skipf("本机已开启自启（host=%q），跳过默认态断言", st.Host)
	}
	if st.Location == "" {
		t.Error("应当给出自启项的系统位置，用户手动排查时要用")
	}
}

// 自启项必须指向**宿主**，不能指向网关自己。
//
// 直接自启 `workbuddy-gateway.exe serve` 会得到一个没有界面、用户无处关闭的
// 后台进程 —— 这条断言挡住那种退化。
func TestHostBinaryNeverReturnsTheGatewayItself(t *testing.T) {
	host, kind := hostBinary()
	if host == "" {
		// 测试环境下既没有桌面壳也没有 launch-hidden.vbs 是正常的，
		// 此时应当如实报 unknown 而不是猜一个。
		if kind != KindUnknown {
			t.Errorf("路径为空时 Kind 应为 unknown，实际 %q", kind)
		}
		return
	}
	base := strings.ToLower(host)
	if strings.Contains(base, "launch-hidden") {
		if kind != KindCLI {
			t.Errorf("launch-hidden.vbs 应对应 cli 形态，实际 %q", kind)
		}
		return
	}
	if !strings.Contains(base, "desktop") {
		t.Errorf("自启项应当指向桌面壳，实际 %q（kind=%q）", host, kind)
	}
	if kind != KindDesktop {
		t.Errorf("桌面壳应对应 desktop 形态，实际 %q", kind)
	}
}

// 解析注册表输出：值与名称同行的常见形态。
//
// 这是 winRead 的核心解析，写错会让面板把「已开启」显示成「未开启」，
// 而用户看到的现象是「我明明开了，面板说没开」。
func TestParseRegValue(t *testing.T) {
	out := "HKEY_CURRENT_USER\\Software\\Microsoft\\Windows\\CurrentVersion\\Run\r\n" +
		"    WorkBuddyGatewayDesktop    REG_SZ    \"C:\\Program Files\\WB Gateway\\workbuddy-gateway-desktop.exe\"\r\n"
	got := parseRegValue(out, "WorkBuddyGatewayDesktop")
	want := `C:\Program Files\WB Gateway\workbuddy-gateway-desktop.exe`
	if got != want {
		t.Errorf("解析路径错误\n got=%q\nwant=%q", got, want)
	}
}

// 路径含空格时不能被切碎 —— 这正是用 REG_SZ 后整体取值而不是 Fields 的原因。
func TestParseRegValueKeepsSpaces(t *testing.T) {
	out := "    WorkBuddyGatewayDesktop    REG_SZ    \"C:\\a b c\\x y.exe\"\r\n"
	got := parseRegValue(out, "WorkBuddyGatewayDesktop")
	if got != `C:\a b c\x y.exe` {
		t.Errorf("含空格路径被破坏：%q", got)
	}
}

// 名称只是前缀相似的值不能被误认。
func TestParseRegValueRejectsPrefixCollision(t *testing.T) {
	out := "    WorkBuddyGatewayDesktopOld    REG_SZ    \"C:\\wrong.exe\"\r\n" +
		"    WorkBuddyGatewayDesktop    REG_SZ    \"C:\\right.exe\"\r\n"
	got := parseRegValue(out, "WorkBuddyGatewayDesktop")
	if got != `C:\right.exe` {
		t.Errorf("前缀相似的值被误取：%q", got)
	}
}

// 找不到时返回空串而不是崩。
func TestParseRegValueMissing(t *testing.T) {
	if got := parseRegValue("no such value here\r\n", "WorkBuddyGatewayDesktop"); got != "" {
		t.Errorf("未命中时应返回空串，实际 %q", got)
	}
}

// -----------------------------------------------------------------------------
// 宿主判定
// -----------------------------------------------------------------------------

// 桌面壳拉起时（WB_GATEWAY_PARENT_WATCH=1），自启项必须指向**壳**。
//
// 这是整个包最重要的一条：指向网关自己会得到一个没界面、用户无处关闭的后台进程。
func TestResolveHostPrefersParentWhenSpawnedByShell(t *testing.T) {
	host, kind := resolveHost(hostProbes{
		parentWatched: true,
		parent:        func() (string, error) { return `C:\app\workbuddy-gateway-desktop.exe`, nil },
		desktopShell:  func() string { return `C:\other\stale.exe` },
		launchScript:  func() string { return `C:\app\launch-hidden.vbs` },
	})
	if kind != KindDesktop {
		t.Fatalf("应由桌面壳托管，实际 kind=%q", kind)
	}
	// 父进程是运行时事实，优先于同目录探测 —— 后者在双装环境下会选错。
	if host != `C:\app\workbuddy-gateway-desktop.exe` {
		t.Errorf("应取父进程路径，实际 %q", host)
	}
}

// 取不到父进程路径时退到同目录探测，而不是直接失败。
func TestResolveHostFallsBackWhenParentUnavailable(t *testing.T) {
	host, kind := resolveHost(hostProbes{
		parentWatched: true,
		parent:        func() (string, error) { return "", os.ErrProcessDone },
		desktopShell:  func() string { return `C:\app\workbuddy-gateway-desktop.exe` },
		launchScript:  func() string { return "" },
	})
	if kind != KindDesktop || host != `C:\app\workbuddy-gateway-desktop.exe` {
		t.Errorf("应退到同目录探测，实际 host=%q kind=%q", host, kind)
	}
}

// 命令行形态：没有桌面壳时用 launch-hidden.vbs。
func TestResolveHostFallsBackToLaunchScript(t *testing.T) {
	host, kind := resolveHost(hostProbes{
		desktopShell: func() string { return "" },
		launchScript: func() string { return `C:\cli\launch-hidden.vbs` },
	})
	if kind != KindCLI || host != `C:\cli\launch-hidden.vbs` {
		t.Errorf("应落到命令行形态，实际 host=%q kind=%q", host, kind)
	}
}

// 什么都找不到时必须如实报 unknown —— 猜一个会让自启项指向不存在的程序，
// 用户重启后什么都没发生，且完全无从排查。
func TestResolveHostReportsUnknownRatherThanGuessing(t *testing.T) {
	host, kind := resolveHost(hostProbes{
		desktopShell: func() string { return "" },
		launchScript: func() string { return "" },
	})
	if host != "" || kind != KindUnknown {
		t.Errorf("找不到宿主时应返回空 + unknown，实际 host=%q kind=%q", host, kind)
	}
}

// -----------------------------------------------------------------------------
// 冲突检测
// -----------------------------------------------------------------------------

// 安装包的计划任务存在时必须报冲突 —— 它同样会占 8317 端口。
//
// 这是唯一的冲突来源：本开关只用一个注册表值名，注册表侧不可能自我冲突。
func TestResolveConflictDetectsCLITask(t *testing.T) {
	for _, self := range []Kind{KindDesktop, KindCLI} {
		c := resolveConflict(self, conflictProbes{
			taskPresent: func(name string) bool { return name == cliTaskName },
		})
		if c == nil {
			t.Fatalf("%s 形态应报计划任务冲突", self)
		}
		if c.Name != cliTaskName {
			t.Errorf("冲突方应为计划任务 %s，实际 %q", cliTaskName, c.Name)
		}
		if c.Message == "" {
			t.Error("冲突必须带可展示的说明")
		}
	}
}

// 干净环境里不能误报 —— 误报会让用户永远开不了自启，且不知为何。
func TestResolveConflictIsQuietOnCleanSystem(t *testing.T) {
	for _, self := range []Kind{KindDesktop, KindCLI} {
		c := resolveConflict(self, conflictProbes{
			taskPresent: func(string) bool { return false },
		})
		if c != nil {
			t.Errorf("%s 形态在干净系统上不应报冲突，实际 %+v", self, c)
		}
	}
}

// -----------------------------------------------------------------------------
// 形态推断
// -----------------------------------------------------------------------------

// launch-hidden.vbs 应被识别为命令行形态。
func TestKindOfHostDetectsCLILauncher(t *testing.T) {
	if got := kindOfHost(`C:\cli\launch-hidden.vbs`); got != KindCLI {
		t.Errorf("应识别为 cli，实际 %q", got)
	}
}

// 空路径不算任何形态 —— 猜成桌面版会让面板显示一个不存在的宿主。
func TestKindOfHostEmptyIsUnknown(t *testing.T) {
	if got := kindOfHost("   "); got != KindUnknown {
		t.Errorf("空路径应为 unknown，实际 %q", got)
	}
}

// 其余按桌面壳算（我们的自启项正常情况下就指向桌面壳）。
func TestKindOfHostDefaultsToDesktop(t *testing.T) {
	got := kindOfHost(`C:\Program Files\WorkBuddy Gateway\workbuddy-gateway-desktop.exe`)
	if got != KindDesktop {
		t.Errorf("应识别为 desktop，实际 %q", got)
	}
}

// -----------------------------------------------------------------------------
// 静默启动参数
// -----------------------------------------------------------------------------

// 桌面壳形态必须带上静默参数：不带就会在每次登录时弹出一个用户没点开的面板。
func TestEntryArgsSilentForDesktop(t *testing.T) {
	args := entryArgs(KindDesktop)
	if len(args) != 1 || args[0] != SilentArg {
		t.Fatalf("桌面壳应当只带 %s，实际 %v", SilentArg, args)
	}
}

// 命令行形态**不能**带参数：宿主是 launch-hidden.vbs，它的第一个参数是端口，
// 塞一个 --silent 进去会把端口顶掉（网关会去监听一个叫 "--silent" 的端口）。
func TestEntryArgsEmptyForCLI(t *testing.T) {
	if args := entryArgs(KindCLI); len(args) != 0 {
		t.Errorf("命令行形态不该带参数，实际 %v", args)
	}
	if args := entryArgs(KindUnknown); len(args) != 0 {
		t.Errorf("宿主不明时不该带参数，实际 %v", args)
	}
}

// quoteCommand 与 splitCommand 必须互逆 —— 写进自启项的和面板读回来的
// 是同一件事，对不上就会出现「面板说指向 A、实际系统里是 B」。
func TestQuoteAndSplitCommandRoundTrip(t *testing.T) {
	cases := []struct {
		host string
		args []string
	}{
		{`C:\Program Files\WB Gateway\workbuddy-gateway-desktop.exe`, []string{SilentArg}},
		{`/Applications/WorkBuddy Gateway.app/Contents/MacOS/workbuddy-gateway-desktop`, []string{SilentArg}},
		{`/opt/wb/launch-hidden.vbs`, nil},
	}
	for _, tc := range cases {
		raw := quoteCommand(tc.host, tc.args)
		host, args := splitCommand(raw)
		if host != tc.host {
			t.Errorf("路径往返不一致：%q → %q（原始命令行 %q）", tc.host, host, raw)
		}
		if len(args) != len(tc.args) {
			t.Errorf("参数个数往返不一致：%v → %v（原始命令行 %q）", tc.args, args, raw)
		}
	}
}

func TestHasSilent(t *testing.T) {
	if !hasSilent([]string{SilentArg}) {
		t.Error("应当认出静默参数")
	}
	if !hasSilent([]string{"--port", "8317", SilentArg}) {
		t.Error("参数在中间时也要认出来")
	}
	if hasSilent([]string{"--silent-typo"}) {
		t.Error("相似但不相同的参数不该被当成静默开关")
	}
	if hasSilent(nil) {
		t.Error("没有参数时不该报静默")
	}
}

// 带参数的注册表值不能被解析成一个粘在一起的路径。
func TestParseRegValueKeepsArgsSeparate(t *testing.T) {
	out := "    WorkBuddyGatewayDesktop    REG_SZ    " +
		`"C:\Program Files\WB\workbuddy-gateway-desktop.exe" --silent` + "\r\n"
	raw := parseRegValue(out, "WorkBuddyGatewayDesktop")
	host, args := splitCommand(raw)
	if host != `C:\Program Files\WB\workbuddy-gateway-desktop.exe` {
		t.Errorf("路径解析错误：%q（原始值 %q）", host, raw)
	}
	if !hasSilent(args) {
		t.Errorf("应当解析出静默参数，实际 %v（原始值 %q）", args, raw)
	}
}

// -----------------------------------------------------------------------------
// 回读自启项内容（macOS / Linux）
// -----------------------------------------------------------------------------

// plist 里第一个 <string> 是 Label，不是程序路径 —— 取错了会让面板显示
// 一个「com.workbuddy.gateway.desktop」当宿主路径。
func TestMacProgramArgumentsSkipsLabel(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.workbuddy.gateway.desktop</string>
    <key>ProgramArguments</key>
    <array>
        <string>/Applications/WorkBuddy Gateway.app/Contents/MacOS/wb</string>
        <string>--silent</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
`
	argv := macProgramArguments(body)
	if len(argv) != 2 {
		t.Fatalf("应当解析出 2 个参数，实际 %v", argv)
	}
	if argv[0] != "/Applications/WorkBuddy Gateway.app/Contents/MacOS/wb" {
		t.Errorf("第一个元素应当是程序路径，实际 %q", argv[0])
	}
	if !hasSilent(argv[1:]) {
		t.Errorf("应当解析出静默参数，实际 %v", argv[1:])
	}
}

// 老版本写出去的 plist 没有参数：必须如实报「不是静默」，否则面板会对用户撒谎。
func TestMacProgramArgumentsWithoutSilent(t *testing.T) {
	body := `<key>ProgramArguments</key>
<array>
    <string>/Applications/WB.app/Contents/MacOS/wb</string>
</array>`
	argv := macProgramArguments(body)
	if len(argv) != 1 || hasSilent(argv[1:]) {
		t.Errorf("老形态不应报静默，实际 %v", argv)
	}
}

func TestDesktopExecParsing(t *testing.T) {
	body := "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=WorkBuddy 网关\n" +
		"Exec=\"/opt/WorkBuddy Gateway/workbuddy-gateway-desktop\" --silent\n" +
		"Terminal=false\n"
	exec, ok := desktopExec(body)
	if !ok {
		t.Fatal("应当能解析出 Exec 行")
	}
	host, args := splitCommand(exec)
	if host != "/opt/WorkBuddy Gateway/workbuddy-gateway-desktop" {
		t.Errorf("路径解析错误：%q", host)
	}
	if !hasSilent(args) {
		t.Errorf("应当解析出静默参数，实际 %v", args)
	}
}

func TestDesktopExecMissing(t *testing.T) {
	if _, ok := desktopExec("[Desktop Entry]\nType=Application\n"); ok {
		t.Error("没有 Exec 行时不该报成功")
	}
}

// 路径里的 & 不转义会让 plist 变成非法 XML，launchd 直接拒绝加载 ——
// 而自启是延迟生效的，写坏了当场没有任何反馈。
func TestXMLEscapeRoundTrip(t *testing.T) {
	raw := `/Users/a & b/<WB>/"x".app`
	esc := xmlEscape(raw)
	if esc == raw {
		t.Fatal("含敏感字符的路径应当被转义")
	}
	if got := xmlUnescape(esc); got != raw {
		t.Errorf("转义往返不一致：%q → %q", got, raw)
	}
}
