package tasks

import (
	"context"
	"testing"
	"time"
)

// TestChatEventPauseIsHumanPaced 守住「对话事件上报必须是真人节奏」。
//
// 背景（对照 wb2api-panel 的 fix c675297）：上游对 chat_request_send 有节奏反作弊，
// 数秒级连发会先被计入进度、随后判定无效并整体回滚（claim 返回 400 task not completed）。
// 实测 2s 连发 4 条全灭，45s 间隔逐条上报全部存活。
//
// 本项目原先用 1050ms（比被证伪的 2s 还密），本测试把下限钉在 45s：
// 任何「为了跑快点」把间隔调小的改动都会在这里失败。
func TestChatEventPauseIsHumanPaced(t *testing.T) {
	const wantMin = 45 * time.Second

	if chatEventGap < wantMin {
		t.Fatalf("对话事件间隔 %v 低于真人节奏下限 %v——连发会被上游判无效并回滚进度", chatEventGap, wantMin)
	}

	// 抖动区间必须落在 [gap, gap+jitter)，且不能为 0（恒定周期本身就是机器特征）。
	lo, hi := chatEventPause(), chatEventPause()
	for i := 0; i < 50; i++ {
		p := chatEventPause()
		if p < chatEventGap {
			t.Fatalf("抖动后间隔 %v 小于基准 %v", p, chatEventGap)
		}
		if p >= chatEventGap+chatEventJitter {
			t.Fatalf("抖动后间隔 %v 超出上限 %v", p, chatEventGap+chatEventJitter)
		}
		if p < lo {
			lo = p
		}
		if p > hi {
			hi = p
		}
	}
	if hi == lo {
		t.Fatal("抖动没有产生任何变化——固定周期是机器特征，应保留随机性")
	}
}

// TestActionGapStaysShort 普通动作间隔不该被误改成真人节奏。
//
// 与上一条配对：accept / claim 这类写操作只需要「不贴脸」，不需要 45s。
// 若把两者混用一个常量，整个队列会被拖到几十分钟，用户会以为卡死。
func TestActionGapStaysShort(t *testing.T) {
	if actionGap >= chatEventGap {
		t.Fatalf("普通动作间隔 %v 不应达到对话事件节奏 %v", actionGap, chatEventGap)
	}
	if actionGap <= 0 {
		t.Fatalf("普通动作间隔应为正数，实际 %v", actionGap)
	}
}

// TestSleepCtxHonoursCancel 可取消等待：上下文已取消时立即返回 false，不空等。
func TestSleepCtxHonoursCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if sleepCtx(ctx, 30*time.Second) {
		t.Fatal("上下文已取消时应返回 false")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("取消后应立即返回，实际等待了 %v", elapsed)
	}
}

// TestSleepCtxWaitsWhenAlive 正常路径：上下文健康时等满时长并返回 true。
func TestSleepCtxWaitsWhenAlive(t *testing.T) {
	start := time.Now()
	if !sleepCtx(context.Background(), 20*time.Millisecond) {
		t.Fatal("上下文健康时应返回 true")
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("应等满 20ms，实际 %v", elapsed)
	}
}
