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

// TestBootStyleDoesNotSetBodyColor 守住「首屏兜底样式不碰文字颜色」这条不变量。
//
// 为什么值得用测试钉住：这段 <style> 是**未分层**的，而 Tailwind 给 body 设的
// text-foreground 在 @layer base 里 —— 按 CSS 层叠规则未分层永远压过分层。
// 所以内联样式里的一句 color 会**永久**盖掉面板的文字颜色令牌。
//
// 症状很有欺骗性：面板主题是用户选的（可手动切），内联样式看的却是
// prefers-color-scheme（只有系统）。二者相反时（系统深色 + 面板浅色），
// 只有「继承 body 颜色」的元素受影响 —— 页面主标题、outline 按钮 ——
// 而对白底几乎不可见；副标题、卡片这些显式用令牌的元素一切正常。
// 表现为「每一页只有主标题淡了」，排查时很难联想到首屏兜底样式。
// 真踩过一次（系统 prefer-dark + 面板浅色）。
//
// background 不受此限（面板各页都自己画背景），它正是防首屏闪白要保留的东西。
func TestBootStyleDoesNotSetBodyColor(t *testing.T) {
	const path = "index.html"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("找不到 %s（%v），跳过", path, err)
	}
	src := string(data)

	// 只检查 <style> 块内部：其它地方出现 color 字样（属性、文案）不算。
	styleBlock := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(src)
	if styleBlock == nil {
		t.Fatalf("%s 里没有 <style> 块：首屏兜底样式不见了？", path)
	}

	// 抓「body { ... color: ... }」形态。允许 color 出现在 .wb-boot 这类
	// 局部选择器里 —— 那是有意给首屏占位文字指定的颜色，不会影响挂载后的令牌。
	bodyRuleRe := regexp.MustCompile(`(?s)body\s*\{([^}]*)\}`)
	colorRe := regexp.MustCompile(`(?m)^\s*color\s*:`)
	for _, m := range bodyRuleRe.FindAllStringSubmatch(styleBlock[1], -1) {
		if loc := colorRe.FindString(m[1]); loc != "" {
			t.Errorf("首屏兜底样式给 body 设了 %q，它会永久盖掉面板的文字颜色令牌"+
				"（未分层的样式压过 @layer base）：\n"+
				"  请删掉这行，文字颜色交给令牌；\n"+
				"  首屏占位文字的颜色由 .wb-boot 自己指定即可。",
				strings.TrimSpace(loc))
		}
	}
}
