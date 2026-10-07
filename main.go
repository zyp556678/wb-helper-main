// Command workbuddy-gateway 是三合一后的本地 AI 代理网关。
//
// 反代层以 wb-gateway 的实现为基准（双站反代、模型透传、分段超时、流式规范化），
// 治理与面板来自 wb2api-panel，本机客户端能力由独立的本机代理进程提供（后续切片接入）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"workbuddy-gateway/internal/accountmeta"
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
	"workbuddy-gateway/internal/server"
	"workbuddy-gateway/internal/session"
	"workbuddy-gateway/internal/stats"
	"workbuddy-gateway/internal/tasks"
	"workbuddy-gateway/internal/upstream"
	"workbuddy-gateway/web"
)

// version 是网关版本号。
const version = "0.9.1"

func main() {
	log.SetFlags(log.LstdFlags)

	args := os.Args[1:]
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	switch command {
	case "serve":
		runServe(args)
	case "status":
		runStatus(args)
	case "login":
		runLogin(args)
	case "refresh":
		runRefresh(args)
	case "monitor":
		runMonitor(args)
	case "reset":
		runReset(args)
	case "probe":
		runProbe(args)
	case "version", "-v", "--version":
		fmt.Printf("WorkBuddy Gateway v%s\n", version)
	case "help", "-h", "--help":
		printHelp()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", command)
		printHelp()
		os.Exit(2)
	}
}

func printHelp() {
	fmt.Print(`WorkBuddy Gateway —— 本地 AI 代理网关（国内站 / 国际站双站）

用法:
  workbuddy-gateway [命令] [选项]

命令:
  serve     启动网关（默认命令，不带子命令时等同 serve）
  status    查看账号池状态（读取凭据并打印表格，不启动服务）
  login     设备授权登录：申请授权链接，浏览器 / 手机完成授权后自动保存凭据
            用法：login [-site cn|intl] [-intl] [-auth-dir 目录] [-dry-run]
  refresh   逐账号刷新令牌并查询积分，打印巡检表格（只读，不签到、不改账号状态）
            用法：refresh [-auth-dir 目录]
  monitor   前台持续监控账号池（优先读取运行中的网关，服务未运行时用本地快照）
            用法：monitor [-interval 秒] [-addr ip] [-port 端口]；Ctrl+C 退出
  probe     对运行中的网关发起模型属性探测（指定账号 × 模型，只走本机回环）
            用法：probe [-auth 账号ID] [-models m1,m2] [-limit N]
  reset     清理本地运行数据（保留登录凭据与关联会话登记表），并重新拉取模型目录
  version   查看版本
  help      查看帮助

选项（serve / status 等命令通用，login / refresh / monitor 复用其中一部分）:
  -addr <ip>               监听地址（默认 127.0.0.1）
  -port <port>             监听端口（默认 8317）
  -auth <path>             凭据文件路径，支持逗号分隔多个
  -auth-dir <dir>          凭据目录：加载目录内所有凭据文件
  -api-key <key>           设置后调用网关必须携带 Authorization: Bearer <key>
  -proxy <url>             上游请求代理，如 http://127.0.0.1:7890
  -verbose                 输出详细日志
  -reload-interval <sec>   凭据热加载扫描间隔，0 关闭（默认 5）
  -models-refresh <min>    模型目录刷新间隔，0 关闭（默认 60）

端点:
  POST /v1/chat/completions   对话补全（支持流式与非流式）
  GET  /v1/models             模型列表
  GET  /healthz               健康检查
  GET  /status                账号池状态
  POST /admin/probe           模型属性探测（仅回环；probe 子命令调用它）
  GET  /panel/                Web 管理面板
`)
}

// registerCommonFlags 注册各命令共用的选项，返回 -auth / -auth-dir 的字符串指针
// （Parse 之后再写回配置）。
//
// 抽成公共函数是为了让 login / refresh / monitor 这些前台子命令复用同一套选项定义，
// 而不是各抄一份 —— 抄一份的代价是以后新增通用选项时子命令会悄悄漏掉。
func registerCommonFlags(fs *flag.FlagSet, cfg *config.Config) (authFile, authDir *string) {
	fs.StringVar(&cfg.Addr, "addr", "127.0.0.1", "监听地址")
	fs.IntVar(&cfg.Port, "port", 8317, "监听端口")
	authFile = fs.String("auth", "", "凭据文件路径（可逗号分隔）")
	authDir = fs.String("auth-dir", "", "凭据目录")
	fs.StringVar(&cfg.APIKey, "api-key", "", "访问网关所需的 API Key")
	fs.StringVar(&cfg.ProxyURL, "proxy", "", "上游请求代理")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "输出详细日志")
	fs.IntVar(&cfg.ReloadInterval, "reload-interval", 0, "凭据热加载扫描间隔（秒）")
	fs.IntVar(&cfg.ModelsRefresh, "models-refresh", 0, "模型目录刷新间隔（分钟）")
	return authFile, authDir
}

// applyCommonFlags 把 -auth / -auth-dir 的解析结果写回配置。
func applyCommonFlags(cfg *config.Config, authFile, authDir *string) {
	cfg.AuthExplicit = strings.TrimSpace(*authFile) != ""
	cfg.AuthFile = *authFile
	cfg.AuthDir = *authDir
}

// parseFlags 解析命令行选项。
func parseFlags(args []string) (*config.Config, *flag.FlagSet, error) {
	cfg := &config.Config{}
	fs := flag.NewFlagSet("workbuddy-gateway", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printHelp() }

	authFile, authDir := registerCommonFlags(fs, cfg)

	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	applyCommonFlags(cfg, authFile, authDir)

	reloadSet, modelsSet := false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "reload-interval":
			reloadSet = true
		case "models-refresh":
			modelsSet = true
		}
	})
	cfg.SetFlagPresence(reloadSet, modelsSet)
	return cfg, fs, nil
}

// buildRuntime 按已加载的配置装配上游客户端与账号池（不含命令行解析与配置加载）。
func buildRuntime(cfg *config.Config) (*pool.Pool, *upstream.Client) {
	client := &upstream.Client{
		Control:               cfg.Control,
		ChatHTTP:              cfg.Chat,
		IdleTimeout:           cfg.IdleTimeout,
		TransientRetries:      cfg.TransientRetries,
		TransientRetryBackoff: cfg.TransientRetryBackoff,
		Verbose:               cfg.Verbose,
		Logf:                  log.Printf,
	}
	accountPool := pool.New(cfg, client)
	accountPool.Logf = log.Printf
	accountPool.SetGov(pool.GovFromConfig(cfg))
	accountPool.SetCreditFloor(cfg.CreditFloor()) // 积分保底（默认 0 = 关闭）
	return accountPool, client
}

// build 完成配置加载与依赖装配。
func build(args []string) (*config.Config, *pool.Pool, *upstream.Client, error) {
	opt, _, err := parseFlags(args)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := config.Load(opt)
	if err != nil {
		return nil, nil, nil, err
	}
	accountPool, client := buildRuntime(cfg)
	return cfg, accountPool, client, nil
}

func runServe(args []string) {
	cfg, p, client, err := build(args)
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}

	n, errs := p.Load()
	for _, e := range errs {
		log.Printf("[凭据] 跳过无效文件: %v", e)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 由桌面壳代管时额外盯住父进程：壳被强杀（安装程序 / 任务管理器 / 崩溃）
	// 不会走它的 kill 子进程逻辑，只有这里能防住孤儿进程占住二进制文件。
	watchParentExit(ctx, stop)

	if n > 0 {
		// 启动时保证令牌有效（距过期不足 15 分钟即刷新）
		p.EnsureAllTokens(ctx)
	}

	panelFS, ferr := fs.Sub(web.Dist, "dist")
	if ferr != nil {
		log.Printf("[面板] 静态资源不可用: %v", ferr)
		panelFS = nil
	}

	registry := metrics.New()

	catFile := filepath.Join(cfg.WorkDir, "wb-models-cache.json")
	cat := catalog.New(catFile, client, p)
	cat.Logf = log.Printf
	cat.NPMEnabled = cfg.ModelsNPMEnabled()
	cat.ProbeEnabled = cfg.ProbeEnabled()
	cat.LoadCache()
	// 目录刷新不再阻塞启动。
	//
	// 原先这里同步等一次 RefreshOnce（上限 30 秒）之后才开监听端口，实测那 30 秒
	// 全花在 npm 兜底去下一份 55MB 的整包上（见 internal/catalog/fetch.go）。
	// 而上面的 LoadCache 已经保证 /v1/models 有内容可给 —— 模型本身是透传的，
	// 列表只影响客户端自动补全，首拉没有必须同步的理由。
	//
	// 默认配置下这次刷新由 RefreshLoop 在启动时自己先拉一次（它在 goroutine 里），
	// 所以这里不重复发起：RefreshOnce 没有并发保护，两份会各拉一遍、
	// 再各写一次缓存文件。
	// 只有把刷新间隔显式设成 0（关闭周期刷新）时才需要补这一次，
	// 否则「关掉周期刷新」会连启动首拉一起没了。
	if cfg.ModelsRefresh <= 0 {
		go func() {
			if err := cat.RefreshOnce(ctx); err != nil {
				log.Printf("[目录] 启动首拉失败（将使用缓存）: %v", err)
			}
		}()
	}
	go cat.RefreshLoop(ctx, time.Duration(cfg.ModelsRefresh)*time.Minute)
	go cat.ProbeLoop(ctx, registry.RequestsFor)
	sticky := session.New(cfg.StickyEnabled(), cfg.StickyTTL(), cfg.StickyGCInterval())
	// 积分本地观察台账（切片 19）：每次成功拉取资源包记一条余额快照，
	// 官方用量接口不可用时页面用它回退显示消耗；签到结果也记这里。
	creditWatch := creditwatch.New(filepath.Join(cfg.WorkDir, "wb-credit-history.json"))
	sched := scheduler.New(cfg, p, client)
	sched.Logf = log.Printf
	sched.SetCheckinRecorder(creditWatch)
	sched.Start(ctx)

	// 用量统计：按小时聚合持久化到 wb-stats.json，供面板画趋势
	statStore := stats.New(filepath.Join(cfg.WorkDir, "wb-stats.json"))
	statStore.Logf = log.Printf
	statStore.Load()
	go statsSampler(ctx, statStore, p)

	// 事件日志：面板「日志」页的数据源（内存环形缓冲，不落盘）
	events := eventlog.New(500)
	events.Info(eventlog.ChannelSystem, "startup", "网关启动",
		map[string]any{"version": version, "accounts": p.Len(), "model_count": len(cat.Merged())})

	// 任务中心（切片 5）：成长任务扫描 / 自动完成 / 报名领奖 + 旅行、连登、抽奖
	taskMgr := tasks.New(p, client, cfg, events)
	taskMgr.Logf = log.Printf
	sched.SetTaskManager(taskMgr)

	// 本机代理：由网关代管生命周期（发现 → 拉起 → 探活 → 异常重启 → 优雅退出）。
	// 找不到二进制时静默跳过，这是服务端部署的正常路径。
	localSup := localagent.New(localagent.Config{
		Enabled: cfg.LocalAgentEnabled(),
		BinName: cfg.Local.Bin,
		DataDir: cfg.Local.DataDir,
		Logf:    log.Printf,
		Events:  events,
	})
	localSup.Start(ctx)
	defer localSup.Stop()

	server.Version = version
	srv := server.New(cfg, p, client, cat, registry, sticky, sched, events, taskMgr, statStore, localSup, panelFS)
	srv.SetCreditWatch(creditWatch)
	// 账号备注 / 显示字段（批次 4）：本机展示偏好，单独落一个文件，
	// 不写进凭据、也不进账号池治理状态。
	srv.SetAccountMeta(accountmeta.New(filepath.Join(cfg.WorkDir, "wb-account-meta.json")))
	// 客户端日志限流扫描（切片 19 追加）：WorkBuddy 桌面客户端直连官方，
	// 它的模型限流不经过网关，只能从客户端日志还原（对齐 wb-switch 的扫日志兜底通路）。
	if home, err := os.UserHomeDir(); err == nil {
		srv.SetClientLimits(clientlimits.New(
			filepath.Join(home, ".workbuddy", "logs"),
			filepath.Join(home, ".workbuddy-ai", "logs"),
		))
	}

	// 调试事件流：debug.enabled 打开时按行落 `debug-YYYY-MM-DD.jsonl`
	// （请求进入/返回、鉴权被拒、模型被策略挡下、上游重试…）。
	debuglog.SetVersion(version)
	if cfg.DebugEnabled {
		closeDebug, err := debuglog.Enable(cfg.WorkDir)
		if err != nil {
			log.Printf("[debug] 事件流开启失败（其余功能不受影响）: %v", err)
		} else {
			defer closeDebug()
			log.Printf("[debug] 事件流已开启: %s", debuglog.Path())
		}
	}

	// 请求级观测：内存指标始终启用；JSONL 归档默认开（只写脱敏元数据，
	// 且队列满会丢弃而非阻塞请求，所以默认开的代价很低，而默认关会让排障没有历史可查）。
	requestLog := reqlog.New(reqlog.Config{
		Dir:           filepath.Join(cfg.WorkDir, "request-logs"),
		Enabled:       cfg.RequestArchiveEnabled,
		RetentionDays: cfg.RequestRetentionDays,
		MaxBytes:      int64(cfg.RequestArchiveMaxMB) << 20,
	})
	defer requestLog.Close()
	srv.SetRequestLog(requestLog)
	if st := requestLog.Snapshot().Archive; st.Enabled {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			st.Dir, cfg.RequestRetentionDays, cfg.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档已关闭")
	}

	srv.StartBackground(ctx)

	printBanner(cfg, p, n)

	httpSrv := &http.Server{
		Addr:    cfg.ListenAddr(),
		Handler: srv.Handler(),
		// ReadTimeout 覆盖整个请求读取（含 body 上传），防慢速 body 拖死连接。
		// 缺省 300s（server.read_timeout 可改，"0" = 不限制）：旧值 120s 会掐掉大上下文 /
		// 文件块经反代链的慢速上传，客户端只看到 400 "read body: ... i/o timeout"，
		// 而看不出是网关掐的。属装配期字段，改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeout,
		// ReadHeaderTimeout 单独设：只约束「连接建立 → 收到请求头」，
		// 让慢速 body（长上传）不被误伤，同时挡住只发半个请求头的空连接。
		ReadHeaderTimeout: 30 * time.Second,
		// 流式对话可能持续数分钟，不设写总时长上限；
		// 长流的安全性由上游空闲看门狗与客户端取消共同保证。
		WriteTimeout: 0,
	}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("收到退出信号，正在关闭网关…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	// 退出前把统计刷盘：否则最后一分钟的请求计数会随进程消失
	// （采样器周期是 1 分钟，被强杀或优雅退出都可能刚好错过那次写）。
	statStore.FlushIfDirty()
	log.Printf("网关已停止")
}

func printBanner(cfg *config.Config, p *pool.Pool, loaded int) {
	listen := cfg.ListenAddr()
	fmt.Println("================================================================")
	fmt.Printf("WorkBuddy Gateway v%s\n", version)
	fmt.Printf("   服务监听地址:  http://%s\n", listen)
	fmt.Printf("   对话接口地址:  http://%s/v1/chat/completions\n", listen)
	fmt.Printf("   模型接口地址:  http://%s/v1/models\n", listen)
	fmt.Printf("   管理面板:      http://%s/panel/\n", listen)
	fmt.Printf("   模型转发策略:  【完全透传】客户端请求的任意 model 原样中继至上游\n")
	fmt.Printf("   凭据来源:      %s\n", credentialSourceLabel(cfg))
	if cfg.ReloadInterval > 0 {
		fmt.Printf("   凭据热加载:    每 %ds 自动扫描，新增/更新/删除凭据免重启生效\n", cfg.ReloadInterval)
	} else {
		fmt.Printf("   凭据热加载:    已关闭\n")
	}
	if cfg.APIKey != "" {
		fmt.Printf("   API 鉴权:      已启用\n")
	} else {
		fmt.Printf("   API 鉴权:      未启用（任何客户端均可直连）\n")
	}
	if cfg.ProxyURL != "" {
		fmt.Printf("   上游出口代理:  %s\n", cfg.ProxyURL)
	}
	fmt.Printf("   上游超时:      响应头等待 %v / 流空闲 %v（%s 可覆盖）\n",
		cfg.HeaderTimeout, cfg.IdleTimeout, cfg.ConfigFile)
	fmt.Printf("   账号池:        %d 个账号\n", loaded)
	if loaded > 0 {
		fmt.Println("   ----------------------------------------------------------")
		for i, a := range p.Snapshot() {
			state := "可用"
			switch {
			case a.Disabled:
				state = "已禁用"
			case a.CooldownUntil > time.Now().Unix():
				state = "冷却中"
			}
			exp := "无"
			if a.TokenExpiresAt > 0 {
				exp = time.Unix(a.TokenExpiresAt, 0).Format("2006-01-02 15:04:05")
			}
			fmt.Printf("   #%d  %-24s %-8s %-12s %-8s 令牌有效至 %s\n",
				i+1, short(displayName(a.Nickname, a.UID, a.File)), a.SiteLabel, state, "", exp)
		}
		fmt.Println("   ----------------------------------------------------------")
	} else {
		fmt.Println("   提示: 未检测到有效凭据。请把凭据文件放到工作目录（自动发现 workbuddy*.json），")
		fmt.Println("         或用 -auth <文件> / -auth-dir <目录> 指定后重启。")
	}
	fmt.Println("================================================================")
}

func credentialSourceLabel(cfg *config.Config) string {
	switch {
	case cfg.AuthDir != "":
		return "目录 " + cfg.AuthDir
	case cfg.AuthExplicit:
		return "指定文件 " + cfg.AuthFile
	default:
		return "自动发现 " + cfg.WorkDir
	}
}

func runStatus(args []string) {
	cfg, p, _, err := build(args)
	if err != nil {
		log.Fatalf("失败: %v", err)
	}
	n, errs := p.Load()
	for _, e := range errs {
		log.Printf("[凭据] 跳过无效文件: %v", e)
	}

	fmt.Println("================== WorkBuddy 账号池状态 ==================")
	fmt.Printf("凭据来源: %s\n", credentialSourceLabel(cfg))
	fmt.Printf("账号总数: %d\n", n)
	if n == 0 {
		fmt.Println("未检测到有效凭据。")
		return
	}
	now := time.Now()
	for i, a := range p.Snapshot() {
		state := "可用"
		switch {
		case a.Disabled:
			state = "已禁用"
		case a.CooldownUntil > now.Unix():
			state = "冷却中"
		}
		fmt.Printf("\n--- 账号 #%d ---\n", i+1)
		fmt.Printf("凭据文件:     %s\n", a.File)
		fmt.Printf("站点:         %s\n", a.SiteLabel)
		fmt.Printf("用户昵称:     %s\n", displayName(a.Nickname, a.UID, "-"))
		fmt.Printf("用户 UID:     %s\n", orDash(a.UID))
		fmt.Printf("企业 ID:      %s\n", orDash(a.EnterpriseID))
		fmt.Printf("状态:         %s\n", state)
		if a.Disabled && a.DisabledReason != "" {
			fmt.Printf("失效原因:     %s\n", a.DisabledReason)
		}
		if a.CooldownUntil > now.Unix() {
			fmt.Printf("冷却至:       %s\n", time.Unix(a.CooldownUntil, 0).Format("2006-01-02 15:04:05"))
		}
		if a.TokenExpiresAt > 0 {
			rest := time.Until(time.Unix(a.TokenExpiresAt, 0)).Round(time.Second)
			fmt.Printf("过期时间:     %s (剩余 %v)\n",
				time.Unix(a.TokenExpiresAt, 0).Format("2006-01-02 15:04:05"), rest)
		} else {
			fmt.Printf("过期时间:     未知\n")
		}
	}
}

// probeResultRow 与 server 侧 /admin/probe 的响应结构对齐（只取展示需要的字段）。
type probeResultRow struct {
	Account string  `json:"account"`
	Site    string  `json:"site"`
	Model   string  `json:"model"`
	Status  string  `json:"status"`
	Credit  float64 `json:"credit"`
	Tokens  int64   `json:"tokens"`
	Detail  string  `json:"detail,omitempty"`
}

type probeResponseDoc struct {
	Results []probeResultRow `json:"results"`
	Summary map[string]int   `json:"summary"`
}

// runProbe 是 probe 子命令：作为**客户端**调用运行中服务的 /admin/probe。
//
// 为什么不由本进程直连上游探测：免费/收费台账保存在 serve 进程内存里
// （只周期性快照到状态文件），独立进程写下的结论会被运行中的服务覆盖 ——
// 探测结果看起来"生效了"，下一次快照就没了。
//
// 所以这里只负责发请求与展示；真正执行探测并更新台账的是运行中的服务。
func runProbe(args []string) {
	// probe 专属参数先摘出来再交给通用解析：把它们注册进 serve 的 flag 集合
	// 会让 `serve -h` 里出现一堆只对 probe 有意义的选项。
	var account, models string
	limit := 0
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		takeValue := func(prefix string) (string, bool) {
			if a == prefix && i+1 < len(args) {
				i++
				return args[i], true
			}
			if strings.HasPrefix(a, prefix+"=") {
				return strings.TrimPrefix(a, prefix+"="), true
			}
			return "", false
		}
		if v, ok := takeValue("-auth"); ok {
			account = v
			continue
		}
		if a == "-auth" || strings.HasPrefix(a, "-auth=") {
			// -auth 也是通用选项（凭据文件路径）：这里当作账号筛选，
			// 因为对 probe 来说「指定哪个账号」才是它唯一的用途。
			continue
		}
		if v, ok := takeValue("-models"); ok {
			models = v
			continue
		}
		if v, ok := takeValue("-limit"); ok {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
			continue
		}
		rest = append(rest, args[i])
	}

	cfg, _, _, err := build(rest)
	if err != nil {
		log.Fatalf("失败: %v", err)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)
	if cfg.Addr == "0.0.0.0" || cfg.Addr == "::" {
		addr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	url := "http://" + addr + "/admin/probe"

	body := map[string]any{}
	if account != "" {
		body["account"] = account
	}
	if models != "" {
		var list []string
		for _, m := range strings.Split(models, ",") {
			if m = strings.TrimSpace(m); m != "" {
				list = append(list, m)
			}
		}
		body["models"] = list
	}
	if limit > 0 {
		body["limit"] = limit
	}
	payload, _ := json.Marshal(body)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		fmt.Printf("构造请求失败: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	fmt.Printf("正在请求 %s（账号=%s，模型=%s）...\n", url,
		orDash(account), short(orDash(models)))

	resp, err := (&http.Client{Timeout: 20 * time.Minute}).Do(req)
	if err != nil {
		fmt.Printf("调用失败: %v\n请确认 serve 正在运行，且 -addr/-port 与之一致。\n", err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("服务返回 HTTP %d: %s\n", resp.StatusCode, short(string(raw)))
		return
	}
	var out probeResponseDoc
	if err := json.Unmarshal(raw, &out); err != nil {
		fmt.Printf("解析响应失败: %v\n", err)
		return
	}

	sort.SliceStable(out.Results, func(i, j int) bool {
		if out.Results[i].Account != out.Results[j].Account {
			return out.Results[i].Account < out.Results[j].Account
		}
		return out.Results[i].Model < out.Results[j].Model
	})
	fmt.Printf("\n%-24s %-8s %-26s %-12s %-10s %-8s %s\n",
		"账号", "站点", "模型", "结果", "credit", "tokens", "说明")
	for _, r := range out.Results {
		fmt.Printf("%-24s %-8s %-26s %-12s %-10s %-8d %s\n",
			short(r.Account), orDash(r.Site), short(r.Model), r.Status,
			fmt.Sprintf("%.4f", r.Credit), r.Tokens, r.Detail)
	}
	keys := make([]string, 0, len(out.Summary))
	for k := range out.Summary {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("\n汇总: ")
	for _, k := range keys {
		fmt.Printf("%s=%d ", k, out.Summary[k])
	}
	fmt.Println()
}

func displayName(nickname, uid, fallback string) string {
	if strings.TrimSpace(nickname) != "" {
		return nickname
	}
	if strings.TrimSpace(uid) != "" {
		return uid
	}
	return fallback
}

func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

func short(s string) string {
	if len(s) <= 24 {
		return s
	}
	return s[:21] + "..."
}

// statsSampler 周期采样账号余额并落盘用量统计。
//
// 采样余额而不是「按请求累加消耗」：上游的计费是延迟结算且存在免费模型，
// 只有余额差分才是真实消耗的可靠来源（这一点在价格探测里已经踩过）。
// 每 5 分钟一次、同一小时只保留最后一个点（见 stats.RecordCredits 的注释）。
func statsSampler(ctx context.Context, store *stats.Store, p *pool.Pool) {
	sample := func() {
		accounts := map[string]float64{}
		for _, a := range p.Snapshot() {
			if a.Quota != nil {
				accounts[a.ID] = a.Quota.Remaining
			}
		}
		if len(accounts) > 0 {
			store.RecordCredits(accounts)
		}
		store.FlushIfDirty()
	}

	// 采样间隔取 1 分钟而不是 5 分钟：余额是趋势图上的**存量**指标，
	// 而刷新额度可能发生在任意时刻（面板按钮、签到、周期刷新）。
	// 间隔太长会让最近一个点长时间停留在旧值，图上看起来像「余额没变」。
	// 代价可忽略：同一小时只保留最后一个采样点，写盘也只在有改动时发生。
	sample() // 启动时先采一次，避免首屏没有余额点
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			store.FlushIfDirty()
			return
		case <-ticker.C:
			sample()
		}
	}
}
