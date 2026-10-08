package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/debuglog"
	"workbuddy-gateway/internal/eventlog"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/session"
	"workbuddy-gateway/internal/stats"
	"workbuddy-gateway/internal/upstream"
)

// handleChatCompletions 处理 OpenAI 兼容的对话补全。
//
// 协议行为（以 wb-gateway 为基准）：
//   - model 完全透传（黑名单可选拦截）
//   - 出站强制 stream:true（上游要求，非流式会被 11101 拒绝）
//   - 首条消息必须是 system，否则补一条保底 system（避免 11128）
//   - 流被中断时不伪造 [DONE]，而是下发明确的错误事件
//
// 切片 2 叠加的治理行为：
//   - 三因子加权选号 + 会话粘性 + 在途租约
//   - 错误统一分类后按类别处置（冷却 / 熔断 / 降权 / 不罚账号）
//   - 请求级换号重试，并记录每模型指标
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}

	started := time.Now()
	// 请求级 trace：同一次请求产生的所有日志事件（出站改写、拦截重试、完成统计）
	// 共用它，日志页才能把一条请求的来龙去脉串起来。
	trace := randomID()[:8]
	r = withTrace(r, trace)

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "读取请求体失败")
		return
	}
	_ = r.Body.Close()

	var reqObj map[string]any
	if err := json.Unmarshal(bodyBytes, &reqObj); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
		return
	}

	modelName, _ := reqObj["model"].(string)
	if strings.TrimSpace(modelName) == "" {
		modelName = "deepseek-v4.1-flash"
		reqObj["model"] = modelName
	}

	// 站点路由：调用方可用 `?site=` / `X-WB-Site` / 模型名前缀（cn/、intl/）指定走哪一站。
	// 必须在**出站改写之前**完成前缀剥离 —— 上游不认识 `intl/` 这个前缀，
	// 带上去会直接回「模型不存在」。同时也要在写回 reqObj 之后再剥，
	// 否则发给上游的 body 里仍带着前缀。
	route := resolveSiteRoute(r, modelName)
	if route.Model != modelName {
		modelName = route.Model
		reqObj["model"] = modelName
	}

	wantStream, _ := reqObj["stream"].(bool)

	if disabled, reason := s.config().ModelDisabled(modelName); disabled {
		debuglog.Event(r, "warn", "model_blocked_by_config", map[string]any{
			"model": modelName, "reason": reason, "status_code": http.StatusForbidden,
		})
		s.logf("[请求被拒绝] 模型=%s 原因=%s", modelName, reason)
		writeOpenAIError(w, http.StatusForbidden, "model_disabled", reason)
		return
	}

	// 出站改写：提示词模式、首条保底 system、强制 stream、参数形态归一、
	// 工具序列自愈、思维链开关与档位降级、指纹脱敏，全部走同一条管线。
	rawBody, err := json.Marshal(reqObj)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "encode_error", "序列化请求失败")
		return
	}
	upstreamBytes, prep := s.prepareOutboundBody(rawBody, false)
	s.logOutbound(modelName, "", trace, prep)

	// 会话粘性：同一会话尽量复用同一账号，保证多轮对话不跳号
	sessionKey := session.Key(reqObj)
	pinID := s.sticky.Lookup(sessionKey)

	s.metrics.RecordRequest(modelName)

	// 降级重试：内容策略拦截时换中性提示词再发一次。
	// 注意这里必须走 dispatchBody 并把「换 body」的能力传进去——
	// 用 dispatch 会传 nil，看起来一样能跑，但重试分支永远不会触发。
	var degradedBody []byte
	resp, acc, release, ok := s.dispatchRouted(w, r, modelName, upstreamBytes, pinID, route, func() []byte {
		if degradedBody != nil {
			return degradedBody
		}
		if b, _ := s.prepareOutboundBody(rawBody, true); len(b) > 0 {
			degradedBody = b
			return b
		}
		return nil
	})
	if !ok {
		return
	}
	defer release()

	if wantStream {
		s.streamChatResponse(w, r, resp, modelName, acc, sessionKey, started)
	} else {
		s.aggregateChatResponse(w, r, resp, modelName, acc, sessionKey, started)
	}
}

// dispatch 完成「选号 → 令牌校验 → 转发 → 错误处置 → 换号重试」的调度循环。
// 成功时返回上游响应（调用方负责关闭 Body）、命中的账号与在途释放函数。
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, modelName string, upstreamBytes []byte, pinID string) (*http.Response, *pool.Account, func(), bool) {
	return s.dispatchBody(w, r, modelName, upstreamBytes, pinID, nil)
}

// bodyFor 在重试时提供一份新的出站请求体（内容拦截降级重试靠它换提示词）。
type bodyFor func() []byte

// dispatchBody 是 dispatch 的完整形态：extraBody 非空时，重试会先向它取新 body。
//
// 不带站点信息，等价于「未指定站点」。保留它是为了让调用方（如 /v1/responses 的
// 分支、以及测试）不必构造一个空的 siteRoute。
func (s *Server) dispatchBody(w http.ResponseWriter, r *http.Request, modelName string, upstreamBytes []byte, pinID string, extraBody bodyFor) (*http.Response, *pool.Account, func(), bool) {
	return s.dispatchRouted(w, r, modelName, upstreamBytes, pinID, siteRoute{Model: modelName}, extraBody)
}

// dispatchRouted 在 dispatchBody 基础上接收站点路由结果。
//
// 站点在这里生效的方式是给 PickAccount 传 `Site`（硬过滤，见 pool.PickOptions）。
// 强制站点时**不做跨站回落**：把请求悄悄发到另一站等于违背调用方的显式意图，
// 而他指定站点往往正是因为另一边会花钱或不被允许。宁可明确报错。
func (s *Server) dispatchRouted(w http.ResponseWriter, r *http.Request, modelName string, upstreamBytes []byte, pinID string, route siteRoute, extraBody bodyFor) (*http.Response, *pool.Account, func(), bool) {
	total := s.pool.Len()
	if total == 0 {
		s.writeAPIError(w, r, http.StatusUnauthorized, "no_auth",
			"未找到有效登录凭据：请把凭据文件放到工作目录（自动发现 workbuddy*.json），或用 -auth / -auth-dir 指定")
		return nil, nil, nil, false
	}

	// 强制站点时先查一次「这一站到底有没有账号」，好在选号失败时给出能直接行动的报错。
	// 不查的话用户只会看到「全部账号均不可用」，而真实原因可能是「你指定了国际站，
	// 但池子里一个国际站账号都没有」—— 这两者的处置方式完全不同。
	if route.Forced() && s.pool.AccountsForSiteCount(route.Site) == 0 {
		label := auth.SiteLabel(route.Site)
		s.metrics.RecordFailure(modelName, "站点无可用账号")
		s.writeAPIError(w, r, http.StatusServiceUnavailable, "no_account_for_site",
			fmt.Sprintf("请求指定了%s（来源：%s），但账号池里没有该站点的账号。"+
				"请在「账号」页添加一个%s账号，或去掉站点指定改用默认调度。",
				label, routeSourceLabel(route.Source), label))
		return nil, nil, nil, false
	}

	// 粘性指定的账号：已不存在则当作未指定
	var pin *pool.Account
	if pinID != "" {
		if a, err := s.pool.Find(pinID); err == nil {
			pin = a
		}
	}

	exclude := make(map[*pool.Account]bool, total)
	var lastErr string
	degradedApplied := false
	// maxTries 可以超过账号数：降级重试是「同一个账号再发一次」，
	// 不该占用换号次数（否则只有单账号时重试永远排不进来，表现为「重试逻辑存在但从不触发」）。
	maxTries := total
	// 免费站点优先：只在「一侧确认免费、另一侧确认收费」时给出倾斜集合，其余情况返回 nil。
	// 整个请求只算一次，避免每轮重试都去读一次目录。
	//
	// 强制站点时不再叠加软优先：软优先的语义是「两个站里挑便宜的」，而用户已经
	// 明确说了要走哪一站 —— 此时再拿价格去覆盖他的选择是自相矛盾的。
	preferSites := map[string]bool(nil)
	if !route.Forced() {
		preferSites = s.preferredSitesFor(modelName)
	}

	for tried := 0; tried < maxTries; tried++ {
		acc, err := s.pool.PickAccount(pool.PickOptions{
			Exclude: exclude, Model: modelName, Pin: pin,
			Site: route.Site, PreferredSites: preferSites,
			PaidSitesForModel: s.paidSitesFor(modelName),
		})
		pin = nil // 粘性只对第一次尝试生效
		if err != nil {
			if tried == 0 {
				s.metrics.RecordFailure(modelName, "选号失败")
				// 先区分「名单把账号全挡了」与「账号都在冷却」。
				//
				// 两者都表现为「选不到号」，但处置完全相反：前者是配置问题，
				// 重试一万次也一样，必须报 403 并说清原因；后者是临时状态，
				// 报 503 让客户端稍后重试才对。只按健康候选数判断无法区分 ——
				// 名单挡掉全部账号时健康候选数同样是 0。
				if s.pool.ModelAccountRuleConfigured(modelName) && s.pool.AllowedAccountsForModel(modelName) == 0 {
					msg := fmt.Sprintf("模型 %s 没有可用的凭据文件：已被 models.accounts 账号名单全部排除", modelName)
					debuglog.Event(r, "warn", "model_account_blocked_by_config", map[string]any{
						"model": modelName, "status_code": http.StatusForbidden,
					})
					s.writeAPIError(w, r, http.StatusForbidden, "model_account_disabled", msg)
					return nil, nil, nil, false
				}
				// 再区分「被积分保底拦住」。这也是一种**配置决定**而非故障：
				// 账号余额还有几十，但低于你设的保底阈值，于是收费模型被拦住 ——
				// 不说清的话用户会去怀疑网络或凭据。
				if paid := s.paidSitesFor(modelName); len(paid) > 0 {
					if n := s.pool.AccountsBlockedByCreditFloor(paid, modelName); n > 0 {
						msg := fmt.Sprintf(
							"模型 %s 在%s上为实测收费，而池内 %d 个账号的余额低于积分保底阈值 %.0f，"+
								"已按保底策略拦下（避免收费请求打穿余额、连免费模型一起被冷却）。"+
								"可在配置页调低 pool.credit_floor，或先签到/刷新余额",
							modelName, siteLabels(paid), n, s.pool.CreditFloor())
						s.writeAPIError(w, r, http.StatusServiceUnavailable, "credit_floor_blocked", msg)
						return nil, nil, nil, false
					}
				}
				msg := err.Error()
				if route.Forced() {
					msg += fmt.Sprintf("（已限定%s，不会自动改用另一站）", auth.SiteLabel(route.Site))
				}
				if lastErr != "" {
					msg += " | 最近一次失败: " + lastErr
				}
				s.writeAPIError(w, r, http.StatusServiceUnavailable, "no_available_account", msg)
				return nil, nil, nil, false
			}
			break
		}
		exclude[acc] = true

		resp, err := s.tryUpstream(r, acc, upstreamBytes)
		if err != nil {
			acc.MarkFailure(err.Error())
			lastErr = err.Error()
			acc.ReleaseInFlight()
			var ue *upstreamError
			if errors.As(err, &ue) {
				// 上游超时 / 停滞：不换号、不罚号。超时不是账号的问题——同一份请求
				// 换到别的号撞上的是同一个慢上游，换号只会把客户端拖到
				// maxTries × header_timeout，期间还给一串健康号喂失败计数。
				if ue.timeout {
					s.metrics.RecordFailure(modelName, "上游超时")
					s.logf("[超时] 模型 %s 账号 %s 上游超时，已停止轮换（不处罚该账号）: %v",
						modelName, acc.Cred.AccountID(), ue.err)
					s.writeAPIError(w, r, http.StatusServiceUnavailable, "upstream_timeout",
						"上游超时，已停止换号重试（换号撞的是同一个慢上游），请稍后重试："+ue.err.Error())
					return nil, nil, nil, false
				}
				s.applyClass(acc, modelName, ue.class)
			}
			continue
		}

		if resp.StatusCode >= 400 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			class := upstream.Classify(resp.StatusCode, string(errBody))
			lastErr = fmt.Sprintf("HTTP %d %s", resp.StatusCode, class.Message)
			s.logf("[%s] 账号 %s 上游返回 %d（分类=%s）",
				modelName, acc.Cred.AccountID(), resp.StatusCode, class.Kind)

			// 内容策略拦截：判定为提示词指纹误报，换中性提示词就地重试一次。
			// 刻意「不换号」——同一请求换号也会再撞同一个审核，
			// 真正起作用的是把 system 侧的指纹面收掉。
			if class.Kind == upstream.ErrContentBlock && !degradedApplied &&
				s.config().PromptDegradedRetry() && extraBody != nil {
				degradedApplied = true
				if newBody := extraBody(); len(newBody) > 0 {
					upstreamBytes = newBody
					s.logf("[%s] 内容策略拦截，已换中性提示词重试（账号 %s）", modelName, acc.Cred.AccountID())
					s.events.Warn(eventlog.ChannelOutbound, "content_block_degraded_retry",
						"请求被内容策略拦截，已换中性提示词就地重试一次",
						map[string]any{"trace_id": traceOf(r), "model": modelName,
							"account": acc.Cred.AccountID(), "detail": class.Message})
					delete(exclude, acc) // 放回候选，允许同号重试
					maxTries++
					acc.ReleaseInFlight()
					continue
				}
			}

			// 请求体畸形（11101 / Unmarshal chat params failed）：同一 body 换任何账号都是
			// 同样的解析结果 —— 那是上游解析请求体阶段的拒绝，还没走到模型路由，
			// 轮转只会放大无效请求（每号一次上游调用 + 轮转退避占着在途名额），
			// 而拖到末端还会落进「全部账号不可用」的 503，把必然失败的请求伪装成
			// 「稍后重试」的临时故障，客户端于是无限重试。
			// 立即 400 透传上游原文并终止轮转。
			// 注意 11102「该后端无此模型」**不在此列**：那是 (账号, 模型) 负缓存避让，
			// 正是「换号可能有不同模型权限」的适用场景（见 applyClass 的 ErrModelBlocked）。
			if class.Kind == upstream.ErrBadRequest {
				s.applyClass(acc, modelName, class)
				acc.ReleaseInFlight()
				s.metrics.RecordFailure(modelName, "请求体被上游拒绝")
				msg := strings.TrimSpace(string(errBody))
				if msg == "" {
					msg = "上游拒绝该请求体（参数不合法），换号重试无意义"
				}
				s.logf("[%s] 账号 %s 上游拒绝请求体（11101），已终止轮换：%s",
					modelName, acc.Cred.AccountID(), class.Message)
				s.writeAPIError(w, r, http.StatusBadRequest, "bad_params", msg)
				return nil, nil, nil, false
			}

			s.applyClass(acc, modelName, class)
			acc.ReleaseInFlight()
			continue
		}

		// 成功：**不在这里记账号成功** —— 上游「200 已开流 + 一帧 error」是真实形态
		// （6004 限流 / 内容拦截 / 审核），在读第一帧之前就 MarkSuccess 会把一个正在
		// 限流的号记成健康号（清零连败计数）。成功判定延后到各响应函数读完流之后
		// （见 markHopSuccess），失败帧在那里按分类处置账号。
		return resp, acc, func() { acc.ReleaseInFlight() }, true
	}

	s.metrics.RecordFailure(modelName, "全部账号尝试失败")
	// 全池不可用时没有可归属的账号，账号维度留空（addToBucket 会跳过空账号）
	s.stats.Record(stats.RecordInput{Model: modelName, OK: false})
	// 这里不填耗时：全池都不可用时的耗时主要由「逐个换号重试」构成，
	// 把它当请求耗时会误导（看起来像是上游慢），真正的失败原因在 lastErr 里。
	s.logRequest(r, modelName, "", "chat", 0, 0, 0, 0, true)
	s.writeAPIError(w, r, http.StatusServiceUnavailable, "no_available_account",
		"全部账号均不可用 | 最近一次失败: "+lastErr)
	return nil, nil, nil, false
}

// upstreamError 承载「网络层 / 前置校验失败」的分类结果。
type upstreamError struct {
	err   error
	class upstream.ErrClass
	// timeout 标记「上游超时 / 停滞」：调用方据此止损（不轮转、不罚号），
	// 判定见 upstream.IsUpstreamTimeout 的三态。
	timeout bool
}

func (e *upstreamError) Error() string { return e.err.Error() }
func (e *upstreamError) Unwrap() error { return e.err }

// tryUpstream 向指定账号发起一次上游请求。
func (s *Server) tryUpstream(r *http.Request, acc *pool.Account, body []byte) (*http.Response, error) {
	if err := s.pool.EnsureToken(r.Context(), acc); err != nil {
		return nil, &upstreamError{err: err, class: upstream.ErrClass{Kind: upstream.ErrSessionDead}}
	}
	if acc.IsDisabled() {
		return nil, &upstreamError{
			err:   errors.New("账号已失效"),
			class: upstream.ErrClass{Kind: upstream.ErrSessionDead},
		}
	}

	prof := acc.Profile()
	ctx, cancel := context.WithCancel(r.Context())

	// 同账号的上游请求严格串行（在途租约之上再加一层账号锁），避免并发双发触发风控
	view := acc.View()
	acc.Lock()
	resp, err := s.client.Chat(ctx, view, prof, body)
	acc.Unlock()

	if err != nil {
		cancel()
		// 客户端已断连（r.Context() 已取消）时，错误里的 context.Canceled 是我们自己
		// 造成的，不算上游超时 —— 交给 IsUpstreamTimeout 的第三态判定。
		return nil, &upstreamError{
			err:     err,
			class:   upstream.ErrClass{Kind: upstream.ErrNetwork},
			timeout: upstream.IsUpstreamTimeout(err, r.Context().Err() != nil),
		}
	}
	// 上下文生命周期与响应体绑定：Body 关闭时才取消，
	// 保证流式读取期间上下文不被提前回收（否则长流会被自己掐断）。
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// applyClass 按错误分类处置账号。
//
// 注意「不罚账号」的两类：内容拦截与请求体畸形都是请求侧问题，
// 换号也会再撞同一个错误，罚了只会白白缩小可用池。
func (s *Server) applyClass(acc *pool.Account, modelName string, class upstream.ErrClass) {
	g := s.pool.Government()
	switch class.Kind {
	case upstream.ErrHardCredit:
		acc.MarkFailure("额度耗尽")
		acc.CooldownHard("额度耗尽，冷却至次日 04:00（签到或额度恢复后自动解冻）")
	case upstream.ErrSessionDead:
		acc.MarkFailure("session 失效")
		if acc.NoteSessionDead() {
			s.logf("[治理] 账号 %s 连续 %d 次 session 失效，已自动禁用",
				acc.Cred.AccountID(), pool.SessionDeadThreshold)
		} else {
			acc.CooldownFixed(60*time.Second, "session 失效，短冷却后重试")
		}
	case upstream.ErrModelRate:
		acc.MarkFailure("模型级限流")
		acc.CooldownModel(modelName, g.SoftRate, class.ResetAt, "模型级限流（6004），只冷却该模型")
		s.logf("[治理] 账号 %s 的模型 %s 进入模型级冷却（切换其他模型立即可用）",
			acc.Cred.AccountID(), modelName)
	case upstream.ErrSoftRate:
		acc.MarkFailure("上游限流")
		acc.CooldownSoft(g.SoftRate, class.ResetAt, "触发上游频率限制")
	case upstream.ErrUpstream5xx:
		acc.MarkFailure("上游服务端错误")
		acc.NoteBreaker()
	case upstream.ErrContentBlock, upstream.ErrBadRequest:
		acc.MarkFailure(class.Kind) // 请求侧问题：只记录，不处罚
	case upstream.ErrNetwork:
		acc.MarkFailure("网络错误")
		acc.NoteDegrade()
	default:
		acc.MarkFailure("上游未知错误")
		acc.NoteDegrade()
	}
}

// truncateForLog 截断日志里的长文本（上游原文可能很长，日志只需要可辨认的前缀）。
func truncateForLog(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…（已截断）"
}

// markHopSuccess 记录「这一跳真的成功了」：账号侧记成功并清零 session 失效计数。
//
// 必须等到读完上游响应、确认它没有以 error 帧报错之后才调用 —— 上游「200 已开流 +
// 一帧 error」（6004 限流 / 内容拦截 / 审核）是真实形态，在开流时记成功会把一个正在
// 限流的号记成健康号（顺带清零连败计数）。
func (s *Server) markHopSuccess(acc *pool.Account) {
	if acc == nil {
		return
	}
	acc.MarkSuccess()
	acc.ClearSessionDead()
}

// applyFrameClass 按上游 error 帧的分类处置账号（流式路径专用）。
//
// 与 HTTP 状态码路径共用同一套治理口径，只有一个例外：分类不出来（ErrUnknown）时
// **只记录、不处罚** —— 认不出的帧多半是我们的分类器没见过，不该拿账号权重去试错。
func (s *Server) applyFrameClass(acc *pool.Account, modelName string, class upstream.ErrClass, frame string) {
	if acc == nil {
		return
	}
	if class.Kind == upstream.ErrUnknown {
		acc.MarkFailure("上游错误帧（未分类）")
		return
	}
	s.applyClass(acc, modelName, class)
}

// -----------------------------------------------------------------------------
// 响应输出
// -----------------------------------------------------------------------------

// streamChatResponse 把上游 SSE 逐行转成 OpenAI 流式响应。
//
// 关键约定：流被中断时绝不补发 [DONE]（补发等于告诉下游「正常结束」，
// 会让残缺的工具调用被当成完整结果执行），而是下发一条明确的错误事件。
func (s *Server) streamChatResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, acc *pool.Account, sessionKey string, started time.Time) {
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}

	body := upstreamWatchdog(resp.Body, s.config().IdleTimeout)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	// cleaner 是有状态的（role 只在首个分片下发），整条流必须共用一个实例。
	cleaner := &chunkCleaner{}

	ttft := time.Duration(0)
	var usage map[string]any
	first := true
	interrupted := false
	// sawDone 记「上游发了 [DONE]」，finishReason 记「上游宣布过结束」。
	// 两者分开：只有确认收到过非空 finish_reason 才补发 [DONE]（见循环后的判定）。
	sawDone := false
	finishReason := ""
	// errorFrame 是上游的 error 帧原文（6004 限流 / 内容拦截 / 审核的流式形态）。
	// 记下来是为了在流尾按帧内容处置账号：这类流 HTTP 状态是 200，但这一跳并没有成功。
	errorFrame := ""

	for scanner.Scan() {
		clean := stripDataPrefix(scanner.Text())
		if clean == "" {
			continue
		}
		if clean == "[DONE]" {
			// 先记下、**不立刻转发**：要等确认收到过 finish_reason 再补 [DONE]。
			// 立刻转发等于对下游宣布「正常结束」—— 若这一轮其实被截断，
			// 残缺的工具调用会被当成完整结果执行。
			sawDone = true
			break
		}
		if first {
			ttft = time.Since(started)
			s.metrics.RecordTTFT(modelName, ttft)
			first = false
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(clean), &chunk) == nil {
			if u, ok := chunk["usage"].(map[string]any); ok {
				usage = u
			}
			finishReason = noteFinishReason(finishReason, chunk)
			if _, hasErr := chunk["error"]; hasErr {
				errorFrame = clean
			}
		}
		if out := cleaner.clean(clean); out != "" {
			_, _ = io.WriteString(w, "data: "+out+"\n\n")
			flusher.Flush()
		}
	}

	if err := scanner.Err(); err != nil {
		interrupted = true
		reason := "上游流式响应中断，本次回复不完整"
		if errors.Is(err, context.DeadlineExceeded) || idleTripped(body) {
			reason = fmt.Sprintf("上游超过 %v 无数据，判定连接卡死并中断，本次回复不完整", s.config().IdleTimeout)
		}
		errPayload, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": reason,
				"type":    "upstream_stream_interrupted",
				"code":    "stream_interrupted",
			},
		})
		_, _ = io.WriteString(w, "data: "+string(errPayload)+"\n\n")
		flusher.Flush()
		s.metrics.RecordFailure(modelName, "流中断")
		s.logf("[异常] 账号=%s 上游流式读取中断: %v", acc.Cred.AccountID(), err)
		s.sticky.Unbind(sessionKey)
	} else if errorFrame != "" {
		// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧内容分类并处置账号，
		// **不记成功、不绑粘性**（error 帧已原样透传给客户端，不再补发事件）。
		// 此前这些动作在流开始前就做了，于是一个正在限流的号被当成健康号，
		// 粘性还会把整个会话钉在它身上，后续每轮都打同一个限流号。
		class := upstream.FrameClass(errorFrame)
		interrupted = true
		s.applyFrameClass(acc, modelName, class, errorFrame)
		s.metrics.RecordFailure(modelName, "上游错误帧("+class.Kind+")")
		s.logf("[异常] 账号=%s 模型=%s 上游以 error 帧报错（分类=%s）：%s",
			acc.Cred.AccountID(), modelName, class.Kind, truncateForLog(errorFrame, 200))
		s.sticky.Unbind(sessionKey)
	} else if finishReason == "" {
		// 连接是**正常关掉**的（没有读错误），但整段流从未出现非空 finish_reason。
		//
		// 这不是成功：上游可能在生成中途被干净地掐断（LB 换后端、上游超时后正常关连接）。
		// 之前这里会被当成完整回复 —— 残缺内容记成成功、还补发 [DONE]，
		// 下游据此执行残缺的工具调用。现在明确报不完整，且**不补 [DONE]**。
		interrupted = true
		reason := "上游流正常结束但没有 finish_reason，本次回复可能不完整"
		errPayload, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": reason,
				"type":    "upstream_stream_incomplete",
				"code":    "stream_closed_without_finish",
			},
		})
		_, _ = io.WriteString(w, "data: "+string(errPayload)+"\n\n")
		flusher.Flush()
		s.metrics.RecordFailure(modelName, "流未正常结束")
		s.logf("[异常] 账号=%s 上游流正常关闭但没有非空 finish_reason（sawDone=%t），拒绝当成成功",
			acc.Cred.AccountID(), sawDone)
		s.sticky.Unbind(sessionKey)
	} else if sawDone {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	inputTokens, outputTokens := splitTokensFromUsage(usage)
	total := tokensFromUsage(usage)
	s.metrics.RecordTokens(modelName, total)
	s.metrics.RecordLatency(modelName, time.Since(started))
	// 只有确认这一跳成功（读到完整 finish_reason 且上游没报 error 帧）才记成功、
	// 记账号成功、把会话钉在该账号上；中断时上面已解绑，避免把会话钉在坏账号上。
	if !interrupted {
		s.metrics.RecordSuccess(modelName)
		s.markHopSuccess(acc)
		s.sticky.Bind(sessionKey, acc.Cred.AccountID())
	}

	s.stats.Record(stats.RecordInput{
		Model:        modelName,
		Account:      acc.Cred.AccountID(),
		OK:           !interrupted,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  total,
		TTFTMs:       ttft.Milliseconds(),
	})
	s.logRequest(r, modelName, accountLogLabel(acc), "chat/stream",
		ttft, time.Since(started), total, outputTokens, interrupted)

	// 三个数都要报，且标签必须与实参一致。
	//
	// 这里曾经写 `输出token=%d` 却把 `total` 传了进去 —— 于是日志里出现
	// 「总耗时=11s 输出token=364346」这种看着像计数 bug 的行（真把排查的人骗了一次：
	// 11 秒 36 万 token 意味着 3 万 tok/s，物理上不可能）。实际上那些请求的
	// prompt 本来就大，total 是对的，错的只是标签。
	s.logf("[请求完成] 模型=%s 账号=%s 流式 首字=%v 总耗时=%v 输入token=%d 输出token=%d 总token=%d 速率=%.1f tok/s",
		modelName, acc.Cred.AccountID(), ttft, time.Since(started), inputTokens, outputTokens, total,
		tokensPerSecond(outputTokens, time.Since(started), ttft))
}

// aggregateChatResponse 把上游 SSE 聚合成一次完整的非流式响应。
func (s *Server) aggregateChatResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, acc *pool.Account, sessionKey string, started time.Time) {
	defer resp.Body.Close()

	body := upstreamWatchdog(resp.Body, s.config().IdleTimeout)
	defer body.Close()

	out, err := aggregateCompletion(body, modelName)
	if err != nil {
		s.metrics.RecordFailure(modelName, "聚合失败")
		s.stats.Record(stats.RecordInput{
			Model:   modelName,
			Account: acc.Cred.AccountID(),
			OK:      false,
		})
		s.logRequest(r, modelName, accountLogLabel(acc), "chat/aggregate",
			0, time.Since(started), 0, 0, true)
		s.sticky.Unbind(sessionKey)
		writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "聚合上游流式响应失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)

	usage := usageFromCompletion(out)
	inputTokens, outputTokens := splitTokensFromUsage(usage)
	total := tokensFromUsage(usage)
	s.metrics.RecordTokens(modelName, total)
	s.metrics.RecordLatency(modelName, time.Since(started))
	s.metrics.RecordSuccess(modelName)
	// 聚合路径同样等聚合完成（上游若无 error 帧才会走到这里，见 aggregateCompletion）
	// 才记账号成功并绑粘性。
	s.markHopSuccess(acc)
	s.sticky.Bind(sessionKey, acc.Cred.AccountID())

	s.stats.Record(stats.RecordInput{
		Model:        modelName,
		Account:      acc.Cred.AccountID(),
		OK:           true,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  total,
	})
	s.logRequest(r, modelName, accountLogLabel(acc), "chat/aggregate",
		0, time.Since(started), total, outputTokens, false)

	// 非流式没有首字时刻，TTFB 记 0 → 分母自然落到端到端，与上游同口径。
	s.logf("[请求完成] 模型=%s 账号=%s 非流式 总耗时=%v 输入token=%d 输出token=%d 总token=%d 速率=%.1f tok/s",
		modelName, acc.Cred.AccountID(), time.Since(started), inputTokens, outputTokens, total,
		tokensPerSecond(outputTokens, time.Since(started), 0))
}

// -----------------------------------------------------------------------------
// 上游响应体包装
// -----------------------------------------------------------------------------

// cancelOnClose 在响应体关闭时取消上游请求上下文。
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(func() { c.cancel() })
	return err
}

// cancelUpstream 是空闲看门狗专用的中止路径：取消上游请求并关闭响应体。
//
// 为什么不能只取消一个本地 ctx：那个 ctx 下面没有挂任何请求，取消它是空动作，
// 表现为「看门狗存在，但上游停滞时永远不中断」，请求会一直挂到客户端自己断开。
func (c *cancelOnClose) cancelUpstream() {
	c.once.Do(func() { c.cancel() })
	if c.ReadCloser != nil {
		_ = c.ReadCloser.Close()
	}
}

// upstreamWatchdog 用空闲看门狗包裹上游响应体：持续有数据就永不超时，
// 超过 idle 无新数据则中止上游请求并关闭流，让 Read 立刻返回错误。
//
// 返回的包装必须 Close（停止看门狗协程），调用方 defer 一个即可；Close 同时
// 关闭上游响应体，等价于原来的 defer resp.Body.Close()。
func upstreamWatchdog(rc io.ReadCloser, idle time.Duration) io.ReadCloser {
	if rc == nil || idle <= 0 {
		return rc
	}
	stop := make(chan struct{})
	rd := upstream.NewIdleReader(rc, idle, func() {
		if c, ok := rc.(interface{ cancelUpstream() }); ok {
			c.cancelUpstream()
			return
		}
		_ = rc.Close()
	})
	rd.Start(stop)
	return &watchdogReader{inner: rd, stop: stop}
}

// idleTripped 报告这次读错误是否由空闲看门狗触发（上游停滞），用于给出可区分的文案。
func idleTripped(body io.Reader) bool {
	if t, ok := body.(interface{ Expired() bool }); ok {
		return t.Expired()
	}
	return false
}

type watchdogReader struct {
	inner *upstream.IdleReader
	stop  chan struct{}
	once  sync.Once
}

func (w *watchdogReader) Read(p []byte) (int, error) { return w.inner.Read(p) }

func (w *watchdogReader) Expired() bool { return w.inner.Expired() }

func (w *watchdogReader) Close() error {
	w.once.Do(func() { close(w.stop) })
	return w.inner.Close()
}
