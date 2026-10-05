package tasks

import (
	"testing"
	"time"

	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/upstream"
)

// decideTravel 是旅行状态机的**判据本体**，每一条分支都要钉住。
//
// 之所以把它抽成纯函数并单独测：这些分支的条件是「语义」而不是实现细节，
// 混在带网络调用的函数里时，一次重构就可能悄悄改掉某条判断，
// 而唯一的回归信号是「某个账号不再领奖了」——那要等一整天才会被发现。
func TestDecideTravel(t *testing.T) {
	cases := []struct {
		name        string
		state       string
		dailyLimit  bool
		recordID    int64
		wantAction  string
		wantProblem bool
	}{
		{name: "到站且有 record_id → 领奖", state: travelStateArrived, recordID: 77, wantAction: travelClaimAction},
		{name: "到站但缺 record_id → 报异常不静默", state: travelStateArrived, recordID: 0, wantAction: travelSkipAction, wantProblem: true},
		{name: "空闲且未达上限 → 出发", state: travelStateIdle, wantAction: travelDepartAction},
		{name: "空闲但今日已达上限 → 跳过", state: travelStateIdle, dailyLimit: true, wantAction: travelSkipAction},
		{name: "在途 → 跳过", state: travelStateTraveling, wantAction: travelSkipAction},
		{name: "未知状态 → 跳过（不猜）", state: "some_future_state", wantAction: travelSkipAction},
		{name: "空状态 → 跳过", state: "", wantAction: travelSkipAction},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			action, reason := decideTravel(c.state, c.dailyLimit, c.recordID)
			if action != c.wantAction {
				t.Fatalf("action = %q，期望 %q", action, c.wantAction)
			}
			if c.wantProblem && reason == "" {
				t.Fatal("应给出可读的异常原因（不能静默跳过）")
			}
			if !c.wantProblem && reason != "" {
				t.Fatalf("不该报异常，实际 reason=%q", reason)
			}
		})
	}
}

// 到站却缺 record_id **必须报出来**：静默跳过会让这一趟永远领不到奖，
// 而界面上看不出任何异常（和其它「正常跳过」长得一样）。
func TestDecideTravelArrivedWithoutRecordIsReported(t *testing.T) {
	action, reason := decideTravel(travelStateArrived, false, 0)
	if action != travelSkipAction {
		t.Fatalf("不该尝试领奖（一定失败），实际 action=%q", action)
	}
	if reason == "" {
		t.Fatal("缺 record_id 必须给出原因")
	}
}

// travelDay 按 CST 自然日切分。
//
// 上游每日重置按 Asia/Shanghai 00:00，所以「UTC 16:00」已经属于 CST 的次日。
// 用 UTC 日期会让每天的边界偏 8 小时 —— 表现是「当天该跳过领养的账号又被打了一次」。
func TestTravelDayUsesCSTBoundary(t *testing.T) {
	// 2026-10-04 15:59 UTC = 2026-10-04 23:59 CST
	before := time.Date(2026, 10, 4, 15, 59, 0, 0, time.UTC)
	if got := travelDay(before); got != "2026-10-04" {
		t.Fatalf("15:59 UTC 应为 10-04，实际 %s", got)
	}
	// 2026-10-04 16:00 UTC = 2026-10-05 00:00 CST
	after := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
	if got := travelDay(after); got != "2026-10-05" {
		t.Fatalf("16:00 UTC 应为 10-05（CST 已跨日），实际 %s", got)
	}
}

// 「当日已试过领养」的门必须是幂等的，且**不同账号互不影响**。
func TestAdoptTriedGateIsPerAccountAndIdempotent(t *testing.T) {
	m := newTestManager(t)

	if m.adoptTriedToday("uid-a") {
		t.Fatal("什么都没记，不该判为已试过")
	}
	m.markAdoptTried("uid-a")
	if !m.adoptTriedToday("uid-a") {
		t.Fatal("标记后应判为已试过")
	}
	if m.adoptTriedToday("uid-b") {
		t.Fatal("别的账号不该被连带标记（一个账号未过门槛不影响其它账号）")
	}
	// 幂等：重复标记不 panic、结果不变。
	m.markAdoptTried("uid-a")
	if !m.adoptTriedToday("uid-a") {
		t.Fatal("重复标记后仍应为已试过")
	}
}

// 空池时 TravelInspect 必须给出**明确错误**而不是静默返回成功。
//
// 静默成功会让调度器日志写「旅行巡检完成：领养 0…」，看起来一切正常，
// 而实际是一个账号都没查 —— 这类「零账号的成功」是最难发现的假信号。
func TestTravelInspectEmptyPoolErrors(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.TravelInspect(t.Context()); err == nil {
		t.Fatal("空池应返回错误")
	}
}

// -----------------------------------------------------------------------------
// 切片 18：客户端指纹事件链的**默认关闭**约定
// -----------------------------------------------------------------------------

// 关闭时这些 code 必须落进「需客户端」而不是「可自动」。
//
// 这是「默认不伪造客户端行为」这条约定的**唯一执行点**：如果 actionFor 漏了过滤，
// 用户会在没有任何提示的情况下开始往上游发伪造事件 —— 代价由账号承担。
func TestDesktopActionsHiddenByDefault(t *testing.T) {
	m := newTestManager(t)
	for _, code := range []string{"template_5", "playbook_prompt", "create_canvas", "Library_read", "Hp_Appearance"} {
		if a := m.actionFor(code); a != nil {
			t.Fatalf("%s 在默认（关闭）状态下不该有自动动作", code)
		}
		cat, _, _ := m.classify(upstream.GrowthTask{TaskCode: code, AcceptStatus: "accepted"})
		if cat != CatManual {
			t.Fatalf("%s 默认应归入「需客户端」，实际 %s", code, cat)
		}
	}
	// 对话类动作不受影响。
	if a := m.actionFor("chat_5"); a == nil {
		t.Fatal("对话上报类动作不该受开关影响")
	}
}

// 显式打开后这五个 code 才变成可自动完成。
func TestDesktopActionsEnabledByConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{WorkDir: dir}
	cfg.ApplyDefaults()
	on := true
	cfg.Tasks.DesktopEventsEnabled = &on
	m := New(pool.New(cfg, nil), nil, cfg, nil)

	want := map[string]string{
		"template_5":      "template",
		"playbook_prompt": "playbook",
		"create_canvas":   "canvas",
		"Library_read":    "library",
		"Hp_Appearance":   "appearance",
	}
	for code, kind := range want {
		a := m.actionFor(code)
		if a == nil {
			t.Fatalf("%s 打开开关后应有自动动作", code)
		}
		if a.mode != modeDesktopEvent {
			t.Fatalf("%s 应是桌面事件动作，实际 mode=%s", code, a.mode)
		}
		if a.desktop != kind {
			t.Fatalf("%s 的子类型应为 %s，实际 %s", code, kind, a.desktop)
		}
		cat, _, _ := m.classify(upstream.GrowthTask{TaskCode: code, AcceptStatus: "accepted"})
		if cat != CatAuto {
			t.Fatalf("%s 打开后应归入可自动，实际 %s", code, cat)
		}
	}
}

// 未命中任何动作的 code 仍然返回 nil（不能因为加了开关就放宽匹配）。
func TestUnknownCodeStillHasNoAction(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{WorkDir: dir}
	cfg.ApplyDefaults()
	on := true
	cfg.Tasks.DesktopEventsEnabled = &on
	m := New(pool.New(cfg, nil), nil, cfg, nil)
	if a := m.actionFor("some_unknown_code"); a != nil {
		t.Fatalf("未知 code 不该有自动动作，实际 %+v", a)
	}
}
