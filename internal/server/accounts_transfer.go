package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"workbuddy-gateway/internal/auth"
)

// -----------------------------------------------------------------------------
// 账号导入 / 导出（切片 12）
//
// 导出的是**可迁移的备份**（含 access_token / refresh_token），不是面板视图。
// 因此字段名刻意与面板的 AccountState 分开：面板视图里带一堆治理状态
// （冷却、熔断、连败），那些是**本机运行时状态**，导到另一台机器毫无意义，
// 反而会让人误以为账号带着「冷却」一起搬家。
//
// 字段命名对齐 wb-switch 的导出（token 用 snake_case、expiresAt 用 camelCase），
// 使两边的备份可以互相导入 —— 这一点是刻意的：用户换工具时不该被格式卡住。
// -----------------------------------------------------------------------------

// exportAccount 是导出文件里的一条账号记录。
//
// 刻意**不含** email：本项目的凭据文件里没有该字段（详见 ImportAccount 的注释），
// 写一个恒为空串的字段只会让人以为导出丢了数据。
type exportAccount struct {
	ID           string `json:"id"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	Site         string `json:"site"`
	Domain       string `json:"domain"`
	EnterpriseID string `json:"enterprise_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expiresAt"`
	Edition      string `json:"edition,omitempty"`
	Realm        string `json:"realm,omitempty"`
}

// exportDoc 是导出文件的顶层结构。
//
// 带 app / version 是为了**将来能给出「这个备份来自哪个版本」的判断依据**；
// 导入侧不依赖它们（缺了也能导），只在结果里回显。
type exportDoc struct {
	App        string          `json:"app"`
	Version    string          `json:"version"`
	ExportedAt int64           `json:"exported_at"`
	Count      int             `json:"count"`
	Accounts   []exportAccount `json:"accounts"`
}

// importPreviewItem 是导入预览里的一项。
//
// **不回传 token**：预览只需要够用户辨认「这是哪个账号」，把凭据再回传一遍
// 只是徒增暴露面。真正的写入靠前端把原始文件内容回传 + 勾选下标（见
// handleAccountsImport），token 全程不离开「原始文件 → 服务端」这条路径。
type importPreviewItem struct {
	Index int    `json:"index"`
	UID   string `json:"uid"`
	// Nickname/Email 供辨认；两者都空时前端回落到「第 N 项」。
	Nickname     string `json:"nickname"`
	Site         string `json:"site"`
	SiteLabel    string `json:"site_label"`
	Domain       string `json:"domain"`
	EnterpriseID string `json:"enterprise_id"`
	HasToken     bool   `json:"has_token"`
	ExpiresAt    int64  `json:"expires_at"`
	// Encrypted 表示这一项的 access_token 是 WorkBuddy 5.6 的**加密信封**
	//（`{"$wbEncrypted": ...}`）：凭据存在，但我们没有解密密钥，无法导入，
	// 需要 wb-switch（或重新登录）才能使用。前端据此显示「加密凭据」徽章。
	Encrypted bool `json:"encrypted"`
	// Conflict 表示池里已有同 uid（或该 uid 的目标文件已存在）的账号。
	// 有冲突不代表不能导，但**必须让用户在勾选前就知道**——
	// 静默覆盖别人的凭据是不可接受的。
	Conflict     bool   `json:"conflict"`
	ConflictWith string `json:"conflict_with"`
	// Reason 非空表示这一项不可导入（缺 token / 站点无法判定等）。
	Reason string `json:"reason"`
}

// handleAccountsExport 导出账号为 JSON（GET /panel/api/accounts/export）。
//
// 支持 ?ids=a,b,c 只导出勾选的账号；不传则导出全部。
// 前端拿到后自行触发下载（Blob），服务端不碰文件系统 —— 服务端落盘要处理
// 「用户选目录」这类跨平台问题，而浏览器下载已经解决了。
func (s *Server) handleAccountsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}

	want := map[string]bool{}
	if raw := strings.TrimSpace(r.URL.Query().Get("ids")); raw != "" {
		for _, id := range strings.Split(raw, ",") {
			if id = strings.TrimSpace(id); id != "" {
				want[id] = true
			}
		}
	}

	accs := s.pool.Accounts()
	out := make([]exportAccount, 0, len(accs))
	for _, a := range accs {
		id := a.Cred.AccountID()
		if len(want) > 0 && !want[id] {
			continue
		}
		c := a.Cred
		out = append(out, exportAccount{
			ID:           id,
			UID:          c.UID,
			Nickname:     c.Nickname,
			Site:         c.Site(),
			Domain:       c.Domain,
			EnterpriseID: c.EnterpriseID,
			AccessToken:  c.AccessToken,
			RefreshToken: c.RefreshToken,
			ExpiresAt:    c.ExpiresAt,
			Edition:      c.Edition,
			Realm:        c.Realm,
		})
	}

	s.logf("[账号] 导出 %d 个账号（含登录 token，文件等同密码）", len(out))
	writeJSON(w, http.StatusOK, exportDoc{
		App:        "workbuddy-gateway",
		Version:    Version,
		ExportedAt: time.Now().Unix(),
		Count:      len(out),
		Accounts:   out,
	})
}

// handleAccountsImportPreview 解析上传的备份文件并返回可勾选的预览
// （POST /panel/api/accounts/import/preview，body {"raw": "<文件内容>"}）。
//
// 与 handleAccountsImport 用同一个 {raw} 形状，而不是直接收原始文件内容：
// 「原始内容直传」看起来省一层转义，但会让「这个 body 是备份文件本身，
// 还是包了一层的信封」变成一个需要猜的问题 —— 猜错的表现是用户上传
// 一个含 raw 字段的备份时被当成信封解析。统一成信封就没有这个歧义。
func (s *Server) handleAccountsImportPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		Raw string `json:"raw"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	if strings.TrimSpace(req.Raw) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少 raw（备份文件内容）"))
		return
	}
	if int64(len(req.Raw)) > maxImportBytes {
		writeJSON(w, http.StatusBadRequest, errBody(fmt.Sprintf("备份文件过大（超过 %d KB）", maxImportBytes>>10)))
		return
	}
	raw := []byte(req.Raw)

	items, err := parseImportFile(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}

	// 已有的 uid → 凭据文件名，用于冲突提示。
	existing := map[string]string{}
	for _, a := range s.pool.Accounts() {
		if uid := strings.TrimSpace(a.Cred.UID); uid != "" {
			existing[uid] = a.Cred.AccountID()
		}
	}

	preview := make([]importPreviewItem, 0, len(items))
	for i, it := range items {
		acc := importPreviewItem{
			Index:        i,
			UID:          it.uid,
			Nickname:     it.nickname,
			Site:         it.site,
			SiteLabel:    auth.SiteLabel(it.site),
			Domain:       it.domain,
			EnterpriseID: it.enterpriseID,
			HasToken:     it.accessToken != "" || it.encrypted,
			ExpiresAt:    it.expiresAt,
			Encrypted:    it.encrypted,
		}
		if it.parseErr != "" {
			acc.Reason = it.parseErr
		}
		if acc.UID != "" {
			if file, ok := existing[acc.UID]; ok {
				acc.Conflict = true
				acc.ConflictWith = file
			} else if _, err := os.Stat(s.pool.CredentialPathFor(acc.UID)); err == nil {
				acc.Conflict = true
				acc.ConflictWith = baseName(s.pool.CredentialPathFor(acc.UID))
			}
		}
		preview = append(preview, acc)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": preview,
		"total":    len(preview),
	})
}

// handleAccountsImport 按勾选写入账号
// （POST /panel/api/accounts/import，body {raw, indexes, overwrite}）。
func (s *Server) handleAccountsImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	var req struct {
		// Raw 是备份文件的原始内容；服务端据此重新解析，而不是信任前端回传的
		// 记录 —— 前端回传整套凭据意味着 token 要在浏览器里再走一遍，
		// 而这份 token 是「等同密码」的东西。
		Raw string `json:"raw"`
		// Indexes 是勾选项在文件中出现的下标。
		Indexes []int `json:"indexes"`
		// Overwrite 为假时，与现有账号冲突的项**跳过**而不是覆盖。
		Overwrite bool `json:"overwrite"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("请求体解析失败: "+err.Error()))
		return
	}
	if strings.TrimSpace(req.Raw) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少 raw（备份文件内容）"))
		return
	}
	if len(req.Indexes) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody("没有勾选任何账号"))
		return
	}

	items, err := parseImportFile([]byte(req.Raw))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}

	existing := map[string]string{}
	for _, a := range s.pool.Accounts() {
		if uid := strings.TrimSpace(a.Cred.UID); uid != "" {
			existing[uid] = a.Cred.AccountID()
		}
	}

	var (
		imported    []string
		overwritten []string
		skipped     []string
		added       []*auth.Credential
	)
	for _, idx := range req.Indexes {
		if idx < 0 || idx >= len(items) {
			skipped = append(skipped, fmt.Sprintf("第 %d 项：下标越界", idx+1))
			continue
		}
		it := items[idx]
		if it.parseErr != "" {
			skipped = append(skipped, fmt.Sprintf("第 %d 项：%s", idx+1, it.parseErr))
			continue
		}
		if it.accessToken == "" {
			skipped = append(skipped, fmt.Sprintf("第 %d 项：缺少 access_token", idx+1))
			continue
		}
		if it.uid == "" {
			skipped = append(skipped, fmt.Sprintf("第 %d 项：缺少 uid（无法确定凭据文件名）", idx+1))
			continue
		}

		path := s.pool.CredentialPathFor(it.uid)
		_, fileExists := os.Stat(path)
		isConflict := fileExists == nil || existing[it.uid] != ""
		if isConflict && !req.Overwrite {
			skipped = append(skipped, fmt.Sprintf("第 %d 项：已存在同名账号，未勾选覆盖", idx+1))
			continue
		}

		cred := auth.NewForLogin(path, it.site, it.accessToken, it.refreshToken,
			it.expiresAt, it.domain, it.uid, it.enterpriseID, it.nickname)
		if err := cred.SaveFull(); err != nil {
			skipped = append(skipped, fmt.Sprintf("第 %d 项：写入失败 %s", idx+1, err.Error()))
			continue
		}
		added = append(added, cred)
		if isConflict {
			overwritten = append(overwritten, cred.AccountID())
		} else {
			imported = append(imported, cred.AccountID())
		}
	}

	// 全部写盘成功后再热加载。放到循环外是因为 Add 会重置该账号的运行状态
	// （冷却、失效标记），中途失败时不该把「已经写入的那部分」反复重置。
	for _, cred := range added {
		s.pool.Add(cred)
	}

	// 导入的国际站账号同样要完成注册地区与激活（否则对话报 14017），
	// 顺带领 trial 加油包。逐号进行且失败只记日志：导入本身已经成功了。
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()
	for _, cred := range added {
		s.ensureIntlRegistration(ctx, cred.AccountID())
	}

	s.logf("[账号] 导入完成：新增 %d，覆盖 %d，跳过 %d", len(imported), len(overwritten), len(skipped))
	writeJSON(w, http.StatusOK, map[string]any{
		"imported":    imported,
		"overwritten": overwritten,
		"skipped":     skipped,
		"ok":          true,
	})
}

// -----------------------------------------------------------------------------
// 单账号：刷新 Token / 签到
// -----------------------------------------------------------------------------

// handleAccountRefreshToken 强制刷新该账号的登录令牌
// （POST /panel/api/accounts/{id}/refresh-token）。
//
// 走 pool.ForceRefreshToken：它不判断「是否临近过期」，就是无条件刷一次。
// 这正是面板上这个按钮的语义 —— 用「还剩几天」推断用户想干什么，
// 只会让「我明明点了却没反应」变成一个偶发抱怨。
func (s *Server) handleAccountRefreshToken(w http.ResponseWriter, r *http.Request) {
	acc, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()

	if err := s.pool.ForceRefreshToken(ctx, acc); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"code": 502, "message": "刷新 Token 失败: " + err.Error()},
		})
		return
	}
	id := acc.Cred.AccountID()
	s.logf("[账号] 已刷新 Token：%s（%s）", id, acc.SiteLabel())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": s.pool.StateOf(acc)})
}

// handleAccountCheckin 手动为该账号签到
// （POST /panel/api/accounts/{id}/checkin）。
//
// 幂等语义直接沿用上游层：上游对「今天已签到」返回业务码 10001/14001，
// 被识别为成功（Already=true）。所以这里「已签到」和「签到成功」都是 200，
// 由 detail 区分 —— 把它报成失败会让用户以为按钮坏了。
func (s *Server) handleAccountCheckin(w http.ResponseWriter, r *http.Request) {
	acc, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	prof := acc.Profile()
	if !prof.SupportsCheckin() {
		writeJSON(w, http.StatusBadRequest, errBody(
			"该站点（"+acc.SiteLabel()+"）不提供每日签到，仅国内站支持"))
		return
	}

	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()

	res, err := s.client.Checkin(ctx, acc.View(), prof)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"code": 502, "message": "签到失败: " + err.Error()},
		})
		return
	}

	detail := "签到成功"
	if res.Already {
		detail = "今天已签到"
	} else if res.Message != "" {
		detail = "签到成功：" + res.Message
	}
	id := acc.Cred.AccountID()
	// 签到成功后立刻让状态缓存失效：不失效的话，用户刚点完「手动签到」，
	// 卡片还会显示「今日未签到」最长 5 分钟。
	s.checkinCache.invalidate(id)
	s.logf("[签到] %s（%s）→ %s", id, acc.SiteLabel(), detail)

	// 本地观察台账：签到事件也记一条（积分页「今天签到了几次」用它）。
	checkinResult := "success"
	if res.Already {
		checkinResult = "already"
	}
	s.creditWatch.RecordCheckin(id, acc.Cred.Nickname, acc.Site(), checkinResult, "")

	// 签到会改变余额，顺带刷新一次额度，让卡片上的数字与签到状态一致。
	_ = s.pool.RefreshQuota(ctx, acc)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"already": res.Already,
		"detail":  detail,
		"account": s.pool.StateOf(acc),
	})
}

// -----------------------------------------------------------------------------
// 导入解析
// -----------------------------------------------------------------------------

// maxImportBytes 限制上传备份的大小（512KB 足够装数千个账号）。
// 这个接口在读 body 时就要设上限 —— 不设的话一个超大 body 会把内存吃满。
const maxImportBytes = 512 << 10

// importItem 是一条待导入账号的归一化结果。
type importItem struct {
	uid          string
	nickname     string
	site         string
	domain       string
	enterpriseID string
	accessToken  string
	refreshToken string
	expiresAt    int64
	parseErr     string
	// encrypted 表示这份记录里的 token 只有加密信封形态（见 importTokenEnvelope）。
	encrypted bool
}

// parseImportFile 解析备份文件。
//
// 接受三种形态（都能导，用户不该被格式卡住）：
//  1. 本项目的导出文档：{"app":..,"accounts":[...]}
//  2. 裸数组：[{...}, {...}]
//  3. wb-switch 的导出（本身也是裸数组，字段名略有差异，见 parseImportItem）
func parseImportFile(raw []byte) ([]importItem, error) {
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err == nil && len(doc.Accounts) > 0 {
		return normalizeImport(doc.Accounts), nil
	}

	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return normalizeImport(arr), nil
	}

	// 兜底：单个对象（有人会只导出一个账号）。
	//
	// 但**必须先确认它真的像账号**：不加这道判断，任意一个 JSON 对象
	// （比如 {"foo":"bar"}）都会被当成「1 个缺 token 的账号」放进预览，
	// 用户看到的是「第 1 项：缺少 access_token」—— 这句话会把人引到
	// 「文件里的 token 丢了」，而真实情况是「这根本不是备份文件」。
	var one map[string]any
	if err := json.Unmarshal(raw, &one); err == nil && len(one) > 0 {
		if item := parseImportItem(one); item.accessToken != "" || item.uid != "" {
			return []importItem{item}, nil
		}
		return nil, fmt.Errorf("文件里没有可识别的账号：既没有 accounts 数组，也没有 access_token / uid 字段")
	}
	return nil, fmt.Errorf("无法解析备份文件：既不是账号数组，也不是含 accounts 字段的对象")
}

func normalizeImport(raw []map[string]any) []importItem {
	out := make([]importItem, 0, len(raw))
	for _, item := range raw {
		out = append(out, parseImportItem(item))
	}
	return out
}

// parseImportItem 从一条记录里取出凭据字段。
//
// 字段名做**宽容读取**：本项目用 snake_case，wb-switch 用 snake_case(token) +
// camelCase(expiresAt)，旧版本还可能整份塞在 auth_raw 里。三处都认，
// 代价只是几个 firstNonEmpty —— 而漏认一种的表现是「导入后没有 token」，
// 用户完全无从判断是文件坏了还是工具不认。
func parseImportItem(m map[string]any) importItem {
	nested := func(key string) map[string]any {
		if o, ok := m[key].(map[string]any); ok {
			return o
		}
		return nil
	}
	authObj := nested("auth")
	acctObj := nested("account")
	// wb-switch 的导出把完整凭据放在 auth_raw 里。
	rawObj := nested("auth_raw")
	if rawObj == nil {
		rawObj = nested("authRaw")
	}

	pick := func(keys ...string) string {
		for _, k := range keys {
			for _, src := range []map[string]any{m, authObj, acctObj, rawObj} {
				if src == nil {
					continue
				}
				if v, ok := src[k]; ok {
					if s := importText(v); s != "" {
						return s
					}
				}
			}
		}
		return ""
	}
	pickInt := func(keys ...string) int64 {
		for _, k := range keys {
			for _, src := range []map[string]any{m, authObj, acctObj, rawObj} {
				if src == nil {
					continue
				}
				if v, ok := src[k]; ok {
					if n, ok := importInt64(v); ok {
						return n
					}
				}
			}
		}
		return 0
	}

	it := importItem{
		uid:          pick("uid", "UID", "userId", "userId"),
		nickname:     pick("nickname", "nickName", "name", "displayName"),
		domain:       pick("domain", "baseUrl", "base_url"),
		enterpriseID: pick("enterprise_id", "enterpriseId", "enterpriseID", "tenantId"),
		accessToken:  pick("access_token", "accessToken", "token"),
		refreshToken: pick("refresh_token", "refreshToken", "refresh"),
		expiresAt:    pickInt("expiresAt", "expires_at", "expire_at", "expireAt"),
	}

	edition := pick("edition", "site")
	realm := pick("realm")
	it.site = auth.ResolveSite(edition, realm, it.domain)

	if it.accessToken == "" {
		if importTokenEnvelope(m, authObj, acctObj, rawObj) {
			// 有凭据，但是加密信封：这不是「文件丢了 token」，
			// 分开报错才不会把用户引向错误的排查方向。
			it.encrypted = true
			it.parseErr = "加密凭据：本工具没有解密密钥，无法导入（可用 wb-switch 导入，或重新登录）"
		} else {
			it.parseErr = "缺少 access_token"
		}
	}
	if it.uid == "" && it.parseErr == "" {
		it.parseErr = "缺少 uid"
	}
	return it
}

// importTokenEnvelope 判断记录里的 access_token 是不是加密信封对象。
//
// 判据与 wb-switch 的 `account::is_envelope` 逐字一致：**值是 JSON 对象且含
// "$wbEncrypted" 键**。只认这个键是刻意的 —— 客户端未来可能加别的对象型字段，
// 把任意对象都当加密会让「加密凭据」徽章失真；普通对象仍按「没有 token」处理
// （与既有行为一致，不冒充加密凭据）。
//
// 只在**没有解析出明文 token** 时才会被调用：wb-switch 的导出里 access_token
// 是信封，但同一份记录的 auth_raw 里可能还有一份旧版明文，那时我们拿到的是
// 可用的明文，不该标成加密 —— 这个标记表达的是「这份凭据只识别出加密形态」。
func importTokenEnvelope(sources ...map[string]any) bool {
	for _, src := range sources {
		if src == nil {
			continue
		}
		for _, key := range []string{"access_token", "accessToken", "token"} {
			obj, ok := src[key].(map[string]any)
			if !ok {
				continue
			}
			if _, isEnvelope := obj["$wbEncrypted"]; isEnvelope {
				return true
			}
		}
	}
	return false
}

// importText 宽容取字符串（别家的导出会把 uid 写成 number）。
//
// 刻意不复用 responses.go 里的 asString：那个只做 `v.(string)` 断言，
// 语义是「上游这一格是不是字符串」；而这里是「把别家导出的值读成字符串」，
// 数字形式的 uid 必须能读出来。两者的失败方向不同，不该合成一个函数。
func importText(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

// asInt64 宽容取整数（字符串形式的时间戳也认）。
func importInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
		return 0, false
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		var n int64
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// baseName 取路径的最后一段（避免为了一个 basename 引入 path/filepath 到本文件）。
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// handleAccountIntlActivate 手动完成国际站账号的注册激活并领取 trial 加油包
// （POST /panel/api/accounts/{id}/intl-activate）。
//
// 为什么需要手动入口：新注册的国际站账号必须先补注册地区并激活，否则对话直接报
// `14017 trial not activated`。登录与导入都会自动跑一次，但那次失败（网络抖动、
// 上游维护）不会拦住登录本身 —— 用户需要一个「再试一次」的按钮，而不是重新登录。
func (s *Server) handleAccountIntlActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}
	acc, ok := s.accountFromPath(w, r)
	if !ok {
		return
	}
	if acc.Profile().Key != "intl" {
		writeJSON(w, http.StatusBadRequest, errBody("只有国际站账号需要注册激活"))
		return
	}
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()
	view := acc.View()
	prof := acc.Profile()

	activated, err := s.client.GlobalCompleteRegistration(ctx, view, prof)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "error": map[string]any{"code": 502, "message": "注册激活失败: " + err.Error()},
		})
		return
	}
	claimed, trialErr := s.client.ClaimTrial(ctx, view, prof)
	_ = s.pool.RefreshQuota(ctx, acc)

	notes := []string{}
	if activated {
		notes = append(notes, "注册激活已完成")
	}
	switch {
	case trialErr != nil:
		notes = append(notes, "trial 加油包领取失败："+trialErr.Error())
	case claimed:
		notes = append(notes, "已领取 trial 加油包")
	default:
		notes = append(notes, "trial 加油包已领取过")
	}
	s.logf("[国际站] %s 手动激活：%s", acc.Cred.AccountID(), strings.Join(notes, "；"))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "activated": activated, "trial_claimed": claimed,
		"detail": strings.Join(notes, "；"),
	})
}

// handleLoginRegions 返回国际站可选的注册地区（GET /panel/api/login/regions?account=<id>）。
//
// 地区列表来自上游（只读），用于面板展示「当前可选的地区」。取不到时返回空列表 +
// 说明，而不是报错 —— 「拉不到列表」不该让整页变成错误态。
func (s *Server) handleLoginRegions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 GET"))
		return
	}
	accountID := strings.TrimSpace(r.URL.Query().Get("account"))
	if accountID == "" {
		writeJSON(w, http.StatusBadRequest, errBody("缺少 account 参数（需要一个已登录的国际站账号来查询）"))
		return
	}
	acc, err := s.pool.Find(accountID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errBody("账号不存在: "+accountID))
		return
	}
	if acc.Profile().Key != "intl" {
		writeJSON(w, http.StatusBadRequest, errBody("只有国际站账号有注册地区"))
		return
	}
	ctx, cancel := contextWithTimeout(r, 30*time.Second)
	defer cancel()
	regions := s.client.GlobalRegions(ctx, acc.View(), acc.Profile())
	writeJSON(w, http.StatusOK, map[string]any{
		"regions": regions,
		"note":    noteIfEmpty(len(regions), "上游没有返回可选地区（网络或上游异常，可稍后重试）"),
	})
}

// noteIfEmpty 在列表为空时给出一句说明（列表非空返回空串）。
func noteIfEmpty(n int, note string) string {
	if n == 0 {
		return note
	}
	return ""
}
