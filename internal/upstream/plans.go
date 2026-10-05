package upstream

// 套餐识别（对照 wb-gateway 的 plans.go，逐表逐规则移植）。
//
// 为什么不能只靠额度摘要里的 IsPaidUser 判断：那个字段只说「有没有正式付费订阅」，
// 说不了**哪一档**（国内站是体验版/标准版/高级版/旗舰版，国际站是 免费/pro/Pro试用）。
// 而「哪一档」正是用户点开账号卡片想看的那一格。
//
// 真实来源是**订阅权益**：`/billing/meter/get-user-resource` 分页返回的
// Accounts[] 里每一条都有 PackageCode / Status / 起止时间。规则如下（顺序不可换）：
//
//  1. 先剔除**非套餐编码**（加油包、积分包等）与两个 IDE 附加子产品；
//  2. 每条按 Status + 时间窗口判「当前有效」（Status 1/2 = 退款/过期；3 = 积分用尽，
//     **不等于套餐过期**）；
//  3. 有效权益里，只认「IDE 子产品」或「已知编码」或「摘要明确指定的当前订阅」；
//     遇到未知子产品**标记歧义**而不是静默忽略 —— 静默忽略的后果是明明有付费订阅
//     却显示成免费，比显示「待确认」糟得多；
//  4. 摘要指定了 SubscriptionPackageCode 时以它为准，且必须能在有效权益里找到
//     （找不到就是不一致，报错而不是猜）；
//  5. 没有摘要时，把有效身份包的名称去重合并（国际站已转付费时不再展示 Pro试用）。
//
// 另外两点来自参考实现的实测教训：
//   - 名称必须**净化**：上游的套餐名可能带控制字符/方向符，直接打进终端会篡改显示；
//   - 时间字段既可能是毫秒时间戳、也可能是 `2006-01-02 15:04:05`（UTC+8），
//     两种都要认，且**不依赖部署机时区**（服务器可能跑在 UTC）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"workbuddy-gateway/internal/auth"
)

// PlanUnknown 是无法确认时的展示值。
//
// 刻意不用「免费」兜底：把「查不到」显示成「免费」会让用户以为自己在白用，
// 而真实情况可能是付费订阅查漏了。宁可显示「待确认」。
const PlanUnknown = "待确认"

// legacyPlanNames 是国际站旧套餐编码的短名称（仅用于展示，不用于过滤请求）。
//
// 新编码仍会被查询，并优先显示上游 PackageName —— 官网加套餐后不会悄悄退回「免费」。
var legacyPlanNames = map[string]string{
	"TCACA_code_001_PqouKr6QWV": "免费",
	"TCACA_code_035_ArVxJcGDsm": "免费",
	"TCACA_code_008_cfWoLwvjU4": "免费",
	"TCACA_code_002_AkiJS3ZHF5": "pro",
	"TCACA_code_003_FAnt7lcmRT": "pro",
	"TCACA_code_039_KRcQj7wUat": "Pro试用",
	"TCACA_code_040_mi9rCYg46x": "Pro试用",
}

// cnPlanNames 是国内站的四档名称。
//
// 同一个 Pro 编码在国内站官网展示为「标准版」，不能套用国际站名称 ——
// 这是两套命名，混用会让国内用户看到「pro」这种不该出现的字样。
var cnPlanNames = map[string]string{
	"TCACA_code_008_cfWoLwvjU4": "体验版",
	"TCACA_code_002_AkiJS3ZHF5": "标准版",
	"TCACA_code_026_BaESVICNoi": "高级版",
	"TCACA_code_027_0FCGVA6vSa": "旗舰版",
}

// cnPlanTiers 是国内站的四档展示名（用于把 PackageName 归一进档位）。
var cnPlanTiers = []string{"体验版", "标准版", "高级版", "旗舰版"}

// nonPlanCodes 是**不是套餐**的资源编码：加油包、积分包之类。
//
// 不剔除的后果很直接：它们也在 Accounts[] 里、也可能处于有效状态，
// 会被当成一个订阅身份包算进套餐名。
var nonPlanCodes = map[string]bool{
	"TCACA_code_006_DbXS0lrypC": true,
	"TCACA_code_007_nzdH5h4Nl0": true,
	"TCACA_code_009_0XmEQc2xOf": true,
	"TCACA_code_036_lupO5WgNdG": true,
	"TCACA_code_037_WxOD3MpI2o": true,
}

// 两个 IDE 附加子产品同样不是套餐身份。
const (
	subProductBonusPack  = "sp_tcaca_codebuddyide_bonus_pack"
	subProductCreditPlan = "sp_tcaca_codebuddyide_creditplan"
	// subProductIDE 是「套餐身份」所属的子产品（新套餐沿用它）。
	subProductIDE = "sp_tcaca_codebuddy_ide"
)

// knownPlanName 返回已知编码在该站点的展示名（未知返回空串）。
func knownPlanName(site, code string) string {
	if code == "" {
		return ""
	}
	if site == auth.SiteCN {
		return cnPlanNames[code]
	}
	return legacyPlanNames[code]
}

// freePlanName 是「无付费订阅」时的展示名。
func freePlanName(site string) string {
	if site == auth.SiteCN {
		return "体验版"
	}
	return "免费"
}

// -----------------------------------------------------------------------------
// 权益资源
// -----------------------------------------------------------------------------

// PlanResource 是 `/billing/meter/get-user-resource` 返回的一条订阅权益。
//
// 数值与时间字段一律用 json.RawMessage：上游同一个字段既可能是数字、
// 也可能是字符串（实测 AccountId/Status 都有两种形态），写死成 int 会在
// 字符串形态上直接解析失败 —— 那会让整个套餐识别失败，而不是少一个字段。
type PlanResource struct {
	AccountID          json.RawMessage `json:"AccountId"`
	ResourceID         string          `json:"ResourceId"`
	PackageCode        string          `json:"PackageCode"`
	PackageName        string          `json:"PackageName"`
	SubProductCode     string          `json:"SubProductCode"`
	Status             json.RawMessage `json:"Status"`
	DeductionStartTime json.RawMessage `json:"DeductionStartTime"`
	DeductionEndTime   json.RawMessage `json:"DeductionEndTime"`
	ExpiredTime        json.RawMessage `json:"ExpiredTime"`
	CycleEndTime       json.RawMessage `json:"CycleEndTime"`
}

// planInteger 把可能是数字或字符串的字段读成整数。
func planInteger(raw json.RawMessage) (int64, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0, fmt.Errorf("数值字段缺失")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("数值字段格式发生变化")
	}
	return n, nil
}

// planTimestamp 解析权益时间：空/null/0 表示「没有这个时间」。
//
// 同时认毫秒时间戳、秒时间戳、RFC3339，以及「2006-01-02 15:04:05」（按 UTC+8 解释，
// 与官网展示一致，**不依赖部署机时区**）。
func planTimestamp(raw json.RawMessage) (time.Time, bool, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" || s == "0" {
		return time.Time{}, false, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}, false, fmt.Errorf("权益时间无效")
		}
		if n > 1e12 {
			return time.UnixMilli(n), true, nil
		}
		return time.Unix(n, 0), true, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.FixedZone("UTC+8", 8*3600)); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("无法识别权益时间格式")
}

// activePlanResource 判断一条权益当前是否有效。
//
// Status：1 = 退款、2 = 过期（两者都无效）；0 = 正常、3 = 积分用尽
// （**积分用尽不等于套餐过期** —— 这是参考实现踩过的坑，把 3 当无效会让
// 月内用光积分的用户显示成「免费」）。
func activePlanResource(r PlanResource, now time.Time) (bool, error) {
	status, err := planInteger(r.Status)
	if err != nil {
		return false, err
	}
	switch status {
	case 1, 2:
		return false, nil
	case 0, 3:
	default:
		return false, fmt.Errorf("未知权益状态 %d", status)
	}
	start, present, err := planTimestamp(r.DeductionStartTime)
	if err != nil {
		return false, err
	}
	if present && now.Before(start) {
		return false, nil
	}
	// 与官网相同的截止字段优先级；月度额度周期（CycleEndTime）不是年付套餐的到期。
	for _, raw := range []json.RawMessage{r.DeductionEndTime, r.ExpiredTime, r.CycleEndTime} {
		end, present, err := planTimestamp(raw)
		if err != nil {
			return false, err
		}
		if present {
			return now.Before(end), nil
		}
	}
	return true, nil // Status 有效且无截止时间 = 长期权益
}

// safePlanName 净化上游套餐名：去掉控制字符与格式符（方向控制符能篡改终端显示），
// 并截断到 80 字。
func safePlanName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	runes := []rune(strings.TrimSpace(name))
	if len(runes) > 80 {
		runes = runes[:80]
	}
	return string(runes)
}

// resourcePlanName 给一条有效权益起展示名。
func resourcePlanName(site string, r PlanResource) string {
	if site == auth.SiteCN {
		if label := cnPlanNames[r.PackageCode]; label != "" {
			return label
		}
		label := safePlanName(r.PackageName)
		// 「CodeBuddy个人标准版」这类全名归一进四档；其余（含新套餐、历史青春版）原样保留 ——
		// 强行归入当前四档会把不认识的东西伪装成认识。
		for _, tier := range cnPlanTiers {
			switch label {
			case tier, "个人" + tier, "CodeBuddy个人" + tier:
				return tier
			}
		}
		if label != "" {
			return label
		}
		return "未识别订阅"
	}
	if label := safePlanName(r.PackageName); label != "" {
		switch strings.ToLower(label) {
		case "free plan subscription", "free plan":
			return "免费"
		case "pro plan monthly subscription", "pro plan annual subscription":
			return "pro"
		case "pro plan trial subscription":
			return "Pro试用"
		default:
			return label
		}
	}
	if label := legacyPlanNames[r.PackageCode]; label != "" {
		return label
	}
	return "未识别订阅"
}

// planSummaryInput 是套餐识别需要的摘要字段（从 quotaSummary 抽出来，
// 让识别逻辑不依赖额度解析的内部结构）。
type planSummaryInput struct {
	IsPaidUser              bool
	ProTrialStatus          any
	SubscriptionPackageCode string
}

// identifyPlan 从摘要 + 权益列表识别套餐展示名。
//
// 规则顺序见文件头注释。返回错误表示**无法确认**（而不是免费）：
// 调用方应当把错误显示成「待确认」，而不是回落成「免费」。
func identifyPlan(site string, summary planSummaryInput, resources []PlanResource, now time.Time) (string, error) {
	var plans []PlanResource
	ambiguous := false
	for _, r := range resources {
		if nonPlanCodes[r.PackageCode] || r.SubProductCode == subProductBonusPack ||
			r.SubProductCode == subProductCreditPlan {
			continue
		}
		active, err := activePlanResource(r, now)
		if err != nil {
			return "", err
		}
		if !active {
			continue
		}
		if r.PackageCode == "" {
			return "", fmt.Errorf("有效权益缺少套餐编码")
		}
		// 新套餐沿用 IDE 子产品，或编码已知，或摘要明确指定 —— 三者之一才认。
		if r.SubProductCode == subProductIDE || knownPlanName(site, r.PackageCode) != "" ||
			r.PackageCode == summary.SubscriptionPackageCode {
			plans = append(plans, r)
		} else {
			ambiguous = true // 未知子产品不能静默忽略后假称免费
		}
	}

	// 官网试用已转付费（ProTrialStatus=2）且试用权益仍有效时，仍展示试用：
	// 付费套餐要等试用结束才生效。
	if trialStatus, _ := planInteger(json.RawMessage(fmt.Sprint(summary.ProTrialStatus))); site != auth.SiteCN && trialStatus == 2 {
		for _, r := range plans {
			if legacyPlanNames[r.PackageCode] == "Pro试用" {
				return "Pro试用", nil
			}
		}
	}

	if summary.SubscriptionPackageCode != "" {
		for _, r := range plans {
			if r.PackageCode == summary.SubscriptionPackageCode {
				return resourcePlanName(site, r), nil
			}
		}
		return "", fmt.Errorf("订阅摘要与有效权益不一致")
	}

	labels := map[string]bool{}
	hasFree := false
	for _, r := range plans {
		label := resourcePlanName(site, r)
		if label == freePlanName(site) {
			hasFree = true
		} else {
			labels[label] = true
		}
	}
	if ambiguous {
		return "", fmt.Errorf("发现未知子产品，无法完整确认套餐")
	}
	if len(labels) > 0 {
		if site != auth.SiteCN && len(labels) > 1 {
			delete(labels, "Pro试用") // 已有有效正式订阅时，普通试用不覆盖订阅
		}
		names := make([]string, 0, len(labels))
		for name := range labels {
			names = append(names, name)
		}
		sort.Strings(names)
		return strings.Join(names, " / "), nil
	}
	if summary.IsPaidUser {
		// 声称付费却没有任何有效订阅权益：这是不一致，不能回落成免费。
		return "", fmt.Errorf("付费标记存在但没有有效订阅权益")
	}
	if hasFree || len(plans) == 0 {
		return freePlanName(site), nil
	}
	return "", fmt.Errorf("套餐无法确认")
}

// planLabelFromSummary 是**只读额度摘要**时的套餐展示值（不做额外请求）。
//
// 与完整识别的差别：摘要只带 SubscriptionPackageCode 与 IsPaidUser，
// 说不了「这个订阅还在不在有效期内」。所以这里只给出可以确定的答案：
//
//   - 摘要指定了编码且该编码已知 → 直接给档位名（这是最常见的路径）；
//   - 指定了编码但未知 → 「付费订阅」而不是「免费」（不假装免费）；
//   - 没指定编码：国际站按试用/付费标记给 Pro试用 / pro / 免费；
//     国内站付费标记为真时同样给「付费订阅」（档位未知），否则「体验版」。
func planLabelFromSummary(site string, s quotaSummary) string {
	if code := strings.TrimSpace(s.SubscriptionPackageCode); code != "" {
		if name := knownPlanName(site, code); name != "" {
			return name
		}
		return "付费订阅"
	}
	if site != auth.SiteCN {
		if isTrialActive(s.ProTrialStatus) {
			return "Pro试用"
		}
		if s.IsPaidUser {
			return "pro"
		}
		return "免费"
	}
	if s.IsPaidUser {
		return "付费订阅"
	}
	return freePlanName(site)
}

// -----------------------------------------------------------------------------
// 权益查询
// -----------------------------------------------------------------------------

// planResourcePath 是订阅权益的只读查询端点。
const planResourcePath = "/billing/meter/get-user-resource"

// planPageLimit 是分页安全上限（每页 200 条，10 页 = 2000 条权益）。
const planPageLimit = 10

// parsePlanPage 解析一页权益查询结果（外层 Response.Data，内层 Accounts + TotalCount）。
func parsePlanPage(data []byte) ([]PlanResource, int64, error) {
	var envelope struct {
		Response *struct {
			Data json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Response == nil {
		return nil, 0, fmt.Errorf("套餐接口缺少 Response")
	}
	var page struct {
		Accounts   json.RawMessage
		TotalCount json.RawMessage
	}
	if err := json.Unmarshal(envelope.Response.Data, &page); err != nil {
		return nil, 0, fmt.Errorf("套餐接口 Data 格式无效")
	}
	total, err := planInteger(page.TotalCount)
	if err != nil || total < 0 {
		return nil, 0, fmt.Errorf("套餐接口 TotalCount 缺失或无效")
	}
	trimmed := bytes.TrimSpace(page.Accounts)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, 0, fmt.Errorf("套餐接口 Accounts 不是数组")
	}
	var rows []PlanResource
	if err := json.Unmarshal(page.Accounts, &rows); err != nil {
		return nil, 0, fmt.Errorf("套餐接口 Accounts 格式无效")
	}
	return rows, total, nil
}

// FetchPlanResources 查询全部订阅权益（只读，不做购买/续订/模型请求）。
//
// 三处刻意的保守设计（都来自参考实现）：
//   - 分页期间**总数变化**就放弃本轮（拿到的不是一份一致的快照）；
//   - 同一 AccountId 重复出现判为「分页重复」并放弃（宁可下次再查，也不用残缺结果）；
//   - 超过页数上限就报错，**不返回部分结果** —— 部分结果会得出错误的套餐结论。
func (c *Client) FetchPlanResources(ctx context.Context, cred *CredentialView, p *Profile) ([]PlanResource, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var resources []PlanResource
	lastTotal := int64(-1)
	seen := map[string]bool{}
	local := time.Now().In(time.FixedZone("UTC+8", 8*3600))

	for page := 1; page <= planPageLimit; page++ {
		payload, _ := json.Marshal(map[string]any{
			"PageNumber": page, "PageSize": 200, "ProductCode": "p_tcaca",
			"Status": []int{0, 3}, "OnlyValidPeriod": true,
			"SlicePeriodStartTime": local.Format("2006-01-02") + " 00:00:00",
			"SlicePeriodEndTime":   local.Format("2006-01-02") + " 23:59:59",
		})
		data, _, err := c.doEnvelope(ctx, http.MethodPost, p.Origin+planResourcePath,
			webHeaders(cred, p, "web"), bytes.NewReader(payload))
		if err != nil {
			// 不把上游错误正文写进日志：可能包含用户标识或令牌。
			return nil, fmt.Errorf("套餐只读接口失败: %w", err)
		}
		rows, total, err := parsePlanPage(data.Data)
		if err != nil {
			return nil, err
		}
		if lastTotal >= 0 && total != lastTotal {
			return nil, fmt.Errorf("查询期间套餐总数变化，等待下一轮")
		}
		lastTotal = total
		for _, row := range rows {
			// 一个订阅资源可能有多个计量账户：优先按 AccountId 去重，退回 ResourceId。
			key := strings.Trim(strings.TrimSpace(string(row.AccountID)), `"`)
			if key == "" || key == "null" {
				key = ""
				if row.ResourceID != "" {
					key = "resource:" + row.ResourceID
				}
			} else {
				key = "account:" + key
			}
			if key != "" {
				if seen[key] {
					return nil, fmt.Errorf("套餐分页重复，拒绝使用不完整结果")
				}
				seen[key] = true
			}
		}
		resources = append(resources, rows...)
		if int64(len(resources)) == total {
			return resources, nil
		}
		if len(rows) == 0 || int64(len(resources)) > total {
			return nil, fmt.Errorf("套餐分页不完整")
		}
	}
	return nil, fmt.Errorf("套餐分页超过安全上限，未使用部分结果")
}

// IdentifyPlan 完整识别一个账号的套餐（额度摘要 + 权益列表）。
//
// 返回的第二个值是可读的判定依据（面板直接展示），出错时表示**无法确认**。
func (c *Client) IdentifyPlan(ctx context.Context, cred *CredentialView, p *Profile) (string, string, error) {
	if p == nil {
		return "", "", fmt.Errorf("缺少站点信息")
	}
	resources, err := c.FetchPlanResources(ctx, cred, p)
	if err != nil {
		return PlanUnknown, "", err
	}
	summary := planSummaryInput{}
	if data, _, serr := c.doJSON(ctx, http.MethodPost, p.QuotaSummaryURL(),
		webHeaders(cred, p, "web"), strings.NewReader("{}")); serr == nil {
		var s quotaSummary
		if json.Unmarshal(data, &s) == nil {
			summary = planSummaryInput{
				IsPaidUser:              s.IsPaidUser,
				ProTrialStatus:          s.ProTrialStatus,
				SubscriptionPackageCode: s.SubscriptionPackageCode,
			}
		}
	}
	label, ierr := identifyPlan(p.Key, summary, resources, time.Now())
	if ierr != nil {
		return PlanUnknown, fmt.Sprintf("共 %d 条权益，%v", len(resources), ierr), nil
	}
	return label, fmt.Sprintf("共 %d 条权益，摘要订阅编码=%s", len(resources),
		orNone(summary.SubscriptionPackageCode)), nil
}

// orNone 是空串的占位（展示用）。
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(无)"
	}
	return s
}
