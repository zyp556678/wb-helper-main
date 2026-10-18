package server

import (
	"context"
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
//	POST /panel/api/update/download  把匹配本机平台的安装包下到数据目录
//	POST /panel/api/update/install   把已下好的安装包装到本机（可能要提权、会重启网关）
//
// 面板上只有「检查更新」与「更新」两个动作。没有「自动下载 / 自动安装」这类开关：
// 检测是后台自动的（代价只有一次只读 GET），而「装不装」就是用户点不点那个按钮 ——
// 把它做成一个需要预先想清楚的配置项，只是把犹豫额外塞给了用户。
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

// installUpdate 把「已经下好的」安装包装到本机。
//
// 端点**不接受客户端传路径**：要装哪个文件由 internal/update 从当前检查结果推导
// （匹配本机平台的资产名，且在下载目录里、大小与 Release 一致），因此不存在
// 「面板让网关执行任意文件」的面。
//
// 这是「点一下更新」这个动作的后半段：面板先把安装包下好（downloadUpdate），
// 下完立刻调这里。所以它仍然是**用户点出来的**动作，只是不再需要第二次点击 ——
// 安装本身不需要任何选择（见 install.go）。
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

// downloadUpdate 下载匹配本机平台的安装包。这是「点一下更新」的前半段，
// 面板在它成功返回后立刻调用 installUpdate（所以用户只需要点一次）。
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
	detail := "安装包已下载：" + res.Path
	if res.Skipped {
		detail = "安装包已存在：" + res.Path
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
