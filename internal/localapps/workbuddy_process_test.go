package localapps

import (
	"os"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// 进程控制（关闭 / 启动）的单测
//
// 全部用注入替身：**绝不能在跑测试的机器上真去结束或启动客户端**。
// 断言的是「命令序列」与「顺序」——关闭必须发生在写认证文件之前，
// 启动必须发生在写完之后（理由见 workbuddy_process.go 顶部）。
// -----------------------------------------------------------------------------

// TestMain 给整个包装上「没有客户端进程、不启动任何东西」的兜底替身。
//
// **这不是洁癖，是必须的**：SwitchWorkBuddy 现在会关闭并重启客户端，
// 而包内多个既有测试都会调用它 —— 真跑起来会把跑测试这台机器上**正在使用的**
// WorkBuddy 客户端关掉再打开（第一次加这个功能时已经真实发生过一次）。
// 需要特定行为的测试自己替换替身，Cleanup 后自动回到这里的兜底实现。
func TestMain(m *testing.M) {
	restore := installNoopProcWorld()
	code := m.Run()
	restore()
	os.Exit(code)
}

// installNoopProcWorld 装上「没有任何客户端在运行」的替身，返回还原函数。
func installNoopProcWorld() func() {
	oldList, oldRun, oldSpawn, oldLaunch := listWorkBuddyProcs, runProcCmd, spawnDetached, launchWorkBuddyClient
	listWorkBuddyProcs = func(string) []wbProc { return nil }
	runProcCmd = func(time.Duration, string, ...string) (string, error) { return "", nil }
	spawnDetached = func(string, ...string) error { return nil }
	launchWorkBuddyClient = func(string, string) (string, error) { return "", nil }
	return func() {
		listWorkBuddyProcs, runProcCmd, spawnDetached, launchWorkBuddyClient =
			oldList, oldRun, oldSpawn, oldLaunch
	}
}

// fakeProcWorld 替换进程枚举 / 命令执行 / 启动器，记录发生过的动作。
type fakeProcWorld struct {
	alive   []wbProc
	calls   []string
	spawned []string
	// onKill 在每次结束命令执行时调用（可用于模拟「进程退出了」「文件被删了」等）。
	onKill func()
}

func newFakeProcWorld(t *testing.T, procs ...wbProc) *fakeProcWorld {
	t.Helper()
	w := &fakeProcWorld{alive: append([]wbProc(nil), procs...)}
	oldList, oldRun, oldSpawn := listWorkBuddyProcs, runProcCmd, spawnDetached
	listWorkBuddyProcs = func(string) []wbProc { return append([]wbProc(nil), w.alive...) }
	runProcCmd = func(_ time.Duration, name string, args ...string) (string, error) {
		w.calls = append(w.calls, name+" "+strings.Join(args, " "))
		if w.onKill != nil {
			w.onKill()
		}
		return "", nil
	}
	spawnDetached = func(exe string, _ ...string) error {
		w.spawned = append(w.spawned, exe)
		return nil
	}
	t.Cleanup(func() { listWorkBuddyProcs, runProcCmd, spawnDetached = oldList, oldRun, oldSpawn })
	return w
}

// isForceKill 判断一条结束命令是不是「强杀」（Windows /F、Unix -9）。
func isForceKill(call string) bool {
	return strings.Contains(call, "/F") || strings.Contains(call, "-9")
}

// 优雅关闭生效时：只发一次结束命令，且不是强杀；返回的 exeHint 用于之后启动。
func TestCloseWorkBuddyGracefulSucceeds(t *testing.T) {
	exe := `C:\Apps\WorkBuddyAI\WorkBuddyAI.exe`
	w := newFakeProcWorld(t, wbProc{PID: 4242, Exe: exe})
	w.onKill = func() { w.alive = nil } // 客户端响应了优雅关闭

	notes, wasRunning, exeHint, err := closeWorkBuddyClient("intl", 400*time.Millisecond)
	if err != nil {
		t.Fatalf("优雅关闭应成功，实际 %v", err)
	}
	if !wasRunning {
		t.Fatal("关闭前有进程，wasRunning 应为 true")
	}
	if exeHint != exe {
		t.Fatalf("exeHint 应为进程映像路径 %q，实际 %q", exe, exeHint)
	}
	if len(w.calls) != 1 {
		t.Fatalf("优雅关闭只需一次结束命令，实际 %v", w.calls)
	}
	if isForceKill(w.calls[0]) {
		t.Fatalf("第一次就强杀会让客户端来不及收尾，实际命令 %q", w.calls[0])
	}
	if !strings.Contains(strings.Join(notes, "；"), "已关闭客户端") {
		t.Fatalf("说明里应写明已关闭客户端，实际 %v", notes)
	}
}

// 优雅关闭不生效时：升级为强杀，并如实说明。
func TestCloseWorkBuddyEscalatesToForceKill(t *testing.T) {
	w := newFakeProcWorld(t, wbProc{PID: 77})
	kills := 0
	w.onKill = func() {
		kills++
		if kills >= 2 { // 第一次（优雅）不生效，第二次（强杀）才退出
			w.alive = nil
		}
	}

	notes, _, _, err := closeWorkBuddyClient("cn", 400*time.Millisecond)
	if err != nil {
		t.Fatalf("强杀后应成功，实际 %v", err)
	}
	if len(w.calls) != 2 {
		t.Fatalf("应先优雅再强杀，共 2 次，实际 %v", w.calls)
	}
	if isForceKill(w.calls[0]) || !isForceKill(w.calls[1]) {
		t.Fatalf("命令顺序应为「先优雅、后强杀」，实际 %v", w.calls)
	}
	if !strings.Contains(strings.Join(notes, "；"), "强制结束") {
		t.Fatalf("说明里应写明是强制结束，实际 %v", notes)
	}
}

// 关不掉时必须报错：调用方据此放弃写入（否则写进去的账号会被客户端退出时覆盖）。
func TestCloseWorkBuddyFailsWhenProcessSurvives(t *testing.T) {
	w := newFakeProcWorld(t, wbProc{PID: 9})
	// onKill 不改变 alive：进程死活不退。

	_, wasRunning, _, err := closeWorkBuddyClient("cn", 400*time.Millisecond)
	if err == nil {
		t.Fatal("进程仍在运行时必须报错，否则会写入一个随即被覆盖的账号")
	}
	if !wasRunning {
		t.Fatal("报错路径也要如实返回「原本在运行」")
	}
	if !strings.Contains(err.Error(), "无法关闭") {
		t.Fatalf("错误里应说明客户端关不掉，实际 %v", err)
	}
	if len(w.calls) != 2 {
		t.Fatalf("优雅 + 强杀各一次，实际 %v", w.calls)
	}
}

// 客户端没在运行：不算失败，也不发任何结束命令。
func TestCloseWorkBuddySkipsWhenNotRunning(t *testing.T) {
	w := newFakeProcWorld(t)

	notes, wasRunning, exeHint, err := closeWorkBuddyClient("cn", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("未运行不该报错，实际 %v", err)
	}
	if wasRunning || exeHint != "" {
		t.Fatalf("未运行时 wasRunning/exeHint 应为空，实际 %v / %q", wasRunning, exeHint)
	}
	if len(w.calls) != 0 {
		t.Fatalf("未运行不该发结束命令，实际 %v", w.calls)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "未在运行") {
		t.Fatalf("应说明跳过关闭，实际 %v", notes)
	}
}

// -----------------------------------------------------------------------------
// 顺序：关闭 → 写认证文件 → 启动
// -----------------------------------------------------------------------------

// authUIDAt 读认证文件当前登录的 uid（读不到返回 ""）。
func authUIDAt(path string) string {
	doc, err := readJSONMap(path)
	if err != nil {
		return ""
	}
	acc, _ := doc["account"].(map[string]any)
	uid, _ := acc["uid"].(string)
	return uid
}

// 切换的完整顺序：关闭（此刻文件里还是旧账号）→ 写入新账号 → 启动（此刻已是新账号）。
func TestSwitchWorkBuddyClosesThenWritesThenLaunches(t *testing.T) {
	_, _ = setupHome(t)
	path := workBuddyAuthPath("intl")
	writeFile(t, path, `{"account":{"uid":"uid-old"},"auth":{"accessToken":"t"},`+
		`"allAccounts":[{"uid":"uid-old"},`+
		`{"uid":"uid-new","accessToken":{"$wbEncrypted":1,"envelope":"AAA"}}]}`)

	w := newFakeProcWorld(t, wbProc{PID: 4242, Exe: `C:\Apps\WorkBuddyAI\WorkBuddyAI.exe`})
	var uidAtKill, uidAtLaunch string
	w.onKill = func() {
		uidAtKill = authUIDAt(path) // 关闭时文件里应还是旧账号（还没写）
		w.alive = nil
	}
	oldLaunch := launchWorkBuddyClient
	launchWorkBuddyClient = func(site, exeHint string) (string, error) {
		uidAtLaunch = authUIDAt(path)
		return "已重新打开 WorkBuddyAI.exe", nil
	}
	t.Cleanup(func() { launchWorkBuddyClient = oldLaunch })

	notes, err := SwitchWorkBuddy("intl",
		PoolAccount{ID: "workbuddy-new.json", UID: "uid-new", Nickname: "新账号", Site: "intl"}, false)
	if err != nil {
		t.Fatalf("切换应成功，实际 %v", err)
	}
	if uidAtKill != "uid-old" {
		t.Fatalf("关闭时必须发生在写入之前（那时文件里应是旧账号），实际读到 %q", uidAtKill)
	}
	if uidAtLaunch != "uid-new" {
		t.Fatalf("启动时必须发生在写入之后（那时文件里应是新账号），实际读到 %q", uidAtLaunch)
	}
	if got := authUIDAt(path); got != "uid-new" {
		t.Fatalf("认证文件最终应是新账号，实际 %q", got)
	}
	joined := strings.Join(notes, "；")
	for _, want := range []string{"已关闭客户端", "已重新打开", "已切到新账号"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("说明里应包含 %q，实际 %v", want, notes)
		}
	}
}

// 客户端原本没运行：不替用户打开它，如实说明。
func TestSwitchWorkBuddyDoesNotLaunchWhenNotRunning(t *testing.T) {
	_, _ = setupHome(t)
	path := workBuddyAuthPath("cn")
	writeFile(t, path, `{"account":{"uid":"uid-old"},"auth":{"accessToken":"t"},`+
		`"allAccounts":[{"uid":"uid-new","accessToken":{"$wbEncrypted":1,"envelope":"AAA"}}]}`)

	newFakeProcWorld(t) // 没有任何进程
	launched := false
	oldLaunch := launchWorkBuddyClient
	launchWorkBuddyClient = func(string, string) (string, error) { launched = true; return "", nil }
	t.Cleanup(func() { launchWorkBuddyClient = oldLaunch })

	notes, err := SwitchWorkBuddy("cn", PoolAccount{ID: "workbuddy-new.json", UID: "uid-new"}, false)
	if err != nil {
		t.Fatalf("切换应成功，实际 %v", err)
	}
	if launched {
		t.Fatal("客户端原本没运行，不该替用户启动它")
	}
	if !strings.Contains(strings.Join(notes, "；"), "未自动启动") {
		t.Fatalf("应说明没有自动启动，实际 %v", notes)
	}
}

// 关闭之后写入失败：必须把客户端恢复回去，并如实说明切换未完成。
func TestSwitchWorkBuddyRelaunchesWhenWriteFails(t *testing.T) {
	_, _ = setupHome(t)
	path := workBuddyAuthPath("cn")
	writeFile(t, path, `{"account":{"uid":"uid-old"},"auth":{"accessToken":"t"},`+
		`"allAccounts":[{"uid":"uid-new","accessToken":{"$wbEncrypted":1,"envelope":"AAA"}}]}`)

	w := newFakeProcWorld(t, wbProc{PID: 4242, Exe: `C:\Apps\WorkBuddy\WorkBuddy.exe`})
	w.onKill = func() {
		// 模拟「关闭后认证文件读不到了」：让切换在写入前失败。
		_ = os.Remove(path)
		w.alive = nil
	}
	relaunched := false
	oldLaunch := launchWorkBuddyClient
	launchWorkBuddyClient = func(string, string) (string, error) { relaunched = true; return "已重新打开", nil }
	t.Cleanup(func() { launchWorkBuddyClient = oldLaunch })

	notes, err := SwitchWorkBuddy("cn", PoolAccount{ID: "workbuddy-new.json", UID: "uid-new"}, false)
	if err == nil {
		t.Fatal("认证文件读不到时应报错")
	}
	if !relaunched {
		t.Fatal("关闭之后失败，必须把客户端恢复回去（否则用户面对一个被关掉却没打开的客户端）")
	}
	if !strings.Contains(strings.Join(notes, "；"), "切换未完成") {
		t.Fatalf("说明里应写明切换未完成，实际 %v", notes)
	}
}

// 明文门槛在**关闭客户端之前**判：不能出现「客户端被关了、账号也没切成」。
func TestSwitchWorkBuddyPlaintextGateRunsBeforeClose(t *testing.T) {
	_, _ = setupHome(t)
	path := workBuddyAuthPath("cn")
	writeFile(t, path, `{"account":{"uid":"uid-old"},"auth":{"accessToken":"t"},`+
		`"allAccounts":[{"uid":"uid-old"}]}`)

	w := newFakeProcWorld(t, wbProc{PID: 4242})
	oldLaunch := launchWorkBuddyClient
	launchWorkBuddyClient = func(string, string) (string, error) {
		t.Fatal("预检就拒绝时不该走到启动")
		return "", nil
	}
	t.Cleanup(func() { launchWorkBuddyClient = oldLaunch })

	_, err := SwitchWorkBuddy("cn", PoolAccount{ID: "workbuddy-new.json", UID: "uid-new"}, false)
	if err == nil {
		t.Fatal("没有可复用信封且未同意明文时应拒绝")
	}
	if len(w.calls) != 0 {
		t.Fatalf("预检拒绝时不该关闭客户端，实际 %v", w.calls)
	}
	if got := authUIDAt(path); got != "uid-old" {
		t.Fatalf("预检拒绝时不该改文件，实际 %q", got)
	}
}

// 只登录国际站时，Windows 进程名必须是国际版（WorkBuddyAI.exe）——
// 两个客户端是独立应用，关错一个等于没关。
func TestClientImageNamesPerSite(t *testing.T) {
	if got := wbClientImages("intl")[0]; got != "WorkBuddyAI.exe" {
		t.Fatalf("国际站进程名应为 WorkBuddyAI.exe，实际 %q", got)
	}
	if got := wbClientImages("cn")[0]; got != "WorkBuddy.exe" {
		t.Fatalf("国内站进程名应为 WorkBuddy.exe，实际 %q", got)
	}
	if got := wbLinuxPattern("cn"); !strings.Contains(got, "^") {
		t.Fatalf("国内站模式必须排除 workbuddy-ai（它是 workbuddy 的超串），实际 %q", got)
	}
	if got := wbLinuxPattern("intl"); got != "workbuddy-ai" {
		t.Fatalf("国际站模式应为 workbuddy-ai，实际 %q", got)
	}
}

// CIM / tasklist 输出解析：字段里的逗号、空行、脏行都不能让解析崩掉。
func TestParseWindowsProcessRows(t *testing.T) {
	cim := "4242|C:\\Apps\\WorkBuddyAI\\WorkBuddyAI.exe\r\n\r\n坏行\r\n77|\r\n"
	rows := parseCIMRows(cim)
	if len(rows) != 2 {
		t.Fatalf("应解析出 2 条，实际 %v", rows)
	}
	if rows[0].PID != 4242 || rows[0].Exe != `C:\Apps\WorkBuddyAI\WorkBuddyAI.exe` {
		t.Fatalf("第一条解析错误：%+v", rows[0])
	}
	if rows[1].PID != 77 || rows[1].Exe != "" {
		t.Fatalf("取不到路径时 Exe 应为空：%+v", rows[1])
	}

	csv := `"WorkBuddy.exe","1234","Console","1","123,456 K"` + "\n" +
		"信息: 没有运行的任务匹配指定标准。\n"
	rows = parseTasklistRows(csv)
	if len(rows) != 1 || rows[0].PID != 1234 {
		t.Fatalf("tasklist 解析应只取到 PID 1234，实际 %v", rows)
	}
}
