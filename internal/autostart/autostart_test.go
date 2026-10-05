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
