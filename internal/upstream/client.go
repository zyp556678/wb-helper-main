package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Envelope 是上游统一响应包络：code=0 时 data 有效。
type Envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 是上游访问客户端：控制类短请求与对话流共用同一 Transport。
type Client struct {
	Control  *http.Client // 控制类短请求（有总时长上限）
	ChatHTTP *http.Client // 对话流（无总时长上限）
	// IdleTimeout 是流式响应的空闲读超时。
	IdleTimeout time.Duration
	// TransientRetries 是「请求头尚未写出」时对瞬时网络错误的额外重试次数（0 禁用）。
	TransientRetries int
	// TransientRetryBackoff 是两次重试之间的等待。
	TransientRetryBackoff time.Duration
	Verbose               bool
	Logf                  func(format string, args ...any)
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// -----------------------------------------------------------------------------
// 请求头
// -----------------------------------------------------------------------------

// commonHeaders 设置与站点相关的伪装头。
func commonHeaders(req *http.Request, p *Profile) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", p.Origin)
	req.Header.Set("Referer", p.Origin+"/")
	req.Header.Set("User-Agent", p.ClientUA)
}

// BackendHeaders 为对话请求注入账号凭据与客户端指纹。
func BackendHeaders(req *http.Request, c *CredentialView, p *Profile) {
	commonHeaders(req, p)
	rid := newRequestID()
	req.Header.Set("X-Request-ID", rid)
	req.Header.Set("X-Trace-ID", rid)
	req.Header.Set("X-Client-ID", p.ClientID)
	req.Header.Set("X-Client-Version", p.ClientVer)
	req.Header.Set("X-Product", p.Product)

	if c != nil {
		if c.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.AccessToken)
		}
		if c.UID != "" {
			req.Header.Set("X-User-Id", c.UID)
		}
		if c.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", c.EnterpriseID)
		}
		if c.Domain != "" {
			req.Header.Set("X-Domain", c.Domain)
		}
		if c.DeviceToken != "" {
			// 设备风控头：仅在凭据里带 device_token 时注入（多数部署没有）
			req.Header.Set("X-Device-Token", c.DeviceToken)
		}
	}
}

// CredentialView 是反代层需要的凭据视图，避免 upstream 反向依赖 pool。
type CredentialView struct {
	AccessToken  string
	RefreshToken string
	UID          string
	EnterpriseID string
	Domain       string
	DeviceToken  string
}

// IsEnterprise 报告账号是否为企业版（凭据带非空 enterpriseId）。
//
// 与 auth.Credential.IsEnterprise 同一判据 —— 那一侧有完整的实测依据说明
// （企业号没有个人成长体系，上游对成长域一律 400/403）。这里复制一份是因为
// 反代层拿到的是 CredentialView 而不是 auth.Credential，两边不能互相引用。
func (c *CredentialView) IsEnterprise() bool {
	return c != nil && strings.TrimSpace(c.EnterpriseID) != ""
}

// -----------------------------------------------------------------------------
// 控制类请求
// -----------------------------------------------------------------------------

// doJSON 发一个控制类请求并解开包络，返回 data 字段。
func (c *Client) doJSON(ctx context.Context, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	env, status, err := c.doEnvelope(ctx, method, fullURL, headers, body)
	if err != nil {
		return nil, status, err
	}
	if env.Code != 0 {
		return nil, status, fmt.Errorf("上游业务错误 code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, status, nil
}

// doEnvelope 发控制类请求并返回完整包络（不把非零 code 当致命错误）。
//
// 登录轮询需要这个粒度：上游在「用户还没授权」时返回非零 code（如 11217 login ing...），
// 那是正常中间态而非失败，调用方需要按 code 自行判断。
func (c *Client) doEnvelope(ctx context.Context, method, fullURL string, headers func(*http.Request), body io.Reader) (Envelope, int, error) {
	var env Envelope
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return env, 0, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := c.Control.Do(req)
	if err != nil {
		return env, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 400 {
		// 有些失败也是标准包络（带 code/msg），优先解析出来给出可读原因
		if json.Unmarshal(raw, &env) == nil && (env.Code != 0 || env.Msg != "") {
			return env, resp.StatusCode, fmt.Errorf("上游返回 %d: code=%d msg=%s", resp.StatusCode, env.Code, env.Msg)
		}
		return env, resp.StatusCode, fmt.Errorf("上游返回 %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, resp.StatusCode, fmt.Errorf("解析上游 JSON 失败: %w", err)
	}
	return env, resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// 令牌刷新
// -----------------------------------------------------------------------------

// RefreshedToken 是刷新接口返回的令牌字段。
type RefreshedToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// RefreshToken 调用上游刷新令牌，只更新入参视图，不落盘（落盘由调用方负责）。
// 返回上游 HTTP 状态码；网络错误时为 0。
func (c *Client) RefreshToken(ctx context.Context, cred *CredentialView, p *Profile) (RefreshedToken, int, error) {
	var out RefreshedToken
	if cred == nil || cred.RefreshToken == "" {
		return out, 0, errors.New("缺少 refreshToken，无法刷新")
	}
	headers := func(r *http.Request) {
		commonHeaders(r, p)
		r.Header.Set("X-Refresh-Token", cred.RefreshToken)
		if cred.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
		}
		r.Header.Set("X-Auth-Refresh-Source", "workbuddy")
	}
	data, status, err := c.doJSON(ctx, http.MethodPost, p.TokenRefreshURL(), headers, nil)
	if err != nil {
		return out, status, err
	}
	if err := json.Unmarshal(data, &out); err != nil || out.AccessToken == "" {
		return out, status, fmt.Errorf("解析新令牌失败: %w", err)
	}
	return out, status, nil
}

// -----------------------------------------------------------------------------
// 模型目录
// -----------------------------------------------------------------------------

// ModelEntry 是模型目录里的一项（切片 0 只用 id/name/上限，倍率在切片 3 补）。
type ModelEntry struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	MaxInputTokens  int    `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int    `json:"maxOutputTokens,omitempty"`
}

// catalogDoc 是模型目录的 data 结构。
type catalogDoc struct {
	Models []ModelEntry `json:"models"`
}

// FetchModels 拉取某站点账号可见的模型目录。
func (c *Client) FetchModels(ctx context.Context, cred *CredentialView, p *Profile) ([]ModelEntry, error) {
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
	var doc catalogDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析模型目录失败: %w", err)
	}
	out := make([]ModelEntry, 0, len(doc.Models))
	seen := map[string]bool{}
	for _, m := range doc.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		m.ID = id
		out = append(out, m)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// 对话
// -----------------------------------------------------------------------------

// Chat 转发一次对话请求，返回原始 SSE 响应。调用方负责关闭 Body。
//
// 走 doChatWithRetry：对「请求头尚未写出」时的瞬时网络错误做有界重试。
func (c *Client) Chat(ctx context.Context, cred *CredentialView, p *Profile, body []byte) (*http.Response, error) {
	return c.doChatWithRetry(ctx, cred, p, body, c.TransientRetries, c.TransientRetryBackoff)
}

// IsTransientNetworkError 判断错误是否为「瞬时网络故障」——这类错误值得重试。
//
// 覆盖：连接被重置/中止（RST）、连接被对端关闭、管道破裂、连接被强制关闭，
// 以及 Go 在复用 keep-alive 连接时常见的 "server closed idle connection"。
//
// 注意 context.Canceled / DeadlineExceeded **不算**：前者是客户端主动断开，
// 后者是明确超时，重试都无意义甚至有害（客户端已经走了，重试只是白烧额度）。
func IsTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// 网络层超时（如 ResponseHeaderTimeout）说明上游确实没响应，可重试。
		// 上面已排除 ctx 超时，所以这里拿到的是传输层自己的超时。
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection reset by peer",
		"connection reset",
		"broken pipe",
		"use of closed network connection",
		"connection refused",
		"server closed idle connection",
		"unexpected eof",
		"http2: server sent goaway",
		"stream error",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// IsUpstreamTimeout 判断一次转发失败是否属于「上游超时 / 停滞」。
//
// 超时不是账号的问题：同一份请求换到别的号，撞上的是同一个慢上游，换号注定白换
// ——只会把客户端拖到 MaxRotate × header_timeout，期间还给一串健康号喂失败计数。
// 调用方据此**止损**：不轮转、不罚号，并回一条与「没有可用账号」可区分的错误。
//
// 判定三态：net.Error.Timeout()（ResponseHeaderTimeout / Client.Timeout）、显式 deadline
// （DeadlineExceeded / os.ErrDeadlineExceeded）、以及**客户端仍在但 ctx 被取消**——
// 后者只能是我们自己的空闲看门狗掐的流，也就是上游停滞。客户端主动断连时
// clientGone=true（r.Context() 已取消），那属于抖动而非超时。
func IsUpstreamTimeout(err error, clientGone bool) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return !clientGone && errors.Is(err, context.Canceled)
}

// credLabel 是日志里的账号标识。
//
// CredentialView 只有令牌本身，不带凭据文件名（那是 auth 层的事），
// 所以退而求其次用 uid；uid 也缺时给出占位，避免日志里出现空白的 "账号="。
func credLabel(cred *CredentialView) string {
	if cred == nil {
		return "(无凭据)"
	}
	if cred.UID != "" {
		return cred.UID
	}
	return "(无 uid)"
}

// doChatWithRetry 发送对话请求，并对瞬时网络错误做有界重试。
//
// ## 为什么只在「请求头尚未写出」时重试
//
// 一旦请求头写出，上游就可能已经开始处理这次 POST。此时收到 EOF/RST，
// 我们**无法判断**它是「上游没收到」还是「上游收到了、正在生成、但连接断了」。
// 后者重放会造成重复生成与重复计费，所以宁可把错误如实抛给调用方。
// 这个边界是上游 v1.13.7 踩出来的结论，不是保守估计。
//
// ## 为什么重试要换新连接
//
// 触发重试的常见原因就是「复用了已被对端回收的 keep-alive 连接」。
// 不换连接的话，下一次尝试会命中同一条死连接，必然再次失败。
// 所以重试时置 Connection: close 并 CloseIdleConnections()。
func (c *Client) doChatWithRetry(ctx context.Context, cred *CredentialView, p *Profile,
	body []byte, retries int, backoff time.Duration) (*http.Response, error) {

	if retries < 0 {
		retries = 0
	}
	if backoff <= 0 {
		backoff = 300 * time.Millisecond
	}

	var lastErr error
	for try := 0; try <= retries; try++ {
		var headersWritten, bodyWritten atomic.Bool
		trace := &httptrace.ClientTrace{
			WroteHeaders: func() { headersWritten.Store(true) },
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				if info.Err == nil {
					bodyWritten.Store(true)
				}
			},
		}

		// 每轮都重建请求对象与 body reader：bytes.Reader 只能用一次。
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace),
			http.MethodPost, p.ChatURL(), strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		BackendHeaders(req, cred, p)
		if try > 0 {
			req.Close = true
			c.ChatHTTP.CloseIdleConnections()
		}

		resp, err := c.ChatHTTP.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		if !IsTransientNetworkError(err) {
			return nil, err
		}
		if headersWritten.Load() || bodyWritten.Load() {
			// 请求已发出：上游可能已处理，重放有重复生成风险，如实报错。
			c.logf("[网络重试跳过] 账号=%s 请求头已写出=%t 请求体已写完=%t 原因=上游可能已处理，重放会重复生成 错误=%v",
				credLabel(cred), headersWritten.Load(), bodyWritten.Load(), err)
			return nil, err
		}
		if try >= retries {
			break
		}
		c.logf("[网络重试] 账号=%s 第 %d/%d 次重试，上一尝试请求头未写出，已重建连接（错误: %v）",
			credLabel(cred), try+1, retries, lastErr)
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

// IdleReader 是带空闲看门狗的 ReadCloser：持续有数据就永不超时，
// 超过 idle 无任何新数据才中断（避免长流被总时长误杀）。
type IdleReader struct {
	rc       io.ReadCloser
	idle     time.Duration
	cancel   context.CancelFunc
	lastRead time.Time
	// tripped 记录这次中断**是看门狗触发的**（上游停滞），调用方据此给出可区分的
	// 错误文案：与客户端断连、上游正常关流不同，这一类的排查方向是「上游卡住了」。
	tripped atomic.Bool
}

// NewIdleReader 包装一个响应体。
func NewIdleReader(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) *IdleReader {
	return &IdleReader{rc: rc, idle: idle, cancel: cancel, lastRead: time.Now()}
}

// Start 启动看门狗协程；返回停止函数。
func (r *IdleReader) Start(stop chan struct{}) {
	if r.idle <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(r.idle / 4)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if time.Since(r.lastRead) > r.idle {
					r.tripped.Store(true)
					// 触发上游请求取消，让 Read 立刻返回错误
					if r.cancel != nil {
						r.cancel()
					}
					return
				}
			}
		}
	}()
}

// Expired 报告本次中断是否由空闲看门狗触发（上游停滞）。
func (r *IdleReader) Expired() bool { return r.tripped.Load() }

// Read 实现 io.Reader。
func (r *IdleReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.lastRead = time.Now()
	}
	return n, err
}

// Close 实现 io.Closer。
func (r *IdleReader) Close() error { return r.rc.Close() }

// -----------------------------------------------------------------------------
// 工具
// -----------------------------------------------------------------------------

// newRequestID 生成请求标识（无外部依赖的 UUIDv4）。
func newRequestID() string {
	return randomHex(16)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
