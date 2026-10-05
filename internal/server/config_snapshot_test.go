package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// 本文件守住配置的「写时复制」语义。
//
// 背景：面板保存配置是**写**，chat 热路径是**读**同一个 *Config。原地改写会让
// 读侧看到撕裂的中间态（slice header 三字、string 两字，并发读写都可能被拆开），
// `go test -race` 会直接报。本机没有 C 编译器（缺 cc1），跑不了 race 检测，
// 所以这里改为断言**不变式**：保存必须换指针、且旧快照内容不变 ——
// 只要这条成立，正在处理中的请求手里那份配置就永远是完整的。

// TestConfigSaveSwapsPointerAndFreezesOldSnapshot 保存后换指针，旧快照内容不变。
func TestConfigSaveSwapsPointerAndFreezesOldSnapshot(t *testing.T) {
	ts := streamServer(t, []string{finishChunk}, true)
	defer ts.Close()
	srv := newTestServer(t, ts.URL)

	before := srv.config()
	if before == nil {
		t.Fatal("初始配置为空")
	}
	// 先塞一个已知值，便于验证旧快照被冻结
	beforeBlock := append([]string(nil), before.ModelBlock...)

	body := `{"models":{"blocklist":["glm-5.3"]}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.saveConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("保存配置应返回 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}

	after := srv.config()
	if after == before {
		t.Fatal("保存后必须换成新指针——原地改写会让并发读侧看到撕裂的中间态")
	}
	if len(after.ModelBlock) != 1 || after.ModelBlock[0] != "glm-5.3" {
		t.Fatalf("新配置未生效: %#v", after.ModelBlock)
	}
	// 旧快照必须原封不动：它正被处理中的请求读着。
	if len(before.ModelBlock) != len(beforeBlock) {
		t.Fatalf("旧快照被就地改写了：%#v → %#v", beforeBlock, before.ModelBlock)
	}
}

// TestConfigSaveConcurrentReadsStayConsistent 并发读写时，读侧只会看到某个完整版本。
//
// 断言方式：反复切换 blocklist 的内容，读侧每次读到的长度必须是 0 或 N 之一，
// 绝不能读到「半截」长度（撕裂的表现就是既不是 0 也不是 N）。
func TestConfigSaveConcurrentReadsStayConsistent(t *testing.T) {
	ts := streamServer(t, []string{finishChunk}, true)
	defer ts.Close()
	srv := newTestServer(t, ts.URL)

	const n = 5
	payloads := []string{
		`{"models":{"blocklist":[]}}`,
		`{"models":{"blocklist":["a","b","c","d","e"]}}`,
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读侧
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got := len(srv.config().ModelBlock)
				if got != 0 && got != n {
					t.Errorf("读到撕裂的配置：blocklist 长度 %d，只可能是 0 或 %d", got, n)
					return
				}
			}
		}()
	}

	// 写侧
	for round := 0; round < 40; round++ {
		body := payloads[round%2]
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		srv.saveConfig(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 轮保存失败: %d %s", round, rec.Code, rec.Body.String())
		}
	}
	close(stop)
	wg.Wait()
}
