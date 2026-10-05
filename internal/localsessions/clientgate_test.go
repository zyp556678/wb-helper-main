package localsessions

import (
	"os"
	"testing"
	"time"
)

// TestMain 给整个包装上「客户端不在运行、登录态读不到、关不掉也不启动任何东西」的兜底替身。
//
// **这不是洁癖，是必须的**：复制/同步的门禁会真的去枚举、结束、启动跑测试这台机器上
// 的 WorkBuddy 客户端 —— 真跑起来会把用户正在用的客户端关掉（账号切换侧已经真实
// 发生过一次）。需要特定行为的用例自己替换替身（见 fakeClientWorld），Cleanup 后回到这里。
func TestMain(m *testing.M) {
	restore := installNoopClientWorld()
	code := m.Run()
	restore()
	os.Exit(code)
}

// installNoopClientWorld 装上「没有客户端在运行」的替身，返回还原函数。
func installNoopClientWorld() func() {
	oldRunning, oldUID, oldClose, oldLaunch := clientRunning, clientLoginUID, clientClose, clientLaunch
	clientRunning = func(string) bool { return false }
	clientLoginUID = func(string) (string, bool) { return "", false }
	clientClose = func(string, time.Duration) ([]string, bool, string, error) { return nil, false, "", nil }
	clientLaunch = func(string, string) (string, error) { return "", nil }
	return func() {
		clientRunning, clientLoginUID, clientClose, clientLaunch = oldRunning, oldUID, oldClose, oldLaunch
	}
}

// fakeClientWorld 替换门禁探针，记录关闭/启动过的档位。
type fakeClientWorld struct {
	running map[string]bool
	uids    map[string]string
	closed  []string
	opened  []string
	// closeErr 非空时模拟「客户端关不掉」。
	closeErr error
}

func newFakeClientWorld(t *testing.T, w *fakeClientWorld) *fakeClientWorld {
	t.Helper()
	if w.running == nil {
		w.running = map[string]bool{}
	}
	if w.uids == nil {
		w.uids = map[string]string{}
	}
	oldRunning, oldUID, oldClose, oldLaunch := clientRunning, clientLoginUID, clientClose, clientLaunch
	clientRunning = func(site string) bool { return w.running[site] }
	clientLoginUID = func(site string) (string, bool) {
		uid, ok := w.uids[site]
		return uid, ok
	}
	clientClose = func(site string, _ time.Duration) ([]string, bool, string, error) {
		if w.closeErr != nil {
			return nil, true, "", w.closeErr
		}
		w.closed = append(w.closed, site)
		return []string{"已关闭客户端"}, true, "", nil
	}
	clientLaunch = func(site, _ string) (string, error) {
		w.opened = append(w.opened, site)
		return "已重新打开", nil
	}
	t.Cleanup(func() {
		clientRunning, clientLoginUID, clientClose, clientLaunch = oldRunning, oldUID, oldClose, oldLaunch
	})
	return w
}
