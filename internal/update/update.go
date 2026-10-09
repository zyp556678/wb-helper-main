// Package update 检查有没有新版本，并把匹配当前平台的安装包下到本地。
//
// # 为什么这件事只能由网关做
//
// 面板的响应头里 CSP 是 `connect-src 'self'`（见 internal/server/server.go 的
// panelSecurity），浏览器里根本发不出跨域请求；桌面壳的职责又被刻意收窄成
// 「进程代管 + 窗口 + 托盘」，不含业务逻辑。于是「问一句有没有新版本」只有网关能做
// —— 这也正好：出站代理（`-proxy`）只有它认，私有仓库的令牌也只有它该拿。
//
// # 为什么只做「检测 + 下载」，不做自更新
//
// 安装包是 deb / dmg / setup.exe 三种平台专用格式，没有一种是「换掉文件就完事」：
//
//   - Windows：正在运行的 exe 被系统锁住，必须另起一个 updater 进程等父进程退出
//     后再替换，还要处理替换失败时的回滚；
//   - Linux：直接覆盖 /usr 下的文件会让 dpkg 的包数据库与磁盘内容不一致，正确做法
//     是交给 dpkg 装新包；
//   - macOS：App 是 bundle，要整包替换并处理 Gatekeeper 隔离属性；
//   - npm 形态：正确做法是 `npm i -g workbuddy-gateway@latest`，覆盖文件反而绕过了
//     npm 的完整性校验（见 npm/scripts/install.js 顶部对「不联网下载二进制」的说明）。
//
// 更关键的是失败代价：自更新失败会留下一个「既起不来、也没有安装包可回退」的现场，
// 而用户手里的 CLI / IDE 全都指着这个端口。所以这里停在「下好安装包 + 告诉用户
// 装哪个文件」，最后一步交给用户点 —— 这比做一个半可靠的自更新进程诚实得多。
//
// # 更新源是 GitHub Release，且默认仓库是私有的
//
// 未认证访问私有仓库的 Release 接口一律返回 404（GitHub 连「这个仓库存在」都不告诉
// 未认证方），所以必须支持配一个只读令牌（config.json 的 update.token，或环境变量
// WB_UPDATE_TOKEN）。**没配令牌时如实报「需要令牌」**，绝不报「已是最新」——
// 把「查不到」说成「没有新版本」会让用户永远等不到更新，而且看不出哪里配错了。
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/debuglog"
	"workbuddy-gateway/internal/eventlog"
	"workbuddy-gateway/internal/upstream"
)

// DefaultRepo 是默认更新源（owner/name）。
//
// 指向快照仓而不是源码仓：Release（三个平台的桌面版安装包）只发在这里。
const DefaultRepo = "zyp556678/wb-helper-main"

// DefaultAPIBase 是 GitHub REST 入口；测试用 httptest 覆盖。
const DefaultAPIBase = "https://api.github.com"

// DefaultCheckHours 是自动检查间隔（小时）。
//
// 6 小时是个折中：比「一天一次」更快让用户知道新版本，而一天 4 次请求对
// GitHub 的限流（未认证 60 次/小时）来说完全不算什么。
const DefaultCheckHours = 6

// MinCheckHours 是允许配置的最小间隔，挡住「配成 0 变成死循环」这类写法。
const MinCheckHours = 1

// DefaultStartupDelay 是启动后首次检查的等待时长。
//
// 刻意不在启动瞬间就查：那时网关正在加载凭据、拉模型目录、探活账号池，
// 插一个外网请求既拖慢启动，也给「启动慢」的排查多引一个变量。
const DefaultStartupDelay = 25 * time.Second

// minGap 是「面板刷新顺带触发的检查」的最小间隔。
//
// 用户按 F5 十次不该打十次 GitHub：未认证限流只有 60 次/小时，而用完之后的
// 表现是「检查更新突然一直失败」，很难联想到是刷新刷出来的。显式点「检查更新」
// 的请求不受这条约束（force=true），因为那是明确的用户意图。
const minGap = 5 * time.Minute

// maxBodyBytes 限制 Release 列表响应体的读取上限。
//
// 响应里有 release body（可能很长），但正常几十 KB 足够；设上限是为了在
// 上游返回异常大响应时不要把它整个读进内存。
const maxBodyBytes = 4 << 20

// maxNotesRunes 是下发给面板的更新说明长度上限（按 rune 截，避免切坏多字节字符）。
const maxNotesRunes = 4000

// maxAssetBytes 是单个安装包的下载上限（512 MiB）。
//
// 三个平台的安装包都在 100 MiB 以内，512 MiB 是「明显不对劲就停下」的护栏，
// 而不是预期值 —— 没有上限的话，一个被劫持的重定向就能把磁盘写满。
const maxAssetBytes = 512 << 20

// transientRetries / retryBackoff 与 upstream 的口径一致：
// 对幂等的 GET 做有界重试，代价低、收益明确。
const (
	transientRetries = 2
	retryBackoff     = 300 * time.Millisecond
)

// State 是一次检查的结果快照，也是面板那张卡片的全部数据来源。
//
// 字段名直接用 snake_case：面板的其它端点（autostart / stats / config）都是这个口径。
type State struct {
	// Current 是当前运行的网关版本（main.go 的 version 常量注入）。
	Current string `json:"current"`
	// Latest 是更新源上最新的版本号（已去掉 tag 的 v 前缀）。
	Latest string `json:"latest,omitempty"`
	// UpdateAvailable 表示 Latest 比 Current 新。
	UpdateAvailable bool `json:"update_available"`
	// Repo 是本次检查用的仓库，便于用户核对「我到底在查哪个仓」。
	Repo string `json:"repo,omitempty"`
	// ReleaseName / ReleaseURL / PublishedAt 描述那个 Release。
	ReleaseName string `json:"release_name,omitempty"`
	ReleaseURL  string `json:"release_url,omitempty"`
	PublishedAt int64  `json:"published_at,omitempty"`
	// Notes 是 Release 正文（截断后），面板直接当纯文本显示。
	Notes string `json:"notes,omitempty"`
	// AssetName / AssetSize / DownloadURL 是**匹配当前平台**的那个安装包；
	// 平台没有对应产物时留空，面板据此把「下载」按钮换成「打开 Release 页」。
	AssetName   string `json:"asset_name,omitempty"`
	AssetSize   int64  `json:"asset_size,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	// assetAPIURL 是不下发给面板的内部字段：私有仓库下载附件要走 API 端点
	//（见 releaseAsset.APIURL）。面板没有它的用途，也就不必扩大暴露面。
	assetAPIURL string
	// CheckedAt 是上次**成功**完成检查的时间（unix 秒；0 表示还没查过）。
	CheckedAt int64 `json:"checked_at,omitempty"`
	// Checking 表示此刻有一次检查在飞（面板据此显示转圈并轮询）。
	Checking bool `json:"checking"`
	// AutoCheck / IntervalHours / TokenSet 来自当前配置，面板据此渲染开关与提示，
	// 不必再去读 /panel/api/config（那条路径还要处理 token 的脱敏）。
	AutoCheck     bool `json:"auto_check"`
	IntervalHours int  `json:"interval_hours"`
	TokenSet      bool `json:"token_set"`
	// Error 是本次检查**失败**的原因（网络、限流、缺令牌……）。
	// 与 Detail 分开：Error 非空时面板显示红色提示条，Detail 是中性说明。
	Error string `json:"error,omitempty"`
	// Detail 是中性补充说明（例如「当前版本不是发行版本号，无法比较」）。
	Detail string `json:"detail,omitempty"`
}

// Options 是构造 Checker 的静态参数。
//
// 除 Current 外全部可注入：测试要在不碰真实网络、不依赖真实平台的前提下
// 覆盖「新版本判定」与「平台资产匹配」这两件最容易出错的事。
type Options struct {
	// Current 是当前版本（main.go 的 version）。
	Current string
	// Client 是出站 HTTP 客户端。传 cfg.Control：它已经带了 `-proxy` 配置，
	// 换一个客户端就等于把代理绕过去了 —— 在国内网络下这是「检查更新永远失败」的常见原因。
	Client *http.Client
	// APIBase 默认 DefaultAPIBase。
	APIBase string
	// DownloadDir 是安装包落盘目录（数据目录下的 updates/）。
	DownloadDir string
	// GOOS / GOARCH 用于挑平台安装包，默认取运行时。
	GOOS   string
	GOARCH string
	Logf   func(format string, args ...any)
	// Events 是面板「日志」页的事件源（可选，nil 安全）。
	Events *eventlog.Log
	// Now 注入时钟（测试用）。
	Now func() time.Time
	// StartupDelay 覆盖 DefaultStartupDelay（测试用）。
	StartupDelay time.Duration
}

// Checker 是更新检查器。零值不可用，请用 New 构造。
type Checker struct {
	current      string
	client       *http.Client
	apiBase      string
	goos         string
	goarch       string
	downloadDir  string
	logf         func(format string, args ...any)
	events       *eventlog.Log
	now          func() time.Time
	startupDelay time.Duration

	// cfgPtr 与 server 同款：配置在面板保存时是「克隆 → 改 → 换指针」，
	// 直接持有旧指针会让面板改的间隔/开关永远不生效。
	cfgPtr atomic.Pointer[config.Config]

	mu        sync.Mutex
	state     State
	lastCheck time.Time
	// dlMu 串行化下载：同一个安装包被两个请求同时下（面板连点两下、或客户端超时后
	// 重试而服务端那次还在跑）会同时写同一个临时文件，先结束的那个把临时文件删掉，
	// 后一个的 rename 就会失败 —— 实测踩到过「rename ... .part ... no such file」。
	// 串行化之后第二个请求看到文件已存在，直接返回 skipped。
	dlMu sync.Mutex
	// inflight 非 nil 表示此刻有一次真实的检查在跑；并发的第二次请求等它，
	// 而不是再打一次上游（见 Check 的说明）。
	inflight chan struct{}
}

// New 构造 Checker。
func New(cfg *config.Config, opt Options) *Checker {
	c := &Checker{
		current:      strings.TrimSpace(opt.Current),
		client:       opt.Client,
		apiBase:      strings.TrimRight(strings.TrimSpace(opt.APIBase), "/"),
		goos:         opt.GOOS,
		goarch:       opt.GOARCH,
		downloadDir:  opt.DownloadDir,
		logf:         opt.Logf,
		events:       opt.Events,
		now:          opt.Now,
		startupDelay: opt.StartupDelay,
	}
	if c.apiBase == "" {
		c.apiBase = DefaultAPIBase
	}
	if c.goos == "" {
		c.goos = runtime.GOOS
	}
	if c.goarch == "" {
		c.goarch = runtime.GOARCH
	}
	if c.client == nil {
		c.client = &http.Client{Timeout: 30 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	c.SetConfig(cfg)
	c.state = State{Current: c.current}
	return c
}

// SetConfig 换入最新的配置快照（面板保存配置后由 server 调用）。
func (c *Checker) SetConfig(cfg *config.Config) {
	if cfg == nil {
		return
	}
	c.cfgPtr.Store(cfg)
}

// Current 返回当前版本。
func (c *Checker) Current() string { return c.current }

// DownloadDir 返回安装包落盘目录。
func (c *Checker) DownloadDir() string { return c.downloadDir }

// settings 从当前配置快照里取本次检查所需的参数（每次现读，改完即生效）。
type settings struct {
	repo       string
	token      string
	prerelease bool
	enabled    bool
	hours      int
}

func (c *Checker) settings() settings {
	s := settings{repo: DefaultRepo, hours: DefaultCheckHours, enabled: true}
	cfg := c.cfgPtr.Load()
	if cfg == nil {
		return s
	}
	s.enabled = cfg.UpdateEnabled()
	s.prerelease = cfg.UpdatePrerelease()
	s.token = cfg.UpdateToken()
	if r := strings.TrimSpace(cfg.Update.Repo); r != "" {
		s.repo = r
	}
	if cfg.Update.CheckHours > 0 {
		s.hours = cfg.Update.CheckHours
	}
	if s.hours < MinCheckHours {
		s.hours = MinCheckHours
	}
	return s
}

func (c *Checker) interval() time.Duration {
	return time.Duration(c.settings().hours) * time.Hour
}

// State 返回当前快照（不触发任何检查）。
//
// 每次调用都重新贴一遍配置派生的字段：面板上的「自动检查」开关是从这里读的，
// 贴旧值会让开关与实际配置不一致。
func (c *Checker) State() State {
	s := c.settings()
	c.mu.Lock()
	st := c.state
	st.Checking = c.inflight != nil
	c.mu.Unlock()
	st.AutoCheck = s.enabled
	st.IntervalHours = s.hours
	st.TokenSet = s.token != ""
	if strings.TrimSpace(st.Repo) == "" {
		st.Repo = s.repo
	}
	return st
}

// Start 启动后台检查循环：启动后先等一小会儿查一次，之后按配置的间隔周期查。
// ctx 结束即退出（与仓库里其它后台循环一致，不等它）。
func (c *Checker) Start(ctx context.Context) {
	go c.loop(ctx)
}

func (c *Checker) loop(ctx context.Context) {
	delay := c.startupDelay
	if delay <= 0 {
		delay = DefaultStartupDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	c.checkIfEnabled(ctx)

	for {
		// 间隔每轮现读：面板把 6 小时改成 1 小时之后，下一轮就按新的算，
		// 不必重启进程（这也是 update 段列在 hotFields 里的原因）。
		timer.Reset(c.interval())
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		c.checkIfEnabled(ctx)
	}
}

func (c *Checker) checkIfEnabled(ctx context.Context) {
	if !c.settings().enabled {
		return
	}
	// force=false：让 minGap 生效，避免「刚启动查过一次，用户又立刻点了一次」时重复打上游。
	c.Check(ctx, false)
}

// Check 执行一次检查并返回结果快照。
//
// force=false 时，距上次检查不足 minGap 会直接给缓存；force=true 用于用户显式点击。
// 并发调用不会重复打上游：后到者等前一次跑完（面板轮询 + 周期循环撞在一起是常态）。
func (c *Checker) Check(ctx context.Context, force bool) State {
	c.mu.Lock()
	if c.inflight != nil {
		wait := c.inflight
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
		}
		return c.State()
	}
	if !force && !c.lastCheck.IsZero() && c.now().Sub(c.lastCheck) < minGap {
		c.mu.Unlock()
		return c.State()
	}
	done := make(chan struct{})
	c.inflight = done
	prev := c.state
	c.mu.Unlock()

	st := c.runCheck(ctx, prev)

	c.mu.Lock()
	c.state = st
	// 节流用的是「上次**尝试**的时间」，不是「上次成功的时间」：检查一直失败时
	// 若只记成功时间，面板每刷新一次就会触发一次后台检查，把额度耗在重复请求上。
	c.lastCheck = c.now()
	c.inflight = nil
	c.mu.Unlock()
	close(done)

	// 只在「新出现一个可升级版本」时记事件：周期检查每 6 小时一次，
	// 每次都记一条会把事件日志刷成一片重复内容，真正有用的告警反而被淹没。
	if st.UpdateAvailable && (!prev.UpdateAvailable || prev.Latest != st.Latest) {
		c.events.Info(eventlog.ChannelSystem, "update_available",
			"发现新版本 "+st.Latest+"（当前 "+c.current+"）",
			map[string]any{"current": c.current, "latest": st.Latest, "repo": st.Repo})
	}
	return c.State()
}

func (c *Checker) runCheck(ctx context.Context, prev State) State {
	s := c.settings()
	// 从上一次的**成功**结果接着写：一次网络抖动不该让面板上已有的「有新版本 0.9.3」
	// 和已经匹配好的安装包名一起消失（那看起来像「检查结果被清空了」）。
	// 失败时只补一个 Error，成功时下面会整体覆盖。
	st := State{
		Current:         c.current,
		Repo:            s.repo,
		AutoCheck:       s.enabled,
		IntervalHours:   s.hours,
		TokenSet:        s.token != "",
		Latest:          prev.Latest,
		UpdateAvailable: prev.UpdateAvailable,
		ReleaseName:     prev.ReleaseName,
		ReleaseURL:      prev.ReleaseURL,
		PublishedAt:     prev.PublishedAt,
		Notes:           prev.Notes,
		AssetName:       prev.AssetName,
		AssetSize:       prev.AssetSize,
		DownloadURL:     prev.DownloadURL,
		CheckedAt:       prev.CheckedAt,
	}
	if !validRepo(s.repo) {
		st.Error = "更新源仓库名不合法：" + s.repo + "（应形如 owner/name）"
		return st
	}

	releases, err := c.fetchReleases(ctx, s)
	if err != nil {
		st.Error = err.Error()
		c.logf("[更新] 检查失败: %v", err)
		debuglog.Event(nil, "warn", "update_check_failed",
			map[string]any{"repo": s.repo, "error": err.Error()})
		return st
	}
	rel, ok := pickRelease(releases, s.prerelease)
	if !ok {
		st.Error = "更新源里没有可用的 Release（可能还没发过版，或只发了预发布版）"
		return st
	}

	cur, curOK := parseSemver(c.current)
	latest, latestOK := parseSemver(rel.TagName)
	if !latestOK {
		st.Error = "Release 的 tag 不是版本号，无法比较：" + rel.TagName
		return st
	}

	st.Detail = ""
	st.Latest = normalizeVersion(rel.TagName)
	st.ReleaseName = rel.Name
	st.ReleaseURL = rel.HTMLURL
	st.PublishedAt = rel.PublishedAt.Unix()
	st.Notes = truncateRunes(strings.TrimSpace(rel.Body), maxNotesRunes)
	st.CheckedAt = c.now().Unix()
	if a, ok := pickAsset(rel.Assets, c.goos, c.goarch); ok {
		st.AssetName = a.Name
		st.AssetSize = a.Size
		st.DownloadURL = a.BrowserDownloadURL
		st.assetAPIURL = a.APIURL
	}

	switch {
	case !curOK:
		// 版本号不是 semver（例如开发期直接跑源码时的 "dev"）。
		// 这时**不能**说「有更新」：比较结果不可信，说成有更新会误导用户去装一个
		// 可能比手上还旧的包。如实说明「只显示最新版本」。
		st.Detail = "当前版本号 " + c.current + " 不是发行版本号，无法比较，仅显示更新源上的最新版本"
	case compareSemver(latest, cur) > 0:
		st.UpdateAvailable = true
		if st.AssetName == "" {
			st.Detail = "更新源上没有匹配 " + c.platform() + " 的安装包，请到 Release 页按平台手动下载"
		}
	default:
		st.Detail = "已是最新版本"
	}
	return st
}

// platform 返回人类可读的平台标识（用于提示文案）。
func (c *Checker) platform() string {
	switch c.goos {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	}
	return c.goos
}

// ---------------------------------------------------------------------------
// GitHub Release
// ---------------------------------------------------------------------------

type release struct {
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	Body        string         `json:"body"`
	HTMLURL     string         `json:"html_url"`
	Draft       bool           `json:"draft"`
	Prerelease  bool           `json:"prerelease"`
	PublishedAt time.Time      `json:"published_at"`
	Assets      []releaseAsset `json:"assets"`
}

// releaseAsset 是 Release 的一个附件。单独起名是为了让挑安装包的那段逻辑
// 是一个只吃纯数据的函数，测试不必造 HTTP 响应。
type releaseAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// BrowserDownloadURL 是给人看的下载页链接（github.com/.../releases/download/...）。
	// 公开仓库直接下它就行；**私有仓库不行** —— 那条链接认的是浏览器会话 Cookie，
	// 带 Bearer 令牌一样 404（实测），必须走下面那个 API 端点。
	BrowserDownloadURL string `json:"browser_download_url"`
	// APIURL 是资产端点（api.github.com/repos/{owner}/{repo}/releases/assets/{id}）。
	// 配 `Accept: application/octet-stream` + 令牌，它会 302 到一个签名地址，
	// 这是 GitHub 文档里下载 Release 附件的正路，私有仓库也认。
	APIURL string `json:"url"`
	ID     int64  `json:"id"`
}

// fetchReleases 拉取 Release 列表。
//
// 用**列表**而不是 `/releases/latest`：后者会把预发布版算进来（也不给跳过选项），
// 而「要不要预发布版」是本项目的一个显式配置。另外列表能让我们在多个 Release
// 之间挑版本号最大的那个，而不是盲信「创建时间最新」——补发一个旧版本的分支时，
// 两者会不一致。
func (c *Checker) fetchReleases(ctx context.Context, s settings) ([]release, error) {
	endpoint := c.apiBase + "/repos/" + s.repo + "/releases?per_page=30"
	var lastErr error
	for attempt := 0; attempt <= transientRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(retryBackoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		body, err := c.get(ctx, endpoint, s.token)
		if err != nil {
			lastErr = err
			// 只重试瞬时网络故障；HTTP 状态码类的错误（404/401/403）重试没有意义。
			if upstream.IsTransientNetworkError(err) {
				continue
			}
			return nil, err
		}
		var rels []release
		if err := json.Unmarshal(body, &rels); err != nil {
			return nil, errors.New("解析 Release 列表失败：" + err.Error())
		}
		return rels, nil
	}
	return nil, lastErr
}

// get 发一次 GET 并返回响应体（带上限）。
func (c *Checker) get(ctx context.Context, endpoint, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.setGitHubHeaders(req, token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, githubStatusError(resp.StatusCode, c.repoForMessage(), token != "")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// setGitHubHeaders 设置 GitHub REST 要求的请求头。
//
// User-Agent 不是可选项：GitHub 对空 UA（含 Go 的默认 UA）直接返回 403，
// 而那条 403 的文案与限流一模一样，很容易被误判成「额度用完了」。
func (c *Checker) setGitHubHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "workbuddy-gateway/"+c.current)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func (c *Checker) repoForMessage() string { return c.settings().repo }

// githubStatusError 把 GitHub 的状态码翻译成用户能照着做的说明。
//
// 这里最要紧的一条是 404：**私有仓库对未认证请求就是 404**。若把它笼统地写成
// 「仓库不存在」，用户永远不会想到要配令牌。
func githubStatusError(code int, repo string, tokenSet bool) error {
	switch code {
	case http.StatusNotFound:
		if tokenSet {
			return fmt.Errorf("GitHub 返回 404：仓库 %s 不存在，或令牌没有读取该仓库的权限", repo)
		}
		return fmt.Errorf("GitHub 返回 404：仓库 %s 不存在或是私有的。"+
			"私有仓库需要只读令牌：在 config.json 的 update.token 里填一个，"+
			"或设环境变量 WB_UPDATE_TOKEN", repo)
	case http.StatusUnauthorized:
		return errors.New("GitHub 返回 401：update.token 无效或已过期")
	case http.StatusForbidden, http.StatusTooManyRequests:
		if tokenSet {
			return fmt.Errorf("GitHub 返回 %d：令牌权限不足或已触发限流", code)
		}
		return fmt.Errorf("GitHub 返回 %d：未认证请求每小时只有 60 次额度，"+
			"配一个只读令牌可以提到 5000 次", code)
	default:
		return fmt.Errorf("GitHub 返回 HTTP %d", code)
	}
}

// pickRelease 从列表里挑出「该拿来比较」的那个 Release。
//
// 规则：排除草稿；除非明确要预发布版，否则排除 prerelease 标记与 tag 带后缀
// （v0.9.0-slice8）的；剩下的取版本号最大的 —— 不是取列表第一个，
// 因为补发旧版本时列表顺序与版本号顺序不一致。
func pickRelease(rels []release, prerelease bool) (release, bool) {
	var best release
	var bestVer semver
	found := false
	for _, r := range rels {
		if r.Draft {
			continue
		}
		v, ok := parseSemver(r.TagName)
		if !ok {
			continue
		}
		if !prerelease && (r.Prerelease || len(v.pre) > 0) {
			continue
		}
		if !found || compareSemver(v, bestVer) > 0 {
			best, bestVer, found = r, v, true
		}
	}
	return best, found
}

// assetRules 给出某平台可接受的安装包特征：扩展名与架构标记，均按优先级排列。
func assetRules(goos, goarch string) (exts, archTokens []string) {
	switch goos {
	case "windows":
		// 先认安装器再认裸 exe：前者才会注册开始菜单与卸载项。
		exts = []string{"-setup.exe", ".exe"}
	case "darwin":
		exts = []string{".dmg", ".pkg"}
	default:
		exts = []string{".deb", ".rpm", ".AppImage", ".tar.gz", ".zip"}
	}
	switch goarch {
	case "amd64":
		archTokens = []string{"amd64", "x86_64", "x64"}
	case "arm64":
		archTokens = []string{"arm64", "aarch64"}
	default:
		archTokens = []string{goarch}
	}
	return exts, archTokens
}

// pickAsset 在 Release 的附件里挑出匹配当前平台的那个。
//
// 按「扩展名优先级 × 架构标记优先级」取第一个命中的：先 .deb 后 .rpm、
// 先 amd64 后 x86_64 —— 顺序即偏好，避免同一平台有多个包时结果随机。
// 找不到就返回 false（调用方据此提示「到 Release 页手动下载」，而不是给一个错的文件）。
func pickAsset(assets []releaseAsset, goos, goarch string) (releaseAsset, bool) {
	exts, archTokens := assetRules(goos, goarch)
	for _, ext := range exts {
		for _, tok := range archTokens {
			for _, a := range assets {
				name := strings.ToLower(a.Name)
				if !strings.HasSuffix(name, strings.ToLower(ext)) {
					continue
				}
				if !strings.Contains(name, strings.ToLower(tok)) {
					continue
				}
				return a, true
			}
		}
	}
	return releaseAsset{}, false
}

// ---------------------------------------------------------------------------
// 下载安装包
// ---------------------------------------------------------------------------

// DownloadResult 是一次下载的结果。
type DownloadResult struct {
	// Path 是安装包在本地的绝对路径。
	Path string `json:"path"`
	// Name 是文件名（面板显示用）。
	Name string `json:"name"`
	// Bytes 是文件大小。
	Bytes int64 `json:"bytes"`
	// Version 是这个安装包对应的版本号。
	Version string `json:"version"`
	// Skipped 表示本地已有同样大小的文件，这次没有重复下载。
	Skipped bool `json:"skipped"`
	// Dir 是所在目录（面板用它做「打开所在文件夹」）。
	Dir string `json:"dir"`
}

// Download 把上次检查匹配到的安装包下到本地（幂等：本地已有同大小文件就跳过）。
//
// 只下载 State 里记着的那个地址 —— 端点不接受客户端传 URL，因此这里不存在
// 「面板让网关去下载任意地址」的 SSRF 面。
func (c *Checker) Download(ctx context.Context) (DownloadResult, error) {
	c.dlMu.Lock()
	defer c.dlMu.Unlock()

	st := c.State()
	if strings.TrimSpace(st.DownloadURL) == "" {
		if st.Error != "" {
			return DownloadResult{}, errors.New(st.Error)
		}
		return DownloadResult{}, errors.New("当前没有匹配本机平台的安装包，请先检查更新或到 Release 页手动下载")
	}
	if strings.TrimSpace(c.downloadDir) == "" {
		return DownloadResult{}, errors.New("未配置安装包下载目录")
	}
	name := assetFileName(st.DownloadURL)
	if name == "" {
		return DownloadResult{}, errors.New("安装包地址里没有可用的文件名：" + st.DownloadURL)
	}
	// 有令牌就走 API 资产端点：私有仓库的 /releases/download/... 链接认的是浏览器
	// 会话，带 Bearer 令牌一样 404（这是实测踩到的）。公开仓库则用浏览器链接，
	// 少一次 API 调用。
	target := st.DownloadURL
	if s := c.settings(); s.token != "" && strings.TrimSpace(st.assetAPIURL) != "" {
		target = st.assetAPIURL
	}
	if err := os.MkdirAll(c.downloadDir, 0o755); err != nil {
		return DownloadResult{}, err
	}
	dest := filepath.Join(c.downloadDir, name)

	// 幂等：上一次已经下过（或用户手动放了一份）就不重复占带宽。
	if info, err := os.Stat(dest); err == nil && !info.IsDir() && st.AssetSize > 0 && info.Size() == st.AssetSize {
		return DownloadResult{Path: dest, Name: name, Bytes: info.Size(), Version: st.Latest,
			Dir: c.downloadDir, Skipped: true}, nil
	}

	// 下载要能重试。安装包几十 MB，慢网络下断在 90% 是常事 —— 而断了就从头再来
	// 是用户唯一能做的动作，不如替他做。只重试**瞬时**故障（连接被重置、响应体
	// 提前结束），HTTP 404/403 这类确定性失败重试没有意义。
	var written int64
	var lastErr error
	for attempt := 0; attempt <= transientRetries; attempt++ {
		if attempt > 0 {
			c.logf("[更新] 下载中断，重试第 %d 次: %v", attempt, lastErr)
			select {
			case <-time.After(retryBackoff):
			case <-ctx.Done():
				return DownloadResult{}, ctx.Err()
			}
		}
		written, lastErr = c.downloadOnce(ctx, target, dest)
		if lastErr == nil {
			break
		}
		if !upstream.IsTransientNetworkError(lastErr) {
			return DownloadResult{}, fmt.Errorf("下载安装包失败：%w", lastErr)
		}
	}
	if lastErr != nil {
		return DownloadResult{}, fmt.Errorf("下载安装包失败（已重试 %d 次）：%w", transientRetries, lastErr)
	}

	c.logf("[更新] 安装包已下载: %s（%.1f MiB）", dest, float64(written)/(1<<20))
	return DownloadResult{Path: dest, Name: name, Bytes: written, Version: st.Latest, Dir: c.downloadDir}, nil
}

// downloadOnce 下载一次并落盘，返回写入的字节数。
//
// 先写临时文件再改名：中途失败（网络断、磁盘满、进程被杀）不会留下一个
// 「大小不对但看起来像安装包」的文件 —— 那种文件最容易被用户双击安装。
func (c *Checker) downloadOnce(ctx context.Context, rawURL, dest string) (int64, error) {
	// 清掉上次异常退出留下的临时文件：它们不会被复用，却会一直占着几十 MB 磁盘，
	// 而且文件名看起来像安装包。下载已由 dlMu 串行化，不会误删正在写的那个。
	if stale, err := filepath.Glob(dest + ".part*"); err == nil {
		for _, p := range stale {
			_ = os.Remove(p)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "workbuddy-gateway/"+c.current)
	// 私有仓库的附件下载同样要鉴权；跨主机重定向时会被 downloadClient 摘掉。
	if s := c.settings(); s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := c.downloadClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return 0, errors.New("HTTP 401：令牌无效或已过期")
		case http.StatusForbidden, http.StatusNotFound:
			if s := c.settings(); s.token != "" {
				return 0, fmt.Errorf("HTTP %d：令牌没有读取该仓库 / 附件的权限（需要 contents:read）", resp.StatusCode)
			}
			return 0, fmt.Errorf("HTTP %d：私有仓库的附件需要令牌（config.json 的 update.token）", resp.StatusCode)
		default:
			return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}

	// 临时文件名带唯一后缀：并发请求 / 多进程不会互相覆盖同一个临时文件
	//（实测踩到过：一个请求清理临时文件，把另一个请求正在写的那个删掉，
	// 后者的 rename 就报 no such file）。
	tmp := fmt.Sprintf("%s.part-%d", dest, time.Now().UnixNano())
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxAssetBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return 0, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return 0, closeErr
	}
	if written > maxAssetBytes {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("安装包超过 %d MiB，已中止下载", maxAssetBytes>>20)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return written, nil
}

// downloadClient 复制一份客户端并加上重定向白名单。
//
// 两个作用：
//  1. 安装包地址来自上游响应，正常会被重定向到 objects.githubusercontent.com；
//     限定主机名可以挡住「响应被篡改 → 把安装包下成任意地址的内容」；
//  2. 跨主机时不带 Authorization：令牌只该发给 github.com。
//
// 顺带把总超时去掉（只保留 ctx 的上限）：安装包几十 MB，60 秒的总超时
// 在慢网络下会稳定失败，而这里的正确约束是「调用方给多久」。
func (c *Checker) downloadClient() *http.Client {
	cp := *c.client
	cp.Timeout = 0
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("安装包地址重定向次数过多")
		}
		if !allowedAssetHost(req.URL.Hostname()) {
			return fmt.Errorf("安装包地址被重定向到非预期主机 %s，已中止", req.URL.Hostname())
		}
		req.Header.Del("Authorization")
		return nil
	}
	return &cp
}

// allowedAssetHost 判断下载重定向的目标主机是否可信。
//
// 回环地址是给测试用的（httptest 监听 127.0.0.1）；GitHub 侧除了 github.com
// 本身，附件实际由 *.githubusercontent.com 提供。
func allowedAssetHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	if h == "github.com" || strings.HasSuffix(h, ".github.com") {
		return true
	}
	return strings.HasSuffix(h, ".githubusercontent.com")
}

// assetFileName 从下载地址里取文件名（并挡掉路径穿越）。
func assetFileName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	// 以 / 结尾的地址是目录而不是文件（path.Base 会返回最后一段目录名，
	// 那样存下来的文件会叫 "v0.9.3" 这种名字，用户根本认不出是什么）。
	if u.Path == "" || strings.HasSuffix(u.Path, "/") {
		return ""
	}
	name := path.Base(u.Path)
	if name == "" || name == "." || name == "/" || name == ".." || strings.ContainsAny(name, `/\`) {
		return ""
	}
	return name
}

// ---------------------------------------------------------------------------
// 版本号比较
// ---------------------------------------------------------------------------

// semver 是语义化版本的一个子集：数字段 + 可选的预发布标识。
//
// 不引第三方库：go.mod 只有 sqlite 一个直接依赖，而这个项目对新增依赖一向谨慎
// （scripts/release.sh 里连 GOPROXY 都固定成了国内镜像）。这里需要的规则总共几十行，
// 而且**必须**容忍本项目自己的 tag 形态（v0.9.0-slice8）。
type semver struct {
	nums []int
	pre  []string
}

// parseSemver 解析版本号（允许 v 前缀、允许缺段、忽略 +build 元数据）。
func parseSemver(v string) (semver, bool) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if s == "" {
		return semver{}, false
	}
	// 构建元数据不参与比较（语义化版本规范第 10 条）。
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre []string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = strings.Split(s[i+1:], ".")
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return semver{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		nums = append(nums, n)
	}
	if len(nums) == 0 {
		return semver{}, false
	}
	return semver{nums: nums, pre: pre}, true
}

// compareSemver 比较两个已解析的版本号：a<b 返回 -1，相等返回 0，a>b 返回 1。
//
// 规则（语义化版本规范第 11 条）：先比数字段（缺段补 0）；数字段相同时
// **有预发布标识的更低**（1.0.0-rc1 < 1.0.0）；都有预发布标识时逐段比，
// 纯数字段按数值、含字母的按字典序，且数字段低于字母段；前缀相同则短的更小。
func compareSemver(a, b semver) int {
	n := len(a.nums)
	if len(b.nums) > n {
		n = len(b.nums)
	}
	for i := 0; i < n; i++ {
		x, y := 0, 0
		if i < len(a.nums) {
			x = a.nums[i]
		}
		if i < len(b.nums) {
			y = b.nums[i]
		}
		if x != y {
			return sign(x - y)
		}
	}
	if len(a.pre) == 0 && len(b.pre) == 0 {
		return 0
	}
	if len(a.pre) == 0 {
		return 1
	}
	if len(b.pre) == 0 {
		return -1
	}
	m := len(a.pre)
	if len(b.pre) > m {
		m = len(b.pre)
	}
	for i := 0; i < m; i++ {
		if i >= len(a.pre) {
			return -1
		}
		if i >= len(b.pre) {
			return 1
		}
		x, y := a.pre[i], b.pre[i]
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				return sign(xn - yn)
			}
		case xerr == nil:
			// 数字标识符的优先级低于字母标识符。
			return -1
		case yerr == nil:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return sign(c)
			}
		}
	}
	return 0
}

// CompareVersions 比较两个版本号字符串；任一无法解析时返回 (0, false)。
//
// 导出是为了让「面板上显示的判定」可以被单独测到。
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseSemver(a)
	bv, bok := parseSemver(b)
	if !aok || !bok {
		return 0, false
	}
	return compareSemver(av, bv), true
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// normalizeVersion 去掉展示用的 v 前缀（tag 是 v0.9.2，版本号本身是 0.9.2）。
func normalizeVersion(v string) string {
	s := strings.TrimSpace(v)
	return strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
}

// validRepo 校验 owner/name（只做形状校验，不去猜测 GitHub 的命名规则）。
func validRepo(repo string) bool {
	r := strings.TrimSpace(repo)
	if r == "" || strings.ContainsAny(r, " \t") {
		return false
	}
	parts := strings.Split(r, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// truncateRunes 按字符截断（不是按字节），避免把中文说明切成乱码。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n…（说明过长已截断，完整内容见 Release 页）"
}
