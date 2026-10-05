package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 本文件是成长域（growth）接口：任务列表 / 报名 / 领奖 / 对话活跃上报，
// 以及连登、猫咪旅行、抽奖三块活动。
//
// 域名分两类，容易搞混，这里统一说清：
//   - CLI 域（Profile.Base，国内站是 copilot.tencent.com）：任务列表与报名走这里，
//     返回的是 PC 口径的完整任务集（实测 19 个），带 progress / accept_status / 奖励。
//   - Web 域（Profile.Origin，国内站是 www.codebuddy.cn）：领奖、连登、旅行、抽奖、
//     活跃上报走这里。领奖端点历史上是从 Web 成长中心实测出来的，CLI 域的
//     /v2/.../tasks/reward/claim 那条路径**不存在**。
//
// 小程序限定任务（Sequential_Tasks_* / school_season）只在叠加
// X-Client-Platform: miniprogram 时下发，且 accept/claim 也必须带该头，
// 缺头时上游返回 task not found。实测 mp 列表是默认口径的超集（PC 的常规任务也在里面），
// 所以合并两份列表时按 task_code 去重，mp 侧独有的才是真正的小程序限定任务。

const (
	growthTasksPath  = "/v2/activity/growth/tasks"
	growthAcceptPath = "/v2/activity/growth/tasks/accept"

	travelStatusPath = "/activity/growth/buddy/travel/status"
	travelDepartPath = "/activity/growth/buddy/travel/depart"
	travelClaimPath  = "/activity/growth/buddy/travel/claim"

	// 领养链路（切片 18）：report → agreement → first。
	// 三者的顺序是硬要求 —— 未上报时 buddy/first 会返回 HTTP 400
	// 「first_buddy task not completed yet」，所以 report 必须在前。
	buddyInfoPath      = "/activity/growth/buddy/info"
	buddyFirstPath     = "/activity/growth/buddy/first"
	buddyAgreementPath = "/activity/growth/buddy/agreement"

	streakPath            = "/activity/growth/streak"
	heatmapPath           = "/activity/growth/heatmap"
	makeupCardPath        = "/activity/growth/makeup-cards/use"
	claimGiftPath         = "/billing/meter/claim-gift"
	claimCompensationPath = "/billing/meter/claim-compensation"
	streakRedeemPath      = "/activity/growth/redeem"
	lotterySummaryPath    = "/activity/growth/lottery/summary"
	lotteryDrawPath       = "/activity/growth/lottery/draw"

	reportPath     = "/v2/report"
	schoolTaskPath = "/portal/activity/school/tasks"
	// schoolVoucherPath 是开学季券码列表（只读，单次拉全）。
	schoolVoucherPath = "/portal/activity/school/vouchers"
)

// mpPlatform 是小程序口径头值。
const mpPlatform = "miniprogram"

// GrowthTask 是 growth 域任务（字段与上游 JSON 对齐，多余字段不透出）。
type GrowthTask struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	TaskDesc     string `json:"task_desc,omitempty"`
	Credit       int64  `json:"credit,omitempty"` // 上游 reward_credit
	Energy       int64  `json:"energy,omitempty"` // 上游 reward_energy
	HasReward    bool   `json:"has_reward,omitempty"`
	TaskType     string `json:"task_type,omitempty"`
	Tag          string `json:"tag,omitempty"`
	JumpURL      string `json:"jump_url,omitempty"`
	Locked       bool   `json:"locked,omitempty"`
	Target       int64  `json:"target"`
	Current      int64  `json:"current"`
	AcceptStatus string `json:"accept_status,omitempty"`
	Status       string `json:"status,omitempty"`
	// Claimable 是本地推算：进度达标且未领取。
	Claimable bool `json:"claimable,omitempty"`
	// Claimed 表示 accept_status == claimed。
	Claimed bool `json:"claimed,omitempty"`
	// MPOnly 表示该任务只在小程序口径下发。
	MPOnly bool `json:"mp_only,omitempty"`
}

// -----------------------------------------------------------------------------
// 请求构造
// -----------------------------------------------------------------------------

// growthHeaders CLI 域请求头（任务列表与报名）。
func growthHeaders(cred *CredentialView, p *Profile, mp bool) func(*http.Request) {
	return func(req *http.Request) {
		BackendHeaders(req, cred, p)
		if mp {
			req.Header.Set("X-Client-Platform", mpPlatform)
		}
	}
}

// webHeaders Web 域请求头（领奖 / 连登 / 旅行 / 抽奖 / 上报）。
//
// 形状对照浏览器实际请求：Origin/Referer 指向成长中心，x-client-platform 标记来源端，
// 并且**必须带 X-User-Id**——缺 uid 时上游会 200 但静默丢弃事件（活跃上报尤其如此）。
func webHeaders(cred *CredentialView, p *Profile, platform string) func(*http.Request) {
	if platform == "" {
		platform = "web"
	}
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("User-Agent", p.ClientUA)
		req.Header.Set("Origin", p.Origin)
		req.Header.Set("Referer", p.Origin+"/profile/growth-center")
		req.Header.Set("x-client-platform", platform)
		if cred != nil {
			if cred.AccessToken != "" {
				req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
			}
			if cred.UID != "" {
				req.Header.Set("X-User-Id", cred.UID)
			}
			if cred.EnterpriseID != "" {
				req.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
				req.Header.Set("X-Tenant-Id", cred.EnterpriseID)
			}
			if cred.Domain != "" {
				req.Header.Set("X-Domain", cred.Domain)
			}
		}
	}
}

// growthCall 发一次 growth 域请求并解开信封，返回 data。
func (c *Client) growthCall(ctx context.Context, method, fullURL string, headers func(*http.Request), body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := c.Control.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode >= 400 {
			return nil, &HTTPStatusError{Status: resp.StatusCode, Body: string(raw)}
		}
		return nil, fmt.Errorf("解析成长域响应失败: %w", err)
	}
	// 先判业务码再判传输码：上游常用 HTTP 400 承载业务语义（例如「今天已签到」），
	// 先判 HTTP 会把可解释的业务结果误报成网络错误。
	if env.Code != 0 {
		return nil, fmt.Errorf("成长域业务错误 code=%d msg=%s (HTTP %d)", env.Code, env.Msg, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, &HTTPStatusError{Status: resp.StatusCode, Body: string(raw)}
	}
	return env.Data, nil
}

// HTTPStatusError 是带 HTTP 状态码的成长域错误。
//
// 为什么必须是**类型**而不是只拼一句字符串：有些判定只能看状态码。
// 典型是领养返回 `HTTP 400 + "first_buddy task not completed yet"` ——
// 那是「门槛未达标」的正常语义（当日不该重试），调用方要静默跳过。
// 只靠字符串匹配的话，上游改一个标点就会让判定失效、变成每天刷错误日志。
type HTTPStatusError struct {
	Status int
	Body   string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("成长域请求失败 HTTP %d: %s", e.Status, truncate(e.Body, 200))
}

// buddyTaskIncompleteMarker 领养门槛未达标的关键词（HTTP 400 时出现）。
const buddyTaskIncompleteMarker = "first_buddy task not completed yet"

// IsBuddyTaskIncomplete 判定「领养门槛未达标」：HTTP 400 + first_buddy 关键词。
//
// 该错误**当日不应重试** —— 门槛的真实来源是「当日无活跃上报」，
// 同一天内重复打只会给上游刷请求。
func IsBuddyTaskIncomplete(err error) bool {
	if err == nil {
		return false
	}
	var he *HTTPStatusError
	if !errors.As(err, &he) || he.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(he.Body), buddyTaskIncompleteMarker)
}

// -----------------------------------------------------------------------------
// 任务：列表 / 报名 / 领奖
// -----------------------------------------------------------------------------

// ListGrowthTasks 拉取任务列表；mp 为真时叠加小程序口径头。
func (c *Client) ListGrowthTasks(ctx context.Context, cred *CredentialView, p *Profile, mp bool) ([]GrowthTask, error) {
	data, err := c.growthCall(ctx, http.MethodGet, p.Base+growthTasksPath, growthHeaders(cred, p, mp), nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data, mp)
}

// AcceptGrowthTasks 报名任务（幂等：已报名时上游返回成功或业务提示，都不算致命错误）。
func (c *Client) AcceptGrowthTasks(ctx context.Context, cred *CredentialView, p *Profile, codes []string, mp bool) error {
	_, err := c.growthCall(ctx, http.MethodPost, p.Base+growthAcceptPath,
		growthHeaders(cred, p, mp), map[string]any{"task_codes": codes})
	return err
}

// ClaimGrowthTask 领取任务奖励，返回本次到账的 (积分, 能量)。
//
// 两个口径：小程序限定任务走 CLI 域并在路径里带 code + mp 头；常规任务走 Web 成长中心。
// 前者失败时降级到后者——上游对部分任务/租户形态只在其中一边可用，实测过。
func (c *Client) ClaimGrowthTask(ctx context.Context, cred *CredentialView, p *Profile, code string, mp bool) (credit, energy int64, err error) {
	esc := url.PathEscape(code)
	if mp {
		if data, e := c.growthCall(ctx, http.MethodPost,
			p.Base+"/activity/growth/tasks/"+esc+"/claim", growthHeaders(cred, p, true), nil); e == nil {
			return parseClaim(data)
		}
	}
	data, err := c.growthCall(ctx, http.MethodPost,
		p.Origin+"/activity/growth/tasks/"+esc+"/claim", webHeaders(cred, p, "web"), nil)
	if err != nil {
		return 0, 0, err
	}
	return parseClaim(data)
}

func parseClaim(data json.RawMessage) (int64, int64, error) {
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, nil // 幂等：重复领取不算错误，但没有新增奖励
	}
	return resp.Credit, resp.Energy, nil
}

// parseGrowthTasks 解析任务列表（默认与 mp 口径共用一个结构）。
func parseGrowthTasks(data json.RawMessage, mp bool) ([]GrowthTask, error) {
	var resp struct {
		Tasks []struct {
			TaskCode     string          `json:"task_code"`
			Title        string          `json:"title"`
			Description  string          `json:"description"`
			TaskDesc     string          `json:"task_desc"`
			RewardCredit int64           `json:"reward_credit"`
			RewardEnergy int64           `json:"reward_energy"`
			HasReward    bool            `json:"has_reward"`
			TaskType     string          `json:"task_type"`
			Tag          string          `json:"tag"`
			JumpURL      string          `json:"jump_url"`
			Locked       bool            `json:"locked"`
			AcceptStatus string          `json:"accept_status"`
			Status       string          `json:"status"`
			Target       int64           `json:"target"`
			Current      int64           `json:"current"`
			Progress     json.RawMessage `json:"progress"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]GrowthTask, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		cur, tgt := t.Current, t.Target
		// progress 既可能是 {current,target} 对象，也可能是平铺字段，两种都试。
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current int64 `json:"current"`
				Target  int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil && (pr.Target > 0 || pr.Current > 0) {
				cur, tgt = pr.Current, pr.Target
			}
		}
		claimed := t.AcceptStatus == "claimed"
		out = append(out, GrowthTask{
			TaskCode: t.TaskCode, Title: t.Title, Description: t.Description, TaskDesc: t.TaskDesc,
			Credit: t.RewardCredit, Energy: t.RewardEnergy, HasReward: t.HasReward,
			TaskType: t.TaskType, Tag: t.Tag, JumpURL: t.JumpURL, Locked: t.Locked,
			Target: tgt, Current: cur, AcceptStatus: t.AcceptStatus, Status: t.Status,
			Claimable: !claimed && tgt > 0 && cur >= tgt,
			Claimed:   claimed,
			MPOnly:    mp,
		})
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// 对话活跃上报
// -----------------------------------------------------------------------------

// chatRequestEvent 是客户端 chat_request_send 事件的完整形状。
//
// 为什么照抄全字段而不是只发最小三字段：上游后续加严过（缺字段的事件 200 但被丢弃），
// 而字段多几个没有任何代价——它本来就是客户端会发的形状。
// userId 是必填：缺失时服务端 200 但静默丢弃，这类「成功但没生效」最难排查。
type chatRequestEvent struct {
	EventCode             string `json:"eventCode"`
	Timestamp             int64  `json:"timestamp"`
	ReportDelay           int    `json:"reportDelay"`
	Mode                  string `json:"mode"`
	ConversationID        string `json:"conversationId"`
	RequestID             string `json:"requestId"`
	InputLength           int    `json:"inputLength"`
	RequestModelID        string `json:"requestModelId"`
	RequestModelName      string `json:"requestModelName"`
	IsPlan                bool   `json:"isPlan"`
	IsAutoExecuteTerminal bool   `json:"isAutoExecuteTerminal"`
	IsAutoModify          bool   `json:"isAutoModify"`
	CodebaseEnable        bool   `json:"codebaseEnable"`
	MaxToken              int    `json:"maxToken"`
	MaxSteps              int    `json:"maxSteps"`
	Temperature           int    `json:"temperature"`
	MaxRetries            int    `json:"maxRetries"`
	MentionContexts       []any  `json:"mentionContexts"`
	KnowledgeID           []any  `json:"knowledgeId"`
	KnowledgeName         []any  `json:"knowledgeName"`
	CodebaseID            string `json:"codebaseId"`
	MentionContextCount   int    `json:"mentionContextCount"`
	Command               string `json:"command"`
	ExpertID              string `json:"expertId"`
	RecommendID           string `json:"recommendId"`
	SkillID               string `json:"skillId"`
	SkillCount            int    `json:"skillCount"`
	TotalCount            int    `json:"totalCount"`
	FileURI               string `json:"fileUri"`
	PresentAt             int64  `json:"presentAt"`
	TraceID               string `json:"traceId"`
	RootRequestID         string `json:"rootRequestId"`
	ParentConversationID  string `json:"parentConversationId"`
	AgentName             string `json:"agentName"`
	AgentType             string `json:"agentType"`
	UserID                string `json:"userId"`
}

// ReportChatActivity 上报一条对话活跃事件。
//
// 它同时点亮 growth 连登、给「聊天 N 次」类任务加进度、并解锁领养前置。
// conversationId 由调用方生成即可，上游不校验会话真实性。
// requestID 为空时回落 conversationId；同会话多轮上报时应各传不同 requestID。
func (c *Client) ReportChatActivity(ctx context.Context, cred *CredentialView, p *Profile,
	conversationID, requestID, modelID, modelName string) error {
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = "deepseek-v4.1-flash"
	}
	if modelName == "" {
		modelName = modelID
	}
	now := time.Now().UnixMilli()
	ev := chatRequestEvent{
		EventCode: "chat_request_send", Timestamp: now, Mode: "craft",
		ConversationID: conversationID, RequestID: requestID, InputLength: 12,
		RequestModelID: modelID, RequestModelName: modelName,
		MentionContexts: []any{}, KnowledgeID: []any{}, KnowledgeName: []any{},
		PresentAt: now, RootRequestID: requestID, ParentConversationID: conversationID,
		AgentName: "default", AgentType: "conversation",
		UserID: cred.UID,
	}
	if cred.UID == "" {
		return fmt.Errorf("账号缺少 uid，上游会静默丢弃该事件（请重新登录以补齐凭据）")
	}
	raw, err := json.Marshal([]chatRequestEvent{ev})
	if err != nil {
		return err
	}
	_, err = c.growthCall(ctx, http.MethodPost, p.Origin+reportPath,
		webHeaders(cred, p, "web"), json.RawMessage(raw))
	return err
}

// -----------------------------------------------------------------------------
// 连登 / 旅行 / 抽奖
// -----------------------------------------------------------------------------

// StreakInfo 是连登完整状态。
//
// **形态是嵌套对象**（`streak.days` / `makeup_cards.balance` / `redemption_status.tier_*_status`）——
// 早期版本把 `streak` 当整数解析，真实响应下会直接反序列化失败（对象塞不进 int），
// 表现是「连登天数永远读不到」，所以这里按实测形态建模。
type StreakInfo struct {
	Streak struct {
		Days              int    `json:"days"`
		MonthTotalDays    int    `json:"month_total_days"`
		NextTier          string `json:"next_tier"`
		NextTierRemaining int    `json:"next_tier_remaining"`
	} `json:"streak"`
	LaunchDate  string `json:"launch_date,omitempty"`
	MakeupCards struct {
		Balance int `json:"balance"`
		Max     int `json:"max"`
	} `json:"makeup_cards"`
	Timezone         string `json:"timezone,omitempty"`
	RedemptionStatus struct {
		Tier7dStatus  string `json:"tier_7d_status"`
		Tier14dStatus string `json:"tier_14d_status"`
		Tier28dStatus string `json:"tier_28d_status"`
		RemainingDays int    `json:"remaining_days"`
		Tiers         []struct {
			Tier    string `json:"tier"`
			Days    int    `json:"days"`
			Credit  int64  `json:"credit"`
			Energy  int64  `json:"energy"`
			Cards   int    `json:"cards"`
			Chances int    `json:"chances"`
		} `json:"tiers"`
	} `json:"redemption_status"`
}

// GrowthStreak 查连登完整状态（兑换档位与补签卡余额都在里面）。
func (c *Client) GrowthStreak(ctx context.Context, cred *CredentialView, p *Profile) (*StreakInfo, error) {
	data, err := c.growthCall(ctx, http.MethodGet, p.Origin+streakPath, webHeaders(cred, p, "web"), nil)
	if err != nil {
		return nil, err
	}
	var out StreakInfo
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// clientToken 是兑换/补签用的幂等令牌（前端 randomUUID 同款语义）。
//
// 上游按它去重：不带这个字段时重复请求会被当成新的兑换（弱网重试下可能重复扣档位）。
func clientToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// GrowthRedeemTier 按档位兑换连登奖励（tier: "7d"|"14d"|"28d"）。
//
// 未解锁返回 403「连续登录天数不足」——调用方按 locked 状态跳过即可。
func (c *Client) GrowthRedeemTier(ctx context.Context, cred *CredentialView, p *Profile, tier string) error {
	_, err := c.growthCall(ctx, http.MethodPost, p.Origin+streakRedeemPath,
		webHeaders(cred, p, "web"), map[string]any{"tier": tier, "client_token": clientToken()})
	return err
}

// ClaimGift 领取新手礼包（每号一次；已领过返回业务错误，调用方静默跳过）。
func (c *Client) ClaimGift(ctx context.Context, cred *CredentialView, p *Profile) (int64, error) {
	env, _, err := c.doEnvelope(ctx, http.MethodPost, p.Origin+claimGiftPath,
		webHeaders(cred, p, "web"), strings.NewReader("{}"))
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(env.Data, &resp)
	return resp.Credit, nil
}

// ClaimCompensation 领取活动补偿（有则领，无则业务错误）。
func (c *Client) ClaimCompensation(ctx context.Context, cred *CredentialView, p *Profile) (int64, error) {
	env, _, err := c.doEnvelope(ctx, http.MethodPost, p.Origin+claimCompensationPath,
		webHeaders(cred, p, "web"), strings.NewReader("{}"))
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(env.Data, &resp)
	return resp.Credit, nil
}

// HeatmapYesterdayMissed 检查昨日是否漏签（heatmap 里昨天的 cell score==0）。
func (c *Client) HeatmapYesterdayMissed(ctx context.Context, cred *CredentialView, p *Profile) (bool, error) {
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	data, err := c.growthCall(ctx, http.MethodGet, p.Origin+heatmapPath, webHeaders(cred, p, "web"), nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int    `json:"score"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == yesterday {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// UseMakeupCard 对指定日期使用补签卡（保住连登连续天数；无卡返回业务错误）。
func (c *Client) UseMakeupCard(ctx context.Context, cred *CredentialView, p *Profile, date string) error {
	_, err := c.growthCall(ctx, http.MethodPost, p.Origin+makeupCardPath,
		webHeaders(cred, p, "web"), map[string]any{"target_date": date, "client_token": clientToken()})
	return err
}

// TravelState 是猫咪旅行状态。
//
// 时间字段与 location / letter 的形态上游并不稳定（实测 depart_at/arrive_at 是毫秒时间戳数字，
// 而 location、letter 可能是对象），所以这几项一律用 any 承接——
// 用强类型会让「读状态」在字段形态变化时直接报错，而读状态失败会连带把整个任务面板拖垮，
// 代价远大于放弃这几个字段的类型信息。
type TravelState struct {
	State             string  `json:"state,omitempty"`
	RecordID          int64   `json:"record_id,omitempty"`
	BuddyID           any     `json:"buddy_id,omitempty"`
	Location          any     `json:"location,omitempty"`
	RewardCredit      float64 `json:"reward_credit,omitempty"`
	DepartAt          any     `json:"depart_at,omitempty"`
	ArriveAt          any     `json:"arrive_at,omitempty"`
	ServerNow         any     `json:"server_now,omitempty"`
	DurationHours     int     `json:"duration_hours,omitempty"`
	DailyLimitReached bool    `json:"daily_limit_reached,omitempty"`
	Letter            any     `json:"letter,omitempty"`
	UseDeeplink       any     `json:"use_deeplink,omitempty"`
}

// TravelStatus 查旅行状态。
func (c *Client) TravelStatus(ctx context.Context, cred *CredentialView, p *Profile) (*TravelState, error) {
	data, err := c.growthCall(ctx, http.MethodGet, p.Origin+travelStatusPath, webHeaders(cred, p, "web"), nil)
	if err != nil {
		return nil, err
	}
	var out TravelState
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TravelDepart 让猫出发（locationID 为 0 时由上游选默认目的地）。
func (c *Client) TravelDepart(ctx context.Context, cred *CredentialView, p *Profile, locationID int) error {
	body := map[string]any{}
	if locationID > 0 {
		body["location_id"] = locationID
	}
	_, err := c.growthCall(ctx, http.MethodPost, p.Origin+travelDepartPath, webHeaders(cred, p, "web"), body)
	return err
}

// TravelClaim 领取旅行奖励。
func (c *Client) TravelClaim(ctx context.Context, cred *CredentialView, p *Profile, recordID int64) (int64, error) {
	body := map[string]any{}
	if recordID > 0 {
		body["record_id"] = recordID
	}
	data, err := c.growthCall(ctx, http.MethodPost, p.Origin+travelClaimPath, webHeaders(cred, p, "web"), body)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"reward_credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// Buddy 账号当前的猫档案；无猫时 BuddyInfo 返回 nil。
type Buddy struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// BuddyInfo 查询当前猫档案。
//
// 返回 (nil, nil) 表示**明确的无猫**（`data.buddy` 为 null / 缺字段 / 空对象）——
// 这是调用方判断「该走领养还是该走旅行」的唯一依据，所以「无猫」必须是
// **正常的 nil 而不是错误**：把它当错误会让每个新账号每轮都记一条假失败。
func (c *Client) BuddyInfo(ctx context.Context, cred *CredentialView, p *Profile) (*Buddy, error) {
	data, err := c.growthCall(ctx, http.MethodGet, p.Origin+buddyInfoPath, webHeaders(cred, p, "web"), nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var b Buddy
	if err := json.Unmarshal(resp.Buddy, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// BuddyAgreement 同意领养协议（幂等：重复调用无副作用）。
func (c *Client) BuddyAgreement(ctx context.Context, cred *CredentialView, p *Profile) error {
	_, err := c.growthCall(ctx, http.MethodPost, p.Origin+buddyAgreementPath,
		webHeaders(cred, p, "web"), map[string]any{"agree": true})
	return err
}

// BuddyFirst 领养第一只猫（通过门槛时送 300 分）。
//
// 门槛未达标时返回 HTTP 400，用 `IsBuddyTaskIncomplete` 判定 ——
// 那是**预期行为**（当日无活跃上报），调用方静默跳过而不是报错。
func (c *Client) BuddyFirst(ctx context.Context, cred *CredentialView, p *Profile) error {
	_, err := c.growthCall(ctx, http.MethodPost, p.Origin+buddyFirstPath,
		webHeaders(cred, p, "web"), map[string]any{})
	return err
}

// LotterySummary 查抽奖机会。
//
// module 上游给的是对象（活动模块信息），用 any 承接，理由同 TravelState 的注释。
type LotterySummary struct {
	Chances int `json:"chances"`
	Module  any `json:"module,omitempty"`
}

// LotteryChances 查剩余抽奖次数。
func (c *Client) LotteryChances(ctx context.Context, cred *CredentialView, p *Profile) (*LotterySummary, error) {
	data, err := c.growthCall(ctx, http.MethodGet, p.Origin+lotterySummaryPath, webHeaders(cred, p, "web"), nil)
	if err != nil {
		return nil, err
	}
	var out LotterySummary
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LotteryDraw 抽一次奖，返回上游原始 data（奖品结构随活动变化，不强行建模）。
func (c *Client) LotteryDraw(ctx context.Context, cred *CredentialView, p *Profile) (json.RawMessage, error) {
	return c.growthCall(ctx, http.MethodPost, p.Origin+lotteryDrawPath, webHeaders(cred, p, "web"), map[string]any{})
}

// -----------------------------------------------------------------------------
// 校园日活动
// -----------------------------------------------------------------------------

// SchoolTask 是校园日任务。
type SchoolTask struct {
	TaskCode    string `json:"task_code"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status,omitempty"` // pending | completed | claimed
	RewardText  string `json:"reward_text,omitempty"`
	ChanceGrant bool   `json:"chance_granted,omitempty"`
}

// SchoolTasks 拉取校园日任务，第二个返回值表示活动是否在进行中。
func (c *Client) SchoolTasks(ctx context.Context, cred *CredentialView, p *Profile) ([]SchoolTask, bool, error) {
	data, err := c.growthCall(ctx, http.MethodGet, p.Origin+schoolTaskPath, webHeaders(cred, p, "mp"), nil)
	if err != nil {
		return nil, false, err
	}
	var resp struct {
		Tasks  []SchoolTask `json:"tasks"`
		Active *bool        `json:"active"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		// 活动未开始时上游可能返回空 data 或其它形状，不当作错误
		return nil, false, nil
	}
	active := true
	if resp.Active != nil {
		active = *resp.Active
	}
	return resp.Tasks, active, nil
}

// -----------------------------------------------------------------------------
// 其它
// -----------------------------------------------------------------------------

// ErrGrowthUnsupported 表示该站点不支持成长域（国际站大概率如此）。
var ErrGrowthUnsupported = fmt.Errorf("该站点不支持成长域活动")

// GrowthSupported 粗判站点是否支持成长域（当前只有国内站实测可用）。
//
// 判定依据从「Origin 含 codebuddy.cn」改成档位标识（见 Profile.SupportsGrowthActivity）：
// 域名是**外部事实**，上游换域就会静默失效；档位是账号自身的属性，稳定得多。
func GrowthSupported(p *Profile) bool {
	return p.SupportsGrowthActivity()
}
