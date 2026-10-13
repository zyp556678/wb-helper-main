package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"workbuddy-gateway/internal/eventlog"
	"workbuddy-gateway/internal/update"
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
//	POST /panel/api/update/auto      开关「自动检查」/「自动下载」，写进 config.json 的 update 段
//	POST /panel/api/update/download  把匹配本机平台的安装包下到数据目录
//	POST /panel/api/update/install   把已下好的安装包装到本机（可能要提权、会重启网关）
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

// setUpdateAutoCheck 处理 `{"enabled": bool}` / `{"auto_download": bool}`：
// 把「自动检查」与「自动下载」两个开关写进 config.json 的 update 段。
//
// 两个开关共用一条端点，是因为它们的读写链路完全一样（都是 update 段的布尔字段），
// 分成两条会多出一份几乎相同的错误处理。传哪个改哪个，两个都可以传。
//
// 复用配置层的 SavePatch（而不是自己写文件）：它同时负责原子替换、保留用户手写的
// 未知键、以及把新配置推给运行期组件 —— 自己写一份迟早会漏掉其中一环。
func (s *Server) setUpdateAutoCheck(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("本实例未启用更新检查"))
		return
	}
	var payload struct {
		Enabled      *bool `json:"enabled"`
		AutoDownload *bool `json:"auto_download"`
		AutoInstall  *bool `json:"auto_install"`
	}
	if err := decodeBody(r, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败："+err.Error()))
		return
	}
	// 必须显式传至少一个字段。用指针判空而不是默认 false：默认 false 的话，
	// 一个拼错的字段名会静默变成「关掉自动检查」，用户以为开着、实际关了。
	if payload.Enabled == nil && payload.AutoDownload == nil && payload.AutoInstall == nil {
		writeJSON(w, http.StatusBadRequest,
			errBody(`缺少字段（应为 {"enabled": true} / {"auto_download": false} / {"auto_install": true}）`))
		return
	}

	updates := map[string]any{}
	if payload.Enabled != nil {
		updates["enabled"] = *payload.Enabled
	}
	if payload.AutoDownload != nil {
		updates["auto_download"] = *payload.AutoDownload
	}
	if payload.AutoInstall != nil {
		updates["auto_install"] = *payload.AutoInstall
	}
	patch, err := json.Marshal(map[string]any{"update": updates})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("构造配置补丁失败："+err.Error()))
		return
	}
	// 写时复制，与 /panel/api/config 同款：先克隆，成功再整体换指针。
	next := s.config().Clone()
	if _, _, err := next.SavePatch(patch); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("保存更新设置失败："+err.Error()))
		return
	}
	s.storeConfig(next)

	// 刚打开「自动检查」就顺手查一次：否则用户要等到下一个周期（默认 6 小时）才有结果，
	// 会以为开关没生效。这是后台进行，不阻塞本次响应。
	if payload.Enabled != nil && *payload.Enabled {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.updates.Check(ctx, true)
		}()
	}
	// 刚打开「自动下载」而本地还缺包时，立刻补一次下载：否则要等到下一次检查
	//（可能 6 小时后）才会下，用户会以为开关没生效。同样是后台进行。
	if payload.AutoDownload != nil && *payload.AutoDownload {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.updates.Check(ctx, false)
		}()
	}
	writeJSON(w, http.StatusOK, s.updateState(false))
}

// setUpdateMirror 处理 `{"mirror": "https://gh-proxy.org" | ""}`：下载加速前缀写进 config.json。
//
// 单独一条端点而不是并进 /update/auto：auto 那条全是布尔开关（按钮点击语义），
// 这里是**文本输入 + 保存**语义，失败提示也不一样（前缀格式错要说明怎么改）。
// 用 SavePatch 走同一条配置链路（原子替换、保留未知键、热生效）。
func (s *Server) setUpdateMirror(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("本实例未启用更新检查"))
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var payload struct {
		Mirror *string `json:"mirror"`
	}
	if err := decodeBody(r, &payload); err != nil || payload.Mirror == nil {
		writeJSON(w, http.StatusBadRequest,
			errBody(`缺少 mirror 字段（应为 {"mirror": "https://gh-proxy.org"}，空串表示直连）`))
		return
	}

	// 先按**提交值**校验形态（空串合法 = 直连）。不合法直接 400，让用户当场改，
	// 而不是存进去之后每次下载都悄悄退回直连。
	value := strings.TrimSpace(*payload.Mirror)
	if value != "" {
		if _, err := update.ApplyDownloadMirror(value, "https://github.com/o/r/releases/download/v1/x.deb"); err != nil {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
	}
	patch, err := json.Marshal(map[string]any{"update": map[string]any{"download_mirror": value}})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("构造配置补丁失败："+err.Error()))
		return
	}
	next := s.config().Clone()
	if _, _, err := next.SavePatch(patch); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("保存下载加速设置失败："+err.Error()))
		return
	}
	s.storeConfig(next)
	detail := "已改为直连 github.com"
	if value != "" {
		detail = "已启用下载加速：" + value
	}
	s.logf("[更新] %s", detail)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"mirror": value,
		"detail": detail,
	})
}

// setUpdateProxy 处理 `{"proxy": "http://127.0.0.1:7897" | ""}`：更新代理写进 config.json。
//
// 与 setUpdateMirror 是两条正交通路：mirror 服务「没代理、靠第三方加速站转发」的用户，
// proxy 服务「本机有 Clash / v2ray」的用户。两者可同时配置，下载时**代理优先**
// （见 internal/update 的 Download：代理生效就不套加速前缀）。
//
// 单独一条端点而不是并进 /update/auto：那条全是布尔开关（按钮点击语义），
// 这里是**文本输入 + 保存**语义，且失败提示要说明正确写法。
func (s *Server) setUpdateProxy(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("本实例未启用更新检查"))
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var payload struct {
		Proxy *string `json:"proxy"`
	}
	if err := decodeBody(r, &payload); err != nil || payload.Proxy == nil {
		writeJSON(w, http.StatusBadRequest,
			errBody(`缺少 proxy 字段（应为 {"proxy": "http://127.0.0.1:7897"}，空串表示不走代理）`))
		return
	}

	// 先按**提交值**校验形态（空串合法 = 不走代理）。不合法直接 400，让用户当场改，
	// 而不是存进去之后每次下载都悄悄回落 —— 那会让「我明明填了代理却还是超时」很难排查。
	value := strings.TrimSpace(*payload.Proxy)
	if err := update.ValidateUpdateProxy(value); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	patch, err := json.Marshal(map[string]any{"update": map[string]any{"proxy": value}})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("构造配置补丁失败："+err.Error()))
		return
	}
	next := s.config().Clone()
	if _, _, err := next.SavePatch(patch); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("保存更新代理失败："+err.Error()))
		return
	}
	s.storeConfig(next)
	detail := "已关闭更新代理（直连 GitHub）"
	if value != "" {
		detail = "已启用更新代理：" + value
	}
	s.logf("[更新] %s", detail)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"proxy":  value,
		"detail": detail,
	})
}

// installUpdate 把「已经下好的」安装包装到本机。
//
// 端点**不接受客户端传路径**：要装哪个文件由 internal/update 从当前检查结果推导
// （匹配本机平台的资产名，且在下载目录里、大小与 Release 一致），因此不存在
// 「面板让网关执行任意文件」的面。
//
// 为什么这一步要人点一下：安装会覆盖已装好的程序、结束正在跑的进程、必要时提权，
// 失败时可能留下一个既起不来也没有回退版本的现场。检测与下载是只读的，可以自动。
func (s *Server) installUpdate(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("本实例未启用更新检查"))
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	// 提权框可能要等用户按指纹/输密码，给足 10 分钟；dpkg/ditto 本身都在秒级。
	//
	// 刻意**不**挂请求的 ctx：安装一旦开始就不该因为浏览器关了、网络断了而半途而废 ——
	// 一个「装到一半」的 dpkg 比安装失败难收拾得多。下载可以随请求取消（幂等可重来），
	// 安装不行。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// 前提不满足（还没下载、没有新版本、平台没产物）与「安装真的失败了」要分开：
	// 前者是 409 + 一句可操作的话，后者才是 502。
	if err := s.updates.Ready(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "detail": err.Error()})
		return
	}

	res, err := s.updates.Install(ctx)
	if err != nil {
		s.events.Warn(eventlog.ChannelSystem, "update_install_failed", "安装新版本失败："+err.Error(), nil)
		writeActionError(w, err)
		return
	}
	s.events.Info(eventlog.ChannelSystem, "update_installed", "新版本安装完成", map[string]any{
		"restart": res.Restart,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      res.OK,
		"detail":  res.Detail,
		"restart": res.Restart,
		"exit":    res.Exit,
		"output":  res.Output,
		// 装完把最新状态一并回给面板：重启/退出途中面板还能用这份数据渲染提示。
		"state": s.updateState(false),
	})

	// 必须**先**把响应写回面板，再请求重启/退出：那会立刻掐断这条连接，
	// 紧接着发生的话面板只会看到「请求失败」，用户会以为安装失败并重复点击。
	switch {
	case res.Restart:
		s.requestRestart(restartDelayAfterInstall)
	case res.Exit:
		// Windows：退出以让出文件锁（详见 installWindows 的说明）。
		s.requestExit(restartDelayAfterInstall)
	}
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
