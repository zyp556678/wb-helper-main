// Package localapps 负责「把账号池里的某个账号写入本机某个应用的登录态」。
//
// 为什么网关能做这件事：网关本身就是**跑在本机上的进程**（127.0.0.1:8317），
// 读写本机文件、探测本机安装是它的本来能力（凭据文件、本机代理、开机自启都已在做）。
// 面板虽然拿不到 Tauri 的 IPC（壳把窗口导航到了 http://127.0.0.1:8317，
// 页面已不在应用源内），但它只要能调 /panel/api/* 就够了 —— 真正的本机操作在网关侧。
//
// 分工：
//   - targets.go —— 探测本机装了哪些目标、各自当前登录的是池里哪个账号
//   - backup.go  —— 写之前备份、写坏了能恢复（所有目标共用）
//   - cli.go     —— CodeBuddy CLI 切换（明文 token，写入 ~/.codebuddy/settings.json）
//   - workbuddy.go —— WorkBuddy 客户端切换（**加密信封**，见该文件顶部的限制说明）
package localapps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// TargetID 是本机目标的稳定标识。**不要改这些字符串** —— 它们会出现在
// 备份文件名、API 路径与前端状态里，改了会让旧备份变成孤儿。
type TargetID string

const (
	TargetCLI        TargetID = "codebuddy-cli"
	TargetWorkBuddy  TargetID = "workbuddy-desktop"
	TargetJetBrains  TargetID = "jetbrains"
	TargetVSCode     TargetID = "vscode"
	TargetCodeBuddyI TargetID = "codebuddy-ide"
	// TargetSessions 是 WorkBuddy 的会话库（workbuddy.db + projects/*.jsonl）。
	// 它不是一个「应用登录态」，而是本工具会读写的另一类本机数据 ——
	// 复用同一套备份设施，出问题都能一键恢复。
	TargetSessions TargetID = "workbuddy-sessions"
)

// Label 返回中文展示名。
func (t TargetID) Label() string {
	switch t {
	case TargetCLI:
		return "CodeBuddy CLI"
	case TargetWorkBuddy:
		return "WorkBuddy 客户端"
	case TargetJetBrains:
		return "JetBrains IDE 插件"
	case TargetVSCode:
		return "VS Code 插件"
	case TargetCodeBuddyI:
		return "CodeBuddy IDE"
	case TargetSessions:
		return "WorkBuddy 会话库"
	}
	return string(t)
}

// AllTargets 返回全部目标，顺序即界面展示顺序（能用的排前面）。
func AllTargets() []TargetID {
	return []TargetID{TargetWorkBuddy, TargetCLI, TargetJetBrains, TargetVSCode, TargetCodeBuddyI}
}

// TargetState 是一个目标的探测结果。
//
// **`Available` 与 `Writable` 是两件事**，必须分开：
//   - Available：本机存在这个应用（能读它的当前登录态）
//   - Writable：我们**实现了**它的写入路径
//
// 合成一个布尔值会让界面把「装了但我们没实现」显示成「没装」，
// 把责任推给用户的环境 —— 那是错的。
type TargetState struct {
	ID    TargetID `json:"id"`
	Label string   `json:"label"`
	// Installed 表示本机存在这个应用。
	Installed bool `json:"installed"`
	// Writable 表示我们实现了写入（false 时前端要禁用「设为当前」并说明原因）。
	Writable bool `json:"writable"`
	// Path 是探测到的关键文件/目录，展示给用户便于自查。
	Path string `json:"path"`
	// CurrentUID / CurrentAccount 是当前登录态的识别结果。
	// CurrentAccount 是它在账号池里的 ID（凭据文件名），匹配不到时为空。
	CurrentUID     string `json:"current_uid"`
	CurrentAccount string `json:"current_account"`
	CurrentLabel   string `json:"current_label"`
	// CurrentAccounts 是本目标**全部**当前登录账号的池内 ID（按站点顺序去重）。
	//
	// **为什么与 CurrentAccount 并存**：CurrentAccount 只表达「一个」，而 WorkBuddy
	// 的国内站与国际站是两个互相隔离的客户端/文件，可以各自登录一个账号。
	// 只报一个的后果是实打实的：国际站那份文件登录的账号在面板上永远不显示
	// 「当前登录」绿色状态（表现为「明明是当前账号，卡片上却是『设为…』按钮」）。
	// 单站点目标最多一个元素；空数组表示没登录或识别不到。
	CurrentAccounts []string `json:"current_accounts"`
	// Note 是给用户看的补充说明（未安装原因、限制、冲突等）。
	Note string `json:"note"`
	// Warning 是「**能写，但有风险/有条件**」的提示（非空时前端要二次确认）。
	//
	// **与 Blockers 分开是刻意的**：Blockers 是「做不到」（前端禁用按钮），
	// Warning 是「做得到，但你得知道代价」。合成一个字段会让
	// 「可以切但有风险」被显示成「不可用」，用户就永远看不到这个选项了。
	Warning string `json:"warning"`
	// AllAccounts 是目标客户端自己记着的账号 uid 列表（目前只有 WorkBuddy 客户端有）。
	// 它决定切换的风险：目标账号若在里面，说明客户端手里可能有它的加密凭据，
	// 可以零风险复用。空表示「这个目标没有这种概念」或「读不到」。
	AllAccounts []string `json:"all_accounts"`
	// Blockers 是**阻止写入**的具体原因，逐条人类可读。
	// 非空时 Writable 必须为 false —— 这两者不一致就是 bug。
	Blockers []string `json:"blockers"`
}

// PoolAccount 是探测需要的账号池最小视图（避免本包依赖 pool，便于单测）。
type PoolAccount struct {
	ID           string
	UID          string
	Nickname     string
	Site         string // cn / intl
	AccessToken  string
	RefreshToken string
}

// Probe 探测全部目标。pool 用于把「当前登录态」对应回池里的账号。
func Probe(pool []PoolAccount) []TargetState {
	out := make([]TargetState, 0, len(AllTargets()))
	for _, id := range AllTargets() {
		out = append(out, probeOne(id, pool))
	}
	return out
}

func probeOne(id TargetID, pool []PoolAccount) TargetState {
	st := probeOneRaw(id, pool)
	// **列表字段一律补成空数组**：Go 的 nil slice 会序列化成 `null`，
	// 前端拿到 null 后 `.map()` / `.length` 直接抛异常 → 整页白屏。
	// 这类问题的表现是「页面整个崩了」，而根因只是一个字段是 null。
	if st.Blockers == nil {
		st.Blockers = []string{}
	}
	if st.AllAccounts == nil {
		st.AllAccounts = []string{}
	}
	if st.CurrentAccounts == nil {
		st.CurrentAccounts = []string{}
	}
	return st
}

func probeOneRaw(id TargetID, pool []PoolAccount) TargetState {
	switch id {
	case TargetCLI:
		return probeCLI(pool)
	case TargetWorkBuddy:
		return probeWorkBuddy(pool)
	case TargetJetBrains:
		return probeJetBrains()
	case TargetVSCode:
		return probeVSCode()
	case TargetCodeBuddyI:
		return probeCodeBuddyIDE()
	}
	return TargetState{ID: id, Label: id.Label(), Note: "未知目标"}
}

// testPathOverride 仅供测试注入路径（生产为 nil）。
//
// **刻意不导出**：生产代码没有「换一个 %LOCALAPPDATA%」的正当需求，
// 导出出去只会让人误以为可以这么用。而测试又必须能注入 ——
// 否则跑一次单测就会往用户真实的备份目录里写东西。
var testPathOverride *pathOverride

type pathOverride struct {
	Home         string
	LocalAppData string
}

// homeDir 取用户主目录（失败时返回空串，调用方按「路径不可用」处理）。
func homeDir() string {
	if testPathOverride != nil && testPathOverride.Home != "" {
		return testPathOverride.Home
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// localAppData 取 %LOCALAPPDATA%（Windows）/ ~/.local/share（Linux）/ ~/Library/Application Support（macOS）。
func localAppData() string {
	if testPathOverride != nil && testPathOverride.LocalAppData != "" {
		return testPathOverride.LocalAppData
	}
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v
		}
		return filepath.Join(homeDir(), "AppData", "Local")
	case "darwin":
		return filepath.Join(homeDir(), "Library", "Application Support")
	default:
		return filepath.Join(homeDir(), ".local", "share")
	}
}

// roamingAppData 取 %APPDATA%（Windows）/ 与 localAppData 同义（其它平台）。
func roamingAppData() string {
	if testPathOverride != nil && testPathOverride.LocalAppData != "" {
		return testPathOverride.LocalAppData
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("APPDATA"); v != "" {
			return v
		}
		return filepath.Join(homeDir(), "AppData", "Roaming")
	}
	return localAppData()
}

// workBuddyAuthPath 返回 WorkBuddy 客户端的官方认证文件路径。
//
// 路径与 wb-switch 的 `variant.auth_file_path()` 逐字一致（三平台同一套约定）。
// 国内站与国际站是两个不同的文件，**不要合并** —— 它们各自维护一份登录态。
func workBuddyAuthPath(site string) string {
	name := "workbuddy-desktop.info"
	if site == "intl" {
		name = "workbuddy-desktop-ai.info"
	}
	return filepath.Join(localAppData(), "CodeBuddyExtension", "Data", "Public", "auth", name)
}

// cliSettingsPath 返回 CodeBuddy CLI 的 settings.json 路径。
func cliSettingsPath() string {
	return filepath.Join(homeDir(), ".codebuddy", "settings.json")
}

// -----------------------------------------------------------------------------
// CodeBuddy CLI
// -----------------------------------------------------------------------------

// probeCLI 探测 CodeBuddy CLI：读 settings.json 的 env.CODEBUDDY_AUTH_TOKEN，
// 按 token 反查账号池。
func probeCLI(pool []PoolAccount) TargetState {
	st := TargetState{
		ID: TargetCLI, Label: TargetCLI.Label(), Writable: true,
	}
	path := cliSettingsPath()
	st.Path = path

	raw, err := os.ReadFile(path)
	if err != nil {
		// 未安装**不是阻塞**：写入会新建该文件，CLI 首次启动就会读到。
		// 但它是个需要说清的**条件**（CLI 到底会不会读，取决于它装没装），
		// 所以放 Warning 而不是 Blockers —— 后者按契约意味着「不可写」。
		st.Note = "未找到 ~/.codebuddy/settings.json（可能未安装或从未运行过 CodeBuddy CLI）"
		st.Warning = "未检测到 settings.json：写入会新建该文件，但 CLI 是否读取取决于它是否已安装"
		st.Installed = false
		st.Writable = true
		return st
	}
	st.Installed = true

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 坏 JSON 是**真阻塞**：SwitchCLI 会拒绝写入（覆盖它等于毁掉用户
		// 可能还能手工修好的配置），所以这里必须把 Writable 置假，
		// 否则界面会给出一个点了必然失败的按钮。
		st.Writable = false
		st.Note = "settings.json 不是有效 JSON，未做任何解析"
		st.Blockers = append(st.Blockers, "settings.json 不是有效 JSON，为避免写坏已停止处理")
		return st
	}

	token, _ := cliEnvToken(doc)
	st.CurrentLabel = "未配置 Token"
	if token != "" {
		st.CurrentLabel = "已配置 Token（" + maskSecret(token) + "）"
		for _, a := range pool {
			if sameSecret(a.AccessToken, token) {
				st.CurrentUID = a.UID
				st.CurrentAccount = a.ID
				st.CurrentLabel = displayName(a)
				break
			}
		}
	}

	// 进程/持久化环境变量会**覆盖** settings.json。
	//
	// **这是告警而不是阻塞**：我们读到的是**网关自己进程**的环境变量，
	// 而 CLI 由用户从别处启动（终端 / IDE），继承的未必是同一套。
	// 把它当阻塞会让这个功能在本机永远不可用（实测本机就带着
	// CODEBUDDY_INTERNET_ENVIRONMENT=external），而实际上多半没问题。
	// 说清「什么情况下会出问题」比直接拒绝有用。
	if b := envOverrideWarnings(); len(b) > 0 {
		if st.Warning != "" {
			st.Warning += "\n"
		}
		st.Warning += strings.Join(b, "\n")
	}
	return st
}

// cliEnvToken 从 settings.json 取 env.CODEBUDDY_AUTH_TOKEN。
func cliEnvToken(doc map[string]any) (string, bool) {
	env, ok := doc["env"].(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := env["CODEBUDDY_AUTH_TOKEN"].(string)
	return strings.TrimSpace(v), ok
}

// -----------------------------------------------------------------------------
// WorkBuddy 客户端
// -----------------------------------------------------------------------------

// probeWorkBuddy 探测 WorkBuddy 客户端：读官方认证文件的 account.uid，按 uid 反查账号池。
//
// 国内站与国际站各一个文件，两个都探。**只要有一个存在就算装了** ——
// 用户可能只装了其中一档。
func probeWorkBuddy(pool []PoolAccount) TargetState {
	st := TargetState{ID: TargetWorkBuddy, Label: TargetWorkBuddy.Label()}

	var found []string
	var uids []string
	var allUIDs []string
	// 按站点记下「当前登录的池内账号 ID」：国内站与国际站是两个独立文件，
	// 各能登录一个账号，两个都要报（见 TargetState.CurrentAccounts 的说明）。
	currentBySite := map[string]string{}
	for _, site := range []string{"cn", "intl"} {
		p := workBuddyAuthPath(site)
		doc, err := readJSONMap(p)
		if err != nil {
			continue
		}
		found = append(found, p)
		if acc, ok := doc["account"].(map[string]any); ok {
			if uid, _ := acc["uid"].(string); strings.TrimSpace(uid) != "" {
				uid = strings.TrimSpace(uid)
				uids = append(uids, uid)
				for _, a := range pool {
					if a.UID == uid {
						currentBySite[site] = a.ID
						break
					}
				}
			}
		}
		// 客户端自己记着的账号列表 —— 决定「切换时能否复用加密凭据」。
		for _, a := range allAccountsOf(doc) {
			if uid, _ := a["uid"].(string); strings.TrimSpace(uid) != "" {
				allUIDs = append(allUIDs, strings.TrimSpace(uid))
			}
		}
	}
	st.AllAccounts = dedupeStrings(allUIDs)

	if len(found) == 0 {
		st.Path = filepath.Dir(workBuddyAuthPath("cn"))
		st.Note = "未找到官方认证文件 workbuddy-desktop.info / workbuddy-desktop-ai.info"
		return st
	}
	st.Installed = true
	st.Path = filepath.Dir(found[0])
	st.Writable = true

	for _, site := range []string{"cn", "intl"} {
		if id := currentBySite[site]; id != "" {
			st.CurrentAccounts = append(st.CurrentAccounts, id)
		}
	}

	if len(uids) > 0 {
		st.CurrentUID = uids[0]
		// CurrentAccount 保持「单值」语义：优先国内站，其次国际站（两个站点都登录时
		// 旧字段表达不了两个）。完整的按站点清单在 CurrentAccounts，前端以它为准。
		st.CurrentAccount = currentBySite["cn"]
		if st.CurrentAccount == "" {
			st.CurrentAccount = currentBySite["intl"]
		}
		if len(uids) > 1 {
			st.CurrentLabel = "国内站 " + shortUID(uids[0]) + " / 国际站 " + shortUID(uids[1])
		} else {
			st.CurrentLabel = shortUID(uids[0])
		}
		if st.CurrentAccount == "" {
			st.CurrentLabel += "（不在账号池中）"
		}
	} else {
		st.CurrentLabel = "未识别到登录账号"
	}

	// 加密信封：WorkBuddy 5.6 起 token/nickname 是 {$wbEncrypted, envelope}，
	// 由客户端用它自己的密钥库解密。我们**没有那个密钥**，所以写进去的只能是明文。
	// 见 workbuddy.go 顶部的完整限制说明。
	// 退出标记：客户端会无视凭据判为未登录。切换时会自动清理，
	// 但**必须让用户知道它存在** —— 否则「凭据是好的却说没登录」无从排查。
	// 用 appendWarning 而不是 `=`：下面还有别的告警要写，
	// 用赋值会把这一条覆盖掉（第一版就踩了）。
	for _, p := range found {
		if HasLogoutMarker(p) {
			appendWarning(&st, "客户端存在退出标记（"+logoutMarkerPath(p)+"）："+
				"该标记存在时客户端会无视认证文件里的凭据判为未登录。切换时会自动清理它")
			break
		}
	}
	if anyTokenEncrypted(found) {
		appendWarning(&st, "该客户端的凭据是加密信封（WorkBuddy 5.6+）。若目标账号已在客户端的账号列表里，"+
			"可直接复用它的加密凭据（零风险）；否则只能写入明文凭据，客户端能否接受无法离线确认，"+
			"切换后需重启客户端验证，失败可用备份一键恢复")
	}
	return st
}

// appendWarning 追加一条告警（多条之间用换行分隔）。
func appendWarning(st *TargetState, msg string) {
	if st.Warning != "" {
		st.Warning += "\n"
	}
	st.Warning += msg
}

// anyTokenEncrypted 判断这些认证文件里的 accessToken 是不是加密信封对象。
//
// 判据是「是对象且带 $wbEncrypted 字段」，不是「是对象」——
// 客户端未来可能加别的对象型字段，把任意对象都当成加密会让提示文案失真。
func anyTokenEncrypted(paths []string) bool {
	for _, p := range paths {
		doc, err := readJSONMap(p)
		if err != nil {
			continue
		}
		auth, ok := doc["auth"].(map[string]any)
		if !ok {
			continue
		}
		tok, ok := auth["accessToken"].(map[string]any)
		if !ok {
			continue
		}
		if _, isEnvelope := tok["$wbEncrypted"]; isEnvelope {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// 其余三个目标：只做探测，不做写入
// -----------------------------------------------------------------------------

func probeJetBrains() TargetState {
	st := TargetState{ID: TargetJetBrains, Label: TargetJetBrains.Label()}
	root := jetbrainsRoot()
	if entries, err := os.ReadDir(root); err != nil || len(entries) == 0 {
		st.Note = "未检测到 JetBrains 配置目录"
		return st
	}
	st.Path = root

	// **装了 JetBrains ≠ 装了 CodeBuddy 插件**，必须查到插件目录才算可用 ——
	// 只看到 IDE 就报「已检测」会让用户点了才发现插件没装。
	dirs := FindJetBrainsDirs()
	if len(dirs) == 0 {
		st.Note = "检测到 JetBrains 配置目录，但未发现 CodeBuddy 插件（请先在 IDE 里安装插件）"
		st.Blockers = append(st.Blockers, "未检测到 JetBrains 的 CodeBuddy 插件")
		return st
	}

	st.Installed = true
	st.Writable = true
	names := make([]string, 0, len(dirs))
	current := ""
	for _, d := range dirs {
		names = append(names, d.Name+"（插件 "+d.PluginDir+"）")
		if current == "" && d.CurrentTokenMasked != "" {
			current = d.CurrentTokenMasked
		}
	}
	st.Path = dirs[0].SecretPath
	st.CurrentLabel = strings.Join(names, "、")
	if current != "" {
		st.CurrentLabel += " · 当前 token " + current
	} else {
		st.CurrentLabel += " · 未配置 token"
	}
	st.Note = "写入 options/secret-storage.xml 的 " + jetbrainsSecretKey + "（其余 Entry 原样保留）"
	return st
}

func probeVSCode() TargetState {
	st := TargetState{ID: TargetVSCode, Label: TargetVSCode.Label()}
	db := filepath.Join(roamingAppData(), "Code", "User", "globalStorage", "state.vscdb")
	extDir := filepath.Join(homeDir(), ".vscode", "extensions")

	_, dbErr := os.Stat(db)
	exts, _ := os.ReadDir(extDir)

	hasExt := false
	for _, e := range exts {
		if strings.Contains(strings.ToLower(e.Name()), "codebuddy") {
			hasExt = true
			break
		}
	}

	switch {
	case dbErr == nil || hasExt:
		st.Installed = true
		st.Path = db
		st.Note = "已检测到 VS Code / 插件，但本工具未实现写入"
		st.Blockers = append(st.Blockers,
			"未实现写入：VS Code 的凭据存在 state.vscdb（SQLite），需要引入数据库依赖，"+
				"而本项目目前是零依赖（go.mod 无任何 require）")
	default:
		st.Path = db
		st.Note = "未检测到 VS Code（既无 globalStorage/state.vscdb，也无 ~/.vscode/extensions）"
	}
	return st
}

func probeCodeBuddyIDE() TargetState {
	st := TargetState{ID: TargetCodeBuddyI, Label: TargetCodeBuddyI.Label()}
	// **只认 IDE 自己的目录**。
	//
	// 曾经把 `%LOCALAPPDATA%\CodeBuddyExtension\Data` 当候选，那是**错的**：
	// 该目录是 WorkBuddy **客户端**的扩展数据（认证文件就在它下面），
	// 拿它判 IDE 会让本机永远报「已检测到 CodeBuddy IDE」—— 一个把别人的目录
	// 算成自己战绩的误报，比「未检测到」更糟。
	//
	// CodeBuddy IDE 与 VS Code 同源，凭据同样落在 SQLite 的 state.vscdb，
	// 所以我们即使检测到了也写不了；这里如实报「未检测到」即可。
	candidates := []string{
		filepath.Join(homeDir(), ".codebuddy-ide"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			st.Path = c
			st.Installed = true
			break
		}
	}
	if !st.Installed {
		st.Note = "未检测到 CodeBuddy IDE 的数据目录"
		return st
	}
	st.Note = "已检测到数据目录，但本工具未实现写入"
	st.Blockers = append(st.Blockers, "未实现写入（同 VS Code：需写 SQLite 的 state.vscdb）")
	return st
}

// -----------------------------------------------------------------------------
// 工具
// -----------------------------------------------------------------------------

// dedupeStrings 去重并保持首次出现的顺序。
//
// 用 map 去重会把顺序变成随机的，而这个列表要展示给用户看
// （「客户端里有 3 个账号」），顺序跳来跳去会让人以为内容变了。
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// readJSONMap 读取并解析 JSON 对象。
func readJSONMap(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// displayName 取账号展示名（与面板同一口径：昵称优先，退回 uid）。
func displayName(a PoolAccount) string {
	if strings.TrimSpace(a.Nickname) != "" {
		return a.Nickname
	}
	if strings.TrimSpace(a.UID) != "" {
		return shortUID(a.UID)
	}
	return a.ID
}

// shortUID 截断长 uid 便于展示。
func shortUID(uid string) string {
	if len(uid) > 14 {
		return uid[:8] + "…"
	}
	return uid
}

// maskSecret 把密钥遮成「前 6 位…长度」—— 足够辨认、不足以泄露。
func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 10 {
		return "***"
	}
	return s[:6] + "…（" + itoa(len(s)) + " 字符）"
}

// sameSecret 比较两个密钥（去 Bearer 前缀、去空白后逐字比较）。
//
// 不做哈希比较：这里不是防时序攻击的场景，而**长度不同的哈希比较会掩盖
// 「前缀相同但被截断」这类真实的坏数据**。
func sameSecret(a, b string) bool {
	return cleanToken(a) != "" && cleanToken(a) == cleanToken(b)
}

// cleanToken 去掉 "Bearer " 前缀与首尾空白。
//
// switch 的 `clean_bearer_token` 做同一件事，原因也一样：token 在配置文件里
// 可能带也可能不带前缀，带前缀时直接字符串比较会**判定为不匹配**，
// 于是「明明就是同一个账号」被显示成「不在账号池中」。
func cleanToken(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 7 && strings.EqualFold(s[:7], "bearer ") {
		s = strings.TrimSpace(s[7:])
	}
	return s
}

// envOverrideWarnings 报告会覆盖 settings.json 的环境变量。
//
// **只查本进程的 env，这是有意的**：持久化的用户/系统级变量会在进程启动时
// 继承进来，所以「读自己的 env」已经覆盖了持久化的情况。要读 Windows 注册表
// 才拿得到的唯一情形是「进程启动之后才设置的用户级变量」——
// 那种情况下重启本进程即可，而为此引入注册表依赖（本项目目前零依赖）
// 是不划算的取舍。**知道这个边界在哪，比假装全覆盖更可靠。**
//
// 注意措辞：说的是「**如果** CLI 从同一环境启动」——不能断言 CLI 一定会带上，
// 因为我们并没有启动它。
func envOverrideWarnings() []string {
	const tokenKey = "CODEBUDDY_AUTH_TOKEN"
	const envKey = "CODEBUDDY_INTERNET_ENVIRONMENT"

	var out []string
	if os.Getenv(tokenKey) != "" {
		out = append(out, "注意：网关进程的环境里有 "+tokenKey+"。"+
			"若 CodeBuddy CLI 从同一环境启动，它会覆盖 settings.json 里的 token —— 届时请先删除该变量")
	}
	if v := os.Getenv(envKey); v != "" {
		out = append(out, "注意：网关进程的环境里有 "+envKey+"="+v+"。"+
			"若 CodeBuddy CLI 从同一环境启动，切换档位会被它覆盖 —— 届时请先删除该变量")
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
