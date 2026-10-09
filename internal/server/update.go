package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"workbuddy-gateway/internal/eventlog"
)

// handlePanelUpdateRoute 按方法分发：GET 读状态（必要时补一次检查），POST 立即检查。
//
// # 为什么「检查更新」要单独一组端点，而不并进 /panel/api/config
//
// 配置端点的语义是「读写偏好」，而这里要的是「现在就去做一件事并把结果给我」。
// 两者混在一起会出现一个说不清的状态：面板 POST 一个 config 补丁，到底是保存了
// 一个开关，还是触发了一次外网请求？所以分成两条：
//
//	GET  /panel/api/update           读状态（不阻塞；没查过时后台补一次）
//	POST /panel/api/update           立即检查（force，用户显式点的那一下）
//	POST /panel/api/update/auto      开关「自动检查」，写进 config.json 的 update.enabled
//	POST /panel/api/update/download  把匹配本机平台的安装包下到数据目录
func (s *Server) handlePanelUpdateRoute(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		// 没注入检查器（例如精简装配的实例）：如实说「未启用」，
		// 而不是返回一个「已是最新」的假状态 —— 后者会让用户以为查过了。
		writeJSON(w, http.StatusServiceUnavailable, errBody("本实例未启用更新检查"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.updateState(true))
	case http.MethodPost:
		s.runUpdateCheck(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET / POST"))
	}
}

// updateState 返回当前状态；kick=true 且从未查过时，在后台补一次检查。
//
// 为什么要「补一次」：网关重启后内存里的结果就没了，面板打开时若不主动发起，
// 用户会看到一张「尚未检查」的卡片，得自己再点一下 —— 而自动检查本来就是开着的。
// 补的这一次走 force=false，因此仍受 minGap 约束，刷新页面不会变成刷 GitHub。
func (s *Server) updateState(kick bool) any {
	st := s.updates.State()
	if kick && st.CheckedAt == 0 && st.Error == "" && !st.Checking {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.updates.Check(ctx, false)
		}()
		st.Checking = true
	}
	return st
}

// runUpdateCheck 立即检查一次并把结果返回给面板。
func (s *Server) runUpdateCheck(w http.ResponseWriter, r *http.Request) {
	// 用户显式点击 → force=true（不受 minGap 约束）。
	// 超时给 25 秒：GitHub 正常在 1 秒内返回，25 秒足够覆盖慢网络与一次重试，
	// 又不至于让面板转圈转到用户以为卡死。
	ctx, cancel := contextWithTimeout(r, 25*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.updates.Check(ctx, true))
}

// setUpdateAutoCheck 处理 `{"enabled": true|false}`：把自动检查开关写进 config.json。
//
// 复用配置层的 SavePatch（而不是自己写文件）：它同时负责原子替换、保留用户手写的
// 未知键、以及把新配置推给运行期组件 —— 自己写一份迟早会漏掉其中一环。
func (s *Server) setUpdateAutoCheck(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("本实例未启用更新检查"))
		return
	}
	var payload struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeBody(r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败："+err.Error()))
		return
	}
	// 必须显式传 enabled。用指针判空而不是默认 false：默认 false 的话，
	// 一个拼错的字段名会静默变成「关掉自动检查」，用户以为开着、实际关了。
	if payload.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, errBody(`缺少 enabled 字段（应为 {"enabled": true} 或 {"enabled": false}）`))
		return
	}

	patch, err := json.Marshal(map[string]any{
		"update": map[string]any{"enabled": *payload.Enabled},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("构造配置补丁失败："+err.Error()))
		return
	}
	// 写时复制，与 /panel/api/config 同款：先克隆，成功再整体换指针。
	next := s.config().Clone()
	if _, _, err := next.SavePatch(patch); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("保存自动检查开关失败："+err.Error()))
		return
	}
	s.cfgPtr.Store(next)
	s.applyRuntimeConfig()

	// 刚打开开关就顺手查一次：否则用户要等到下一个周期（默认 6 小时）才有结果，
	// 会以为开关没生效。这是后台进行，不阻塞本次响应。
	if *payload.Enabled {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.updates.Check(ctx, true)
		}()
	}
	writeJSON(w, http.StatusOK, s.updateState(false))
}

// downloadUpdate 下载匹配本机平台的安装包。
//
// 端点**不接受客户端传 URL**：只下载检查结果里记着的那个地址，因此这里不存在
// 「面板让网关去下载任意地址」的 SSRF 面。
func (s *Server) downloadUpdate(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("本实例未启用更新检查"))
		return
	}
	// 安装包几十 MB，给足时间；上限 10 分钟是为了不让一个卡死的连接永久占着。
	ctx, cancel := contextWithTimeout(r, 10*time.Minute)
	defer cancel()

	res, err := s.updates.Download(ctx)
	if err != nil {
		// 动作型端点：失败走 {"ok":false,"detail":...}（前端 actionRequest 按它解析）。
		writeActionError(w, err)
		return
	}
	detail := "安装包已下载到 " + res.Path + "，双击安装即可完成升级"
	if res.Skipped {
		detail = "安装包已存在，无需重复下载：" + res.Path
	}
	s.events.Info(eventlog.ChannelSystem, "update_downloaded", "已下载新版本安装包 "+res.Name,
		map[string]any{"version": res.Version, "path": res.Path, "bytes": res.Bytes})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"detail":  detail,
		"path":    res.Path,
		"name":    res.Name,
		"dir":     res.Dir,
		"bytes":   res.Bytes,
		"version": res.Version,
		"skipped": res.Skipped,
	})
}
