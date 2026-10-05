package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/upstream"
)

// 本文件是切片 8「双站与交付」的服务端部分：
//   - preferredSitesFor：把「免费站点优先」的判定接进调度路径
//   - handlePanelSites：双站视图的聚合接口
//
// 站位说明：两站（国内站 / 国际站）在上游是两套独立的域名、凭据与计费，
// 但在网关内部被归一成同一个账号池。所以「双站」不是一个开关，而是一份**对照视图**：
// 让用户看清每一侧各自有多少账号、覆盖多少模型、价格结论如何，以及调度当前在往哪边倾斜。

// preferredSitesFor 返回该模型应优先使用的站点集合；无需倾斜时返回 nil。
//
// 判定规则只有一条：**一侧确认免费、另一侧确认收费**时才倾斜。
// 两站都免费、都收费、或有一侧结论未知，都不做任何倾斜 —— 未知就倾斜等于拿猜测做决策，
// 而把流量压到实际收费的一侧是要花钱的错。目录层的 PreferredFreeSites 已经实现这条口径。
func (s *Server) preferredSitesFor(model string) map[string]bool {
	if s.cat == nil || model == "" {
		return nil
	}
	if !s.config().PreferFreeSiteEnabled() {
		return nil
	}
	return s.cat.PreferredFreeSites(model)
}

// paidSitesFor 返回该模型「已确认收费」的站点集合，供积分保底使用；无结论时返回 nil。
//
// 只收**已确认收费**（tier 2）的站点，这是积分保底能被安全启用的前提：
// 免费（tier 0）与无观测（tier 1）的站点必须不受限，否则账本被清空
// （重启 / 缓存过期）后，触底账号会被永久锁在「学不回来」的死锁里 ——
// 它拿不到任何请求，就永远没有机会证明这个模型其实是免费的。
func (s *Server) paidSitesFor(model string) map[string]bool {
	if s.cat == nil || model == "" {
		return nil
	}
	out := map[string]bool{}
	for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
		if s.cat.KnownPaid(site, model) {
			out[site] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// siteLabels 把站点集合拼成人类可读的标签列表（排序后拼接，保证报错文案稳定）。
func siteLabels(sites map[string]bool) string {
	labels := make([]string, 0, len(sites))
	for s := range sites {
		labels = append(labels, auth.SiteLabel(s))
	}
	sort.Strings(labels)
	return strings.Join(labels, "、")
}

// siteAccountStat 是单个站点的账号汇总。
type siteAccountStat struct {
	Total            int     `json:"total"`
	Active           int     `json:"active"`
	Cooldown         int     `json:"cooldown"`
	Disabled         int     `json:"disabled"`
	InFlight         int64   `json:"in_flight"`
	CreditsRemaining float64 `json:"credits_remaining"`
	QuotaKnown       int     `json:"quota_known"`
}

// sitePaidModel 是收费模型在该站点上的展示项。
type sitePaidModel struct {
	ID              string  `json:"id"`
	MultiplierLabel string  `json:"multiplier_label"`
	Multiplier      float64 `json:"multiplier"`
}

// siteCatalogStat 是该站点的目录覆盖情况。
type siteCatalogStat struct {
	Models    int    `json:"models"`
	FetchedAt int64  `json:"fetched_at"`
	Source    string `json:"source"`
}

// siteProbeStat 是该站点已获得的探测结论分布。
type siteProbeStat struct {
	Free        int   `json:"free"`
	Paid        int   `json:"paid"`
	Unknown     int   `json:"unknown"`
	LastProbeAt int64 `json:"last_probe_at"`
}

// siteView 是双站视图里的单个站点。
type siteView struct {
	Site    string `json:"site"`
	Label   string `json:"label"`
	BaseURL string `json:"base_url"`
	Origin  string `json:"origin"`
	// Usable 表示该站点当前有「未禁用且带令牌」的账号，即请求真能打到它。
	Usable   bool            `json:"usable"`
	Accounts siteAccountStat `json:"accounts"`
	// AccountIDs 是归属该站点的账号标识（与账号页的 id 一致，便于跳转排查）。
	AccountIDs []string        `json:"account_ids"`
	Catalog    siteCatalogStat `json:"catalog"`
	// Pricing.Free 是「目录或探测明确免费」的模型数；Unknown 是既非免费也非收费。
	Pricing struct {
		Free int `json:"free"`
		Paid int `json:"paid"`
		// Unknown 计数与 Free/Paid 之和可以小于 Models —— 因为目录里存在
		// 既无倍率也无探测结论的模型（上游没给 credits）。
		Unknown int `json:"unknown"`
		// ConfirmedFree 列出明确免费的模型 ID，这是「免费站点优先」真正会用到的集合。
		ConfirmedFree []string        `json:"confirmed_free"`
		PaidModels    []sitePaidModel `json:"paid_models"`
		Probe         siteProbeStat   `json:"probe"`
	} `json:"pricing"`
	Note string `json:"note"`
}

// preferenceRule 是一条当前生效（或可生效）的倾斜规则。
type preferenceRule struct {
	Model     string   `json:"model"`
	Preferred []string `json:"preferred"`
	Avoid     []string `json:"avoid"`
	Reason    string   `json:"reason"`
	Requests  int64    `json:"requests"`
}

// handlePanelSites 返回双站对照视图。
func (s *Server) handlePanelSites(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}

	enabled := s.config().PreferFreeSiteEnabled()
	source, _ := s.cat.Source()

	// ---- 账号按站点聚合 ----
	type accAgg struct {
		stat siteAccountStat
		ids  []string
	}
	bySite := map[string]*accAgg{
		auth.SiteCN:   {stat: siteAccountStat{}},
		auth.SiteINTL: {stat: siteAccountStat{}},
	}
	now := time.Now().Unix()
	for _, a := range s.pool.Snapshot() {
		agg := bySite[a.Site]
		if agg == nil {
			agg = &accAgg{}
			bySite[a.Site] = agg
		}
		agg.stat.Total++
		switch {
		case a.Disabled:
			agg.stat.Disabled++
		case a.CooldownUntil > now:
			agg.stat.Cooldown++
		default:
			agg.stat.Active++
		}
		agg.stat.InFlight += a.InFlight
		if a.Quota != nil {
			agg.stat.QuotaKnown++
			agg.stat.CreditsRemaining += a.Quota.Remaining
		}
		agg.ids = append(agg.ids, a.ID)
	}

	// ---- 逐站点组装 ----
	order := []string{auth.SiteCN, auth.SiteINTL}
	sites := make([]siteView, 0, len(order))
	for _, site := range order {
		agg := bySite[site]
		if agg == nil {
			agg = &accAgg{}
		}
		sort.Strings(agg.ids)

		prof := upstream.ProfileForSite(site)
		view := siteView{
			Site:       site,
			Label:      auth.SiteLabel(site),
			BaseURL:    prof.Base,
			Origin:     prof.Origin,
			Accounts:   agg.stat,
			AccountIDs: agg.ids,
		}
		if view.AccountIDs == nil {
			view.AccountIDs = []string{}
		}
		view.Usable = agg.stat.Total-agg.stat.Disabled > 0

		models := s.cat.ModelsForSite(site)
		view.Catalog = siteCatalogStat{
			Models:    len(models),
			FetchedAt: s.cat.FetchedAtSite(site),
			Source:    source,
		}
		view.Pricing.ConfirmedFree = []string{}
		view.Pricing.PaidModels = []sitePaidModel{}

		for _, m := range models {
			free := s.cat.KnownFree(site, m.ID)
			paid := s.cat.KnownPaid(site, m.ID)
			switch {
			case free:
				view.Pricing.Free++
				view.Pricing.ConfirmedFree = append(view.Pricing.ConfirmedFree, m.ID)
			case paid:
				view.Pricing.Paid++
				label := s.cat.DisplayMultiplier(site, m.ID)
				view.Pricing.PaidModels = append(view.Pricing.PaidModels, sitePaidModel{
					ID:              m.ID,
					MultiplierLabel: label,
					Multiplier:      m.Multiplier,
				})
			default:
				view.Pricing.Unknown++
			}
		}
		sort.Slice(view.Pricing.PaidModels, func(i, j int) bool {
			if view.Pricing.PaidModels[i].Multiplier != view.Pricing.PaidModels[j].Multiplier {
				return view.Pricing.PaidModels[i].Multiplier > view.Pricing.PaidModels[j].Multiplier
			}
			return view.Pricing.PaidModels[i].ID < view.Pricing.PaidModels[j].ID
		})
		// 收费模型可能几十个，全量透出会让页面变成一张长表；截断到 40 并保留倍率最高的那些。
		if len(view.Pricing.PaidModels) > 40 {
			view.Pricing.PaidModels = view.Pricing.PaidModels[:40]
		}

		f, p, u, last := s.cat.ProbeSummarySite(site)
		view.Pricing.Probe = siteProbeStat{Free: f, Paid: p, Unknown: u, LastProbeAt: last}

		// 站点备注：把「为什么这一侧是空的」直接说清楚，省掉用户的排查。
		switch {
		case view.Catalog.Models == 0 && view.Accounts.Total == 0:
			view.Note = "该站点没有账号，也没有目录数据。用「账号」页添加一个" + view.Label + "账号后即可启用。"
		case view.Catalog.Models == 0:
			view.Note = "该站点有账号但拉不到目录（可能网络受限或凭据已失效），模型与价格结论均未知。"
		default:
			view.Note = ""
		}
		sites = append(sites, view)
	}

	// ---- 倾斜规则：只报有真实流量的模型，避免把整张目录铺开 ----
	rules := []preferenceRule{}
	for _, m := range s.cat.Merged() {
		reqs := int64(0)
		if s.metrics != nil {
			reqs = s.metrics.RequestsFor(m.ID)
		}
		if reqs <= 0 {
			continue
		}
		pref := s.cat.PreferredFreeSites(m.ID)
		if len(pref) == 0 {
			continue
		}
		rule := preferenceRule{Model: m.ID, Requests: reqs}
		for _, site := range order {
			if pref[site] {
				rule.Preferred = append(rule.Preferred, auth.SiteLabel(site))
				continue
			}
			// 只在「另一侧确实确认收费」时才写进 avoid，未知的不写——
			// 否则界面会暗示「这一侧不该用」，而实际只是还没探测过。
			if s.cat.KnownPaid(site, m.ID) {
				rule.Avoid = append(rule.Avoid, auth.SiteLabel(site))
			}
		}
		if rule.Preferred == nil {
			rule.Preferred = []string{}
		}
		if rule.Avoid == nil {
			rule.Avoid = []string{}
		}
		rule.Reason = "优先站点的该模型为免费（或限免），另一侧为收费"
		rules = append(rules, rule)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Model < rules[j].Model })

	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at":     time.Now().Unix(),
		"prefer_free_site": enabled,
		"sites":            sites,
		"preference": map[string]any{
			"enabled": enabled,
			"rules":   rules,
			"note": "倾斜仅在「一侧确认免费、另一侧确认收费」时生效；" +
				"两站都免费、都收费或有一侧结论未知时都不倾斜。优先站点不可用时自动回落到全部站点",
		},
	})
}
