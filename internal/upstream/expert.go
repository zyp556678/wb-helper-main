package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

// -----------------------------------------------------------------------------
// 专家市场与「召唤 + 使用」链路
//
// expert_5 / Expert_team_use_3 / Expert_lighthouse 这几个任务的判据是
// `expert_actual_use` 事件，而它有两条硬性要求（参考实现三账号实测）：
//
//  1. `id` 必须是**专家市场里真实存在的专家** —— 编造的 id 服务端不入账；
//  2. `requestId` 必须是**真实 chat 请求的服务端 id**（`cmb-<32hex>` 或裸 32hex），
//     自造 UUID 同样不计数。
//
// 所以链路是固定的三步：市场列表拿真实 id → 发一次真实 chat 从 SSE 里取服务端
// requestId → 用这个 requestId 组装 expert_actual_use。少一步都点不亮。
//
// 本文件只做「取 id / 发真实对话 / 组装事件」这三件事，不做任务判定。
// -----------------------------------------------------------------------------

// marketExpertListPath 是专家市场列表端点（相对 Base，即对话域）。
const marketExpertListPath = "/portal/operation-platform/market/expert/list"

// MarketExpert 是专家市场里的一个专家（响应子集）。
type MarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
}

// expertDisplayName 是展示名的兜底：display_name_zh 为空时用职业名，再空用 id。
func (e MarketExpert) expertDisplayName() string {
	if e.DisplayNameZH != "" {
		return e.DisplayNameZH
	}
	if e.ProfessionZH != "" {
		return e.ProfessionZH
	}
	return e.ExpertID
}

// expertCategory 取分类（事件的 type 字段）；没有分类时用 "expert-all"。
func (e MarketExpert) expertCategory() string {
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok && s != "" {
			return s
		}
	}
	return "expert-all"
}

// expertVersion 取版本；空时回落 1.0.0。
func (e MarketExpert) expertVersion() string {
	if e.Version != "" {
		return e.Version
	}
	return "1.0.0"
}

// desktopChatHeaders 是桌面客户端发对话请求时的头族。
//
// 与反代转发用的 BackendHeaders **刻意不同**：这里是**冒充桌面客户端**发一条
// 自己发起的对话（带 X-Expert-Id / X-IDE-* / x-codebuddy-request），
// 反代那条路是转发用户的真实请求。两套头的形状不能互相污染。
func desktopChatHeaders(req *http.Request, cred *CredentialView, p *Profile, conversationID, requestID, expertID string) {
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", desktopUA)
	h.Set("X-Domain", p.Base)
	h.Set("X-Product", "SaaS")
	h.Set("X-Conversation-ID", conversationID)
	h.Set("X-Request-ID", requestID)
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Type", "main")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Version", "5.5.6")
	h.Set("x-codebuddy-request", "1")
	if cred != nil {
		if cred.AccessToken != "" {
			h.Set("Authorization", "Bearer "+cred.AccessToken)
		}
		if cred.UID != "" {
			h.Set("X-User-Id", cred.UID)
		}
		if cred.EnterpriseID != "" {
			h.Set("X-Enterprise-Id", cred.EnterpriseID)
		}
		if cred.Domain != "" {
			h.Set("X-Domain", cred.Domain)
		}
	}
	if expertID != "" {
		h.Set("X-Expert-Id", expertID)
	}
}

// serverRequestIDPattern 是服务端 requestId 的形状（cmb- 前缀 32hex 或裸 32hex）。
//
// 用它过滤 SSE 里的其它 id（消息 id、会话 id 等），只认真正的请求 id ——
// 拿错一个 id 组出来的 expert_actual_use 会被上游丢弃，而且**不报错**。
var serverRequestIDPattern = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// MarketExpertList 拉取专家市场真实专家列表（expertType: "agent" 单专家 / "team" 专家团）。
//
// expertType 为空时返回全部类型（上游默认排序）。
func (c *Client) MarketExpertList(ctx context.Context, cred *CredentialView, p *Profile, expertType string) ([]MarketExpert, error) {
	if p == nil {
		return nil, fmt.Errorf("专家市场：缺少站点信息")
	}
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Experts []MarketExpert `json:"experts"`
	}
	// doJSON 返回的是包络的 data 字段，再解一层才是 experts。
	data, _, err := c.doJSON(ctx, http.MethodPost, p.Base+marketExpertListPath, func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", desktopUA)
		req.Header.Set("X-Domain", p.Base)
		req.Header.Set("X-Product", "SaaS")
		if cred != nil {
			if cred.AccessToken != "" {
				req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
			}
			if cred.UID != "" {
				req.Header.Set("X-User-Id", cred.UID)
			}
		}
	}, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("专家市场列表解析失败: %w", err)
	}
	return out.Experts, nil
}

// DesktopChatWithExpert 发一条真实桌面指纹对话（可带 X-Expert-Id），
// 从 SSE 流里解析**服务端返回的 requestId** 并返回。
//
// 读干流：不读完会在上游留下一条悬空连接，且下次复用连接时可能读到上一轮的残留数据。
func (c *Client) DesktopChatWithExpert(ctx context.Context, cred *CredentialView, p *Profile, expertID string) (conversationID, requestID string, err error) {
	if p == nil {
		return "", "", fmt.Errorf("专家对话：缺少站点信息")
	}
	conversationID = fmt.Sprintf("wbgw-conv-%d", time.Now().UnixNano())
	body := map[string]any{
		"model": "fast-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": "1+1等于几？直接回答。"},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.ChatURL(), bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	desktopChatHeaders(req, cred, p, conversationID, fmt.Sprintf("%d", time.Now().UnixNano()), expertID)
	resp, err := c.ChatHTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", fmt.Errorf("专家对话 HTTP %d: %s", resp.StatusCode, truncate(string(b), 160))
	}
	id, err := serverRequestIDFromSSE(resp.Body)
	if err != nil {
		return "", "", err
	}
	return conversationID, id, nil
}

// serverRequestIDFromSSE 从 SSE 流里抓第一个形如服务端 requestId 的 `"id":"..."`。
//
// 搜索偏移必须**只前进**：SSE 里 `"id":"` 会先出现在消息 id 等字段上，
// 若每次从头搜，首个不匹配的 id 会让循环永远命中同一位置，
// 读满上限后误报「未找到」。
func serverRequestIDFromSSE(r io.Reader) (string, error) {
	const maxBytes = 1 << 20
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 8192)
	searchFrom := 0
	for {
		n, rerr := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				i := bytes.Index(buf[searchFrom:], []byte(`"id":"`))
				if i < 0 {
					break
				}
				abs := searchFrom + i
				rest := buf[abs+6:]
				end := bytes.IndexByte(rest, '"')
				if end < 0 {
					// id 还没读全，下次循环再试。
					break
				}
				id := string(rest[:end])
				if serverRequestIDPattern.MatchString(id) {
					return id, nil
				}
				searchFrom = abs + 1
			}
			// 只在「确定不会再有更早的匹配」时推进下界，避免把跨包边界的 id 切掉。
			if searchFrom < len(buf)-64 {
				searchFrom = len(buf) - 64
			}
		}
		if rerr != nil || len(buf) > maxBytes {
			break
		}
	}
	return "", fmt.Errorf("SSE 中未找到服务端 requestId")
}

// DesktopExpertSummonSequence 构造「召唤平台专家」事件组。
//
// 载荷对齐真实抓包样本：web_element_click(expert_summon_click) →
// expert_summon_click → expert_summoned。三者是「用户点了召唤」的完整语义，
// 缺环时 expert_actual_use 不成立。
func DesktopExpertSummonSequence(e MarketExpert) []DesktopEvent {
	name := e.expertDisplayName()
	cat := e.expertCategory()
	ver := e.expertVersion()
	return []DesktopEvent{
		{
			"eventCode": "web_element_click", "source": e.ExpertID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": name,
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": ver, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": name,
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent 构造 expert_actual_use（expert_5 / Expert_team_use_3 的判据）。
// requestID 必须是 DesktopChatWithExpert 返回的服务端 requestId。
func DesktopExpertActualUseEvent(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "craft"
	return ev
}

// DesktopExpertActualUseLocal 是 mode:"LOCAL" 变体（Expert_lighthouse 的判据要求）。
//
// 对齐真实样本：轻量云专家使用时 mode=LOCAL、type 为空、cost=0。
// 调用方负责把那两个字段改成样本值（本函数只保证 mode）。
func DesktopExpertActualUseLocal(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "LOCAL"
	return ev
}

// desktopExpertActualUse 是 expert_actual_use 的公共载荷。
func desktopExpertActualUse(e MarketExpert, conversationID, requestID string) DesktopEvent {
	suffix := requestID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return DesktopEvent{
		"eventCode": "expert_actual_use",
		"id":        e.ExpertID, "name": e.expertDisplayName(), "expertTitle": e.ProfessionZH,
		"type": e.expertCategory(), "expertType": e.ExpertType, "source": "builtin",
		"version": e.expertVersion(), "cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID, "messageId": "msg-" + suffix,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}

// -----------------------------------------------------------------------------
// skill_1：真实对话 + skill_info 技能加载事件
// -----------------------------------------------------------------------------

// skillInfoEvent 构造 skill_info 事件（skill_1 的判据）。
//
// 判据来自真实抓包：`skill_info` 事件必须 **JOIN 一次真实会话**
// （conversationId / requestId 是服务端 id），且 toolStatus=success。
// 此前试过的 skill_request_send / skill_installed / skill_action 都是错误方向。
func skillInfoEvent(conversationID, requestID, skillID, skillName, skillVersion string) DesktopEvent {
	suffix := requestID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return DesktopEvent{
		"eventCode": "skill_info",
		"id":        skillName, "skillId": skillID, "skillVersion": skillVersion,
		"toolStatus": "success", "fileCount": 56, "source": "workbuddy-desktop",
		"conversationId": conversationID, "requestId": requestID, "messageId": "msg-" + suffix,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": requestID,
	}
}

// DesktopSkillUseSequence 构造「真实对话 + 技能加载」事件组（skill_1 的判据载体）。
//
// 与其它事件组的差别：chat_message_response 的 finishReason 改成 tool_calls
// （语义是「模型发起了工具调用」，也就是技能被加载），再追加 skill_info。
func DesktopSkillUseSequence(conversationID, requestID, skillID, skillName, skillVersion string) []DesktopEvent {
	suffix := requestID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	events := DesktopChatSequence(conversationID, requestID, "msg-"+suffix, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls"
		}
	}
	return append(events, skillInfoEvent(conversationID, requestID, skillID, skillName, skillVersion))
}
