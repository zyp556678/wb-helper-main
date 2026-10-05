package server

import (
	"net/http"
	"strings"

	"workbuddy-gateway/internal/auth"
)

// 本文件是「站点路由」：让调用方能显式指定一个请求走国内站还是国际站。
//
// # 为什么需要它
//
// 两站（国内站 copilot.tencent.com / 国际站 www.workbuddy.ai）在上游是两套独立的
// 域名、凭据与计费，但在网关内部被归一成同一个账号池。默认调度只看「哪边有可用账号」
// 与「哪边该模型免费」，不区分用户的意图 —— 于是会出现「我想走国际站，它却给我发了
// 国内站」这种无法解释、也无法纠正的行为。
//
// # 判定优先级（高 → 低）
//
//  1. URL 查询参数 `?site=intl`
//  2. 请求头 `X-WB-Site: intl`
//  3. 模型名前缀 `CN-deepseek-v4.1-flash` / `AI-deepseek-v4.1-flash`
//
// 为什么把 URL 参数放最高：它是**单次请求**粒度、最贴近调用点的表达，改一个 query
// 就能验证，不用动调用方的全局配置。请求头适合「整个客户端都走国际站」的场景。
// 模型名前缀则是最省事的一种 —— 粘到任何支持 model 字段的工具里都能生效，
// 不需要该工具支持自定义 header。
//
// # 前缀会被剥离
//
// `AI-deepseek-v4.1-flash` 里的 `AI-` 只用于网关内部路由，**必须剥掉再发给上游** ——
// 上游不认识这个前缀，带上去会直接 404（模型不存在）。这一点很容易写漏，
// 表现为「一用前缀就全都报模型不存在」。
//
// # 换号只在同站内进行
//
// 一旦指定了站点，整个「换号重试」循环都受该站点约束：国内站的账号额度耗尽时，
// 只会改用**同站的另一个国内账号**，绝不会为了把请求发出去而跳到国际站。这条约束
// 由 dispatchRouted 每轮都给 PickAccount 传 Site 保证（见该函数的注释），
// 并有测试锁住 —— 它比「默认调度会倾向便宜的一侧」重要得多，因为跨站意味着
// 违背调用方的显式意图，且可能产生预期外的费用。

// sitePrefixes 是模型名前缀到站点的映射。
//
// 值用 auth.SiteCN / auth.SiteINTL 而不是字面量，避免以后改常量时这里漏改。
//
// # 为什么是 `CN-` / `AI-` 这种「大写 + 连字符」形态
//
// 前缀是**塞进模型名里**的，因此它的形状直接决定了碰撞面。上游模型名全是小写
// 且大量含连字符（`deepseek-v4.1-flash`、`glm-5.3-flashx`），所以：
//
//   - **大写**：上游没有任何大写字母的模型名，`CN-` / `AI-` 因此与真实命名空间
//     天然隔离。若用小写 `cn-`，一旦将来出现 `cn-something` 这类真模型就会被
//     永久劫持，而症状是「这个模型怎么也调不到」，很难联想到是路由前缀干的。
//   - **连字符分隔**：`AI-deepseek-v4-flash` 比 `AIdeepseek-v4-flash` 可读得多，
//     也与模型名自身的连字符风格一致。
//
// 匹配时统一转小写，因此调用方写 `cn-` / `Cn-` 也能用（宽容输入、只认一种输出）。
var sitePrefixes = map[string]string{
	"cn": auth.SiteCN,
	"ai": auth.SiteINTL,
}

// siteModelID 把真实模型名包成带站点头的 ID（`CN-deepseek-v4.1-flash`）。
//
// 与 splitSitePrefix **严格互逆**，这是它能安全出现在模型列表里的前提：
// 客户端从 `/v1/models` 拿到 ID 后原样发请求，网关必须能还原出同一个模型名。
// 前缀规则复用 prefixFor（它已从 sitePrefixes 逆查），**不另立一张表** ——
// 两张表一定会有一张忘记改，而症状是「列表给的 ID 贴过去调不通」。
func siteModelID(site, model string) string {
	p := prefixFor(site)
	if p == "" || model == "" {
		return model
	}
	return p + model
}

// siteRoute 是一次站点判定的结果。
type siteRoute struct {
	// Site 是目标站点；空串表示「未指定」，交回默认调度逻辑。
	Site string
	// Model 是**剥离前缀后**的真实模型名。
	Model string
	// Source 说明站点是从哪来的（query / header / model_prefix），供日志与面板展示。
	Source string
}

// Forced 表示调用方显式指定了站点。
//
// 显式指定时**不回落到另一边**：把请求悄悄发到另一个站等于违背用户的显式意图，
// 而他之所以指定，往往正是因为另一边会花钱或不被允许。宁可报错让他知道。
func (r siteRoute) Forced() bool { return r.Site != "" }

// routeSourceLabel 把来源标识翻成用户能看懂的话，用于错误提示。
//
// 报错时一定要带来源：用户常常同时用了前缀和请求头，看到「指定了国际站」却
// 想不起自己在哪儿指定的，就只能靠猜。
func routeSourceLabel(source string) string {
	switch source {
	case "query":
		return "URL 参数 ?site="
	case "header":
		return "请求头 X-WB-Site"
	case "model_prefix":
		return "模型名前缀"
	default:
		return "调用方指定"
	}
}

// resolveSiteRoute 按优先级解析目标站点并剥离模型名前缀。
//
// **前缀剥离是无条件的**，与站点从哪来无关：`?site=cn` + `AI-gpt-4` 这种组合下，
// 站点取 cn、模型名必须是 `gpt-4`。若只在「用前缀定站点」时才剥离，上游会收到
// 带前缀的模型名并回 404 —— 表现为「一用 ?site= 就报模型不存在」，很难联想到前缀。
//
// 返回的 Model 恒非空：调用方传入的 modelName 本身为空时保留为空，
// 不该在这里补默认值（补默认是 handleChatCompletions 的职责，它知道默认模型是谁）。
func resolveSiteRoute(r *http.Request, modelName string) siteRoute {
	// 先无条件剥离前缀，拿到真实模型名与「前缀里写的站点」。
	prefixSite, model, _ := splitSitePrefix(modelName)

	// 1) URL 参数
	if raw := r.URL.Query().Get("site"); strings.TrimSpace(raw) != "" {
		if site, ok := normalizeSiteQuery(raw); ok {
			return siteRoute{Site: site, Model: model, Source: "query"}
		}
	}
	// 2) 请求头
	if raw := r.Header.Get("X-WB-Site"); strings.TrimSpace(raw) != "" {
		if site, ok := normalizeSiteQuery(raw); ok {
			return siteRoute{Site: site, Model: model, Source: "header"}
		}
	}
	// 3) 模型名前缀
	if prefixSite != "" {
		return siteRoute{Site: prefixSite, Model: model, Source: "model_prefix"}
	}
	return siteRoute{Model: model}
}

// normalizeSiteQuery 归一化 `?site=` / 请求头里的站点写法。
//
// 词表刻意与 auth 的 normalize 口径对齐（它认得 intl / international / global /
// ai / workbuddy.ai / codebuddy.ai），这样「面板里叫国际站的那个东西」与
// 「这里能写的字符串」是同一套，不会出现两种说法。
//
// 返回 ok=false 表示**没听懂**：此时应当忽略这个来源而继续往下找，
// 而不是当成国内站 —— 把打错的 `?site=usa` 理解为「走国内」是最坏的结果。
//
// 不直接复用 auth.ResolveSite：它对未知值会**回退国内站**且不告诉你，
// 而我们恰恰要区分「认成 cn」与「没听懂」。
func normalizeSiteQuery(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", false
	// 显式列出「未指定」的写法，让调用方能主动表达「这次用默认逻辑」，
	// 覆盖掉可能存在的全局默认站点。
	case "auto", "default", "any", "none":
		return "", false
	case "cn", "china", "domestic", "mainland":
		return auth.SiteCN, true
	case "intl", "international", "global", "ai", "workbuddy.ai", "codebuddy.ai", "overseas":
		return auth.SiteINTL, true
	}
	return "", false
}

// splitSitePrefix 拆出模型名前缀形式的站点。
//
// 形状固定为 `<前缀>-<模型名>`，前缀只认 `cn` / `ai`（大小写不敏感）：
//   - `CN-deepseek-v4-flash` → cn + deepseek-v4-flash
//   - `AI-glm-5.3`           → ai + glm-5.3
//   - `cn-`                  → 不拆（前缀后为空，多半是用户写了一半）
//   - `deepseek-v4-flash`    → 不拆（`deepseek` 不是已知前缀）
//   - `CN-vendor/model`      → cn + vendor/model（模型名本身可含斜杠，如 openai/gpt-4）
//   - `CN_deepseek`          → 不拆（分隔符必须是 `-`，用 `_` 是在猜用户意图）
//   - `xCN-deepseek`         → 不拆（前缀必须在最前，不能出现在中间）
//
// # 为什么必须在第一个连字符处切、且前缀段要完全相等
//
// 模型名里到处都是连字符。若规则放宽成「以 cn/ai 开头就算前缀」，那么
// `cndeepseek-v4-flash`（无分隔符）会被当成 `deepseek-v4-flash` —— 用户少打一个
// 连字符，请求却"看起来成功"，只是打到了预期之外的站点。宁可当时就报模型不存在，
// 也不要静默走错站：走错站是要花钱的。
//
// 同理，前缀段必须与词表的键**完全相等**，不能用 HasPrefix —— 否则
// `cn-something` 会先被认成站点前缀、再把 `something` 当模型名发出去。
func splitSitePrefix(modelName string) (string, string, bool) {
	idx := strings.Index(modelName, sitePrefixSeparator)
	// idx <= 0：没有分隔符，或分隔符在开头（如 `-glm`），都不构成前缀形态。
	// idx == len-1：分隔符在末尾（如 `CN-`），前缀后没有模型名。
	if idx <= 0 || idx == len(modelName)-1 {
		return "", modelName, false
	}
	prefix := strings.ToLower(strings.TrimSpace(modelName[:idx]))
	site, ok := sitePrefixes[prefix]
	if !ok {
		return "", modelName, false
	}
	rest := strings.TrimSpace(modelName[idx+len(sitePrefixSeparator):])
	if rest == "" {
		return "", modelName, false
	}
	return site, rest, true
}
