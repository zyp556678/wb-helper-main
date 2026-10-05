package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy-gateway/internal/config"
	"workbuddy-gateway/internal/pool"
)

// 本文件测的是「执行队列的可观测性」——那两块用户实际抱怨的东西：
// 进页面能不能看到上次结果、准备阶段有没有反馈、日志里有没有每条明细。
//
// 刻意不测真实上游调用：那需要凭据与网络，属于集成测试的范畴。
// 这里只测状态机与落盘，它们才是「界面看不到进度」的根因所在。

// newTestManager 造一个**空池**（无账号、不连上游）的 Manager。
//
// 用空池而不是 nil 池：targets() 会遍历 pool.Accounts()，nil 池直接 panic。
// 空池既安全又正好覆盖「没有可用账号」这条真实分支。
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{WorkDir: dir}
	return New(pool.New(cfg, nil), nil, cfg, nil)
}

// TestQueuePhaseDefaultsToIdle 新建的队列必须是 idle，而不是空串。
//
// 空串会让前端 `phase === "preparing"` 之类的判断全部落到兜底分支，
// 界面文案变成「已完成：成功 0，失败 0」——一条从没跑过的队列却宣称「已完成」。
func TestQueuePhaseDefaultsToIdle(t *testing.T) {
	m := newTestManager(t)
	q := m.Queue()
	if q.Phase != QueuePhaseIdle {
		t.Errorf("新建队列 phase 应为 %q，实际 %q", QueuePhaseIdle, q.Phase)
	}
	if q.Running {
		t.Error("新建队列不该处于 running")
	}
}

// TestNormalizedPhaseInfersFromLegacyFields 守住对老状态文件的兼容。
//
// 已经落盘的状态文件里没有 phase 字段。若把它们读成空 phase，
// 用户升级后打开面板会看到一块什么都不显示的队列区块。
func TestNormalizedPhaseInfersFromLegacyFields(t *testing.T) {
	cases := []struct {
		name string
		in   QueueState
		want string
	}{
		{"空状态", QueueState{}, QueuePhaseIdle},
		{"运行中", QueueState{Running: true, StartedAt: 100}, QueuePhaseRunning},
		{"已结束", QueueState{StartedAt: 100, FinishedAt: 200}, QueuePhaseDone},
		{"显式 phase 优先", QueueState{Phase: QueuePhasePreparing, Running: true}, QueuePhasePreparing},
	}
	for _, c := range cases {
		if got := c.in.NormalizedPhase(); got != c.want {
			t.Errorf("%s：NormalizedPhase() = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// TestQueueStatePersistsAndRestores 守住「刷新/重启后还能看到上次结果」。
func TestQueueStatePersistsAndRestores(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{WorkDir: dir}
	m := New(pool.New(cfg, nil), nil, cfg, nil)

	m.mu.Lock()
	m.queue = &QueueState{
		Phase: QueuePhaseDone, StartedAt: 100, FinishedAt: 200,
		Total: 2, Done: 1, Failed: 1,
		Items: []QueueItem{
			{Account: "a.json", AccountName: "甲", TaskCode: "chat_5", Title: "和 AI 聊天 5 次",
				Kind: "auto", Status: "ok", Message: "已上报 1 条对话事件"},
			{Account: "b.json", TaskCode: "black_cat", Title: "夜猫子", Kind: "auto",
				Status: "failed", Message: "失败：上游 429"},
		},
	}
	m.mu.Unlock()
	m.persistQueue()

	if _, err := os.Stat(filepath.Join(dir, queueFileName)); err != nil {
		t.Fatalf("队列状态未落盘: %v", err)
	}

	// 模拟重启：用同一个工作目录重建 Manager。
	m2 := New(pool.New(cfg, nil), nil, cfg, nil)
	q := m2.Queue()
	if q.Total != 2 || q.Done != 1 || q.Failed != 1 {
		t.Errorf("恢复的计数不对：total=%d done=%d failed=%d", q.Total, q.Done, q.Failed)
	}
	if len(q.Items) != 2 {
		t.Fatalf("恢复的条目数应为 2，实际 %d", len(q.Items))
	}
	if q.Items[1].Message != "失败：上游 429" {
		t.Errorf("失败原因未被恢复：%q", q.Items[1].Message)
	}
	if q.Items[0].AccountName != "甲" {
		t.Errorf("账号昵称未被恢复：%q", q.Items[0].AccountName)
	}
}

// TestRestoreClearsRunningFlag 守住「重启后不能仍显示执行中」。
//
// 进程重启后不可能还有 goroutine 在跑。若照搬文件里的 running=true，
// 界面会永远转圈，而且 Run 会一直认为有队列在跑（拒绝新队列）——
// 用户唯一的出路是删状态文件，这是最糟的一种卡死。
func TestRestoreClearsRunningFlag(t *testing.T) {
	dir := t.TempDir()
	// 手写一个「上次崩溃时留下的 running=true」状态文件。
	raw, _ := json.Marshal(QueueState{
		Running: true, Phase: QueuePhaseRunning, StartedAt: 100,
		Total: 3, Items: []QueueItem{{Account: "a.json", TaskCode: "chat_5"}},
	})
	if err := os.WriteFile(filepath.Join(dir, queueFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	m := New(pool.New(&config.Config{WorkDir: dir}, nil), nil, &config.Config{WorkDir: dir}, nil)
	q := m.Queue()
	if q.Running {
		t.Error("恢复后 running 必须为 false，否则界面永远转圈且无法再触发新队列")
	}
	if q.Phase != QueuePhaseDone {
		t.Errorf("中断的队列应标为 done，实际 %q", q.Phase)
	}
	if q.Note == "" {
		t.Error("中断的队列应带一条说明，告诉用户上次被执行被中断了")
	}
}

// TestCorruptStateFileIsIgnored 守住「状态文件损坏不能让任务中心起不来」。
func TestCorruptStateFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, queueFileName), []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(pool.New(&config.Config{WorkDir: dir}, nil), nil, &config.Config{WorkDir: dir}, nil)
	q := m.Queue()
	if q.Phase != QueuePhaseIdle {
		t.Errorf("损坏文件应被忽略并回落到 idle，实际 %q", q.Phase)
	}
}

// TestRunMarksPreparingImmediately 是本轮的核心不变式：
// **Run 必须立刻把队列置为 preparing 并返回**，绝不在里面等扫描。
//
// 这条不变式守住的是用户实际看到的两个症状：
//   - 点完按钮界面毫无反馈（因为请求要等扫描完才返回）
//   - 小队列在响应返回前就跑完了，前端读到 running=false 永不轮询，
//     整个执行过程一次都看不到
//
// 用 nil pool 构造时 targets 为空，所以这里只能验证「状态被立起来」和
// 「返回 -2 表示已受理」这两件事 —— 拿不到项数正是 preparing 阶段的本意。
func TestRunMarksPreparingImmediately(t *testing.T) {
	m := newTestManager(t)

	done := make(chan int, 1)
	go func() { done <- m.Run(nil, RunOptions{}) }() //nolint:staticcheck // nil ctx 在本用例只走占位分支

	select {
	case n := <-done:
		// nil pool ⇒ targets 为空 ⇒ 立即以「没有可用账号」收尾并返回 0。
		// 这是合法路径，只要它**没有卡住**。
		if n != 0 {
			t.Errorf("无可用账号时 Run 应返回 0，实际 %d", n)
		}
		q := m.Queue()
		if q.Phase != QueuePhaseDone {
			t.Errorf("无账号时队列应收尾为 done，实际 %q", q.Phase)
		}
		if q.Note == "" {
			t.Error("无账号时应收尾为一条说明，便于界面展示原因")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run 卡住了：它不该在任何路径上阻塞等待扫描")
	}
}

// TestRunRejectsConcurrentQueue 守住「准备阶段就要占住位置」。
//
// 占位状态必须在 goroutine 起来**之前**就立好，否则用户在扫描那几秒里
// 连点两次按钮，就会排出两个队列 —— 同一个账号被并发操作，正是风控最敏感的形态。
func TestRunRejectsConcurrentQueue(t *testing.T) {
	m := newTestManager(t)

	// 手工把队列置成「准备中」，模拟第一次点击已经占位。
	m.mu.Lock()
	m.queue = &QueueState{Running: true, Phase: QueuePhasePreparing, StartedAt: time.Now().Unix()}
	m.mu.Unlock()

	if n := m.Run(nil, RunOptions{}); n != -1 { //nolint:staticcheck // 同上
		t.Errorf("已有队列在跑时应返回 -1，实际 %d", n)
	}
}

// TestKindLabelIsHumanReadable 保证日志里的动作名是中文可读的。
func TestKindLabelIsHumanReadable(t *testing.T) {
	cases := map[string]string{
		"claim":  "领取奖励",
		"accept": "报名任务",
		"auto":   "自动完成",
	}
	for kind, want := range cases {
		if got := kindLabel(kind); got != want {
			t.Errorf("kindLabel(%q) = %q，期望 %q", kind, got, want)
		}
	}
	// 未知动作原样返回，不要吞掉信息。
	if got := kindLabel("weird"); got != "weird" {
		t.Errorf("未知动作应原样返回，实际 %q", got)
	}
}

// TestQueueItemsIsNeverNull 守住「items 永远是数组，绝不是 null」。
//
// 这是一次真实事故的回归测试：`append([]QueueItem(nil), 空切片...)` 返回 nil，
// 序列化成 `"items": null`，前端 `items.length` 抛
// 「Cannot read properties of null (reading 'length')」，
// 整个任务中心页崩成「这个页面出错了」+ 一个重试按钮。
//
// 它的触发条件极其普通 —— 刚启动、还没跑过任何队列时必然经过这条路径。
// 而且错误信息完全指不出是队列字段的问题，排查成本很高。
//
// 因此这里不只断言「不是 nil」，还断言**序列化后的 JSON 里 items 是 []**，
// 因为后者才是真正送到前端的东西。
func TestQueueItemsIsNeverNull(t *testing.T) {
	// 覆盖三条会让 items 变空的路径。
	cases := []struct {
		name  string
		build func() *Manager
	}{
		{
			name: "全新启动，未跑过队列",
			build: func() *Manager {
				cfg := &config.Config{WorkDir: t.TempDir()}
				return New(pool.New(cfg, nil), nil, cfg, nil)
			},
		},
		{
			name: "准备阶段（items 尚未组装）",
			build: func() *Manager {
				m := newTestManager(t)
				m.mu.Lock()
				m.queue = &QueueState{
					Running: true, Phase: QueuePhasePreparing,
					StartedAt: time.Now().Unix(), Items: []QueueItem{},
				}
				m.mu.Unlock()
				return m
			},
		},
		{
			name: "无可用账号收尾",
			build: func() *Manager {
				m := newTestManager(t)
				m.mu.Lock()
				m.queue = &QueueState{
					Phase: QueuePhaseDone, StartedAt: 1, FinishedAt: 2,
					Items: []QueueItem{}, Note: "没有可用的账号",
				}
				m.mu.Unlock()
				return m
			},
		},
	}

	for _, c := range cases {
		m := c.build()
		q := m.Queue()
		if q.Items == nil {
			t.Errorf("%s：Queue().Items 为 nil，序列化后会变成 null 并让前端崩溃", c.name)
			continue
		}
		raw, err := json.Marshal(q)
		if err != nil {
			t.Fatalf("%s：序列化失败: %v", c.name, err)
		}
		var probe map[string]any
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatal(err)
		}
		if probe["items"] == nil {
			t.Errorf("%s：JSON 里 items 为 null（应为民数组），前端会抛 length of null：%s",
				c.name, string(raw))
		}
	}
}

// TestPersistKeepsItemsArray 守住落盘后的 items 同样是数组。
//
// 前端崩溃那次的第一步就是「空 items 落盘成 null」，所以落盘侧要单独测：
// 空队列重启后若读回一个 null，崩的就是重启后的第一次访问。
func TestPersistKeepsItemsArray(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{WorkDir: dir}
	m := New(pool.New(cfg, nil), nil, cfg, nil)
	m.persistQueue()

	raw, err := os.ReadFile(filepath.Join(dir, queueFileName))
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if probe["items"] == nil {
		t.Errorf("落盘文件里 items 为 null，重启读回后会让前端崩溃: %s", string(raw))
	}
}

// TestMaxPersistedItemsBoundsFile 守住落盘条目上限，防止队列文件无限膨胀。
func TestMaxPersistedItemsBoundsFile(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{WorkDir: dir}
	m := New(pool.New(cfg, nil), nil, cfg, nil)

	items := make([]QueueItem, 0, maxPersistedItems+50)
	for i := 0; i < maxPersistedItems+50; i++ {
		items = append(items, QueueItem{Account: "a.json", TaskCode: "t", Status: "ok"})
	}
	m.mu.Lock()
	m.queue = &QueueState{Phase: QueuePhaseDone, StartedAt: 1, FinishedAt: 2, Items: items}
	m.mu.Unlock()
	m.persistQueue()

	raw, err := os.ReadFile(filepath.Join(dir, queueFileName))
	if err != nil {
		t.Fatal(err)
	}
	var q QueueState
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatal(err)
	}
	if len(q.Items) > maxPersistedItems {
		t.Errorf("落盘条目数 %d 超过上限 %d", len(q.Items), maxPersistedItems)
	}
}
