package installer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// makePayload 造一个假的安装载荷目录，形如安装包解压后的样子。
func makePayload(t *testing.T, extras map[string]string) string {
	t.Helper()
	src := t.TempDir()
	exe := exeSuffix()

	write(t, filepath.Join(src, "workbuddy-gateway"+exe), "GATEWAY-BINARY")
	write(t, filepath.Join(src, "wb-local-agent"+exe), "AGENT-BINARY")
	write(t, filepath.Join(src, "workbuddy-gateway-desktop"+exe), "SHELL-BINARY")
	for name, body := range extras {
		write(t, filepath.Join(src, name), body)
	}
	return src
}

// testRoot 为每个测试造一个隔离根，并把它连同 TestGuard 一起交给安装器。
//
// ## 为什么必须有这道护栏（而不是「每个测试记得传临时目录」）
//
// 曾经只靠约定：测试各自传 `t.TempDir()` 当安装目录。但 `Install` 里有一段
// 「读注册表决定落点」的接管逻辑，它能把传进来的目录换成开发机上**真实**的
// 安装目录 —— 结果一次 `go test` 把装好的三个可执行文件覆盖成了几十字节的
// 假数据，还删掉了卸载注册表项。靠约定挡不住「代码自己改变了目标」，
// 所以改成让安装器在动任何东西之前自己校验目标是否在护栏内。
//
// 返回的 root 同时用作安装目录与数据目录的父目录：两者都在它的下面，
// 于是 `-drop-data` 那类删数据的测试也一并被约束住。
func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// 解析一次符号链接：Windows 的 %TEMP% 常是 8.3 短名（WUXIAN~1），
	// 而 TempDir 返回的可能是长名，护栏里的路径比较必须基于同一形态。
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	return root
}

// installForTest 是测试里的安装入口：带护栏、跳过注册。
func installForTest(t *testing.T, root string, opt Options) (Result, error) {
	t.Helper()
	opt.NoRegister = true
	opt.TestGuard = root
	if opt.DataDir == "" {
		opt.DataDir = filepath.Join(root, "data")
	}
	return Install(opt)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestInstallLaysOutPayload 验证装完之后的目录布局与查找约定一致。
//
// 布局是**契约**：backend.rs::locate 与 internal/localagent 按这些名字找文件，
// 任何一个名字写错都会表现为「装完了但启动报找不到网关」。
func TestInstallLaysOutPayload(t *testing.T) {
	exe := exeSuffix()
	root := testRoot(t)
	src := makePayload(t, nil)
	dir := filepath.Join(root, "WorkBuddy Gateway")

	res, err := installForTest(t, root, Options{SrcDir: src, InstallDir: dir})
	if err != nil {
		t.Fatalf("Install 失败：%v", err)
	}
	if res.InstallDir != dir {
		t.Errorf("安装目录 = %q，期望 %q", res.InstallDir, dir)
	}

	for _, want := range []string{
		filepath.Join(dir, "workbuddy-gateway-desktop"+exe),
		filepath.Join(dir, "bin", "workbuddy-gateway"+exe),
		filepath.Join(dir, "bin", "wb-local-agent"+exe),
	} {
		if !fileExists(want) {
			t.Errorf("缺少 %s", want)
		}
	}
}

// TestInstallAtomicSwapRemovesStaleFiles 是**本包最重要的不变量**。
//
// 旧实现（Tauri NSIS）是原地覆盖，只写同名文件，于是某一版曾经有、后来删掉的
// 文件会永久留在安装目录里。原子切换让「装完的目录内容 == 这一版清单」成为
// 结构性保证 —— 这个测试就是守它的。
func TestInstallAtomicSwapRemovesStaleFiles(t *testing.T) {
	exe := exeSuffix()
	root := testRoot(t)
	dir := filepath.Join(root, "WorkBuddy Gateway")

	// 先造一个「旧版本」安装：含一个新版已经没有的文件。
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "workbuddy-gateway-desktop"+exe), "OLD-SHELL")
	write(t, filepath.Join(dir, "bin", "workbuddy-gateway"+exe), "OLD-GATEWAY")
	write(t, filepath.Join(dir, "legacy-removed-file.dll"), "SHOULD-BE-GONE")

	src := makePayload(t, nil)
	if _, err := installForTest(t, root, Options{SrcDir: src, InstallDir: dir}); err != nil {
		t.Fatalf("Install 失败：%v", err)
	}

	if fileExists(filepath.Join(dir, "legacy-removed-file.dll")) {
		t.Error("旧版本独有的文件在升级后仍然存在 —— 原子切换没有生效（退化成了原地覆盖）")
	}
	// 新内容确实写进去了。
	body, err := os.ReadFile(filepath.Join(dir, "workbuddy-gateway-desktop"+exe))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "SHELL-BINARY" {
		t.Errorf("壳内容 = %q，期望 SHELL-BINARY", body)
	}
}

// TestInstallIsIdempotent 连装两次不该失败或留下中间目录。
//
// 这里覆盖的场景很实际：上一次安装中途失败会留下 `<目录>.new`，
// 下一次安装必须先清掉它，否则 rename 会因为目标非空而失败。
func TestInstallIsIdempotent(t *testing.T) {
	root := testRoot(t)
	dir := filepath.Join(root, "WorkBuddy Gateway")
	src := makePayload(t, nil)

	for i := 1; i <= 2; i++ {
		if _, err := installForTest(t, root, Options{SrcDir: src, InstallDir: dir}); err != nil {
			t.Fatalf("第 %d 次 Install 失败：%v", i, err)
		}
		if _, err := os.Stat(dir + ".new"); err == nil {
			t.Errorf("第 %d 次安装后残留了暂存目录 %s.new", i, dir)
		}
	}
}

// TestInstallClearsStaleStage 验证上一次失败留下的 .new 不会阻断本次安装。
func TestInstallClearsStaleStage(t *testing.T) {
	root := testRoot(t)
	dir := filepath.Join(root, "WorkBuddy Gateway")
	// 模拟上次安装留下的残骸。
	write(t, filepath.Join(dir+".new", "leftover.txt"), "junk")

	src := makePayload(t, nil)
	if _, err := installForTest(t, root, Options{SrcDir: src, InstallDir: dir}); err != nil {
		t.Fatalf("Install 失败（未清理上次残留的 .new）：%v", err)
	}
	if fileExists(filepath.Join(dir+".new", "leftover.txt")) {
		t.Error("上次残留的暂存文件仍在 —— 应先清空 .new 再解包")
	}
}

// TestInstallMissingGatewayFails 验证缺少必需文件时**明确报错**而不是装出半个包。
//
// 装一个没有后端的桌面壳，用户看到的是「启动失败：未找到 workbuddy-gateway」，
// 而那本该在安装期就拦下（prepare-resources.mjs 的注释里也是这个口径）。
func TestInstallMissingGatewayFails(t *testing.T) {
	root := testRoot(t)
	_, err := installForTest(t, root, Options{
		SrcDir:     root, // 空载荷：连网关都没有
		InstallDir: filepath.Join(root, "app"),
	})
	if err == nil {
		t.Fatal("载荷里没有网关却安装成功了")
	}
	if !strings.Contains(err.Error(), "workbuddy-gateway") {
		t.Errorf("错误信息应指明缺的是哪个文件，实际：%v", err)
	}
}

// TestInstallWithoutShellSucceeds 验证命令行形态（没有桌面壳）能正常安装。
//
// 服务端/命令行形态本来就没有壳，缺它是**正常路径**而不是故障
// （面板会自动隐藏本机相关页面，见 internal/localagent）。
func TestInstallWithoutShellSucceeds(t *testing.T) {
	exe := exeSuffix()
	root := testRoot(t)
	src := filepath.Join(root, "payload")
	write(t, filepath.Join(src, "workbuddy-gateway"+exe), "GATEWAY")
	write(t, filepath.Join(src, "wb-local-agent"+exe), "AGENT")

	dir := filepath.Join(root, "cli")
	if _, err := installForTest(t, root, Options{SrcDir: src, InstallDir: dir}); err != nil {
		t.Fatalf("没有桌面壳时安装失败：%v", err)
	}
	if !fileExists(filepath.Join(dir, "bin", "workbuddy-gateway"+exe)) {
		t.Error("网关没有装到位")
	}
}

// TestInstallExtrasCopied 验证随包文件按清单落盘。
func TestInstallExtrasCopied(t *testing.T) {
	root := testRoot(t)
	src := makePayload(t, map[string]string{
		"app.ico":     "ICON",
		"INSTALL.txt": "README",
	})
	dir := filepath.Join(root, "app")
	if _, err := installForTest(t, root, Options{SrcDir: src, InstallDir: dir}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"app.ico": "ICON", "INSTALL.txt": "README"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("缺少随包文件 %s：%v", name, err)
			continue
		}
		if string(body) != want {
			t.Errorf("%s 内容 = %q，期望 %q", name, body, want)
		}
	}
	// 清单以外的文件不该被带进去（防止将来有人改成「全拷目录」）。
	write(t, filepath.Join(src, "build-artifact.pdb"), "NOISE")
	dir2 := filepath.Join(root, "app2")
	if _, err := installForTest(t, root, Options{SrcDir: src, InstallDir: dir2}); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(dir2, "build-artifact.pdb")) {
		t.Error("清单外的构建产物被拷进了安装目录")
	}
}

// TestUninstallKeepsDataByDefault 验证卸载默认**保留**数据目录。
//
// 数据目录里是真实的 `workbuddy*.json` 凭据。卸载程序无权替用户决定删掉它们
// （重装一次就能用回来，删了却不可恢复）。
func TestUninstallKeepsDataByDefault(t *testing.T) {
	root := testRoot(t)
	dir := filepath.Join(root, "app")
	data := filepath.Join(root, "data")
	write(t, filepath.Join(data, "workbuddy-abc.json"), `{"accessToken":"secret"}`)

	if _, err := installForTest(t, root, Options{SrcDir: makePayload(t, nil), InstallDir: dir, DataDir: data, KeepData: true}); err != nil {
		t.Fatal(err)
	}
	res, err := Uninstall(Options{InstallDir: dir, DataDir: data, KeepData: true, TestGuard: root})
	if err != nil {
		t.Fatalf("Uninstall 失败：%v", err)
	}
	if !res.DataKept {
		t.Error("DataKept 应为 true")
	}
	if fileExists(filepath.Join(dir, "bin", "workbuddy-gateway"+exeSuffix())) {
		t.Error("安装目录没有被删除")
	}
	if !fileExists(filepath.Join(data, "workbuddy-abc.json")) {
		t.Error("数据目录被删了 —— 默认必须保留（里面有真实凭据）")
	}
}

// TestUninstallDropData 验证显式要求时才删数据目录。
func TestUninstallDropData(t *testing.T) {
	root := testRoot(t)
	dir := filepath.Join(root, "app")
	data := filepath.Join(root, "data")
	write(t, filepath.Join(data, "workbuddy-abc.json"), `{"accessToken":"secret"}`)

	if _, err := installForTest(t, root, Options{SrcDir: makePayload(t, nil), InstallDir: dir, DataDir: data}); err != nil {
		t.Fatal(err)
	}
	res, err := Uninstall(Options{InstallDir: dir, DataDir: data, KeepData: false, TestGuard: root})
	if err != nil {
		t.Fatalf("Uninstall 失败：%v", err)
	}
	if res.DataKept {
		t.Error("DataKept 应为 false")
	}
	if fileExists(filepath.Join(data, "workbuddy-abc.json")) {
		t.Error("显式要求删数据目录时它应该被删掉")
	}
}

// TestGuardBlocksRealInstallDir 是那道护栏本身的测试 —— 它守的是**一次真实事故**。
//
// 事故：`adoptLegacy` 读注册表拿到开发机上真实的安装目录，覆盖了测试传进来的
// 临时目录，于是 `go test` 把已装好的三个可执行文件写成了几十字节的假数据，
// 并删掉了卸载注册表项。这个测试确保同样的越界再发生时会**当场报错**而不是
// 继续往下写。
func TestGuardBlocksRealInstallDir(t *testing.T) {
	root := testRoot(t)
	outside := filepath.Join(root, "..", "outside-guard")

	_, err := Install(Options{
		SrcDir:     makePayload(t, nil),
		InstallDir: outside,
		DataDir:    filepath.Join(root, "data"),
		NoRegister: true,
		TestGuard:  root,
	})
	if err == nil {
		t.Fatal("目标目录在护栏之外却安装成功了 —— 护栏失效")
	}
	if !strings.Contains(err.Error(), "护栏") {
		t.Errorf("错误信息应点明是护栏拦下的，实际：%v", err)
	}
	// 越界路径不该被创建。
	if fileExists(outside) {
		t.Errorf("护栏外的目录被创建了：%s", outside)
	}
}

// TestExplicitDirIsNotOverriddenByAdoption 钉住那个**把测试写进真实目录**的根因。
//
// 旧实现无条件让「接管旧安装」覆盖调用方指定的目录：
//
//	if adopted.Dir != "" { opt.InstallDir = adopted.Dir }
//
// 生产路径上这同样是 bug（用户说装到 D 盘，却被装回 C 盘）。正确语义是
// 「显式指定优先」，只在未指定时才去接管旧安装。
func TestExplicitDirIsNotOverriddenByAdoption(t *testing.T) {
	root := testRoot(t)
	want := filepath.Join(root, "explicit")
	res, err := installForTest(t, root, Options{
		SrcDir:     makePayload(t, nil),
		InstallDir: want,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.InstallDir != want {
		t.Errorf("安装目录 = %q，期望 %q（显式指定被覆盖了）", res.InstallDir, want)
	}
	if res.Adopted {
		t.Error("显式指定了安装目录时不该去接管旧安装")
	}
}

// TestDefaultPathsAreStable 钉住默认路径。
//
// 这两个路径是**兼容性契约**：数据目录是账号池所在地，三个入口共用；
// 安装目录决定了老用户的升级路径。改动它们会让已装用户「装完还要重新登录」，
// 所以用测试把它们钉死，让改动必须是有意识的。
func TestDefaultPathsAreStable(t *testing.T) {
	switch runtime.GOOS {
	case "windows":
		t.Setenv("LOCALAPPDATA", `C:\Users\test\AppData\Local`)
		if got, want := DefaultInstallDir(), `C:\Users\test\AppData\Local\WorkBuddy Gateway`; got != want {
			t.Errorf("DefaultInstallDir = %q，期望 %q", got, want)
		}
		if got, want := DefaultDataDir(), `C:\Users\test\AppData\Local\wb-gateway`; got != want {
			t.Errorf("DefaultDataDir = %q，期望 %q", got, want)
		}
	case "darwin":
		if got, want := DefaultInstallDir(), "/Applications/WorkBuddy Gateway.app"; got != want {
			t.Errorf("DefaultInstallDir = %q，期望 %q", got, want)
		}
		home, _ := os.UserHomeDir()
		if got, want := DefaultDataDir(), filepath.Join(home, ".wb-gateway"); got != want {
			t.Errorf("DefaultDataDir = %q，期望 %q", got, want)
		}
	default:
		// Linux 由 dpkg 决定落点，不由我们选。
		if got := DefaultInstallDir(); got != "" {
			t.Errorf("Linux 上 DefaultInstallDir 应为空（文件归 dpkg 所有），实际 %q", got)
		}
	}
}
