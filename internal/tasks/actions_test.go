package tasks

import (
	"testing"
	"time"

	"workbuddy-gateway/internal/upstream"
)

// mustHour 造一个当天指定小时的时间点（窗口判断只看小时）。
func mustHour(h int) time.Time {
	return time.Date(2026, 10, 5, h, 30, 0, 0, time.Local)
}

// 自动动作表的**登记完整性**：每个自动化的 task_code 只能出现一次，
// 且复杂动作必须带 run、纯上报动作必须带 times —— 两条都缺的动作跑起来
// 不会报错，只会「什么都没做然后汇报成功」，那是最难发现的一类坏。
func TestAutoActionTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range autoActions {
		if a.code == "" {
			t.Fatal("动作表里有空 task_code")
		}
		if seen[a.code] {
			t.Fatalf("task_code 重复登记：%s", a.code)
		}
		seen[a.code] = true
		if a.desc == "" {
			t.Fatalf("%s 缺说明文案（面板要展示给用户）", a.code)
		}
		switch a.mode {
		case modeReport, modeRealChat, modeBuddy, modeDesktopEvent, modeMP:
		default:
			t.Fatalf("%s 的 mode 未知：%q", a.code, a.mode)
		}
		if a.run == nil && a.times == nil {
			t.Fatalf("%s 既没有自定义链路也没有条数计算，跑起来会什么都不做", a.code)
		}
	}
}

// 参考实现已实测可点亮的那些任务，本项目必须都登记了动作 ——
// 少一条就是「用户以为能一键完成，实际它静静躺在需客户端那一档」。
func TestAutoActionTableCoversKnownTasks(t *testing.T) {
	want := []string{
		"chat_5", "first_buddy", "RichMeow_Chat", "Model_chat_GLM5.2", "black_cat",
		"template_5", "playbook_prompt", "create_canvas", "Library_read", "Hp_Appearance",
		"Buddy_App", "Buddy_App_QQ", "automation_1",
		// 专家系（真实召唤 + 真实对话 requestId）
		"expert_5", "Expert_team_use_3", "Expert_lighthouse", "skill_1",
		// 小程序口径
		"school_season", "Sequential_Tasks_1", "Sequential_Tasks_2", "Sequential_Tasks_3",
		"Sequential_Tasks_4", "Sequential_Tasks_5", "Sequential_Tasks_6", "Sequential_Tasks_7",
	}
	have := map[string]bool{}
	for _, a := range autoActions {
		have[a.code] = true
	}
	for _, code := range want {
		if !have[code] {
			t.Fatalf("自动动作表缺少 %s", code)
		}
	}
}

// 小程序专属任务表的登记：这些码在默认（无 mp 头）列表里查不到，
// 回读/接受/领奖都必须走 mp 变体，所以判定必须准确。
func TestMPTaskCodeRegistration(t *testing.T) {
	mpCodes := []string{
		"school_season", "Sequential_Tasks_1", "Sequential_Tasks_2", "Sequential_Tasks_3",
		"Sequential_Tasks_4", "Sequential_Tasks_5", "Sequential_Tasks_6", "Sequential_Tasks_7",
	}
	for _, code := range mpCodes {
		if !IsMPTaskCode(code) {
			t.Fatalf("%s 应登记为小程序口径专属", code)
		}
	}
	for _, code := range []string{"chat_5", "expert_5", "first_buddy", ""} {
		if IsMPTaskCode(code) {
			t.Fatalf("%s 不应被当成小程序任务", code)
		}
	}
}

// 「伪造客户端行为」的动作默认必须关着：开关关掉时 actionFor 返回 nil，
// 这些 code 会自然落进「需客户端」分类，界面提示与开关状态因此永远一致。
//
// 开关判断是这条约定的**唯一执行点**，所以单独钉住。
func TestGatedActionsRespectSwitch(t *testing.T) {
	off := newTestManager(t)
	on := newTestManager(t)
	// 开关走「换配置快照」热生效路径（与面板保存配置同一条），因此这里也换快照，
	// 而不是改写 Manager 持有的那一份 —— 后者已不是配置的来源。
	enabled := true
	next := on.config().Clone()
	next.Tasks.DesktopEventsEnabled = &enabled
	on.SetConfig(next)

	gated := []string{"expert_5", "Expert_team_use_3", "Expert_lighthouse", "skill_1",
		"template_5", "RichMeow_Chat", "school_season", "Sequential_Tasks_1"}
	for _, code := range gated {
		if off.actionFor(code) != nil {
			t.Fatalf("开关关闭时 %s 不应有自动动作", code)
		}
		if on.actionFor(code) == nil {
			t.Fatalf("开关打开后 %s 应有自动动作", code)
		}
		// 被开关挡住时要能给出可行动的提示（告诉用户去设置里打开），
		// 而不是笼统的「需客户端操作」—— 后者会让用户真的去客户端手动做一遍。
		if reason := manualReason(code, ""); reason == "" || !gatedActionExists(code) {
			t.Fatalf("%s 被开关挡住时应给出「开关未打开」类提示，实际 %q", code, reason)
		}
	}

	// 会话事件类的动作**不受**开关影响：对话是真的，上报形状与网关既有的一致。
	ungated := []string{"chat_5", "Model_chat_GLM5.2", "black_cat", "first_buddy"}
	for _, code := range ungated {
		if off.actionFor(code) == nil {
			t.Fatalf("%s 不应受客户端事件开关影响", code)
		}
	}
}

// 夜猫子的时间窗口：23:00–08:00 计分，其余时段不做。
func TestBlackCatWindow(t *testing.T) {
	in := []int{23, 0, 3, 7}
	out := []int{8, 12, 18, 22}
	for _, h := range in {
		if !blackCatWindow(mustHour(h)) {
			t.Fatalf("%d 点应在夜猫子窗口内", h)
		}
	}
	for _, h := range out {
		if blackCatWindow(mustHour(h)) {
			t.Fatalf("%d 点不应在夜猫子窗口内", h)
		}
	}
	// upstream.InNightWindow 是同一个口径（两处判断必须一致，
	// 否则会出现「面板说在窗口内、动作自己判不在」的矛盾）。
	for _, h := range append(append([]int{}, in...), out...) {
		if got, want := upstream.InNightWindow(mustHour(h)), blackCatWindow(mustHour(h)); got != want {
			t.Fatalf("%d 点两处窗口判断不一致：upstream=%v tasks=%v", h, got, want)
		}
	}
}

// 进度文案：有 target 时给 n/m，没有时回落到状态词。
func TestProgressText(t *testing.T) {
	cases := []struct {
		in   *upstream.GrowthTask
		want string
	}{
		{nil, "?"},
		{&upstream.GrowthTask{Current: 2, Target: 5}, "2/5"},
		{&upstream.GrowthTask{Claimed: true}, "已领取"},
		{&upstream.GrowthTask{AcceptStatus: "accepted"}, "accepted"},
		{&upstream.GrowthTask{}, "?"},
	}
	for _, c := range cases {
		if got := progressText(c.in); got != c.want {
			t.Fatalf("progressText(%+v) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
