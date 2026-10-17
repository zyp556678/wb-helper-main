package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// 本文件是「官方请求用量」查询。
//
// 端点实测（与参考实现不同，容易踩错，这里写清楚）：
//
//	POST {Web 域}/billing/meter/get-user-request-usage
//	body: {"startTime":"2026-09-17 00:00:00","endTime":"2026-09-24 23:59:59","pageNum":1,"pageSize":3000}
//	→ data: {"total":182,"data":[{requestTime, model, credit, client, requestId, input...}]}
//
// 两个坑：
//   - **是 POST 不是 GET**，且必须带时间范围；用 GET 会拿到 404/空。
//   - **没有 /v2 前缀**（带 /v2 会 404），而同期其它 billing 端点（额度汇总、签到）是带 /v2 的。
//
// 为什么值得单独做：网关自己记的用量（internal/stats）是**按我们的口径**统计的
// ——按小时桶、按我们解析到的 token。这份是**上游计费口径**的原始请求行，
// 两者对照才能发现漏记、重复计、token 解析偏差。面板上会标成「官方口径」。
//
// 分页取舍：单页上限 3000 条，且**超过时上游保留区间内最早的一批**
// （连同 `total` 一起被截断）。因此「只取首页」拿到的是窗口里最老的 3000 条，
// 最新数据永远进不来 —— 表现为「今日消耗恒为 0，而 7 天/本月有数」。
// 这里改成与 wb-switch 相同的**滑动窗口**扫描：
//
//	取满一页 → 把 startTime 推进到本轮最新一条的时间 → 再取 → 直到某轮不满一页
//
// 相邻两轮边界上必然重复，靠 (requestId, requestTime) 去重。
// 全部轮次都写进同一个 ctx 超时预算：面板每刷新一次都要跑完整轮，不能让
// 某个账号的翻页把整页拖死。

const officialUsagePath = "/billing/meter/get-user-request-usage"

// officialUsagePageSize 单页条数（上游上限 3000）。
//
// 用变量而不是常量：滑动扫描的测试要把它压小才能在一屏数据里模拟「一页装不下」。
const officialUsagePageSizeDefault = 3000

var officialUsagePageSize = officialUsagePageSizeDefault

// officialUsageMaxRounds 是滑动扫描的轮数上限。
//
// 每轮最多 3000 条，100 轮足以覆盖单账号单窗口的任何真实量级，同时兜住
// 「服务端异常返回同一页」的死循环。与 wb-switch 的 OFFICIAL_USAGE_MAX_ROUNDS 同值。
const officialUsageMaxRounds = 100

// officialUsageMaxRows 面板展示用的明细条数上限（**每个账号**分别计）。
//
// 与 wb-switch 的 detailLimitPerAccount 取同一个值：面板的「请求用量」页签要按
// 账号筛选后仍看到足够多的行，100 条在多账号场景下会立刻被第一个账号占满。
const officialUsageMaxRows = 200

// OfficialUsageRow 是一条官方请求记录。
type OfficialUsageRow struct {
	RequestTime string  `json:"request_time"`
	Model       string  `json:"model"`
	Credit      float64 `json:"credit"`
	Client      string  `json:"client,omitempty"`
	RequestID   string  `json:"request_id,omitempty"`
}

// OfficialUsageModel 是按模型汇总。
type OfficialUsageModel struct {
	Model    string  `json:"model"`
	Requests int64   `json:"requests"`
	Credit   float64 `json:"credit"`
}

// OfficialUsageDay 是按天汇总。
//
// Models 是**同一天内再按模型拆一层**，为的是让趋势图能画「按模型堆叠」的柱。
// 只有按天汇总的总量是不够的：那样只能画一根单色柱，看不出每天由哪些模型构成，
// 而这正是面板最想问的问题（「今天这几百积分是谁花的」）。
type OfficialUsageDay struct {
	Date     string               `json:"date"`
	Requests int64                `json:"requests"`
	Credit   float64              `json:"credit"`
	Models   []OfficialUsageModel `json:"models,omitempty"`
}

// OfficialUsageClient 是按客户端来源汇总（可用于识别流量来自哪个客户端）。
type OfficialUsageClient struct {
	Client   string `json:"client"`
	Requests int64  `json:"requests"`
}

// OfficialUsageSummary 是单账号的官方用量。
type OfficialUsageSummary struct {
	FetchedAt int64 `json:"fetched_at"`
	// Total 是上游自报的真实条数；Fetched 是本次实际取到的条数。两者不等即被分页截断。
	Total     int64 `json:"total"`
	Fetched   int64 `json:"fetched"`
	Truncated bool  `json:"truncated"`

	Requests int64   `json:"requests"`
	Credit   float64 `json:"credit"`
	// Today / Usage7D / UsageThisMonth 三个窗口是面板顶部指标带的口径。
	// 为什么在拉取时就算好而不是让前端从 Days 里筛：窗口边界按**本地日历日**
	// 定义，前端各算一遍容易出现「今天」被算成「最近 24 小时」这类偏差。
	Today          float64 `json:"credit_today"`
	Usage7D        float64 `json:"usage_7d"`
	UsageThisMonth float64 `json:"usage_this_month"`

	Days    []OfficialUsageDay    `json:"days"`
	Models  []OfficialUsageModel  `json:"models"`
	Clients []OfficialUsageClient `json:"clients"`
	Rows    []OfficialUsageRow    `json:"rows"`
}

// OfficialUsage 拉取官方用量（默认最近 days 天）。
func (c *Client) OfficialUsage(ctx context.Context, cred *CredentialView, p *Profile, days int) (*OfficialUsageSummary, error) {
	if days <= 0 {
		days = 7
	}
	now := time.Now()
	rangeStart := now.AddDate(0, 0, -(days-1)).Format("2006-01-02") + " 00:00:00"
	endTime := now.Format("2006-01-02") + " 23:59:59"

	rows, reportedTotal, truncated, err := c.sweepOfficialUsage(ctx, cred, p, rangeStart, endTime)
	if err != nil {
		return nil, err
	}

	sum := &OfficialUsageSummary{
		FetchedAt: now.Unix(),
		Total:     reportedTotal,
		Fetched:   int64(len(rows)),
		Days:      []OfficialUsageDay{},
		Models:    []OfficialUsageModel{},
		Clients:   []OfficialUsageClient{},
		Rows:      []OfficialUsageRow{},
		Truncated: truncated,
	}

	dayAgg := map[string]*OfficialUsageDay{}
	// dayModelAgg 是「天 × 模型」的交叉聚合，用来给每天挂上模型构成。
	dayModelAgg := map[string]map[string]*OfficialUsageModel{}
	modelAgg := map[string]*OfficialUsageModel{}
	clientAgg := map[string]*OfficialUsageClient{}
	today := now.Format("2006-01-02")
	// 7 天窗口按本地日历日算：含今天在内的 7 个自然日。
	weekStart := now.AddDate(0, 0, -6).Format("2006-01-02")
	monthStart := now.Format("2006-01") + "-01"

	for _, row := range rows {
		// requestTime 形如 "2026-09-24 19:47:00"，按空格切出日期部分。
		date := row.RequestTime
		if i := strings.IndexByte(date, ' '); i > 0 {
			date = date[:i]
		}

		sum.Requests++
		sum.Credit += row.Credit
		if date == today {
			sum.Today += row.Credit
		}
		if date >= weekStart && date <= today {
			sum.Usage7D += row.Credit
		}
		if date >= monthStart && date <= today {
			sum.UsageThisMonth += row.Credit
		}

		d := dayAgg[date]
		if d == nil {
			d = &OfficialUsageDay{Date: date}
			dayAgg[date] = d
		}
		d.Requests++
		d.Credit += row.Credit

		if row.Model != "" {
			m := modelAgg[row.Model]
			if m == nil {
				m = &OfficialUsageModel{Model: row.Model}
				modelAgg[row.Model] = m
			}
			m.Requests++
			m.Credit += row.Credit

			dm := dayModelAgg[date]
			if dm == nil {
				dm = map[string]*OfficialUsageModel{}
				dayModelAgg[date] = dm
			}
			entry := dm[row.Model]
			if entry == nil {
				entry = &OfficialUsageModel{Model: row.Model}
				dm[row.Model] = entry
			}
			entry.Requests++
			entry.Credit += row.Credit
		}
		if row.Client != "" {
			cl := clientAgg[row.Client]
			if cl == nil {
				cl = &OfficialUsageClient{Client: row.Client}
				clientAgg[row.Client] = cl
			}
			cl.Requests++
		}
		if len(sum.Rows) < officialUsageMaxRows {
			sum.Rows = append(sum.Rows, OfficialUsageRow{
				RequestTime: row.RequestTime, Model: row.Model,
				Credit: row.Credit, Client: row.Client, RequestID: row.RequestID,
			})
		}
	}

	for date, dm := range dayModelAgg {
		d := dayAgg[date]
		if d == nil {
			continue
		}
		for _, m := range dm {
			d.Models = append(d.Models, *m)
		}
		sortModelsByCredit(d.Models)
	}

	for _, d := range dayAgg {
		sum.Days = append(sum.Days, *d)
	}
	sort.Slice(sum.Days, func(i, j int) bool { return sum.Days[i].Date < sum.Days[j].Date })
	for _, m := range modelAgg {
		sum.Models = append(sum.Models, *m)
	}
	sortModelsByCredit(sum.Models)
	for _, cl := range clientAgg {
		sum.Clients = append(sum.Clients, *cl)
	}
	sort.Slice(sum.Clients, func(i, j int) bool { return sum.Clients[i].Requests > sum.Clients[j].Requests })
	// 明细按时间倒序（新的在前），面板只看最近的
	sort.Slice(sum.Rows, func(i, j int) bool { return sum.Rows[i].RequestTime > sum.Rows[j].RequestTime })

	return sum, nil
}

// officialUsageRow 是上游返回的一条请求记录（内部形态，JSON 键是上游的驼峰）。
type officialUsageRow struct {
	RequestTime string  `json:"requestTime"`
	Model       string  `json:"model"`
	Credit      float64 `json:"credit"`
	Client      string  `json:"client"`
	RequestID   string  `json:"requestId"`
}

// sweepOfficialUsage 用滑动窗口把窗口内的全部记录扫回来。
//
// 上游限制：单次最多返回 officialUsagePageSize 条，**超出时保留区间内最早的一批**，
// 并且 `total` 也被截断成同一数字。所以「取一页就停」拿到的是最老的 3000 条 ——
// 近期数据反而取不到（「今日消耗恒为 0」就是这么来的）。
//
// 修法与 wb-switch 一致：取满一页就把 startTime 推进到本轮**最新一条**的时间，
// 再取下一页。相邻两轮在边界时间上必然重复，按 (requestId, requestTime) 去重。
// 某轮不满一页即说明取完；若本轮最新时间没有前进（服务端异常返回同一页），
// 也立即停止，避免死循环。
//
// 返回的 truncated 表示是否因轮数上限而提前结束 —— 那种情况下的合计不完整，
// 上层要如实标注，而不是当成精确值。
func (c *Client) sweepOfficialUsage(ctx context.Context, cred *CredentialView, p *Profile,
	rangeStart, endTime string) ([]officialUsageRow, int64, bool, error) {

	var (
		out        []officialUsageRow
		seen       = make(map[string]bool)
		startTime  = rangeStart
		reported   int64
		prevNewest string
	)

	for round := 0; round < officialUsageMaxRounds; round++ {
		page, total, err := c.fetchOfficialUsagePage(ctx, cred, p, startTime, endTime)
		if err != nil {
			return nil, 0, false, err
		}
		// total 是上游自己报的条数；取各轮最大值（每轮都可能被截断成 3000）。
		if total > reported {
			reported = total
		}
		if len(page) < officialUsagePageSize {
			appendUniqueRows(&out, seen, page, rangeStart, endTime)
			return out, reported, false, nil
		}
		appendUniqueRows(&out, seen, page, rangeStart, endTime)

		newest := newestRequestTime(page)
		if newest == "" || newest <= prevNewest {
			// 起点推不动：要么服务端在异常返同一页，要么时间格式不认识。
			// 保留已取到的部分并标注不完整，比无限重试或直接报错更有用。
			return out, reported, true, nil
		}
		prevNewest = newest
		// 起点用**边界那条本身**（不是 +1 秒）：同秒可能有多条，跳过会漏。
		// 重复由 seen 去掉。
		startTime = newest
	}
	return out, reported, true, nil
}

// appendUniqueRows 把一轮结果按 (requestId, requestTime) 去重后并入 out。
//
// 同时按窗口两端过滤：滑动窗口的终点是先取最新一批，理论上都在窗口内，
// 但上游对边界时间戳的解释不完全一致，超出的一律丢弃更稳妥。
func appendUniqueRows(out *[]officialUsageRow, seen map[string]bool, rows []officialUsageRow,
	rangeStart, endTime string) {
	startDate := rangeStart
	if i := strings.IndexByte(startDate, ' '); i > 0 {
		startDate = startDate[:i]
	}
	endDate := endTime
	if i := strings.IndexByte(endDate, ' '); i > 0 {
		endDate = endDate[:i]
	}
	for _, row := range rows {
		date := row.RequestTime
		if i := strings.IndexByte(date, ' '); i > 0 {
			date = date[:i]
		}
		if date < startDate || date > endDate {
			continue
		}
		key := row.RequestID + "\x00" + row.RequestTime
		if seen[key] {
			continue
		}
		seen[key] = true
		*out = append(*out, row)
	}
}

// newestRequestTime 返回一轮里最晚的 requestTime（字符串可直接比较，
// 上游格式固定为 "2006-01-02 15:04:05"）。空的 time 字段会被忽略。
func newestRequestTime(rows []officialUsageRow) string {
	newest := ""
	for _, row := range rows {
		if row.RequestTime != "" && row.RequestTime > newest {
			newest = row.RequestTime
		}
	}
	return newest
}

// fetchOfficialUsagePage 取一页官方用量，返回解析后的行与上游自报的 total。
func (c *Client) fetchOfficialUsagePage(ctx context.Context, cred *CredentialView, p *Profile,
	startTime, endTime string) ([]officialUsageRow, int64, error) {

	reqBody := map[string]any{
		"startTime": startTime,
		"endTime":   endTime,
		"pageNum":   1,
		"pageSize":  officialUsagePageSize,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Origin+officialUsagePath, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, err
	}
	webHeaders(cred, p, "web")(req)

	resp, err := c.Control.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))

	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, 0, fmt.Errorf("解析官方用量响应失败 (HTTP %d): %s", resp.StatusCode, truncate(string(body), 200))
	}
	if env.Code != 0 {
		return nil, 0, fmt.Errorf("官方用量业务错误 code=%d msg=%s", env.Code, env.Msg)
	}

	// data 是一层分页对象：{total, data:[rows], ...}
	var page struct {
		Total int64             `json:"total"`
		Rows  []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(env.Data, &page); err != nil {
		return nil, 0, fmt.Errorf("解析官方用量分页失败: %w", err)
	}

	rows := make([]officialUsageRow, 0, len(page.Rows))
	for _, rawRow := range page.Rows {
		var row officialUsageRow
		if err := json.Unmarshal(rawRow, &row); err != nil {
			continue
		}
		rows = append(rows, row)
	}
	return rows, page.Total, nil
}

// sortModelsByCredit 按消耗降序排列模型（同额时按请求数、再按名字，保证顺序稳定）。
//
// 面板的「按模型分类」问的是「谁花的积分多」，所以排序键必须是 credit 而不是
// 请求数：调用少但单价高的模型才是真正的大头，按请求数排会把它压到末尾。
func sortModelsByCredit(list []OfficialUsageModel) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Credit != list[j].Credit {
			return list[i].Credit > list[j].Credit
		}
		if list[i].Requests != list[j].Requests {
			return list[i].Requests > list[j].Requests
		}
		return list[i].Model < list[j].Model
	})
}
