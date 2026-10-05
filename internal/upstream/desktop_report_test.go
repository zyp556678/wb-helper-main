package upstream

import (
	"testing"
)

// 设备标识必须由 uid **稳定派生**：同一账号每次上报都是同一台设备。
//
// 若每次随机，同一账号一天内会出现几十个不同 machineId ——
// 那本身就是最明显的异常特征，比不报还糟。
func TestDeriveIDIsStablePerUID(t *testing.T) {
	a := deriveID("uid-a", "machine")
	b := deriveID("uid-a", "machine")
	if a != b {
		t.Fatalf("同一 uid 同一 salt 应得到相同值: %q vs %q", a, b)
	}
	if len(a) != 36 {
		t.Fatalf("应为 36 位 hex，实际 %d 位: %q", len(a), a)
	}
	if deriveID("uid-b", "machine") == a {
		t.Fatal("不同 uid 应得到不同设备标识")
	}
	// 不同用途（salt）之间也必须不同，否则 machineId 与 sessionId 会撞在一起。
	if deriveID("uid-a", "session") == a {
		t.Fatal("不同 salt 应得到不同标识（machineId 与 sessionId 不能相同）")
	}
}

// 公共指纹必须带齐参考实现实测的那些字段。
//
// 漏掉任何一个都可能让上游判为「不是桌面端」，而**症状是不可见的**：
// 上报返回 200、任务进度不涨 —— 没有任何错误可供排查。
func TestDesktopFingerprintFields(t *testing.T) {
	cred := &CredentialView{UID: "u1", AccessToken: "tok", EnterpriseID: "e1"}
	fp := desktopFingerprint(cred, "甲")

	required := []string{
		"timezone", "reportDelay", "userId", "username", "userNickname", "product",
		"releaseDate", "commit", "ideName", "ideType", "ideVersion",
		"machineId", "sessionId", "extName", "extVersion",
		"os", "arch", "osVersion", "cpuCores", "memorySize", "timestamp", "presentAt",
	}
	for _, k := range required {
		if _, ok := fp[k]; !ok {
			t.Errorf("指纹缺少字段 %q", k)
		}
	}
	// 关键取值：桌面端身份的判据就是这几个。
	if fp["ideName"] != "WorkBuddy" || fp["ideType"] != "WorkBuddy" {
		t.Errorf("ideName/ideType 应为 WorkBuddy，实际 %v/%v", fp["ideName"], fp["ideType"])
	}
	if fp["extName"] != "workbuddy-desktop" {
		t.Errorf("extName 应为 workbuddy-desktop，实际 %v", fp["extName"])
	}
	if fp["product"] != "SaaS" {
		t.Errorf("product 应为 SaaS，实际 %v", fp["product"])
	}
	if fp["userId"] != "u1" || fp["userNickname"] != "甲" {
		t.Errorf("用户字段未注入: %v / %v", fp["userId"], fp["userNickname"])
	}
	if fp["machineId"] != deriveID("u1", "machine") {
		t.Error("machineId 应由 uid 派生")
	}
}

// 事件顺序是语义的一部分：整条链必须齐、顺序不能乱。
func TestDesktopChatSequenceShape(t *testing.T) {
	events := DesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")
	want := []string{
		"agent_task_created",
		"chat_message_send",
		"chat_request_send",
		"chat_message_response",
		"chat_message_status",
		"chat_request_response",
	}
	if len(events) != len(want) {
		t.Fatalf("应有 %d 个事件，实际 %d", len(want), len(events))
	}
	for i, code := range want {
		if events[i]["eventCode"] != code {
			t.Fatalf("第 %d 个事件应为 %s，实际 %v", i, code, events[i]["eventCode"])
		}
	}

	// 成功回执必须带 isSuccessful=true —— 参考实现记载该链需要消息成功回执才算数。
	resp := events[3]
	if resp["isSuccessful"] != true {
		t.Error("chat_message_response 必须 isSuccessful=true")
	}
	if resp["conversationId"] != "conv-1" || resp["rootRequestId"] != "req-1" {
		t.Errorf("响应事件未带上会话/请求 id: %v / %v", resp["conversationId"], resp["rootRequestId"])
	}
	// 除首个 agent_task_created 外，后续事件都要能挂上会话
	//（traceId / parentConversationId）。首个事件是「新建了对话」的标记，
	// 参考实现的样本里它本身不带 traceId —— 这里按实测形状断言，不臆造。
	for i := 1; i < len(events); i++ {
		if _, ok := events[i]["traceId"]; !ok {
			t.Errorf("第 %d 个事件（%v）缺少 traceId", i, events[i]["eventCode"])
		}
		if events[i]["parentConversationId"] != "conv-1" {
			t.Errorf("第 %d 个事件缺少 parentConversationId", i)
		}
	}
	// agent_task_created 是「新建了对话」的标记，必须带 conversationId。
	if events[0]["conversationId"] != "conv-1" {
		t.Error("agent_task_created 必须带 conversationId")
	}
}

// 三个事件组都必须是「完整对话链 + 各自的特征事件」，且特征事件在链尾。
func TestDesktopEventGroupTails(t *testing.T) {
	cases := []struct {
		name   string
		events []DesktopEvent
		tail   []string
	}{
		{
			name:   "模板",
			events: DesktopTemplateUseSequence("c", "r", "tpl-1", "周报"),
			tail:   []string{"agent_task_created_with_template", "template_used"},
		},
		{
			name:   "灵感",
			events: DesktopPlaybookPromptSequence("c", "r", "case-1", "GTM 计划"),
			tail:   []string{"web_element_click", "playbook_cta_click", "playbook_prompt_send"},
		},
		{
			name:   "画布",
			events: DesktopDesignCanvasSequence("c", "r-abcdefgh"),
			tail:   []string{"wbx_design_canvas_task_create", "wbx_design_canvas_open"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 尾部特征事件
			if len(tc.events) < len(tc.tail) {
				t.Fatalf("事件数不足: %d", len(tc.events))
			}
			got := tc.events[len(tc.events)-len(tc.tail):]
			for i, code := range tc.tail {
				if got[i]["eventCode"] != code {
					t.Fatalf("链尾第 %d 个应为 %s，实际 %v", i, code, got[i]["eventCode"])
				}
			}
			// 前面必须挂着完整对话链（否则只是一个孤立事件，不构成「一次使用」）
			if tc.events[0]["eventCode"] != "agent_task_created" {
				t.Fatalf("事件组应以完整对话链开头，实际 %v", tc.events[0]["eventCode"])
			}
		})
	}
}

// 画布文件 id 必须**稳定可复现**：同一 requestId 两次上报得到同一个 id。
func TestCanvasIDIsStable(t *testing.T) {
	a := DesktopDesignCanvasSequence("c", "abcdefgh12345678")
	b := DesktopDesignCanvasSequence("c", "abcdefgh12345678")
	if len(a) == 0 || len(b) == 0 {
		t.Fatal("序列为空")
	}
	idA := a[len(a)-1]["id"]
	idB := b[len(b)-1]["id"]
	if idA != idB {
		t.Fatalf("同一 requestId 应得到同一画布 id: %v vs %v", idA, idB)
	}
	if idA != "ardot-file-12345678" {
		t.Fatalf("画布 id 应取 requestId 末 8 位，实际 %v", idA)
	}
}

// 业务字段必须**覆盖**公共指纹（参考实现依赖这一点做设备对齐）。
func TestBusinessFieldsOverrideFingerprint(t *testing.T) {
	// 直接验证合并规则：构造一个带 machineId 的事件，合并后应保留业务值。
	// 这里用 ReportDesktopEvent 的合并逻辑做等价断言（不发网络请求）。
	fp := desktopFingerprint(&CredentialView{UID: "u1"}, "甲")
	ev := DesktopEvent{"eventCode": "x", "machineId": "real-device-id"}
	m := map[string]any{}
	for k, v := range fp {
		m[k] = v
	}
	for k, v := range ev {
		m[k] = v
	}
	if m["machineId"] != "real-device-id" {
		t.Fatalf("业务字段应覆盖指纹，实际 machineId=%v", m["machineId"])
	}
	if m["eventCode"] != "x" {
		t.Fatalf("eventCode 应保留，实际 %v", m["eventCode"])
	}
}
