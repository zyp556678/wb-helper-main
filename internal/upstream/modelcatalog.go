package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy-gateway/internal/auth"
)

// -----------------------------------------------------------------------------
// 实时模型目录（含促销、倍率、上下文与思考档位）
//
// 上游接口：GET {Base}/v2/enterprises/personal/models
//
// 实测字段（比早期实现解析的多得多，这里是完整口径）：
//
//	id / name / descriptionZh / descriptionEn
//	credits                    倍率原文，形如 "x0.11 credits"
//	maxInputTokens / maxOutputTokens / maxAllowedSize
//	contextWindow{defaultLength, supportedLengths}
//	supportsReasoning / onlyReasoning / supportsImages / supportsToolCall
//	reasoning{effort|defaultEffort, summary, supportedEfforts, canDisableThinking}
//	isDefault / tags / vendor / temperature / relatedModels
//
// 倍率取法：
//
//	促销生效中 → 生效倍率 = credits × factor（factor=0 即限免）
//	促销已过期 → 只标注 PromoExpired，不把 credits 当现价（上游常把促销价固化进 credits）
//	无促销     → 直接用 credits
//
// 注意：用 CLI 三段式 UA 时上游会下发**精简目录**（部分模型不带 supportedEfforts、
// 上下文数字也偏小）。因此 supportedEfforts 缺席时不要当成「不支持档位」，
// 只表示「本次未下发」，展示侧应回落到默认档位。
//
// 目录探测是「/v3/config 主路 + 企业端点补路」的两路结构（见 FetchModelCatalog）：
//
//	/v3/config（主路）   intl 三路 UA（桌面端 / IDE / CLI，实测各自下发不同集合）；
//	                     cn 仅 IDE 单路（参考实现 FetchModels 口径）；条目字段权威
//	/v2/enterprises/…（补路）  现状单路（Profile.ClientUA），只补 v3 缺失的 id
//
// 该结构（含合并优先级与逐路降级）照参考实现（repos/wb2api-panel/internal/upstream/
// global_models.go 的 probeGlobalModels 与 client.go 的 FetchModels）移植；
// /v3/config 的请求构造、解析与促销折算见 v3config.go。
// -----------------------------------------------------------------------------

// LivePromotion 是上游的促销条目。
type LivePromotion struct {
	ID       string   `json:"id"`
	Enabled  bool     `json:"enabled"`
	ModelIDs []string `json:"modelIds"`
	Badge    struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount struct {
		Factor float64 `json:"factor"`
	} `json:"discount"`
	Schedule struct {
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

// LiveModel 是实时目录里的一项。
type LiveModel struct {
	ID          string
	Name        string
	Description string // 中文描述优先

	Credits        string
	BaseMultiplier float64
	Multiplier     float64
	HasMultiplier  bool

	PromoLabel   string
	PromoUntil   string
	PromoFactor  *float64 // 非 nil 表示有机器可读折扣
	PromoFree    bool
	PromoExpired bool

	MaxInputTokens  int
	MaxOutputTokens int
	MaxAllowedSize  int
	// ContextWindowDefault 是 contextWindow.defaultLength（比 maxInputTokens 更贴近实际可用窗口）。
	ContextWindowDefault int
	// ContextWindowOptions 是 contextWindow.supportedLengths（上下文可选档位）。
	ContextWindowOptions []int

	SupportsReasoning  bool
	OnlyReasoning      bool
	CanDisableThinking bool
	ReasoningEffort    string   // reasoning.defaultEffort 或老键 reasoning.effort
	ReasoningSummary   string   // reasoning.summary
	SupportedEfforts   []string // reasoning.supportedEfforts（可能不下发）

	SupportsImages   bool
	SupportsToolCall bool
	IsDefault        bool
	Tags             []string
	Vendor           string
	Temperature      float64
	RelatedModels    map[string]string
}

// LiveCatalog 是实时目录的完整解析结果。
//
// 为什么不能只返回 []LiveModel：上游对**部分站点**下发的是「覆盖层」而不是完整
// 目录 —— 响应里带 `include: ["../common/product.json"]` + `mergeStrategy: "merge"`，
// 表示该站点的真实模型集 = 公共基座 ∪ 本站覆盖层。只取 models 会把基座里的模型
// 全部丢掉（实测国际站因此少了 deepseek-v4.1-flash 等 5 个模型，
// 而这些模型在该站**实际可调用**）。
type LiveCatalog struct {
	Models []LiveModel
	// Include 是上游声明的继承文件路径，形如 "../common/product.json"。
	// 为空表示本站目录已完整（实测国内站如此）。
	Include []string
	// MergeStrategy 是合并策略，实测只有 "merge"（并集，本站优先）。
	MergeStrategy string
}

// WantsInclude 报告本站是否需要合并公共基座。
//
// 判据是「声明了 include」而不是「站点是不是国际站」：基座要不要合由上游说了算，
// 前端硬编码站点名单会在上游调整后静默失效。实测国内站不下发 include，
// 且其目录里没有的模型（gpt-6-astra / deepseek-v4.1-flash-sg）在国内站确实不可用 ——
// 所以「一律合并」是错的，会把不可用模型报成可用。
func (c *LiveCatalog) WantsInclude() bool {
	return len(c.Include) > 0
}

// liveContextWindowDoc / liveReasoningDoc 是模型条目的窗口与思考能力子结构。
type liveContextWindowDoc struct {
	DefaultLength    int   `json:"defaultLength"`
	SupportedLengths []int `json:"supportedLengths"`
}

type liveReasoningDoc struct {
	DefaultEffort      string   `json:"defaultEffort"`
	Effort             string   `json:"effort"`
	Summary            string   `json:"summary"`
	SupportedEfforts   []string `json:"supportedEfforts"`
	CanDisableThinking bool     `json:"canDisableThinking"`
}

// liveModelDoc 是目录里单条模型的解析形态（不含促销）。
//
// /v2/enterprises/personal/models（企业补路）与 /v3/config（主路）下发的模型对象
// 实测同构（参考实现里两域共用 dynModelEntry），故共用一份解析结构 + baseLiveModel
// 映射，杜绝两域字段口径漂移。
type liveModelDoc struct {
	ID                string                `json:"id"`
	Name              string                `json:"name"`
	DescriptionZh     string                `json:"descriptionZh"`
	DescriptionEn     string                `json:"descriptionEn"`
	Credits           any                   `json:"credits"`
	MaxInputTokens    int                   `json:"maxInputTokens"`
	MaxOutputTokens   int                   `json:"maxOutputTokens"`
	MaxAllowedSize    int                   `json:"maxAllowedSize"`
	ContextWindow     *liveContextWindowDoc `json:"contextWindow"`
	SupportsReasoning bool                  `json:"supportsReasoning"`
	OnlyReasoning     bool                  `json:"onlyReasoning"`
	SupportsImages    bool                  `json:"supportsImages"`
	SupportsToolCall  bool                  `json:"supportsToolCall"`
	IsDefault         bool                  `json:"isDefault"`
	Tags              []string              `json:"tags"`
	Vendor            string                `json:"vendor"`
	Temperature       float64               `json:"temperature"`
	RelatedModels     map[string]string     `json:"relatedModels"`
	Reasoning         *liveReasoningDoc     `json:"reasoning"`
}

type liveCatalogDoc struct {
	// Include / MergeStrategy 是目录继承声明（见 LiveCatalog 注释）。
	Include       []string `json:"include"`
	MergeStrategy string   `json:"mergeStrategy"`

	Models          []liveModelDoc  `json:"models"`
	ModelPromotions []LivePromotion `json:"modelPromotions"`
}

// promoActive 判断促销当前是否生效（enabled 且落在时间窗内）。
func promoActive(p LivePromotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if p.Schedule.ValidFrom != "" {
		if from, err := time.Parse(time.RFC3339, p.Schedule.ValidFrom); err == nil && now.Before(from) {
			return false
		}
	}
	if p.Schedule.ValidUntil != "" {
		if until, err := time.Parse(time.RFC3339, p.Schedule.ValidUntil); err == nil && !now.Before(until) {
			return false
		}
	}
	return true
}

// v3UARoute 是一条 /v3/config 的 UA 探测路：label 仅用于降级日志，ua 是请求 UA。
type v3UARoute struct {
	label string
	ua    string
}

// v3UARoutes 返回本站点 /v3/config 的 UA 集合。
//
// intl → 桌面端主路 + IDE + CLI 三路：参考实现实测同一 global 账号三路下发不同模型集合，
// 缺一路就少一截（桌面端独有 gpt-6-sol / gpt-6-luna / grok-4.7 / gemini-3.8-flash；
// IDE 独有 o4-mini / enhance-1.0 / auto-chat；CLI 独有 deepseek 系列 /
// gpt-6-astra / kimi-k2.8-preview）。cn → IDE 单路（参考实现 FetchModels 口径，
// 不做三 UA 并集）。
//
// 桌面端 UA 必须是**桌面端三段式**（`WorkBuddy/<ver> <平台段>/<ver> CLI/<cliVer>`），
// 不能拿 Profile.ClientUA 顶替：那是 CLI 两段式（`CLI/… CodeBuddy/…`），而该端点按 UA
// 决定下发哪一份目录，参考实现明确警告「送错平台段会被 403 或只给精简目录」。
// 平台段按 realm 取（intl → `WorkBuddy AI`，cn → `WorkBuddy`），与参考
// defaultWorkBuddyUAFor 同口径。
func v3UARoutes(p *Profile) []v3UARoute {
	if p != nil && p.Key == auth.SiteINTL {
		return []v3UARoute{
			{"桌面端-UA", desktopUAFor(p.Key)},
			{"IDE-UA", codeBuddyIDEUA},
			{"CLI-UA", codeBuddyCLIUA},
		}
	}
	return []v3UARoute{{"IDE-UA", codeBuddyIDEUA}}
}

// FetchModelCatalog 拉取实时模型目录：/v3/config 主路 + 企业端点补路**并发**，
// v3 条目字段权威、企业端点只补 v3 缺失的 id（参考实现 probeGlobalModels /
// FetchModels 的合并口径）。
//
// 主路 UA 集合按站点决定（见 v3UARoutes）；补路固定现状单路
// /v2/enterprises/personal/models（Profile.ClientUA，与本次改动前的企业端点请求
// 逐字一致）。
//
// 降级语义（必须守住失败路径零回归）：
//   - v3 某一 UA 路失败 → warn 后用其余 UA 路继续，不报错；
//   - v3 各 UA 路全部失败、企业端点成功 → 返回企业端点结果，与「今天只有企业端点」
//     完全一致；
//   - 企业端点失败、v3 成功 → 只用 v3 结果，不报错；
//   - 两路全失败 → 返回错误（上层 catalog.RefreshOnce 走既有 npm 兜底与缓存逻辑）。
func (c *Client) FetchModelCatalog(ctx context.Context, cred *CredentialView, p *Profile) (*LiveCatalog, error) {
	type routeResult struct {
		cat *LiveCatalog
		err error
	}
	routes := v3UARoutes(p)

	// 每个 UA 路独立 channel、按 routes 顺序收集：并发探测但结果顺序确定
	// （不依赖完成先后）。
	v3Chs := make([]chan routeResult, len(routes))
	for i, r := range routes {
		ch := make(chan routeResult, 1)
		v3Chs[i] = ch
		go func(ua string, ch chan<- routeResult) {
			cat, err := c.fetchV3Config(ctx, cred, p, ua)
			ch <- routeResult{cat: cat, err: err}
		}(r.ua, ch)
	}
	// 企业端点补路与 v3 主路并发；它的失败不拖累主路。
	entCh := make(chan routeResult, 1)
	go func() {
		cat, err := c.fetchEnterpriseCatalog(ctx, cred, p)
		entCh <- routeResult{cat: cat, err: err}
	}()

	// v3 各路自合并：首条成功路整体作为基底（字段权威），后续成功路只补缺失的 id。
	var v3 *LiveCatalog
	var firstV3Err error
	nV3OK := 0
	for i, r := range routes {
		res := <-v3Chs[i]
		if res.err != nil {
			log.Printf("WARN: [upstream] %s模型目录 /v3/config %s 探测失败（降级用其余路）: %v",
				profileLabel(p), r.label, res.err)
			if firstV3Err == nil {
				firstV3Err = res.err
			}
			continue
		}
		nV3OK++
		v3 = mergeLiveCatalogs(v3, res.cat)
	}
	ent := <-entCh

	if nV3OK == 0 {
		if ent.err != nil {
			// 两路全失败：返回企业端点错误。这条错误与今天 CN 单路失败、
			// intl 首路（Profile.ClientUA）失败同源，保持既有失败信息不漂移。
			return nil, ent.err
		}
		// v3 全挂：行为与「只有企业端点」的今天完全一致（零回归）。
		log.Printf("WARN: [upstream] %s模型目录 /v3/config 各 UA 路全部失败，降级为企业端点: %v",
			profileLabel(p), firstV3Err)
		return ent.cat, nil
	}
	if ent.err != nil {
		// 企业端点失败：只用 v3 结果，不报错（与「v3 失败降级为企业」互为镜像）。
		log.Printf("WARN: [upstream] %s模型目录 企业端点探测失败（只用 /v3/config 结果）: %v",
			profileLabel(p), ent.err)
		return v3, nil
	}
	// 两路皆成功：v3 为主、企业端点只补缺失 id（包络字段缺省继承见 mergeLiveCatalogs）。
	return mergeLiveCatalogs(v3, ent.cat), nil
}

// profileLabel 返回日志用站点名；p 为 nil 时给占位，避免日志出现空白的「模型目录」。
func profileLabel(p *Profile) string {
	if p == nil {
		return ""
	}
	return p.Label
}

// fetchEnterpriseCatalog 单路探测企业端点 /v2/enterprises/personal/models（补路）。
//
// 请求 URL 与请求头保持本次改动前的企业端点行为逐字一致：commonHeaders（含
// Profile.ClientUA）+ X-Client-ID / X-Client-Version / X-Product / Authorization /
// X-User-Id / X-Enterprise-Id / X-Domain。
func (c *Client) fetchEnterpriseCatalog(ctx context.Context, cred *CredentialView, p *Profile) (*LiveCatalog, error) {
	headers := func(r *http.Request) {
		commonHeaders(r, p)
		r.Header.Set("X-Client-ID", p.ClientID)
		r.Header.Set("X-Client-Version", p.ClientVer)
		r.Header.Set("X-Product", p.Product)
		if cred != nil {
			if cred.AccessToken != "" {
				r.Header.Set("Authorization", "Bearer "+cred.AccessToken)
			}
			if cred.UID != "" {
				r.Header.Set("X-User-Id", cred.UID)
			}
			if cred.EnterpriseID != "" {
				r.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
			}
			if cred.Domain != "" {
				r.Header.Set("X-Domain", cred.Domain)
			}
		}
	}
	data, _, err := c.doJSON(ctx, http.MethodGet, p.ModelsURL(), headers, nil)
	if err != nil {
		return nil, err
	}
	return ParseLiveCatalog(data)
}

// mergeLiveCatalogs 合并两路目录（v3 多 UA 路之间，以及 v3 主路 + 企业补路）：
// primary 字段权威，secondary 只补 primary 缺失的模型 id（该 id 的条目字段取自其
// 所属路）。primary 为 nil 时整体取 secondary（首个成功路即基底）。
// 输出顺序 = primary 原序 + secondary 补充项原序，稳定不依赖 map 迭代序。
//
// 包络字段（include / mergeStrategy）特殊处理：primary 缺省时继承 secondary ——
// 国际站的 include 声明来自企业端点，/v3/config 不下发该字段时不能丢；丢了会让
// 目录退回 npm 单源后不再合并公共基座（少一截模型）。
func mergeLiveCatalogs(primary, secondary *LiveCatalog) *LiveCatalog {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	out := *primary
	if len(out.Include) == 0 && len(secondary.Include) > 0 {
		out.Include = secondary.Include
	}
	if out.MergeStrategy == "" {
		out.MergeStrategy = secondary.MergeStrategy
	}
	if len(secondary.Models) == 0 {
		return &out
	}
	seen := make(map[string]bool, len(primary.Models)+len(secondary.Models))
	out.Models = make([]LiveModel, 0, len(primary.Models)+len(secondary.Models))
	for _, m := range primary.Models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out.Models = append(out.Models, m)
	}
	for _, m := range secondary.Models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out.Models = append(out.Models, m)
	}
	return &out
}

// nonChatModel 判定是否非对话模型（应从模型目录过滤掉）。
// 来源：harness buddy.ts:547-555 的三类规则：
//   - id 前缀 nes- / completion- / codewise-：嵌入 / 补全 / 代码专用模型，选了报 code=11102；
//   - maxOutputTokens ≤ 256：tiny 输出非对话模型；
//   - tags 含生成类标签（图片 / 视频）：生成模型走各自专用端点，作为对话模型选上去
//     只会报 11102，非本网关用途。
//
// 生成类标签按参考实现（commit 44ca8ff）一次枚举四类：早期参考实现只认
// text-to-image，桌面端目录（2026-10-02 实测）另有 text-to-video / image-to-video
// （seedance 系列）与 image-to-image（gpt-image 系列，通常与 text-to-image 同时
// 出现）——四类全列，避免 tags 只带视频标签的条目漏过。注意本函数 CN 与 global
// 两域共用（在 ParseLiveCatalog 内调用），新增标签对两域同时生效。
func nonChatModel(id string, maxOutputTokens int, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		switch t {
		case "text-to-image", "image-to-image", "text-to-video", "image-to-video":
			return true
		}
	}
	return false
}

// ParseLiveCatalog 解析实时目录的 data 段并折算生效倍率。
func ParseLiveCatalog(data []byte) (*LiveCatalog, error) {
	var doc liveCatalogDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析实时目录失败: %w", err)
	}
	if len(doc.Models) == 0 {
		return nil, fmt.Errorf("实时目录 models 为空")
	}
	now := time.Now()

	// 把促销摊平到模型维度：同一模型命中多条生效促销时取折扣最低（factor 最小）的那条
	type hit struct {
		factor  float64
		label   string
		until   string
		expired bool
	}
	hits := map[string]hit{}
	for _, pr := range doc.ModelPromotions {
		on := promoActive(pr, now)
		label := strings.TrimSpace(pr.Badge.Label)
		if label == "" {
			label = "Promo"
		}
		for _, id := range pr.ModelIDs {
			if on {
				if old, ok := hits[id]; ok && !old.expired && old.factor <= pr.Discount.Factor {
					continue
				}
				hits[id] = hit{factor: pr.Discount.Factor, label: label, until: pr.Schedule.ValidUntil}
			} else {
				if old, ok := hits[id]; ok && old.expired {
					continue
				}
				hits[id] = hit{label: label, until: pr.Schedule.ValidUntil, expired: true}
			}
		}
	}

	seen := map[string]bool{}
	out := make([]LiveModel, 0, len(doc.Models))
	for _, m := range doc.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		// 非对话模型过滤（见 nonChatModel）：生成类 / 嵌入 / 补全模型混进目录，
		// 客户端选中后只会报 code=11102。
		if nonChatModel(id, m.MaxOutputTokens, m.Tags) {
			continue
		}
		seen[id] = true

		entry := baseLiveModel(m)

		if h, ok := hits[id]; ok {
			entry.PromoLabel = h.label
			entry.PromoUntil = h.until
			if h.expired {
				entry.PromoExpired = true
			} else {
				f := h.factor
				entry.PromoFactor = &f
				entry.Multiplier = entry.BaseMultiplier * h.factor
				if !entry.HasMultiplier {
					entry.Multiplier = h.factor
				}
				entry.HasMultiplier = true
				if h.factor == 0 {
					entry.PromoFree = true
				}
			}
		}
		out = append(out, entry)
	}
	return &LiveCatalog{
		Models:        out,
		Include:       doc.Include,
		MergeStrategy: doc.MergeStrategy,
	}, nil
}

// baseLiveModel 把解析条目映射为 LiveModel 的基础字段（不含促销折算）。
//
// 企业端点（ParseLiveCatalog）与 /v3/config（v3config.go parseV3Config）共用这一份
// 映射，杜绝两域字段口径漂移；上游省略的字段保持零值（展示侧按「空值省略」处理，
// 不编造）。
func baseLiveModel(m liveModelDoc) LiveModel {
	entry := LiveModel{
		ID:                strings.TrimSpace(m.ID),
		Name:              strings.TrimSpace(m.Name),
		Description:       pickDescription(m.DescriptionZh, m.DescriptionEn),
		MaxInputTokens:    m.MaxInputTokens,
		MaxOutputTokens:   m.MaxOutputTokens,
		MaxAllowedSize:    m.MaxAllowedSize,
		SupportsReasoning: m.SupportsReasoning,
		OnlyReasoning:     m.OnlyReasoning,
		SupportsImages:    m.SupportsImages,
		SupportsToolCall:  m.SupportsToolCall,
		IsDefault:         m.IsDefault,
		Tags:              m.Tags,
		Vendor:            m.Vendor,
		Temperature:       m.Temperature,
		RelatedModels:     m.RelatedModels,
	}
	if m.ContextWindow != nil {
		entry.ContextWindowDefault = m.ContextWindow.DefaultLength
		entry.ContextWindowOptions = m.ContextWindow.SupportedLengths
	}
	if m.Reasoning != nil {
		// 新模型用 defaultEffort，老模型用 effort，两者都兜
		entry.ReasoningEffort = firstNonEmptyStr(m.Reasoning.DefaultEffort, m.Reasoning.Effort)
		entry.ReasoningSummary = m.Reasoning.Summary
		entry.SupportedEfforts = m.Reasoning.SupportedEfforts
		entry.CanDisableThinking = m.Reasoning.CanDisableThinking
	}

	if s, ok := m.Credits.(string); ok {
		entry.Credits = strings.TrimSpace(s)
	}
	base, hasBase := parseCreditsValue(m.Credits)
	entry.BaseMultiplier = base
	entry.HasMultiplier = hasBase
	entry.Multiplier = base
	return entry
}

func pickDescription(zh, en string) string {
	if s := strings.TrimSpace(zh); s != "" {
		return s
	}
	return strings.TrimSpace(en)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// parseCreditsValue 解析 credits 字段为数值倍率（"x0.29 credits" → 0.29）。
func parseCreditsValue(v any) (float64, bool) {
	switch x := v.(type) {
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		s = strings.TrimPrefix(s, "x")
		s = strings.ReplaceAll(s, "credits", "")
		s = strings.ReplaceAll(s, "credit", "")
		s = strings.TrimSpace(s)
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}
