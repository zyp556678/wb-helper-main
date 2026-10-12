package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFile 覆盖写一份 config.json。
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}

// TestReloadFromFilePicksUpEditsAndDeletions 文件是运行层的唯一真源：
// 改了的键生效，**删掉的键回到默认值**。
//
// 后半条是这次设计的重点：如果在旧值上叠加文件内容，从 config.json 里删掉一个键
// 会让它继续以旧值生效 —— 「配置删了却依然生效」比「改了没生效」更难查。
func TestReloadFromFilePicksUpEditsAndDeletions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"upstream":{"header_timeout_seconds":42},`+
		`"models":{"blocklist":["a"]},"pool":{"max_in_flight":7}}`)

	cfg, err := Load(&Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.HeaderTimeout != 42*time.Second {
		t.Fatalf("文件里的上游超时未生效: %v", cfg.HeaderTimeout)
	}
	if len(cfg.ModelBlock) != 1 || cfg.Pool.MaxInFlight != 7 {
		t.Fatalf("文件里的名单/并发未生效: %v %d", cfg.ModelBlock, cfg.Pool.MaxInFlight)
	}

	// 改一个键，同时把 max_in_flight 从文件里删掉。
	writeFile(t, path, `{"upstream":{"header_timeout_seconds":90},"models":{"blocklist":["a","b"]}}`)
	next, err := cfg.ReloadFromFile()
	if err != nil {
		t.Fatalf("热重载失败: %v", err)
	}
	if next.HeaderTimeout != 90*time.Second {
		t.Fatalf("改动未生效: %v", next.HeaderTimeout)
	}
	if len(next.ModelBlock) != 2 {
		t.Fatalf("名单改动未生效: %v", next.ModelBlock)
	}
	if next.Pool.MaxInFlight != 3 {
		t.Fatalf("从文件里删掉的键应回到默认值 3，实际 %d", next.Pool.MaxInFlight)
	}

	// 换快照不换连接：http.Client 必须共享（连接池只有一套）。
	if next.Chat != cfg.Chat || next.Control != cfg.Control {
		t.Fatal("热重载不应重建 http.Client（连接池会被凭空多出一套）")
	}
	// 启动层字段只能由命令行决定，文件不该动它们。
	if next.WorkDir != cfg.WorkDir || next.ConfigFile != cfg.ConfigFile {
		t.Fatal("热重载不应改动启动层字段")
	}
	// 原快照保持冻结（并发读的那份不能被就地改写）。
	if cfg.HeaderTimeout != 42*time.Second || cfg.Pool.MaxInFlight != 7 {
		t.Fatal("旧快照被就地改写了")
	}
}

// TestReloadFromFileRejectsBadConfig 坏配置返回错误，调用方据此保留上一份有效配置。
func TestReloadFromFileRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, `{"upstream":{"header_timeout_seconds":42}}`)

	cfg, err := Load(&Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"语法错误", `{"upstream":`},
		{"读取上限非法", `{"server":{"read_timeout":"-5s"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, path, tc.body)
			if _, err := cfg.ReloadFromFile(); err == nil {
				t.Fatal("坏配置应当报错")
			}
			// 报错不该污染当前生效的配置。
			if cfg.HeaderTimeout != 42*time.Second {
				t.Fatalf("当前配置被改坏了: %v", cfg.HeaderTimeout)
			}
		})
	}
}

// TestReloadFromFileWithoutConfigFile 没有 config.json 时等价于「全默认值」，
// 且不会把启动层字段当成配置项。
func TestReloadFromFileWithoutConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(&Config{WorkDir: dir, Addr: "0.0.0.0", Port: 9000})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	next, err := cfg.ReloadFromFile()
	if err != nil {
		t.Fatalf("热重载失败: %v", err)
	}
	if next.Addr != "0.0.0.0" || next.Port != 9000 {
		t.Fatalf("启动层字段被文件重载破坏: %s:%d", next.Addr, next.Port)
	}
	if next.HeaderTimeout != DefaultUpstreamHeaderTimeout {
		t.Fatalf("无配置文件时应回落默认超时，实际 %v", next.HeaderTimeout)
	}
}

// TestWatchTracksContentHash 游标按**内容**判断变化，并抑制同一份坏内容的重复告警。
func TestWatchTracksContentHash(t *testing.T) {
	w := NewWatch()
	if !w.Changed([]byte("a")) {
		t.Fatal("从未记录过内容时应视为有变化")
	}
	w.MarkApplied([]byte("a"))
	if w.Changed([]byte("a")) {
		t.Fatal("同一份内容不应重复触发")
	}
	if !w.Changed([]byte("b")) {
		t.Fatal("内容变了应触发")
	}

	if !w.MarkFailed([]byte("bad")) {
		t.Fatal("第一次见到坏内容应告警")
	}
	if w.MarkFailed([]byte("bad")) {
		t.Fatal("同一份坏内容不该每秒重复告警")
	}
	if !w.MarkFailed([]byte("bad-2")) {
		t.Fatal("换了一份坏内容应重新告警")
	}
	// 成功应用之后，再遇到之前那份坏内容应当重新告警（用户可能又改回来了）。
	w.MarkApplied([]byte("good"))
	if !w.MarkFailed([]byte("bad")) {
		t.Fatal("应用成功后再遇到坏内容应重新告警")
	}
}
