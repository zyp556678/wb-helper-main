package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoHardcodedPanelOrigin 守住「面板请求址必须是同源相对路径」这条不变量。
//
// 为什么值得用测试钉住：面板由网关在 /panel/ 下提供，而用户访问它的地址可能是
// 任意 host:port —— 本机 http://127.0.0.1:8317、SSH 端口转发、局域网地址、
// 桌面壳里的自定义端口。前端只要把**请求基址**写成绝对地址，除本机默认端口外的
// 所有场景都会连不上（表现为「页面能打开、数据全是错误」），而开发期在本机跑
// 永远不会暴露这个问题。
//
// 只盯「赋值形态」：`BASE = "http://127.0.0.1:..."`、`target: "..."` 这类。
// 用户可见文案里提到回环地址（安全边界说明）是合理的，不该被误判。
func TestNoHardcodedPanelOrigin(t *testing.T) {
	srcDir := "src"
	if _, err := os.Stat(srcDir); err != nil {
		t.Skipf("前端源码目录不存在（%v），跳过", err)
	}

	// 赋值/参数形态下的绝对回环地址：= "http://127.0.0.1..."、
	// `target: "http://localhost..."`、`fetch("http://127.0.0.1...")` 等。
	assignRe := regexp.MustCompile(`(?:=|:|\(|,)\s*["'` + "`" + `]https?://(?:127\.0\.0\.1|localhost)[:/]`)

	err := filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".ts" && ext != ".tsx" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(data), "\n") {
			// 跳过整行注释：说明文字里提到回环地址不算请求基址。
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if loc := assignRe.FindString(line); loc != "" {
				t.Errorf("%s:%d 把绝对地址赋给了请求基址（%q），面板必须用同源相对路径：\n\t%s",
					path, i+1, loc, trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历前端源码失败: %v", err)
	}
}
