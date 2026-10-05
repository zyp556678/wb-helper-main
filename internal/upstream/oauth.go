package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// 浏览器内登录（OAuth 设备授权）
//
// 流程（与 wb-gateway 的 login 命令同一套协议，只是把终端二维码换成浏览器）：
//   1. AuthState   取 state + 授权链接（国内站是微信/企业微信扫码页，国际站在浏览器内完成）
//   2. 用户在浏览器/手机完成授权
//   3. PollToken   轮询取令牌；未授权时上游返回非零 code（中间态，不是失败）
//   4. LoginAccount 用令牌取账号信息
//   5. 落盘凭据并热加载进池
// -----------------------------------------------------------------------------

// LoginTTL 是等待授权完成的超时：国际站要在浏览器里走邮箱/验证码/SSO，比扫码慢。
var LoginTTL = map[string]time.Duration{
	"cn":   5 * time.Minute,
	"intl": 15 * time.Minute,
}

// pendingLoginCodes 是「仍在等待用户授权」的上游业务码。
// 11217 = login ing...（与 wb-gateway 实测一致）；0 表示成功。
var pendingLoginCodes = map[int]bool{11217: true, 11218: true}

// AuthStateResult 是授权状态查询结果。
type AuthStateResult struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
}

// AuthState 取 state 与授权链接。
func (c *Client) AuthState(ctx context.Context, p *Profile) (AuthStateResult, error) {
	var out AuthStateResult
	headers := func(r *http.Request) { commonHeaders(r, p) }
	data, _, err := c.doJSON(ctx, http.MethodPost, p.AuthStateURL(), headers, strings.NewReader("{}"))
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("解析授权状态失败: %w", err)
	}
	if out.State == "" || out.AuthURL == "" {
		return out, fmt.Errorf("上游返回的登录状态信息异常")
	}
	return out, nil
}

// PollToken 轮询令牌。第二个返回值表示「是否已拿到令牌」：
// 为 false 且 err 为 nil 表示仍在等待授权（调用方应继续轮询）。
func (c *Client) PollToken(ctx context.Context, p *Profile, state string) (RefreshedToken, bool, error) {
	var out RefreshedToken
	// 轮询时与官方客户端一致，显式声明无 Authorization
	headers := func(r *http.Request) {
		commonHeaders(r, p)
		r.Header.Set("X-No-Authorization", "1")
	}
	env, status, err := c.doEnvelope(ctx, http.MethodGet, p.AuthTokenURL(state), headers, nil)
	if err != nil {
		if env.Code != 0 && pendingLoginCodes[env.Code] {
			return out, false, nil
		}
		return out, false, err
	}
	if env.Code != 0 {
		if pendingLoginCodes[env.Code] {
			return out, false, nil
		}
		return out, false, fmt.Errorf("上游业务错误 code=%d msg=%s (HTTP %d)", env.Code, env.Msg, status)
	}
	if err := json.Unmarshal(env.Data, &out); err != nil || out.AccessToken == "" {
		return out, false, nil // 数据还没就绪，继续轮询
	}
	return out, true, nil
}

// LoginAccount 用刚拿到的令牌取账号信息。
func (c *Client) LoginAccount(ctx context.Context, p *Profile, state, accessToken string) (AccountInfo, error) {
	var out AccountInfo
	headers := func(r *http.Request) {
		commonHeaders(r, p)
		r.Header.Set("Authorization", "Bearer "+accessToken)
	}
	data, _, err := c.doJSON(ctx, http.MethodGet, p.LoginAccountURL(state), headers, nil)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("解析账号信息失败: %w", err)
	}
	return out, nil
}

// AccountInfo 是上游返回的账号信息。
type AccountInfo struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// -----------------------------------------------------------------------------
// 额度查询
// -----------------------------------------------------------------------------

// Quota 是账号额度快照。
type Quota struct {
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Plan      string  `json:"plan"` // pro | Pro试用 | 免费
	Paid      bool    `json:"paid"`
}

type quotaPackage struct {
	CycleTotalCapacity  string `json:"CycleTotalCapacity"`
	CycleUsedCapacity   string `json:"CycleUsedCapacity"`
	CycleRemainCapacity string `json:"CycleRemainCapacity"`
}

type quotaSummary struct {
	Packages []quotaPackage `json:"Packages"`
	// IsPaidUser 是上游「正式付费订阅」标记；Pro 试用用户该字段同样为 false。
	IsPaidUser bool `json:"IsPaidUser"`
	// ProTrialStatus 是 Pro 试用状态：1=试用中；国内站可能不返回。
	ProTrialStatus any `json:"ProTrialStatus"`
	// SubscriptionPackageCode 非空表示存在订阅包。
	SubscriptionPackageCode string `json:"SubscriptionPackageCode"`
}

// FetchQuota 查询账号额度。走站点 Web 域，需要 web 平台指纹。
func (c *Client) FetchQuota(ctx context.Context, cred *CredentialView, p *Profile) (Quota, error) {
	var out Quota
	headers := func(r *http.Request) {
		commonHeaders(r, p)
		r.Header.Set("Authorization", "Bearer "+cred.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		if cred.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
		}
	}
	data, _, err := c.doJSON(ctx, http.MethodPost, p.QuotaSummaryURL(), headers, strings.NewReader("{}"))
	if err != nil {
		return out, err
	}
	var s quotaSummary
	if err := json.Unmarshal(data, &s); err != nil {
		return out, fmt.Errorf("解析额度响应失败: %w", err)
	}
	for _, pkg := range s.Packages {
		t, err1 := parseCapacity(pkg.CycleTotalCapacity)
		u, err2 := parseCapacity(pkg.CycleUsedCapacity)
		r, err3 := parseCapacity(pkg.CycleRemainCapacity)
		if err1 != nil || err2 != nil || err3 != nil {
			return out, fmt.Errorf("额度字段解析失败: %v %v %v", err1, err2, err3)
		}
		out.Total += t
		out.Used += u
		out.Remaining += r
	}
	out.Paid = s.IsPaidUser
	// 套餐名按站点给：国内站有四档名（体验版/标准版/高级版/旗舰版），
	// 国际站是 免费/pro/Pro试用。摘要里没带编码时给出保守值（不假装免费），
	// 完整识别（含权益有效期）走 IdentifyPlan，见 plans.go。
	out.Plan = planLabelFromSummary(p.Key, s)
	return out, nil
}

func parseCapacity(v string) (float64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	return strconv.ParseFloat(v, 64)
}

// -----------------------------------------------------------------------------
// 积分资源包明细
// -----------------------------------------------------------------------------

// CreditResource 是一个积分资源包（已脱敏：只有数值与到期时间，不含任何凭据）。
//
// 为什么单独做而不是复用 Quota：Quota 只把各包的 total/used/remaining 求和成一个数，
// 而面板的「积分明细」要**逐包**展示（哪个包快到期、哪个包是买的、哪个是活动送的）。
// 求和之后就再也拆不回来了。
type CreditResource struct {
	PackageCode string `json:"package_code"`
	PackageName string `json:"package_name"`
	// Total / Remaining / Used 三者互为兜底：上游有时只给其中两个，
	// 缺失的一个由另外两个推出来（见 normalizeResource）。
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	Used      float64 `json:"used"`
	// ExpireAt 是 Unix 毫秒；0 表示长期有效（上游用 2049 这类占位值表达）。
	ExpireAt int64 `json:"expire_at"`
	Expired  bool  `json:"expired"`
	// ExpiringSoon 是「7 天内到期」，面板用它在明细行上加重。
	ExpiringSoon bool `json:"expiring_soon"`
	// Status 是上游原始状态码；-1 表示上游没给。
	Status int64 `json:"status"`
}

// CreditExpiry 是单账号的积分资源包查询结果。
type CreditExpiry struct {
	Resources []CreditResource `json:"resources"`
	// TotalRemaining 是各包剩余之和（与 summary 的口径一致，用于交叉校验）。
	TotalRemaining float64 `json:"total_remaining"`
	// SoonestExpireAt 是「仍有余额的包里最早的那个到期时间」（Unix 毫秒；0 表示都没有到期时间）。
	SoonestExpireAt int64 `json:"soonest_expire_at"`
	// ExpiringRemaining 是 7 天内到期的余额之和 —— 选号层「快过期优先」就是用它。
	ExpiringRemaining float64 `json:"expiring_remaining"`
}

// resourcePackage 是上游 Packages[] 里的一项。
//
// 字段全部用 any：同一个语义在不同站点/不同版本里既可能是字符串 "100.5"、
// 也可能是数字 100.5，写死成 string 会在数字形态上直接解析失败 —— 而失败的表现是
// 「额度显示为 0」，看起来像真的没额度，比报错更难排查。
type resourcePackage struct {
	PackageCode string `json:"PackageCode"`
	PackageName string `json:"PackageName"`
	Status      any    `json:"Status"`

	CycleCapacitySizePrecise       any `json:"CycleCapacitySizePrecise"`
	CycleCapacitySize              any `json:"CycleCapacitySize"`
	CycleTotalCapacity             any `json:"CycleTotalCapacity"`
	CapacitySizePrecise            any `json:"CapacitySizePrecise"`
	CapacitySize                   any `json:"CapacitySize"`
	SlicePeriodCapacitySizePrecise any `json:"SlicePeriodCapacitySizePrecise"`
	SlicePeriodCapacitySize        any `json:"SlicePeriodCapacitySize"`

	CycleCapacityRemainPrecise       any `json:"CycleCapacityRemainPrecise"`
	CycleCapacityRemain              any `json:"CycleCapacityRemain"`
	CycleRemainCapacity              any `json:"CycleRemainCapacity"`
	CapacityRemainPrecise            any `json:"CapacityRemainPrecise"`
	CapacityRemain                   any `json:"CapacityRemain"`
	SlicePeriodCapacityRemainPrecise any `json:"SlicePeriodCapacityRemainPrecise"`
	SlicePeriodCapacityRemain        any `json:"SlicePeriodCapacityRemain"`

	CycleCapacityUsedPrecise       any `json:"CycleCapacityUsedPrecise"`
	CycleCapacityUsed              any `json:"CycleCapacityUsed"`
	CycleUsedCapacity              any `json:"CycleUsedCapacity"`
	CapacityUsedPrecise            any `json:"CapacityUsedPrecise"`
	CapacityUsed                   any `json:"CapacityUsed"`
	SlicePeriodCapacityUsedPrecise any `json:"SlicePeriodCapacityUsedPrecise"`
	SlicePeriodCapacityUsed        any `json:"SlicePeriodCapacityUsed"`

	DeductionEndTime any `json:"DeductionEndTime"`
	CycleEndTime     any `json:"CycleEndTime"`
}

// expiringSoonDays / farFutureDays 与 wb-switch 取同一组阈值。
const (
	// expiringSoonDays：距到期 7 天内算「快到期」。
	expiringSoonDays = 7
	// cycleOverrideDays：DeductionEndTime 比 CycleEndTime 晚超过一年时，
	// 视前者为长期占位（官方数据里出现过 DeductionEndTime=2049 与
	// CycleEndTime=当月月底并存），改用 CycleEndTime。
	cycleOverrideDays = 365
	// farFutureDays：最终到期时间距现在超过两年时视为长期有效（expire_at=0），
	// 避免 2049 这类占位值流到前端变成「还有 23 年到期」。
	farFutureDays = 730
)

// 积分资源的三路端点。
//
// 为什么必须打三个而不是一个（这是本项目此前的一个**错误结论**，已实测推翻）：
// summary 端点**只返回容量数字，不返回到期时间** —— 它的 Packages[] 里既没有
// CycleEndTime 也没有 DeductionEndTime。到期时间只出现在 paid/free 两个明细端点
// 的 Accounts[] 里（字段名与 Packages[] 完全一致，可直接复用同一套解析）。
//
// 此前只打 summary，于是「到期时间」永远拿不到：卡片只能显示「长期」，
// 选号层的「快过期优先」也永远拿不到数据。
//
// 实测（2026-09-29，国内站 Camellia）：free 端点返回 63 个包，带
// CycleStartTime/CycleEndTime/DeductionEndTime，与官方客户端的到期展示一致。
const (
	resourceSummaryPath = "/billing/meter/get-user-resource-summary"
	resourcePaidPath    = "/billing/meter/get-user-resource-paid-packages"
	resourceFreePath    = "/billing/meter/get-user-resource-free-packages"
)

// 付费/免费包查询码表：国内公开套餐配置 ∪ 官网 usercenter 国际版码集 ∪
// 官方客户端 PAID/FREE_PACKAGE_CODES。
//
// 多带码对不存在的包无副作用；解析侧**不依赖**这份清单 —— summary 仍可带回
// 未列出的包（只是那些包没有时间字段）。所以漏了某个新码的后果是
// 「该包没有到期时间」，而不是「整个查询失败」。
var paidPackageCodes = []string{
	"TCACA_code_002_AkiJS3ZHF5",
	"TCACA_code_023_4xbGhMrE6q",
	"TCACA_code_026_BaESVICNoi",
	"TCACA_code_027_0FCGVA6vSa",
	"TCACA_code_009_0XmEQc2xOf",
	"TCACA_code_038_OhvqZtiPKr",
	"TCACA_code_003_FAnt7lcmRT",
	"TCACA_code_036_lupO5WgNdG",
}

var freePackageCodes = []string{
	"TCACA_code_008_cfWoLwvjU4",
	"TCACA_code_007_nzdH5h4Nl0",
	"TCACA_code_028_NtpWi0jzXs",
	"TCACA_code_029_6wCGEWquYy",
	"TCACA_code_030_BjSt89qTvr",
	"TCACA_code_001_PqouKr6QWV",
	"TCACA_code_006_DbXS0lrypC",
	"TCACA_code_035_ArVxJcGDsm",
	"TCACA_code_037_WxOD3MpI2o",
	"TCACA_code_039_KRcQj7wUat",
}

// creditResourceDoc 同时容纳两种响应形态：
//   - summary 用 Packages[]
//   - paid/free 用 Accounts[]
type creditResourceDoc struct {
	Packages []resourcePackage `json:"Packages"`
	Accounts []resourcePackage `json:"Accounts"`
}

// normalize 把两种形态的条目归一化到同一个切片。
func (d creditResourceDoc) normalize(now int64) []CreditResource {
	out := make([]CreditResource, 0, len(d.Packages)+len(d.Accounts))
	for _, pkg := range d.Accounts {
		out = append(out, normalizeResource(pkg, now))
	}
	for _, pkg := range d.Packages {
		out = append(out, normalizeResource(pkg, now))
	}
	return out
}

// creditResourceRequest 打一个积分资源端点。
func (c *Client) creditResourceRequest(ctx context.Context, cred *CredentialView, p *Profile,
	path, body string) (creditResourceDoc, error) {

	var doc creditResourceDoc
	headers := func(r *http.Request) {
		commonHeaders(r, p)
		r.Header.Set("Authorization", "Bearer "+cred.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		if cred.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
		}
	}
	data, _, err := c.doJSON(ctx, http.MethodPost, p.Origin+path, headers, strings.NewReader(body))
	if err != nil {
		return doc, err
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("解析积分资源响应失败: %w", err)
	}
	return doc, nil
}

// FetchCreditResources 查询单账号的积分资源包明细。
//
// 三路并行：summary（容量兜底）+ paid + free（**到期时间的唯一来源**）。
// 合并规则与官方客户端一致：**明细优先**，summary 只补明细没覆盖到的包
// （按 PackageCode 去重）—— 只有明细带到期时间，让 summary 覆盖会把时间字段抹掉。
//
// 单路失败不影响整体：只要有一路成功就返回它的结果，三路全失败才报错。
// 这样即使 paid/free 某个码表过时，summary 的容量数据仍可用（只是没到期时间）。
func (c *Client) FetchCreditResources(ctx context.Context, cred *CredentialView, p *Profile) (CreditExpiry, error) {
	out := CreditExpiry{Resources: []CreditResource{}}
	now := time.Now().UnixMilli()

	// 免费包要带当天切片时间窗（官方客户端如此）；付费包带续期信息开关。
	day := time.Now().Format("2006-01-02")
	paidBody := mustJSON(map[string]any{
		"PageNumber": 1, "PageSize": 200, "Status": []int{0, 3},
		"PackageCodes": paidPackageCodes, "NeedRenewInfo": true,
	})
	freeBody := mustJSON(map[string]any{
		"PageNumber": 1, "PageSize": 200, "Status": []int{0, 3},
		"SlicePeriodStartTime": day + " 00:00:00", "SlicePeriodEndTime": day + " 23:59:59",
		"PackageCodes": freePackageCodes,
	})

	type result struct {
		kind string // summary | paid | free
		doc  creditResourceDoc
		err  error
	}
	ch := make(chan result, 3)
	launch := func(kind, path, body string) {
		go func() {
			d, e := c.creditResourceRequest(ctx, cred, p, path, body)
			ch <- result{kind: kind, doc: d, err: e}
		}()
	}
	launch("summary", resourceSummaryPath, "{}")
	launch("paid", resourcePaidPath, paidBody)
	launch("free", resourceFreePath, freeBody)

	var (
		detail  []CreditResource
		summary []CreditResource
		errs    []string
		okN     int
	)
	// 按 kind 分派而不是按返回顺序：goroutine 完成次序不确定，
	// 用索引分派会把 paid/free 的结果错当成 summary。
	for i := 0; i < 3; i++ {
		r := <-ch
		if r.err != nil {
			errs = append(errs, r.kind+": "+r.err.Error())
			continue
		}
		okN++
		if r.kind == "summary" {
			summary = r.doc.normalize(now)
		} else {
			detail = append(detail, r.doc.normalize(now)...)
		}
	}
	if okN == 0 {
		return out, fmt.Errorf("积分资源查询全部失败: %s", strings.Join(errs, "; "))
	}

	// 明细优先，summary 只补明细没有的包。
	seen := map[string]bool{}
	for _, r := range detail {
		if r.PackageCode != "" {
			seen[r.PackageCode] = true
		}
	}
	out.Resources = append(out.Resources, detail...)
	for _, r := range summary {
		if r.PackageCode != "" && seen[r.PackageCode] {
			continue
		}
		out.Resources = append(out.Resources, r)
	}

	// 汇总给选号层用的两个数（见 CreditExpiry 注释）。
	for _, r := range out.Resources {
		if r.Remaining <= 0 {
			continue
		}
		if r.ExpireAt > 0 && (out.SoonestExpireAt == 0 || r.ExpireAt < out.SoonestExpireAt) {
			out.SoonestExpireAt = r.ExpireAt
		}
		if r.ExpiringSoon {
			out.ExpiringRemaining += r.Remaining
		}
	}
	out.TotalRemaining = 0
	for _, r := range out.Resources {
		out.TotalRemaining += r.Remaining
	}
	return out, nil
}

// mustJSON 序列化一个必然可序列化的结构；失败时返回 "{}"（不该发生）。
func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// normalizeResource 把上游的一项资源包归一化成面板契约。
//
// 三个容量字段两两兜底：上游不同版本给的名目不一样，且有时只给其中两个。
// 全部缺失时保持 0（而不是报错）—— 一个包没有额度不代表整次查询失败。
func normalizeResource(pkg resourcePackage, now int64) CreditResource {
	res := CreditResource{
		PackageCode: strings.TrimSpace(pkg.PackageCode),
		PackageName: strings.TrimSpace(pkg.PackageName),
		Status:      -1,
	}
	if n, ok := toNumber(pkg.Status); ok {
		res.Status = int64(n)
	}

	rawTotal, hasTotal := firstNumber(
		pkg.CycleCapacitySizePrecise, pkg.CycleCapacitySize, pkg.CycleTotalCapacity,
		pkg.CapacitySizePrecise, pkg.CapacitySize,
		pkg.SlicePeriodCapacitySizePrecise, pkg.SlicePeriodCapacitySize,
	)
	rawRemain, hasRemain := firstNumber(
		pkg.CycleCapacityRemainPrecise, pkg.CycleCapacityRemain, pkg.CycleRemainCapacity,
		pkg.CapacityRemainPrecise, pkg.CapacityRemain,
		pkg.SlicePeriodCapacityRemainPrecise, pkg.SlicePeriodCapacityRemain,
	)
	rawUsed, hasUsed := firstNumber(
		pkg.CycleCapacityUsedPrecise, pkg.CycleCapacityUsed, pkg.CycleUsedCapacity,
		pkg.CapacityUsedPrecise, pkg.CapacityUsed,
		pkg.SlicePeriodCapacityUsedPrecise, pkg.SlicePeriodCapacityUsed,
	)

	switch {
	case hasTotal:
		res.Total = clamp0(rawTotal)
		if hasRemain {
			res.Remaining = clamp0(rawRemain)
		} else {
			res.Remaining = clamp0(res.Total - clamp0(rawUsed))
		}
	case hasRemain && hasUsed:
		res.Remaining = clamp0(rawRemain)
		res.Total = clamp0(rawRemain) + clamp0(rawUsed)
	default:
		res.Remaining = clamp0(rawRemain)
		res.Total = res.Remaining
	}
	if hasUsed {
		res.Used = clamp0(rawUsed)
	} else {
		res.Used = clamp0(res.Total - res.Remaining)
	}

	res.ExpireAt = resolveExpireAt(pkg, now)
	res.Expired = res.ExpireAt > 0 && res.ExpireAt <= now
	res.ExpiringSoon = res.ExpireAt > now && res.ExpireAt-now <= expiringSoonDays*24*3600*1000
	return res
}

// resolveExpireAt 解析到期时间，返回 Unix 毫秒；0 表示长期有效。
func resolveExpireAt(pkg resourcePackage, now int64) int64 {
	deduction, hasDeduction := toMillis(pkg.DeductionEndTime)
	cycle, hasCycle := toMillis(pkg.CycleEndTime)

	expire := int64(0)
	switch {
	case hasDeduction && hasCycle:
		// 占位值修正：DeductionEndTime 远晚于 CycleEndTime 时以周期末为准。
		if deduction-cycle > cycleOverrideDays*24*3600*1000 {
			expire = cycle
		} else {
			expire = deduction
		}
	case hasDeduction:
		expire = deduction
	case hasCycle:
		expire = cycle
	}
	if expire == 0 {
		return 0
	}
	// 远未来视为长期有效，避免占位值被渲染成「还有 N 年到期」。
	if expire-now > farFutureDays*24*3600*1000 {
		return 0
	}
	return expire
}

// firstNumber 返回第一个能解析成数字的值。
func firstNumber(vals ...any) (float64, bool) {
	for _, v := range vals {
		if n, ok := toNumber(v); ok {
			return n, true
		}
	}
	return 0, false
}

// toNumber 宽容解析数字：接受 float64 / int / json.Number / 字符串数字。
func toNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		n, err := t.Float64()
		return n, err == nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		n, err := strconv.ParseFloat(s, 64)
		return n, err == nil
	}
	return 0, false
}

// toMillis 宽容解析时间戳为 Unix 毫秒。
//
// 上游把时间给成毫秒数或日期字符串两种形态；日期字符串按本地时区解析
// （到期时间是「当地当天结束」，用 UTC 解析会整体偏 8 小时）。
func toMillis(v any) (int64, bool) {
	if n, ok := toNumber(v); ok {
		if n <= 0 {
			return 0, false
		}
		// 秒级时间戳（10 位）换算成毫秒。
		if n < 1e11 {
			n *= 1000
		}
		return int64(n), true
	}
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02",
		time.RFC3339, "2006/01/02 15:04:05",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

func clamp0(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// isTrialActive 兼容 ProTrialStatus 的数字与字符串两种编码。
func isTrialActive(v any) bool {
	switch t := v.(type) {
	case float64:
		return t == 1
	case int:
		return t == 1
	case int64:
		return t == 1
	case json.Number:
		n, err := t.Int64()
		return err == nil && n == 1
	case string:
		return strings.TrimSpace(t) == "1"
	default:
		return false
	}
}

// ProbeCredential 用**只读**接口确认凭据是否还能取到账号数据（切片 18）。
//
// 返回上游 HTTP 状态码（网络错误为 0）与错误。
// 与 FetchQuota 的差别只有一个：**把状态码带出来** —— 调用方要靠它区分
// 「上游明确拒绝」（401/403，凭据确实不可用）与「无法判定」（超时 / 5xx，
// 凭据可能还好着）。这两者对「能不能销毁这份凭据」的结论完全相反：
//
//	明确拒绝 → 可以删 / 可以覆盖
//	无法判定 → **必须保守保留**（删除不可逆，宁可留一个失效文件）
//
// 这也正是它单独存在、而不是让调用方解析错误字符串的原因：
// 错误文案随时可能变，而状态码是协议的一部分。
func (c *Client) ProbeCredential(ctx context.Context, cred *CredentialView, p *Profile) (int, error) {
	if cred == nil || strings.TrimSpace(cred.AccessToken) == "" {
		return 0, fmt.Errorf("缺少访问令牌，无法校验")
	}
	if p == nil {
		return 0, fmt.Errorf("缺少站点信息，无法校验")
	}
	headers := func(r *http.Request) {
		commonHeaders(r, p)
		r.Header.Set("Authorization", "Bearer "+cred.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		if cred.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
		}
	}
	_, status, err := c.doJSON(ctx, http.MethodPost, p.QuotaSummaryURL(), headers, strings.NewReader("{}"))
	return status, err
}
