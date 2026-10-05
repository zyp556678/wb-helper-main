// Package creditwatch 维护「本地观察口径」的积分台账（对齐 wb-switch 的
// credit_usage.rs，v0.1.58）。
//
// ## 为什么需要它
//
// 官方用量接口会挂（上游故障、鉴权过期、限流）。挂掉时如果整页数值全变「—」，
// 用户读到的信息是「什么都没发生」——但积分确实在消耗，而且**本地本来就看得见**：
// 每次成功拉取积分资源包，我们都能记下「此刻余额是多少」。
//
// 余额在两次快照之间的**下降量**就是这段时间的消耗（只统计正差值；
// 余额不可能因为「用」而上升，上升来自签到/发放，属于另一类事件）。
// 这个口径与 switch 完全一致：**只统计连续快照中余额下降的正差值**。
//
// ## 存储
//
// 单文件 JSON（工作目录下的 `wb-credit-history.json`），原子替换写入。
// 快照保留 `RetentionDays` 天、签到保留 `CheckinKeepDays` 天，
// 并各自设记录上限 —— 无上限的话每次刷新都追加一条，一年能把文件撑到几十 MB。
//
// 同一账号、同一资源值在 `dedupeWindow` 内只保留一条；值一变化立刻保留，
// 这样余额下降能准确归因到发生变化的那个快照。
package creditwatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// RetentionDays 快照保留天数（与 switch 的 CREDIT_SNAPSHOT_RETENTION_DAYS 一致）。
	RetentionDays = 90
	// CheckinKeepDays 签到记录保留天数（switch 的 CHECKIN_LOG_KEEP_DAYS）。
	CheckinKeepDays = 30
	// CheckinLogCap 是签到记录条数上限（面板「签到日志」要显示这条说明）。
	CheckinLogCap = 2000
	// maxSnapshots 快照记录上限（switch 的 CREDIT_SNAPSHOT_MAX_RECORDS 与同量级）。
	maxSnapshots = 5000
	// maxEvents 统计里返回的最近事件条数（switch 的 CREDIT_STATS_MAX_EVENTS）。
	maxEvents = 200
	// dedupeWindow 同值去重窗口（switch 的 CREDIT_SNAPSHOT_DEDUPE_WINDOW_MS）。
	dedupeWindow = 5 * time.Minute
)

type snapshot struct {
	TS          int64   `json:"ts"`
	AccountID   string  `json:"account_id"`
	AccountName string  `json:"account_name"`
	Site        string  `json:"site"`
	Total       float64 `json:"total"`
	Remaining   float64 `json:"remaining"`
}

type checkin struct {
	TS          int64  `json:"ts"`
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	Site        string `json:"site"`
	Result      string `json:"result"`
	Error       string `json:"error,omitempty"`
}

type history struct {
	Version   int        `json:"version"`
	Snapshots []snapshot `json:"snapshots"`
	Checkins  []checkin  `json:"checkins"`
}

// Store 是台账的读写入口。零值不可用；用 New 构造。
// 所有方法对 nil 接收者安全（未接线时静默跳过，调用方不必判空）。
type Store struct {
	mu     sync.Mutex
	path   string
	data   history
	loaded bool
}

// New 创建台账。path 通常是 `<工作目录>/wb-credit-history.json`。
func New(path string) *Store { return &Store{path: path} }

func (s *Store) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.data = history{Version: 1}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return // 文件不存在是正常路径
	}
	var parsed history
	if json.Unmarshal(raw, &parsed) != nil {
		return // 损坏时按空处理，不阻塞页面
	}
	s.data = parsed
	if s.data.Version == 0 {
		s.data.Version = 1
	}
}

// saveLocked 裁剪后原子写盘。写失败只丢这一条记录，不影响调用方。
func (s *Store) saveLocked(now time.Time) {
	cutoff := now.Add(-RetentionDays * 24 * time.Hour).UnixMilli()
	checkinCutoff := now.Add(-CheckinKeepDays * 24 * time.Hour).UnixMilli()

	keptSnapshots := s.data.Snapshots[:0]
	for _, item := range s.data.Snapshots {
		if item.TS >= cutoff && item.TS <= now.UnixMilli() {
			keptSnapshots = append(keptSnapshots, item)
		}
	}
	if extra := len(keptSnapshots) - maxSnapshots; extra > 0 {
		keptSnapshots = keptSnapshots[extra:]
	}
	s.data.Snapshots = append([]snapshot(nil), keptSnapshots...)

	keptCheckins := s.data.Checkins[:0]
	for _, item := range s.data.Checkins {
		if item.TS >= checkinCutoff && item.TS <= now.UnixMilli() {
			keptCheckins = append(keptCheckins, item)
		}
	}
	if extra := len(keptCheckins) - CheckinLogCap; extra > 0 {
		keptCheckins = keptCheckins[extra:]
	}
	s.data.Checkins = append([]checkin(nil), keptCheckins...)

	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

func (s *Store) latestSnapshotLocked(accountID string) (snapshot, bool) {
	var latest snapshot
	found := false
	for _, item := range s.data.Snapshots {
		if item.AccountID != accountID {
			continue
		}
		if !found || item.TS >= latest.TS {
			latest, found = item, true
		}
	}
	return latest, found
}

// RecordSnapshot 记录一次成功的资源观察值。
//
// 返回是否实际写入：同账号同值且在去重窗口内时返回 false。
func (s *Store) RecordSnapshot(accountID, accountName, site string, total, remaining float64) bool {
	return s.recordSnapshotAt(accountID, accountName, site, total, remaining, time.Now())
}

// recordSnapshotAt 是 RecordSnapshot 的可注入时间版本（测试用）。
func (s *Store) recordSnapshotAt(accountID, accountName, site string, total, remaining float64, now time.Time) bool {
	if s == nil {
		return false
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()

	if latest, ok := s.latestSnapshotLocked(accountID); ok {
		if now.UnixMilli()-latest.TS <= dedupeWindow.Milliseconds() &&
			latest.Total == total && latest.Remaining == remaining {
			return false
		}
	}

	if total < 0 {
		total = 0
	}
	if remaining < 0 {
		remaining = 0
	}
	s.data.Snapshots = append(s.data.Snapshots, snapshot{
		TS:          now.UnixMilli(),
		AccountID:   accountID,
		AccountName: strings.TrimSpace(accountName),
		Site:        site,
		Total:       total,
		Remaining:   remaining,
	})
	s.saveLocked(now)
	return true
}

// RecordCheckin 记录一次签到结果。
//
// result 取 switch 的三个值：`success` / `already` / `error`。
// 国际站账号不参与签到，不应调用（调用方负责跳过）。
func (s *Store) RecordCheckin(accountID, accountName, site, result, errMsg string) {
	s.recordCheckinAt(accountID, accountName, site, result, errMsg, time.Now())
}

// recordCheckinAt 是 RecordCheckin 的可注入时间版本（测试用）。
func (s *Store) recordCheckinAt(accountID, accountName, site, result, errMsg string, now time.Time) {
	if s == nil {
		return
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" && strings.TrimSpace(accountName) == "" {
		return
	}
	if result == "" {
		result = "error"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	s.data.Checkins = append(s.data.Checkins, checkin{
		TS:          now.UnixMilli(),
		AccountID:   accountID,
		AccountName: strings.TrimSpace(accountName),
		Site:        site,
		Result:      result,
		Error:       errMsg,
	})
	s.saveLocked(now)
}

// CheckinLog 是面板「签到日志」的一行（GET /panel/api/checkin/logs）。
//
// 字段与 wb-switch 的签到日志对齐：ts 为 Unix 毫秒，date 是**本地日历日**
// （跨午夜时用来归日），result 取 success / already / error。
type CheckinLog struct {
	TS          int64  `json:"ts"`
	Date        string `json:"date"`
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	Site        string `json:"site"`
	Result      string `json:"result"`
	Error       string `json:"error,omitempty"`
}

// CheckinLogs 返回最近 days 天内的签到记录，**倒序**（最新在前）。
//
// days <= 0 取默认 CheckinKeepDays；大于 CheckinKeepDays 时截到上限 ——
// 更早的记录在写入时已被裁剪，没有可返回的数据，如实按上限处理而不是假装有。
// 纯本地读取，不打上游。
func (s *Store) CheckinLogs(days int, now time.Time) []CheckinLog {
	out := []CheckinLog{}
	if s == nil {
		return out
	}
	if days <= 0 {
		days = CheckinKeepDays
	}
	if days > CheckinKeepDays {
		days = CheckinKeepDays
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	nowMS := now.UnixMilli()

	s.mu.Lock()
	s.loadLocked()
	items := append([]checkin(nil), s.data.Checkins...)
	s.mu.Unlock()

	for _, item := range items {
		if item.TS < cutoff || item.TS > nowMS {
			continue
		}
		out = append(out, CheckinLog{
			TS:          item.TS,
			Date:        localDate(item.TS),
			AccountID:   item.AccountID,
			AccountName: item.AccountName,
			Site:        item.Site,
			Result:      item.Result,
			Error:       item.Error,
		})
	}
	// 倒序：同毫秒时保持原始写入顺序（稳定排序），结果可复现。
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out
}

// -----------------------------------------------------------------------------
// 统计投影
// -----------------------------------------------------------------------------

// AccountInfo 是统计时用来对齐「当前账号集合」的最小信息。
type AccountInfo struct {
	ID   string
	Name string
	Site string
}

// Summary 本地观察口径的汇总。
type Summary struct {
	CurrentRemaining float64 `json:"current_remaining"`
	CurrentCapacity  float64 `json:"current_capacity"`
	UsageToday       float64 `json:"usage_today"`
	Usage7Days       float64 `json:"usage_7d"`
	UsageThisMonth   float64 `json:"usage_this_month"`
	// 今天有签到结果的账号数（success / already）。
	TodayCheckedInAccounts int `json:"today_checked_in_accounts"`
	TodaySuccess           int `json:"today_success"`
	TodayAlready           int `json:"today_already"`
	TodayFailed            int `json:"today_failed"`
}

// DailyPoint 是逐日观察消耗（无数据的天补 0）。
type DailyPoint struct {
	Date  string  `json:"date"`
	Usage float64 `json:"usage"`
}

// AccountStat 是单账号的观察结果。
type AccountStat struct {
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	Site        string `json:"site"`
	IsCurrent   bool   `json:"is_current"`
	// 仅 is_current 的账号给余额（与 switch 的「只统计当前账号」同口径）。
	CurrentRemaining *float64 `json:"current_remaining"`
	TotalCapacity    *float64 `json:"total_capacity"`
	LastSnapshotAt   *int64   `json:"last_snapshot_at"`
	UsageToday       float64  `json:"usage_today"`
	Usage7Days       float64  `json:"usage_7d"`
	UsageThisMonth   float64  `json:"usage_this_month"`
	CheckedInToday   *bool    `json:"checked_in_today"`
	// 今日签到结果：success / already / error。
	CheckinStatusToday *string `json:"checkin_status_today"`
	LastCheckinAt      *int64  `json:"last_checkin_at"`
	LastCheckinResult  *string `json:"last_checkin_result"`
	// 按账号的逐日观察消耗（缺省兼容旧后端；官方可用时趋势图优先用官方 daily）。
	Daily []DailyPoint `json:"daily"`
}

// Event 是使用事件或签到事件（kind 区分）。
type Event struct {
	Kind        string  `json:"kind"` // usage | checkin
	TS          int64   `json:"ts"`
	Date        string  `json:"date"`
	AccountID   string  `json:"account_id"`
	AccountName string  `json:"account_name"`
	Site        string  `json:"site"`
	Amount      float64 `json:"amount,omitempty"`
	Result      string  `json:"result,omitempty"`
	Error       string  `json:"error,omitempty"`
}

// Statistics 是给面板的完整投影（对齐 switch 的 get_credit_statistics）。
type Statistics struct {
	GeneratedAt     int64         `json:"generated_at"`
	RetentionDays   int           `json:"retention_days"`
	CoverageStartAt *int64        `json:"coverage_start_at"`
	Summary         Summary       `json:"summary"`
	Daily           []DailyPoint  `json:"daily"`
	Accounts        []AccountStat `json:"accounts"`
	Events          []Event       `json:"events"`
}

func localDate(ms int64) string {
	return time.UnixMilli(ms).Local().Format("2006-01-02")
}

func localDayStart(ms int64) time.Time {
	t := time.UnixMilli(ms).Local()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

// calendarDays 返回 b - a 的日历日差（两者都取本地零点）。
func calendarDays(a, b time.Time) int {
	return int((b.Sub(a).Hours() + 12) / 24)
}

// windows 判断某日期是否落在今日 / 近 7 天 / 本月（与 switch 的
// usage_in_windows 同口径：0<=distance<7）。
func windows(date string, today time.Time) (bool, bool, bool) {
	d, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return false, false, false
	}
	distance := calendarDays(d, today)
	return distance == 0,
		distance >= 0 && distance < 7,
		d.Year() == today.Year() && d.Month() == today.Month()
}

// Statistics 计算本地观察统计。accounts 是当前账号集合（未禁用的池内账号）。
func (s *Store) Statistics(accounts []AccountInfo, now time.Time) Statistics {
	result := Statistics{
		GeneratedAt:   now.UnixMilli(),
		RetentionDays: RetentionDays,
		Daily:         []DailyPoint{},
		Accounts:      []AccountStat{},
		Events:        []Event{},
	}
	if s == nil {
		return result
	}

	s.mu.Lock()
	s.loadLocked()
	allSnapshots := append([]snapshot(nil), s.data.Snapshots...)
	allCheckins := append([]checkin(nil), s.data.Checkins...)
	s.mu.Unlock()

	snapshotCutoff := now.Add(-RetentionDays * 24 * time.Hour).UnixMilli()
	checkinCutoff := now.Add(-CheckinKeepDays * 24 * time.Hour).UnixMilli()
	nowMS := now.UnixMilli()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)

	snapshots := make([]snapshot, 0, len(allSnapshots))
	for _, item := range allSnapshots {
		if item.TS >= snapshotCutoff && item.TS <= nowMS {
			snapshots = append(snapshots, item)
		}
	}
	checkins := make([]checkin, 0, len(allCheckins))
	for _, item := range allCheckins {
		if item.TS >= checkinCutoff && item.TS <= nowMS {
			checkins = append(checkins, item)
		}
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].TS < snapshots[j].TS })

	// 账号集合与显示名：账号列表优先，其次快照，再次签到。
	order := make([]string, 0, len(accounts))
	names := map[string]string{}
	sites := map[string]string{}
	current := map[string]bool{}
	addAccount := func(id, name, site string) {
		if id == "" {
			return
		}
		if _, ok := names[id]; !ok {
			order = append(order, id)
		}
		if name != "" {
			if _, ok := names[id]; !ok || names[id] == "" {
				names[id] = name
			}
		}
		if site != "" {
			if _, ok := sites[id]; !ok || sites[id] == "" {
				sites[id] = site
			}
		}
	}
	for _, item := range accounts {
		addAccount(item.ID, item.Name, item.Site)
		current[item.ID] = true
	}
	for _, item := range snapshots {
		addAccount(item.AccountID, item.AccountName, item.Site)
	}
	for _, item := range checkins {
		addAccount(item.AccountID, item.AccountName, item.Site)
	}
	for id := range names {
		if names[id] == "" {
			names[id] = id
		}
	}

	// 逐账号的余额下降区间 → 消耗。
	byAccount := map[string][]snapshot{}
	for _, item := range snapshots {
		byAccount[item.AccountID] = append(byAccount[item.AccountID], item)
	}

	var coverageStart *int64
	for _, item := range snapshots {
		if coverageStart == nil || item.TS < *coverageStart {
			ts := item.TS
			coverageStart = &ts
		}
	}

	dailyByAccount := map[string]map[string]float64{}
	usageTotals := map[string][3]float64{} // today / 7d / month
	events := make([]Event, 0, len(snapshots)+len(checkins))
	latestByAccount := map[string]snapshot{}

	for accountID, list := range byAccount {
		latestByAccount[accountID] = list[len(list)-1]
		for i := 1; i < len(list); i++ {
			previous, cur := list[i-1], list[i]
			amount := previous.Remaining - cur.Remaining
			if amount <= 0 {
				continue
			}
			date := localDate(cur.TS)
			totals := usageTotals[accountID]
			isToday, in7d, inMonth := windows(date, today)
			if isToday {
				totals[0] += amount
			}
			if in7d {
				totals[1] += amount
			}
			if inMonth {
				totals[2] += amount
			}
			usageTotals[accountID] = totals
			if dailyByAccount[accountID] == nil {
				dailyByAccount[accountID] = map[string]float64{}
			}
			dailyByAccount[accountID][date] += amount
			events = append(events, Event{
				Kind: "usage", TS: cur.TS, Date: date,
				AccountID: accountID, AccountName: names[accountID],
				Site: cur.Site, Amount: amount,
			})
		}
	}

	// 签到：今日计数 + 每账号最近一次。
	type checkinInfo struct {
		todayResult string
		todayTS     int64
		lastTS      int64
		lastResult  string
	}
	checkinByAccount := map[string]*checkinInfo{}
	todayKey := today.Format("2006-01-02")
	var todaySuccess, todayAlready, todayFailed int
	// 「今日已签到账号」按**每个账号今天最近一次结果**判定（与 switch 一致）：
	// 先成功后失败（重试失败）不算已签到 —— 最新一次才代表当前状态。
	type latestToday struct {
		ts     int64
		result string
	}
	todayLatestByIdentity := map[string]latestToday{}
	for _, item := range checkins {
		date := localDate(item.TS)
		if date == todayKey {
			switch item.Result {
			case "success":
				todaySuccess++
			case "already":
				todayAlready++
			default:
				todayFailed++
			}
			identity := item.AccountID
			if identity == "" {
				identity = "legacy:" + item.AccountName
			}
			if previous, ok := todayLatestByIdentity[identity]; !ok || item.TS >= previous.ts {
				todayLatestByIdentity[identity] = latestToday{ts: item.TS, result: item.Result}
			}
		}
		if item.AccountID == "" {
			continue
		}
		info := checkinByAccount[item.AccountID]
		if info == nil {
			info = &checkinInfo{}
			checkinByAccount[item.AccountID] = info
		}
		if date == todayKey && item.TS >= info.todayTS {
			info.todayTS, info.todayResult = item.TS, item.Result
		}
		if item.TS >= info.lastTS {
			info.lastTS, info.lastResult = item.TS, item.Result
		}
		events = append(events, Event{
			Kind: "checkin", TS: item.TS, Date: date,
			AccountID: item.AccountID, AccountName: names[item.AccountID],
			Site: item.Site, Result: item.Result, Error: item.Error,
		})
	}

	// 逐日序列：起点取「最早快照日」与「保留窗口下界」的较大者；无快照则空序列。
	var dailyStart *time.Time
	if coverageStart != nil {
		start := localDayStart(*coverageStart)
		earliest := today.AddDate(0, 0, -(RetentionDays - 1))
		if start.Before(earliest) {
			start = earliest
		}
		dailyStart = &start
	}
	fillDaily := func(usage map[string]float64) []DailyPoint {
		points := []DailyPoint{}
		if dailyStart == nil {
			return points
		}
		for d := *dailyStart; !d.After(today); d = d.AddDate(0, 0, 1) {
			key := d.Format("2006-01-02")
			points = append(points, DailyPoint{Date: key, Usage: usage[key]})
		}
		return points
	}

	aggregateDaily := map[string]float64{}
	for _, usage := range dailyByAccount {
		for date, amount := range usage {
			aggregateDaily[date] += amount
		}
	}
	result.Daily = fillDaily(aggregateDaily)
	result.CoverageStartAt = coverageStart

	// 汇总：余额与容量只统计当前账号（与 switch 同口径）。
	for _, id := range order {
		if !current[id] {
			continue
		}
		if latest, ok := latestByAccount[id]; ok {
			result.Summary.CurrentRemaining += latest.Remaining
			result.Summary.CurrentCapacity += latest.Total
		}
	}
	for _, totals := range usageTotals {
		result.Summary.UsageToday += totals[0]
		result.Summary.Usage7Days += totals[1]
		result.Summary.UsageThisMonth += totals[2]
	}
	todayCheckedIn := 0
	for _, latest := range todayLatestByIdentity {
		if latest.result == "success" || latest.result == "already" {
			todayCheckedIn++
		}
	}
	result.Summary.TodayCheckedInAccounts = todayCheckedIn
	result.Summary.TodaySuccess = todaySuccess
	result.Summary.TodayAlready = todayAlready
	result.Summary.TodayFailed = todayFailed

	for _, id := range order {
		stat := AccountStat{
			AccountID:   id,
			AccountName: names[id],
			Site:        sites[id],
			IsCurrent:   current[id],
			Daily:       fillDaily(dailyByAccount[id]),
		}
		if totals, ok := usageTotals[id]; ok {
			stat.UsageToday, stat.Usage7Days, stat.UsageThisMonth = totals[0], totals[1], totals[2]
		}
		if latest, ok := latestByAccount[id]; ok {
			ts := latest.TS
			stat.LastSnapshotAt = &ts
			if stat.IsCurrent {
				remaining, total := latest.Remaining, latest.Total
				stat.CurrentRemaining = &remaining
				stat.TotalCapacity = &total
			}
		}
		if info, ok := checkinByAccount[id]; ok {
			if info.todayTS > 0 {
				checked := info.todayResult == "success" || info.todayResult == "already"
				status := info.todayResult
				stat.CheckedInToday = &checked
				stat.CheckinStatusToday = &status
			}
			if info.lastTS > 0 {
				ts, res := info.lastTS, info.lastResult
				stat.LastCheckinAt = &ts
				stat.LastCheckinResult = &res
			}
		}
		result.Accounts = append(result.Accounts, stat)
	}

	// 事件按时间倒序，截断到 200 条。
	sort.Slice(events, func(i, j int) bool { return events[i].TS > events[j].TS })
	if len(events) > maxEvents {
		events = events[:maxEvents]
	}
	result.Events = events
	return result
}
