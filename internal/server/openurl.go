package server

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// -----------------------------------------------------------------------------
// 用系统默认浏览器打开链接（POST /panel/api/open-url）
//
// 为什么需要它：面板经常跑在**桌面壳的 WebView 里**（Tauri 壳把窗口导航到
// http://127.0.0.1:8317/panel/，且刻意没有 IPC）。WebView 里 `window.open` 与
// `target="_blank"` 会被直接拦掉且**不报错** —— 表现就是「点链接没反应」。
// 而 OAuth 授权链接（尤其国际站，官方流程本来就要在浏览器里完成）恰恰是必须
// 打开的那一类，点不开就等于这条路走不通。
//
// 安全边界（两条都要）：
//  1. 只接受 http/https 且带主机名 —— 其它协议（file:// 等）一律拒绝；
//  2. **只对来自回环地址的请求生效**：网关可能被部署在远端、面板在浏览器里远程访问，
//     那种情况下「打开浏览器」应该发生在用户自己的机器上，绝不能去开服务器上的浏览器。
//     非回环请求返回 403，前端据此提示「复制链接手动打开」。
// -----------------------------------------------------------------------------

// openInSystemBrowser 用系统默认浏览器打开链接。
//
// 做成包级变量：单测替换后即可断言「校验通过的链接才交给系统」，
// 而不会在跑测试的机器上真的弹出浏览器窗口。
var openInSystemBrowser = openInSystemBrowserOS

// handlePanelOpenURL 在本机用默认浏览器打开一个链接。
func (s *Server) handlePanelOpenURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	if !isLoopbackRequest(r) {
		// 远程访问面板时不能替用户开浏览器 —— 那台机器是服务器，不是用户桌面。
		writeJSON(w, http.StatusForbidden, errBody(
			"该请求不是来自本机，网关不会在服务器上打开浏览器。请复制链接后在自己的浏览器中打开"))
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	target := strings.TrimSpace(req.URL)
	parsed, err := url.Parse(target)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("链接无法解析: "+err.Error()))
		return
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeJSON(w, http.StatusBadRequest, errBody(
			"只允许打开 http/https 链接，收到的是 "+parsed.Scheme))
		return
	}
	if err := openInSystemBrowser(parsed.String()); err != nil {
		writeJSON(w, http.StatusBadGateway, errBody("打开浏览器失败: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// isLoopbackRequest 判断请求是否来自本机（含 IPv6 回环）。
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}
