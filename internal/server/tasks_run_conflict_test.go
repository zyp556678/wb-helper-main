package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTasksRunConflictUsesActionEnvelope 守住 /tasks/run 冲突时的响应信封。
//
// 为什么单独测这一条：前端对动作型端点的处理是
//
//	const res = await runTasks(...)
//	if (res.ok === false) toast.error(res.detail)
//	else if (res.planned > 0) ...
//
// 它**依赖响应体里有 ok 与 detail 两个平铺字段**。
// 若后端在这里回 errBody（{error:{code,message}}），前端两个分支都读不到，
// 只能掉进 catch，把「队列正在跑」显示成一句无从下手的失败。
//
// 这类「信封换了、字段读不到」的故障不会报错，只会让文案变差，
// 所以必须用测试把信封形状钉死。
func TestTasksRunConflictUsesActionEnvelope(t *testing.T) {
	s := newTestServer(t, "")
	mux := http.NewServeMux()
	mux.HandleFunc("/panel/api/tasks/run", s.handleTasksRun)

	// 第一次调用让队列进入 preparing/running；第二次必然撞上「已有队列在执行中」。
	// 队列一旦被置起就不会因为这次测试而立刻结束，所以第二次调用是稳定的。
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/panel/api/tasks/run",
			strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	first := call()
	if first.Code != http.StatusOK {
		t.Fatalf("首次调用应被受理，得到 %d：%s", first.Code, first.Body.String())
	}

	second := call()
	if second.Code != http.StatusConflict {
		t.Fatalf("队列在跑时第二次调用应为 409，得到 %d：%s", second.Code, second.Body.String())
	}

	var body struct {
		OK      bool   `json:"ok"`
		Detail  string `json:"detail"`
		Running bool   `json:"running"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &body); err != nil {
		t.Fatalf("409 响应不是合法 JSON: %v（body=%s）", err, second.Body.String())
	}
	if body.OK {
		t.Error("409 的 ok 应为 false，否则前端会按成功处理")
	}
	if strings.TrimSpace(body.Detail) == "" {
		t.Error("409 缺少 detail，前端只能显示一句 generic 失败")
	}
	if !body.Running {
		t.Error("409 应带 running:true，便于前端立刻把队列视为活跃")
	}
}
