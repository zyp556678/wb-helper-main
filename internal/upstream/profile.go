// Package upstream 是反代层，实现以 wb-gateway 为基准：
// 国内站与国际站走同一套 /v2/plugin/* 协议，差异收敛在站点 Profile 里。
package upstream

import (
	"net/url"
	"time"

	"workbuddy-gateway/internal/auth"
)

// Profile 描述一个上游站点的接入参数。
type Profile struct {
	Key       string // 站点标识：cn / intl
	Label     string // 展示名
	Base      string // 上游 API 基础地址
	Origin    string // Origin/Referer 伪装来源（该站 Web 控制台）
	Platform  string // auth/state 的 platform 参数
	ClientUA  string // User-Agent
	ClientID  string // X-Client-ID
	ClientVer string // X-Client-Version
	Product   string // X-Product
}

const (
	clientVersion = "2.143.1"

	// modelsPath 是上游模型目录端点（相对 Base）。
	modelsPath = "/v2/enterprises/personal/models"
)

var (
	// ProfileCN 国内站：copilot.tencent.com，控制台 www.codebuddy.cn。
	ProfileCN = Profile{
		Key: auth.SiteCN, Label: "国内站",
		Base: "https://copilot.tencent.com", Origin: "https://www.codebuddy.cn",
		Platform: "VSCode", ClientUA: "CLI/" + clientVersion + " CodeBuddy/" + clientVersion,
		ClientID: "codebuddy-cli", ClientVer: clientVersion, Product: "SaaS",
	}
	// ProfileINTL 国际站：www.workbuddy.ai，登录在浏览器内完成。
	ProfileINTL = Profile{
		Key: auth.SiteINTL, Label: "国际站",
		Base: "https://www.workbuddy.ai", Origin: "https://www.workbuddy.ai",
		Platform: "workbuddy-ai", ClientUA: "CLI/" + clientVersion + " CodeBuddy/" + clientVersion,
		ClientID: "codebuddy-cli", ClientVer: clientVersion, Product: "SaaS",
	}
)

// ProfileForSite 按站点标识取 Profile；未知值回退国内站。
func ProfileForSite(site string) *Profile {
	if site == auth.SiteINTL {
		return &ProfileINTL
	}
	return &ProfileCN
}

// -----------------------------------------------------------------------------
// 站点能力
//
// 两端**协议相同、能力不同**：登录、对话、模型目录、额度汇总、官方用量两站同构，
// 但有两项活动只有国内站有。参考实现（wb-switch）把这两项写成
// `WbVariant::supports_checkin` / `supports_travel`，并且**在发请求前**就跳过
// 国际站账号 —— 不门控的后果不是「报错」，而是每个周期白发一次注定 404 的请求，
// 在日志里留下一串噪声，让人误以为是网络问题。
// -----------------------------------------------------------------------------

// SupportsCheckin 该站点是否提供每日签到（国际站没有签到接口）。
func (p *Profile) SupportsCheckin() bool {
	return p != nil && p.Key == auth.SiteCN
}

// SupportsGrowthActivity 该站点是否有成长中心活动（成长任务 / 连登 / 派猫猫旅行 / 抽奖）。
func (p *Profile) SupportsGrowthActivity() bool {
	return p != nil && p.Key == auth.SiteCN
}

// AuthStateURL 取授权二维码/链接的地址。
func (p *Profile) AuthStateURL() string {
	return p.Base + "/v2/plugin/auth/state?platform=" + url.QueryEscape(p.Platform)
}

// LoginAccountURL 取授权后的账号信息。
func (p *Profile) LoginAccountURL(state string) string {
	return p.Base + "/v2/plugin/login/account?state=" + url.QueryEscape(state)
}

// AuthTokenURL 轮询取令牌。
func (p *Profile) AuthTokenURL(state string) string {
	return p.Base + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)
}

// TokenRefreshURL 刷新令牌。
func (p *Profile) TokenRefreshURL() string { return p.Base + "/v2/plugin/auth/token/refresh" }

// ChatURL 对话补全（SSE）。
func (p *Profile) ChatURL() string { return p.Base + "/v2/chat/completions" }

// ModelsURL 模型目录（企业端点，补路）。
func (p *Profile) ModelsURL() string { return p.Base + modelsPath }

// V3ConfigURL 官方 IDE 客户端配置目录（/v3/config，模型目录主路）。
//
// 参考实现（repos/wb2api-panel client.go fetchV3ConfigModelMap）用 chat base +
// /v3/config；我们两站没有独立的 chat base，Profile.Base 即 chat base。
func (p *Profile) V3ConfigURL() string { return p.Base + "/v3/config" }

// QuotaSummaryURL 额度汇总（走站点 Web 域）。
func (p *Profile) QuotaSummaryURL() string {
	return p.Origin + "/billing/meter/get-user-resource-summary"
}

// EnterpriseQuotaURL 企业版额度（走站点 Web 域）。
//
// 与个人口径是两个体系：企业号在 get-user-resource-summary 上恒返回空 Packages，
// 必须走这条。详见 Client.enterpriseQuota 的说明。
func (p *Profile) EnterpriseQuotaURL() string {
	return p.Origin + "/v2/billing/meter/get-enterprise-user-usage"
}

// DailyCheckinURL 每日签到（走站点 Web 域）。
func (p *Profile) DailyCheckinURL() string {
	return p.Origin + "/v2/billing/meter/daily-checkin"
}

// CheckinStatusURLs 今日签到状态的查询路径，按优先级排列。
//
// 上游有两个同族端点：新的 `checkin-activity-status` 与旧的 `checkin-status`。
// 两个都要试是**照抄上游客户端的行为**（它在收到 404 时才回落）——
// 只认新端点的话，一旦上游把新端点摘掉，签到状态会整体退化成「查不到」，
// 而这时旧端点其实还是好的。
//
// 注意两个路径的语义差别是「活动制签到」与「旧版签到」，返回体里表示
// 今日是否已签到的字段都是 data.today_checked_in。
func (p *Profile) CheckinStatusURLs() []string {
	return []string{
		p.Origin + "/v2/billing/meter/checkin-activity-status",
		p.Origin + "/v2/billing/meter/checkin-status",
	}
}

// 供调用方按需覆盖控制类请求超时（测试用）。
var ControlTimeout = 60 * time.Second
