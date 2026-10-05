package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// 上游签到相关业务码：这两个码表示「今天已经签过了」，属于成功而非失败。
var alreadyCheckedInCodes = map[int]bool{10001: true, 14001: true}

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	Already bool   // 今天已签到（幂等）
	Message string // 上游文案
	Code    int
}

// Checkin 执行每日签到。走站点 Web 域的 /v2/billing/meter/daily-checkin。
//
// 幂等语义：上游对「今天已签到」返回业务码 10001/14001 而非报错，
// 因此这里把它们识别为成功（Already=true），避免调度器把重复签到当成失败刷告警。
func (c *Client) Checkin(ctx context.Context, cred *CredentialView, p *Profile) (CheckinResult, error) {
	var out CheckinResult
	headers := func(r *http.Request) {
		commonHeaders(r, p)
		r.Header.Set("Authorization", "Bearer "+cred.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		if cred.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
		}
	}
	env, status, err := c.doEnvelope(ctx, http.MethodPost, p.DailyCheckinURL(), headers, strings.NewReader("{}"))
	out.Code = env.Code
	out.Message = env.Msg

	// 先判业务码，再判传输错误。
	// 关键：上游对「今天已签到」返回的是 **HTTP 400 + code 10001**，
	// 若先判 HTTP 错误就短路了，会把幂等成功误报成失败（每天都在刷假告警）。
	switch {
	case env.Code != 0 && alreadyCheckedInCodes[env.Code]:
		out.Already = true
		return out, nil
	case env.Code == 0 && err == nil:
		return out, nil
	case strings.Contains(env.Msg, "已签到") || strings.Contains(strings.ToLower(env.Msg), "already"):
		out.Already = true
		return out, nil
	case err != nil:
		return out, err
	}
	return out, fmt.Errorf("签到失败 code=%d msg=%s (HTTP %d)", env.Code, env.Msg, status)
}

// CheckinStatus 是「今日是否已签到」的查询结果。
type CheckinStatus struct {
	// TodayCheckedIn 为真表示今天已经签过。
	TodayCheckedIn bool
	// Unsupported 为真表示该站点不提供签到（国际站），**不是失败**。
	Unsupported bool
	// Raw 是上游返回的 data 原文，供排查字段漂移。
	Raw map[string]any
}

// CheckinStatus 查询今日签到状态。
//
// 为什么需要它：`Checkin`（POST daily-checkin）本身是幂等的，重复调用只会返回
// 「今天已签到」，所以**光靠签到接口无法区分「还没签」和「签过了」**。
// 要让界面在点之前就告诉用户状态，必须单独查。
//
// 端点两个候选、只有 404 才回落 —— 与上游客户端的策略一致。401/403 这类
// 鉴权失败**必须原样返回**：它们不是路径问题，回落只会掩盖真因，
// 还会让每个账号多打一次无意义的请求。
func (c *Client) CheckinStatus(ctx context.Context, cred *CredentialView, p *Profile) (CheckinStatus, error) {
	var out CheckinStatus
	if !p.SupportsCheckin() {
		// 国际站没有签到，不发请求 —— 这是能力缺失而非错误，
		// 调用方据此把状态显示成「不支持」而不是「查询失败」。
		out.Unsupported = true
		return out, nil
	}

	headers := func(r *http.Request) {
		commonHeaders(r, p)
		r.Header.Set("Authorization", "Bearer "+cred.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		if cred.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
		}
	}

	urls := p.CheckinStatusURLs()
	var lastErr error
	for i, u := range urls {
		env, status, err := c.doEnvelope(ctx, http.MethodPost, u, headers, strings.NewReader("{}"))
		isLast := i == len(urls)-1

		if err != nil {
			lastErr = err
			if isLast {
				return out, err
			}
			continue
		}
		// 只有 404 才认为「这个路径不存在，试下一个」。
		if status == http.StatusNotFound && !isLast {
			lastErr = fmt.Errorf("路径不存在 (HTTP 404): %s", u)
			continue
		}
		if status >= 400 {
			// 鉴权/风控类失败：不回落，直接报出来。
			return out, fmt.Errorf("查询签到状态失败 code=%d msg=%s (HTTP %d)", env.Code, env.Msg, status)
		}

		var data map[string]any
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &data)
		}
		out.Raw = data
		// 字段名两种写法都认：上游给过 today_checked_in 与 todayCheckedIn。
		out.TodayCheckedIn = boolField(data, "today_checked_in", "todayCheckedIn")
		return out, nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的签到状态端点")
	}
	return out, lastErr
}

// boolField 从若干候选键里取布尔值（宽容：字符串 "true" 也认）。
func boolField(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t
		case string:
			return strings.EqualFold(strings.TrimSpace(t), "true")
		}
	}
	return false
}
