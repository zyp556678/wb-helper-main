package clientlimits

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureLog 用**实测原文**的行形态（2026-10-04 客户端日志）：
// uid 变更行、认证成功行、模型请求行、以及带 requestId 的 429 限额行。
const fixtureLog = `[2026/10/4 19:14:13.442] [Info] [pid=42048] [AuthenticationManager]  [AuthenticationManager] userId changed:  -> 23edcad8-4a88-462e-85d6-ff696cd77d40
[2026/10/4 19:14:13.442] [Info] [pid=42048] [AuthenticationSessionListener] [auth_success: Camellia]  认证成功,执行 Notification 钩子:
[2026/10/4 19:14:15.179] [Info] [pid=42048] [ModelProvider]  [ModelProvider] Sending request: agent=cli, model=deepseek-v4.1-flash, requestId=01a1069f23ff7049abb5ed59e8b0c81e, stream=true
[2026/10/4 19:14:15.469] [Info] [pid=42048] [SessionManager] await result.completed ERROR — session=15e48553-c56e-4eea-9919-40f5ac680a4a, error=429 您的使用量已超出频率限制，将在 2026-10-05 13:49:05 UTC+8 重置，您也可以切换其他模型继续使用。 (01a1069f23ff7049abb5ed59e8b0c81e/15e48553-c56e-4eea-9919-40f5ac680a4a)
`

func writeLog(t *testing.T, root, date, name, content string) {
	t.Helper()
	dir := filepath.Join(root, date)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanParsesAndAttributes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	writeLog(t, root, "2026-10-04", "task.log", fixtureLog)

	now := time.Date(2026, 10, 4, 20, 0, 0, 0, time.Local)
	got := New(root).Limits(now)
	if len(got) != 1 {
		t.Fatalf("应解析出 1 条限流，实际 %d：%+v", len(got), got)
	}
	item := got[0]
	if item.UID != "23edcad8-4a88-462e-85d6-ff696cd77d40" {
		t.Errorf("uid 归因错误: %q", item.UID)
	}
	if item.Nickname != "Camellia" {
		t.Errorf("昵称归因错误: %q", item.Nickname)
	}
	if item.Model != "deepseek-v4.1-flash" {
		t.Errorf("模型应通过 requestId 关联到请求行，实际 %q", item.Model)
	}
	want := time.Date(2026, 10, 5, 13, 49, 5, 0, time.FixedZone("UTC+8", 8*3600))
	if !item.Until.Equal(want) {
		t.Errorf("重置时刻应为 %v，实际 %v", want, item.Until)
	}
	if item.Evidence == "" {
		t.Error("应保留命中原文片段")
	}
}

func TestWindowIgnoresOldDateDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	// 旧目录（超出最近 2 个日期目录）里的限流不该被看到。
	writeLog(t, root, "2026-09-01", "old.log", fixtureLog)
	// 窗口内的最新目录里没有限流文案（只用于占住窗口）。
	writeLog(t, root, "2026-10-04", "plain.log", "no marker here\n")
	writeLog(t, root, "2026-10-03", "plain2.log", "no marker here\n")

	now := time.Date(2026, 10, 4, 20, 0, 0, 0, time.Local)
	if got := New(root).Limits(now); len(got) != 0 {
		t.Fatalf("旧日期目录不应参与扫描，实际 %+v", got)
	}

	// 把同一份 fixture 放进窗口内的目录，就应命中。
	writeLog(t, root, "2026-10-03", "hit.log", fixtureLog)
	got := New(root).Limits(now)
	if len(got) != 1 {
		t.Fatalf("窗口内目录应命中 1 条，实际 %d", len(got))
	}
}

func TestExpiredUntilFiltered(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	writeLog(t, root, "2026-10-04", "task.log", fixtureLog)

	// 已经过了重置时刻 → 不再显示。
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	if got := New(root).Limits(now); len(got) != 0 {
		t.Fatalf("过期条目应被过滤，实际 %+v", got)
	}
}

func TestNoAttributionSkipped(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	line := "[2026/10/4 19:14:15.469] [Info] [pid=1] [SessionManager] error=429 您的使用量已超出频率限制，将在 2026-10-05 13:49:05 UTC+8 重置。\n"
	writeLog(t, root, "2026-10-04", "task.log", line)

	now := time.Date(2026, 10, 4, 20, 0, 0, 0, time.Local)
	if got := New(root).Limits(now); len(got) != 0 {
		t.Fatalf("无账号归因的条目不应返回，实际 %+v", got)
	}
}

func TestUnknownModelWhenAmbiguous(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	content := "[2026/10/4 19:14:13.442] [Info] [pid=1] userId changed:  -> 23edcad8-4a88-462e-85d6-ff696cd77d40\n" +
		"[2026/10/4 19:14:15.179] [Info] [pid=1] Sending request: model=glm-5.3-flash, requestId=aaaabbbbccccdddd1111222233334444, stream=true\n" +
		"[2026/10/4 19:14:15.179] [Info] [pid=1] Sending request: model=deepseek-v4.1-flash, requestId=eeeeffff000011112222333344445555, stream=true\n" +
		"[2026/10/4 19:14:15.469] [Info] [pid=1] error=429 您的使用量已超出频率限制，将在 2026-10-05 13:49:05 UTC+8 重置。 (99998888777766665555444433332222/15e48553-c56e-4eea-9919-40f5ac680a4a)\n"
	writeLog(t, root, "2026-10-04", "task.log", content)

	now := time.Date(2026, 10, 4, 20, 0, 0, 0, time.Local)
	got := New(root).Limits(now)
	if len(got) != 1 {
		t.Fatalf("应解析出 1 条，实际 %d", len(got))
	}
	if got[0].Model != UnknownModel {
		t.Fatalf("requestId 对不上且文件里有多个模型时应显示 %q，实际 %q", UnknownModel, got[0].Model)
	}
}

func TestMultipleModelsInOneFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	content := "[2026/10/4 19:14:13.442] [Info] [pid=1] userId changed:  -> 23edcad8-4a88-462e-85d6-ff696cd77d40\n" +
		// 两个模型各自被限流：两次请求行 + 两条 429 行（各自带自己的 requestId）。
		"[2026/10/4 19:14:15.179] [Info] [pid=1] Sending request: model=deepseek-v4.1-flash, requestId=aaaa1111bbbb2222cccc3333dddd4444, stream=true\n" +
		"[2026/10/4 19:14:15.469] [Info] [pid=1] error=429 您的使用量已超出频率限制，将在 2026-10-05 13:49:05 UTC+8 重置。 (aaaa1111bbbb2222cccc3333dddd4444/15e48553-c56e-4eea-9919-40f5ac680a4a)\n" +
		"[2026/10/4 19:20:01.100] [Info] [pid=1] Sending request: model=glm-5.3-flash, requestId=eeee5555ffff6666aaaa7777bbbb8888, stream=true\n" +
		"[2026/10/4 19:20:01.400] [Info] [pid=1] error=429 您的使用量已超出频率限制，将在 2026-10-05 09:30:00 UTC+8 重置。 (eeee5555ffff6666aaaa7777bbbb8888/15e48553-c56e-4eea-9919-40f5ac680a4a)\n"
	writeLog(t, root, "2026-10-04", "task.log", content)

	now := time.Date(2026, 10, 4, 20, 0, 0, 0, time.Local)
	got := New(root).Limits(now)
	if len(got) != 2 {
		t.Fatalf("两个模型各被限流应产生 2 条，实际 %d：%+v", len(got), got)
	}
	byModel := map[string]Limit{}
	for _, item := range got {
		byModel[item.Model] = item
	}
	if _, ok := byModel["deepseek-v4.1-flash"]; !ok {
		t.Errorf("缺少 deepseek-v4.1-flash 条目：%+v", got)
	}
	if _, ok := byModel["glm-5.3-flash"]; !ok {
		t.Errorf("缺少 glm-5.3-flash 条目：%+v", got)
	}
	// 按恢复时间升序：09:30 的 glm 排在 13:49 的 deepseek 前面。
	if got[0].Model != "glm-5.3-flash" {
		t.Errorf("应按恢复时间升序排列，实际首位 %q", got[0].Model)
	}
}

func TestSameModelKeepsLatestReset(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	content := "[2026/10/4 19:14:13.442] [Info] [pid=1] userId changed:  -> 23edcad8-4a88-462e-85d6-ff696cd77d40\n" +
		"[2026/10/4 19:14:15.179] [Info] [pid=1] Sending request: model=deepseek-v4.1-flash, requestId=aaaa1111bbbb2222cccc3333dddd4444, stream=true\n" +
		"[2026/10/4 19:14:15.469] [Info] [pid=1] error=429 您的使用量已超出频率限制，将在 2026-10-05 09:30:00 UTC+8 重置。 (aaaa1111bbbb2222cccc3333dddd4444/15e48553-c56e-4eea-9919-40f5ac680a4a)\n" +
		// 同一账号同一模型的第二次限流（更晚的重置时刻）应覆盖前一条。
		"[2026/10/4 19:30:00.100] [Info] [pid=1] Sending request: model=deepseek-v4.1-flash, requestId=eeee5555ffff6666aaaa7777bbbb8888, stream=true\n" +
		"[2026/10/4 19:30:00.400] [Info] [pid=1] error=429 您的使用量已超出频率限制，将在 2026-10-05 13:49:05 UTC+8 重置。 (eeee5555ffff6666aaaa7777bbbb8888/15e48553-c56e-4eea-9919-40f5ac680a4a)\n"
	writeLog(t, root, "2026-10-04", "task.log", content)

	now := time.Date(2026, 10, 4, 20, 0, 0, 0, time.Local)
	got := New(root).Limits(now)
	if len(got) != 1 {
		t.Fatalf("同一账号同一模型应去重为 1 条，实际 %d：%+v", len(got), got)
	}
	want := time.Date(2026, 10, 5, 13, 49, 5, 0, time.FixedZone("UTC+8", 8*3600))
	if !got[0].Until.Equal(want) {
		t.Errorf("应保留更晚的恢复时刻 %v，实际 %v", want, got[0].Until)
	}
}

func TestCacheTTL(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	writeLog(t, root, "2026-10-04", "a.log", fixtureLog)
	scanner := New(root)
	now := time.Date(2026, 10, 4, 20, 0, 0, 0, time.Local)
	if got := scanner.Limits(now); len(got) != 1 {
		t.Fatalf("首次扫描应命中，实际 %d", len(got))
	}

	// TTL 内新增文件不重扫（缓存生效）。
	writeLog(t, root, "2026-10-04", "b.log", strings.ReplaceAll(fixtureLog, "deepseek-v4.1-flash", "glm-5.3-flash"))
	if got := scanner.Limits(now.Add(time.Minute)); len(got) != 1 {
		t.Fatalf("缓存期内不应重扫，实际 %d", len(got))
	}

	// 超过 TTL 后重扫，能看到新条目。
	got := scanner.Limits(now.Add(CacheTTL + time.Minute))
	if len(got) != 2 {
		t.Fatalf("TTL 过后应重扫出 2 条，实际 %d：%+v", len(got), got)
	}
}

func TestMissingRootIsNotAnError(t *testing.T) {
	got := New(filepath.Join(t.TempDir(), "not-exist")).Limits(time.Now())
	if len(got) != 0 {
		t.Fatalf("不存在的根应返回空，实际 %+v", got)
	}
	var nilScanner *Scanner
	if got := nilScanner.Limits(time.Now()); got != nil {
		t.Fatalf("nil Scanner 应返回 nil，实际 %+v", got)
	}
}
