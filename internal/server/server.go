// Package server 提供网关的 HTTP 层：OpenAI 兼容端点 + 面板 API + 面板静态资源。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy-gateway/internal/accountmeta"
	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/catalog"
	"workbuddy-gateway/internal/clientlimits"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/creditwatch"
	"workbuddy-gateway/internal/debuglog"
	"workbuddy-gateway/internal/eventlog"
	"workbuddy-gateway/internal/localagent"
	"workbuddy-gateway/internal/metrics"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/reqlog"
	"workbuddy-gateway/internal/scheduler"
	"workbuddy-gateway/internal/session"
	"workbuddy-gateway/internal/stats"
	"workbuddy-gateway/internal/tasks"
	"workbuddy-gateway/internal/upstream"
)

// Version 由 main 注入（保留在 server 便于 /healthz 与面板展示）。
var Version = "dev"

// Server 聚合网关运行期依赖。
type Server struct {
	// cfgPtr 用原子指针而不是裸 *config.Config：
	// 面板保存配置是**写**、chat 热路径是**读**，原地改写会数据竞争。
	// 保存时改为「克隆 → 在克隆上改 → 整体换指针」，读侧永远看到完整版本。
	// 一律通过 s.config() 取，不要直接 Load 后长期持有（那会读到旧版本）。
	cfgPtr  atomic.Pointer[config.Config]
	pool    *pool.Pool
	client  *upstream.Client
	cat     *catalog.Catalog
	logins  *loginStore
	events  *eventlog.Log
	tasks   *tasks.Manager
	stats   *stats.Store
	local   *localagent.Supervisor
	metrics *metrics.Registry
	sticky  *session.Store
	sched   *scheduler.Scheduler
	// reqLog 是请求级观测记录器（内存指标 + 可选脱敏归档）。
	// nil 表示未启用 —— 所有调用点都要能容忍 nil。
	reqLog *reqlog.Recorder
	// checkinCache 缓存各账号的「今日是否已签到」（TTL 5 分钟，见 checkin_status.go）。
	checkinCache *checkinStatusCache
	// creditWatch 是积分本地观察台账（internal/creditwatch）。
	// 可选增强：nil 时静默跳过，官方接口挂掉时页面只是没有本地回退口径。
	creditWatch *creditwatch.Store
	// clientLimits 扫描客户端日志里的模型限流（internal/clientlimits）。
	// WorkBuddy 客户端直连官方，它的 429 不经过网关，只能从客户端日志还原。
	// 可选增强：nil 时只显示网关自己观测到的冷却。
	clientLimits *clientlimits.Scanner
	// accountMeta 是账号备注与显示字段（internal/accountmeta）。
	// 可选增强：nil 时按「全员无备注、昵称显示」处理。
	accountMeta *accountmeta.Store
	// groupLocks 串行化同一会话组的写操作（组同步 / 复制并关联）。
	// 组同步会连续写多份正文，前端在批量期间重复点击必须被挡住；不同组互不影响。
	groupLocks keyedLocks
	start      time.Time

	// panelFS 是面板静态资源（前端构建产物），由 main 注入。
	panelFS fs.FS
}

// config 取当前生效的配置快照。
//
// 每次调用都 Load：不要把它缓存到局部变量再跨越「可能发生保存」的代码段使用，
// 否则拿到的是旧版本。一次 Load 是原子读，开销可忽略。
func (s *Server) config() *config.Config {
	return s.cfgPtr.Load()
}

// New 构造 Server。
func New(cfg *config.Config, p *pool.Pool, client *upstream.Client, cat *catalog.Catalog,
	reg *metrics.Registry, sticky *session.Store, sched *scheduler.Scheduler,
	events *eventlog.Log, taskMgr *tasks.Manager, statStore *stats.Store,
	localSup *localagent.Supervisor, panelFS fs.FS) *Server {
	s := &Server{
		pool:    p,
		client:  client,
		cat:     cat,
		logins:  newLoginStore(),
		events:  events,
		tasks:   taskMgr,
		stats:   statStore,
		local:   localSup,
		metrics: reg,
		sticky:  sticky,
		sched:   sched,
		start:   time.Now(),
		// 签到状态缓存：按账号 ID 存「今日是否已签到」，并落盘到工作目录
		//（否则每次网关重启后第一次开账号页，都要对每个国内站账号各打一次上游）。
		checkinCache: newCheckinStatusCache(),
		panelFS:      panelFS,
	}
	s.cfgPtr.Store(cfg)

	// 缓存落盘 + 启动时恢复（放在 Store 之后：saveTo 可能用到 cfg）。
	s.checkinCache.workDir = cfg.WorkDir
	s.checkinCache.logf = s.logf
	s.checkinCache.loadFrom(cfg.WorkDir)

	return s
}

func (s *Server) logf(format string, args ...any) { log.Printf(format, args...) }

// SetRequestLog 注入请求级记录器。
//
// 用 setter 而不是加构造参数：New 已经有 12 个参数，再加一个会让每个调用点都要改，
// 而记录器是**可选增强**（nil 时全部功能照常，只是没有请求明细）。
func (s *Server) SetRequestLog(rec *reqlog.Recorder) { s.reqLog = rec }

// SetCreditWatch 注入积分本地观察台账（可选增强，nil 安全）。
func (s *Server) SetCreditWatch(store *creditwatch.Store) { s.creditWatch = store }

// SetClientLimits 注入客户端日志限流扫描器（可选增强，nil 安全）。
func (s *Server) SetClientLimits(scanner *clientlimits.Scanner) { s.clientLimits = scanner }

// SetAccountMeta 注入账号备注 / 显示字段存储（可选增强，nil 安全）。
func (s *Server) SetAccountMeta(store *accountmeta.Store) { s.accountMeta = store }

// StartBackground 启动后台任务。
func (s *Server) StartBackground(ctx context.Context) {
	go s.reloadLoop(ctx)
}

// Handler 组装完整路由与中间件。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ---- OpenAI 兼容端点（切片 0 只上 chat/models；responses 与探测在后续切片）----
	// 对话类端点挂请求级观测（记明细 + X-Request-Id）；面板 API 不挂（量大且无 token 语义）。
	mux.HandleFunc("/v1/chat/completions", s.withRequestLog(s.handleChatCompletions))
	mux.HandleFunc("/v1/responses", s.withRequestLog(s.handleResponses))
	// Anthropic Messages API：Claude Code 这类客户端只会说 Anthropic 协议，
	// 没有这两条路由它们完全连不上。
	mux.HandleFunc("/v1/messages", s.withRequestLog(s.handleMessages))
	mux.HandleFunc("/messages", s.withRequestLog(s.handleMessages))
	mux.HandleFunc("/v1/messages/count_tokens", s.withRequestLog(s.handleCountTokens))
	mux.HandleFunc("/messages/count_tokens", s.withRequestLog(s.handleCountTokens))
	// 别名：部分客户端只认不带 /v1 的路径（对照 wb-gateway 的双注册）。
	mux.HandleFunc("/responses", s.withRequestLog(s.handleResponses))
	mux.HandleFunc("/chat/completions", s.withRequestLog(s.handleChatCompletions))
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/models", s.handleModels)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/ping", s.handleHealth)
	mux.HandleFunc("/status", s.handleStatus)
	// 模型属性探测（指定账号 × 模型）：只接受回环来源，见 probe.go 的说明。
	mux.HandleFunc("/admin/probe", s.handleAdminProbe)

	// ---- 面板：静态资源 + API ----
	mux.HandleFunc("/panel/api/overview", s.withPanelAuth(s.handlePanelOverview))
	mux.HandleFunc("/panel/api/accounts", s.withPanelAuth(s.handlePanelAccounts))

	// ---- 面板：登录（浏览器内 OAuth）----
	mux.HandleFunc("/panel/api/login/sites", s.withPanelAuth(s.handleLoginSites))
	mux.HandleFunc("/panel/api/login/start", s.withPanelAuth(s.handleLoginStart))
	mux.HandleFunc("/panel/api/login/poll", s.withPanelAuth(s.handleLoginPoll))
	// 桌面壳的 WebView 会拦掉 window.open / target="_blank"（且不报错），
	// 授权链接点不开等于国际站加不了账号 —— 由网关用系统默认浏览器打开。
	mux.HandleFunc("/panel/api/open-url", s.withPanelAuth(s.handlePanelOpenURL))

	// ---- 面板：单账号与批量操作 ----
	mux.HandleFunc("/panel/api/accounts/{id}/quota", s.withPanelAuth(s.handleAccountQuota))
	mux.HandleFunc("/panel/api/accounts/{id}/plan", s.withPanelAuth(s.handleAccountPlan))
	mux.HandleFunc("/panel/api/accounts/{id}/disable", s.withPanelAuth(s.handleAccountDisable))
	mux.HandleFunc("/panel/api/accounts/{id}/enable", s.withPanelAuth(s.handleAccountEnable))
	mux.HandleFunc("/panel/api/accounts/{id}/revive", s.withPanelAuth(s.handleAccountRevive))
	mux.HandleFunc("/panel/api/accounts/{id}/remove", s.withPanelAuth(s.handleAccountRemove))
	mux.HandleFunc("/panel/api/accounts/{id}/refresh-token", s.withPanelAuth(s.handleAccountRefreshToken))
	mux.HandleFunc("/panel/api/accounts/{id}/checkin", s.withPanelAuth(s.handleAccountCheckin))
	mux.HandleFunc("/panel/api/accounts/{id}/intl-activate", s.withPanelAuth(s.handleAccountIntlActivate))
	mux.HandleFunc("/panel/api/login/regions", s.withPanelAuth(s.handleLoginRegions))
	// 导入 / 导出（切片 12）。注意 import/preview 与 import 都是**一级路径**，
	// 不要写成 /accounts/{id}/... —— 会被 {id} 路由抢走。
	mux.HandleFunc("/panel/api/accounts/export", s.withPanelAuth(s.handleAccountsExport))
	mux.HandleFunc("/panel/api/accounts/import/preview", s.withPanelAuth(s.handleAccountsImportPreview))
	mux.HandleFunc("/panel/api/accounts/import", s.withPanelAuth(s.handleAccountsImport))
	mux.HandleFunc("/panel/api/accounts/checkin-status", s.withPanelAuth(s.handleAccountsCheckinStatus))
	// 账号备注 / 显示字段（批次 4）：GET 全员、PATCH 单账号。
	// 注意「meta 一览」是一级路径（/accounts/meta），与 /accounts/{id}/meta 是两条路由。
	mux.HandleFunc("/panel/api/accounts/meta", s.withPanelAuth(s.handleAccountsMeta))
	mux.HandleFunc("/panel/api/accounts/{id}/meta", s.withPanelAuth(s.handleAccountMetaUpdate))
	// 批量「刷新积分并签到」（批次 4）：遵守签到时间段与排除名单，返回结构化计数。
	mux.HandleFunc("/panel/api/accounts/checkin_all", s.withPanelAuth(s.handleAccountsCheckinAll))

	// ---- 面板：本机应用接入（切片 14）----
	// 把账号池里的账号写入本机某个应用的登录态（CLI / WorkBuddy 客户端）。
	// 网关本身是本机进程，读写本机文件是它的本来能力；面板只需调这些接口。
	mux.HandleFunc("/panel/api/local-apps", s.withPanelAuth(s.handleLocalApps))
	mux.HandleFunc("/panel/api/local-apps/switch", s.withPanelAuth(s.handleLocalAppsSwitch))
	mux.HandleFunc("/panel/api/local-apps/backups", s.withPanelAuth(s.handleLocalAppsBackups))
	mux.HandleFunc("/panel/api/local-apps/restore", s.withPanelAuth(s.handleLocalAppsRestore))

	// ---- 面板：本机会话库（切片 15）----
	// 看 WorkBuddy 客户端的会话、把某条会话复制给另一个账号。**只新增，不改不删。**
	// 「取消/删除关联」只解除分组关系（记排除项），同样不碰会话内容。
	mux.HandleFunc("/panel/api/local-sessions", s.withPanelAuth(s.handleLocalSessions))
	mux.HandleFunc("/panel/api/local-sessions/copy", s.withPanelAuth(s.handleLocalSessionsCopy))
	mux.HandleFunc("/panel/api/local-sessions/sync", s.withPanelAuth(s.handleLocalSessionsSync))
	mux.HandleFunc("/panel/api/local-sessions/groups", s.withPanelAuth(s.handleLocalSessionGroups))
	mux.HandleFunc("/panel/api/local-sessions/recover", s.withPanelAuth(s.handleLocalSessionsRecover))
	// 扩展数据仓会话（VS Code 插件 / CodeBuddy IDE）。
	mux.HandleFunc("/panel/api/local-sessions/ext", s.withPanelAuth(s.handleExtSessions))
	mux.HandleFunc("/panel/api/local-sessions/ext/copy", s.withPanelAuth(s.handleExtSessionsCopy))
	mux.HandleFunc("/panel/api/local-sessions/ext/groups/{id}/preview-pair", s.withPanelAuth(s.handleExtSessionGroupPreviewPair))
	mux.HandleFunc("/panel/api/local-sessions/ext/groups/{id}/unify", s.withPanelAuth(s.handleExtSessionGroupUnify))
	// 组详情 / 组同步 / 关联新账号 / 解除关联（切片 19 批次 3）。
	// /groups/{id}/sync 与 /add 比 /groups/{id} 更具体，ServeMux 会优先匹配。
	mux.HandleFunc("/panel/api/local-sessions/groups/{id}", s.withPanelAuth(s.handleLocalSessionGroup))
	mux.HandleFunc("/panel/api/local-sessions/groups/{id}/sync", s.withPanelAuth(s.handleLocalSessionGroupSync))
	mux.HandleFunc("/panel/api/local-sessions/groups/{id}/add", s.withPanelAuth(s.handleLocalSessionGroupAdd))
	// 按模式统一 / 批量快进 / 逐对预览（登记表模型，对照 wb-switch 的三个入口）。
	mux.HandleFunc("/panel/api/local-sessions/groups/{id}/unify", s.withPanelAuth(s.handleLocalSessionGroupSync))
	mux.HandleFunc("/panel/api/local-sessions/groups/{id}/safe-batch", s.withPanelAuth(s.handleLocalSessionGroupSafeBatch))
	mux.HandleFunc("/panel/api/local-sessions/groups/{id}/preview-pair", s.withPanelAuth(s.handleLocalSessionGroupPreviewPair))
	mux.HandleFunc("/panel/api/local-sessions/groups/{id}/unlink", s.withPanelAuth(s.handleLocalSessionGroupUnlink))
	mux.HandleFunc("/panel/api/local-sessions/groups/{id}/delete", s.withPanelAuth(s.handleLocalSessionGroupDelete))
	mux.HandleFunc("/panel/api/quota_all", s.withPanelAuth(s.handleQuotaAll))

	// ---- 面板：监控与治理（切片 2）----
	mux.HandleFunc("/panel/api/metrics", s.withPanelAuth(s.handlePanelMetrics))
	mux.HandleFunc("/panel/api/config", s.withPanelAuth(s.handlePanelConfigRoute))
	mux.HandleFunc("/panel/api/scheduler/run", s.withPanelAuth(s.handleSchedulerRun))
	mux.HandleFunc("/panel/api/session/sticky", s.withPanelAuth(s.handleStickyInfo))

	// ---- 面板：模型与倍率（切片 3）----
	mux.HandleFunc("/panel/api/models", s.withPanelAuth(s.handlePanelModels))
	mux.HandleFunc("/panel/api/models/refresh", s.withPanelAuth(s.handleModelsRefresh))
	mux.HandleFunc("/panel/api/models/probe", s.withPanelAuth(s.handleModelsProbe))

	// ---- 面板：双站视图（切片 8）----
	mux.HandleFunc("/panel/api/sites", s.withPanelAuth(s.handlePanelSites))
	// 站点路由的用法说明（切片 11）。与 /sites 分开：那个是「两站现状」，
	// 这个是「怎么指定走哪一站」，变更频率和用途都不同。
	mux.HandleFunc("/panel/api/site-route", s.withPanelAuth(s.handlePanelSiteRoute))

	// ---- 面板：任务中心（切片 5）----
	mux.HandleFunc("/panel/api/tasks", s.withPanelAuth(s.handlePanelTasks))
	mux.HandleFunc("/panel/api/tasks/scan", s.withPanelAuth(s.handleTasksScan))
	mux.HandleFunc("/panel/api/tasks/run", s.withPanelAuth(s.handleTasksRun))
	mux.HandleFunc("/panel/api/tasks/queue", s.withPanelAuth(s.handleTasksQueue))
	mux.HandleFunc("/panel/api/tasks/overview", s.withPanelAuth(s.handleTasksOverview))
	mux.HandleFunc("/panel/api/tasks/report", s.withPanelAuth(s.handleTasksReport))
	mux.HandleFunc("/panel/api/tasks/streak-bonus", s.withPanelAuth(s.handleTasksStreakBonus))
	mux.HandleFunc("/panel/api/tasks/vouchers", s.withPanelAuth(s.handleTasksVouchers))
	// 账号级任务动作（面板「一键完成」）：单任务与全量，per-account 互斥。
	mux.HandleFunc("/panel/api/accounts/{id}/tasks/auto", s.withPanelAuth(s.handleAccountTaskAuto))
	mux.HandleFunc("/panel/api/accounts/{id}/tasks/auto_all", s.withPanelAuth(s.handleAccountTaskAutoAll))
	mux.HandleFunc("/panel/api/growth/action", s.withPanelAuth(s.handleGrowthAction))

	// ---- 面板：本机代理（切片 7）----
	// /panel/local/* 是反代到本机代理的通道，同样要过面板鉴权——
	// 本机代理持有读本机数据的能力，不能因为「它在回环上」就免鉴权。
	if s.local != nil {
		mux.Handle(ProxyPathLocal+"/", s.withPanelAuthHandler(s.local.NewProxy()))
		mux.Handle(ProxyPathLocal, s.withPanelAuthHandler(s.local.NewProxy()))
	}
	mux.HandleFunc("/panel/api/local/capabilities", s.withPanelAuth(s.handleLocalCapabilities))
	mux.HandleFunc("/panel/api/local/restart", s.withPanelAuth(s.handleLocalRestart))

	// ---- 面板：开机自启动（切片 9）----
	// 自启项是系统级事实（注册表 / plist / .desktop），网关直接读写，
	// 因此桌面版与命令行版的面板看到的是同一份真实状态。
	mux.HandleFunc("/panel/api/autostart", s.withPanelAuth(s.handleAutostartRoute))

	// ---- 面板：用量统计（切片 6）----
	mux.HandleFunc("/panel/api/stats", s.withPanelAuth(s.handlePanelStats))
	mux.HandleFunc("/panel/api/stats/daily", s.withPanelAuth(s.handlePanelStatsDaily))
	mux.HandleFunc("/panel/api/stats/purge", s.withPanelAuth(s.handleStatsPurge))
	mux.HandleFunc("/panel/api/stats/official", s.withPanelAuth(s.handleOfficialUsage))
	mux.HandleFunc("/panel/api/credits", s.withPanelAuth(s.handlePanelCredits))
	mux.HandleFunc("/panel/api/credits/stats", s.withPanelAuth(s.handleCreditStatistics))
	mux.HandleFunc("/panel/api/token-stats", s.withPanelAuth(s.handleTokenStats))

	// ---- 面板：事件日志（切片 4）----
	mux.HandleFunc("/panel/api/logs", s.withPanelAuth(s.handlePanelLogs))
	mux.HandleFunc("/panel/api/logs/clear", s.withPanelAuth(s.handleLogsClear))
	// 设置页的只读查询（批次 5）：签到日志（本地台账）、日志落点、最近请求日志下载。
	mux.HandleFunc("/panel/api/checkin/logs", s.withPanelAuth(s.handleCheckinLogs))
	mux.HandleFunc("/panel/api/logs/paths", s.withPanelAuth(s.handleLogPaths))
	mux.HandleFunc("/panel/api/logs/download", s.withPanelAuth(s.handleRequestLogDownload))
	// 请求级观测：进程内指标 + 脱敏归档
	mux.HandleFunc("/panel/api/request-metrics", s.withPanelAuth(s.handleRequestMetrics))
	mux.HandleFunc("/panel/api/request-logs", s.withPanelAuth(s.handleRequestLogs))
	mux.HandleFunc("/panel/", s.handlePanelStatic)
	mux.HandleFunc("/panel", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/panel/", http.StatusFound)
	})

	mux.HandleFunc("/", s.handleIndex)

	// 最外层是管理面守卫：它要在被拒绝的请求上也把安全头写好，
	// 因此必须排在 cors/auth 之前。
	return s.panelSecurity(s.cors(s.auth(mux)))
}

// -----------------------------------------------------------------------------
// 中间件
// -----------------------------------------------------------------------------

// panelCSP 是面板页面的内容安全策略。
//
// 为什么 style-src 留 'unsafe-inline'：React 与若干组件库会直接写 style 属性，
// 禁掉会把面板样式打散；而 script-src 不留 —— 面板产物里**没有任何内联脚本**
// （Vite 只注入同源的 <script src>），所以这条能真正挡住注入执行。
//
// 其余几项：frame-ancestors 'none' 防点击劫持（面板会把接入密钥显示给用户）、
// object-src 'none' 与 base-uri 'none' 关掉两条老注入路径、
// form-action 'self' 防止被诱导把表单提交到别处。
const panelCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; " +
	"object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// isModelAPIPath 判断请求是否落在「给外部客户端调用」的模型接口上。
func isModelAPIPath(path string) bool {
	return path == "/v1" || strings.HasPrefix(path, "/v1/")
}

// isAdminPath 判断请求是否落在管理面（面板页面本身与面板接口）。
//
// 注意 `/` 也算：它服务的就是面板单页应用。而 /status、/healthz 这些是网关自身
// 的状态接口，不在这里 —— 它们的数据同样敏感，但走的是 Bearer 鉴权那条路。
func isAdminPath(path string) bool {
	return path == "/" || path == "/panel" || strings.HasPrefix(path, "/panel/")
}

// sameOrigin 判断 Origin 头是否与本请求同源。
//
// 逐项卡死而不是只比 Host：`http://127.0.0.1:8317@evil.com` 这类带 userinfo 的
// 写法、以及带 path/query 的畸形 Origin，都不该被当成同源。
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.User != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// panelSecurity 给管理面加同源校验与安全响应头。
//
// # 为什么需要它
//
// 面板接口与模型接口**共用同一个端口**，而鉴权用的是 Authorization 头。
// 传统 CSRF（靠浏览器自动带 Cookie）因此本来就不成立，但还有一条更隐蔽的路：
//
//	**DNS rebinding** —— 攻击者把自己的域名解析到 127.0.0.1，浏览器就会把他的
//	页面当成与我们同源，此时同源策略不再提供任何保护。
//
// Origin 与 Sec-Fetch-Site 是浏览器自己填的、页面脚本无法伪造的信号，据此可以
// 把跨站来源挡在门外。这也是上游（workbuddy-gateway）在 6565407 里补的那一层。
//
// 安全头则针对面板页面的特点：它会把接入密钥显示给用户，所以不能被缓存、
// 不能被别的站点嵌进 iframe、也不该被 MIME 嗅探。
func (s *Server) panelSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", panelCSP)

		reject := func(reason string) {
			s.logf("[管理来源校验] 结果=拒绝 原因=%s 路径=%s 状态码=403", reason, r.URL.Path)
			h.Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": http.StatusForbidden, "message": reason},
			})
		}

		// Origin 缺失是允许的：同源的导航请求与命令行工具本来就不带它
		// （curl、脚本、桌面壳的首次加载）。带上了就必须与本机同源。
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			reject("管理面只接受同源请求")
			return
		}
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
			reject("拒绝跨站管理请求")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// cors 只对 /v1/* 放开跨域。
//
// 这里曾经对**所有**路由设 `Access-Control-Allow-Origin: *`。问题在于面板接口与
// 模型接口共用一个端口：通配 CORS 意味着任何网站都能向 http://127.0.0.1:8317
// 发带 Authorization 头的跨域请求并**读到响应** —— 唯一的门槛就只剩密钥本身。
//
// 浏览器里的客户端确实需要跨域调 /v1/*，但管理面不需要：面板自己与桌面壳都是
// 同源访问（壳把密钥走 URL fragment 注入，见 web/src/lib/injected-key.ts）。
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isModelAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auth 对 /v1/* 与 /status 做 Bearer 校验；健康检查、首页与面板静态资源始终放行。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.config().APIKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/", "/health", "/healthz", "/ping":
			next.ServeHTTP(w, r)
			return
		}
		if isPanelAsset(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if bearer(r) != s.config().APIKey {
			debuglog.Event(r, "warn", "authentication_rejected", map[string]any{
				"status_code": http.StatusUnauthorized,
				"reason":      "invalid_api_key",
			})
			// 鉴权在 handler 之前：按**路径**判断协议形状（Anthropic 客户端解析不了
			// OpenAI 形状的错误体，会把它当成协议异常）。
			s.writeAPIError(w, r, http.StatusUnauthorized, "invalid_api_key", "未提供有效 API 密钥")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isPanelAsset 判断是否为面板的**静态资源**（HTML / JS / CSS / 图标）。
//
// 这些必须放行，否则设了 --api-key 之后面板会**完全打不开**：
// 浏览器导航（地址栏回车、点链接）带不了 `Authorization` 头，于是
// `/panel/` 直接返回一段 401 JSON，前端脚本根本加载不了 ——
// 连「让用户输入密钥」的那个界面都渲染不出来，形成死锁。
// （`ApiKeyPanel` 的存在正说明预期流程是：先加载壳 → 接口 401 → 用户填密钥。）
//
// 放行的**只有静态资源**：`/panel/api/*`（数据接口）与 `/panel/local/*`
// （本机代理反代，持有读本机数据的能力）继续走鉴权。
func isPanelAsset(p string) bool {
	if p != "/panel" && !strings.HasPrefix(p, "/panel/") {
		return false
	}
	if strings.HasPrefix(p, "/panel/api/") {
		return false
	}
	return !strings.HasPrefix(p, ProxyPathLocal)
}

// withPanelAuth 面板 API 与 /v1 同口径鉴权（api_key 为空则放行）。
func (s *Server) withPanelAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.config().APIKey != "" && bearer(r) != s.config().APIKey {
			debuglog.Event(r, "warn", "panel_authentication_rejected", map[string]any{
				"status_code": http.StatusUnauthorized,
			})
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": 401, "message": "未提供有效 API 密钥"},
			})
			return
		}
		next(w, r)
	}
}

func bearer(r *http.Request) string {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		// Anthropic 客户端（Claude Code 等）用 x-api-key 头携带密钥；
		// 只认 Authorization 会让它们在配置了 api-key 的网关上永远 401。
		token = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	return token
}

// withPanelAuthHandler 是 withPanelAuth 的 http.Handler 版本（反代需要挂 Handler 而非 HandlerFunc）。
func (s *Server) withPanelAuthHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.withPanelAuth(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		})(w, r)
	})
}

// ProxyPathLocal 是反代到本机代理的挂载前缀。
const ProxyPathLocal = localagent.ProxyPath

// -----------------------------------------------------------------------------
// 基础端点
// -----------------------------------------------------------------------------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "WorkBuddy Gateway v"+Version+" is running.\n\n"+
		"Endpoints:\n"+
		"- POST /v1/chat/completions\n"+
		"- GET  /v1/models\n"+
		"- GET  /healthz\n"+
		"- GET  /status\n"+
		"- GET  /panel/          (Web 管理面板)\n")
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	source, _ := s.cat.Source()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":       "healthy",
		"timestamp":    time.Now().Unix(),
		"version":      Version,
		"accounts":     s.pool.Summary(),
		"model_count":  len(s.cat.Merged()),
		"model_source": source,
	})
}

// handleModels 输出 OpenAI 兼容的模型列表。
//
// 两条约定：
//
//  1. **默认按站点展开**：同一个模型在国内站与国际站是两条独立条目，id 带 `CN-` / `AI-`
//     前缀。客户端（Codex / Claude Code / OpenCode 等）拉一次列表就能直接挑站点，
//     不必自己拼前缀，也不会把两边的倍率与可用性混为一谈 —— 原先只发合并后的裸名，
//     而两站的倍率与账号池本就不同，客户端无从区分。
//  2. **附带上游真实能力**：上下文长度、最大输出、倍率、思考档位。客户端据此决定
//     上下文与 reasoning 参数，字段缺失时它们只能保守取值。
//
// 裸名（不带前缀）**仍然可用**，只是不再出现在列表里 —— 它走默认调度逻辑。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.cat.Merged()
	source, _ := s.cat.Source()

	list := make([]map[string]any, 0, len(models)*2)
	for _, m := range models {
		blocked, _ := s.config().ModelDisabled(m.ID)
		for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
			// 只列该站点真实存在的模型。把另一站的也列出来，客户端会拿到一个
			// 「看起来能调、实际不存在」的 ID。
			if _, ok := s.cat.Entry(site, m.ID); !ok {
				continue
			}
			item := map[string]any{
				"id":         siteModelID(site, m.ID),
				"object":     "model",
				"owned_by":   "workbuddy",
				"permission": []any{},
				"disabled":   blocked,
				"site":       site,
				"site_label": auth.SiteLabel(site),
				"base_model": m.ID,
			}
			if cl := m.ContextLength(); cl > 0 {
				item["context_length"] = cl
			}
			if m.MaxOutputTokens > 0 {
				item["max_output_tokens"] = m.MaxOutputTokens
			}
			if len(m.ContextWindowOptions) > 0 {
				item["context_length_options"] = m.ContextWindowOptions
			}
			// 倍率按各自站点取。原先写死取国内站，国际站条目会带上国内站的倍率 ——
			// 两站定价不同，这个数字会直接误导用户判断该用哪边。
			if label := s.cat.DisplayMultiplier(site, m.ID); label != "-" {
				item["multiplier"] = label
			}
			attachReasoning(item, m)
			item["supports_images"] = m.SupportsImages
			item["supports_tool_call"] = m.SupportsToolCall
			list = append(list, item)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Model-Source", source)
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": list})
}

// attachReasoning 把模型的思考能力写进条目。
//
// 为什么要专门成块：客户端想知道 reasoning_effort 该传什么，只能靠这些字段；
// 缺失时它们只能保守取值或干脆不传，表现是「思考按模型默认档跑，用户以为设置没生效」。
//
// **档位清单的兜底规则**：上游没下发 supportedEfforts 时，用 reasoningEffort（默认档）
// 兜底 —— 默认档必然是支持的档位，这是推断而不是编造。改之前只在 supportedEfforts
// 非空时才给字段，实测 56 条里只有 11 条带档位清单、却有 35 条带默认档，
// 客户端拿到的是残缺信息。
func attachReasoning(item map[string]any, m catalog.Model) {
	if !m.SupportsReasoning && !m.OnlyReasoning && m.ReasoningEffort == "" && len(m.SupportedEfforts) == 0 {
		return
	}

	efforts := m.SupportedEfforts
	if len(efforts) == 0 && m.ReasoningEffort != "" {
		efforts = []string{m.ReasoningEffort}
	}

	// 聚合块：一次说清「能不能思考 / 能不能关 / 用哪一档」。
	reasoning := map[string]any{}
	switch {
	case m.OnlyReasoning:
		reasoning["only"] = true
	case m.SupportsReasoning:
		reasoning["supported"] = true
	}
	if m.CanDisableThinking {
		reasoning["can_disable"] = true
	}
	if m.ReasoningEffort != "" {
		reasoning["default_effort"] = m.ReasoningEffort
	}
	if len(efforts) > 0 {
		reasoning["supported_efforts"] = efforts
	}
	if m.ReasoningSummary != "" {
		reasoning["summary"] = m.ReasoningSummary
	}
	item["reasoning"] = reasoning

	// 扁平原字段保留（对外可能已被消费），并把档位清单补上兜底值。
	if m.ReasoningEffort != "" {
		item["reasoning_default_effort"] = m.ReasoningEffort
	}
	if len(efforts) > 0 {
		item["reasoning_supported_efforts"] = efforts
	}
	if m.CanDisableThinking {
		item["reasoning_can_disable"] = true
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	source, _ := s.cat.Source()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":      Version,
		"uptime":       int64(time.Since(s.start).Seconds()),
		"summary":      s.pool.Summary(),
		"model_source": source,
		"accounts":     s.pool.Snapshot(),
	})
}

// -----------------------------------------------------------------------------
// 面板 API
// -----------------------------------------------------------------------------

func (s *Server) handlePanelOverview(w http.ResponseWriter, r *http.Request) {
	source, _ := s.cat.Source()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        Version,
		"uptime_seconds": int64(time.Since(s.start).Seconds()),
		"accounts":       s.pool.Summary(),
		"models": map[string]any{
			"count":  len(s.cat.Merged()),
			"source": source,
		},
	})
}

func (s *Server) handlePanelAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"accounts": s.panelAccounts()})
}

func (s *Server) credentialSource() string {
	switch {
	case s.config().AuthDir != "":
		return "dir:" + s.config().AuthDir
	case s.config().AuthExplicit:
		return "explicit:" + s.config().AuthFile
	default:
		return "auto-discover:" + s.config().WorkDir
	}
}

// -----------------------------------------------------------------------------
// 面板静态资源
// -----------------------------------------------------------------------------

// handlePanelStatic 提供前端构建产物：/panel/ 下命中文件则直出，未命中回落到 index.html
// （SPA 行为，便于后续引入前端路由）。
func (s *Server) handlePanelStatic(w http.ResponseWriter, r *http.Request) {
	if s.panelFS == nil {
		http.Error(w, "面板未构建：请先在 web/ 下执行 npm run build", http.StatusServiceUnavailable)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/panel/")
	if rel == "" {
		rel = "index.html"
	}
	data, err := fs.ReadFile(s.panelFS, rel)
	if err != nil {
		// 未命中静态文件 → 回落到 index.html（前端路由由前端自理）
		serveEmbeddedIndex(w, s.panelFS)
		return
	}
	w.Header().Set("Content-Type", contentTypeByExt(rel))
	// 带内容哈希的产物可长缓存；index.html 不缓存，避免升级后拿到旧壳
	if strings.HasPrefix(rel, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// no-store 而不是 no-cache：面板页面会把接入密钥显示给用户，
		// no-cache 只要求「用前校验」，磁盘上仍然留得下这份带密钥的 HTML。
		w.Header().Set("Cache-Control", "no-store")
	}
	_, _ = w.Write(data)
}

func serveEmbeddedIndex(w http.ResponseWriter, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		http.Error(w, "面板资源缺失", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 面板 HTML 会显示接入密钥，必须 no-store（理由同 handlePanelStatic）。
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func contentTypeByExt(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"), strings.HasSuffix(name, ".mjs"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}

// -----------------------------------------------------------------------------
// 工具
// -----------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeOpenAIError 输出 OpenAI 风格的错误体。
func writeOpenAIError(w http.ResponseWriter, statusCode int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    statusCode,
		},
	})
}

var errNoBody = errors.New("读取请求体失败")
