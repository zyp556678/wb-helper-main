// 桌面端行为指纹上报（切片 18）。
//
// 点亮「需电脑端」类任务的关键**不是独立端点**，而是同一个
// `POST {chatBase}/v2/report` 通道上携带**桌面客户端的指纹**：
//
//	POST https://copilot.tencent.com/v2/report
//	User-Agent: WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1
//	X-Domain / X-Product: SaaS / X-User-Id
//	Body: [ {...event...} ]     ← 数组
//
// 每个事件除业务字段外必带一份公共指纹（ideName/ideType=WorkBuddy、
// extName=workbuddy-desktop、machineId/qimei36 等），指纹里的设备标识由 uid
// **稳定派生**（同一账号每次得到同一设备），模拟一台固定的机器。
//
// # 这套东西的性质与限制（读之前先看这段）
//
// 它是**按参考实现的实测样本复刻的事件形状**，不是官方接口。因此：
//
//  1. 形状是确定的（本文件里每个字段都来自参考实现的抓包样本），
//     但**上游是否接受、是否会计数，本仓库无法离线验证** ——
//     要验证必须拿真实账号发一次并观察任务进度，那会消耗额度。
//  2. 上游对事件链有真实性校验倾向（参考实现记载：它需要消息成功回执），
//     所以**不保证**每个任务都能点亮。
//  3. 因此这条通路在配置里默认**关闭**（`tasks.desktop_events_enabled`），
//     由使用者显式打开 —— 见 internal/tasks 里的开关说明。
package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"workbuddy-gateway/internal/auth"
)

const (
	desktopReportPath    = "/v2/report"
	desktopAppearanceSet = "/v2/user-asset/appearance/set"
	// desktopUA 是实测的桌面客户端 UA（5.5.6 内嵌 CLI 2.137.1）。
	desktopUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
	// desktopWebUA 是 Web 域事件用的浏览器 UA（形状与桌面通道不同，单独一份）。
	desktopWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
)

// desktopUAFor 生成桌面端三段式 UA（按站点切换平台段）。
//
// 平台段不是装饰：`/v3/config` 按 UA 决定下发哪一份模型目录，参考实现明确记载
// 「送错平台段会被 403 或只给精简目录」——国际站账号必须用 `WorkBuddy AI` 段。
// 版本号沿用实测样本（桌面端 5.5.6 / 内嵌 CLI 2.137.1），因为公开可得的桌面端
// 版本号只有这一个样本；CN 与 intl 共用同一份版本号，只换平台段。
func desktopUAFor(site string) string {
	platform := "WorkBuddy"
	if site == auth.SiteINTL {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/5.5.6 " + platform + "/5.5.6 CLI/2.137.1"
}

// DesktopEvent 是一个桌面端事件：业务字段任意，公共指纹由 ReportDesktopEvent 注入。
// 业务字段**优先于**指纹（可用于覆盖 machineId 等做真实设备对齐）。
type DesktopEvent map[string]any

// deriveID 由 uid 稳定派生一个 36 位 hex 设备标识（machineId / sessionId 复用）。
//
// 为什么要派生而不是随机：同一账号每次上报必须是**同一台设备**。
// 每次随机会让同一账号在一天内出现几十个不同 machineId，
// 那本身就是最明显的异常特征。
func deriveID(uid, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 18 字节 = 36 个 hex 字符
}

// desktopFingerprint 返回桌面端公共指纹字段。
func desktopFingerprint(cred *CredentialView, nick string) map[string]any {
	now := time.Now().UnixMilli()
	var uid string
	if cred != nil {
		uid = cred.UID
	}
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       uid,
		"username":     nick,
		"userNickname": nick,
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   "5.5.6",
		"machineId":    deriveID(uid, "machine"),
		"sessionId":    deriveID(uid, "session"),
		"extName":      "workbuddy-desktop",
		"extVersion":   "5.5.6",
		"os":           "win32",
		"arch":         "x64",
		"osVersion":    "10.0.26220",
		"cpuCores":     20,
		"memorySize":   24,
		"timestamp":    now,
		"presentAt":    now,
	}
}

// ReportDesktopEvent 以桌面客户端指纹向 `{chatBase}/v2/report` 批量上报事件。
func (c *Client) ReportDesktopEvent(ctx context.Context, cred *CredentialView, p *Profile, nick string, events ...DesktopEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("桌面事件上报：没有事件")
	}
	if p == nil {
		return fmt.Errorf("桌面事件上报：缺少站点信息")
	}
	fp := desktopFingerprint(cred, nick)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := make(map[string]any, len(fp)+len(ev))
		for k, v := range fp {
			m[k] = v
		}
		for k, v := range ev { // 业务字段覆盖指纹
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	uid := ""
	token := ""
	if cred != nil {
		uid, token = cred.UID, cred.AccessToken
	}
	_, _, err = c.doEnvelope(ctx, http.MethodPost, p.Base+desktopReportPath, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("User-Agent", desktopUA)
		req.Header.Set("X-Domain", p.Base)
		req.Header.Set("X-Product", "SaaS")
		req.Header.Set("X-Request-ID", deriveID(uid, "req"))
		if uid != "" {
			req.Header.Set("X-User-Id", uid)
		}
	}, bytes.NewReader(raw))
	return err
}

// ReportWebEvent 以 Web 域口径上报一个元素点击类事件（资料库介绍等）。
//
// 与桌面通道**不是同一套头与同一个域**：这条走 Origin（Web 控制台）、
// `x-client-platform: web`、浏览器 UA，事件体也更扁（无 ideName 等）。
// 两者混用会被上游按错误的来源解析。
func (c *Client) ReportWebEvent(ctx context.Context, cred *CredentialView, p *Profile, nick, eventCode, pageURL, elementID, elementName string) error {
	if p == nil {
		return fmt.Errorf("Web 事件上报：缺少站点信息")
	}
	var uid, token, entID string
	if cred != nil {
		uid, token, entID = cred.UID, cred.AccessToken, cred.EnterpriseID
	}
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": desktopWebUA,
		"machineId": deriveID(uid, "webmachine"), "userId": uid,
		"userNickname": nick, "enterpriseId": entID,
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	_, _, err = c.doEnvelope(ctx, http.MethodPost, p.Origin+desktopReportPath, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("x-client-platform", "web")
		req.Header.Set("Origin", p.Origin)
		req.Header.Set("Referer", pageURL)
		req.Header.Set("User-Agent", desktopWebUA)
		if uid != "" {
			req.Header.Set("X-User-Id", uid)
		}
	}, bytes.NewReader(raw))
	return err
}

// SetAppearanceTheme 设置外观主题（`kind: theme`）。
//
// 注意：**单独调这个接口不计 Hp_Appearance 分** —— 参考实现记载该任务需要
// 「客户端切主题后的真实活跃」。保留它用于还原主题与后续验证，不当作点亮手段。
func (c *Client) SetAppearanceTheme(ctx context.Context, cred *CredentialView, p *Profile, resourceKey string) error {
	if p == nil {
		return fmt.Errorf("设置主题：缺少站点信息")
	}
	var uid, token string
	if cred != nil {
		uid, token = cred.UID, cred.AccessToken
	}
	body, err := json.Marshal(map[string]string{"kind": "theme", "resource_key": resourceKey})
	if err != nil {
		return err
	}
	_, _, err = c.doEnvelope(ctx, http.MethodPost, p.Base+desktopAppearanceSet, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		req.Header.Set("User-Agent", desktopUA)
		req.Header.Set("X-Product", "SaaS")
		if uid != "" {
			req.Header.Set("X-User-Id", uid)
		}
	}, bytes.NewReader(body))
	return err
}

// -----------------------------------------------------------------------------
// 事件序列构造
//
// 下面这些都是**纯函数**（不碰网络、不看时间之外的状态），所以它们的形状
// 可以在单测里逐字段钉住 —— 这正是「无法验证上游是否接受」时唯一能做的事：
// 至少保证我们发出去的东西和实测样本一致。
// -----------------------------------------------------------------------------

// mkEvent 造一个带 eventCode 的事件。
func mkEvent(code string, extra map[string]any) DesktopEvent {
	ev := DesktopEvent{"eventCode": code}
	for k, v := range extra {
		ev[k] = v
	}
	return ev
}

// DesktopChatSequence 构造一次「桌面端成功对话」的完整事件链。
//
// 顺序本身是语义：agent_task_created → chat_message_send → chat_request_send →
// chat_message_response(isSuccessful=true) → chat_message_status → chat_request_response。
// 中间缺环或顺序错乱会让「这一轮对话」不成立。
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []DesktopEvent {
	assistantID := messageID + "-assistant"
	base := func(extra map[string]any) map[string]any {
		m := map[string]any{
			"traceId": requestID, "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName":            "cli", "agentType": "main",
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	agent := base(nil)
	agent["codebuddy.session_id"] = conversationID
	agent["codebuddy.conversation_request_id"] = requestID

	return []DesktopEvent{
		mkEvent("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mkEvent("chat_message_send", base(map[string]any{
			"messageId": assistantID, "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
		})),
		mkEvent("chat_request_send", func() map[string]any {
			m := base(map[string]any{
				"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
				"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
				"maxSteps": 500, "temperature": 0, "maxRetries": 0,
				"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
				"codebaseId": "", "mentionContextCount": 0, "command": "",
				"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			})
			m["codebuddy.session_id"] = conversationID
			m["codebuddy.conversation_request_id"] = requestID
			return m
		}()),
		mkEvent("chat_message_response", func() map[string]any {
			m := base(map[string]any{
				"messageId": assistantID, "responseModelId": modelID,
				"inputToken": 120, "outputToken": 80, "totalToken": 200,
				"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
				"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
				"firstTokenAt":   time.Now().UnixMilli(),
				"conversationId": conversationID,
			})
			m["codebuddy.session_id"] = conversationID
			m["codebuddy.conversation_request_id"] = requestID
			return m
		}()),
		mkEvent("chat_message_status", base(map[string]any{
			"messageId": assistantID, "messageErrorCode": "0",
		})),
		mkEvent("chat_request_response", base(map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
		})),
	}
}

// DesktopTemplateUseSequence 构造「使用模板创建任务」事件组（对应 template_5 计数）。
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	return append(events,
		mkEvent("agent_task_created_with_template", map[string]any{
			"mode": "working", "isCustomModel": false,
			"id": templateID, "name": templateName, "requestId": requestID,
		}),
		mkEvent("template_used", map[string]any{
			"template_id": templateID, "task_mode": "working",
		}),
	)
}

// DesktopPlaybookPromptSequence 构造「灵感案例做同款」事件组（对应 playbook_prompt）。
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	withPayload := func(code string, extra map[string]any) DesktopEvent {
		m := map[string]any{"eventCode": code}
		for k, v := range extra {
			m[k] = v
		}
		for k, v := range payload {
			m[k] = v
		}
		return m
	}
	return append(events,
		mkEvent("web_element_click", map[string]any{
			"pageName": "playbook_detail", "elementId": "playbook_ctaClick",
			"elementName": caseName, "source": "discover",
		}),
		withPayload("playbook_cta_click", map[string]any{"source": "discover", "position": 0}),
		withPayload("playbook_prompt_send", map[string]any{
			"conversationId": conversationID, "requestId": requestID,
		}),
	)
}

// DesktopDesignCanvasSequence 构造「设计创意画布」事件组（对应 create_canvas）。
func DesktopDesignCanvasSequence(conversationID, requestID string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-canvas", "fast-model", "fast-model")
	// 画布文件 id 取 requestId 末 8 位：上游只当它是一个标识，但**必须是稳定可复现**的，
	// 否则同一轮上报两次会得到两个不同的画布 id。
	suffix := requestID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return append(events,
		mkEvent("wbx_design_canvas_task_create", map[string]any{
			"conversationId": conversationID, "requestId": requestID,
			"source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		}),
		mkEvent("wbx_design_canvas_open", map[string]any{
			"conversationId": conversationID, "requestId": requestID,
			"id": "ardot-file-" + suffix, "source": "summon_keyword",
			"type": "page", "cost": 13000, "isSuccessful": true,
		}),
	)
}

// DesktopBuddyAppSequence 构造「进入 Buddy 应用」五连事件。
//
// 参考实现实测（两账号纯 API 点亮 Buddy_App 与 Buddy_App_QQ）：buddyID 固定用
// 企鹅教师助手 cb_y5Dy46tPQGGWtueMxXbe —— 它同时是 Buddy_App_QQ 的判据应用，
// 所以这一组事件把两个任务一起覆盖（Buddy_App 只要求「进入任一应用」）。
func DesktopBuddyAppSequence(buddyID, buddyName string) []DesktopEvent {
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{
			"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1",
		}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// DesktopAutomationCreateEvent 构造「定时任务创建成功」事件（automation_1 的判据）。
func DesktopAutomationCreateEvent(name string) DesktopEvent {
	return DesktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}
