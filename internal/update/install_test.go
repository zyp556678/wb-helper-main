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
	// 自动下载在后台跑：等它收尾，否则它会在测试结束后 t.Logf（panic）。
	t.Cleanup(c.WaitBackground)
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
		t.Fatalf("windows 方案应为「支持 + 由安装程序提权」，实际 %+v", win)
	}
	if win.Restart {
		t.Fatal("windows 上不该承诺自动重启：exe 被锁，重启由安装程序与用户完成")
	}
	if win.Note == "" {
		t.Fatal("windows 方案必须提醒会弹安装向导")
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
	c, _ := assetServer(t, "linux", "pkg.deb", []byte("x"),
		config.UpdateSection{Repo: "o/r", AutoDownload: boolPtr(false)})
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
	c, _ := assetServer(t, "plan9", "pkg.deb", []byte("x"),
		config.UpdateSection{Repo: "o/r", AutoDownload: boolPtr(false)})
	c.Check(context.Background(), true)

	if _, err := c.Install(context.Background()); err == nil {
		t.Fatal("未知平台应当拒绝安装")
	}
}

// TestStateExposesReadyAssetAndPlan 下好之后状态里要带「本地路径 + 本机安装方案」，
// 面板据此把「下载」换成「安装并重启」（刷新页面也不必重新下载）。
func TestStateExposesReadyAssetAndPlan(t *testing.T) {
	const name = "workbuddy-gateway-desktop_0.9.3_amd64.deb"
	c, _ := assetServer(t, "linux", name, []byte("deb"),
		config.UpdateSection{Repo: "o/r", AutoDownload: boolPtr(false)})
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
	// 关掉自动下载后状态要如实反映（面板据此渲染开关）。
	c.SetConfig(testConfig(t, config.UpdateSection{Repo: "o/r", AutoDownload: boolPtr(false)}))
	if st := c.State(); st.AutoDownload {
		t.Fatal("配置关掉自动下载后状态里不该还是 true")
	}
}

// TestAutoDownloadDefaultsTrue 缺省（配置里没写 auto_download）就是「自动下载」。
//
// 这条钉的是一个判断：下载只是往数据目录放一个文件，不改动已安装的程序；
// 默认关的代价是用户点开面板还得等一次几十 MB 的下载。
func TestAutoDownloadDefaultsTrue(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.3", nil))
	c := New(testConfig(t, config.UpdateSection{Repo: "o/r"}), Options{
		Current: "0.9.2", Client: f.Client(), APIBase: f.URL,
		GOOS: "linux", GOARCH: "amd64", DownloadDir: t.TempDir(), Logf: t.Logf,
	})
	t.Cleanup(c.WaitBackground)

	if !c.State().AutoDownload {
		t.Fatal("没配 auto_download 时应当默认开启")
	}
	if !c.settings().autoDownload {
		t.Fatal("检查路径读到的开关也应当是开")
	}
}

// TestCheckAutoDownloads 发现新版本会自动把安装包下好（默认开）。
func TestCheckAutoDownloads(t *testing.T) {
	const name = "workbuddy-gateway-desktop_0.9.3_amd64.deb"
	payload := []byte("deb-bytes")
	c, dir := assetServer(t, "linux", name, payload, config.UpdateSection{Repo: "o/r"})

	c.Check(context.Background(), true)

	dest := filepath.Join(dir, name)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if data, err := os.ReadFile(dest); err == nil {
			if string(data) != string(payload) {
				t.Fatalf("自动下载的内容不对: %q", data)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("检查发现新版本后没有自动下载安装包")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestCheckAutoDownloadDisabled 关掉开关后不再自动下载（手动下载仍然可用）。
func TestCheckAutoDownloadDisabled(t *testing.T) {
	const name = "workbuddy-gateway-desktop_0.9.3_amd64.deb"
	c, dir := assetServer(t, "linux", name, []byte("deb"),
		config.UpdateSection{Repo: "o/r", AutoDownload: boolPtr(false)})

	c.Check(context.Background(), true)
	time.Sleep(200 * time.Millisecond) // 给后台 goroutine 一点时间（它本就不该被启动）

	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		t.Fatal("关掉自动下载后不该有文件落盘")
	}
	// 关掉自动下载不影响手动下载。
	if _, err := c.Download(context.Background()); err != nil {
		t.Fatalf("手动下载不该受开关影响: %v", err)
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
