// Package auth 定义三合一后的统一凭据模型。
//
// 要同时读两种历史格式，不强制迁移：
//
//	格式一（wb-gateway）：嵌套形，站点标识在顶层 edition 字段
//	  {"auth":{"accessToken":"..","refreshToken":"..","expiresAt":1,"domain":".."},
//	   "account":{"uid":"..","enterpriseId":"..","nickname":".."}, "edition":"cn"}
//
//	格式二（wb2api-panel）：嵌套形或扁平形，站点标识在 realm 字段
//	  {"auth":{...,"realm":"global"},"account":{...}}
//	  {"accessToken":"..","refreshToken":"..","expiresAt":1,"uid":"..","realm":"cn"}
//
// 站点归一化优先级：edition（显式）→ realm（显式）→ domain 后缀 → 默认国内站。
// 写回时按原文件形态就地打补丁，保留未知键（双写不丢字段，便于回退到旧版本）。
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 站点标识。
const (
	SiteCN   = "cn"
	SiteINTL = "intl"
)

// Credential 是归一化后的账号凭据。
type Credential struct {
	Path string

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
	DeviceToken  string

	// 站点原始标识与归一化结果
	Edition string // 格式一：cn / intl（可为空）
	Realm   string // 格式二：cn / global（可为空）
	site    string // 归一化：SiteCN / SiteINTL

	raw map[string]any // 原始 JSON，写回时就地打补丁
}

// Site 返回归一化后的站点标识（cn / intl），恒非空。
func (c *Credential) Site() string {
	if c.site == "" {
		c.site = ResolveSite(c.Edition, c.Realm, c.Domain)
	}
	return c.site
}

// IsEnterprise 报告账号是否为企业版（凭据里带非空 enterpriseId）。
//
// # 为什么需要它
//
// 企业版账号**没有个人成长体系**，上游对这些端点一律拒绝。参考实现
// （wb2api-panel）2026-10-07 做过同一时刻 A/B 实测：
//
//	POST /v2/billing/meter/daily-checkin                → 400 code 10001「企业账号不支持该操作」
//	POST /billing/meter/claim-gift / claim-compensation → 400 code 10001 同上
//	GET  /activity/growth/{streak,buddy/info,heatmap}   → 403「growth system is only available for personal users」
//	GET  /v2/activity/growth/tasks                      → 403 同上
//
// 所以签到 / 成长任务 / 连登管家 / 猫猫旅行 / 夜猫子这五类**周期任务**必须先跳过，
// 否则每个周期都白发一批注定 400/403 的请求，在日志里留下一串噪声，让人误以为是
// 网络问题。显式动作（用户在面板点按钮）不走这条门控 —— 那类失败已被
// tasks.PreconditionError 归成 409「重试无用」。
//
// 注意它与**站点**门控（upstream.Profile.SupportsCheckin / SupportsGrowthActivity）
// **正交**：企业号可以在国内站，所以「国内站」不代表能做成长任务。
//
// 不受影响的能力：选号派发、保活（token 刷新）、额度查询 —— 企业额度改走
// /v2/billing/meter/get-enterprise-user-usage。
func (c *Credential) IsEnterprise() bool {
	return c != nil && strings.TrimSpace(c.EnterpriseID) != ""
}

// SiteLabel 返回站点中文名。
func SiteLabel(site string) string {
	if site == SiteINTL {
		return "国际站"
	}
	return "国内站"
}

// ResolveSite 归一化站点：edition 显式优先 → realm 显式 → domain 后缀 → 默认国内站。
func ResolveSite(edition, realm, domain string) string {
	if s, ok := siteFromToken(edition); ok {
		return s
	}
	if s, ok := siteFromToken(realm); ok {
		return s
	}
	if strings.Contains(strings.ToLower(domain), "workbuddy.ai") {
		return SiteINTL
	}
	return SiteCN
}

// siteFromToken 把各种站点写法收敛为 cn/intl；空值返回 ok=false 表示「未指定」。
func siteFromToken(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return "", false
	case "intl", "international", "global", "ai", "workbuddy.ai", "codebuddy.ai":
		return SiteINTL, true
	default:
		return SiteCN, true
	}
}

// wire 覆盖嵌套形与扁平形的全部已知键。
type wire struct {
	Auth *struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
		Edition      string `json:"edition"`
	} `json:"auth"`
	Account *struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
		Realm        string `json:"realm"`
		Edition      string `json:"edition"`
	} `json:"account"`

	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
	Realm        string `json:"realm"`
	Edition      string `json:"edition"`

	DeviceToken string `json:"device_token"`
}

// Parse 解析凭据文件内容（嵌套形或扁平形），两种格式都能读。
func Parse(data []byte) (*Credential, error) {
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("解析凭据文件失败: %w", err)
	}

	c := &Credential{}
	if w.Auth != nil {
		c.AccessToken = w.Auth.AccessToken
		c.RefreshToken = w.Auth.RefreshToken
		c.ExpiresAt = w.Auth.ExpiresAt
		c.Domain = w.Auth.Domain
		c.Realm = w.Auth.Realm
		c.Edition = w.Auth.Edition
	}
	if w.Account != nil {
		c.UID = w.Account.UID
		c.EnterpriseID = w.Account.EnterpriseID
		c.Nickname = w.Account.Nickname
		if c.Realm == "" {
			c.Realm = w.Account.Realm
		}
		if c.Edition == "" {
			c.Edition = w.Account.Edition
		}
	}
	// 扁平形补齐（同名字段以嵌套形优先，避免两种形态混写时被覆盖）
	c.AccessToken = firstNonEmpty(c.AccessToken, w.AccessToken)
	c.RefreshToken = firstNonEmpty(c.RefreshToken, w.RefreshToken)
	if c.ExpiresAt == 0 {
		c.ExpiresAt = w.ExpiresAt
	}
	c.Domain = firstNonEmpty(c.Domain, w.Domain)
	c.UID = firstNonEmpty(c.UID, w.UID)
	c.EnterpriseID = firstNonEmpty(c.EnterpriseID, w.EnterpriseID)
	c.Nickname = firstNonEmpty(c.Nickname, w.Nickname)
	c.Realm = firstNonEmpty(c.Realm, w.Realm)
	c.Edition = firstNonEmpty(c.Edition, w.Edition)
	c.DeviceToken = w.DeviceToken

	if strings.TrimSpace(c.AccessToken) == "" {
		return nil, errors.New("凭据文件缺少 accessToken")
	}
	c.site = ResolveSite(c.Edition, c.Realm, c.Domain)

	// 保留原始 map 用于写回时打补丁（保留未知键，便于回退到旧版本）
	var root map[string]any
	if err := json.Unmarshal(data, &root); err == nil {
		c.raw = root
	}
	return c, nil
}

// LoadFile 读取并解析单个凭据文件。
func LoadFile(path string) (*Credential, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取凭据文件失败 (%s): %w", path, err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	return c, nil
}

// IsCredentialFile 判断文件名是否像凭据文件。
// 兼容两边的命名习惯：wb-gateway 的 workbuddy*.json 与 wb2api-panel 的
// workbuddy-<uid>.json（后者本就匹配前缀规则），同时排除运行时产物。
func IsCredentialFile(name string) bool {
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		return false
	}
	base := strings.ToLower(name)
	if base == "workbuddy-status.json" || base == "wb-models-cache.json" || base == "config.json" {
		return false
	}
	if strings.HasSuffix(base, ".disabled") {
		return false
	}
	return strings.HasPrefix(base, "workbuddy")
}

// Discover 汇总应加载的凭据路径：
//  1. authDir 非空 → 目录下所有凭据文件（排序，保证轮询顺序稳定）
//  2. explicit 为真 → 按逗号切分 authFile
//  3. 否则 → 扫描工作目录；一个都没有时回退 authFile 以便给出「请先登录」的提示
func Discover(workDir, authFile, authDir string, explicit bool) []string {
	var paths []string
	if strings.TrimSpace(authDir) != "" {
		dir := authDir
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(workDir, dir)
		}
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() || !IsCredentialFile(e.Name()) {
					continue
				}
				paths = append(paths, filepath.Join(dir, e.Name()))
			}
			sort.Strings(paths)
		}
		return paths
	}

	if explicit {
		for _, p := range strings.Split(authFile, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				paths = append(paths, p)
			}
		}
		return paths
	}

	entries, err := os.ReadDir(workDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !IsCredentialFile(e.Name()) {
				continue
			}
			// 必须拼上 workDir 再返回：调用方的进程 CWD 不一定等于工作目录
			// （桌面壳用 WB_GATEWAY_DATA_DIR 指定工作目录，而子进程 CWD 是自己的 bin 目录）。
			// 只返回裸文件名时，读取方会按 CWD 去 open，结果是「扫到了文件名却打不开文件」，
			// 表现为账号池恒为空且日志里全是"读取凭据文件失败"。
			paths = append(paths, filepath.Join(workDir, e.Name()))
		}
		sort.Strings(paths)
	}
	if len(paths) == 0 {
		if filepath.IsAbs(authFile) {
			paths = append(paths, authFile)
		} else {
			paths = append(paths, filepath.Join(workDir, authFile))
		}
	}
	return paths
}

// SaveAtomic 把当前令牌字段写回原文件：就地打补丁保留未知键，tmp + rename 原子替换，权限 0600。
func (c *Credential) SaveAtomic() error {
	if c.Path == "" {
		return errors.New("凭据没有来源路径，无法写回")
	}
	root := c.raw
	if root == nil {
		root = map[string]any{}
	}

	// 判断原文件是嵌套形还是扁平形：有 auth 对象即嵌套形
	if nested, ok := root["auth"].(map[string]any); ok {
		nested["accessToken"] = c.AccessToken
		nested["refreshToken"] = c.RefreshToken
		nested["expiresAt"] = c.ExpiresAt
		if c.Domain != "" {
			nested["domain"] = c.Domain
		}
	} else {
		root["accessToken"] = c.AccessToken
		root["refreshToken"] = c.RefreshToken
		root["expiresAt"] = c.ExpiresAt
		if c.Domain != "" {
			root["domain"] = c.Domain
		}
	}

	return writeDoc(c.Path, root)
}

// NewForLogin 构造登录成功后的凭据对象（尚未落盘）。
func NewForLogin(path, site, accessToken, refreshToken string, expiresAt int64, domain, uid, enterpriseID, nickname string) *Credential {
	c := &Credential{
		Path:         path,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Domain:       domain,
		UID:          uid,
		EnterpriseID: enterpriseID,
		Nickname:     nickname,
	}
	if site == SiteINTL {
		c.Edition = "intl"
		c.Realm = "global"
	} else {
		c.Edition = "cn"
		c.Realm = "cn"
	}
	c.site = ResolveSite(c.Edition, c.Realm, c.Domain)
	return c
}

// SaveFull 写入完整凭据文档（嵌套形 + 站点标识），用于新建账号。
//
// 同时写 edition（wb-gateway 口径）与 realm（wb2api-panel 口径）两个字段，
// 这样新文件对两个旧版本工具都是可读的，避免用户回退版本后账号池为空。
func (c *Credential) SaveFull() error {
	if c.Path == "" {
		return errors.New("缺少目标路径，无法写入凭据")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  c.AccessToken,
			"refreshToken": c.RefreshToken,
			"expiresAt":    c.ExpiresAt,
			"domain":       c.Domain,
		},
		"account": map[string]any{
			"uid":          c.UID,
			"enterpriseId": c.EnterpriseID,
			"nickname":     c.Nickname,
		},
	}
	if c.Edition != "" {
		doc["edition"] = c.Edition
	}
	if c.Realm != "" {
		doc["realm"] = c.Realm
	}
	if c.DeviceToken != "" {
		doc["device_token"] = c.DeviceToken
	}
	if err := writeDoc(c.Path, doc); err != nil {
		return err
	}
	c.raw = doc
	return nil
}

// writeDoc 以 0600 权限原子写入 JSON 文档（tmp + rename）。
func writeDoc(path string, doc map[string]any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化凭据失败: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建凭据目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("创建临时凭据文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时凭据文件失败: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("设置凭据文件权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时凭据文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("替换凭据文件失败: %w", err)
	}
	return nil
}

// SanitizeFileNameComponent 校验并清洗用于文件名的片段（uid 等），防路径穿越。
// 只放行 [A-Za-z0-9_-]，其余字符替换为下划线，空值返回空串。
func SanitizeFileNameComponent(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// AccountID 返回账号的稳定标识：凭据文件名。
// 单账号操作用它做路径参数，比 uid 可靠（部分凭据没有 uid）。
func (c *Credential) AccountID() string { return filepath.Base(c.Path) }

// ExpiresIn 返回距过期的剩余时长秒数（负数表示已过期）。
func (c *Credential) ExpiresIn(nowUnix int64) int64 {
	if c.ExpiresAt == 0 {
		return 0
	}
	return c.ExpiresAt - nowUnix
}

// Display 返回控制台展示用的账号名。
func (c *Credential) Display() string {
	if strings.TrimSpace(c.Nickname) != "" {
		return c.Nickname
	}
	if strings.TrimSpace(c.UID) != "" {
		return c.UID
	}
	return filepath.Base(c.Path)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
