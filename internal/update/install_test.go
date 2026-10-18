package update

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy-gateway/internal/config"
)

// assetServer 起一个「假 Release 源 + 假安装包下载」的服务端。
//
// 返回的 Checker 里 GOOS 可注入，因此三平台的规划都能在 Linux 上被断言到；
// 只有真正**执行**安装的那几条路径依赖平台命令（pkexec / hdiutil），不在这里跑 ——
// 本地包不存在时 Install 会在任何 exec 之前就返回，这也正是这些用例的边界。
func assetServer(t *testing.T, goos, name string, payload []byte, sec config.UpdateSection) (*Checker, string) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_, _ = io.WriteString(w, releasesJSON(mkRelease("v0.9.3", map[string]any{
				"assets": []map[string]any{
					asset(name, int64(len(payload)), srv.URL+"/dl/"+name),
				},
			})))
		case "/dl/" + name:
			_, _ = w.Write(payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	c := New(testConfig(t, sec), Options{
		Current: "0.9.2", Client: srv.Client(), APIBase: srv.URL,
		GOOS: goos, GOARCH: "amd64", DownloadDir: dir, Logf: t.Logf,
	})
	return c, dir
}

// TestPlanForPlatforms 三个平台的安装方案：支持与否、要不要提权、装完谁重启。
//
// 这些字段直接决定确认框里给用户看什么，写错会让人在点下去之前做出错误判断。
func TestPlanForPlatforms(t *testing.T) {
	const path = "/tmp/updates/pkg.deb"

	linux := planFor("linux", path)
	if !linux.Supported || !linux.NeedsAuth || !linux.Restart {
		t.Fatalf("linux 方案应为「支持 + 提权 + 自动重启」，实际 %+v", linux)
	}
	if !strings.Contains(linux.Command, "pkexec dpkg -i") || !strings.Contains(linux.Command, path) {
		t.Fatalf("linux 命令应展示 pkexec dpkg 与包路径，实际 %q", linux.Command)
	}

	mac := planFor("darwin", path)
	if !mac.Supported || mac.NeedsAuth || !mac.Restart {
		t.Fatalf("darwin 方案应为「支持 + 不提权 + 自动重启」，实际 %+v", mac)
	}
	for _, want := range []string{"hdiutil attach", "ditto", "hdiutil detach"} {
		if !strings.Contains(mac.Command, want) {
			t.Fatalf("darwin 命令缺少 %q：%q", want, mac.Command)
		}
	}

	win := planFor("windows", `C:\x\setup.exe`)
	if !win.Supported || win.NeedsAuth {
		t.Fatalf("windows 方案应为「支持 + 不提权」，实际 %+v", win)
	}
	// Windows 也能自动回到新版本了：/S 静默装完、/R 把应用重新拉起来，
	// 不再有「用户去向导最后一页点运行」这一步。
	if !win.Restart {
		t.Fatal("windows 上装完会自动把应用拉起来，应当承诺自动重启")
	}
	// 命令里那两个开关就是「无人值守」的全部依据：/S 不显示页面也不执行页面回调
	// （不会有选择题）、/R 装完拉起应用。它们必须与 installWindows 真正发出去的
	// 参数是同一份 —— 面板上写的和实际执行的不一样，比不写更糟。
	if !strings.Contains(win.Command, "/S") || !strings.Contains(win.Command, "/R") {
		t.Fatalf("windows 命令应带上无人值守开关 /S /R，实际 %q", win.Command)
	}
	// 说明里要交代两件用户看不到却会经历的事：装的过程不弹任何界面、网关会先退出
	// 让出文件锁（面板会短暂断连）。
	for _, want := range []string{"静默", "文件锁", "重新拉起来"} {
		if !strings.Contains(win.Note, want) {
			t.Fatalf("windows 方案的说明缺少 %q：%q", want, win.Note)
		}
	}

	other := planFor("plan9", path)
	if other.Supported || other.Reason == "" {
		t.Fatalf("未知平台应当明确报「不支持」并给出原因，实际 %+v", other)
	}
}

// TestLocalAssetRequiresMatchingSize 本地文件大小与 Release 不符时视为「没下好」。
//
// Release 上的同名资产可能被重新上传，装上一份内容不明的包比「请重新下载」危险得多。
func TestLocalAssetRequiresMatchingSize(t *testing.T) {
	c, dir := assetServer(t, "linux", "pkg.deb", []byte("0123456789"), config.UpdateSection{Repo: "o/r"})

	if p, _ := c.localAsset("pkg.deb", 10); p != "" {
		t.Fatal("文件还不存在时不该报告已就绪")
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg.deb"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, n := c.localAsset("pkg.deb", 10)
	if p == "" || n != 10 {
		t.Fatalf("大小一致时应报告已就绪，实际 %q %d", p, n)
	}
	if p, _ := c.localAsset("pkg.deb", 11); p != "" {
		t.Fatal("大小不一致（内容可能被换过）时不该报告已就绪")
	}

	// 文件名来自 Release，但仍然只接受保守字符集：它会被拼进计划执行的命令行。
	for _, bad := range []string{"", " ", "../etc/passwd", "a b.deb", "a;rm -rf.deb", "中文.deb"} {
		if p, _ := c.localAsset(bad, 0); p != "" {
			t.Fatalf("非法文件名 %q 不该被接受", bad)
		}
	}
}

// TestInstallRequiresDownloadedAsset 没下好之前安装必须被拒绝，并说清该做什么；
// 这条同时守住「本地包不存在时绝不执行任何平台命令」这条边界。
func TestInstallRequiresDownloadedAsset(t *testing.T) {
	c, _ := assetServer(t, "linux", "pkg.deb", []byte("x"), config.UpdateSection{Repo: "o/r"})
	c.Check(context.Background(), true)

	_, err := c.Install(context.Background())
	if err == nil {
		t.Fatal("安装包未下载时应当拒绝安装")
	}
	if !strings.Contains(err.Error(), "下载") {
		t.Fatalf("报错要指向「先下载」，实际 %q", err.Error())
	}
}

// TestInstallRejectsWithoutUpdate 没有可升级版本时不该装任何东西。
func TestInstallRejectsWithoutUpdate(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.2", nil)) // 与当前版本相同
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{Repo: "o/r"})
	c.Check(context.Background(), true)

	if _, err := c.Install(context.Background()); err == nil {
		t.Fatal("已是最新时不应允许安装")
	}
}

// TestInstallUnsupportedPlatform plan9 这类平台没有可执行方案时应如实拒绝。
func TestInstallUnsupportedPlatform(t *testing.T) {
	c, _ := assetServer(t, "plan9", "pkg.deb", []byte("x"), config.UpdateSection{Repo: "o/r"})
	c.Check(context.Background(), true)

	if _, err := c.Install(context.Background()); err == nil {
		t.Fatal("未知平台应当拒绝安装")
	}
}

// TestStateExposesReadyAssetAndPlan 下好之后状态里要带「本地路径 + 本机安装方案」，
// 面板据此把「下载」换成「安装」（刷新页面也不必重新下载）。
func TestStateExposesReadyAssetAndPlan(t *testing.T) {
	const name = "workbuddy-gateway-desktop_0.9.3_amd64.deb"
	c, _ := assetServer(t, "linux", name, []byte("deb"), config.UpdateSection{Repo: "o/r"})
	c.Check(context.Background(), true)

	if st := c.State(); st.LocalPath != "" {
		t.Fatalf("还没下载时不该有本地路径：%q", st.LocalPath)
	}
	if _, err := c.Download(context.Background()); err != nil {
		t.Fatalf("下载失败: %v", err)
	}

	st := c.State()
	if st.LocalPath == "" || st.LocalBytes != 3 {
		t.Fatalf("下载后应报告本地就绪：%q %d", st.LocalPath, st.LocalBytes)
	}
	if !st.Install.Supported || !st.Install.Restart {
		t.Fatalf("linux 上应给出可安装方案：%+v", st.Install)
	}
	// 检测本身不再下载任何东西：这是「点更新按钮 → 下载 → 装」的第一步，
	// 与检查解耦。这里用「检查后本地仍然没有包」把它钉住。
	c2, dir2 := assetServer(t, "linux", name, []byte("deb"), config.UpdateSection{Repo: "o/r"})
	c2.Check(context.Background(), true)
	if _, err := os.Stat(filepath.Join(dir2, name)); err == nil {
		t.Fatal("检查更新不该顺手下载安装包（下载是用户点出来的一步）")
	}
}

// TestDownloadRoutesPreferDirectThenMirror 下载路线：**直连优先**，只有 GitHub 家族的
// 地址才追加镜像回退；本地/自建源不触发回退。
//
// 最后一条不只是省流量：测试与自建源用的都是本地地址，若也套镜像前缀，
// 每个失败的用例都会去打一次真实网络（这正是上一版测试里那套「显式关掉加速」
// 的由来 —— 现在由地址本身决定，不需要开关）。
func TestDownloadRoutesPreferDirectThenMirror(t *testing.T) {
	c := &Checker{}
	const raw = "https://github.com/o/r/releases/download/v1/app.deb"

	routes := c.downloadRoutes(raw)
	if len(routes) != 2 {
		t.Fatalf("GitHub 地址应有「直连 + 镜像」两条路线，实际 %d：%+v", len(routes), routes)
	}
	if !routes[0].direct || routes[0].url != raw {
		t.Fatalf("第一条路线必须是原地址直连：%+v", routes[0])
	}
	if want := DefaultDownloadMirror + "/" + raw; routes[1].url != want {
		t.Fatalf("第二条路线应是镜像拼接：%+v（期望 %s）", routes[1], want)
	}
	if routes[1].mirrorHost != "gh-proxy.org" {
		t.Fatalf("镜像路线要带上主机名（重定向白名单要用）：%+v", routes[1])
	}

	local := c.downloadRoutes("http://127.0.0.1:8080/dl/app.deb")
	if len(local) != 1 || !local[0].direct {
		t.Fatalf("非 GitHub 地址不该回退镜像：%+v", local)
	}
}

// TestPlanWarnsNpmInstalls 三个平台的方案都要提醒 npm 安装的实例别用系统安装包。
//
// 面板里下到的是 deb / dmg / setup.exe，而 npm 全局安装的实例该走 npm 自己那条路：
// 两者装的是同一份网关但落点不同，混着装会出现「装完还显示旧版本」的困惑。
func TestPlanWarnsNpmInstalls(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		if got := planFor(goos, "/tmp/pkg"); !strings.Contains(got.Note, "npm i -g") {
			t.Fatalf("%s 的方案没有提醒 npm 安装的实例：%q", goos, got.Note)
		}
	}
}

// TestWindowsLaunchFlags 两种 Windows 安装包的「无人值守」开关不能混用。
//
// 桌面版是 Tauri 的 NSIS 安装包：命令行开关 /S /R 生效（/S 静默、/R 装完拉起应用）。
// 命令行版是 IExpress 自解压包：它只认环境变量，传命令行开关会让 install.ps1 收到
// 不认识的位置参数而直接报错 —— 一个静默升级的修复不该把另一条安装路径弄坏。
func TestWindowsLaunchFlags(t *testing.T) {
	args, env := windowsLaunch(formDesktop, false)
	if strings.Join(args, " ") != "/S /R" {
		t.Fatalf("桌面版应当用 /S /R 调，实际 %v", args)
	}
	if len(env) != 0 {
		t.Fatalf("桌面版不该依赖环境变量，实际 %v", env)
	}

	args, env = windowsLaunch(formCLI, false)
	if len(args) != 0 {
		t.Fatalf("命令行版不该收到命令行开关（会被当成位置参数而报错），实际 %v", args)
	}
	if got := strings.Join(env, " "); got != "WB_SETUP_SILENT=1 WB_SETUP_FLAGS=-Launch" {
		t.Fatalf("命令行版的无人值守环境变量不对：%q", got)
	}

	// 用户原先开了登录自启时必须带上 -AutoStart：install.ps1 没有它就会按
	// 「不粘性」把那个计划任务删掉，静默升级不该顺手关掉用户的开机自启。
	_, env = windowsLaunch(formCLI, true)
	if got := strings.Join(env, " "); got != "WB_SETUP_SILENT=1 WB_SETUP_FLAGS=-AutoStart -Launch" {
		t.Fatalf("原先开了自启时应带上 -AutoStart：%q", got)
	}

	// 形态判不出来时按桌面版处理：仓库里 Windows 上只有这两种产物，而桌面版是
	// 公开发行的那个；挑错形态的问题由 pickAsset 的 formAllows 负责挡。
	if args, _ := windowsLaunch(formUnknown, false); strings.Join(args, " ") != "/S /R" {
		t.Fatalf("形态未知时按桌面版处理，实际 %v", args)
	}
}

// TestApplyDownloadMirror 加速前缀的拼接与校验：前缀只能是 origin，原始地址必须是 http(s)。
//
// 这个值决定**请求发去哪**，且来自用户手写的 config.json —— 校验松了会把
// 「半截 URL」拼成一个难懂的请求失败，不如在这里就报清楚。
func TestApplyDownloadMirror(t *testing.T) {
	raw := "https://github.com/o/r/releases/download/v1/pkg.deb"
	for mirror, want := range map[string]string{
		"https://ghfast.top":      "https://ghfast.top/" + raw,
		"https://ghfast.top/":     "https://ghfast.top/" + raw,
		"https://mirror.example/": "https://mirror.example/" + raw,
	} {
		got, err := ApplyDownloadMirror(mirror, raw)
		if err != nil {
			t.Fatalf("合法前缀 %q 报错: %v", mirror, err)
		}
		if got != want {
			t.Fatalf("拼接结果不对：mirror=%q got=%s want=%s", mirror, got, want)
		}
	}

	for _, bad := range []string{"ghfast.top", "ghfast.top/", "https://ghfast.top/sub", "https://ghfast.top/sub/", "ftp://x"} {
		if _, err := ApplyDownloadMirror(bad, raw); err == nil {
			t.Fatalf("非法前缀 %q 应当报错", bad)
		}
	}
	// 空前缀 = 原样返回（直连）。
	got, err := ApplyDownloadMirror("", raw)
	if err != nil || got != raw {
		t.Fatalf("空前缀应原样返回，实际 %q err=%v", got, err)
	}
}

// TestInstallFailureIsReported 安装失败要**如实报错**，而且要把话说清楚
// （面板把 err 直接显示给用户），进程不能崩。
//
// 用 plan9 当「失败」：它没有任何安装方案，Install 会在碰平台命令之前就被拒 ——
// 测试绝不能真的去调 pkexec。
func TestInstallFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_, _ = io.WriteString(w, releasesJSON(mkRelease("v0.9.3", map[string]any{
				"assets": []map[string]any{
					asset("pkg_amd64.deb", 3, srv.URL+"/dl/pkg_amd64.deb"),
				},
			})))
		case "/dl/pkg_amd64.deb":
			_, _ = w.Write([]byte("deb"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(testConfig(t, config.UpdateSection{Repo: "o/r"}), Options{
		Current: "0.9.2", Client: srv.Client(), APIBase: srv.URL,
		GOOS: "plan9", GOARCH: "amd64", DownloadDir: dir, Logf: t.Logf,
	})
	c.Check(context.Background(), true)
	if _, err := c.Download(context.Background()); err != nil {
		t.Fatalf("下载失败: %v", err)
	}

	_, err := c.Install(context.Background())
	if err == nil {
		t.Fatal("没有安装方案的平台应当报错")
	}
	if !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("报错要说清是哪个平台没有方案，实际 %q", err.Error())
	}
	// 失败之后「正在安装」标记必须复位，否则面板会一直置灰按钮。
	if st := c.State(); st.Installing {
		t.Fatal("安装失败后 Installing 应当复位")
	}
}

// TestDownloadReportsProgress 下载过程中 State() 要能报出进度（面板进度条的数据来源）。
//
// 服务端边下边写、面板每 800ms 轮询一次，所以这里也必须**并发**观察：
// 起一个慢速上游（分块 + 停顿），主协程轮询 State 直到看到 downloading=true、
// 已下载字节数在增长、总量等于资产大小。
func TestDownloadReportsProgress(t *testing.T) {
	const total = 64 * 1024
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dl/pkg_amd64.deb" {
			_, _ = io.WriteString(w, releasesJSON(mkRelease("v0.9.3", map[string]any{
				"assets": []map[string]any{
					asset("pkg_amd64.deb", total, srv.URL+"/dl/pkg_amd64.deb"),
				},
			})))
			return
		}
		chunk := make([]byte, 4*1024)
		for i := 0; i < total/len(chunk); i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(5 * time.Millisecond) // 拖慢，留出观察窗口
		}
	}))
	defer srv.Close()

	c := New(testConfig(t, config.UpdateSection{Repo: "o/r"}), Options{
		Current: "0.9.2", Client: srv.Client(), APIBase: srv.URL,
		GOOS: "linux", GOARCH: "amd64", DownloadDir: t.TempDir(), Logf: t.Logf,
	})
	if st := c.Check(context.Background(), true); st.Error != "" {
		t.Fatalf("前置检查失败: %s", st.Error)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.Download(context.Background())
		done <- err
	}()

	sawActive, sawGrowth := false, int64(0)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := c.State()
		if st.Downloading {
			sawActive = true
			if st.DownloadTotal != total {
				t.Fatalf("总量应等于资产大小 %d，实际 %d", total, st.DownloadTotal)
			}
			if st.Downloaded > sawGrowth {
				sawGrowth = st.Downloaded
			}
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("下载失败: %v", err)
			}
			if !sawActive {
				t.Fatal("整个下载过程都没看到 downloading=true，进度条不会有数据")
			}
			if sawGrowth <= 0 {
				t.Fatal("没见过已下载字节数增长，进度条不会动")
			}
			// 下载结束后必须清零，否则面板会一直转圈。
			if st := c.State(); st.Downloading || st.Downloaded != 0 {
				t.Fatalf("下载结束后进度未清零: %+v", st)
			}
			return
		default:
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatal("下载没有在 5 秒内结束")
}
