package server

import (
	"net/http"
	"time"

	"workbuddy-gateway/internal/eventlog"
)

// handleLocalCapabilities 返回本机代理的能力与运行状态。
//
// 前端用它决定「本机客户端」页是显示还是隐藏：服务端部署下 local=false 是正常形态，
// 不是故障——所以这里返回 200 并把原因写在 state/last_error 里，而不是返回 4xx，
// 否则前端只能把 4xx 当错误提示，反而制造噪音。
func (s *Server) handleLocalCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	if s.local == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":   false,
			"state":     "disabled",
			"local":     false,
			"available": false,
			"reason":    "本机代理未接入（当前构建未启用）",
		})
		return
	}
	caps := s.local.Capabilities()
	caps["proxy_prefix"] = ProxyPathLocal
	// 把「为什么不可用」翻译成一句人话，省得用户对着一堆 state 猜
	if reason := localReason(caps); reason != "" {
		caps["reason"] = reason
	}
	writeJSON(w, http.StatusOK, caps)
}

// localReason 把状态翻译成可读原因（可用时返回空串）。
func localReason(caps map[string]any) string {
	state, _ := caps["state"].(string)
	switch state {
	case "running":
		return ""
	case "disabled":
		return "本机代理已在配置中关闭（config.local.enabled=false）"
	case "missing":
		return "未找到本机代理二进制：把它放到网关同目录即可启用本机相关功能"
	case "starting":
		return "本机代理正在启动"
	case "failed":
		if e, ok := caps["last_error"].(string); ok && e != "" {
			return "本机代理启动失败：" + e
		}
		return "本机代理启动失败"
	default:
		return "本机代理已停止（等待重启或已超出重启上限）"
	}
}

// handleLocalRestart 手动重启本机代理。
func (s *Server) handleLocalRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	if s.local == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("本机代理未接入"))
		return
	}
	ctx, cancel := contextWithTimeout(r, 20*time.Second)
	defer cancel()

	if err := s.local.Restart(ctx); err != nil {
		if s.events != nil {
			s.events.Warn(eventlog.ChannelSystem, "local_agent_restart_failed",
				"手动重启本机代理失败", map[string]any{"error": err.Error()})
		}
		writeActionError(w, err)
		return
	}
	s.logf("[本机代理] 面板手动重启成功")
	if s.events != nil {
		s.events.Info(eventlog.ChannelSystem, "local_agent_restarted", "面板手动重启了本机代理", nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": "本机代理已重启"})
}
