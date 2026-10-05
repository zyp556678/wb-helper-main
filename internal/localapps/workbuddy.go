package localapps

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// -----------------------------------------------------------------------------
// WorkBuddy 客户端切换
//
// 写官方认证文件 `workbuddy-desktop{,-ai}.info`，四段结构：
//
//	{"account": {...}, "auth": {...}, "accounts": [...], "allAccounts": [...]}
//
// **这与 CLI 的情况有本质区别，必须说清：**
//
// WorkBuddy 5.6 起，`auth.accessToken` / `auth.refreshToken` / `account.nickname`
// 等字段是**加密信封** `{"$wbEncrypted":1,"envelope":"<base64>"}`，由客户端用它
// 自己的密钥库（envelope 里的 keyId）解密。**我们没有那个密钥**，
// 因此**无法为任意账号生成有效信封** —— 这是硬限制，不是没实现。
//
// 应对策略（按优先级）：
//  1. 若目标账号**已经在 allAccounts 里**带着信封 → 原样复用它的信封，零风险；
//  2. 否则只能写明文 token。客户端能否接受明文**本机无法离线验证**：
//     要验证就得真写一次、重启客户端、看能否登录 —— 那会动到你正在用的登录态。
//
// 所以：全流程强制备份 + 写后回读校验 + 一键恢复，并且**明文写入要显式确认**。
//
// **切换会关闭并重新打开客户端**（顺序：关闭 → 写认证文件 → 启动，见
// workbuddy_process.go 顶部的理由：不关的话客户端退出时会把内存里的登录态回写，
// 把刚写进去的账号覆盖掉）。客户端原本没在运行时不会替用户打开它；
// 关闭之后任何失败都会把客户端恢复回去，不留下一个「被关掉却没打开」的客户端。
// -----------------------------------------------------------------------------

// WorkBuddyStatus 是 WorkBuddy 客户端的当前状态。
type WorkBuddyStatus struct {
	// Present 表示认证文件是否存在。
	Present bool `json:"present"`
	// Encrypted 表示现有 token 是否为加密信封。
	Encrypted bool `json:"encrypted"`
	// UID / Nickname 是当前登录账号的标识。
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	// AllAccounts 是客户端自己记着的账号 uid 列表。
	//
	// **它决定切换的风险**：目标账号若在这个列表里，说明客户端曾登录过它、
	// 手里可能有它的信封，可以零风险复用。
	AllAccounts []string `json:"all_accounts"`
	// PlaintextUIDs 是 allAccounts 里 token 为明文（非信封）的那些 uid。
	PlaintextUIDs []string `json:"plaintext_uids"`
	// LoggedOut 表示存在退出标记 —— 客户端会**无视认证文件里的凭据**判为未登录。
	// 切换时会自动清理；这里暴露出来是为了让界面能解释「凭据明明是好的，
	// 客户端却说没登录」这种反常现象。
	LoggedOut bool `json:"logged_out"`
}

// ReadWorkBuddyStatus 读取指定站点的认证文件状态。
func ReadWorkBuddyStatus(site string) (WorkBuddyStatus, error) {
	var st WorkBuddyStatus
	path := workBuddyAuthPath(site)
	doc, err := readJSONMap(path)
	if err != nil {
		return st, err
	}
	st.Present = true
	st.LoggedOut = HasLogoutMarker(path)

	if acc, ok := doc["account"].(map[string]any); ok {
		st.UID, _ = acc["uid"].(string)
		if n, ok := acc["nickname"].(string); ok {
			st.Nickname = n
		} else if _, isObj := acc["nickname"].(map[string]any); isObj {
			st.Nickname = "（加密）"
		}
	}
	if a, ok := doc["auth"].(map[string]any); ok {
		if _, isObj := a["accessToken"].(map[string]any); isObj {
			st.Encrypted = true
		}
	}
	for _, entry := range allAccountsOf(doc) {
		uid, _ := entry["uid"].(string)
		if uid == "" {
			continue
		}
		st.AllAccounts = append(st.AllAccounts, uid)
		// allAccounts 里每一项的 token 可能在 auth 子对象里，也可能没有。
		if tok, ok := entry["accessToken"].(map[string]any); ok {
			if _, isEnv := tok["$wbEncrypted"]; !isEnv {
				st.PlaintextUIDs = append(st.PlaintextUIDs, uid)
			}
		} else if tok, ok := entry["accessToken"].(string); ok && strings.TrimSpace(tok) != "" {
			st.PlaintextUIDs = append(st.PlaintextUIDs, uid)
		}
	}
	return st, nil
}

// SwitchWorkBuddy 把 acc 设为指定站点 WorkBuddy 客户端的当前账号。
//
// allowPlaintext 为假时，若目标账号没有可复用的信封，直接拒绝并说明原因 ——
// 把「有风险」变成「必须显式同意」，而不是弹个提示就照写。
//
// 顺序（对照 wb-switch 的 switch_account）：
//
//	关闭客户端 → 备份 → 写认证文件 → 写后校验 → 清理退出标记 → 启动客户端
//
// **关闭必须在写入之前**：客户端退出时会把内存里的登录态回写认证文件，
// 先写后关会被它覆盖（现象是「切了又变回去」）。
func SwitchWorkBuddy(site string, acc PoolAccount, allowPlaintext bool) (notes []string, err error) {
	if site != "cn" && site != "intl" {
		return nil, fmt.Errorf("站点必须是 cn 或 intl，实际 %q", site)
	}
	if strings.TrimSpace(acc.UID) == "" {
		return nil, fmt.Errorf("账号 %s 没有 uid，无法写入认证文件（客户端的身份就是 uid）", displayName(acc))
	}

	path := workBuddyAuthPath(site)
	preDoc, err := readJSONMap(path)
	if err != nil {
		return nil, fmt.Errorf("读取认证文件失败（该站点的客户端可能未登录过）: %w", err)
	}

	// 明文门槛先判一次：能复用信封就不必打扰用户；不能复用时**在关闭客户端之前**
	// 就拒绝 —— 否则用户会得到一个「客户端被关了、账号也没切成」的结果。
	if _, ok := findReusableEnvelope(preDoc, acc.UID); !ok && !allowPlaintext {
		return nil, errPlaintextNeeded
	}

	// ---- 0. 关闭客户端 ----
	//
	// 关掉它有两个作用：一是避免它退出时回写覆盖我们写的内容（见函数注释），
	// 二是客户端运行时可能一直持有认证文件，写入的可见性没有保证。
	closeNotes, wasRunning, exeHint, err := closeWorkBuddyClient(site, wbCloseTimeout)
	notes = append(notes, closeNotes...)
	if err != nil {
		return notes, err
	}

	// 关闭之后的任何失败都要把客户端恢复回去：用户点「切换」失败后，
	// 不该面对一个被我们关掉却没打开的客户端。
	clientHandled := false
	defer func() {
		if clientHandled || !wasRunning {
			return
		}
		if note, lerr := launchWorkBuddyClient(site, exeHint); lerr != nil {
			notes = append(notes, "提示：切换未完成，且自动重开客户端失败（"+lerr.Error()+"），请手动打开")
		} else {
			notes = append(notes, "切换未完成，已重新打开客户端（"+note+"）")
		}
	}()

	// 关客户端后**重新读**：它退出时可能刚回写过一份，用旧内容做判断会失真。
	doc, err := readJSONMap(path)
	if err != nil {
		return notes, fmt.Errorf("关闭客户端后读取认证文件失败: %w", err)
	}

	// ---- 1. 备份 ----
	// 放在关闭之后：此时文件是客户端退出后稳定下来的状态，备份才是「切换前的真实登录态」。
	entry, err := BackupFor(TargetWorkBuddy, []string{path},
		fmt.Sprintf("切换 %s 客户端账号到 %s", acc.Site, displayName(acc)))
	if err != nil {
		return notes, fmt.Errorf("备份失败，已放弃切换: %w", err)
	}
	notes = append(notes, "已备份 "+path+" → "+entry.ID)

	// ---- 2. 找一个可复用的信封 ----
	//
	// **注意结构**：顶层 `auth` 是「当前账号」的凭据；`allAccounts[i]` 才是
	// 各账号各自的历史记录，信封（若有）挂在那一条上。所以复用不是「保留顶层 auth」，
	// 而是「把目标账号那一条里的凭据**提升**成新的顶层 auth」。
	all := allAccountsOf(doc)
	targetEntry, reusedEnvelope := findReusableEnvelope(doc, acc.UID)

	if !reusedEnvelope && !allowPlaintext {
		return notes, errPlaintextNeeded
	}

	// ---- 3. 构造目标 account / auth ----
	var (
		accountObj map[string]any
		auth       map[string]any
	)
	if reusedEnvelope {
		accountObj = *targetEntry
		// 把该账号自己的 auth 子对象提升为顶层 auth；没有子对象时，
		// 说明信封是直接挂在记录上的（accessToken 与 uid 同级）。
		auth = map[string]any{}
		if sub, ok := (*targetEntry)["auth"].(map[string]any); ok {
			for k, v := range sub {
				auth[k] = v
			}
		}
		if _, ok := auth["accessToken"]; !ok {
			if tok, ok := (*targetEntry)["accessToken"]; ok {
				auth["accessToken"] = tok
			}
		}
		if _, ok := auth["tokenType"]; !ok {
			if prev, ok := doc["auth"].(map[string]any); ok {
				if tt, ok := prev["tokenType"]; ok {
					auth["tokenType"] = tt
				}
			}
		}
		if _, ok := auth["accessToken"]; !ok {
			// 走到这里说明 hasEnvelopeToken 判真但取不出来，属于实现不一致，
			// 宁可报错也不要退化成写明文（那正是本次要避免的事）。
			return notes, fmt.Errorf("该账号在客户端里有加密凭据但无法取出，" +
				"为避免写入明文已中止；请改用「允许写入明文凭据」显式确认")
		}
		notes = append(notes, "复用了客户端已有的加密凭据（该账号在客户端的账号列表里），未写入明文")
	} else {
		token := cleanToken(acc.AccessToken)
		if token == "" {
			return notes, fmt.Errorf("账号 %s 没有可用的 accessToken", displayName(acc))
		}
		accountObj = map[string]any{
			"uid":           acc.UID,
			"nickname":      acc.Nickname,
			"type":          "personal",
			"lastLogin":     true,
			"pluginEnabled": true,
		}
		auth = map[string]any{
			"accessToken": token,
			"tokenType":   "Bearer",
		}
		if strings.TrimSpace(acc.RefreshToken) != "" {
			auth["refreshToken"] = cleanToken(acc.RefreshToken)
		}
		notes = append(notes, "客户端里没有该账号的加密凭据 → 写入明文 token 与昵称")
	}

	// ---- 4. 并入 allAccounts（按 uid 去重后追加）----
	merged := make([]map[string]any, 0, len(all)+1)
	for _, a := range all {
		if uid, _ := a["uid"].(string); uid == acc.UID {
			continue
		}
		merged = append(merged, a)
	}
	merged = append(merged, accountObj)

	doc["account"] = accountObj
	doc["auth"] = auth
	doc["accounts"] = merged
	doc["allAccounts"] = merged

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return notes, err
	}
	if err := atomicWrite(path, append(data, '\n')); err != nil {
		return notes, fmt.Errorf("写入认证文件失败: %w", err)
	}
	notes = append(notes, fmt.Sprintf("已写入 %s（账号 %s，共 %d 个账号）",
		path, shortUID(acc.UID), len(merged)))

	// ---- 5. 写后回读校验 ----
	verify, err := readJSONMap(path)
	if err != nil {
		return notes, fmt.Errorf("写后无法回读认证文件（请用备份恢复）: %w", err)
	}
	var gotUID string
	if a, ok := verify["account"].(map[string]any); ok {
		gotUID, _ = a["uid"].(string)
	}
	if gotUID != acc.UID {
		return notes, fmt.Errorf("写后校验失败：认证文件里的 uid 与目标不一致。"+
			"已保留备份 %s，可一键恢复", entry.ID)
	}
	notes = append(notes, "写后校验通过（回读 uid 与目标一致）")

	// ---- 6. 清理退出标记 ----
	//
	// WorkBuddy **优先读取退出标记**：标记存在时，**即使认证文件里有有效凭据也判为未登录**。
	// 不清理的话「切换成功」是假的 —— 用户看到成功、客户端仍是未登录。
	//
	// 顺序要求（对照 wb-switch 的 `a657dc8`）：必须在**写入与写后校验都成功之后**才删；
	// 删除失败时保留退出状态并**让本次调用失败**，不能报告成功。
	if clearedMarker, err := clearLogoutMarker(path); err != nil {
		return notes, err
	} else if clearedMarker {
		notes = append(notes, "已清理退出标记（该标记会让客户端无视认证文件里的凭据）")
	}

	if !reusedEnvelope {
		notes = append(notes, "提示：本次写入的是明文凭据，重启后请确认能否正常登录；"+
			"若失败，请用备份 "+entry.ID+" 恢复")
	}

	// ---- 7. 启动客户端 ----
	//
	// 原本在运行才重启：用户没开着客户端时，替他打开一个应用是多余的打扰。
	// 启动失败**不把整次切换判为失败** —— 认证文件已经写好并校验过，
	// 报错会让用户以为没切成而反复重试；这里如实说明并让他手动打开。
	if wasRunning {
		note, lerr := launchWorkBuddyClient(site, exeHint)
		if lerr != nil {
			notes = append(notes, "提示："+lerr.Error())
		} else {
			notes = append(notes, note+"，已切到新账号")
		}
	} else {
		notes = append(notes, "客户端原本未运行，未自动启动（下次打开即为该账号）")
	}
	clientHandled = true
	return notes, nil
}

// errPlaintextNeeded 是「只能写明文但没有显式同意」时的拒绝理由。
//
// 抽成变量是为了让**两处**判断（关闭客户端之前的预检、关闭之后的正式判断）
// 给出逐字一致的文案 —— 同一件事在两处说成两种话会让用户以为是两个问题。
var errPlaintextNeeded = fmt.Errorf(
	"该账号在 WorkBuddy 客户端里没有可复用的加密凭据，只能写入明文 token，" +
		"而客户端是否接受明文无法离线确认。请确认风险后重试（勾选「允许写入明文凭据」）")

// findReusableEnvelope 在认证文件里找目标账号的记录，并判断它是否带着可复用的加密信封。
//
// 返回的记录指针用于把该账号那一条里的凭据**提升**成顶层 auth（见 SwitchWorkBuddy 注释）。
func findReusableEnvelope(doc map[string]any, uid string) (*map[string]any, bool) {
	all := allAccountsOf(doc)
	for i := range all {
		if got, _ := all[i]["uid"].(string); got == uid {
			return &all[i], hasEnvelopeToken(all[i])
		}
	}
	return nil, false
}

// logoutMarkerPath 返回退出标记路径。
//
// **在完整认证文件名后追加** `.logged-out`，而不是替换 `.info` 扩展名：
// 正确是 `workbuddy-desktop.info.logged-out`，
// 写成 `workbuddy-desktop.logged-out` 客户端根本不会读（静默无效）。
func logoutMarkerPath(authPath string) string {
	return authPath + ".logged-out"
}

// clearLogoutMarker 删除退出标记。
//
// 返回「是否真的删掉了一个标记」：标记本来不存在是**常态**（用户没退出过登录），
// 不该在切换说明里写「已清理退出标记」——那会让用户以为发生过什么。
func clearLogoutMarker(authPath string) (bool, error) {
	err := os.Remove(logoutMarkerPath(authPath))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("清理 WorkBuddy 退出标记失败（%s）：%w。"+
		"该标记存在时客户端会无视认证文件里的凭据，所以本次切换尚未生效",
		logoutMarkerPath(authPath), err)
}

// HasLogoutMarker 表示该认证文件对应的客户端是否处于「已退出登录」状态。
//
// 界面据此提示「客户端已被标记为退出登录，切换时会自动清理」——
// 否则用户很难理解「凭据明明是好的，客户端却说没登录」。
func HasLogoutMarker(authPath string) bool {
	_, err := os.Stat(logoutMarkerPath(authPath))
	return err == nil
}

// allAccountsOf 取认证文件里的账号列表（兼容 accounts / allAccounts 两种键名）。
//
// 两个键在官方文件里是**同一份内容**，但历史上出现过只有其中一个的版本，
// 所以读的时候都认、写的时候都写。
func allAccountsOf(doc map[string]any) []map[string]any {
	for _, key := range []string{"allAccounts", "accounts"} {
		raw, ok := doc[key].([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// hasEnvelopeToken 判断一条账号记录里有没有加密信封形态的 token。
func hasEnvelopeToken(entry map[string]any) bool {
	// token 可能直接挂在记录上，也可能嵌在 auth 子对象里。
	candidates := []map[string]any{entry}
	if sub, ok := entry["auth"].(map[string]any); ok {
		candidates = append(candidates, sub)
	}
	for _, m := range candidates {
		if tok, ok := m["accessToken"].(map[string]any); ok {
			if _, isEnv := tok["$wbEncrypted"]; isEnv {
				return true
			}
		}
	}
	return false
}
