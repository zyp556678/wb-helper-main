package upstream

// /v3/config 主路模型目录探测（企业内部端点 /v2/enterprises/personal/models 的补充路
// 之对侧：v3 主、企业补）。
//
// 依据（repos/wb2api-panel，只读参考）：
//   - client.go fetchV3ConfigModelMap：请求构造（URL = chatBase + /v3/config、X-Domain
//     取法、UA、Authorization/X-User-Id/X-Product/X-CodeBuddy-Request）与响应解析
//     （data.models + data.productFeaturesConfig.ModelTrialBanner + data.modelPromotions）；
//   - client.go codeBuddyIDEUA / codeBuddyCLIUA 注释：同一账号同一端点，不同 UA 下发
//     不同模型集合（2026-09-22 与 2026-10-02 两次实测）；
//   - global_models.go probeGlobalModels：v3 三路 UA 取并集（桌面端为主路、字段权威）、
//     与企业端点家族并发、单路失败 warn 降级、两路全失败才报错。
//
// 该端点与企业端点要求的请求头**不同**：不带 Content-Type / Origin / Referer /
// X-Client-*，但要求 X-Domain / X-Product / X-CodeBuddy-Request，且 UA 必须能解析出
// CodeBuddy 版本号（否则 400 code=12403）——故不复用 commonHeaders，照参考实现逐头写。
//
// 离线限制与零回归保证：我们无法在离线环境验证 /v3/config 在现网账号上是否返回 200、
// 三路 UA 差异是否仍然存在，因此本路全部失败时，FetchModelCatalog 会退回「只有企业
// 端点」的既有行为；若三路返回同集，合并退化为无副作用的去重。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// codeBuddyIDEUA / codeBuddyCLIUA 是 /v3/config 的 IDE / CLI 两路 UA（intl 侧还有一条
// Profile.ClientUA 的桌面端主路，见 v3UARoutes）。
//
// 参考实现实测该端点对 UA 敏感，且**不同 UA 下发不同模型集合**（同一 global 账号）：
//   - IDE UA  → 2026-09-22 实测 14 条：含 o4-mini / enhance-1.0 / auto-chat，
//     无 deepseek 系列；
//   - CLI UA  → 2026-09-22 实测 22 条：含 deepseek-v4.1-flash / deepseek-v4.1-flash-sg /
//     gpt-6-astra / kimi-k2.8-preview，无 o4-mini / enhance-1.0 / auto-chat；
//   - 桌面端 UA → 2026-10-02 复测 29 条：唯一含 gpt-6-sol / gpt-6-luna / grok-4.7 /
//     gemini-3.8-flash。
//
// 易误读点：IDE 响应体积更大（26003B vs 21111B）是**单条字段更全**（flash：393216 +
// low/high/max 档位），不是模型更多——模型数量恰好相反。两路各有独有模型，缺一不可。
//
// 版本号需随上游客户端发版跟进：UA 版本过旧时该端点可能拒绝或返回精简目录。
const (
	codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"
	codeBuddyCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// -----------------------------------------------------------------------------
// 响应解析结构（字段名对齐参考实现 v3ModelPromotion / dynModelEntry）
// -----------------------------------------------------------------------------

// v3Promotion 是 /v3/config data.modelPromotions 的单条优惠定义。
//
// 与 /v2 端点下发的 LivePromotion 形状不完全相同：多出 priority（多条命中时定胜负，
// 如 glm-5.2 白天 badge-only(50) 与夜间五折(100) 靠 priority + daily 窗口双轨切换）、
// schedule.daily 时段窗口（可跨午夜）与 hover 说明。discount 只在部分条目上存在：
// 有 factor 的可算生效价；「错峰使用」类只有时段文案（无机器可读 factor），仅挂标签。
type v3Promotion struct {
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge    *struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		Factor            float64 `json:"factor"`
	} `json:"discount"`
	Hover *struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	Schedule *v3PromotionSchedule `json:"schedule"`
}

// v3PromotionSchedule 优惠时间窗：daily 为每日时段（可跨午夜），validFrom/validUntil
// 为整体有效期（RFC3339，可缺省）；时区实测恒 Asia/Shanghai。
type v3PromotionSchedule struct {
	Daily      []v3PromotionDailyWindow `json:"daily"`
	Timezone   string                   `json:"timezone"`
	ValidFrom  string                   `json:"validFrom"`
	ValidUntil string                   `json:"validUntil"`
}

// v3PromotionDailyWindow 每日时段窗口，形如 {Start:"23:00", End:"7:50"}。
type v3PromotionDailyWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// v3ConfigDoc 是 /v3/config 的 data 段结构（doJSON 已解开统一包络）。
type v3ConfigDoc struct {
	// Include / MergeStrategy：与企业端点同款的目录继承声明。参考实现未解析该端点
	// 的这两个字段，这里按「若下发则解析、不下发则留空」处理，不猜。
	Include       []string `json:"include"`
	MergeStrategy string   `json:"mergeStrategy"`

	Models []liveModelDoc `json:"models"`
	// ProductFeaturesConfig.ModelTrialBanner：试用模型横幅。上游把「N 天免费试用」的
	// 模型只放在这里——实测 global 侧 hy4-preview-f 只出现在此（modelId=hy4-preview-f、
	// targetModelId=hy4-preview），纯 data.models 解析会漏掉它，而它**实际可调用**。
	ProductFeaturesConfig struct {
		ModelTrialBanner struct {
			Banners []struct {
				ModelID       string `json:"modelId"`
				TargetModelID string `json:"targetModelId"`
			} `json:"banners"`
		} `json:"ModelTrialBanner"`
	} `json:"productFeaturesConfig"`
	ModelPromotions []v3Promotion `json:"modelPromotions"`
}

// -----------------------------------------------------------------------------
// 请求
// -----------------------------------------------------------------------------

// v3ConfigDomain 取 /v3/config 的 X-Domain：优先账号落盘 domain（剥 scheme 与尾部斜杠），
// 否则 chat base 的 host；都拿不到时回落到参考实现的默认值。
func v3ConfigDomain(cred *CredentialView, base string) string {
	if cred != nil {
		if d := strings.TrimSpace(cred.Domain); d != "" {
			d = strings.TrimPrefix(d, "https://")
			d = strings.TrimPrefix(d, "http://")
			return strings.TrimSuffix(d, "/")
		}
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return "copilot.tencent.com"
}

// fetchV3Config 单路探测 /v3/config（UA 参数化）。
//
// 请求头照参考实现 fetchV3ConfigModelMap 逐头写：
//
//	Accept: application/json, text/plain, */*
//	X-Requested-With: XMLHttpRequest
//	Authorization: Bearer <accessToken>
//	X-User-Id: <uid>
//	X-Domain: <账号 domain 或 chat base host>
//	X-Product: SaaS（两站 Profile.Product 均为 SaaS，与参考实现硬编码值一致）
//	User-Agent: <该路 UA>
//	X-CodeBuddy-Request: 1（官方客户端风控闸门头）
//
// 失败（非 2xx / 解析失败 / 过滤后为空）返回错误，由 FetchModelCatalog 逐路降级。
func (c *Client) fetchV3Config(ctx context.Context, cred *CredentialView, p *Profile, ua string) (*LiveCatalog, error) {
	product := p.Product
	if product == "" {
		product = "SaaS"
	}
	headers := func(r *http.Request) {
		r.Header.Set("Accept", "application/json, text/plain, */*")
		r.Header.Set("X-Requested-With", "XMLHttpRequest")
		r.Header.Set("X-CodeBuddy-Request", "1")
		r.Header.Set("X-Product", product)
		r.Header.Set("X-Domain", v3ConfigDomain(cred, p.Base))
		r.Header.Set("User-Agent", ua)
		if cred != nil {
			if cred.AccessToken != "" {
				r.Header.Set("Authorization", "Bearer "+cred.AccessToken)
			}
			if cred.UID != "" {
				r.Header.Set("X-User-Id", cred.UID)
			}
		}
	}
	data, _, err := c.doJSON(ctx, http.MethodGet, p.V3ConfigURL(), headers, nil)
	if err != nil {
		return nil, err
	}
	return parseV3Config(data)
}

// -----------------------------------------------------------------------------
// 解析与促销折算
// -----------------------------------------------------------------------------

// parseV3Config 解析 /v3/config 的 data 段，映射为 LiveCatalog。
//
// 解析口径：
//   - 条目字段经 baseLiveModel 映射（与企业端点共用，杜绝口径漂移），窗口 / 能力 /
//     credits 牌价 / efforts 全部照实映射；上游没给的字段留空，不编造；
//   - nonChatModel 过滤在写入前执行（参考实现在 probeV3 里过滤，口径一致）；
//   - 试用横幅模型补入（字段继承 targetModelId 的既有条目，Credits / Tags 与促销
//     折算字段清空——它们描述的是「转正后」的计费与营销信息，用在免费试用版上会
//     误导下游展示）；
//   - modelPromotions 折算生效倍率（见 applyV3Promotions）。
func parseV3Config(data []byte) (*LiveCatalog, error) {
	var doc v3ConfigDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析 /v3/config 失败: %w", err)
	}

	seen := map[string]bool{}
	out := make([]LiveModel, 0, len(doc.Models))
	for _, m := range doc.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		// 非对话模型过滤：id 前缀 / maxOutputTokens≤256 / 生成类 tags（见 nonChatModel）。
		if nonChatModel(id, m.MaxOutputTokens, m.Tags) {
			continue
		}
		seen[id] = true
		out = append(out, baseLiveModel(m))
	}

	// 补入试用横幅模型（见 v3ConfigDoc.ModelTrialBanner 注释）。
	for _, b := range doc.ProductFeaturesConfig.ModelTrialBanner.Banners {
		id := strings.TrimSpace(b.ModelID)
		if id == "" || seen[id] {
			continue
		}
		if nonChatModel(id, 0, nil) {
			continue
		}
		entry := LiveModel{ID: id}
		if tgt := strings.TrimSpace(b.TargetModelID); tgt != "" {
			if i := findLiveModelIndex(out, tgt); i >= 0 {
				entry = out[i]
				entry.ID = id
			}
		}
		// 清空「转正后」的计费与营销字段（见函数注释）；能力字段保留继承值。
		entry.Credits = ""
		entry.Tags = nil
		entry.BaseMultiplier = 0
		entry.HasMultiplier = false
		entry.Multiplier = 0
		entry.PromoLabel = ""
		entry.PromoUntil = ""
		entry.PromoFactor = nil
		entry.PromoFree = false
		entry.PromoExpired = false
		seen[id] = true
		out = append(out, entry)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("/v3/config 模型为空")
	}
	applyV3Promotions(out, doc.ModelPromotions)
	return &LiveCatalog{
		Models:        out,
		Include:       doc.Include,
		MergeStrategy: doc.MergeStrategy,
	}, nil
}

// findLiveModelIndex 返回 id 在 models 中的下标；不存在返回 -1。
func findLiveModelIndex(models []LiveModel, id string) int {
	for i := range models {
		if models[i].ID == id {
			return i
		}
	}
	return -1
}

// v3PromoZone 优惠时区：上游恒 Asia/Shanghai（UTC+8 无夏令时），用 FixedZone 免依赖
// 系统 tzdata（Windows 无 IANA 库时 LoadLocation 会失败）。
var v3PromoZone = time.FixedZone("CST", 8*3600)

// v3PromoClock 解析 "HH:MM" 为当日分钟数；坏值返回 (-1, false)。
func v3PromoClock(hhmm string) (int, bool) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return -1, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return -1, false
	}
	return h*60 + m, true
}

// v3PromoActive 评估优惠在 now 是否生效：enabled + validFrom/validUntil 内 + 落在任一
// daily 窗口（支持跨午夜，如 23:00→7:50，结束时刻为开区间）。schedule 为 nil 视为
// 全天生效。now 需已是 v3PromoZone 时区（调用方转换）。
func v3PromoActive(p *v3Promotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	sc := p.Schedule
	if sc == nil {
		return true
	}
	if sc.ValidFrom != "" {
		if from, err := time.Parse(time.RFC3339, sc.ValidFrom); err == nil && now.Before(from) {
			return false
		}
	}
	if sc.ValidUntil != "" {
		if until, err := time.Parse(time.RFC3339, sc.ValidUntil); err == nil && !now.Before(until) {
			return false
		}
	}
	if len(sc.Daily) == 0 {
		return true
	}
	cur := now.Hour()*60 + now.Minute()
	for _, w := range sc.Daily {
		st, ok1 := v3PromoClock(w.Start)
		ed, ok2 := v3PromoClock(w.End)
		if !ok1 || !ok2 {
			continue
		}
		if st <= ed {
			if cur >= st && cur < ed {
				return true
			}
		} else if cur >= st || cur < ed { // 跨午夜（23:00→7:50）
			return true
		}
	}
	return false
}

// applyV3Promotions 把当前生效的优惠挂到目录条目（参考实现 applyModelPromotions 口径）：
// 同模型多条命中取 priority 最高（相等保留先到者）。只挂标签 / 说明的无机器可读折扣
// 条目不动倍率（PromoFactor 留 nil）——不编造价。
//
// 生效倍率：原生 credits 是牌价，命中带 factor 的优惠才折算生效价
// （factor=0 即限免）；该字段只透出展示，不参与选号。
func applyV3Promotions(out []LiveModel, promos []v3Promotion) {
	if len(out) == 0 || len(promos) == 0 {
		return
	}
	now := time.Now().In(v3PromoZone)
	best := map[string]int{} // 模型 id → promos 下标
	for i := range promos {
		p := &promos[i]
		if !v3PromoActive(p, now) {
			continue
		}
		for _, id := range p.ModelIDs {
			if findLiveModelIndex(out, id) < 0 {
				continue // 目录外模型（如同名他域变体）不挂
			}
			if prev, ok := best[id]; !ok || p.Priority > promos[prev].Priority {
				best[id] = i
			}
		}
	}
	for id, pi := range best {
		p := &promos[pi]
		idx := findLiveModelIndex(out, id)
		if idx < 0 {
			continue
		}
		if p.Badge != nil {
			out[idx].PromoLabel = strings.TrimSpace(p.Badge.Label)
		}
		if p.Schedule != nil {
			out[idx].PromoUntil = p.Schedule.ValidUntil
		}
		if p.Discount != nil {
			f := p.Discount.Factor
			out[idx].PromoFactor = &f
			out[idx].Multiplier = out[idx].BaseMultiplier * f
			if !out[idx].HasMultiplier {
				out[idx].Multiplier = f
			}
			out[idx].HasMultiplier = true
			out[idx].PromoFree = f == 0
		}
	}
}
