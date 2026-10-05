package config

import (
	"strings"
	"testing"
	"time"
)

// TestServerReadTimeout 覆盖入站读取上限：空值回落默认 300s；"0" = 显式不限制
// （0 是合法值，不能被默认值顶掉）；负值/不可解析 fail fast
// （静默钳 0 等于把保护悄悄关掉，而用户以为自己设了保护）。
func TestServerReadTimeout(t *testing.T) {
	t.Run("默认 300s", func(t *testing.T) {
		c := newTestConfig(t)
		if c.Server.ReadTimeout != DefaultServerReadTimeout {
			t.Fatalf("默认 read_timeout=%q，期望 %s", c.Server.ReadTimeout, DefaultServerReadTimeout)
		}
		if c.ServerReadTimeout != 300*time.Second {
			t.Fatalf("默认时长=%v，期望 300s", c.ServerReadTimeout)
		}
		if err := c.ServerErr(); err != nil {
			t.Fatalf("默认值不应报错: %v", err)
		}
	})

	t.Run("显式 0 = 不限制", func(t *testing.T) {
		c := newTestConfig(t)
		save(t, c, `{"server":{"read_timeout":"0"}}`)
		if c.ServerReadTimeout != 0 {
			t.Fatalf("时长=%v，期望 0（不限制）", c.ServerReadTimeout)
		}
	})

	t.Run("负值 fail fast", func(t *testing.T) {
		c := newTestConfig(t)
		if _, _, err := c.SavePatch([]byte(`{"server":{"read_timeout":"-5s"}}`)); err == nil {
			t.Fatal("负时长应被拒绝")
		}
	})

	t.Run("不可解析 fail fast", func(t *testing.T) {
		c := newTestConfig(t)
		if _, _, err := c.SavePatch([]byte(`{"server":{"read_timeout":"bogus"}}`)); err == nil {
			t.Fatal("不可解析的时长应被拒绝")
		}
	})

	t.Run("保存后提示需重启", func(t *testing.T) {
		c := newTestConfig(t)
		applied, needRestart := save(t, c, `{"server":{"read_timeout":"60s"}}`)
		if len(applied) != 0 {
			t.Fatalf("server 段是装配期字段，不该出现在热生效列表: %v", applied)
		}
		if !containsStr(needRestart, "server.read_timeout") {
			t.Fatalf("需重启列表应包含 server.read_timeout，实际 %v", needRestart)
		}
		if c.Server.ReadTimeout != "60s" {
			t.Fatalf("内存值应同步为新值供展示，实际 %q", c.Server.ReadTimeout)
		}
	})
}

// TestIncludeDisabledInTasks 覆盖「保号任务覆盖已禁用账号」开关。
//
// 默认 false = 保持既有行为（禁用即跳过四类保号任务）；打开是热生效字段
// （调度器与任务中心每次现场读配置），因此必须出现在 applied 列表里。
func TestIncludeDisabledInTasks(t *testing.T) {
	c := newTestConfig(t)
	if c.IncludeDisabledInTasks() {
		t.Fatal("默认应关闭（对老配置零影响）")
	}

	applied, _ := save(t, c, `{"schedule":{"include_disabled_in_tasks":true}}`)
	if !containsStr(applied, "schedule") {
		t.Fatalf("schedule 段应为热生效，实际 applied=%v", applied)
	}
	if !c.IncludeDisabledInTasks() {
		t.Fatal("开关应为打开")
	}

	save(t, c, `{"schedule":{"include_disabled_in_tasks":false}}`)
	if c.IncludeDisabledInTasks() {
		t.Fatal("开关应能被关回去")
	}
}

// TestServerSectionSurvivesPartialSchedulePatch server 段与 schedule 段互不干扰。
func TestServerSectionSurvivesPartialSchedulePatch(t *testing.T) {
	c := newTestConfig(t)
	save(t, c, `{"server":{"read_timeout":"45s"}}`)
	save(t, c, `{"schedule":{"include_disabled_in_tasks":true}}`)
	if c.Server.ReadTimeout != "45s" {
		t.Fatalf("改 schedule 后 server.read_timeout 被重置为 %q", c.Server.ReadTimeout)
	}
	if !strings.Contains(c.Server.ReadTimeout, "45s") {
		t.Fatalf("read_timeout=%q", c.Server.ReadTimeout)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
