package server

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy-gateway/internal/auth"
)

// sitesCacheFixture 是一份「国内站免费、国际站收费」的目录缓存。
//
// 用缓存文件而不是打真实上游：这里要验证的是**接线**（配置开关 → 目录结论 →
// 调度决策），不是上游响应解析 —— 后者已经由 catalog 包的测试覆盖。
const sitesCacheFixture = `{
  "schema": 1,
  "source": "test-fixture",
  "fetchedAt": {"cn": 1, "intl": 1},
  "catalogs": {
    "cn":   [{"id": "hy3", "hasMultiplier": true, "multiplier": 0}],
    "intl": [{"id": "hy3", "hasMultiplier": true, "multiplier": 1.5}]
  }
}`

// TestPreferredSitesWiring 验证「免费站点优先」从配置到调度决策的整条链路。
//
// 中间任何一段没接上，用户看到的现象都是「配置项在、但调度永远不倾斜」——
// 不报错、不提示，只能靠测试守住。
func TestPreferredSitesWiring(t *testing.T) {
	s := newTestServer(t, "")

	cachePath := filepath.Join(s.config().WorkDir, "wb-models-cache.json")
	if err := os.WriteFile(cachePath, []byte(sitesCacheFixture), 0o600); err != nil {
		t.Fatalf("写入目录缓存失败: %v", err)
	}
	s.cat.LoadCache()

	// 1) 默认开启：一侧免费、另一侧收费 → 倾斜到免费侧
	got := s.preferredSitesFor("hy3")
	if len(got) != 1 || !got[auth.SiteCN] {
		t.Fatalf("期望倾斜到国内站，实际 %v", got)
	}

	// 2) 通过配置接口关掉（走真实补丁路径，验证开关确实影响决策）
	if _, _, err := s.config().SavePatch([]byte(`{"pool":{"prefer_free_site":false}}`)); err != nil {
		t.Fatalf("关闭开关失败: %v", err)
	}
	if got := s.preferredSitesFor("hy3"); len(got) != 0 {
		t.Fatalf("开关关闭后不应倾斜，实际 %v", got)
	}

	// 3) 重新打开
	if _, _, err := s.config().SavePatch([]byte(`{"pool":{"prefer_free_site":true}}`)); err != nil {
		t.Fatalf("重新打开开关失败: %v", err)
	}
	if got := s.preferredSitesFor("hy3"); len(got) != 1 || !got[auth.SiteCN] {
		t.Fatalf("重新打开后应恢复倾斜，实际 %v", got)
	}

	// 4) 结论相同的模型不倾斜（另一侧未知同样不倾斜）
	if got := s.preferredSitesFor("unknown-model"); len(got) != 0 {
		t.Fatalf("结论未知的模型不应倾斜，实际 %v", got)
	}
}
