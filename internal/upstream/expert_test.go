package upstream

import (
	"strings"
	"testing"
)

// 专家召唤链的三个事件缺一不可，顺序也不能乱：
// 上游把「点了召唤」认成这条三连，缺环时 expert_actual_use 不成立。
func TestDesktopExpertSummonSequence(t *testing.T) {
	e := MarketExpert{
		ExpertID: "ex_abc", ExpertType: "agent", DisplayNameZH: "测试专家",
		ProfessionZH: "法务专家", Version: "1.2.3",
		Categories: []any{"legal"},
	}
	events := DesktopExpertSummonSequence(e)
	if len(events) != 3 {
		t.Fatalf("召唤链应为 3 个事件，实际 %d", len(events))
	}
	wantCodes := []string{"web_element_click", "expert_summon_click", "expert_summoned"}
	for i, code := range wantCodes {
		if events[i]["eventCode"] != code {
			t.Fatalf("第 %d 个事件应为 %s，实际 %v", i, code, events[i]["eventCode"])
		}
	}
	// 专家 id 必须出现在三个事件里 —— 上游按它关联「召唤了谁」。
	for i, ev := range events {
		if i == 0 {
			if ev["source"] != e.ExpertID {
				t.Fatalf("web_element_click 的 source 应为专家 id，实际 %v", ev["source"])
			}
			continue
		}
		if ev["id"] != e.ExpertID {
			t.Fatalf("第 %d 个事件的 id 应为专家 id，实际 %v", i, ev["id"])
		}
	}
	// 分类取自 Categories[0]，没有分类时回落 expert-all。
	// 注意 web_element_click 用分类值，而 summon_click/summoned 两条在真实样本里
	// 固定写 expert-all —— 这是抓包样本的形状，不是笔误。
	if events[0]["type"] != "legal" {
		t.Fatalf("web_element_click 的 type 应取自 categories[0]，实际 %v", events[0]["type"])
	}
	if events[1]["type"] != "expert-all" || events[2]["type"] != "expert-all" {
		t.Fatalf("summon 两条事件的 type 应为样本里的 expert-all：%v / %v",
			events[1]["type"], events[2]["type"])
	}
	noCat := DesktopExpertSummonSequence(MarketExpert{ExpertID: "ex_x"})
	if noCat[0]["type"] != "expert-all" {
		t.Fatalf("无分类时 type 应为 expert-all，实际 %v", noCat[0]["type"])
	}
}

// expert_actual_use 必须把**服务端 requestId** 原样带进三个关联字段；
// 自造 id 上游不计数，而且不会报错。
func TestDesktopExpertActualUseCarriesRequestID(t *testing.T) {
	e := MarketExpert{ExpertID: "ex_abc", ExpertType: "team", DisplayNameZH: "团队"}
	const conv = "conv-1"
	const req = "cmb-0123456789abcdef0123456789abcdef"
	ev := DesktopExpertActualUseEvent(e, conv, req)
	if ev["requestId"] != req || ev["conversationId"] != conv {
		t.Fatalf("必须带真实 conversationId/requestId：%v", ev)
	}
	if ev["messageId"] != "msg-"+req[len(req)-8:] {
		t.Fatalf("messageId 应由 requestId 末 8 位派生，实际 %v", ev["messageId"])
	}
	if ev["mode"] != "craft" {
		t.Fatalf("默认变体 mode 应为 craft，实际 %v", ev["mode"])
	}
	// 轻量云专家要求 LOCAL 口径（对齐真实样本）。
	if local := DesktopExpertActualUseLocal(e, conv, req); local["mode"] != "LOCAL" {
		t.Fatalf("LOCAL 变体 mode 应为 LOCAL，实际 %v", local["mode"])
	}
}

// skill_1 的判据是「真实对话 + skill_info」，且对话必须带上工具调用语义
// （finishReason=tool_calls）—— 只发 skill_info 不 JOIN 会话不计数。
func TestDesktopSkillUseSequence(t *testing.T) {
	const req = "cmb-0123456789abcdef0123456789abcdef"
	events := DesktopSkillUseSequence("conv-1", req, "skill-1", "技能名", "1.0.0")
	var sawToolCalls, sawSkill bool
	for _, ev := range events {
		switch ev["eventCode"] {
		case "chat_message_response":
			if ev["finishReason"] == "tool_calls" {
				sawToolCalls = true
			}
		case "skill_info":
			sawSkill = true
			if ev["skillId"] != "skill-1" || ev["toolStatus"] != "success" {
				t.Fatalf("skill_info 字段不对：%v", ev)
			}
			if ev["requestId"] != req || ev["conversationId"] != "conv-1" {
				t.Fatalf("skill_info 必须 JOIN 真实会话：%v", ev)
			}
		}
	}
	if !sawToolCalls {
		t.Fatal("对话链里必须有一条 finishReason=tool_calls 的响应（技能加载语义）")
	}
	if !sawSkill {
		t.Fatal("必须追加 skill_info 事件")
	}
}

// SSE 里 `"id":"` 会先出现在消息 id 等字段上：解析器必须能跳过它们，
// 只在偏移前进时继续找，否则会读满上限后误报「未找到」。
func TestServerRequestIDFromSSE(t *testing.T) {
	const want = "cmb-0123456789abcdef0123456789abcdef"
	stream := strings.Join([]string{
		`data: {"id":"msg-1","object":"chat.completion.chunk"}`,
		`data: {"id":"conv-not-a-request-id"}`,
		`data: {"id":"` + want + `","choices":[]}`,
		`data: [DONE]`,
	}, "\n\n")
	got, err := serverRequestIDFromSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("应能解析出 requestId: %v", err)
	}
	if got != want {
		t.Fatalf("解析到的 requestId 不对：%q", got)
	}
}

// 没有合法 id 时必须报错而不是返回空串：空 id 组出来的专家事件会被上游静默丢弃。
func TestServerRequestIDFromSSEMissing(t *testing.T) {
	stream := `data: {"id":"msg-1"}` + "\n\n" + `data: {"id":"short"}` + "\n\n"
	if _, err := serverRequestIDFromSSE(strings.NewReader(stream)); err == nil {
		t.Fatal("没有合法 requestId 时应返回错误")
	}
}

// id 形状校验：cmb- 前缀 32hex 或裸 32hex，其余一律不认。
func TestServerRequestIDPattern(t *testing.T) {
	ok := []string{
		"cmb-0123456789abcdef0123456789abcdef",
		"0123456789abcdef0123456789abcdef",
	}
	for _, s := range ok {
		if !serverRequestIDPattern.MatchString(s) {
			t.Fatalf("%q 应被认作 requestId", s)
		}
	}
	bad := []string{"", "msg-1", "cmb-0123", "cmb-0123456789ABCDEF0123456789abcdef", "cmb-0123456789abcdef0123456789abcde"}
	for _, s := range bad {
		if serverRequestIDPattern.MatchString(s) {
			t.Fatalf("%q 不应被认作 requestId", s)
		}
	}
}
