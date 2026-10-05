package server

import (
	"encoding/json"
	"io"
	"net/http"

	"workbuddy-gateway/internal/autostart"
)

// handleAutostartRoute 按方法分发：GET 查状态，POST 切换开关。
//
// # 为什么自启不走 config.SavePatch
//
// 自启项不在 `config.json` 里，而是系统里的注册表 / plist / .desktop。
// 塞进配置热更新链会带来两个问题：一是它不属于网关的配置（换一台机器
// 拷 config.json 过去，自启项并不会跟着来），二是 `hotFields` 的语义是
// 「改完立即生效」，而自启改的是**下次登录**的行为。所以单独一个端点更诚实。
func (s *Server) handleAutostartRoute(w http.ResponseWriter, r *http.Request) {
	// 防止 nil 屏蔽：包级函数不依赖 Server 状态，但是方法式挂载便于与其它路由一致。
	_ = s
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, autostart.Query())
	case http.MethodPost:
		s.toggleAutostart(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET / POST"))
	}
}

// toggleAutostart 处理 `{"enabled": true|false}`。
func (s *Server) toggleAutostart(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("读取请求体失败："+err.Error()))
		return
	}
	_ = r.Body.Close()

	var payload struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败："+err.Error()))
		return
	}
	// 必须显式传 enabled。用指针判空而不是默认 false：默认 false 的话，
	// 一个拼错的字段名会静默变成「关闭自启」，用户以为开了、实际被关了。
	if payload.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, errBody(`缺少 enabled 字段（应为 {"enabled": true} 或 {"enabled": false}）`))
		return
	}

	toggle := autostart.Enable
	if !*payload.Enabled {
		toggle = autostart.Disable
	}
	st, err := toggle()
	if err != nil {
		// 带上最新状态一起返回：面板据此渲染（例如冲突时把现有状态显示出来），
		// 不必再发一次 GET。
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  err.Error(),
			"status": st,
		})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
