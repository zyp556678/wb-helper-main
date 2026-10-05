package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/tasks"
	"workbuddy-gateway/internal/upstream"
)

// -----------------------------------------------------------------------------
// 浏览器内登录（OAuth 设备授权）
//
// 与 wb-gateway 的 login 命令同一套协议，只是把终端二维码换成浏览器：
// 面板展示授权链接与二维码，用户完成授权后前端轮询，服务端拿到令牌即落盘并热加载进池。
// -----------------------------------------------------------------------------

// 登录会话状态。
const (
	loginPending = "pending"
	loginOK      = "ok"
	loginExpired = "expired"
	loginError   = "error"
)

// loginSession 是一次登录尝试的服务端会话。
type loginSession struct {
	ID        string
	Site      string
	State     string
	AuthURL   string
	ExpiresAt time.Time
	Status    string
	Err       string
	Account   string // 成功后落盘的账号 ID
}

// loginStore 保存进行中的登录会话。
//
// 轮询由前端驱动（每 2 秒一次），服务端每次请求向上游查一次，
// 因此不需要后台协程，会话超时后按需清理。
type loginStore struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
}

func newLoginStore() *loginStore {
	return &loginStore{sessions: map[string]*loginSession{}}
}

// gc 清理已完成或已超时较久的会话。
func (s *loginStore) gc(now time.Time) {
	for id, sess := range s.sessions {
		deadline := sess.ExpiresAt
		if sess.Status == loginOK {
			deadline = deadline.Add(5 * time.Minute)
		}
		if now.After(deadline.Add(30 * time.Minute)) {
			delete(s.sessions, id)
		}
	}
}

func (s *loginStore) put(sess *loginSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
}

func (s *loginStore) get(id string) *loginSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// -----------------------------------------------------------------------------
// 端点
// -----------------------------------------------------------------------------

// handleLoginSites 列出可登录的站点。
func (s *Server) handleLoginSites(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sites": []map[string]any{
			{
				"site": auth.SiteCN, "label": upstream.ProfileCN.Label,
				"hint": "微信 / 企业微信扫码",
			},
			{
				"site": auth.SiteINTL, "label": upstream.ProfileINTL.Label,
				"hint": "浏览器内登录（邮箱 / 验证码 / SSO）",
			},
		},
	})
}

// handleLoginStart 发起一次登录：取 state 与授权链接。
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Site string `json:"site"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	site := auth.SiteCN
	if req.Site == auth.SiteINTL {
		site = auth.SiteINTL
	}
	prof := upstream.ProfileForSite(site)

	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()

	st, err := s.client.AuthState(ctx, prof)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody("获取授权链接失败: "+err.Error()))
		return
	}

	ttl := upstream.LoginTTL[site]
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	now := time.Now()
	s.logins.gc(now)

	sess := &loginSession{
		ID:        randomID(),
		Site:      site,
		State:     st.State,
		AuthURL:   st.AuthURL,
		ExpiresAt: now.Add(ttl),
		Status:    loginPending,
	}
	s.logins.put(sess)
	s.logf("[登录] 发起 %s 登录，等待授权（%v 有效）", prof.Label, ttl.Round(time.Second))

	writeJSON(w, http.StatusOK, map[string]any{
		"login_id":   sess.ID,
		"site":       site,
		"site_label": prof.Label,
		"auth_url":   sess.AuthURL,
		"expires_in": int(ttl.Seconds()),
	})
}

// handleLoginPoll 轮询一次登录结果：拿到令牌即落盘并热加载进池。
func (s *Server) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		id = r.URL.Query().Get("login_id")
	}
	sess := s.logins.get(id)
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": loginError, "error": "登录会话不存在或已失效，请重新发起",
		})
		return
	}
	if sess.Status == loginOK {
		s.writeLoginOK(w, sess)
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		sess.Status = loginExpired
		sess.Err = "登录已超时，请重新发起"
		s.writeLoginOK(w, sess)
		return
	}

	prof := upstream.ProfileForSite(sess.Site)
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()

	tok, ok, err := s.client.PollToken(ctx, prof, sess.State)
	if err != nil {
		sess.Status = loginError
		sess.Err = "查询登录状态失败: " + err.Error()
		s.writeLoginOK(w, sess)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": loginPending})
		return
	}

	// 拿到令牌：取账号信息 → 落盘 → 热加载进池
	info, err := s.client.LoginAccount(ctx, prof, sess.State, tok.AccessToken)
	if err != nil {
		s.logf("[登录] 已拿到令牌但取账号信息失败（继续落盘）: %v", err)
	}

	expiresAt := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	if tok.ExpiresIn <= 0 {
		expiresAt = 0
	}
	path := s.pool.CredentialPathFor(info.UID)
	cred := auth.NewForLogin(path, sess.Site, tok.AccessToken, tok.RefreshToken,
		expiresAt, tok.Domain, info.UID, info.EnterpriseID, info.Nickname)

	if err := cred.SaveFull(); err != nil {
		sess.Status = loginError
		sess.Err = "登录成功但保存凭据失败: " + err.Error()
		s.writeLoginOK(w, sess)
		return
	}
	s.pool.Add(cred)

	// 顺带查一次额度，让面板立刻有积分数据
	if acc, err := s.pool.Find(cred.AccountID()); err == nil {
		_ = s.pool.RefreshQuota(ctx, acc)
	}
	// 国际站新账号要先完成注册地区与激活，否则对话直接报 14017 trial not activated；
	// 顺带领一次性 trial 加油包（已领过是幂等的，不算失败）。
	s.ensureIntlRegistration(ctx, cred.AccountID())

	sess.Status = loginOK
	sess.Account = cred.AccountID()
	s.logf("[登录] %s 登录成功，账号 %s（%s），凭据已写入 %s",
		prof.Label, displayName(info.Nickname, info.UID), cred.AccountID(), path)
	s.writeLoginOK(w, sess)
}

// ensureIntlRegistration 让国际站账号完成「注册地区 + 激活」并领取 trial 加油包。
//
// **只在国际站账号上跑**：国内站没有这条链路（发请求只会拿到 404）。
// 整段是幂等的：已激活直接返回、已领过 trial 视为成功，所以登录/导入都能安全重复调用。
// 失败只记日志 —— 登录本身已经成功了，把注册失败讲成「登录失败」会误导用户。
func (s *Server) ensureIntlRegistration(ctx context.Context, accountID string) {
	acc, err := s.pool.Find(accountID)
	if err != nil || acc.Profile().Key != "intl" {
		return
	}
	view := acc.View()
	prof := acc.Profile()
	if activated, err := s.client.GlobalCompleteRegistration(ctx, view, prof); err != nil {
		s.logf("[国际站] 账号 %s 注册激活未完成: %v", accountID, err)
	} else if activated {
		s.logf("[国际站] 账号 %s 注册激活完成", accountID)
	}
	if claimed, err := s.client.ClaimTrial(ctx, view, prof); err != nil {
		s.logf("[国际站] 账号 %s trial 加油包领取失败: %v", accountID, err)
	} else if claimed {
		s.logf("[国际站] 账号 %s 已领取 trial 加油包", accountID)
	}
	// 领到积分后刷新一次额度，面板立刻能看到变化。
	_ = s.pool.RefreshQuota(ctx, acc)
}

// writeLoginOK 输出终态或 pending 之外的状态。
func (s *Server) writeLoginOK(w http.ResponseWriter, sess *loginSession) {
	payload := map[string]any{"status": sess.Status}
	if sess.Err != "" {
		payload["error"] = sess.Err
	}
	if sess.Status == loginOK {
		if acc, err := s.pool.Find(sess.Account); err == nil {
			payload["account"] = s.pool.StateOf(acc)
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// -----------------------------------------------------------------------------
// 工具
// -----------------------------------------------------------------------------

func errBody(msg string) map[string]any {
	return map[string]any{"error": map[string]any{"code": 400, "message": msg}}
}

// writeActionError 输出「动作型端点」的失败响应，并按错误性质选择状态码。
//
// 为什么要区分：这类端点（任务 / 成长活动 / 本机代理）的失败有两种，
// 但历史上**一律返回 502**，而 502 的语义是「上游网关坏了」。
// 于是「国际站没有成长中心活动」这种**确定性前提不满足**也被报成 502，
// 用户看到「请求失败（HTTP 502）」，会以为是网络问题去重试——
// 重试一百次也不会变，因为这不是故障。
//
//   - 前提不满足（tasks.IsPrecondition）→ 409 Conflict：请求本身没毛病，
//     是当前资源状态不允许。明确告诉调用方「重试无用」。
//   - 其它（真·上游/网络故障）→ 502，语义与原来一致。
//
// 两种都用平铺的 {"ok":false,"detail":...} 信封（前端动作型端点统一按它解析）。
func writeActionError(w http.ResponseWriter, err error) {
	writeActionErrorExtra(w, err, nil)
}

// writeActionErrorExtra 同 writeActionError，另附若干字段（如 {"task": "..."}）。
func writeActionErrorExtra(w http.ResponseWriter, err error, extra map[string]any) {
	status := http.StatusBadGateway
	if tasks.IsPrecondition(err) {
		status = http.StatusConflict
	}
	body := map[string]any{"ok": false, "detail": err.Error()}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

func decodeBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// contextWithTimeout 基于请求上下文派生一个带超时的上下文。
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func displayName(nickname, uid string) string {
	if nickname != "" {
		return nickname
	}
	if uid != "" {
		return uid
	}
	return "(未知账号)"
}
