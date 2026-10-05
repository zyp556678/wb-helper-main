// 小程序口径事件上报（growth 域小程序限定任务）。
//
// 与桌面通道（desktop_report.go）、Web 通道（ReportWebEvent）是**三套不同的指纹**，
// 混用会被上游按错误的来源解析。本文件这套的特征：
//
//	POST {Origin}/v2/report
//	X-Client-Product: workbuddy-mp / X-Client-Version: 2.4.0
//	X-Client-Platform: mp-weixin / X-Platform: wechatmp
//	事件体带 platform=mini_program、extName=workbuddy-mp、ideType=WorkBuddy_MP
//
// 判据形态（参考实现按小程序源码发射点对齐 + 实测）：
//   - 对话类（school_season / Sequential_Tasks_1/3/6）：chat_request_send，
//     服务端按 source=mini_program 指纹关联；school_season 额外要求 activityId。
//   - 专家类（Sequential_Tasks_2）：expert_actual_use，**不带** conversationId/
//     activityId、extVersion=2.2.8、type=send_message —— 与 school 域的专家事件
//     是两套口径，不能照抄。
//   - 灵感类（Sequential_Tasks_7）：playbook_cta_click + playbook_prompt_send。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// schoolOpenDayActivityID 是校园日/开学季的活动 id（事件 activityId 字段值）。
const schoolOpenDayActivityID = "school_open_day_2026"

// mpEventBase 是小程序埋点公共指纹（对齐小程序 app-service 的真实发射形状）。
func mpEventBase(cred *CredentialView, nick string) map[string]any {
	var uid string
	if cred != nil {
		uid = cred.UID
	}
	return map[string]any{
		"timestamp":    time.Now().UnixMilli(),
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   "2.4.0",
		"extName":      "workbuddy-mp",
		"extVersion":   "2.4.0",
		"product":      "SaaS",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"machineId":    deriveID(uid, "mpmachine"),
		"timezone":     "Asia/Shanghai",
		"userId":       uid,
		"userNickname": nick,
	}
}

// ReportMPEvent 以小程序指纹向 `{Origin}/v2/report` 批量上报事件。
func (c *Client) ReportMPEvent(ctx context.Context, cred *CredentialView, p *Profile, nick string, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("小程序事件上报：没有事件")
	}
	if p == nil {
		return fmt.Errorf("小程序事件上报：缺少站点信息")
	}
	base := mpEventBase(cred, nick)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := make(map[string]any, len(base)+len(ev))
		for k, v := range base {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	var uid, token string
	if cred != nil {
		uid, token = cred.UID, cred.AccessToken
	}
	_, _, err = c.doEnvelope(ctx, http.MethodPost, p.Origin+desktopReportPath, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Client-Product", "workbuddy-mp")
		req.Header.Set("X-Client-Version", "2.4.0")
		req.Header.Set("X-Client-Platform", "mp-weixin")
		req.Header.Set("X-Platform", "wechatmp")
		if uid != "" {
			req.Header.Set("X-User-Id", uid)
		}
	}, bytes.NewReader(raw))
	return err
}

// SchoolChatTimesEvents 构造一条小程序对话事件（chat_request_send）。
//
// 字段形状对齐小程序源码的真实发射点：agentName=mp / agentType=main，
// 并且带 codebuddy.session_id / codebuddy.conversation_request_id 两个点号键。
func SchoolChatTimesEvents(conversationID string) map[string]any {
	rid := "wbgw-" + clientToken()
	return map[string]any{
		"eventCode":   "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": "msg-" + rid[len(rid)-8:],
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": rid,
	}
}

// SchoolSeasonChatEvent 构造校园日（school_season）的判据事件：
// 小程序对话事件 + activityId=school_open_day_2026。
//
// **activityId 是必需项**：实测不带 activityId 的同形状事件不点亮该任务
// （服务端按活动 id 关联，而不是按 source 指纹）。
func SchoolSeasonChatEvent(conversationID string) map[string]any {
	ev := SchoolChatTimesEvents(conversationID)
	ev["activityId"] = schoolOpenDayActivityID
	return ev
}

// MiniExpertUseEvent 构造 Sequential_Tasks_2「在小程序内选中专家并完成有效对话」的判据事件。
//
// expertID 必须是专家市场里的真实 ex_ id（空 id 服务端不入账）。
// 形状要点（与 school 域专家事件是两套口径，勿照抄）：
//   - 不带 conversationId / activityId —— 真实事件就是两个字段都不带；
//   - extVersion 用小程序自身版本 2.2.8（覆盖 mpEventBase 的 2.4.0）；
//   - type 固定 "send_message"。
func MiniExpertUseEvent(expertID, expertName, expertType string) map[string]any {
	if expertType == "" {
		expertType = "agent"
	}
	if expertName == "" {
		expertName = expertID
	}
	return map[string]any{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": "2.2.8", "source": "mini_program",
		"id": expertID, "name": expertID,
		"expertTitle": expertName, "type": "send_message",
		"characterCount": 12, "expertType": expertType,
	}
}

// MiniChatModelEvent 构造「带模型字段」的小程序对话事件（Sequential_Tasks_5 的判据载体）。
//
// 与裸对话事件的差别只有 requestModelId / requestModelName：
// Tasks_1/3 的事件不带模型，模型任务必须用本形态。
func MiniChatModelEvent(conversationID, modelID, modelName string) map[string]any {
	ev := SchoolChatTimesEvents(conversationID)
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName
	return ev
}

// MiniPlaybookEvents 构造小程序指纹的灵感事件组（Sequential_Tasks_7 的判据载体）。
//
// 形状对齐小程序源码的两个发射点：playbook_cta_click → playbook_prompt_send。
func MiniPlaybookEvents(caseID, caseName string) []map[string]any {
	base := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
		"skills": "", "skillNames": "",
	}
	withBase := func(ev map[string]any) map[string]any {
		for k, v := range base {
			ev[k] = v
		}
		return ev
	}
	cta := withBase(map[string]any{
		"eventCode": "playbook_cta_click", "source": "discover", "position": 1,
		"extVersion": "2.2.8",
	})
	send := withBase(map[string]any{
		"eventCode": "playbook_prompt_send", "source": "discover",
		"promptLength": 96, "isOfficial": 1,
		"conversationId": "wbgw-mp-pb-" + clientToken(),
		"extVersion":     "2.2.8",
	})
	return []map[string]any{cta, send}
}
