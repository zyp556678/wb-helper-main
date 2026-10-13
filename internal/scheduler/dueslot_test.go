package scheduler

import (
	"testing"
	"time"
)

// at 构造一个本地时刻，便于按「今天几号几点」写用例。
// 用固定日期而不是 time.Now()：判定里有跨天逻辑，测试必须能控制"哪一天"。
func at(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.Local)
}

// dueSlot 的核心语义：**时点过后仍算到点**（补跑），而不是只在那一小时算到点。
//
// 这是「软件自启动了但当天没签到」的直接修复点：旧实现要求
// now.Hour() 精确等于时点，08:38 开机、时点 9/21 时整天不跑。
func TestDueSlotCatchesUpAfterTheHour(t *testing.T) {
	hours := []int{9, 21}

	cases := []struct {
		name     string
		now      time.Time
		wantSlot string
		wantOK   bool
	}{
		{
			name:     "时点整点当刻",
			now:      at(2026, time.October, 13, 9, 0),
			wantSlot: "20261013-9",
			wantOK:   true,
		},
		{
			name:     "时点前一小时（还没到）",
			now:      at(2026, time.October, 13, 8, 59),
			wantSlot: "",
			wantOK:   false,
		},
		{
			// 这条就是本次修复的靶心：09:30 开机，9 点那趟必须补上。
			name:     "时点过后（补跑）",
			now:      at(2026, time.October, 13, 9, 30),
			wantSlot: "20261013-9",
			wantOK:   true,
		},
		{
			name:     "两个时点之间取最近那个",
			now:      at(2026, time.October, 13, 15, 0),
			wantSlot: "20261013-9",
			wantOK:   true,
		},
		{
			name:     "第二个时点过后取第二个",
			now:      at(2026, time.October, 13, 22, 30),
			wantSlot: "20261013-21",
			wantOK:   true,
		},
		{
			// 跨天不补：昨天的时点不该在今天被触发。
			name:     "次日凌晨（昨天的不补）",
			now:      at(2026, time.October, 14, 0, 30),
			wantSlot: "",
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slot, ok := dueSlot(tc.now, hours)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v，期望 %v", ok, tc.wantOK)
			}
			if slot != tc.wantSlot {
				t.Fatalf("slot = %q，期望 %q", slot, tc.wantSlot)
			}
		})
	}
}

// 槽位必须**绑定时点**而不是当前小时，否则补跑后下一个小时会重复触发。
//
// 具体地：10:00 补跑了 9 点那趟，若槽位记成 "…-10"，11:00 检查时
// dueSlot 会给出 "…-10"（10 仍是"最近已过的时点"），claim 又通过 —— 重复签到。
func TestDueSlotIsStableAcrossHoursWithinSamePoint(t *testing.T) {
	hours := []int{9}
	// 9 点过后的一整个白天，时段标识都应稳定指向 9 点那趟。
	want := "20261013-9"
	for _, h := range []int{9, 10, 11, 15, 20, 23} {
		got, ok := dueSlot(at(2026, time.October, 13, h, 0), hours)
		if !ok || got != want {
			t.Fatalf("%02d:00 时 slot = %q (ok=%v)，期望 %q —— "+
				"槽位若随当前小时变化，补跑会在下一个小时重复触发", h, got, ok, want)
		}
	}
}

// 多时点：每个时点各自到期、各自执行一次（9 点跑过之后，21 点还要再跑）。
func TestDueSlotAdvancesToNextPointOnSameDay(t *testing.T) {
	hours := []int{9, 21}
	before, _ := dueSlot(at(2026, time.October, 13, 10, 0), hours)
	after, _ := dueSlot(at(2026, time.October, 13, 21, 5), hours)
	if before == after {
		t.Fatalf("9 点与 21 点应是不同槽位，都是 %q —— 第二个时点会被去重吃掉", before)
	}
	if before != "20261013-9" || after != "20261013-21" {
		t.Fatalf("槽位不符：9 点后 = %q，21 点后 = %q", before, after)
	}
}

// 边界：空时点列表、越界时点。
func TestDueSlotEdgeCases(t *testing.T) {
	if _, ok := dueSlot(at(2026, time.October, 13, 12, 0), nil); ok {
		t.Fatal("空时点列表不该有到点项")
	}
	// 越界时点被忽略：不该让 24 被当成 0 点、也不该让整个任务空转出错。
	if _, ok := dueSlot(at(2026, time.October, 13, 12, 0), []int{24, -1}); ok {
		t.Fatal("越界时点应被忽略")
	}
	// 合法时点与越界时点混在一起时，合法那个照常生效。
	slot, ok := dueSlot(at(2026, time.October, 13, 12, 0), []int{24, 10, -1})
	if !ok || slot != "20261013-10" {
		t.Fatalf("混入越界时点后应取到合法的 10 点，得到 %q (ok=%v)", slot, ok)
	}
}

// slotHour 与 dueSlot 是一对：解析回来必须是原时点。
func TestSlotHourRoundTrip(t *testing.T) {
	for _, h := range []int{0, 9, 12, 23} {
		slot, _ := dueSlot(at(2026, time.October, 13, h, 30), []int{h})
		if got := slotHour(slot); got != h {
			t.Fatalf("slotHour(%q) = %d，期望 %d", slot, got, h)
		}
	}
	// 畸形输入不 panic（日志格式化会用到，不能因此崩掉排程循环）。
	if got := slotHour("garbage"); got != 0 {
		t.Fatalf("畸形槽位应返回 0，得到 %d", got)
	}
}

// dayOpenSlot：当天首次运行、时点还没到时，提前取**最早**那个时点。
//
// 场景：用户早上 8:38 开机，签到/任务时点是 9 点与 21 点。
// 旧实现要干等到 9:00；这里让当天第一次检查就把它做掉。
func TestDayOpenSlotFiresEarliestPointWhenNoneDueYet(t *testing.T) {
	hours := []int{9, 21}
	slot, ok := dayOpenSlot(at(2026, time.October, 13, 8, 38), hours)
	if !ok || slot != "20261013-9" {
		t.Fatalf("首次运行应取最早的 9 点，得到 %q (ok=%v)", slot, ok)
	}

	// 只提前最早那个：不能把 21 点也一起跑了（上游对密集操作敏感）。
	if slotHour(slot) != 9 {
		t.Fatalf("提前触发的必须是 9 点，而不是 %d 点", slotHour(slot))
	}
}

// 关键：dayOpenSlot 给出的槽位必须与 dueSlot 在**真正到点时**给出的一致，
// 否则 claim 拦不住重复执行 —— 8:38 提前跑一次、9:00 又跑一次。
func TestDayOpenSlotAgreesWithDueSlotAtRealHour(t *testing.T) {
	hours := []int{9, 21}
	early, _ := dayOpenSlot(at(2026, time.October, 13, 8, 38), hours)
	real, _ := dueSlot(at(2026, time.October, 13, 9, 0), hours)
	if early != real {
		t.Fatalf("提前触发的槽位 %q 与真实到点时的 %q 不一致 —— "+
			"同一趟会被执行两次", early, real)
	}
}

// 多时点的夜猫子（时点 23，计分窗口 23:00-08:00）：早上开机时它会取 23 点槽位，
// 但真正执行时由任务中心按窗口拒绝 —— 这里只钉住槽位判定本身。
func TestDayOpenSlotSinglePoint(t *testing.T) {
	slot, ok := dayOpenSlot(at(2026, time.October, 13, 7, 0), []int{23})
	if !ok || slot != "20261013-23" {
		t.Fatalf("单时点应取它自己，得到 %q (ok=%v)", slot, ok)
	}
}

// dayOpenSlot 的边界：空列表、全越界。
func TestDayOpenSlotEdgeCases(t *testing.T) {
	if _, ok := dayOpenSlot(at(2026, time.October, 13, 8, 0), nil); ok {
		t.Fatal("空时点列表不该提前触发")
	}
	if _, ok := dayOpenSlot(at(2026, time.October, 13, 8, 0), []int{24, -5}); ok {
		t.Fatal("全越界的时点列表不该提前触发")
	}
}

// 签到的取槽顺序必须「先 dueSlot、后 dayOpenSlot（仅首次）」。
//
// 反过来写会重复签到：22:00 启动时先返回最早的 9 点槽位并签一次，
// 下一分钟 dueSlot 又给出 21 点槽位 —— 同一分钟内签两趟。
// 这里模拟 checkHourlyTasks 的判定顺序，钉住「首次之后不再提前触发」。
func TestCheckinSlotOrderAvoidsDoubleFire(t *testing.T) {
	hours := []int{9, 21}

	// 22:00 启动（时点 21 已过、9 也早已过）：应当只取 21 点那个槽位。
	first := func(now time.Time, dayOpen bool) (string, bool) {
		if slot, ok := dueSlot(now, hours); ok {
			return slot, true
		}
		if dayOpen {
			return dayOpenSlot(now, hours)
		}
		return "", false
	}
	slot, ok := first(at(2026, time.October, 13, 22, 0), true)
	if !ok || slot != "20261013-21" {
		t.Fatalf("22:00 启动应取 21 点槽位，得到 %q (ok=%v)", slot, ok)
	}
	// 下一分钟（dayOpen=false）必须给出**同一个**槽位，claim 才能拦下重复执行。
	again, ok := first(at(2026, time.October, 13, 22, 1), false)
	if !ok || again != slot {
		t.Fatalf("下一分钟应给出同一槽位 %q 以触发去重，得到 %q (ok=%v)", slot, again, ok)
	}

	// 08:00 启动（任何时点都还没到）：首次提前取最早的 9 点。
	early, ok := first(at(2026, time.October, 13, 8, 0), true)
	if !ok || early != "20261013-9" {
		t.Fatalf("08:00 启动应提前取 9 点槽位，得到 %q (ok=%v)", early, ok)
	}
	// 但**只在首次**：下一分钟不提前触发（否则会在 8 点这段每小时签一次）。
	if _, ok := first(at(2026, time.October, 13, 8, 1), false); ok {
		t.Fatal("非首次检查不该提前触发时点未到的任务 —— 会变成每小时签一次")
	}
	// 等到真正 9:00 时，dueSlot 给出的必须仍是同一槽位（不重复签）。
	at9, ok := first(at(2026, time.October, 13, 9, 0), false)
	if !ok || at9 != early {
		t.Fatalf("9:00 到点时槽位应仍为 %q，得到 %q —— 否则会被重复签一次", early, at9)
	}
}
