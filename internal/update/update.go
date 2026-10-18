// Package update 检查有没有新版本，并把匹配当前平台的安装包下到本地。
//
// # 与面板的分工
//
// 面板上只有两件事可做：**看版本状态**（当前 / 最新 / 有新版本）与**点更新按钮**。
// 检测是后台自动的（每 DefaultCheckHours 一次，代价只有一次只读 GET）；点一下按钮
// 之后：还没下载就先把安装包下到数据目录，**下载完成接着自动安装**并把网关带到
// 新版本。没有「自动下载」「自动安装」这类开关 —— 该不该装是用户点不点按钮的问题，
// 而不是一个需要预先想清楚的配置。
//
// # 为什么检测与下载只能由网关做
//
// 面板的响应头里 CSP 是 `connect-src 'self'`（见 internal/server/server.go 的
// panelSecurity），浏览器里根本发不出跨域请求；桌面壳的职责又被刻意收窄成
// 「进程代管 + 窗口 + 托盘」，不含业务逻辑。于是「问一句有没有新版本」只有网关能做
// —— 这也正好：出站代理（`-proxy`）只有它认，私有仓库的令牌也只有它该拿。
//
// # 为什么不自己替换文件（自更新）
//
// 安装包是 deb / dmg / setup.exe 三种平台专用格式，没有一种是「换掉文件就完事」：
//
//   - Windows：正在运行的 exe 被系统锁住，必须由一个独立的安装程序结束进程再覆盖
//     （见 install.go 的 installWindows）；
//   - Linux：直接覆盖 /usr 下的文件会让 dpkg 的包数据库与磁盘内容不一致，正确做法
//     是交给 dpkg 装新包；
//   - macOS：App 是 bundle，要整包替换并处理 Gatekeeper 隔离属性；
//   - npm 形态：正确做法是 `npm i -g workbuddy-gateway@latest`，覆盖文件反而绕过了
//     npm 的完整性校验（见 npm/scripts/install.js 顶部对「不联网下载二进制」的说明）。
//
// 更关键的是失败代价：自更新失败会留下一个「既起不来、也没有安装包可回退」的现场，
// 而用户手里的 CLI / IDE 全都指着这个端口。所以每一步都交给平台自己的安装器去做，
// 装完的结局（重启 / 退出）由 install.go 说明 —— 这比做一个半可靠的自更新进程诚实得多。
//
// # 更新源是 GitHub Release
//
// 走 api.github.com 的公开接口（仓库已公开，无需令牌）。列表接口有 60 次/小时/IP
// 的未认证限额，所以成功后按 CheckHours 缓存，不重复问。安装包下载**直连优先**，
// 直连不通或慢到不可用才回退到国内镜像（见 downloadRoutes）。
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

// directRouteTimeout 是「直连 github.com」这一次尝试的总时限。
//
// 「不通」在国内不只表现为连不上：更常见的是连上了但只有几十 KB/s。等满安装包
// 那 10 分钟再报错对用户没有任何价值，所以在 5 分钟（9 MB ≈ 30 KB/s）就切到
// 加速镜像 —— 那正是这个回退要覆盖的情形。
const directRouteTimeout = 5 * time.Minute

// directHeaderTimeout 是直连时「等响应头」的时限。
//
// TCP 层被丢 SYN（最常见的封锁形态）时，连接会一直挂着；这个值把「完全连不上」
// 从分钟级压到 20 秒，好让镜像尽快接管。响应头到手之后就不再限速 —— 慢而能完成的
// 下载不该被掐断。
const directHeaderTimeout = 20 * time.Second

// DefaultDownloadMirror 是直连失败后自动回退的国内加速前缀。
//
// 它不再是「默认走它」：现在的顺序是**直连优先**，只有直连这条路失败或慢到不可用
// 才切过来（见 Download）。语义变了，所以这个常量也搬到了本包 —— 它是下载策略的
// 一部分，不再是用户配置项。
const DefaultDownloadMirror = "https://gh-proxy.org"

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
	// CheckedAt 是上次**成功**完成检查的时间（unix 秒；0 表示还没查过）。
	CheckedAt int64 `json:"checked_at,omitempty"`
	// Checking 表示此刻有一次检查在飞（面板据此显示转圈并轮询）。
	Checking bool `json:"checking"`
	// Downloading / Downloaded / DownloadTotal 是**下载进度**：面板据此画进度条。
	// 只在 Downloading 为真时有意义（下载结束就清零）。
	Downloading   bool  `json:"downloading"`
	Downloaded    int64 `json:"downloaded_bytes,omitempty"`
	DownloadTotal int64 `json:"download_total_bytes,omitempty"`
	// Installing 表示此刻有一次安装在跑（面板据此显示「正在安装」并把按钮置灰，
	// 避免连点）。
	Installing bool `json:"installing,omitempty"`
	// LocalPath / LocalBytes 是**本地已就绪**的安装包（不存在或大小不符时为空）。
	// 面板据此把「下载安装包」换成「安装并重启」，刷新页面也不必重新下载。
	LocalPath  string `json:"local_path,omitempty"`
	LocalBytes int64  `json:"local_bytes,omitempty"`
	// Install 是「在这台机器上装这个包」的方案：面板在确认框里如实展示要执行的命令、
	// 是否提权、装完谁负责重启。平台差异只有后端知道，前端不自己拼这些文案。
	Install InstallPlan `json:"install"`
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
	// ExePath 是本进程的可执行文件路径（main 传入 os.Executable 的结果）。
	// 用来判断「这份网关是桌面版还是命令行版」，从而只挑对应形态的安装包。
	ExePath string
	// Host 是代管方声明的形态（`WB_GATEWAY_HOST=desktop|cli`，由启动器设置）。
	// 优先于 ExePath 推断：路径规则会随安装布局变化，而启动器是自己写的。
	Host string
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
	// form 是本实例的安装形态（formDesktop / formCLI / formUnknown），
	// 决定从 Release 里挑哪一类安装包。见 detectForm。
	form string

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
	// dlActive/dlDone/dlTotal 是下载进度（面板的进度条）。用原子量而不是加锁：
	// io.Copy 的每个 chunk 都要更新一次，加锁会把写盘的快路径拖慢。
	dlActive atomic.Bool
	dlDone   atomic.Int64
	dlTotal  atomic.Int64
	// installing 表示有一次安装在跑（面板据此置灰按钮、显示「正在安装」）。
	// 由 Install 自己置位：手动点的那一次也要在状态里看得见，而且不许连点。
	installing atomic.Bool
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
		form:         detectForm(opt.Host, opt.ExePath),
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
	// 下载进度（面板的进度条）：原子读，不用加锁 —— 每写一个 chunk 都会更新。
	st.Downloading = c.dlActive.Load()
	st.Downloaded, st.DownloadTotal = 0, 0
	if st.Downloading {
		st.Downloaded = c.dlDone.Load()
		st.DownloadTotal = c.dlTotal.Load()
	}
	st.Installing = c.installing.Load()
	if strings.TrimSpace(st.Repo) == "" {
		st.Repo = s.repo
	}
	// 本地已就绪的安装包 + 本机安装方案：面板据此决定显示「下载」还是「安装并重启」。
	// 每次 State() 都 stat 一次（几微秒），换来「刷新页面不用重新下载」的体验。
	st.LocalPath, st.LocalBytes = "", 0
	if st.UpdateAvailable {
		st.LocalPath, st.LocalBytes = c.localAsset(st.AssetName, st.AssetSize)
	}
	st.Install = planFor(c.goos, st.LocalPath)
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
	if a, ok := pickAsset(rel.Assets, c.goos, c.goarch, c.form); ok {
		st.AssetName = a.Name
		st.AssetSize = a.Size
		st.DownloadURL = a.BrowserDownloadURL
	}

	switch {
	case !curOK:
		// 版本号不是 semver（例如开发期直接跑源码时的 "dev"）。
		// 这时**不能**说「有更新」：比较结果不可信，说成有更新会误导用户去装一个
		// 可能比手上还旧的包。如实说明「只显示最新版本」。
		st.Detail = "当前版本号 " + c.current + " 不是发行版本号，无法比较，仅显示更新源上的最新版本"
	case compareSemver(latest, cur) > 0:
		st.UpdateAvailable = true
		switch {
		case c.form == formNPM:
			// npm 安装没有可用的系统安装包，而且装上去会变成两份程序。
			// 这里明说该怎么升，而不是给一个「下载」按钮。
			st.Detail = "本实例由 npm 全局安装，请用 npm i -g workbuddy-gateway@latest 升级"
		case st.AssetName == "":
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
	// BrowserDownloadURL 是下载页链接（github.com/.../releases/download/...）。
	BrowserDownloadURL string `json:"browser_download_url"`
	ID                 int64  `json:"id"`
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
		body, err := c.get(ctx, endpoint)
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
func (c *Checker) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.setGitHubHeaders(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, githubStatusError(resp.StatusCode, c.repoForMessage())
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
func (c *Checker) setGitHubHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "workbuddy-gateway/"+c.current)
}

func (c *Checker) repoForMessage() string { return c.settings().repo }

// githubStatusError 把 GitHub 的状态码翻译成用户能照着做的说明。
//
// 未认证请求每小时只有 60 次额度，所以 403/429 基本就是撞了限流 —— 如实说明，
// 而不是笼统写「请求失败」。
func githubStatusError(code int, repo string) error {
	switch code {
	case http.StatusNotFound:
		return fmt.Errorf("GitHub 返回 404：仓库 %s 不存在（检查更新源仓库名是否写错）", repo)
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return fmt.Errorf("GitHub 返回 %d：未认证请求每小时只有 60 次额度，已触发限流，稍后再试", code)
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

// 安装形态。Release 上同时存在两种 Windows 产物：
//
//	workbuddy-gateway-desktop_<ver>_x64-setup.exe   带窗口 + 托盘的桌面版
//	workbuddy-gateway_<ver>_x64-setup.exe           无窗口的命令行版
//
// 两者装到不同目录、注册不同卸载项。如果不管形态都挑「第一个匹配的 setup.exe」，
// 命令行版用户点一下更新就会被装成桌面版，机器上出现两份安装、快捷方式互相覆盖。
const (
	formDesktop = "desktop"
	formCLI     = "cli"
	// formNPM 是 npm 全局安装。它没有对应的安装包：升级必须走 npm，
	// 用 Release 上的 setup.exe / deb 覆盖只会多出一份安装。
	formNPM     = "npm"
	formUnknown = "unknown"
)

// hostEnv 是启动器声明的形态。桌面壳、命令行启动器与 npm 入口各自设置，
// 比从路径推断可靠（见 main.go 里传给 Options.Host 的来源）。
const hostEnv = "WB_GATEWAY_HOST"

// HostEnv 是 WB_GATEWAY_HOST 的名字，供 main 读取。
const HostEnv = hostEnv

// detectForm 判断本实例属于哪种安装形态。
//
// 顺序：启动器声明 > 可执行文件路径 > 未知。
// 路径规则只用于兜底（例如用户直接双击了压缩包里的二进制，没有启动器），
// 因此只认两种安装器写死的目录名，不做模糊猜测 —— 猜错会让用户装错形态。
//
// 归一化用显式的 `\` → `/` 替换，而不是 filepath.ToSlash：后者在 Unix 上对
// 反斜杠是空操作，于是同一份规则在 Linux/macOS 上判不出 Windows 安装目录
// （测试也随宿主系统漂移 —— CI 六个平台的作业都跑在 Linux 上，一眼就能看出）。
// 规则本身与宿主无关，归一化也必须与宿主无关。
func detectForm(host, exePath string) string {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case formDesktop, formCLI, formNPM:
		return strings.ToLower(strings.TrimSpace(host))
	}
	p := strings.ToLower(strings.ReplaceAll(exePath, `\`, "/"))
	switch {
	// 桌面版：Tauri 每用户安装的 bin/ 资源目录。
	case strings.Contains(p, "/workbuddy gateway/bin/"):
		return formDesktop
	// npm 全局安装：包内 bin/ 目录，路径形如
	//   <prefix>/node_modules/workbuddy-gateway/bin/workbuddy-gateway
	// 必须排在命令行版之前判断：npm 的 prefix 可以是任意目录，但 node_modules
	// 这一层是它独有的。
	case strings.Contains(p, "/node_modules/workbuddy-gateway/"):
		return formNPM
	// 命令行版：install.ps1 / deploy 脚本的落点，以及 DEPLOY.md 建议的 ~/.wb-gateway/bin。
	// 注意 Linux/macOS 的数据目录带点（.wb-gateway），Windows 的不带 —— 两种都要认。
	case strings.Contains(p, "/programs/workbuddy-gateway/"),
		strings.Contains(p, "/wb-gateway/bin/"),
		strings.Contains(p, "/.wb-gateway/bin/"):
		return formCLI
	}

	return formUnknown
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
//
// form 限制候选形态（见 detectForm）。它只做**排除**，不做加分：
//   - formDesktop：只要桌面版（文件名带 -desktop）。
//   - formCLI：只要命令行版（文件名不带 -desktop）。
//   - formUnknown：不限制 —— 连自己是什么形态都不知道时，任何排除都可能把
//     唯一可用的包挡掉，那比装错形态更糟。
//
// 同一形态内仍然先按扩展名、再按架构标记挑，因此补发资产不会改变结果。
func pickAsset(assets []releaseAsset, goos, goarch, form string) (releaseAsset, bool) {
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
				if !formAllows(form, name) {
					continue
				}
				return a, true
			}
		}
	}
	return releaseAsset{}, false
}

// formAllows 判断某个资产文件名是否属于当前安装形态。
func formAllows(form, lowerName string) bool {
	switch form {
	case formDesktop:
		return strings.Contains(lowerName, "-desktop")
	case formCLI:
		return !strings.Contains(lowerName, "-desktop")
	case formNPM:
		// npm 安装没有对应的安装包：系统安装包一律不匹配，调用方据此提示
		// 「用 npm 升级」。装上去只会多出一份程序，而 npm 那份仍然在。
		return false
	}
	return true
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

// downloadRoute 是一条下载路线：直连 github.com，或直连失败后的国内加速镜像。
type downloadRoute struct {
	// name 给人看：日志与最终报错里都要说清走的是哪条路。
	name string
	// url 是这条路线实际请求的地址。
	url string
	// mirrorHost 是重定向白名单要额外放行的镜像主机（空 = 直连，没有额外主机）。
	mirrorHost string
	// direct 为真表示「直连」这条路，受总时限与响应头时限约束（见 directRouteTimeout）。
	direct bool
}

// downloadRoutes 给出本次下载的路线顺序：**直连优先，不通（或慢到不可用）才切镜像**。
//
// 镜像只对 GitHub 家族的地址有意义（它就是个 GitHub 反向代理）；本地自建源、
// 测试里的 httptest 地址都不触发回退，否则每个失败的用例都会去打真实网络。
func (c *Checker) downloadRoutes(rawURL string) []downloadRoute {
	routes := []downloadRoute{{name: "直连 github.com", url: rawURL, direct: true}}
	if !isGitHubAssetURL(rawURL) {
		return routes
	}
	mirrored, err := ApplyDownloadMirror(DefaultDownloadMirror, rawURL)
	if err != nil {
		return routes
	}
	host, _ := mirrorHostOf(DefaultDownloadMirror)
	return append(routes, downloadRoute{
		name:       "加速镜像 " + DefaultDownloadMirror,
		url:        mirrored,
		mirrorHost: host,
	})
}

// isGitHubAssetURL 判断下载地址是不是 GitHub 家族的。
func isGitHubAssetURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return isGitHubHost(u.Hostname())
}

// isGitHubHost 判断主机名是否属于 GitHub（github.com 与它托管附件的那几个域）。
func isGitHubHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == "github.com" || strings.HasSuffix(h, ".github.com") ||
		strings.HasSuffix(h, ".githubusercontent.com")
}

// Download 把上次检查匹配到的安装包下到本地（幂等：本地已有同大小文件就跳过）。
//
// 只下载 State 里记着的那个地址 —— 端点不接受客户端传 URL，因此这里不存在
// 「面板让网关去下载任意地址」的 SSRF 面。
//
// 下载顺序是**直连优先，不通再走国内加速镜像**（见 downloadRoutes）：国内直连
// github.com 下几十 MB 经常只有几十 KB/s 甚至完全连不上，而这恰好是升级路上
// 最贵的一步；反过来，无脑走第三方镜像也没必要 —— 直连能通时就不该绕道。
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
	if err := os.MkdirAll(c.downloadDir, 0o755); err != nil {
		return DownloadResult{}, err
	}
	dest := filepath.Join(c.downloadDir, name)

	// 幂等：上一次已经下过（或用户手动放了一份）就不重复占带宽。
	if info, err := os.Stat(dest); err == nil && !info.IsDir() && st.AssetSize > 0 && info.Size() == st.AssetSize {
		return DownloadResult{Path: dest, Name: name, Bytes: info.Size(), Version: st.Latest,
			Dir: c.downloadDir, Skipped: true}, nil
	}

	// 进度从这里开始记：上面那条幂等分支没下东西，不该让面板闪一下进度条。
	c.beginProgress(st.AssetSize)
	defer c.endProgress()

	routes := c.downloadRoutes(st.DownloadURL)
	var written int64
	var lastErr error
	for i, rt := range routes {
		if i > 0 {
			// 直连失败的**原始**原因要留住：它就是「为什么走了镜像」的答案。
			c.logf("[更新] %s 不通或太慢（%v），改走 %s", routes[0].name, lastErr, rt.name)
			c.events.Info(eventlog.ChannelSystem, "update_download_fallback",
				"直连下载失败，改用加速镜像 "+DefaultDownloadMirror,
				map[string]any{"error": errText(lastErr)})
		}
		routeCtx, cancel := c.routeContext(ctx, rt)
		written, lastErr = c.downloadRoute(routeCtx, rt, dest)
		cancel()
		if lastErr == nil {
			break
		}
		// 上层给的 ctx 没了（面板关了、请求取消）就不必再试下一条 —— 那是调用方
		// 不想下了，不是「这条路不通」。
		if ctx.Err() != nil {
			return DownloadResult{}, ctx.Err()
		}
	}
	if lastErr != nil {
		return DownloadResult{}, fmt.Errorf("下载安装包失败（%s）：%w", routeNames(routes), lastErr)
	}

	c.logf("[更新] 安装包已下载: %s（%.1f MiB）", dest, float64(written)/(1<<20))
	return DownloadResult{Path: dest, Name: name, Bytes: written, Version: st.Latest, Dir: c.downloadDir}, nil
}

// routeContext 给「直连」那条路加总时限；镜像不限时（它本来就是最后一条路，
// 再给它设上限只会把「慢但能完成」变成失败）。
func (c *Checker) routeContext(ctx context.Context, rt downloadRoute) (context.Context, context.CancelFunc) {
	if !rt.direct {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, directRouteTimeout)
}

// downloadRoute 在一条路线上完成整次下载（含瞬时故障重试）。
func (c *Checker) downloadRoute(ctx context.Context, rt downloadRoute, dest string) (int64, error) {
	var written int64
	var lastErr error
	for attempt := 0; attempt <= transientRetries; attempt++ {
		if attempt > 0 {
			c.logf("[更新] 下载中断，重试第 %d 次: %v", attempt, lastErr)
			select {
			case <-time.After(retryBackoff):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		written, lastErr = c.downloadOnce(ctx, rt, dest)
		if lastErr == nil {
			return written, nil
		}
		// 只重试**瞬时**故障（连接被重置、响应体提前结束），HTTP 404/403 这类
		// 确定性失败重试没有意义 —— 交给下一路线或如实报错。
		if !upstream.IsTransientNetworkError(lastErr) {
			return 0, lastErr
		}
	}
	return 0, fmt.Errorf("已重试 %d 次：%w", transientRetries, lastErr)
}

// routeNames 把走过的路线拼成一句人话（失败文案里要说清都试过什么）。
func routeNames(routes []downloadRoute) string {
	names := make([]string, 0, len(routes))
	for _, rt := range routes {
		names = append(names, rt.name)
	}
	return "已试过 " + strings.Join(names, "、")
}

// errText 取错误的文本（nil 安全，事件日志不该因为一个 nil 崩）。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// beginProgress / endProgress 标记一次真实下载的起止（面板的进度条）。
// total=0 表示服务器没给 Content-Length（罕见）——面板会退化为「转圈」而不是画条。
func (c *Checker) beginProgress(total int64) {
	c.dlDone.Store(0)
	if total > 0 {
		c.dlTotal.Store(total)
	} else {
		c.dlTotal.Store(0)
	}
	c.dlActive.Store(true)
}

func (c *Checker) endProgress() {
	c.dlActive.Store(false)
	c.dlDone.Store(0)
	c.dlTotal.Store(0)
}

// countingWriter 把写入的字节数累计到进度计数器里（每个 chunk 一次原子加）。
type countingWriter struct {
	w   io.Writer
	got *atomic.Int64
}

func (cw countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	if n > 0 {
		cw.got.Add(int64(n))
	}
	return n, err
}

// ApplyDownloadMirror 把 Release 附件地址套上加速前缀，返回可请求的 URL。
//
// 前缀的形态是「服务 origin」：`https://gh-proxy.org/`（结尾斜杠可有可无），
// 拼接结果是 `https://gh-proxy.org/https://github.com/...`—— 这是这类服务的约定。
// 严格校验而不是随手拼：这个值**决定请求发去哪**（默认是内置的 DefaultDownloadMirror，
// 也可能来自调用方），拼错会产生一个难懂的请求失败，不如当场报清楚。
func ApplyDownloadMirror(mirror, rawURL string) (string, error) {
	m := strings.TrimSpace(mirror)
	if m == "" {
		return rawURL, nil
	}
	// 只接 origin（或 origin+`/`）：不许带路径与 query，避免「半截 URL」被拼出意外形态。
	if !strings.HasPrefix(m, "https://") && !strings.HasPrefix(m, "http://") {
		return "", fmt.Errorf("加速前缀必须以 https:// 开头（当前值 %q）", m)
	}
	// 解析后只允许 host（允许显式端口，如 :8080）；结尾斜杠视为「就这一个 origin」，
	// 先剥掉再判 —— 拼接时无论如何都会补一个。
	u, err := url.Parse(m)
	if err != nil || u.Host == "" || strings.TrimSuffix(u.Path, "/") != "" || u.RawQuery != "" {
		return "", fmt.Errorf("加速前缀只能是主机地址（如 https://gh-proxy.org），不能带路径（当前值 %q）", m)
	}
	// 原始地址必须是 http(s)，且知道要加速的是谁（拼接形态要求它是完整 URL）。
	raw, err := url.Parse(rawURL)
	if err != nil || raw.Scheme != "https" && raw.Scheme != "http" {
		return "", fmt.Errorf("安装包地址不是有效的 http(s) URL：%q", rawURL)
	}
	return strings.TrimSuffix(m, "/") + "/" + rawURL, nil
}

// mirrorHostOf 取加速前缀的主机名（小写）。前缀非法时 ok=false。
//
// 单独一个函数而不是在 Download 里内联解析：下载的重定向白名单要按主机名放行，
// 而这里必须与 ApplyDownloadMirror 认的是同一个东西，否则会出现「拼接通过了、
// 白名单却把它当成非预期主机挡掉」的自相矛盾。
func mirrorHostOf(mirror string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(mirror))
	if err != nil || u.Host == "" {
		return "", false
	}
	return strings.ToLower(u.Hostname()), true
}

// downloadOnce 在某条路线上下载一次并落盘，返回写入的字节数。
//
// 先写临时文件再改名：中途失败（网络断、磁盘满、进程被杀）不会留下一个
// 「大小不对但看起来像安装包」的文件 —— 那种文件最容易被用户双击安装。
func (c *Checker) downloadOnce(ctx context.Context, rt downloadRoute, dest string) (int64, error) {
	// 清掉上次异常退出留下的临时文件：它们不会被复用，却会一直占着几十 MB 磁盘，
	// 而且文件名看起来像安装包。下载已由 dlMu 串行化，不会误删正在写的那个。
	if stale, err := filepath.Glob(dest + ".part*"); err == nil {
		for _, p := range stale {
			_ = os.Remove(p)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rt.url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "workbuddy-gateway/"+c.current)

	resp, err := c.downloadClient(rt).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusForbidden, http.StatusNotFound:
			return 0, fmt.Errorf("HTTP %d：下载地址不可用（Release 可能已被删除，或镜像上没有这个文件）", resp.StatusCode)
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
	// 边写边记进度：面板的进度条读 dlDone/dlTotal。重试时从当前重试的那一轮重新计数
	//（断点续传没做，作者判断不值得 —— 重下一次几秒钟，而 Range 请求会让「临时文件
	// 可能是半截」这种状态多出一层）。
	c.dlDone.Store(0)
	written, copyErr := io.Copy(countingWriter{w: f, got: &c.dlDone}, io.LimitReader(resp.Body, maxAssetBytes+1))
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
// 作用是防篡改：安装包地址来自上游响应，正常会被重定向到 objects.githubusercontent.com；
// 限定主机名可以挡住「响应被篡改 → 把安装包下成任意地址的内容」。
// 白名单必须包含**镜像的主机**，否则走镜像时请求会被自己挡下。
//
// 顺带把总超时去掉（只保留 ctx 的上限）：安装包几十 MB，60 秒的总超时
// 在慢网络下会稳定失败，而这里的正确约束是「调用方给多久」。
// 唯一例外是直连那条路的**等响应头**时限（见 directHeaderTimeout）。
func (c *Checker) downloadClient(rt downloadRoute) *http.Client {
	cp := *c.client
	cp.Timeout = 0
	if rt.direct {
		cp.Transport = transportWithHeaderTimeout(cp.Transport)
	}
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("安装包地址重定向次数过多")
		}
		if !allowedAssetHost(req.URL.Hostname(), rt.mirrorHost) {
			return fmt.Errorf("安装包地址被重定向到非预期主机 %s，已中止", req.URL.Hostname())
		}
		return nil
	}
	return &cp
}

// transportWithHeaderTimeout 复制一份 Transport 并设上响应头时限。
//
// 为什么要复制而不是直接改：这个客户端是从配置里拿的（cfg.Control，可能带着
// `-proxy` 的代理设置），直接改会污染其它所有出站请求。
// Transport 为 nil 时（用默认 Transport）克隆 http.DefaultTransport —— 否则我们
// 一设属性就把系统代理之类的默认行为丢掉了。
func transportWithHeaderTimeout(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	tr, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	clone := tr.Clone()
	clone.ResponseHeaderTimeout = directHeaderTimeout
	return clone
}

// allowedAssetHost 判断下载（含重定向）的目标主机是否可信。
//
// 回环地址是给测试用的（httptest 监听 127.0.0.1）；GitHub 侧除了 github.com
// 本身，附件实际由 *.githubusercontent.com 提供。mirrorHost 是当次路线用的镜像
// 主机（空串表示直连）—— 走镜像时请求本来就要发到那儿，必须放行。
func allowedAssetHost(host, mirrorHost string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	if m := strings.ToLower(strings.TrimSpace(mirrorHost)); m != "" && h == m {
		return true
	}
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return isGitHubHost(h)
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
