// Package catalog 维护模型目录与价格台账（切片 3）。
//
// 数据来源两路合并（接口优先）：
//
//	实时接口  GET {Base}/v2/enterprises/personal/models —— 权威，带促销与倍率
//	npm 静态目录 @tencent-ai/codebuddy-code 包内 product.*.json —— 兜底，接口失败时仍能列模型
//
// 合并规则：按模型 ID 去重、实时接口优先；两路都失败则用本地缓存；
// 失败且无缓存时该站点本轮不展示模型（不影响模型调用——模型是透传的）。
//
// 价格判定为什么用**余额差分**而不是只看 usage.credit：
// 实测遇到过 usage.credit=0（看起来免费）但余额确实下降的情况，
// 也就是说 credit 并非所有模型的真实计费信号。因此以「探测前后余额差」为准，
// credit 只作为参考值记录。
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/upstream"
)

// 缓存与探测节奏。
const (
	cacheSchema = 1

	// ProbeInterval 是同一 (站点, 模型) 的价格复测间隔。
	ProbeInterval = 12 * time.Hour
	// ProbeStartDelay 是启动后首轮探测的延迟：先让账号池与目录就绪。
	ProbeStartDelay = 20 * time.Second
	// ProbeTick 是收敛后的探测巡检间隔。
	ProbeTick = 30 * time.Minute
	// ProbeBatch 是单轮最多探测的模型数（实测消耗额度，必须限量）。
	ProbeBatch = 5
	// ProbeInitialBatch 是首轮/刷新后的追赶批量。
	ProbeInitialBatch = 20
	// freeMinTokens 是判定「免费」所需的最小样本量：
	// 太短的回复可能根本没产生计费，样本不足时保持未知而不是误判免费。
	freeMinTokens = 100
)

// Model 是目录里的一个模型（字段口径对齐上游实时目录）。
type Model struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`

	Credits        string  `json:"credits,omitempty"`
	BaseMultiplier float64 `json:"baseMultiplier,omitempty"`
	Multiplier     float64 `json:"multiplier,omitempty"`
	HasMultiplier  bool    `json:"hasMultiplier,omitempty"`

	PromoLabel   string   `json:"promoLabel,omitempty"`
	PromoUntil   string   `json:"promoUntil,omitempty"`
	PromoFactor  *float64 `json:"promoFactor,omitempty"`
	PromoFree    bool     `json:"promoFree,omitempty"`
	PromoExpired bool     `json:"promoExpired,omitempty"`

	FromLive        bool `json:"fromLive,omitempty"`
	MaxInputTokens  int  `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int  `json:"maxOutputTokens,omitempty"`
	MaxAllowedSize  int  `json:"maxAllowedSize,omitempty"`
	// ContextWindowDefault 是上游 contextWindow.defaultLength（优先于 maxInputTokens 展示）。
	ContextWindowDefault int `json:"contextWindowDefault,omitempty"`
	// ContextWindowOptions 是上游 contextWindow.supportedLengths。
	ContextWindowOptions []int `json:"contextWindowOptions,omitempty"`

	SupportsReasoning  bool     `json:"supportsReasoning,omitempty"`
	OnlyReasoning      bool     `json:"onlyReasoning,omitempty"`
	CanDisableThinking bool     `json:"canDisableThinking,omitempty"`
	ReasoningEffort    string   `json:"reasoningEffort,omitempty"`
	ReasoningSummary   string   `json:"reasoningSummary,omitempty"`
	SupportedEfforts   []string `json:"supportedEfforts,omitempty"`

	SupportsImages   bool              `json:"supportsImages,omitempty"`
	SupportsToolCall bool              `json:"supportsToolCall,omitempty"`
	IsDefault        bool              `json:"isDefault,omitempty"`
	Tags             []string          `json:"tags,omitempty"`
	Vendor           string            `json:"vendor,omitempty"`
	Temperature      float64           `json:"temperature,omitempty"`
	RelatedModels    map[string]string `json:"relatedModels,omitempty"`
}

// ContextLength 返回展示用上下文长度：优先 contextWindow.defaultLength，其次 maxInputTokens。
func (m Model) ContextLength() int {
	if m.ContextWindowDefault > 0 {
		return m.ContextWindowDefault
	}
	return m.MaxInputTokens
}

// Probe 是某 (站点, 模型) 的价格探测结论。
type Probe struct {
	LastProbeAt int64   `json:"lastProbeAt,omitempty"`
	Verdict     string  `json:"verdict,omitempty"` // free | paid
	Cost        float64 `json:"cost,omitempty"`    // 余额差分（本次请求消耗的积分）
	Credit      float64 `json:"credit,omitempty"`  // 上游 usage.credit（参考）
	Tokens      int64   `json:"tokens,omitempty"`
	Detail      string  `json:"detail,omitempty"`
}

// Catalog 是目录与价格台账。
type Catalog struct {
	mu        sync.RWMutex
	bySite    map[string][]Model
	probes    map[string]Probe
	source    string
	fetchedAt map[string]int64
	// includes 记录各站点是否声明了 include（见 cacheDoc.Includes）。
	includes map[string]bool

	cacheFile string
	client    *upstream.Client
	accts     *pool.Pool

	// NPMEnabled 控制是否使用 npm 静态目录兜底（默认开）。
	NPMEnabled bool
	// ProbeEnabled 控制价格探测（默认开）。
	ProbeEnabled bool

	// probeMu 串行化探测：探测会消耗额度，且需要前后两次额度读数稳定，
	// 并发探测会互相污染余额差分。
	probeMu sync.Mutex

	Logf func(format string, args ...any)
}

// New 构造目录。
//
// accts 直接依赖 *pool.Pool 而不是抽象接口：探测需要「持有某账号的串行锁」，
// 抽象成接口反而容易写出「持锁后又调加锁方法」的自锁代码（已踩过一次）。
func New(cacheFile string, client *upstream.Client, accts *pool.Pool) *Catalog {
	return &Catalog{
		bySite:       map[string][]Model{},
		probes:       map[string]Probe{},
		fetchedAt:    map[string]int64{},
		includes:     map[string]bool{},
		cacheFile:    cacheFile,
		client:       client,
		accts:        accts,
		NPMEnabled:   true,
		ProbeEnabled: true,
	}
}

func (c *Catalog) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func probeKey(site, model string) string {
	return site + "|" + strings.ToLower(strings.TrimSpace(model))
}

// -----------------------------------------------------------------------------
// 缓存
// -----------------------------------------------------------------------------

type cacheDoc struct {
	Schema    int                `json:"schema"`
	Source    string             `json:"source"`
	FetchedAt map[string]int64   `json:"fetchedAt"`
	Catalogs  map[string][]Model `json:"catalogs"`
	Probes    map[string]Probe   `json:"probes,omitempty"`
	// Includes 记住各站点是否声明了 include（需合并公共基座）。
	//
	// 为什么要持久化：这个声明只能从实时接口学到，而实时接口故障时目录会退回
	// npm 单源。若不留存，一次实时抖动就会让国际站从 44 个模型掉回 22 个 ——
	// 而目录退化的表现是「模型列表少了一截」，不像报错那样容易被发现。
	Includes map[string]bool `json:"includes,omitempty"`
}

// LoadCache 读本地缓存（不存在或损坏都静默跳过，不影响启动）。
func (c *Catalog) LoadCache() {
	if c.cacheFile == "" {
		return
	}
	data, err := os.ReadFile(c.cacheFile)
	if err != nil {
		return
	}
	var doc cacheDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		c.logf("[目录] 缓存损坏，忽略: %v", err)
		return
	}
	if doc.Schema != cacheSchema {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bySite = doc.Catalogs
	c.probes = doc.Probes
	c.source = doc.Source
	c.fetchedAt = doc.FetchedAt
	c.includes = doc.Includes
	if c.fetchedAt == nil {
		c.fetchedAt = map[string]int64{}
	}
	if c.probes == nil {
		c.probes = map[string]Probe{}
	}
	if c.includes == nil {
		c.includes = map[string]bool{}
	}
	total := 0
	for _, ms := range c.bySite {
		total += len(ms)
	}
	if total > 0 {
		c.logf("[目录] 已从缓存载入 %d 个模型（来源=%s）", total, c.source)
	}
}

// saveCache 原子写缓存。
func (c *Catalog) saveCache() {
	if c.cacheFile == "" {
		return
	}
	c.mu.RLock()
	doc := cacheDoc{
		Schema:    cacheSchema,
		Source:    c.source,
		FetchedAt: c.fetchedAt,
		Catalogs:  c.bySite,
		Probes:    c.probes,
		Includes:  c.includes,
	}
	c.mu.RUnlock()

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return
	}
	tmp := c.cacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.cacheFile)
}

// -----------------------------------------------------------------------------
// 查询
// -----------------------------------------------------------------------------

// Merged 返回跨站点去重后的模型列表（保持各站点内的原始顺序，站点按 cn/intl）。
func (c *Catalog) Merged() []Model {
	c.mu.RLock()
	defer c.mu.RUnlock()
	seen := map[string]bool{}
	out := []Model{}
	for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
		for _, m := range c.bySite[site] {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	return out
}

// EffortTables 返回「模型 → 支持档位」与「模型 → 默认档」两张表，
// 供出站改写层做档位降级与补默认档。
func (c *Catalog) EffortTables() (map[string][]string, map[string]string) {
	efforts := map[string][]string{}
	defaults := map[string]string{}
	for _, m := range c.Merged() {
		if len(m.SupportedEfforts) > 0 {
			if _, ok := efforts[m.ID]; !ok {
				efforts[m.ID] = m.SupportedEfforts
			}
		}
		if m.ReasoningEffort != "" {
			if _, ok := defaults[m.ID]; !ok {
				defaults[m.ID] = m.ReasoningEffort
			}
		}
	}
	return efforts, defaults
}

// ModelIDs 返回去重后的模型 ID 列表（排序，供 /v1/models 使用）。
func (c *Catalog) ModelIDs() []string {
	models := c.Merged()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

// Entry 取某站点某模型的目录条目。
func (c *Catalog) Entry(site, model string) (Model, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.bySite[site] {
		if strings.EqualFold(m.ID, model) {
			return m, true
		}
	}
	return Model{}, false
}

// Source 返回目录来源标签与快照时间。
func (c *Catalog) Source() (string, int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var latest int64
	for _, t := range c.fetchedAt {
		if t > latest {
			latest = t
		}
	}
	src := c.source
	if src == "" {
		src = "unavailable"
	}
	return src, latest
}

// SiteCount 返回某站点本轮目录的模型数。
func (c *Catalog) SiteCount(site string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.bySite[site])
}

// ModelsForSite 返回某站点本轮目录的模型列表（按 ID 排序，拷贝出去避免调用方误改内部切片）。
func (c *Catalog) ModelsForSite(site string) []Model {
	c.mu.RLock()
	out := make([]Model, len(c.bySite[site]))
	copy(out, c.bySite[site])
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// FetchedAtSite 返回某站点目录快照时间；0 表示本轮没有该站点的数据
// （通常是该站点没有可用凭据，拉不到实时目录）。
func (c *Catalog) FetchedAtSite(site string) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fetchedAt[site]
}

// ProbeSummarySite 汇总某站点的探测结论计数，用于双站视图展示价格覆盖度。
func (c *Catalog) ProbeSummarySite(site string) (free, paid, unknown int, lastAt int64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	prefix := site + "|"
	for k, p := range c.probes {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		switch p.Verdict {
		case "free":
			free++
		case "paid":
			paid++
		default:
			unknown++
		}
		if p.LastProbeAt > lastAt {
			lastAt = p.LastProbeAt
		}
	}
	return
}

// ProbeOf 返回某 (站点, 模型) 的探测结论。
func (c *Catalog) ProbeOf(site, model string) (Probe, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.probes[probeKey(site, model)]
	return p, ok
}

// Probes 返回探测台账快照。
func (c *Catalog) Probes() map[string]Probe {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]Probe, len(c.probes))
	for k, v := range c.probes {
		out[k] = v
	}
	return out
}

// -----------------------------------------------------------------------------
// 价格判定与展示口径
// -----------------------------------------------------------------------------

// DisplayMultiplier 生成展示用倍率文案。
//
// 规则（与 wb-gateway 同口径）：
//
//	实测免费 / 接口明确 0.00x        → 0.00x
//	实测收费但接口给不出有效倍率      → 收费(倍率未知)
//	促销已过期                        → -（上游常把促销价固化进 credits，过期后不可信）
//	其余以接口倍率为准                → 0.29x 形式
//	接口无此模型且未探测              → -
func (c *Catalog) DisplayMultiplier(site, model string) string {
	if p, ok := c.ProbeOf(site, model); ok {
		switch p.Verdict {
		case "free":
			return "0.00x"
		case "paid":
			if e, ok2 := c.Entry(site, model); ok2 && e.HasMultiplier && !e.PromoExpired && e.Multiplier > 0 {
				return fmt.Sprintf("%.2fx", e.Multiplier)
			}
			return "收费(倍率未知)"
		}
	}
	e, ok := c.Entry(site, model)
	if !ok || !e.HasMultiplier {
		return "-"
	}
	if e.PromoExpired {
		return "-"
	}
	return fmt.Sprintf("%.2fx", e.Multiplier)
}

// KnownFree 判断某站点该模型是否已确认免费。
func (c *Catalog) KnownFree(site, model string) bool {
	if p, ok := c.ProbeOf(site, model); ok && p.Verdict == "free" {
		return true
	}
	e, ok := c.Entry(site, model)
	return ok && e.HasMultiplier && e.Multiplier == 0 && !e.PromoExpired
}

// KnownPaid 判断某站点该模型是否已确认收费。
func (c *Catalog) KnownPaid(site, model string) bool {
	if p, ok := c.ProbeOf(site, model); ok && p.Verdict == "paid" {
		return true
	}
	e, ok := c.Entry(site, model)
	return ok && e.HasMultiplier && e.Multiplier > 0
}

// PreferredFreeSites 返回应优先使用的免费站点集合。
//
// 只在「一个站点免费、另一个站点收费」时启用优先；两个站点都免费或都收费时不做倾斜，
// 保持正常轮询（否则会把流量无理由压到单一站点上）。
func (c *Catalog) PreferredFreeSites(model string) map[string]bool {
	var free, paid []string
	for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
		if c.KnownFree(site, model) {
			free = append(free, site)
		}
		if c.KnownPaid(site, model) {
			paid = append(paid, site)
		}
	}
	if len(free) == 0 || len(paid) == 0 {
		return nil
	}
	out := make(map[string]bool, len(free))
	for _, s := range free {
		out[s] = true
	}
	return out
}

// NeedsProbe 判断某 (站点, 模型) 是否需要价格探测。
//
// 只在两种情况下探测，避免无谓消耗额度：
//   - 该模型被实际请求过（说明用户真在用），但结论未知
//   - 接口给出 0 倍率却带已过期促销（上游把促销价固化了，必须实测才知道真实价格）
func (c *Catalog) NeedsProbe(site, model string, requests int64) bool {
	if p, ok := c.ProbeOf(site, model); ok {
		if p.Verdict == "" {
			return true
		}
		// 已确认免费的要定期复测：限免活动结束后价格会变
		if p.Verdict == "free" {
			return time.Now().Unix()-p.LastProbeAt > int64(ProbeInterval.Seconds())
		}
		return false
	}
	e, ok := c.Entry(site, model)
	if !ok || !e.FromLive {
		return requests > 0
	}
	return e.HasMultiplier && e.Multiplier == 0 && e.PromoExpired
}

// -----------------------------------------------------------------------------
// 刷新
// -----------------------------------------------------------------------------

// RefreshOnce 拉取两个站点的目录并合并落盘。
func (c *Catalog) RefreshOnce(ctx context.Context) error {
	sites := c.accts.Sites()
	if len(sites) == 0 {
		return errors.New("没有可用账号，无法拉取模型目录")
	}

	fetched := map[string][]Model{}
	includes := map[string]bool{}
	usedNPM := false
	var failures []string

	// 公共基座只取一次（多个站点声明 include 时复用），且是惰性的：
	// 没有站点声明 include 就一次网络请求都不发。
	var (
		commonBase    []Model
		commonErr     error
		commonFetched bool
	)

	for _, site := range sites {
		live, wantInclude, liveErr := c.fetchLive(ctx, site)
		if liveErr != nil {
			failures = append(failures, fmt.Sprintf("%s 实时接口: %v", auth.SiteLabel(site), liveErr))
			// 实时接口拿不到声明时，沿用上次学到的结论（见 cacheDoc.Includes）：
			// 目录退回 npm 单源已经少一截，不该再因为「这次没问到」而丢掉公共基座。
			wantInclude = c.siteWantsInclude(site)
		}
		includes[site] = wantInclude

		var npm []Model
		if c.NPMEnabled {
			var npmErr error
			npm, npmErr = FetchNPMCatalog(ctx, c.client, site)
			if npmErr != nil {
				failures = append(failures, fmt.Sprintf("%s npm 目录: %v", auth.SiteLabel(site), npmErr))
			} else {
				usedNPM = true
			}
		}
		// 本站声明了 include 时补上公共基座（基座只取一次，多个站点复用）。
		if wantInclude && !commonFetched {
			commonFetched = true
			if c.NPMEnabled {
				commonBase, commonErr = FetchNPMCommonCatalog(ctx, c.client)
				if commonErr == nil {
					usedNPM = true
				}
			} else {
				commonErr = errors.New("npm 目录已禁用，无法解析 include 基座")
			}
		}
		if wantInclude && commonErr != nil {
			failures = append(failures, fmt.Sprintf("%s 公共基座: %v", auth.SiteLabel(site), commonErr))
		}

		merged := mergeSiteCatalog(live, npm, commonBase, wantInclude)
		if wantInclude && commonErr == nil {
			if added := len(merged) - len(MergeCatalogs(live, npm)); added > 0 {
				c.logf("[目录] %s 合并公共基座，补入 %d 个模型", auth.SiteLabel(site), added)
			}
		}

		if len(merged) > 0 {
			fetched[site] = merged
		}
	}

	if len(fetched) == 0 {
		if len(failures) > 0 {
			return errors.New("两路来源均不可用：" + strings.Join(failures, "；"))
		}
		return errors.New("两路来源均无模型数据")
	}

	now := time.Now().Unix()
	source := "live-api"
	if usedNPM && len(fetched) > 0 {
		// 只要有一路用了 npm 兜底就标注，便于排查数据来源
		source = "live-api+npm"
	}
	if len(failures) > 0 {
		source += "(部分降级)"
	}

	c.mu.Lock()
	c.bySite = fetched
	c.source = source
	c.includes = includes
	for site := range fetched {
		c.fetchedAt[site] = now
	}
	c.mu.Unlock()

	c.saveCache()
	for _, f := range failures {
		c.logf("[目录] 降级: %s", f)
	}
	total := 0
	for _, ms := range fetched {
		total += len(ms)
	}
	c.logf("[目录] 已刷新，共 %d 个模型（来源=%s）", total, source)
	return nil
}

// mergeSiteCatalog 合并某站点的三路来源。
//
// 顺序即优先级（MergeCatalogs 是先到先得）：实时接口 > 本站 npm 覆盖层 > 公共基座。
//
// common 只在 wantInclude 时并入。这条判据不能放宽成「有基座就合」：
// 未声明 include 的站点（实测国内站）合并基座会把该站**不可用**的模型报成可用
// （gpt-6-astra / deepseek-v4.1-flash-sg 在国内站实测 11102 not found）。
// 少列一个模型只是列表不全，多列一个不可用的模型会让客户端选中后直接报错。
func mergeSiteCatalog(live, npm, common []Model, wantInclude bool) []Model {
	merged := MergeCatalogs(live, npm)
	if wantInclude {
		merged = MergeCatalogs(merged, common)
	}
	return merged
}

// siteWantsInclude 返回上次学到的「本站是否声明 include」（实时接口故障时沿用）。
func (c *Catalog) siteWantsInclude(site string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.includes[site]
}

// fetchLive 用该站点某个可用账号拉实时目录。
//
// 第二个返回值表示该站点是否声明了 include（需要合并公共基座）。
// 实时接口失败时为 false —— 拿不到声明就不猜，宁可少合并也不误报可用。
func (c *Catalog) fetchLive(ctx context.Context, site string) ([]Model, bool, error) {
	_, cred, prof, ok := c.accts.CredentialForSite(site)
	if !ok {
		return nil, false, fmt.Errorf("没有可用的%s账号", auth.SiteLabel(site))
	}
	live, err := c.client.FetchModelCatalog(ctx, cred, prof)
	if err != nil {
		return nil, false, err
	}
	return convertLive(live.Models), live.WantsInclude(), nil
}

// convertLive 把 upstream 的目录条目转成 catalog 的模型（跨包类型转换，避免导入环）。
func convertLive(in []upstream.LiveModel) []Model {
	out := make([]Model, 0, len(in))
	for _, m := range in {
		out = append(out, Model{
			ID:                   m.ID,
			Name:                 m.Name,
			Description:          m.Description,
			Credits:              m.Credits,
			BaseMultiplier:       m.BaseMultiplier,
			Multiplier:           m.Multiplier,
			HasMultiplier:        m.HasMultiplier,
			PromoLabel:           m.PromoLabel,
			PromoUntil:           m.PromoUntil,
			PromoFactor:          m.PromoFactor,
			PromoFree:            m.PromoFree,
			PromoExpired:         m.PromoExpired,
			FromLive:             true,
			MaxInputTokens:       m.MaxInputTokens,
			MaxOutputTokens:      m.MaxOutputTokens,
			MaxAllowedSize:       m.MaxAllowedSize,
			ContextWindowDefault: m.ContextWindowDefault,
			ContextWindowOptions: m.ContextWindowOptions,
			SupportsReasoning:    m.SupportsReasoning,
			OnlyReasoning:        m.OnlyReasoning,
			CanDisableThinking:   m.CanDisableThinking,
			ReasoningEffort:      m.ReasoningEffort,
			ReasoningSummary:     m.ReasoningSummary,
			SupportedEfforts:     m.SupportedEfforts,
			SupportsImages:       m.SupportsImages,
			SupportsToolCall:     m.SupportsToolCall,
			IsDefault:            m.IsDefault,
			Tags:                 m.Tags,
			Vendor:               m.Vendor,
			Temperature:          m.Temperature,
			RelatedModels:        m.RelatedModels,
		})
	}
	return out
}

// MergeCatalogs 合并两路目录：接口优先，按 ID 去重。
func MergeCatalogs(live, npm []Model) []Model {
	out := make([]Model, 0, len(live)+len(npm))
	seen := map[string]bool{}
	for _, m := range live {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	for _, m := range npm {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	return out
}

// RefreshLoop 周期刷新目录。
func (c *Catalog) RefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	// 启动后先拉一次
	_ = c.RefreshOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.RefreshOnce(ctx); err != nil {
				c.logf("[目录] 刷新失败: %v", err)
			}
		}
	}
}
