package upstream

// -----------------------------------------------------------------------------
// 国际站（global realm）新账号：注册激活 + 地区完善 + trial 加油包
//（对照 wb2api-panel 的 upstream/global_register.go + upstream/trial.go）
//
// 背景：新注册的国际站账号**没有完成注册地区**，chat 会直接报
// `14017 trial not activated` —— 用户看到的是「账号明明登录成功了，一对话就失败」。
// 链路（逆向自 web 注册完善页）：
//
//	POST /billing/area/get-country-code {filterForbidden:1}   → 可选地区列表
//	GET  /auth/realms/copilot/overseas/user/register?userId=  → 查激活状态
//	     （code:200 已激活；code:500 "region required" 需要补地区）
//	POST /console/login/account {attributes:{countryCode,countryFullName,countryName}} → 提交地区（幂等）
//	POST /billing/ide/trial                                   → 一次性 trial 加油包（幂等码 14051）
//
// 两个响应体细节：`get-country-code` 的 `data` 是**被序列化成字符串的 JSON**（双层信封），
// 需要二次解析；`register` 端点的成功码是 **200 而不是 0**（与其它端点不同）。
// -----------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// globalWebUA 是国际站 web 端的 UA（注册完善页走 web 指纹，不是桌面端 CLI 指纹）。
const globalWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// trialPath 是国际站一次性 trial 加油包端点。
const trialPath = "/billing/ide/trial"

// trialAlreadyMarkers 是幂等码 14051「已领取过」的两种指纹（不同分支拼出的文案不同）。
var trialAlreadyMarkers = []string{"14051"}

// GlobalCountry 是一个可选注册地区。
type GlobalCountry struct {
	EnName string `json:"EnName"` // 英文全名（countryFullName）
	Name   string `json:"Name"`   // 显示名
	IOS2   string `json:"IOS2"`   // 二字码（countryName）
	IOS3   string `json:"IOS3"`
	Code   string `json:"Code"` // 数字码（countryCode）
}

// globalCountriesWhitelist 是国际版 web 展示的地区白名单（顺序与页面一致）。
var globalCountriesWhitelist = []string{"HK", "MO", "SG", "TH", "PH", "MY", "ID"}

// globalReq 构造注册链路的请求：web 指纹 UA + 同域 Origin/Referer + Bearer。
func (c *Client) globalReq(ctx context.Context, method, url, token string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	base := ProfileINTL.Origin
	req.Header.Set("User-Agent", globalWebUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// globalCall 发注册链路请求并解外层信封（code/msg/data）。
func (c *Client) globalCall(ctx context.Context, method, url, token string, body any) (int, string, json.RawMessage, error) {
	req, err := c.globalReq(ctx, method, url, token, body)
	if err != nil {
		return 0, "", nil, err
	}
	resp, err := c.Control.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, "", nil, fmt.Errorf("解析注册链路响应失败: %w", err)
	}
	return env.Code, env.Msg, env.Data, nil
}

// isGlobal 判断凭据是不是国际站账号（只有它们走这条链路）。
func isGlobal(cred *CredentialView, p *Profile) bool {
	return cred != nil && p != nil && p.Key == "intl"
}

// GlobalFetchCountries 拉取可选注册地区；intlOnly 为真时按国际版白名单过滤。
func (c *Client) GlobalFetchCountries(ctx context.Context, cred *CredentialView, p *Profile) ([]GlobalCountry, error) {
	if !isGlobal(cred, p) {
		return nil, fmt.Errorf("只有国际站账号需要注册地区")
	}
	code, msg, raw, err := c.globalCall(ctx, http.MethodPost,
		p.Origin+"/billing/area/get-country-code", cred.AccessToken,
		map[string]any{"filterForbidden": 1})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("拉取地区列表失败: %s (code=%d)", msg, code)
	}
	// data 是 JSON 字符串（双层信封）或对象，两种形态都要能解。
	if s := strings.TrimSpace(string(raw)); strings.HasPrefix(s, `"`) {
		var unwrapped string
		if err := json.Unmarshal(raw, &unwrapped); err != nil {
			return nil, fmt.Errorf("地区列表解包失败: %w", err)
		}
		raw = json.RawMessage(unwrapped)
	}
	var inner struct {
		Data struct {
			List []GlobalCountry `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &inner); err != nil {
		return nil, fmt.Errorf("地区列表解析失败: %w", err)
	}
	list := inner.Data.List
	byCode := make(map[string]GlobalCountry, len(list))
	for _, ctry := range list {
		byCode[ctry.IOS2] = ctry
	}
	out := make([]GlobalCountry, 0, len(globalCountriesWhitelist))
	for _, code := range globalCountriesWhitelist {
		if ctry, ok := byCode[code]; ok {
			out = append(out, ctry)
		}
	}
	return out, nil
}

// GlobalRegisterStatus 查注册激活状态：activated / needsRegion / 原始文案。
func (c *Client) GlobalRegisterStatus(ctx context.Context, cred *CredentialView, p *Profile) (activated, needsRegion bool, msg string, err error) {
	if !isGlobal(cred, p) {
		return false, false, "", fmt.Errorf("只有国际站账号需要注册激活")
	}
	req, err := c.globalReq(ctx, http.MethodGet,
		p.Origin+"/auth/realms/copilot/overseas/user/register?userId="+cred.UID, cred.AccessToken, nil)
	if err != nil {
		return false, false, "", err
	}
	req.Header.Set("X-User-Id", cred.UID)
	resp, err := c.Control.Do(req)
	if err != nil {
		return false, false, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return false, false, "", fmt.Errorf("解析注册状态失败: %w", err)
	}
	switch {
	case env.Code == 200:
		// 注意：这个端点的成功码是 200，不是其它端点的 0。
		return true, false, "register success", nil
	case env.Code == 500 || strings.Contains(strings.ToLower(env.Msg), "region required"):
		return false, true, env.Msg, nil
	default:
		return false, false, env.Msg, nil
	}
}

// GlobalSubmitRegion 提交注册地区（幂等）。country 来自 GlobalFetchCountries。
func (c *Client) GlobalSubmitRegion(ctx context.Context, cred *CredentialView, p *Profile, country GlobalCountry) error {
	if !isGlobal(cred, p) {
		return fmt.Errorf("只有国际站账号需要注册地区")
	}
	attrs := map[string]any{
		"countryCode":     []string{country.Code},
		"countryFullName": []string{country.EnName},
		"countryName":     []string{country.IOS2},
	}
	code, msg, _, err := c.globalCall(ctx, http.MethodPost, p.Origin+"/console/login/account",
		cred.AccessToken, map[string]any{"attributes": attrs})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("提交地区失败: %s (code=%d)", msg, code)
	}
	return nil
}

// GlobalCompleteRegistration 一键注册激活：查状态 → 需要补地区就按默认地区（白名单首个，通常是 HK）
// 提交后重新激活。已激活时直接返回（幂等）。失败不阻断调用方，只把原因交回去记日志。
func (c *Client) GlobalCompleteRegistration(ctx context.Context, cred *CredentialView, p *Profile) (bool, error) {
	activated, needsRegion, msg, err := c.GlobalRegisterStatus(ctx, cred, p)
	if err != nil {
		return false, err
	}
	if activated {
		return true, nil
	}
	if !needsRegion {
		return false, fmt.Errorf("注册未激活: %s", msg)
	}
	countries, err := c.GlobalFetchCountries(ctx, cred, p)
	if err != nil {
		return false, err
	}
	if len(countries) == 0 {
		return false, fmt.Errorf("上游没有返回可用的注册地区")
	}
	if err := c.GlobalSubmitRegion(ctx, cred, p, countries[0]); err != nil {
		return false, err
	}
	activated, needsRegion, msg, err = c.GlobalRegisterStatus(ctx, cred, p)
	if err != nil {
		return false, err
	}
	if !activated {
		return false, fmt.Errorf("提交地区后仍未激活: %s", msg)
	}
	return true, nil
}

// GlobalRegions 返回可选注册地区（面板「选择地区」用；只读，失败返回空列表）。
func (c *Client) GlobalRegions(ctx context.Context, cred *CredentialView, p *Profile) []GlobalCountry {
	countries, err := c.GlobalFetchCountries(ctx, cred, p)
	if err != nil {
		return nil
	}
	return countries
}

// ClaimTrial 领取国际站一次性 trial 加油包。
//
// 返回 claimed：true=本次新领到；false=已领过（幂等码 14051，**不是失败**）。
func (c *Client) ClaimTrial(ctx context.Context, cred *CredentialView, p *Profile) (bool, error) {
	if !isGlobal(cred, p) {
		return false, fmt.Errorf("只有国际站账号有 trial 加油包")
	}
	env, _, err := c.doEnvelope(ctx, http.MethodPost, p.Origin+trialPath, webHeaders(cred, p, "web"), strings.NewReader("{}"))
	if err != nil {
		// 已领过：上游返回非 0 业务码，幂等成功。
		if trialAlreadyErr(env.Msg) || trialAlreadyErr(err.Error()) {
			return false, nil
		}
		return false, err
	}
	if trialAlreadyErr(env.Msg) {
		return false, nil
	}
	return true, nil
}

// trialAlreadyErr 判定文案里是否携带幂等码 14051（已领取过）。
func trialAlreadyErr(msg string) bool {
	for _, marker := range trialAlreadyMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
