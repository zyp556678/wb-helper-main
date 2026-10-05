package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy-gateway/internal/tasks"
)

// TestWriteActionErrorStatusMapping 守住动作型端点的状态码语义。
//
// 背景（真实用户反馈）：点「猫咪出发」报 502。追问下去发现，
// 那条 502 的 detail 是「国际站没有成长中心活动」——一个**确定性结论**，
// 却被塞进了语义为「上游网关坏了」的 502。
//
// 这件事的危害不在于文案，而在于**诱导用户做无用功**：
// 502 会让人以为等等就好、重试就好，于是反复点击一个永远不会成功的按钮。
// 所以这里把映射关系钉死：前提不满足 → 409，其它 → 502。
func TestWriteActionErrorStatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{
			name:       "前提不满足应为 409",
			err:        &tasks.PreconditionError{Msg: "国际站 没有成长中心活动，无法执行旅行动作"},
			wantStatus: http.StatusConflict,
		},
		{
			name:       "上游故障应为 502",
			err:        errors.New("成长域请求失败 HTTP 502: upstream unreachable"),
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeActionError(rec, c.err)

			if rec.Code != c.wantStatus {
				t.Errorf("状态码 = %d，期望 %d", rec.Code, c.wantStatus)
			}

			// 文案必须原样带出去：前端读的就是这个 detail。
			var body struct {
				OK     bool   `json:"ok"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
			}
			if body.OK {
				t.Error("失败响应的 ok 应为 false")
			}
			if body.Detail != c.err.Error() {
				t.Errorf("detail 被丢失或改写：\n got = %q\nwant = %q", body.Detail, c.err.Error())
			}
		})
	}
}

// TestWriteActionErrorExtraKeepsExtraFields 守住 writeActionErrorExtra 不会丢掉附加字段。
//
// 配置页的「立即执行」端点要靠 task 字段区分是哪条任务失败了；
// 丢了它，前端就只能显示一句没有指向的失败。
func TestWriteActionErrorExtraKeepsExtraFields(t *testing.T) {
	rec := httptest.NewRecorder()
	writeActionErrorExtra(rec, errors.New("上游超时"), map[string]any{"task": "checkin"})

	var body struct {
		OK     bool   `json:"ok"`
		Task   string `json:"task"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Task != "checkin" {
		t.Errorf("task 字段丢失：got = %q", body.Task)
	}
	if body.Detail != "上游超时" {
		t.Errorf("detail 字段丢失：got = %q", body.Detail)
	}
}
