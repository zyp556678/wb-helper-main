package server

import (
	"net/http"
	"os"
	"strings"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/localapps"
	"workbuddy-gateway/internal/pool"
)

// -----------------------------------------------------------------------------
// 本机应用接入（切片 14）
//
// 把账号池里的账号写入本机某个应用的登录态。
//
// 为什么这件事归**网关**做而不是面板做：网关是本机进程，读写本机文件是它的
// 本来能力；面板虽然拿不到 Tauri IPC（壳把窗口导航到了 http://127.0.0.1:8317，
// 页面已不在应用源内），但它只要能调 /panel/api/* 就够了。
// -----------------------------------------------------------------------------

// poolSnapshotForLocalApps 把账号池转成本机探测需要的视图。
//
// 只取必要字段：探测只需要 uid / 昵称 / 站点 / token，
// 把整个 AccountState 传进去会让 localapps 包反向依赖 pool 的展示结构。
func poolSnapshotForLocalApps(accs []*pool.Account) []localapps.PoolAccount {
	out := make([]localapps.PoolAccount, 0, len(accs))
	for _, a := range accs {
		c := a.Cred
		out = append(out, localapps.PoolAccount{
			ID:           c.AccountID(),
			UID:          c.UID,
			Nickname:     c.Nickname,
			Site:         c.Site(),
			AccessToken:  c.AccessToken,
			RefreshToken: c.RefreshToken,
		})
	}
	return out
}

// handleLocalApps 探测本机各目标的安装情况与当前登录账号（GET /panel/api/local-apps）。
func (s *Server) handleLocalApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}

	snapshot := poolSnapshotForLocalApps(s.pool.Accounts())
	targets := localapps.Probe(snapshot)

	// 探测结果与契约的一致性自检：Blockers 非空却不允许写、或反之，都是 bug。
	// 放在这里而不是只写注释 —— 这类不一致会直接让前端显示错的状态。
	for i := range targets {
		t := &targets[i]
		if len(t.Blockers) > 0 && t.Writable {
			t.Writable = false
		}
	}

	payload := map[string]any{
		"targets": targets,
		// 与 wb-switch 的共存冲突：两个工具写同一批文件，必须主动告知。
		"switch_conflict": localapps.DetectSwitchConflict(),
	}

	// CLI 的详细配置单独给一份：探测项里只放「当前是哪个账号」，
	// 而设置页要看的是「档位标记、base url、我们没动过的其他键」。
	if home := userHome(); home != "" {
		if st, err := localapps.ReadCLIStatus(home); err == nil {
			payload["cli"] = st
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleLocalAppsSwitch 把某个账号写入某个本机目标（POST /panel/api/local-apps/switch）。
func (s *Server) handleLocalAppsSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Target string `json:"target"`
		// AccountID 是凭据文件名（与账号池的 id 同源）。
		AccountID string `json:"account_id"`
		// Site 只对 WorkBuddy 客户端有意义（国内站/国际站是两个不同的文件）。
		// 留空时按账号自身的站点推断。
		Site string `json:"site"`
		// AllowPlaintext 是「允许写入明文凭据」的显式确认。
		AllowPlaintext bool `json:"allow_plaintext"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}

	acc, err := s.pool.Find(strings.TrimSpace(req.AccountID))
	if err != nil {
		writeJSON(w, http.StatusNotFound, errBody("账号不存在: "+req.AccountID))
		return
	}
	target := localapps.TargetID(strings.TrimSpace(req.Target))
	pa := poolSnapshotForLocalApps([]*pool.Account{acc})[0]

	home := userHome()
	if home == "" {
		writeJSON(w, http.StatusInternalServerError, errBody("无法确定用户主目录，已放弃操作"))
		return
	}

	var (
		notes []string
		sErr  error
	)
	switch target {
	case localapps.TargetCLI:
		notes, sErr = localapps.SwitchCLI(home, pa)

	case localapps.TargetWorkBuddy:
		site := strings.TrimSpace(req.Site)
		if site == "" {
			site = pa.Site
		}
		if site != "cn" && site != "intl" {
			writeJSON(w, http.StatusBadRequest, errBody(
				"无法确定要写哪个档位（账号站点为 "+pa.Site+"）。WorkBuddy 的国内站与国际站是两个独立文件，必须明确指定"))
			return
		}
		notes, sErr = localapps.SwitchWorkBuddy(site, pa, req.AllowPlaintext)

	case localapps.TargetJetBrains:
		var jb *localapps.SwitchJetBrainsResult
		jb, sErr = localapps.SwitchJetBrains(pa)
		if jb != nil {
			notes = jb.Notes
		}

	default:
		// 明确区分「未实现」与「失败」：返回 501 而不是 400/500。
		writeJSON(w, http.StatusNotImplemented, errBody(
			"尚未实现该目标的写入："+target.Label()+"。可用 /panel/api/local-apps 查看各目标的可写状态"))
		return
	}

	if sErr != nil {
		// 失败时把已经做到的步骤（通常是「已备份」）一并回传 ——
		// 用户据此知道「虽然没切成，但备份在，能恢复」。
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"code": 502, "message": sErr.Error()},
			"notes": notes,
		})
		return
	}

	s.logf("[本机] 已把账号 %s 写入 %s（%s）", pa.ID, target.Label(), auth.SiteLabel(pa.Site))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notes": notes})
}

// handleLocalAppsBackups 列出本机应用登录态的备份（GET /panel/api/local-apps/backups）。
func (s *Server) handleLocalAppsBackups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	list, err := localapps.ListBackups(50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("读取备份列表失败: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": list})
}

// handleLocalAppsRestore 用备份恢复（POST /panel/api/local-apps/restore）。
func (s *Server) handleLocalAppsRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		BackupID string `json:"backup_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	if strings.TrimSpace(req.BackupID) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少 backup_id"))
		return
	}
	notes, err := localapps.RestoreBackup(strings.TrimSpace(req.BackupID))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	s.logf("[本机] 已用备份 %s 恢复本机登录态（%d 个文件）", req.BackupID, len(notes))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notes": notes})
}

// userHome 取用户主目录（取不到时返回空串，调用方据此拒绝操作而不是猜一个路径）。
func userHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}
