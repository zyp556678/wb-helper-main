package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCred(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"auth":{"accessToken":"T","refreshToken":"R","expiresAt":4102444800},`+
		`"account":{"uid":"u1"},"edition":"cn"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoverEmptyWorkDirReturnsNothing 工作目录里一个凭据文件都没有时返回空列表。
//
// 这条钉的是一个真实缺陷：此前会「回落到默认文件名 workbuddy.json」，于是全新安装
// （桌面壳刚装好、还没登录）的账号池会记下一条「读取凭据文件失败 (…/workbuddy.json):
// no such file or directory」，面板据此把整页渲染成「状态快照暂不可用」——
// 用户以为自己装坏了，其实他只是还没加账号。
func TestDiscoverEmptyWorkDirReturnsNothing(t *testing.T) {
	dir := t.TempDir()
	if got := Discover(dir, "workbuddy.json", "", false); len(got) != 0 {
		t.Fatalf("空目录不该发现任何凭据路径，实际 %v", got)
	}
}

// TestDiscoverScansWorkDir 工作目录里的 workbuddy*.json 全部收进来（排序稳定）。
func TestDiscoverScansWorkDir(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, filepath.Join(dir, "workbuddy-b.json"))
	writeCred(t, filepath.Join(dir, "workbuddy-a.json"))
	// 非凭据文件不该被收进来。
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := Discover(dir, "workbuddy.json", "", false)
	if len(got) != 2 {
		t.Fatalf("应发现 2 个凭据文件，实际 %v", got)
	}
	if filepath.Base(got[0]) != "workbuddy-a.json" || filepath.Base(got[1]) != "workbuddy-b.json" {
		t.Fatalf("顺序应稳定（排序），实际 %v", got)
	}
}

// TestDiscoverExplicitMissingFileStillReported 显式指定的文件即使不存在也要返回 ——
// 那是用户指明要读的，读不到必须报错，而不是当成「还没登录」静默跳过。
func TestDiscoverExplicitMissingFileStillReported(t *testing.T) {
	dir := t.TempDir()
	got := Discover(dir, "my-creds.json", "", true)
	if len(got) != 1 || filepath.Base(got[0]) != "my-creds.json" {
		t.Fatalf("显式指定的路径应当原样返回（由调用方报错），实际 %v", got)
	}
}

// TestDiscoverAuthDirTakesPrecedence 指定了凭据目录时只扫那个目录、不看工作目录。
func TestDiscoverAuthDirTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, filepath.Join(dir, "workbuddy-in-workdir.json"))
	authDir := t.TempDir()
	writeCred(t, filepath.Join(authDir, "workbuddy-intl-a.json"))

	got := Discover(dir, "workbuddy.json", authDir, false)
	if len(got) != 1 || filepath.Base(got[0]) != "workbuddy-intl-a.json" {
		t.Fatalf("指定 authDir 时只该扫它（凭据文件名以 workbuddy 开头），实际 %v", got)
	}
}

// TestDiscoverSkipsDisabledMarker 已手工禁用（.disabled 标记）不算凭据文件。
func TestDiscoverSkipsDisabledMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-a.json.disabled"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Discover(dir, "workbuddy.json", "", false); len(got) != 0 {
		t.Fatalf(".disabled 标记不该被当成凭据文件，实际 %v", got)
	}
}
