package upstream

import "testing"

// 校园日（school_season）的判据必须带 activityId：
// 实测不带 activityId 的同形状事件不点亮该任务。
func TestSchoolSeasonChatEventHasActivityID(t *testing.T) {
	ev := SchoolSeasonChatEvent("conv-1")
	if ev["activityId"] != schoolOpenDayActivityID {
		t.Fatalf("school_season 事件必须带 activityId，实际 %v", ev["activityId"])
	}
	// 其余字段与普通小程序对话事件同构。
	base := SchoolChatTimesEvents("conv-1")
	for _, key := range []string{"eventCode", "agentName", "agentType", "conversationId"} {
		if ev[key] != base[key] {
			t.Fatalf("字段 %s 应与基础事件一致：%v vs %v", key, ev[key], base[key])
		}
	}
	// Sequential_Tasks_1/3/6 用的事件**不带** activityId。
	if _, ok := base["activityId"]; ok {
		t.Fatal("基础小程序对话事件不应带 activityId")
	}
}

// 小程序对话事件必须带两个点号键：小程序源码就是这么发的，
// 少一个都可能让服务端认不出 source=mini_program。
func TestSchoolChatTimesEventsShape(t *testing.T) {
	ev := SchoolChatTimesEvents("conv-9")
	if ev["eventCode"] != "chat_request_send" {
		t.Fatalf("eventCode 不对：%v", ev["eventCode"])
	}
	if ev["agentName"] != "mp" || ev["agentType"] != "main" {
		t.Fatalf("小程序对话事件的 agent 字段应为 mp/main：%v", ev)
	}
	if ev["codebuddy.session_id"] != "conv-9" {
		t.Fatalf("缺 codebuddy.session_id：%v", ev)
	}
	if ev["codebuddy.conversation_request_id"] == nil {
		t.Fatalf("缺 codebuddy.conversation_request_id：%v", ev)
	}
	if ev["parentConversationId"] != "conv-9" {
		t.Fatalf("parentConversationId 应为会话 id：%v", ev["parentConversationId"])
	}
}

// Sequential_Tasks_2 的判据是 mp 指纹 expert_actual_use，与 school 域那套是两套口径：
// 不带 conversationId/activityId、extVersion=2.2.8、type=send_message。
func TestMiniExpertUseEventShape(t *testing.T) {
	ev := MiniExpertUseEvent("ex_abc", "法务专家", "")
	if ev["eventCode"] != "expert_actual_use" {
		t.Fatalf("eventCode 不对：%v", ev["eventCode"])
	}
	if ev["id"] != "ex_abc" {
		t.Fatalf("id 必须是市场真实专家 id：%v", ev["id"])
	}
	if ev["extVersion"] != "2.2.8" {
		t.Fatalf("extVersion 应为小程序自身版本 2.2.8，实际 %v", ev["extVersion"])
	}
	if ev["type"] != "send_message" {
		t.Fatalf("type 应固定为 send_message，实际 %v", ev["type"])
	}
	if ev["source"] != "mini_program" {
		t.Fatalf("source 应为 mini_program，实际 %v", ev["source"])
	}
	if ev["expertType"] != "agent" {
		t.Fatalf("expertType 缺省应为 agent，实际 %v", ev["expertType"])
	}
	for _, forbidden := range []string{"conversationId", "activityId"} {
		if _, ok := ev[forbidden]; ok {
			t.Fatalf("mp 专家事件不应带 %s：%v", forbidden, ev)
		}
	}
}

// Sequential_Tasks_5 的载体是「带模型字段」的对话事件，其余与裸事件同构。
func TestMiniChatModelEvent(t *testing.T) {
	ev := MiniChatModelEvent("conv-1", "glm-5.2", "GLM-5.2")
	if ev["requestModelId"] != "glm-5.2" || ev["requestModelName"] != "GLM-5.2" {
		t.Fatalf("模型字段没带上：%v", ev)
	}
	if ev["eventCode"] != "chat_request_send" {
		t.Fatalf("eventCode 不对：%v", ev["eventCode"])
	}
}

// Sequential_Tasks_7 的载体是两条事件，顺序是「点击 → 发送」。
func TestMiniPlaybookEvents(t *testing.T) {
	events := MiniPlaybookEvents("case-1", "案例名")
	if len(events) != 2 {
		t.Fatalf("应为 2 条事件，实际 %d", len(events))
	}
	if events[0]["eventCode"] != "playbook_cta_click" || events[1]["eventCode"] != "playbook_prompt_send" {
		t.Fatalf("事件顺序应为 cta_click → prompt_send：%v", events)
	}
	for _, ev := range events {
		if ev["id"] != "case-1" || ev["name"] != "案例名" {
			t.Fatalf("案例字段没带上：%v", ev)
		}
	}
	if events[1]["conversationId"] == "" || events[1]["conversationId"] == nil {
		t.Fatal("prompt_send 必须带 conversationId")
	}
}

// 小程序公共指纹：平台字段是「小程序」而不是桌面 —— 发错通道上游按错误来源解析。
func TestMPEventBaseFingerprint(t *testing.T) {
	cred := &CredentialView{UID: "u1"}
	fp := mpEventBase(cred, "甲")
	checks := map[string]any{
		"extName": "workbuddy-mp", "platform": "mini_program", "ideType": "WorkBuddy_MP",
		"product": "SaaS", "userId": "u1", "userNickname": "甲",
	}
	for k, want := range checks {
		if fp[k] != want {
			t.Fatalf("指纹 %s 应为 %v，实际 %v", k, want, fp[k])
		}
	}
	// machineId 与桌面通道**必须不同**（各自稳定派生），否则两条通道会共用一台假设备。
	if fp["machineId"] == deriveID("u1", "machine") {
		t.Fatal("小程序 machineId 不应与桌面通道相同")
	}
}
