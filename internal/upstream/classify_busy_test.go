package upstream

import "testing"

// 本文件覆盖 wb2api-panel 的两个协议级分类修复（见 classify.go 的
// ErrUpstreamBusy / ErrModelDenied）。
//
// 这两条都极易被「看起来更通用」的规则抢走：
//   - 6021 上游给的是 HTTP 429，若不先判就会落进 ErrSoftRate；
//   - 11158 若只按状态码走，会掉进 unknown 连败降权。
// 因此断言必须同时钉住「分到哪一类」和「罚不罚号」。

func TestClassify6021UpstreamBusy(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"结构化 code", `{"code":6021,"msg":"queue.full.title"}`},
		{"嵌套 data", `{"code":0,"data":{"code":6021,"msg":"queue.full.title"}}`},
		{"只有文案", `{"msg":"queue.full.title"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Classify(429, tc.body)
			if c.Kind != ErrUpstreamBusy {
				t.Fatalf("6021 应归为 %s，实际 %s（body=%s）", ErrUpstreamBusy, c.Kind, tc.body)
			}
			if !c.Retryable {
				t.Error("渠道满载是可重试的，Retryable 应为 true")
			}
			if c.Punishable() {
				t.Error("渠道容量事件不该处罚账号")
			}
		})
	}
}

// TestClassify6021BeforeSoftRate 6021 必须排在 429 兜底之前 —— 否则整池会被误杀。
func TestClassify6021BeforeSoftRate(t *testing.T) {
	c := Classify(429, `{"code":6021,"msg":"queue.full.title"}`)
	if c.Kind == ErrSoftRate {
		t.Fatal("6021 落进了 ErrSoftRate：这正是会把整池健康号逐号软冷却的路径")
	}
}

// TestClassify6021DoesNotOverreach 别把普通限流误判成渠道满载。
func TestClassify6021DoesNotOverreach(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"msg":"rate limit exceeded"}`,
		`{"code":6004,"msg":"too many requests"}`,
		`{"code":14018,"msg":"some other busy"}`,
	} {
		c := Classify(429, body)
		if c.Kind == ErrUpstreamBusy {
			t.Errorf("误判成渠道满载: %s", body)
		}
	}
}

func TestClassify11158ModelDenied(t *testing.T) {
	c := Classify(400, `{"code":11158,"msg":"model not permitted for this account","model":"glm-5.3"}`)
	if c.Kind != ErrModelDenied {
		t.Fatalf("11158 应归为 %s，实际 %s", ErrModelDenied, c.Kind)
	}
	if c.Model != "glm-5.3" {
		t.Errorf("应提取出触发模型，实际 %q", c.Model)
	}
	if !c.Punishable() {
		t.Error("模型级拒绝仍应进入模型锁（Punishable 为真），只是锁的粒度是 (账号,模型)")
	}
}

// TestClassify11158NotConfusedWithOthers 11158 不能与相邻的业务码串味。
func TestClassify11158NotConfusedWithOthers(t *testing.T) {
	c := Classify(400, `{"code":11101,"msg":"Unmarshal chat params failed"}`)
	if c.Kind != ErrBadRequest {
		t.Errorf("11101 应仍归为 bad_request，实际 %s", c.Kind)
	}
	c = Classify(400, `{"code":11102,"msg":"not found"}`)
	if c.Kind == ErrModelDenied {
		t.Error("11102 不该被判成 11158 模型权限拒绝")
	}
}
