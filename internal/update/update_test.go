package update

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/eventlog"
)

// testConfig 造一份只带 update 段的配置。
//
// 不调 config.Load：那会去读工作目录里的 config.json，测试结果就会随开发机上的
// 配置变化 —— 而这里的每条断言都只该取决于测试自己写下的输入。
func testConfig(t *testing.T, sec config.UpdateSection) *config.Config {
	t.Helper()
	cfg := &config.Config{WorkDir: t.TempDir(), Update: sec}
	return cfg
}

func boolPtr(v bool) *bool { return &v }

// fakeGitHub 起一个假 GitHub：返回固定的 Release 列表，并记录收到的请求头。
type fakeGitHub struct {
	*httptest.Server
	status   int
	body     string
	lastAuth string
	lastUA   string
	lastPath string
	calls    int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.lastAuth = r.Header.Get("Authorization")
		f.lastUA = r.Header.Get("User-Agent")
		f.lastPath = r.URL.Path
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, `{"message":"nope"}`)
			return
		}
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(f.Close)
	return f
}

// releasesJSON 拼一份 GitHub 风格的 Release 列表。
func releasesJSON(items ...map[string]any) string {
	b, _ := json.Marshal(items)
	return string(b)
}

func mkRelease(tag string, extra map[string]any) map[string]any {
	r := map[string]any{
		"tag_name":     tag,
		"name":         "桌面版 " + tag,
		"html_url":     "https://github.com/zyp556678/wb-helper-main/releases/tag/" + tag,
		"body":         "## 更新内容\n- 修了点东西",
		"draft":        false,
		"prerelease":   false,
		"published_at": "2026-10-08T02:54:38Z",
		"assets":       []map[string]any{},
	}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func asset(name string, size int64, url string) map[string]any {
	return map[string]any{"name": name, "size": size, "browser_download_url": url}
}

func newChecker(t *testing.T, f *fakeGitHub, cur string, goos, goarch string, sec config.UpdateSection) *Checker {
	t.Helper()
	return New(testConfig(t, sec), Options{
		Current:     cur,
		Client:      f.Client(),
		APIBase:     f.URL,
		GOOS:        goos,
		GOARCH:      goarch,
		DownloadDir: t.TempDir(),
		Logf:        t.Logf,
	})
}

// ---------------------------------------------------------------------------
// 版本号比较
// ---------------------------------------------------------------------------

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.9.2", "0.9.2", 0},
		{"v0.9.2", "0.9.2", 0},
		{"0.9.2", "0.10.0", -1},
		{"0.10.0", "0.9.9", 1},
		// 字符串比较会在这里给出相反的答案（"9" > "10"），这正是需要真解析的理由。
		{"0.9.10", "0.9.9", 1},
		{"1.0", "1.0.0", 0},
		{"1.0.1", "1.0", 1},
		// 预发布版低于同号的正式版：否则 v0.9.0-slice8 会被当成比 0.9.0 还新。
		{"0.9.0-slice8", "0.9.0", -1},
		{"0.9.0", "0.9.0-slice8", 1},
		{"1.0.0-rc.1", "1.0.0-rc.2", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-1", "1.0.0-alpha", -1},
		{"1.0.0+build.7", "1.0.0", 0},
	}
	for _, tc := range cases {
		got, ok := CompareVersions(tc.a, tc.b)
		if !ok {
			t.Fatalf("CompareVersions(%q, %q) 应当可解析", tc.a, tc.b)
		}
		if got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d，期望 %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCompareVersionsRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"dev", "", "v", "x.y.z", "1.2.-3"} {
		if _, ok := CompareVersions(bad, "1.0.0"); ok {
			t.Errorf("%q 不该被当成版本号解析成功", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// 挑 Release
// ---------------------------------------------------------------------------

func TestPickReleaseSkipsDraftsAndPrereleases(t *testing.T) {
	var rels []release
	for _, raw := range []map[string]any{
		mkRelease("v0.9.1", nil),
		mkRelease("v0.9.2", map[string]any{"draft": true}),
		mkRelease("v0.9.3-rc1", map[string]any{"prerelease": true}),
		mkRelease("v0.9.4-rc1", nil), // tag 带后缀但没打 prerelease 标记，同样要跳过
	} {
		rels = append(rels, toRelease(t, raw))
	}
	got, ok := pickRelease(rels, false)
	if !ok {
		t.Fatal("应当挑出一个稳定版")
	}
	if got.TagName != "v0.9.1" {
		t.Errorf("应挑 v0.9.1，实际 %q", got.TagName)
	}

	got, ok = pickRelease(rels, true)
	if !ok || got.TagName != "v0.9.4-rc1" {
		t.Errorf("允许预发布时应挑出版本号最大的 v0.9.4-rc1，实际 %v ok=%v", got.TagName, ok)
	}
}

// 补发旧版本时，列表顺序（创建时间倒序）与版本号顺序不一致 —— 必须按版本号挑。
func TestPickReleaseUsesHighestVersionNotListOrder(t *testing.T) {
	rels := []release{
		toRelease(t, mkRelease("v0.9.1", nil)),
		toRelease(t, mkRelease("v0.9.3", nil)),
		toRelease(t, mkRelease("v0.9.2", nil)),
	}
	got, _ := pickRelease(rels, false)
	if got.TagName != "v0.9.3" {
		t.Errorf("应按版本号取最大，实际 %q", got.TagName)
	}
}

func toRelease(t *testing.T, raw map[string]any) release {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("造 Release 失败: %v", err)
	}
	var r release
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("造 Release 失败: %v", err)
	}
	return r
}

// ---------------------------------------------------------------------------
// 挑平台安装包
// ---------------------------------------------------------------------------

func TestPickAssetPerPlatform(t *testing.T) {
	assets := []releaseAsset{
		{Name: "workbuddy-gateway-desktop_0.9.3_amd64.deb"},
		{Name: "workbuddy-gateway-desktop_0.9.3_arm64.deb"},
		{Name: "workbuddy-gateway-desktop_0.9.3_arm64.dmg"},
		{Name: "workbuddy-gateway-desktop_0.9.3_amd64.dmg"},
		{Name: "workbuddy-gateway-desktop_0.9.3_x64-setup.exe"},
		{Name: "workbuddy-gateway-desktop_0.9.3_arm64-setup.exe"},
	}
	cases := []struct {
		goos, goarch string
		want         string
	}{
		{"linux", "amd64", "workbuddy-gateway-desktop_0.9.3_amd64.deb"},
		{"linux", "arm64", "workbuddy-gateway-desktop_0.9.3_arm64.deb"},
		{"darwin", "arm64", "workbuddy-gateway-desktop_0.9.3_arm64.dmg"},
		{"darwin", "amd64", "workbuddy-gateway-desktop_0.9.3_amd64.dmg"},
		{"windows", "amd64", "workbuddy-gateway-desktop_0.9.3_x64-setup.exe"},
		{"windows", "arm64", "workbuddy-gateway-desktop_0.9.3_arm64-setup.exe"},
	}
	for _, tc := range cases {
		got, ok := pickAsset(assets, tc.goos, tc.goarch)
		if !ok {
			t.Errorf("%s/%s 应当匹配到安装包", tc.goos, tc.goarch)
			continue
		}
		if got.Name != tc.want {
			t.Errorf("%s/%s 应匹配 %q，实际 %q", tc.goos, tc.goarch, tc.want, got.Name)
		}
	}
}

// 只发过 arm64 的 dmg 时，Intel Mac 不该拿到一个 arm64 的包 —— 宁可没有。
func TestPickAssetDoesNotFallBackToWrongArch(t *testing.T) {
	assets := []releaseAsset{{Name: "workbuddy-gateway-desktop_0.9.3_arm64.dmg"}}
	if got, ok := pickAsset(assets, "darwin", "amd64"); ok {
		t.Errorf("不该把 arm64 的包发给 amd64，实际给了 %q", got.Name)
	}
}

// ---------------------------------------------------------------------------
// 端到端（假 GitHub）
// ---------------------------------------------------------------------------

func TestCheckFindsNewVersion(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.1", nil), mkRelease("v0.9.3", map[string]any{
		"assets": []map[string]any{
			asset("workbuddy-gateway-desktop_0.9.3_amd64.deb", 12*1024*1024,
				"https://github.com/zyp556678/wb-helper-main/releases/download/v0.9.3/x.deb"),
		},
	}))
	events := eventlog.New(10)
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{})
	c.events = events

	st := c.Check(context.Background(), true)
	if st.Error != "" {
		t.Fatalf("不该有错误: %s", st.Error)
	}
	if !st.UpdateAvailable {
		t.Fatal("0.9.2 → v0.9.3 应当判定为有更新")
	}
	if st.Latest != "0.9.3" {
		t.Errorf("Latest 应当去掉 v 前缀，实际 %q", st.Latest)
	}
	if st.AssetName != "workbuddy-gateway-desktop_0.9.3_amd64.deb" {
		t.Errorf("应当匹配到 linux/amd64 的 deb，实际 %q", st.AssetName)
	}
	if st.CheckedAt == 0 {
		t.Error("成功检查后应当记下时间")
	}
	if f.lastUA == "" {
		t.Error("GitHub 要求非空 User-Agent")
	}
	if !strings.Contains(f.lastPath, "/repos/zyp556678/wb-helper-main/releases") {
		t.Errorf("请求路径不对: %s", f.lastPath)
	}
	if hits := events.Query(eventlog.Query{}).Total; hits == 0 {
		t.Error("发现新版本应当记一条事件")
	}
	if hits := events.Query(eventlog.Query{}).Total; hits != 1 {
		t.Errorf("重复检查不该重复记事件，实际 %d 条", hits)
	}
}

func TestCheckReportsUpToDate(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.2", nil))
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{})
	st := c.Check(context.Background(), true)
	if st.UpdateAvailable {
		t.Fatal("同版本不该报有更新")
	}
	if st.Error != "" {
		t.Errorf("不该有错误: %s", st.Error)
	}
}

// 没配令牌 + 私有仓库 → 404。必须报「需要令牌」，绝不能报「已是最新」。
func TestCheckPrivateRepoWithoutTokenExplainsItself(t *testing.T) {
	f := newFakeGitHub(t)
	f.status = http.StatusNotFound
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{})
	st := c.Check(context.Background(), true)
	if st.Error == "" || !strings.Contains(st.Error, "令牌") {
		t.Fatalf("应当提示需要令牌，实际 %q", st.Error)
	}
	if st.UpdateAvailable {
		t.Error("查不到时不该声称有更新")
	}
}

func TestCheckSendsTokenWhenConfigured(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.2", nil))
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{Token: "ghp_test"})
	c.Check(context.Background(), true)
	if f.lastAuth != "Bearer ghp_test" {
		t.Errorf("配了令牌就应当带上，实际 %q", f.lastAuth)
	}
}

// 环境变量是容器部署的主通路：配置文件里不写令牌也得能查。
func TestCheckTokenFromEnv(t *testing.T) {
	t.Setenv("WB_UPDATE_TOKEN", "env_token")
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.2", nil))
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{})
	st := c.Check(context.Background(), true)
	if f.lastAuth != "Bearer env_token" {
		t.Errorf("应当用环境变量里的令牌，实际 %q", f.lastAuth)
	}
	if !st.TokenSet {
		t.Error("State 应当如实反映「配了令牌」")
	}
}

// 开发期版本号（"dev"）不是 semver：只显示最新版本，不能声称有更新。
func TestCheckNonSemverCurrentVersionDoesNotClaimUpdate(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.2", nil))
	c := newChecker(t, f, "dev", "linux", "amd64", config.UpdateSection{})
	st := c.Check(context.Background(), true)
	if st.UpdateAvailable {
		t.Fatal("当前版本无法比较时不该声称有更新")
	}
	if !strings.Contains(st.Detail, "无法比较") {
		t.Errorf("应当说明无法比较，实际 %q", st.Detail)
	}
}

func TestCheckSkipsRepeatedCallsWithinMinGap(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.2", nil))
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{})
	c.Check(context.Background(), true)
	if f.calls != 1 {
		t.Fatalf("第一次检查应当发一次请求，实际 %d", f.calls)
	}
	// 面板刷新顺带触发的检查（force=false）在 minGap 内应当直接吃缓存。
	c.Check(context.Background(), false)
	if f.calls != 1 {
		t.Errorf("minGap 内不该重复打上游，实际 %d 次", f.calls)
	}
	// 用户显式点「检查更新」必须真的去查。
	c.Check(context.Background(), true)
	if f.calls != 2 {
		t.Errorf("显式检查应当重新请求，实际 %d 次", f.calls)
	}
}

func TestCheckSurvivesNetworkError(t *testing.T) {
	f := newFakeGitHub(t)
	f.Close() // 端口关掉，请求必然失败
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{})
	st := c.Check(context.Background(), true)
	if st.Error == "" {
		t.Fatal("网络失败应当如实记在 State.Error 里，而不是抛给调用方崩溃")
	}
	if st.CheckedAt != 0 {
		t.Error("失败的检查不该更新 CheckedAt")
	}
}

// ---------------------------------------------------------------------------
// 下载
// ---------------------------------------------------------------------------

func TestDownloadWritesAssetAtomically(t *testing.T) {
	payload := []byte("deb-bytes")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_, _ = io.WriteString(w, releasesJSON(mkRelease("v0.9.3", map[string]any{
				"assets": []map[string]any{
					asset("workbuddy-gateway-desktop_0.9.3_amd64.deb", int64(len(payload)),
						srv.URL+"/dl/workbuddy-gateway-desktop_0.9.3_amd64.deb"),
				},
			})))
		case "/dl/workbuddy-gateway-desktop_0.9.3_amd64.deb":
			_, _ = w.Write(payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := New(testConfig(t, config.UpdateSection{Repo: "o/r"}), Options{
		Current: "0.9.2", Client: srv.Client(), APIBase: srv.URL,
		GOOS: "linux", GOARCH: "amd64", DownloadDir: dir, Logf: t.Logf,
	})
	c.Check(context.Background(), true)

	res, err := c.Download(context.Background())
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if res.Skipped {
		t.Error("第一次下载不该是跳过")
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("读回下载结果失败: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("文件内容不对: %q", got)
	}
	if filepath.Dir(res.Path) != dir {
		t.Errorf("应当落在配置的目录里，实际 %s", res.Path)
	}
	if _, err := os.Stat(res.Path + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Error("临时文件应当已被改名，不该留在磁盘上")
	}

	// 再来一次：大小一致就跳过，不重复占带宽。
	again, err := c.Download(context.Background())
	if err != nil {
		t.Fatalf("重复下载失败: %v", err)
	}
	if !again.Skipped {
		t.Error("本地已有同大小文件时应当跳过")
	}
}

// 端点不接受客户端传 URL，所以「没有匹配资产」时应当明确拒绝，而不是去下载点什么。
func TestDownloadWithoutAssetFails(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.3", nil)) // 没有任何附件
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{})
	c.Check(context.Background(), true)
	if _, err := c.Download(context.Background()); err == nil {
		t.Fatal("没有可下载资产时应当报错")
	}
}

func TestAssetFileNameRejectsTraversal(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r/releases/download/v1/pkg.deb": "pkg.deb",
		"https://github.com/o/r/releases/download/v1/":        "",
		"file:///etc/passwd":                                  "",
		"https://github.com/../../etc/passwd":                 "passwd",
	}
	for raw, want := range cases {
		if got := assetFileName(raw); got != want {
			t.Errorf("assetFileName(%q) = %q，期望 %q", raw, got, want)
		}
	}
}

func TestAllowedAssetHost(t *testing.T) {
	ok := []string{"github.com", "objects.githubusercontent.com", "127.0.0.1", "localhost", "release-assets.githubusercontent.com"}
	for _, h := range ok {
		if !allowedAssetHost(h) {
			t.Errorf("%s 应当被允许", h)
		}
	}
	bad := []string{"", "evil.com", "github.com.evil.com", "10.0.0.1"}
	for _, h := range bad {
		if allowedAssetHost(h) {
			t.Errorf("%s 不该被允许", h)
		}
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func TestValidRepo(t *testing.T) {
	for _, good := range []string{"o/r", "zyp556678/wb-helper-main"} {
		if !validRepo(good) {
			t.Errorf("%q 应当合法", good)
		}
	}
	for _, bad := range []string{"", "r", "o/r/x", "o r/x"} {
		if validRepo(bad) {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("短", 10); got != "短" {
		t.Errorf("短文本不该被改动，实际 %q", got)
	}
	// 按字符截断：不能把多字节字符切成乱码。
	got := truncateRunes(strings.Repeat("更", 10), 3)
	if !strings.HasPrefix(got, "更更更") {
		t.Errorf("应当保留前 3 个字符，实际 %q", got)
	}
}

// 后台循环不该在「显式关掉自动检查」时打上游。
func TestLoopRespectsDisabled(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.3", nil))
	c := New(testConfig(t, config.UpdateSection{Enabled: boolPtr(false)}), Options{
		Current: "0.9.2", Client: f.Client(), APIBase: f.URL,
		GOOS: "linux", GOARCH: "amd64", DownloadDir: t.TempDir(),
		Logf: t.Logf, StartupDelay: time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	time.Sleep(50 * time.Millisecond)
	if f.calls != 0 {
		t.Errorf("关掉自动检查后不该发请求，实际 %d 次", f.calls)
	}
	if st := c.State(); st.AutoCheck {
		t.Error("State.AutoCheck 应当如实反映配置")
	}
}

// 大文件下载断在中间是常事：瞬时中断应当自动重试，而不是让用户从头再来。
func TestDownloadRetriesTransientFailure(t *testing.T) {
	payload := []byte(strings.Repeat("x", 8192))
	var srv *httptest.Server
	attempts := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_, _ = io.WriteString(w, releasesJSON(mkRelease("v0.9.3", map[string]any{
				"assets": []map[string]any{
					asset("pkg_0.9.3_amd64.deb", int64(len(payload)), srv.URL+"/dl/pkg_0.9.3_amd64.deb"),
				},
			})))
		case "/dl/pkg_0.9.3_amd64.deb":
			attempts++
			if attempts == 1 {
				// 声明 8192 字节却只写 100 字节就断开连接 —— 客户端读到 unexpected EOF。
				w.Header().Set("Content-Length", "8192")
				_, _ = w.Write(payload[:100])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				panic(http.ErrAbortHandler)
			}
			_, _ = w.Write(payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := New(testConfig(t, config.UpdateSection{Repo: "o/r"}), Options{
		Current: "0.9.2", Client: srv.Client(), APIBase: srv.URL,
		GOOS: "linux", GOARCH: "amd64", DownloadDir: dir, Logf: t.Logf,
	})
	c.Check(context.Background(), true)

	res, err := c.Download(context.Background())
	if err != nil {
		t.Fatalf("瞬时中断应当自动重试并成功，实际失败: %v", err)
	}
	if attempts != 2 {
		t.Errorf("应当重试一次（共 2 次请求），实际 %d 次", attempts)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("读回文件失败: %v", err)
	}
	if len(got) != len(payload) {
		t.Errorf("重试后的文件大小不对：%d，期望 %d", len(got), len(payload))
	}
	if _, err := os.Stat(res.Path + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Error("中断留下的临时文件应当被清掉")
	}
}

// 确定性失败（404）不该重试：重试一百次也不会变，只会拖住面板。
func TestDownloadDoesNotRetryHTTPError(t *testing.T) {
	var srv *httptest.Server
	attempts := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_, _ = io.WriteString(w, releasesJSON(mkRelease("v0.9.3", map[string]any{
				"assets": []map[string]any{
					asset("pkg_0.9.3_amd64.deb", 10, srv.URL+"/dl/pkg_0.9.3_amd64.deb"),
				},
			})))
		default:
			attempts++
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(testConfig(t, config.UpdateSection{Repo: "o/r"}), Options{
		Current: "0.9.2", Client: srv.Client(), APIBase: srv.URL,
		GOOS: "linux", GOARCH: "amd64", DownloadDir: t.TempDir(), Logf: t.Logf,
	})
	c.Check(context.Background(), true)
	if _, err := c.Download(context.Background()); err == nil {
		t.Fatal("404 时应当报错")
	}
	if attempts != 1 {
		t.Errorf("HTTP 404 不该重试，实际请求 %d 次", attempts)
	}
}

// 一次网络抖动不该把已经查到的结果抹掉：失败时保留上次的成功结果，只补一个错误说明。
func TestCheckKeepsLastResultOnFailure(t *testing.T) {
	f := newFakeGitHub(t)
	f.body = releasesJSON(mkRelease("v0.9.3", map[string]any{
		"assets": []map[string]any{
			asset("pkg_0.9.3_amd64.deb", 10, "https://github.com/o/r/releases/download/v0.9.3/pkg.deb"),
		},
	}))
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{Repo: "o/r"})
	good := c.Check(context.Background(), true)
	if good.Error != "" || !good.UpdateAvailable {
		t.Fatalf("前置检查应当成功且发现更新，实际 %+v", good)
	}

	f.status = http.StatusInternalServerError
	bad := c.Check(context.Background(), true)
	if bad.Error == "" {
		t.Fatal("上游报错时应当记下 Error")
	}
	if bad.Latest != "0.9.3" || !bad.UpdateAvailable || bad.AssetName == "" {
		t.Errorf("失败时应当保留上次的成功结果，实际 %+v", bad)
	}
	if bad.CheckedAt != good.CheckedAt {
		t.Error("失败的检查不该改写「上次成功检查时间」")
	}
}

// 检查一直失败时也要节流：否则面板每刷新一次就补打一次上游。
func TestCheckThrottlesAfterFailure(t *testing.T) {
	f := newFakeGitHub(t)
	f.status = http.StatusInternalServerError
	c := newChecker(t, f, "0.9.2", "linux", "amd64", config.UpdateSection{Repo: "o/r"})
	c.Check(context.Background(), true)
	if f.calls != 1 {
		t.Fatalf("第一次应当发一次请求，实际 %d", f.calls)
	}
	c.Check(context.Background(), false)
	if f.calls != 1 {
		t.Errorf("失败后 minGap 内不该重复请求，实际 %d 次", f.calls)
	}
}

// 私有仓库的附件必须走 API 资产端点：/releases/download/... 认的是浏览器会话
// Cookie，带 Bearer 令牌一样 404（实测踩到过）。这条测试把「用哪个地址」钉住。
func TestDownloadUsesAPIEndpointWhenTokenSet(t *testing.T) {
	payload := []byte("private-deb")
	var srv *httptest.Server
	var assetPath, assetAuth string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/releases":
			_, _ = io.WriteString(w, releasesJSON(mkRelease("v0.9.3", map[string]any{
				"assets": []map[string]any{{
					"name":                 "pkg_0.9.3_amd64.deb",
					"size":                 len(payload),
					"browser_download_url": srv.URL + "/dl/pkg_0.9.3_amd64.deb",
					"url":                  srv.URL + "/repos/o/r/releases/assets/42",
					"id":                   42,
				}},
			})))
		case r.URL.Path == "/repos/o/r/releases/assets/42":
			assetPath, assetAuth = r.URL.Path, r.Header.Get("Authorization")
			if r.Header.Get("Accept") != "application/octet-stream" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write(payload)
		default:
			// 浏览器下载链接在私有仓库上会 404 —— 请求打到这儿就说明选错了地址。
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(testConfig(t, config.UpdateSection{Repo: "o/r", Token: "ghp_x"}), Options{
		Current: "0.9.2", Client: srv.Client(), APIBase: srv.URL,
		GOOS: "linux", GOARCH: "amd64", DownloadDir: t.TempDir(), Logf: t.Logf,
	})
	c.Check(context.Background(), true)
	res, err := c.Download(context.Background())
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if assetPath != "/repos/o/r/releases/assets/42" {
		t.Errorf("应当走 API 资产端点，实际请求了 %q", assetPath)
	}
	if assetAuth != "Bearer ghp_x" {
		t.Errorf("API 端点应当带令牌，实际 %q", assetAuth)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil || string(got) != string(payload) {
		t.Errorf("文件内容不对: %q err=%v", got, err)
	}
}

// 没配令牌（公开仓库）时用浏览器下载链接，省一次 API 调用。
func TestDownloadUsesBrowserURLWithoutToken(t *testing.T) {
	payload := []byte("public-deb")
	var srv *httptest.Server
	var hit string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/releases":
			_, _ = io.WriteString(w, releasesJSON(mkRelease("v0.9.3", map[string]any{
				"assets": []map[string]any{{
					"name":                 "pkg_0.9.3_amd64.deb",
					"size":                 len(payload),
					"browser_download_url": srv.URL + "/dl/pkg_0.9.3_amd64.deb",
					"url":                  srv.URL + "/repos/o/r/releases/assets/42",
					"id":                   42,
				}},
			})))
		default:
			hit = r.URL.Path
			_, _ = w.Write(payload)
		}
	}))
	defer srv.Close()

	c := New(testConfig(t, config.UpdateSection{Repo: "o/r"}), Options{
		Current: "0.9.2", Client: srv.Client(), APIBase: srv.URL,
		GOOS: "linux", GOARCH: "amd64", DownloadDir: t.TempDir(), Logf: t.Logf,
	})
	c.Check(context.Background(), true)
	if _, err := c.Download(context.Background()); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if hit != "/dl/pkg_0.9.3_amd64.deb" {
		t.Errorf("无令牌时应当走浏览器下载链接，实际 %q", hit)
	}
}

// 面板连点两下「下载安装包」不能让两个请求同时写同一个临时文件：
// 实测出现过「一个请求清理临时文件 → 另一个的 rename 报 no such file」。
func TestDownloadIsSerialized(t *testing.T) {
	payload := []byte(strings.Repeat("d", 64*1024))
	var srv *httptest.Server
	var assetHits int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_, _ = io.WriteString(w, releasesJSON(mkRelease("v0.9.3", map[string]any{
				"assets": []map[string]any{
					asset("pkg_0.9.3_amd64.deb", int64(len(payload)), srv.URL+"/dl/pkg_0.9.3_amd64.deb"),
				},
			})))
		case "/dl/pkg_0.9.3_amd64.deb":
			atomic.AddInt32(&assetHits, 1)
			// 慢一点，制造两个请求重叠的窗口。
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write(payload)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := New(testConfig(t, config.UpdateSection{Repo: "o/r"}), Options{
		Current: "0.9.2", Client: srv.Client(), APIBase: srv.URL,
		GOOS: "linux", GOARCH: "amd64", DownloadDir: dir, Logf: t.Logf,
	})
	c.Check(context.Background(), true)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	results := make([]DownloadResult, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.Download(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发下载第 %d 个失败: %v", i, err)
		}
	}
	// 只应当真的下一遍：第二个请求要么等锁后看到文件已存在（skipped），
	// 要么干脆没触发第二次请求。
	if got := atomic.LoadInt32(&assetHits); got > 2 {
		t.Errorf("不该重复下载多次，实际请求 %d 次", got)
	}
	skipped := 0
	for _, r := range results {
		if r.Skipped {
			skipped++
		}
	}
	if skipped != 1 {
		t.Errorf("应当恰好有一个请求走「已存在」分支，实际 %d 个", skipped)
	}
	// 临时文件不能留下任何残渣。
	left, _ := filepath.Glob(filepath.Join(dir, "*.part*"))
	if len(left) != 0 {
		t.Errorf("不该留下临时文件: %v", left)
	}
	got, err := os.ReadFile(results[0].Path)
	if err != nil || len(got) != len(payload) {
		t.Errorf("文件不完整: %d 字节，err=%v", len(got), err)
	}
}

// 上次异常退出留下的临时文件要被清掉：它们不会被复用，却一直占着磁盘。
func TestDownloadCleansStaleTempFiles(t *testing.T) {
	payload := []byte("deb")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases":
			_, _ = io.WriteString(w, releasesJSON(mkRelease("v0.9.3", map[string]any{
				"assets": []map[string]any{
					asset("pkg_0.9.3_amd64.deb", int64(len(payload)), srv.URL+"/dl/pkg_0.9.3_amd64.deb"),
				},
			})))
		default:
			_, _ = w.Write(payload)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	stale := filepath.Join(dir, "pkg_0.9.3_amd64.deb.part-123456")
	if err := os.WriteFile(stale, []byte("half-written"), 0o644); err != nil {
		t.Fatalf("造陈旧临时文件失败: %v", err)
	}

	c := New(testConfig(t, config.UpdateSection{Repo: "o/r"}), Options{
		Current: "0.9.2", Client: srv.Client(), APIBase: srv.URL,
		GOOS: "linux", GOARCH: "amd64", DownloadDir: dir, Logf: t.Logf,
	})
	c.Check(context.Background(), true)
	if _, err := c.Download(context.Background()); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("陈旧的临时文件应当被清掉，实际仍在: %v", err)
	}
}
