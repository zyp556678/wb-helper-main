package localsessions

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"workbuddy-gateway/internal/localapps"
)

// 造一个与真实库同形的会话库。
//
// 列名与实测一致（sessions 表），**故意多放一列 `extra_col`**：
// 复制走的是「读整行 → 只覆盖身份列」的路径，多一列能验证新列不会丢。
func setupStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	// 备份设施会往 %LOCALAPPDATA% 下写，测试里要隔离掉，否则污染真实目录。
	local := filepath.Join(root, "local")
	for _, d := range []string{home, local} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LOCALAPPDATA", local)

	s := NewStore(home)
	// 状态目录必须显式注入：空值时关联功能会（正确地）拒绝工作 ——
	// 否则测试会把登记表写到进程 CWD（go test 的包目录）里，留下垃圾。
	s.StateDir = filepath.Join(root, "state")
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, cwd TEXT, user_id TEXT, title TEXT, custom_title TEXT,
		status TEXT, created_at INTEGER, updated_at INTEGER, last_activity_at INTEGER,
		deleted_at INTEGER, is_playground INTEGER, source_mode TEXT,
		is_background_automation INTEGER, mode TEXT, model TEXT, expert_id TEXT,
		extra_col TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	now := int64(1759000000000) // 毫秒，与实测一致
	_, err = db.Exec(`INSERT INTO sessions VALUES
		('src-1111','D:\software\workbuddy-gateway','uid-A','原会话','ct',
		 'active',? ,? ,? ,0,0,'cli',0,'code','deepseek-v4','','KEEP-ME')`, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO sessions VALUES
		('del-2222','D:\software\workbuddy-gateway','uid-A','已删除',NULL,
		 'active',?,?,?,12345,0,'cli',0,'code','m','','')`, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// 正文：首行 session-meta，其余行带 sessionId；
	// **其中一行的正文内容里故意嵌入同一串 uuid** —— 用来验证「只改字段值、
	// 不改消息正文」，这是逐行解析与朴素字符串替换的分水岭。
	dir := filepath.Join(s.Root, "projects", "d-software-workbuddy-gateway")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 第 5 行是**合法但结构不常见**的一行（没有 sessionId 字段）：
	// 复制要求内容可验证（每行合法 JSON），但**不要求**每行都长得像会话记录 ——
	// 客户端将来加新行类型时不该因为我们认不出来就拒绝复制。
	// 第 7 行是**关键边界**：消息正文里贴着**转义过的**字段片段。
	// 在文件字节里它是 `\"sessionId\":\"src-1111\"`（引号被转义），
	// 所以「未被反斜杠转义的 `"sessionId":"`」这个判据不会误伤它。
	// 这正是「精准字节替换」能安全工作的依据。
	lines := []string{
		`{"type":"session-meta","sessionId":"src-1111","id":"m1","timestamp":1}`,
		`{"type":"user","sessionId":"src-1111","id":"m2","cwd":"D:\\software\\workbuddy-gateway","text":"hello"}`,
		`{"type":"assistant","sessionId":"src-1111","id":"m3","text":"我之前的会话 id 是 src-1111，请记住"}`,
		`{"type":"user","sessionId":"OTHER-ID","id":"m4","text":"别的会话"}`,
		`{"type":"system","id":"m6","note":"结构不常见但合法的行"}`,
		`{"type":"user","sessionId":"src-1111","id":"m5","text":"尾行"}`,
		`{"type":"user","sessionId":"src-1111","id":"m7","text":"我贴过 \"sessionId\":\"src-1111\" 这段文本"}`,
	}
	if err := os.WriteFile(filepath.Join(dir, "src-1111.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestListSessionsExcludesDeleted(t *testing.T) {
	s, _ := setupStore(t)

	all, err := s.ListSessions("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("应只列未删除的 1 条，实际 %d（%+v）", len(all), all)
	}
	if all[0].ID != "src-1111" {
		t.Fatalf("应为 src-1111，实际 %s", all[0].ID)
	}
	if !all[0].HasBody || all[0].BodyBytes == 0 {
		t.Fatal("应识别出正文存在")
	}

	// 按账号过滤。
	got, err := s.ListSessions("uid-A", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("uid-A 应有 1 条，实际 %d", len(got))
	}
	none, err := s.ListSessions("uid-nonexistent", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("不存在的账号应为空，实际 %d", len(none))
	}

	uids, err := s.ListUserIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 1 || uids[0] != "uid-A" {
		t.Fatalf("应只列出 uid-A，实际 %v", uids)
	}
}

// 复制的主路径：新行归属正确、正文重写、**源完全不动**、多出来的列不丢。
func TestCopySessionRewritesBodyAndKeepsSource(t *testing.T) {
	s, dir := setupStore(t)
	srcBody := filepath.Join(dir, "src-1111.jsonl")
	before, _ := os.ReadFile(srcBody)

	res, err := s.CopySession("src-1111", CopyOptions{TargetUID: "uid-B"})
	if err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	if res.NewID == "" || res.NewID == "src-1111" {
		t.Fatalf("新 id 应非空且不同于源，实际 %q", res.NewID)
	}
	if res.BackupID == "" {
		t.Fatal("必须产生备份")
	}
	// 7 行里 5 行 sessionId == src-1111（第 4 行是 OTHER-ID，第 5 行没有 sessionId 字段）。
	if res.BodyLines != 5 {
		t.Fatalf("应重写 5 行的 sessionId，实际 %d", res.BodyLines)
	}

	// 源正文**一个字节都不能变**。
	after, _ := os.ReadFile(srcBody)
	if string(before) != string(after) {
		t.Fatal("源正文被改动了 —— 本工具绝不该动已有会话")
	}

	// 新正文：sessionId 全换成新 id。
	newBody, err := os.ReadFile(res.BodyPath)
	if err != nil {
		t.Fatalf("新正文不存在: %v", err)
	}
	text := string(newBody)
	if strings.Contains(text, `"sessionId":"src-1111"`) {
		t.Fatal("新正文里仍残留旧 sessionId")
	}
	if strings.Count(text, `"sessionId":"`+res.NewID+`"`) != 5 {
		t.Fatalf("应有 5 行 sessionId 被改为新 id，实际 %d",
			strings.Count(text, `"sessionId":"`+res.NewID+`"`))
	}
	// 别的会话的 sessionId 不该被碰。
	if !strings.Contains(text, `"sessionId":"OTHER-ID"`) {
		t.Fatal("别的会话的 sessionId 被误改了")
	}
	// **消息正文里那串 uuid 必须原样保留** —— 只改字段值，不改对话内容。
	if !strings.Contains(text, "我之前的会话 id 是 src-1111，请记住") {
		t.Fatal("消息正文被改动了：这属于篡改对话内容（朴素字符串替换就会这样）")
	}
	// **关键边界**：正文里贴着**转义过的**字段片段，必须原封不动。
	if !strings.Contains(text, `\"sessionId\":\"src-1111\"`) {
		t.Fatal("正文里被转义的字段片段被误改了 —— 这正是「只认未转义字段名」这条判据要防的")
	}
	// 结构不常见（没有 sessionId）的合法行必须原样保留（丢一行就是丢用户内容）。
	if !strings.Contains(text, "结构不常见但合法的行") {
		t.Fatal("结构不常见的合法行被丢掉了")
	}
	// **最强断言：新正文必须逐字节等于「源正文只做了该替换」的结果。**
	//
	// 这一条同时钉住两件事：
	//   - 没有多做（键序、HTML 转义、空白都没被动过 —— 第一版全量重序列化会在这里红）；
	//   - 没有少做（5 处字段值都换了）。
	// 注意 `strings.ReplaceAll` 不会命中正文里那段**转义过**的片段
	// （文件里是 `\"sessionId\":\"src-1111\"`，`sessionId` 后面跟的是 `\` 不是 `"`），
	// 所以期望值与实现口径一致。
	expected := strings.ReplaceAll(string(before), `"sessionId":"src-1111"`,
		`"sessionId":"`+res.NewID+`"`)
	if string(newBody) != expected {
		t.Fatalf("新正文与「仅替换 sessionId」的期望结果不一致：\n  长度 %d vs %d",
			len(newBody), len(expected))
	}
	// 长度差必须恰好等于 5 处替换的净增（源 id 8 字符、新 id 36 字符）。
	if want := len(before) + 5*(len(res.NewID)-len("src-1111")); len(newBody) != want {
		t.Fatalf("长度应为 %d，实际 %d", want, len(newBody))
	}

	// 数据库侧：新行归属正确、标题带后缀、源行不动、额外列不丢。
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(s.DBPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var uid, title, extra, cwd string
	err = db.QueryRow(`SELECT user_id, title, extra_col, cwd FROM sessions WHERE id = ?`, res.NewID).
		Scan(&uid, &title, &extra, &cwd)
	if err != nil {
		t.Fatalf("新行不存在: %v", err)
	}
	if uid != "uid-B" {
		t.Fatalf("归属应为 uid-B，实际 %q", uid)
	}
	if !strings.HasSuffix(title, "（副本）") {
		t.Fatalf("标题应带副本后缀，实际 %q", title)
	}
	if extra != "KEEP-ME" {
		t.Fatalf("额外列应被照抄（写死列名会丢它），实际 %q", extra)
	}
	if cwd == "" {
		t.Fatal("cwd 应被照抄")
	}

	var srcUID string
	if err := db.QueryRow(`SELECT user_id FROM sessions WHERE id='src-1111'`).Scan(&srcUID); err != nil {
		t.Fatal(err)
	}
	if srcUID != "uid-A" {
		t.Fatalf("源行归属被改动了：%q", srcUID)
	}
	// 源行必须还在。
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id='src-1111'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("源会话记录被删了 —— 本工具绝不该删已有会话")
	}
}

// 目标账号就是源账号时必须拒绝：那是无意义的自我复制。
func TestCopySessionRejectsSameUser(t *testing.T) {
	s, _ := setupStore(t)
	_, err := s.CopySession("src-1111", CopyOptions{TargetUID: "uid-A"})
	if err == nil {
		t.Fatal("同账号复制应被拒绝")
	}
	if !strings.Contains(err.Error(), "本来就属于") {
		t.Fatalf("错误信息应说明原因，实际: %v", err)
	}
}

func TestCopySessionRejectsEmptyTarget(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := s.CopySession("src-1111", CopyOptions{}); err == nil {
		t.Fatal("空目标账号应被拒绝")
	}
}

func TestCopySessionRejectsMissingSource(t *testing.T) {
	s, _ := setupStore(t)
	if _, err := s.CopySession("no-such-session", CopyOptions{TargetUID: "uid-B"}); err == nil {
		t.Fatal("源会话不存在时应报错")
	}
}

// 预演模式：只备份、不写任何东西。
func TestCopySessionDryRunWritesNothing(t *testing.T) {
	s, dir := setupStore(t)

	before, _ := os.ReadDir(dir)
	res, err := s.CopySession("src-1111", CopyOptions{TargetUID: "uid-B", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.BackupID == "" {
		t.Fatal("预演也应备份（这样后续真跑能立刻恢复）")
	}
	after, _ := os.ReadDir(dir)
	if len(before) != len(after) {
		t.Fatalf("预演不该新增文件：%d → %d", len(before), len(after))
	}
	db, _ := sql.Open("sqlite", "file:"+filepath.ToSlash(s.DBPath))
	defer db.Close()
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 2 {
		t.Fatalf("预演不该新增行，实际 %d", n)
	}
}

// 只读打开必须真的是只读：写入应当失败。
//
// 实时库正被 WorkBuddy 客户端使用，读路径一旦能写，就有误伤它的可能。
func TestReadOnlyOpenRejectsWrite(t *testing.T) {
	s, _ := setupStore(t)
	db, err := s.openLib(&s.Libraries[0], true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO sessions (id) VALUES ('x')`); err == nil {
		t.Fatal("只读连接不该允许写入")
	}
}

func TestFlattenWorkspace(t *testing.T) {
	cases := map[string]string{
		`D:\software\workbuddy-gateway`: "d-software-workbuddy-gateway",
		`C:/Users/x/WorkBuddy`:          "c-users-x-workbuddy",
		``:                              "",
	}
	for in, want := range cases {
		if got := flattenWorkspace(in); got != want {
			t.Fatalf("flattenWorkspace(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// newUUID 必须给出合法的 v4 形态，且互不相同。
func TestNewUUIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		u, err := newUUID()
		if err != nil {
			t.Fatal(err)
		}
		if len(u) != 36 {
			t.Fatalf("长度应为 36，实际 %d (%q)", len(u), u)
		}
		parts := strings.Split(u, "-")
		if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 ||
			len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
			t.Fatalf("形态不对: %q", u)
		}
		if parts[2][0] != '4' {
			t.Fatalf("版本位应为 4: %q", u)
		}
		if seen[u] {
			t.Fatalf("出现重复 UUID: %q", u)
		}
		seen[u] = true
	}
}

// 备份必须真的能被列出来（否则「可恢复」只是一句话）。
func TestCopySessionBackupIsListable(t *testing.T) {
	s, _ := setupStore(t)
	res, err := s.CopySession("src-1111", CopyOptions{TargetUID: "uid-B"})
	if err != nil {
		t.Fatal(err)
	}
	list, err := localapps.ListBackups(20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range list {
		if b.ID == res.BackupID {
			found = true
			if b.Target != localapps.TargetSessions {
				t.Fatalf("备份目标应为会话库，实际 %s", b.Target)
			}
			// 备份里必须含 DB 与正文。
			var hasDB, hasBody bool
			for _, f := range b.Files {
				if strings.HasSuffix(f.Original, "workbuddy.db") && f.Existed {
					hasDB = true
				}
				if strings.HasSuffix(f.Original, ".jsonl") && f.Existed {
					hasBody = true
				}
			}
			if !hasDB || !hasBody {
				t.Fatalf("备份应含 DB 与正文，实际 %+v", b.Files)
			}
		}
	}
	if !found {
		t.Fatalf("备份 %s 未出现在列表里", res.BackupID)
	}
}
