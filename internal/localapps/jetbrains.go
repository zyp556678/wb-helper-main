package localapps

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// -----------------------------------------------------------------------------
// JetBrains 插件切换
//
// 目标文件：`<配置目录>/options/secret-storage.xml`
//
// **格式必须与 JetBrains 平台真实写盘形态逐字一致**（实测样本，照抄 wb-switch
// 的结论）：
//
//	<?xml version="1.0" encoding="UTF-8"?>
//	<application>
//	  <component name="SecretStorage">
//	    <Scores>
//	      <Entry key="..." value="..." />
//	    </Scores>
//	  </component>
//	</application>
//
// 为什么不能「按反编译代码写成裸 <MapStorage> 顶层」：SecretStorage 的
// getState() 返回的 MapStorage 根**会被平台展平**，组件文件里没有这一层。
// 写成裸 MapStorage 顶层时，平台读回时**整体丢弃** —— 表现为「写入的 key 消失、
// 插件显示未登录」，而且不报任何错。这是最难自查的一类失败。
//
// 为什么手写 XML 解析/序列化而不用 encoding/xml：这个文件的**键顺序、缩进、
// 自闭合写法**都是平台自己产出的固定形态；用通用编码器重排一遍会产生
// 「语义等价但平台可能不认」的差异，而这个文件我们改完还要给平台读。
// 只做「定位 <Entry 并 upsert」，其余字节原样保留，风险最小。
// -----------------------------------------------------------------------------

const (
	// jetbrainsSecretKey 是插件在 SecretStorage 里的固定键。
	jetbrainsSecretKey = "Tencent-Cloud.coding-copilot.new.accessToken"
	// jetbrainsSecretRel 是相对配置目录的路径。
	jetbrainsSecretRel = "options/secret-storage.xml"
	// jetbrainsPluginPrefix 是插件目录名前缀（实测 `coding-copilot*`）。
	jetbrainsPluginPrefix = "coding-copilot"
)

// JetBrainsConfigDir 是一个可写的 JetBrains 配置目录。
type JetBrainsConfigDir struct {
	// Name 是配置目录名（形如 `IntelliJIdea2026.2`）。
	Name string `json:"name"`
	// Dir 是绝对路径。
	Dir string `json:"dir"`
	// PluginDir 是探测到的插件目录名。
	PluginDir string `json:"plugin_dir"`
	// SecretPath 是 secret-storage.xml 的路径。
	SecretPath string `json:"secret_path"`
	// HasSecret 表示该文件当前是否存在。
	HasSecret bool `json:"has_secret"`
	// CurrentToken 是当前存着的 token（已脱敏）。
	CurrentTokenMasked string `json:"current_token_masked"`
}

// jetbrainsRoot 返回 JetBrains 配置根目录。
func jetbrainsRoot() string {
	return filepath.Join(roamingAppData(), "JetBrains")
}

// FindJetBrainsDirs 找出「装了 CodeBuddy 插件」的 JetBrains 配置目录。
//
// **必须查插件目录**：只看到 IDE 就报「已检测到」会让用户点了才发现插件没装。
// 多个 IDE（IDEA / PyCharm / ...）各自一个配置目录，全部返回。
func FindJetBrainsDirs() []JetBrainsConfigDir {
	root := jetbrainsRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := []JetBrainsConfigDir{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		pluginDir := findJetBrainsPlugin(dir)
		if pluginDir == "" {
			continue
		}
		secret := filepath.Join(dir, filepath.FromSlash(jetbrainsSecretRel))
		item := JetBrainsConfigDir{
			Name:       e.Name(),
			Dir:        dir,
			PluginDir:  pluginDir,
			SecretPath: secret,
		}
		if _, err := os.Stat(secret); err == nil {
			item.HasSecret = true
			if tok, err := readJetBrainsSecret(secret); err == nil && tok != "" {
				item.CurrentTokenMasked = maskSecret(tok)
			}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// findJetBrainsPlugin 在 `plugins/` 下找 coding-copilot* 目录。
func findJetBrainsPlugin(configDir string) string {
	entries, err := os.ReadDir(filepath.Join(configDir, "plugins"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(strings.ToLower(e.Name()), jetbrainsPluginPrefix) {
			return e.Name()
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// secret-storage.xml 读写
// -----------------------------------------------------------------------------

// readJetBrainsSecret 读目标 key 的值（不存在返回空串）。
func readJetBrainsSecret(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, e := range parseSecretEntries(string(raw)) {
		if e.Key == jetbrainsSecretKey {
			return e.Value, nil
		}
	}
	return "", nil
}

// secretEntry 是 XML 里的一条 Entry。
type secretEntry struct {
	Key   string
	Value string
}

// parseSecretEntries 解析全部 Entry，**保持文件顺序**。
//
// 只认 `<Entry ... />` 的行内属性；非目标结构返回空表（上层按「没有该 key」处理，
// 于是会走「新建文件」路径 —— 那会覆盖用户原有内容，所以调用方在写之前
// **必须**先备份，见 SwitchJetBrains）。
func parseSecretEntries(content string) []secretEntry {
	var out []secretEntry
	rest := content
	for {
		pos := strings.Index(rest, "<Entry ")
		if pos < 0 {
			break
		}
		rest = rest[pos:]
		end := strings.Index(rest, "/>")
		if end < 0 {
			break
		}
		frag := rest[:end+2]
		key, okK := entryAttr(frag, "key")
		val, okV := entryAttr(frag, "value")
		if okK && okV {
			out = append(out, secretEntry{Key: key, Value: val})
		}
		rest = rest[end+2:]
	}
	return out
}

// entryAttr 从一段 `<Entry ... />` 里取属性值（已反转义）。
func entryAttr(fragment, name string) (string, bool) {
	needle := name + `="`
	pos := strings.Index(fragment, needle)
	if pos < 0 {
		return "", false
	}
	rest := fragment[pos+len(needle):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return "", false
	}
	return xmlUnescape(rest[:end]), true
}

// xmlEscapeAttr 按平台口径转义属性值。
//
// **换行/回车/制表符必须转成字符引用**：XML 属性值里的裸换行会被解析器
// 规范化成空格，token 里一旦有这类字符就会被静默改坏。
func xmlEscapeAttr(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 8)
	for _, r := range v {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		case '\n':
			b.WriteString("&#10;")
		case '\r':
			b.WriteString("&#13;")
		case '\t':
			b.WriteString("&#9;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// xmlUnescape 反转义属性值（含数字字符引用）。
func xmlUnescape(v string) string {
	if !strings.Contains(v, "&") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); {
		if v[i] != '&' {
			b.WriteByte(v[i])
			i++
			continue
		}
		semi := strings.IndexByte(v[i:], ';')
		if semi < 0 || semi > 12 {
			b.WriteByte('&')
			i++
			continue
		}
		ent := v[i+1 : i+semi]
		var decoded string
		switch ent {
		case "amp":
			decoded = "&"
		case "lt":
			decoded = "<"
		case "gt":
			decoded = ">"
		case "quot":
			decoded = `"`
		case "apos":
			decoded = "'"
		default:
			if n, err := parseCharRef(ent); err == nil {
				decoded = n
			}
		}
		if decoded == "" {
			b.WriteByte('&')
			i++
			continue
		}
		b.WriteString(decoded)
		i += semi + 1
	}
	return b.String()
}

// parseCharRef 解析 `#123` / `#x1F` 形式的字符引用。
func parseCharRef(ent string) (string, error) {
	if !strings.HasPrefix(ent, "#") {
		return "", fmt.Errorf("not a char ref")
	}
	body := ent[1:]
	base := 10
	if len(body) > 0 && (body[0] == 'x' || body[0] == 'X') {
		base = 16
		body = body[1:]
	}
	if body == "" {
		return "", fmt.Errorf("empty char ref")
	}
	var n int64
	for _, c := range body {
		var d int64
		switch {
		case c >= '0' && c <= '9':
			d = int64(c - '0')
		case base == 16 && c >= 'a' && c <= 'f':
			d = int64(c-'a') + 10
		case base == 16 && c >= 'A' && c <= 'F':
			d = int64(c-'A') + 10
		default:
			return "", fmt.Errorf("bad digit %q", c)
		}
		n = n*int64(base) + d
		if n > 0x10FFFF {
			return "", fmt.Errorf("out of range")
		}
	}
	return string(rune(n)), nil
}

// serializeSecretEntries 按平台形态序列化。
//
// **外层 `<application><component name="SecretStorage"><Scores>` 是必需的**，
// 理由见文件顶部注释（写成裸 MapStorage 顶层会被平台整体丢弃）。
func serializeSecretEntries(entries []secretEntry) string {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<application>\n")
	b.WriteString("  <component name=\"SecretStorage\">\n    <Scores>\n")
	for _, e := range entries {
		b.WriteString("      <Entry key=\"")
		b.WriteString(xmlEscapeAttr(e.Key))
		b.WriteString("\" value=\"")
		b.WriteString(xmlEscapeAttr(e.Value))
		b.WriteString("\" />\n")
	}
	b.WriteString("    </Scores>\n  </component>\n</application>\n")
	return b.String()
}

// upsertSecret 在内容里 upsert 目标 key，返回新内容与被替换的旧值。
//
// 已有该 key → **原位替换**（保持位置，减少与平台写盘的差异）；
// 没有 → 追加到末尾。
func upsertSecret(content, key, value string) (string, string, bool) {
	entries := parseSecretEntries(content)
	replaced := ""
	found := false
	for i := range entries {
		if entries[i].Key == key {
			replaced = entries[i].Value
			entries[i].Value = value
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, secretEntry{Key: key, Value: value})
	}
	return serializeSecretEntries(entries), replaced, found
}

// -----------------------------------------------------------------------------
// 切换
// -----------------------------------------------------------------------------

// SwitchJetBrainsResult 是一次切换的结果。
type SwitchJetBrainsResult struct {
	// Dirs 是逐目录的处理结果说明。
	Notes []string `json:"notes"`
	// BackupID 是备份标识（可一键恢复）。
	BackupID string `json:"backup_id"`
	// Written 是实际写入的目录数。
	Written int `json:"written"`
}

// SwitchJetBrains 把 acc 的 token 写进所有装了 CodeBuddy 插件的 JetBrains 配置目录。
//
// 全流程：备份 → 逐目录 upsert → 回读校验。
// **备份走统一的备份设施**（而不是像 wb-switch 那样在文件旁留 `.wb-switch-bak`）：
// 统一的备份能列出、能一键恢复，散落各处的 `.bak` 用户根本找不到。
func SwitchJetBrains(acc PoolAccount) (*SwitchJetBrainsResult, error) {
	token := cleanToken(acc.AccessToken)
	if token == "" {
		return nil, fmt.Errorf("账号 %s 没有可用的 accessToken", displayName(acc))
	}
	dirs := FindJetBrainsDirs()
	if len(dirs) == 0 {
		return nil, fmt.Errorf("未找到装了 CodeBuddy 插件的 JetBrains 配置目录" +
			"（需要先在 IDE 里安装插件并至少启动过一次）")
	}

	res := &SwitchJetBrainsResult{Notes: []string{}}

	// ---- 1. 备份（含尚不存在的路径，恢复时会把它们删掉）----
	paths := make([]string, 0, len(dirs))
	for _, d := range dirs {
		paths = append(paths, d.SecretPath)
	}
	entry, err := BackupFor(TargetJetBrains, paths, "切换 JetBrains 插件账号到 "+displayName(acc))
	if err != nil {
		return nil, fmt.Errorf("备份失败，已放弃切换: %w", err)
	}
	res.BackupID = entry.ID
	res.Notes = append(res.Notes, "已备份 "+fmt.Sprint(len(dirs))+" 个配置目录的 secret-storage.xml → "+entry.ID)

	// ---- 2. 逐目录写入 ----
	for _, d := range dirs {
		content := ""
		if raw, err := os.ReadFile(d.SecretPath); err == nil {
			content = string(raw)
		} else if !os.IsNotExist(err) {
			return res, fmt.Errorf("读取 %s 失败: %w", d.SecretPath, err)
		} else {
			content = serializeSecretEntries(nil) // 新建时也要带完整外壳
		}

		next, replaced, found := upsertSecret(content, jetbrainsSecretKey, token)
		if err := atomicWrite(d.SecretPath, []byte(next)); err != nil {
			return res, fmt.Errorf("写入 %s 失败: %w", d.SecretPath, err)
		}

		// ---- 3. 回读校验 ----
		got, err := readJetBrainsSecret(d.SecretPath)
		if err != nil {
			return res, fmt.Errorf("写后无法回读 %s（请用备份 %s 恢复）: %w", d.SecretPath, entry.ID, err)
		}
		if got != token {
			return res, fmt.Errorf("写后校验失败：%s 里的 token 与目标不一致。请用备份 %s 恢复",
				d.SecretPath, entry.ID)
		}
		res.Written++
		action := "新增"
		if found {
			action = "替换（旧值 " + maskSecret(replaced) + "）"
		}
		res.Notes = append(res.Notes, fmt.Sprintf("%s：%s %s", d.Name, action, jetbrainsSecretKey))
	}

	res.Notes = append(res.Notes, fmt.Sprintf("写后校验通过（%d 个目录的 token 均与目标一致）", res.Written))
	res.Notes = append(res.Notes, "提示：需要重启对应的 JetBrains IDE 才会读取新的登录态")
	return res, nil
}
