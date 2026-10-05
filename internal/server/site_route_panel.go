package server

import (
	"net/http"
	"sort"
	"strings"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/catalog"
	"workbuddy-gateway/internal/upstream"
)

// 本文件是「站点路由」的面板侧：把路由能力与用法透出给界面。
//
// 为什么需要它：站点路由有三种表达（URL 参数 / 请求头 / 模型名前缀），优先级也不同。
// 这些规则只写在文档里等于没有 —— 用户在面板里看到某个模型「国际站免费」，
// 想做的就是「把这一条贴到我的调用里」。让他自己去翻 README 找前缀写法，
// 中间就断了一环：他很可能随手写上 `?site=international`（能work）
// 或 `model=international/xxx`（不 work，因为前缀只认 intl/cn）。
//
// 所以这里把「当前网关实际接受哪些写法」由**同一份词表**算出来下发，
// 而不是在前端再抄一遍 —— 抄一遍就会随后端改词表而漂移，最后界面教错人。

// siteRouteOption 是一种可用的站点指定方式。
type siteRouteOption struct {
	// Kind 是机读标识：query / header / model_prefix。
	Kind string `json:"kind"`
	// Label 是给人看的一句话名称。
	Label string `json:"label"`
	// Priority 从 1 开始，1 最高。同一份来源用于排序与显示。
	Priority int `json:"priority"`
	// Syntax 是写法本身，前端可直接展示。
	Syntax string `json:"syntax"`
	// Example 是一个完整可用的例子（含真实模型名占位）。
	Example string `json:"example"`
	// Note 说明这种方式适合什么场景。
	Note string `json:"note"`
}

// siteAliasGroup 是一个站点可接受的写法集合。
type siteAliasGroup struct {
	Site string `json:"site"`
	// Label 是站点展示名（国内站 / 国际站）。
	Label string `json:"label"`
	// BaseURL / Origin 是该站点的上游地址，便于用户核对。
	BaseURL string `json:"base_url"`
	Origin  string `json:"origin"`
	// QueryAliases 是可写进 `?site=` 与请求头的别名。
	QueryAliases []string `json:"query_aliases"`
	// ModelPrefix 是唯一可用于模型名前缀的写法，**含分隔符**（如 `CN-`）。
	//
	// 刻意把分隔符一起下发而不是单发 `CN` 让前端自己拼：分隔符属于「词表」的一部分
	// （它决定了 `CN-deepseek` 能拆而 `CNdeepseek` 不能拆），前端拼错就会教出
	// 后端不认的写法。这正是这个接口存在的意义 —— 单一事实来源。
	ModelPrefix string `json:"model_prefix"`
}

// handlePanelSiteRoute 返回站点路由的用法说明。
//
// 刻意**把词表从后端算出来**而不是前端写死：`normalizeSiteQuery` 里那串别名
// （intl / international / global / ai / workbuddy.ai / …）是后端真正生效的那一份，
// 前端自己维护一份一定会漂移。反过来，前端如果只写「用 intl」，
// 用户就永远不知道 `workbuddy.ai` 也能写。
func (s *Server) handlePanelSiteRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}

	// 用一个真实模型名做例子，比 `xxx` 更能让人直接粘去试。
	//
	// 但**要挑一个像模型名的**：目录按字母序排的话第一个常常是 `auto`、
	// `default` 这类虚拟条目，拿它当例子会让人以为前缀要配这种词用。
	exampleModel := pickExampleModel(s.cat.Merged())

	options := []siteRouteOption{
		{
			Kind:     "model_prefix",
			Label:    "模型名前缀",
			Priority: 1,
			Syntax:   "<前缀>-<模型名>",
			// 前缀用后端词表里的值拼，不写字面量 —— 改词表时这里自动跟上。
			// prefixFor 返回的已经是**含分隔符**的完整前缀（如 `AI-`），
			// 这里绝不能再补一个 `-`，否则示例会变成 `AI--deepseek-v4.1-flash`（贴过去必失败）。
			Example: prefixFor(auth.SiteINTL) + exampleModel,
			Note: "最省事的一种：粘到任何支持 model 字段的工具里即可生效，不需要该工具支持自定义请求头。" +
				"前缀发往上游前会被剥掉（上游不认识它）。",
		},
		{
			Kind:     "query",
			Label:    "URL 参数",
			Priority: 2,
			Syntax:   "POST /v1/chat/completions?site=<站点>",
			Example:  "/v1/chat/completions?site=" + auth.SiteINTL,
			Note:     "单次请求粒度，改一个参数就能验证；优先级高于模型名前缀。",
		},
		{
			Kind:     "header",
			Label:    "请求头",
			Priority: 3,
			Syntax:   "X-WB-Site: <站点>",
			Example:  "X-WB-Site: " + auth.SiteCN,
			Note:     "适合「整个客户端都走某一站」。多个来源同时出现时按上面的优先级判定。",
		},
	}

	groups := make([]siteAliasGroup, 0, 2)
	for _, site := range []string{auth.SiteCN, auth.SiteINTL} {
		prof := upstream.ProfileForSite(site)
		groups = append(groups, siteAliasGroup{
			Site:         site,
			Label:        auth.SiteLabel(site),
			BaseURL:      prof.Base,
			Origin:       prof.Origin,
			QueryAliases: queryAliasesFor(site),
			ModelPrefix:  prefixFor(site),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"options": options,
		"sites":   groups,
		"notes": []string{
			"换号只在同一站内进行：国内站账号额度耗尽时只会改用同站的另一个国内账号，" +
				"不会为了把请求发出去而跳到国际站；反之亦然。",
			"指定站点后不会回落到另一站：该站没有可用账号时直接报错，" +
				"避免请求被悄悄发到会产生费用的另一侧。",
			"不指定时按默认调度：优先有可用账号、且该模型免费的一侧。",
			"写成 auto / default / any / none 可显式表示「这次用默认逻辑」。",
			"模型名本就含连字符（如 deepseek-v4.1-flash），前缀只认最前面那一段，" +
				"且必须是 CN- / AI- 才算数；其余一律当普通模型名原样转发。",
		},
	})
}

// pickExampleModel 从目录里挑一个适合做示例的模型名。
//
// 排除 `auto` / `default` 这类虚拟条目：它们在字母序里往往排在最前，
// 直接取第一个会让人以为「模型名前缀要配这种词用」，而这类名字恰恰不是真实模型。
// 挑不到合适的就回落到一个稳定的字面量，宁可示例不是当前目录里的，
// 也不要给一个会误导的例子。
func pickExampleModel(models []catalog.Model) string {
	const fallback = "deepseek-v4.1-flash"
	ids := make([]string, 0, len(models))
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" || isVirtualModelName(id) {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return fallback
	}
	sort.Strings(ids)
	return ids[0]
}

// isVirtualModelName 判断是否是「不是真模型」的调度占位名。
func isVirtualModelName(id string) bool {
	switch strings.ToLower(id) {
	case "auto", "default", "any", "none", "fast", "best":
		return true
	}
	return false
}

// sitePrefixSeparator 是模型名前缀与模型名之间的分隔符。
//
// 单独提出来是因为它被两处共用：写入词表（sitePrefixes 的键）与拼接面板示例。
// 两处各写一个字面量，改一处漏一处时症状是「面板给的示例贴过去不生效」。
const sitePrefixSeparator = "-"

// prefixFor 返回该站点用于模型名的**完整前缀（含分隔符）**，如 `CN-`。
//
// 含分隔符是刻意的：前端拿到就能直接展示、直接拼示例、直接复制，
// 不需要知道分隔符是什么。逆查而不是再写一张表 —— 两张表一定会有一张忘记改。
//
// **统一转大写**：词表的键是给小写匹配用的（`sitePrefixes` 的 key 全小写），
// 但下发给界面/文档的是**规范写法**。两者混用的症状是「面板教你写 `ai-xxx`，
// README 教你写 `AI-xxx`」，用户不知道哪个才算数（其实都能用，但没人愿意去验证）。
// 上游模型名全是小写，大写是网关为路由自造的一个不可能冲突的命名空间，
// 因此大写才是这个前缀的「本来面目」。
func prefixFor(site string) string {
	// 键按字典序取第一个命中的，保证同一站点的输出稳定（map 遍历顺序是随机的）。
	best := ""
	for key, s := range sitePrefixes {
		if s != site {
			continue
		}
		if best == "" || key < best {
			best = key
		}
	}
	if best == "" {
		return ""
	}
	return strings.ToUpper(best) + sitePrefixSeparator
}

// queryAliasesFor 列出 normalizeSiteQuery 实际接受该站点的全部写法。
//
// 同样走**真词表**而不是列一份常量：这里遍历的是归一化函数能认的值域，
// 因此只要 normalizeSiteQuery 改了，面板展示的别名就跟着变。
func queryAliasesFor(site string) []string {
	out := []string{}
	// 候选集必须覆盖 normalizeSiteQuery 里出现的所有字面量。
	// 漏掉一个的后果是「面板没教，但后端其实支持」—— 比反过来好，但仍会困惑用户。
	for _, candidate := range []string{
		"cn", "china", "domestic", "mainland",
		"intl", "international", "global", "ai", "workbuddy.ai", "codebuddy.ai", "overseas",
	} {
		if got, ok := normalizeSiteQuery(candidate); ok && got == site {
			out = append(out, candidate)
		}
	}
	sort.Strings(out)
	return out
}
