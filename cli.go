package main

// -----------------------------------------------------------------------------
// 前台 CLI 子命令：login / refresh / monitor
//
// 对照参考实现 wb-gateway 的三个命令，落到本仓库的结构上：
//
//	login    终端内走同一套设备授权流程（internal/upstream 的 AuthState / PollToken /
//	         LoginAccount），成功后用 internal/auth 的 NewForLogin + SaveFull 落盘
//	         （workbuddy-<uid>.json，0600、原子替换，与面板登录同一格式）。
//	refresh  命令行版「令牌 + 余额」巡检：只读刷新，不签到、不解冻、不改账号状态。
//	monitor  持续监控：优先读取运行中 serve 的 /status（服务端由 pool.Snapshot() 产出），
//	         服务不可达时回退到本进程按同一套 pool 查询接口取本地凭据快照。
//
// 安全红线：本文件任何输出都不得包含 accessToken / refreshToken / 凭据文件内容，
// 只允许 uid、昵称、站点、状态、积分、到期时间这类展示字段。
// 因此这里的打印语句逐字段手写，不要把凭据或上游响应结构体整体 %+v 打出来。
// -----------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/upstream"
)

const (
	// exitUsage 是参数错误退出码（与 main 对未知命令的取值一致）。
	exitUsage = 2
	// exitFailure 是执行失败退出码。
	exitFailure = 1
)

// refreshAccountTimeout 是 refresh 对单个账号的令牌 + 额度操作总预算。
const refreshAccountTimeout = 60 * time.Second

// loginPollEvery 是 login 轮询上游授权状态的间隔；测试会调小，
// 与 upstream.ControlTimeout 的「测试可调」手法一致。
var loginPollEvery = 2 * time.Second

func runLogin(args []string)   { os.Exit(cmdLogin(args, os.Stdout, os.Stderr)) }
func runRefresh(args []string) { os.Exit(cmdRefresh(args, os.Stdout, os.Stderr)) }
func runMonitor(args []string) { os.Exit(cmdMonitor(args, os.Stdout, os.Stderr)) }

// -----------------------------------------------------------------------------
// 参数解析的公共部分
// -----------------------------------------------------------------------------

// cliFlagSet 构造子命令专属的 flag 集合：注册共用选项，错误输出丢弃
// （解析结果由调用方统一打印中文提示与用法，避免 flag 包再插一段英文）。
func cliFlagSet(name string, cfg *config.Config, usage func()) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = usage
	authFile, authDir := registerCommonFlags(fs, cfg)
	return fs, authFile, authDir
}

// cliHelpRequested 判断参数是否在请求用法（`<cmd> help` / `-h` / `--help`）。
// 命中时打印用法而不是执行命令，避免误跑（登录、刷新都有副作用或耗时）。
func cliHelpRequested(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "help", "-h", "--help":
		return true
	}
	return false
}

// cliParseError 统一处理解析失败：ErrHelp 打印用法并成功返回；
// 其余按参数错误处理（打印原因与用法，退出码 2）。
func cliParseError(err error, usage func(), errOut io.Writer) int {
	if errors.Is(err, flag.ErrHelp) {
		usage()
		return 0
	}
	fmt.Fprintf(errOut, "参数错误: %v\n\n", err)
	usage()
	return exitUsage
}

// cliExtraArgs 检查是否存在多余的位置参数（拼错的选项、忘了加 `-` 等）。
func cliExtraArgs(fs *flag.FlagSet) error {
	if fs.NArg() == 0 {
		return nil
	}
	return fmt.Errorf("无法识别的参数: %s", strings.Join(fs.Args(), " "))
}

// credentialDir 返回新凭据应写入的目录：-auth-dir 优先，否则工作目录。
// 与 pool.CredentialPathFor 的落点保持一致（相对目录按工作目录展开）。
func credentialDir(cfg *config.Config) string {
	if strings.TrimSpace(cfg.AuthDir) != "" {
		dir := cfg.AuthDir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cfg.WorkDir, dir)
		}
		return dir
	}
	return cfg.WorkDir
}

// -----------------------------------------------------------------------------
// login：设备授权登录
// -----------------------------------------------------------------------------

func printLoginUsage(w io.Writer) {
	fmt.Fprint(w, `用法: workbuddy-gateway login [选项]

设备授权登录：向上游申请授权链接，在浏览器 / 手机完成授权后自动保存凭据。

选项:
  -site cn|intl   登录站点（默认 cn 国内站；intl 国际站）
  -intl           等价于 -site intl
  -dry-run        只展示将要写入的凭据路径与账号摘要，不写任何文件
  -auth-dir <dir> 凭据目录（默认取配置的工作目录）
  -proxy <url>    上游请求代理
  -verbose        输出详细日志

说明:
  授权链接打印在终端；凭据写入格式与面板登录一致（workbuddy-<uid>.json，权限 0600）。
  令牌内容不会出现在终端输出里。
`)
}

func cmdLogin(args []string, out, errOut io.Writer) int {
	usage := func() { printLoginUsage(errOut) }
	if cliHelpRequested(args) {
		printLoginUsage(out)
		return 0
	}

	cfg := &config.Config{}
	fs, authFile, authDir := cliFlagSet("login", cfg, usage)
	var (
		site   = fs.String("site", "cn", "登录站点：cn 国内站 / intl 国际站")
		intl   = fs.Bool("intl", false, "等价于 -site intl")
		dryRun = fs.Bool("dry-run", false, "只打印将要写入的路径与账号摘要，不落盘")
	)
	if err := fs.Parse(args); err != nil {
		return cliParseError(err, usage, errOut)
	}
	if err := cliExtraArgs(fs); err != nil {
		return cliParseError(err, usage, errOut)
	}

	siteName := strings.ToLower(strings.TrimSpace(*site))
	if *intl && siteName == auth.SiteCN {
		siteName = auth.SiteINTL
	}
	if siteName != auth.SiteCN && siteName != auth.SiteINTL {
		fmt.Fprintf(errOut, "参数错误: 不支持的站点 %q（只支持 cn / intl）\n\n", *site)
		usage()
		return exitUsage
	}

	applyCommonFlags(cfg, authFile, authDir)
	cfg, err := config.Load(cfg)
	if err != nil {
		fmt.Fprintf(errOut, "读取配置失败: %v\n", err)
		return exitFailure
	}
	p, client := buildRuntime(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := doLogin(ctx, client, cfg, p, siteName, *dryRun, out); err != nil {
		fmt.Fprintf(errOut, "登录失败: %v\n", err)
		return exitFailure
	}
	return 0
}

// doLogin 执行设备授权登录的完整流程：申请授权 → 打印链接与会话码 → 轮询令牌。
//
// 轮询期间的瞬时网络错误不打断登录（用户可能还在授权，重试即可）；
// 只有拿到令牌才会进入落盘收尾（见 finishLogin）。
func doLogin(ctx context.Context, client *upstream.Client, cfg *config.Config, p *pool.Pool, site string, dryRun bool, out io.Writer) error {
	prof := upstream.ProfileForSite(site)

	fmt.Fprintln(out, "================ WorkBuddy 登录 ================")
	fmt.Fprintf(out, "目标站点:     %s (%s)\n", prof.Label, strings.TrimPrefix(prof.Base, "https://"))
	fmt.Fprintf(out, "凭据目录:     %s\n", credentialDir(cfg))
	fmt.Fprintln(out, "正在向上游申请设备授权…")

	st, err := client.AuthState(ctx, prof)
	if err != nil {
		return fmt.Errorf("获取授权链接失败: %w", err)
	}

	fmt.Fprintln(out, "\n请在浏览器 / 手机中打开以下授权链接完成登录：")
	fmt.Fprintf(out, "  %s\n", st.AuthURL)
	fmt.Fprintf(out, "登录会话码:   %s（本次登录使用，不是账号凭据）\n", st.State)

	ttl := upstream.LoginTTL[site]
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	fmt.Fprintf(out, "\n等待授权完成（最长 %v，按 Ctrl+C 取消）...\n", ttl.Round(time.Second))

	ticker := time.NewTicker(loginPollEvery)
	defer ticker.Stop()
	deadline := time.Now().Add(ttl)
	warned := false
	for {
		select {
		case <-ctx.Done():
			return errors.New("已取消（Ctrl+C）")
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("等待授权超时（%v），请重新执行 login", ttl.Round(time.Second))
			}
			tok, ok, err := client.PollToken(ctx, prof, st.State)
			if err != nil {
				if !warned {
					warned = true
					fmt.Fprintf(out, "  轮询登录状态暂时失败（将继续重试）: %v\n", err)
				}
				continue
			}
			warned = false
			if !ok {
				continue // 用户还没完成授权，继续等
			}
			return finishLogin(ctx, client, p, prof, st.State, tok, dryRun, out)
		}
	}
}

// finishLogin 是拿到令牌后的收尾：取账号信息 → 写凭据（dry-run 只预览）→ 打印摘要。
func finishLogin(ctx context.Context, client *upstream.Client, p *pool.Pool, prof *upstream.Profile,
	state string, tok upstream.RefreshedToken, dryRun bool, out io.Writer) error {

	info, err := client.LoginAccount(ctx, prof, state, tok.AccessToken)
	if err != nil {
		// 与面板登录一致：账号信息失败不阻断落盘 —— uid 缺失时按时间戳命名文件。
		fmt.Fprintf(out, "  已拿到令牌但取账号信息失败（继续保存）: %v\n", err)
	}

	expiresAt := int64(0)
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	path := p.CredentialPathFor(info.UID)
	cred := auth.NewForLogin(path, prof.Key, tok.AccessToken, tok.RefreshToken,
		expiresAt, tok.Domain, info.UID, info.EnterpriseID, info.Nickname)

	fmt.Fprintln(out)
	if dryRun {
		fmt.Fprintln(out, "[dry-run] 已取得凭据，按 -dry-run 不落盘。")
		fmt.Fprintf(out, "将要写入:     %s\n", path)
	} else {
		if err := cred.SaveFull(); err != nil {
			return fmt.Errorf("登录成功但保存凭据失败: %w", err)
		}
		// 重新登录即恢复：清掉同名 .disabled 标记并加入内存池，
		// 走 pool.Add 的既有语义（除标记文件外无其他磁盘副作用）。
		p.Add(cred)
		fmt.Fprintln(out, "登录成功")
		fmt.Fprintf(out, "凭据已保存至: %s\n", path)
	}

	fmt.Fprintf(out, "站点:         %s (%s)\n", prof.Label, strings.TrimPrefix(prof.Base, "https://"))
	fmt.Fprintf(out, "账号:         %s (UID: %s)\n",
		displayName(info.Nickname, info.UID, "(未知)"), orDash(info.UID))
	if info.EnterpriseID != "" {
		fmt.Fprintf(out, "企业 ID:      %s\n", info.EnterpriseID)
	}
	if expiresAt > 0 {
		fmt.Fprintf(out, "令牌有效期至: %s\n", time.Unix(expiresAt, 0).Format("2006-01-02 15:04:05"))
	}
	if dryRun {
		fmt.Fprintln(out, "\n（dry-run 结束，未写入任何文件）")
	} else {
		fmt.Fprintln(out, "\n现在可以运行 workbuddy-gateway serve 启动网关。")
	}
	return nil
}

// -----------------------------------------------------------------------------
// refresh：命令行巡检（刷新令牌 + 积分）
// -----------------------------------------------------------------------------

func printRefreshUsage(w io.Writer) {
	fmt.Fprint(w, `用法: workbuddy-gateway refresh [选项]

逐账号巡检：确保令牌新鲜并刷新积分，最后打印一张对齐的表格与汇总。

选项:
  -auth-dir <dir> 凭据目录（默认取配置的工作目录；也可用 -auth 指定单文件）
  -proxy <url>    上游请求代理
  -verbose        输出详细日志

说明:
  只读刷新：不签到、不解冻、不修改账号状态。
  唯一的例外与网关一致：令牌刷新被上游 401/403 明确拒绝时，会把该账号标记失效。
  任何令牌内容都不会出现在输出里。
`)
}

func cmdRefresh(args []string, out, errOut io.Writer) int {
	usage := func() { printRefreshUsage(errOut) }
	if cliHelpRequested(args) {
		printRefreshUsage(out)
		return 0
	}

	cfg := &config.Config{}
	fs, authFile, authDir := cliFlagSet("refresh", cfg, usage)
	if err := fs.Parse(args); err != nil {
		return cliParseError(err, usage, errOut)
	}
	if err := cliExtraArgs(fs); err != nil {
		return cliParseError(err, usage, errOut)
	}
	applyCommonFlags(cfg, authFile, authDir)

	cfg, err := config.Load(cfg)
	if err != nil {
		fmt.Fprintf(errOut, "读取配置失败: %v\n", err)
		return exitFailure
	}
	p, _ := buildRuntime(cfg)
	n, errs := p.Load()
	for _, e := range errs {
		fmt.Fprintf(errOut, "[凭据] 跳过无效文件: %v\n", e)
	}
	if n == 0 {
		fmt.Fprintln(out, "账号池为空：未检测到有效凭据。")
		fmt.Fprintf(out, "凭据来源: %s\n", credentialSourceLabel(cfg))
		fmt.Fprintln(out, "提示：先执行 workbuddy-gateway login 登录，或用 -auth-dir 指定凭据目录。")
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runRefreshAll(ctx, p, out)
	return 0
}

// runRefreshAll 逐账号「确保令牌新鲜 → 刷新积分」并输出巡检表格与汇总。
//
// 刻意不调用任何签到 / 解冻 / 复活逻辑：这条命令的定位是**只读巡检**，
// 能在不影响账号状态的前提下回答「哪些号还活着、有多少积分、最近出过什么错」。
func runRefreshAll(ctx context.Context, p *pool.Pool, out io.Writer) {
	accs := p.Accounts()
	fmt.Fprintln(out, "============ WorkBuddy 账号巡检：刷新令牌 + 积分 ============")
	fmt.Fprintf(out, "账号总数: %d（只读刷新：不签到、不解冻）\n\n", len(accs))

	rows := make([][]string, 0, len(accs))
	okN, failN, skipN, quotaN := 0, 0, 0, 0
	for i, acc := range accs {
		st := p.StateOf(acc)
		name := truncateText(displayName(st.Nickname, st.UID, st.ID), 24)
		if st.Disabled {
			fmt.Fprintf(out, "[%d/%d] %s ... 跳过（已失效）\n", i+1, len(accs), name)
			skipN++
			rows = append(rows, refreshRow(st, "已失效（跳过）", st.DisabledReason))
			continue
		}
		if ctx.Err() != nil {
			fmt.Fprintf(out, "[%d/%d] 已取消，停止刷新\n", i+1, len(accs))
			break
		}

		fmt.Fprintf(out, "[%d/%d] %s ... ", i+1, len(accs), name)
		accCtx, cancel := context.WithTimeout(ctx, refreshAccountTimeout)
		tokenErr := p.EnsureToken(accCtx, acc) // 距过期不足 15 分钟才真正刷新
		quotaErr := p.RefreshQuota(accCtx, acc)
		cancel()

		st = p.StateOf(acc)
		lastErr := ""
		switch {
		case quotaErr != nil:
			lastErr = quotaErr.Error()
		case tokenErr != nil:
			lastErr = tokenErr.Error()
		default:
			lastErr = st.LastError
		}
		if tokenErr != nil || quotaErr != nil {
			failN++
			fmt.Fprintf(out, "失败（%s）\n", truncateText(lastErr, 60))
		} else {
			okN++
			fmt.Fprintln(out, "完成")
		}
		if st.Quota != nil {
			quotaN++
		}
		rows = append(rows, refreshRow(st, "", lastErr))
	}

	headers := []string{"账号", "站点", "状态", "积分（剩余）", "令牌到期", "最近错误"}
	widths := []int{22, 8, 10, 14, 20, 40}
	fmt.Fprintln(out)
	fmt.Fprintln(out, renderCLITable(headers, widths, rows))
	fmt.Fprintf(out, "\n刷新完成: 成功 %d 个，失败 %d 个，跳过 %d 个（已失效）；积分已刷新 %d/%d 个。\n",
		okN, failN, skipN, quotaN, len(accs))
}

// refreshRow 把账号状态快照转成一行表格单元格。
func refreshRow(st pool.AccountState, stateOverride, lastErr string) []string {
	state := stateOverride
	if state == "" {
		state = accountStateLabel(st, time.Now())
	}
	if strings.TrimSpace(lastErr) == "" {
		lastErr = "-"
	}
	return []string{
		truncateText(displayName(st.Nickname, st.UID, st.ID), 24),
		st.SiteLabel,
		state,
		quotaCell(st.Quota),
		tokenExpiryCell(st.TokenExpiresAt),
		truncateText(lastErr, 80),
	}
}

// -----------------------------------------------------------------------------
// monitor：前台持续监控
// -----------------------------------------------------------------------------

func printMonitorUsage(w io.Writer) {
	fmt.Fprint(w, `用法: workbuddy-gateway monitor [选项]

前台持续监控账号池状态（账号 / 站点 / 状态 / 在途 / 积分 / 冷却剩余 / 失败计数），
按 Ctrl+C 退出。

选项:
  -interval <sec>  刷新间隔秒数（默认 5，必须为正数）
  -addr <ip>       运行中网关的监听地址（默认 127.0.0.1）
  -port <port>     运行中网关的监听端口（默认 8317）
  -api-key <key>   网关启用了鉴权时携带的 API Key
  -auth-dir <dir>  凭据目录（仅在使用本地快照回退时用到）

说明:
  默认读取运行中 serve 的 /status（含在途 / 冷却 / 失败计数等运行态）；
  服务不可达时回退到本进程加载凭据的只读快照，此时运行态字段为空。
  输出到非终端时不做 ANSI 清屏，改为追加分段输出（便于重定向到文件）。
`)
}

func cmdMonitor(args []string, out, errOut io.Writer) int {
	usage := func() { printMonitorUsage(errOut) }
	if cliHelpRequested(args) {
		printMonitorUsage(out)
		return 0
	}

	cfg := &config.Config{}
	fs, authFile, authDir := cliFlagSet("monitor", cfg, usage)
	interval := fs.Int("interval", 5, "刷新间隔（秒）")
	if err := fs.Parse(args); err != nil {
		return cliParseError(err, usage, errOut)
	}
	if err := cliExtraArgs(fs); err != nil {
		return cliParseError(err, usage, errOut)
	}
	if *interval <= 0 {
		fmt.Fprintf(errOut, "参数错误: -interval 必须为正数（收到 %d）\n\n", *interval)
		usage()
		return exitUsage
	}
	applyCommonFlags(cfg, authFile, authDir)

	cfg, err := config.Load(cfg)
	if err != nil {
		fmt.Fprintf(errOut, "读取配置失败: %v\n", err)
		return exitFailure
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runMonitorLoop(ctx, cfg, time.Duration(*interval)*time.Second, out)
	return 0
}

// monitorStatusDoc 与运行中网关 GET /status 的响应对齐；
// account 部分就是 pool.AccountState 的 JSON 形态（服务端由 pool.Snapshot() 产出）。
type monitorStatusDoc struct {
	Version  string              `json:"version"`
	Uptime   int64               `json:"uptime"`
	Summary  pool.Summary        `json:"summary"`
	Accounts []pool.AccountState `json:"accounts"`
}

// runMonitorLoop 周期取数并重画状态表，直到 Ctrl+C。
func runMonitorLoop(ctx context.Context, cfg *config.Config, interval time.Duration, out io.Writer) {
	isTTY := false
	if fi, err := os.Stdout.Stat(); err == nil {
		isTTY = fi.Mode()&os.ModeCharDevice != 0
	}
	addr := gatewayStatusAddr(cfg)
	fmt.Fprintln(out, "================ WorkBuddy 实时监控 ================")
	fmt.Fprintf(out, "网关地址: %s | 刷新间隔: %v | 按 Ctrl+C 退出\n", addr, interval)

	// 本地回退池按需创建：只加载凭据，不发任何上游请求，也不修改任何账号状态。
	var local *pool.Pool
	for {
		now := time.Now()
		doc, err := fetchGatewayStatus(ctx, cfg, addr)
		source := fmt.Sprintf("运行中的网关 %s", addr)
		if err != nil || doc == nil {
			if local == nil {
				p, _ := buildRuntime(cfg)
				_, _ = p.Load()
				local = p
			} else {
				local.Reload()
			}
			doc = &monitorStatusDoc{
				Summary:  local.Summary(),
				Accounts: local.Snapshot(),
			}
			source = "本地凭据快照（网关不可达: " + truncateText(err.Error(), 40) + "）"
		}

		if isTTY {
			fmt.Fprint(out, "\033[H\033[2J") // ANSI 清屏重画
		} else {
			fmt.Fprintln(out)
			fmt.Fprintln(out, strings.Repeat("-", 64))
		}
		renderMonitorFrame(out, now, source, doc)

		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "\n监控已退出。")
			return
		case <-time.After(interval):
		}
	}
}

// gatewayStatusAddr 返回探测运行中网关的地址（监听 0.0.0.0/:: 时按回环访问）。
func gatewayStatusAddr(cfg *config.Config) string {
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)
	if cfg.Addr == "0.0.0.0" || cfg.Addr == "::" {
		addr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	return addr
}

// fetchGatewayStatus 读取运行中网关的 /status；失败时由调用方回退本地快照。
func fetchGatewayStatus(ctx context.Context, cfg *config.Config, addr string) (*monitorStatusDoc, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/status", nil)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("网关返回 HTTP %d", resp.StatusCode)
	}
	var doc monitorStatusDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析 /status 响应失败: %w", err)
	}
	return &doc, nil
}

// renderMonitorFrame 画一帧监控画面。
func renderMonitorFrame(w io.Writer, now time.Time, source string, doc *monitorStatusDoc) {
	s := doc.Summary
	fmt.Fprintf(w, "更新时间: %s | 数据源: %s\n", now.Format("2006-01-02 15:04:05"), source)
	fmt.Fprintf(w, "账号池: 共 %d 个 | 可用 %d | 冷却 %d | 已禁用 %d | 在途 %d | 剩余积分 %.2f（%d 个已查）\n",
		s.Total, s.Active, s.Cooldown, s.Disabled, s.InFlight, s.CreditsRemaining, s.QuotaKnown)

	if len(doc.Accounts) == 0 {
		fmt.Fprintln(w, "（没有账号；请先执行 login 或用 -auth-dir 指定凭据目录）")
		return
	}
	headers := []string{"账号", "站点", "状态", "在途", "积分", "冷却剩余", "失败计数"}
	widths := []int{22, 8, 10, 6, 14, 12, 10}
	rows := make([][]string, 0, len(doc.Accounts))
	for _, a := range doc.Accounts {
		rows = append(rows, []string{
			truncateText(displayName(a.Nickname, a.UID, a.File), 24),
			a.SiteLabel,
			accountStateLabel(a, now),
			strconv.FormatInt(a.InFlight, 10),
			quotaCell(a.Quota),
			cooldownCell(a, now),
			strconv.FormatInt(a.FailureCount, 10),
		})
	}
	fmt.Fprintln(w, renderCLITable(headers, widths, rows))
}

// -----------------------------------------------------------------------------
// 展示辅助
// -----------------------------------------------------------------------------

// accountStateLabel 把账号状态快照收敛成一个中文短标签（优先级：失效 > 熔断 > 冷却 > 过期 > 耗尽）。
func accountStateLabel(st pool.AccountState, now time.Time) string {
	switch {
	case st.Disabled:
		return "已失效"
	case st.BreakerUntil > now.Unix():
		return "熔断中"
	case st.CooldownUntil > now.Unix():
		return "冷却中"
	case st.TokenExpiresAt > 0 && st.TokenExpiresAt <= now.Unix():
		return "令牌过期"
	case st.Quota != nil && st.Quota.Exhausted:
		return "积分耗尽"
	default:
		return "可用"
	}
}

func quotaCell(q *pool.QuotaView) string {
	if q == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", q.Remaining)
}

func cooldownCell(st pool.AccountState, now time.Time) string {
	if until := st.CooldownUntil; until > now.Unix() {
		return humanLeft(time.Until(time.Unix(until, 0)))
	}
	if until := st.BreakerUntil; until > now.Unix() {
		return "熔断 " + humanLeft(time.Until(time.Unix(until, 0)))
	}
	return "-"
}

func tokenExpiryCell(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

// humanLeft 把剩余时长渲染成紧凑文本（1m30s / 2h5m0s）。
func humanLeft(d time.Duration) string {
	if d < 0 {
		return "-"
	}
	return d.Round(time.Second).String()
}

// truncateText 按 rune 截断（不会把中文截成半个字），超长时补省略号。
func truncateText(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || s == "" {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// renderCLITable 渲染带边框的文本表：列宽按**显示宽度**计算（中文按 2 列），
// widths[i] <= 0 表示该列按内容自适应（上限 40 显示列）。
func renderCLITable(headers []string, widths []int, rows [][]string) string {
	w := append([]int(nil), widths...)
	for i := range w {
		if w[i] > 0 {
			continue
		}
		width := displayWidth(headers[i])
		for _, r := range rows {
			if i < len(r) && displayWidth(r[i]) > width {
				width = displayWidth(r[i])
			}
		}
		if width > 40 {
			width = 40
		}
		if width < 4 {
			width = 4
		}
		w[i] = width
	}

	border := func() string {
		var b strings.Builder
		b.WriteByte('+')
		for _, width := range w {
			b.WriteString(strings.Repeat("-", width+2))
			b.WriteByte('+')
		}
		return b.String()
	}
	row := func(cells []string) string {
		var b strings.Builder
		b.WriteByte('|')
		for i, width := range w {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			b.WriteByte(' ')
			b.WriteString(fitCell(cell, width))
			b.WriteString(" |")
		}
		return b.String()
	}

	var b strings.Builder
	b.WriteString(border())
	b.WriteByte('\n')
	b.WriteString(row(headers))
	b.WriteByte('\n')
	b.WriteString(border())
	b.WriteByte('\n')
	for _, r := range rows {
		b.WriteString(row(r))
		b.WriteByte('\n')
	}
	b.WriteString(border())
	return b.String()
}

// displayWidth 返回字符串的终端显示宽度（CJK 全角按 2 列，其余按 1 列）。
func displayWidth(s string) int {
	width := 0
	for _, r := range s {
		if isWideRune(r) {
			width += 2
		} else {
			width++
		}
	}
	return width
}

// isWideRune 判断是否按 2 列显示的宽字符（与参考实现的区间表一致）。
func isWideRune(r rune) bool {
	return r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe10 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6))
}

// fitCell 把单元格补齐或截断到指定显示宽度（截断时保留省略号）。
func fitCell(s string, width int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	if dw := displayWidth(s); dw <= width {
		return s + strings.Repeat(" ", width-dw)
	}
	limit := width - 3
	if limit < 0 {
		limit = 0
	}
	var b strings.Builder
	used := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		rw := 2
		if !isWideRune(r) {
			rw = 1
		}
		if used+rw > limit {
			break
		}
		b.WriteRune(r)
		used += rw
		s = s[size:]
	}
	pad := width - used - 3
	if pad < 0 {
		pad = 0
	}
	return b.String() + "..." + strings.Repeat(" ", pad)
}
