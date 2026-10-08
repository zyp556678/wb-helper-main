package server

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy-gateway/internal/eventlog"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/upstream"
)

// 本文件是用量统计的面板接口（切片 6）。
//
// 与监控页的区别先说清：监控页的 /panel/api/metrics 是**进程内滚动窗口**指标，
// 用途是「现在快不快」（TTFT 均值、在途、冷却）；
// 这里的 /panel/api/stats 是**落盘的长历史**（按小时聚合），用途是「这段时间用了多少」。
// 两者回答的是不同问题，所以没有合并成一个接口。

// handlePanelStats 返回用量趋势。
//
// range 参数接受 24h / 7d / 30d（也兼容纯数字小时数），默认 24h。
func (s *Server) handlePanelStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	hours := parseRange(r.URL.Query().Get("range"))
	writeJSON(w, http.StatusOK, s.stats.Query(hours))
}

// handlePanelStatsDaily 返回按天统计（用量统计 / 积分统计两个页面用）。
//
// 为什么单独一个接口而不是给 /stats 加 granularity 参数：两者的**返回结构不同**
// （按天要带账号维度与输入/输出拆分，按小时不需要），硬塞进一个结构会让
// 每个调用方都要面对一堆用不上的字段；而且保留期也不同（天桶一年、小时桶 30 天），
// 非法组合（比如 1y + 小时粒度）需要额外校验。分成两个接口，各自的契约都干净。
//
// range 接受 today / 7d / 30d / 90d / 180d / 1y（也兼容纯数字天数），默认 30d。
func (s *Server) handlePanelStatsDaily(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	days := parseDays(r.URL.Query().Get("range"))
	writeJSON(w, http.StatusOK, s.stats.QueryDaily(days))
}

// parseRange 解析时间范围参数。
func parseRange(raw string) int {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "24h":
		return 24
	case "7d":
		return 7 * 24
	case "30d":
		return 30 * 24
	case "1h":
		return 1
	case "6h":
		return 6
	}
	// 兼容纯数字（小时）
	if n, err := strconv.Atoi(strings.TrimSuffix(raw, "h")); err == nil && n > 0 {
		return n
	}
	return 24
}

// parseDays 把范围参数解析成天数（按天接口用）。
//
// 与 parseRange 刻意分开：按天视图的语义单位就是天，「近 30 天」与「720 小时」
// 在补齐缺失点后并不等价（前者 30 个点、后者 720 个点），共用一个函数只会把
// 单位换算埋进调用方。
func parseDays(raw string) int {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "30d":
		return 30
	case "today", "1d":
		return 1
	case "7d":
		return 7
	case "90d":
		return 90
	case "180d":
		return 180
	case "1y", "365d":
		return 365
	}
	// 兼容纯数字（天）
	if n, err := strconv.Atoi(strings.TrimSuffix(raw, "d")); err == nil && n > 0 {
		return n
	}
	return 30
}

// handleOfficialUsage 返回**官方计费口径**的请求用量。
//
// 与 /panel/api/stats 的区别必须说清：那个是网关自己按小时桶统计的，这个是上游计费系统
// 给的原始请求行。两者对照才能发现漏记、重复计、token 解析偏差。面板上会标成「官方口径」。
//
// 多账号时按日期合并各账号的日用量（求和），并保留各账号的请求数与 credit 合计。
//
// ## 为什么响应里同时有「新形状」和「旧字段」
//
// 这个接口原先只服务积分页的两个数字（合计消耗、合计请求），字段是扁平的
// `days[].credit` / `accounts[].credit`。要 1:1 复刻 wb-switch 的积分页，
// 还需要三样原先没有的东西：
//
//  1. **按天 × 模型**（`days[].models`）—— 趋势图是按模型堆叠的柱，
//     只有按天总量画不出构成；
//  2. **账号级窗口值**（`usage_today` / `usage_7d` / `usage_this_month`）——
//     顶部指标带与账号筛选都要按账号看，只有全局合计不够；
//  3. **日期轴连续** —— 没有流量的那天必须补 0 出现，否则 X 轴会跳过空白日，
//     柱子的间距不再代表时间间隔，读图会得出错误结论。
//
// 旧字段（`days[].requests` / `credit`、`accounts[].total` / `error`、`ok_count`、
// `fail_count`）**全部保留**：`web/src/lib/usage-split.ts` 依赖它们做「反代 vs 官方直连」
// 的差分，删掉会让用量统计页静默退化。新增字段只做加法。
func (s *Server) handleOfficialUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 7
	}
	// 窗口边界只算一次：账号级与顶层日序列必须用同一个 now，
	// 否则跨零点时两处会落在不同的一天上。
	now := time.Now()

	type accountUsage struct {
		ID        string  `json:"id"`
		Nickname  string  `json:"nickname"`
		Site      string  `json:"site"`
		SiteLabel string  `json:"site_label"`
		Requests  int64   `json:"requests"`
		Credit    float64 `json:"credit"`
		Today     float64 `json:"credit_today"`
		Total     int64   `json:"total"`
		Error     string  `json:"error,omitempty"`

		// ---- 1:1 复刻 wb-switch 所需的账号级字段 ----
		OK             bool                          `json:"ok"`
		UsageToday     float64                       `json:"usage_today"`
		Usage7D        float64                       `json:"usage_7d"`
		UsageThisMonth float64                       `json:"usage_this_month"`
		RequestCount   int64                         `json:"request_count"`
		ReportedTotal  int64                         `json:"reported_total"`
		DetailLimit    int                           `json:"detail_limit"`
		DetailTrunc    bool                          `json:"detail_truncated"`
		Days           []upstream.OfficialUsageDay   `json:"days"`
		Models         []upstream.OfficialUsageModel `json:"models"`
	}

	type requestRow struct {
		AccountID   string  `json:"account_id"`
		AccountName string  `json:"account_name"`
		RequestTime string  `json:"request_time"`
		Model       string  `json:"model"`
		Credit      float64 `json:"credit"`
		Client      string  `json:"client,omitempty"`
		RequestID   string  `json:"request_id,omitempty"`
	}

	dayAgg := map[string]*upstream.OfficialUsageDay{}
	dayModelAgg := map[string]map[string]*upstream.OfficialUsageModel{}
	modelAgg := map[string]*upstream.OfficialUsageModel{}
	clientAgg := map[string]int64{}
	// 必须初始化为**非 nil 空切片**。
	//
	// Go 的 nil slice 会被 encoding/json 序列化成 `null`，而前端把这两个字段声明为
	// 数组（types.ts 的 OfficialUsageResponse.rows / .accounts）。一旦返回 null，
	// 前端任何 `data.rows.length` 式的取值都会抛
	// "Cannot read properties of null (reading 'length')"，进而在没有 ErrorBoundary
	// 的情况下卸载整棵 React 树 —— 表现为整个窗口白屏，且用户拿不到任何线索。
	// 契约上应始终是数组，空就是 `[]`。
	accounts := make([]accountUsage, 0)
	allRows := make([]requestRow, 0)
	errorsOut := make([]map[string]any, 0)
	okCount, failCount := 0, 0

	var sumToday, sum7D, sumMonth float64
	var sumRequests int64
	var sumCredit float64

	for _, a := range s.pool.Accounts() {
		if a.IsDisabled() {
			continue
		}
		view := a.View()
		if view == nil || view.AccessToken == "" {
			continue
		}
		name := a.Cred.Nickname
		if name == "" {
			name = a.Cred.AccountID()
		}
		rec := accountUsage{
			ID: a.Cred.AccountID(), Nickname: a.Cred.Nickname,
			Site: a.Site(), SiteLabel: a.SiteLabel(),
			Days: []upstream.OfficialUsageDay{}, Models: []upstream.OfficialUsageModel{},
			DetailLimit: 0,
		}
		sum, err := s.client.OfficialUsage(ctx, view, a.Profile(), days)
		if err != nil {
			// 国际站等不提供该端点的站点必然失败，如实标注而不是让整页失败
			rec.Error = err.Error()
			rec.OK = false
			failCount++
			accounts = append(accounts, rec)
			errorsOut = append(errorsOut, map[string]any{
				"account_id": rec.ID, "account_name": name, "error": err.Error(),
			})
			continue
		}
		okCount++
		rec.OK = true
		rec.Requests = sum.Requests
		rec.Credit = sum.Credit
		rec.Today = sum.Today
		rec.Total = sum.Total
		rec.UsageToday = sum.Today
		rec.Usage7D = sum.Usage7D
		rec.UsageThisMonth = sum.UsageThisMonth
		rec.RequestCount = sum.Requests
		rec.ReportedTotal = sum.Total
		rec.DetailLimit = officialDetailLimit
		rec.DetailTrunc = sum.Truncated || sum.Requests > int64(len(sum.Rows))
		// 账号级日序列同样补零：否则按账号筛选趋势图时，X 轴会跳过该账号没有
		// 流量的那几天，柱间距不再代表时间间隔（与顶层 days 是同一个理由）。
		rec.Days = zeroFillDays(sum.Days, now, days)
		rec.Models = sum.Models
		accounts = append(accounts, rec)

		sumToday += sum.Today
		sum7D += sum.Usage7D
		sumMonth += sum.UsageThisMonth
		sumRequests += sum.Requests
		sumCredit += sum.Credit

		for _, d := range sum.Days {
			agg := dayAgg[d.Date]
			if agg == nil {
				agg = &upstream.OfficialUsageDay{Date: d.Date}
				dayAgg[d.Date] = agg
			}
			agg.Requests += d.Requests
			agg.Credit += d.Credit

			dm := dayModelAgg[d.Date]
			if dm == nil {
				dm = map[string]*upstream.OfficialUsageModel{}
				dayModelAgg[d.Date] = dm
			}
			for _, m := range d.Models {
				entry := dm[m.Model]
				if entry == nil {
					entry = &upstream.OfficialUsageModel{Model: m.Model}
					dm[m.Model] = entry
				}
				entry.Requests += m.Requests
				entry.Credit += m.Credit
			}
		}
		for _, m := range sum.Models {
			agg := modelAgg[m.Model]
			if agg == nil {
				agg = &upstream.OfficialUsageModel{Model: m.Model}
				modelAgg[m.Model] = agg
			}
			agg.Requests += m.Requests
			agg.Credit += m.Credit
		}
		for _, cl := range sum.Clients {
			clientAgg[cl.Client] += cl.Requests
		}
		for _, row := range sum.Rows {
			allRows = append(allRows, requestRow{
				AccountID: rec.ID, AccountName: name,
				RequestTime: row.RequestTime, Model: row.Model,
				Credit: row.Credit, Client: row.Client, RequestID: row.RequestID,
			})
		}
	}

	// 日期轴补零：把窗口内每一天都发出去，没有流量的那天 credit=0。
	// 不补的话 X 轴会跳过空白日，柱间距不再代表时间间隔（读图会误判趋势）。
	rangeEnd := now.Format("2006-01-02")
	rangeStart := now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	merged := make([]upstream.OfficialUsageDay, 0, len(dayAgg))
	for date, d := range dayAgg {
		if dm := dayModelAgg[date]; dm != nil {
			for _, m := range dm {
				d.Models = append(d.Models, *m)
			}
			sort.Slice(d.Models, func(i, j int) bool {
				if d.Models[i].Credit != d.Models[j].Credit {
					return d.Models[i].Credit > d.Models[j].Credit
				}
				return d.Models[i].Model < d.Models[j].Model
			})
		}
		merged = append(merged, *d)
	}
	daysOut := zeroFillDays(merged, now, days)

	modelsOut := make([]upstream.OfficialUsageModel, 0, len(modelAgg))
	for _, m := range modelAgg {
		modelsOut = append(modelsOut, *m)
	}
	sort.Slice(modelsOut, func(i, j int) bool {
		if modelsOut[i].Credit != modelsOut[j].Credit {
			return modelsOut[i].Credit > modelsOut[j].Credit
		}
		return modelsOut[i].Model < modelsOut[j].Model
	})

	clientsOut := make([]map[string]any, 0, len(clientAgg))
	for name, n := range clientAgg {
		clientsOut = append(clientsOut, map[string]any{"client": name, "requests": n})
	}
	sort.Slice(clientsOut, func(i, j int) bool {
		return clientsOut[i]["requests"].(int64) > clientsOut[j]["requests"].(int64)
	})

	sort.Slice(allRows, func(i, j int) bool { return allRows[i].RequestTime > allRows[j].RequestTime })
	if len(allRows) > officialDetailTotalCap {
		allRows = allRows[:officialDetailTotalCap]
	}

	status := "unavailable"
	switch {
	case okCount > 0 && failCount == 0:
		status = "complete"
	case okCount > 0:
		status = "partial"
	}

	if s.events != nil {
		s.events.Info(eventlog.ChannelSystem, "official_usage", "读取官方口径用量",
			map[string]any{"ok": okCount, "failed": failCount, "rows": len(allRows)})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fetched_at":   now.Unix(),
		"collected_at": now.UnixMilli(),
		"status":       status,
		"range_start":  rangeStart,
		"range_end":    rangeEnd,
		"range_days":   days,

		"summary": map[string]any{
			"usage_today":      sumToday,
			"usage_7d":         sum7D,
			"usage_this_month": sumMonth,
			"requests":         sumRequests,
			"credit":           sumCredit,
		},
		"detail_limit_per_account": officialDetailLimit,

		"days":     daysOut,
		"models":   modelsOut,
		"clients":  clientsOut,
		"accounts": accounts,
		"requests": allRows,
		"errors":   errorsOut,

		// 旧字段：usage-split.ts 依赖，勿删。
		"rows":       allRows,
		"ok_count":   okCount,
		"fail_count": failCount,
	})
}

// officialDetailLimit 是**单账号**保留的请求明细条数上限，与 upstream 侧一致。
const officialDetailLimit = 200

// officialDetailTotalCap 是合并后返回给面板的明细总条数上限。
//
// 单账号各自 200 条，多账号直接相加会让响应体随账号数线性膨胀（10 个账号就是 2000 行），
// 而面板的明细表是给人看的，没有翻页也没有导出。这里兜一个总量上限，
// 超出的部分由各账号自己的 detail_truncated 标注。
const officialDetailTotalCap = 600

// zeroFillDays 把稀疏的日序列铺满 `[now-(n-1), now]` 的每一天（缺失日补 0）并按日期升序。
//
// 补零不是美化，是**正确性问题**：图表的 X 轴是分类型（日期字符串），
// 缺失的日期不会占位，于是 09/20 与 09/24 会挨在一起，柱间距不再代表时间间隔。
// 读图的人会得出「这两天之间没有间断」的错误结论。
//
// Models 在空日给空数组而不是 nil —— nil 会被序列化成 null，前端按数组处理会抛错。
func zeroFillDays(days []upstream.OfficialUsageDay, now time.Time, n int) []upstream.OfficialUsageDay {
	if n <= 0 {
		return days
	}
	byDate := make(map[string]upstream.OfficialUsageDay, len(days))
	for _, d := range days {
		byDate[d.Date] = d
	}
	out := make([]upstream.OfficialUsageDay, 0, n)
	for i := 0; i < n; i++ {
		date := now.AddDate(0, 0, -(n - 1 - i)).Format("2006-01-02")
		d, ok := byDate[date]
		if !ok {
			out = append(out, upstream.OfficialUsageDay{Date: date, Models: []upstream.OfficialUsageModel{}})
			continue
		}
		if d.Models == nil {
			d.Models = []upstream.OfficialUsageModel{}
		}
		out = append(out, d)
	}
	return out
}

// handleStatsPurge 清空统计数据。
func (s *Server) handleStatsPurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	if err := s.stats.Purge(); err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("清空统计失败: "+err.Error()))
		return
	}
	s.logf("[统计] 面板清空了用量统计")
	if s.events != nil {
		s.events.Warn(eventlog.ChannelSystem, "stats_purged", "面板清空了用量统计", nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// -----------------------------------------------------------------------------
// 请求级 trace
// -----------------------------------------------------------------------------

// traceCtxKey 是 trace ID 在请求上下文里的键。
type traceCtxKey struct{}

// withTrace 把 trace ID 挂到请求上。
//
// 用上下文而不是加参数：trace 需要被出站改写、调度重试、完成统计、日志事件
// 这么多环节拿到，一路透传参数会把签名改得很难看，而它本身是请求级元数据。
func withTrace(r *http.Request, id string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), traceCtxKey{}, id))
}

// traceOf 读请求的 trace ID（不存在时返回空串）。
func traceOf(r *http.Request) string {
	v, _ := r.Context().Value(traceCtxKey{}).(string)
	return v
}

// accountLogLabel 把账号压成脱敏标签，用于日志与**落盘归档**。
//
// 为什么必须脱敏：凭据文件名形如 `workbuddy-<完整 uid>.json`，而 reqlog 会把账号
// 写进长期留在磁盘上的归档文件。完整 uid 一旦落盘，就多了一条「翻归档即可拿到
// 账号标识」的旁路。留前 8 位足够人辨认是哪个号，不足以复原。
//
// 昵称能拿到时带上（「昵称(uid8)」）：只有 uid8 时人眼判断不出是哪个号，
// 排障还得多查一次凭据目录。
func accountLogLabel(acc *pool.Account) string {
	if acc == nil || acc.Cred == nil {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(acc.Cred.Path), filepath.Ext(acc.Cred.Path))
	base = strings.TrimPrefix(base, "workbuddy-")
	if len(base) > 8 {
		base = base[:8]
	}
	if base == "" {
		base = "-"
	}
	if nick := strings.TrimSpace(acc.Cred.Nickname); nick != "" {
		return nick + "(" + base + ")"
	}
	return base
}

// logRequest 记录一次请求完成的事件（面板日志页的主要数据来源）。
//
// 为什么值得每条请求记一条：日志页要回答的核心问题就是「刚才那次请求发生了什么」，
// 而只有把模型、账号、trace、耗时、token 放在同一条事件里，才能顺着 trace 串起
// 出站改写、拦截重试、账号治理这些下游事件。
//
// 同时把同样的统计喂给 reqlog（请求级明细 + 脱敏归档）。挂在这里而不是各返回分支
// 各写一遍：本项目**每一个**请求完成分支都会走到这里，新增分支时不会漏记 ——
// 漏记的表现是「成功率偏高」，且没人会发现。
// logRequest 记一条请求归档（面板的请求日志）。
//
// outputTokens 单独传：速率的分子必须是**输出** token，用 tokens（总量）会把
// prompt 也算进"每秒生成多少"。调用方拿不到输出量时传 0，速率字段留空。
func (s *Server) logRequest(r *http.Request, model, account, mode string, ttft, total time.Duration, tokens, outputTokens int64, failed bool) {
	if tr := traceFrom(r); tr != nil {
		tr.noteStats(model, account, mode, ttft, tokens, outputTokens, failed)
	}
	if s.events == nil {
		return
	}
	level := eventlog.LevelInfo
	msg := "请求完成"
	if failed {
		level = eventlog.LevelWarn
		msg = "请求未正常结束"
	}
	fields := map[string]any{
		"trace_id": traceOf(r),
		"model":    model,
		"account":  account,
		"mode":     mode,
		"total_ms": total.Milliseconds(),
		"tokens":   tokens,
	}
	if ttft > 0 {
		fields["ttft_ms"] = ttft.Milliseconds()
	}
	s.events.Add(level, eventlog.ChannelRequest, "request_done", msg, fields)
}
