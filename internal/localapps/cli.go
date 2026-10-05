package localapps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// -----------------------------------------------------------------------------
// CodeBuddy CLI 切换
//
// 把账号池里的某个账号设成 CLI 的当前登录账号。做法是写
// `~/.codebuddy/settings.json` 的 `env.CODEBUDDY_AUTH_TOKEN`，并同步档位标记。
//
// 为什么**不能只写 token**：CLI 启动时会把 `local_storage` 里的
// Environment/Endpoint 缓存读进 process.env，settings.json 缺省时会继续用缓存里的
// 档位 —— 也就是说「token 换成国际站账号了，请求还是打国内站」。
// 所以档位标记与缓存必须一起改（cache 的三个文件名是 CLI 的固定 md5，不能自己算）。
//
// 键值与 wb-switch 的 codebuddy_cli.rs 逐字一致（常量见该文件 26~63 行）。
// -----------------------------------------------------------------------------

const (
	cliSettingsDir = ".codebuddy"

	// 国际站/国内站的档位标记值。**必须写明确值而不是删 key**：
	// 删掉后 CLI 会沿用缓存里的旧档位。
	cliEnvCN = "internal"
	cliEnvAI = "public"

	// CLI 的 OpenAI 兼容基地址。国内站不设置这个键（而是**删除**它）——
	// 与 wb-switch 的行为一致（codebuddy_cli.rs:237 `env.remove(CODEBUDDY_BASE_URL)`）。
	cliBaseURLCN = "https://copilot.tencent.com"
	cliBaseURLAI = "https://www.codebuddy.ai/v2"

	// local_storage 里三个缓存文件名（CLI 用固定 md5(key)，不是按 key 现算的）。
	cliCacheEnvFile      = "entry_3bab4ce61838088127d444e4cc042d6d.info"
	cliCacheEndpointFile = "entry_933d5543e80177622c17a73869c0fad7.info"
	cliCacheProductFile  = "entry_604f48c944053e01d9546675443286c1.info"
)

// cliSettingsPathOf 返回 home 下的 settings.json（便于单测注入临时 home）。
func cliSettingsPathOf(home string) string {
	return filepath.Join(home, cliSettingsDir, "settings.json")
}

func cliStorageDirOf(home string) string {
	return filepath.Join(home, cliSettingsDir, "local_storage")
}

// SwitchCLI 把 acc 设为 CodeBuddy CLI 的当前账号。
//
// 返回的 notes 是「实际做了什么」的逐条记录 —— 界面直接展示它，
// 比一句「切换成功」有用得多（用户能看出档位标记、缓存是否也跟着改了）。
func SwitchCLI(home string, acc PoolAccount) (notes []string, err error) {
	token := cleanToken(acc.AccessToken)
	if token == "" {
		return nil, fmt.Errorf("账号 %s 没有可用的 accessToken", displayName(acc))
	}
	if acc.Site != "cn" && acc.Site != "intl" {
		return nil, fmt.Errorf("账号站点未知（%q），无法决定写国内站还是国际站档位", acc.Site)
	}

	settings := cliSettingsPathOf(home)
	storage := cliStorageDirOf(home)

	// ---- 1. 备份（含缓存文件，因为切换会改它们）----
	backupPaths := []string{
		settings,
		filepath.Join(storage, cliCacheEnvFile),
		filepath.Join(storage, cliCacheEndpointFile),
		filepath.Join(storage, cliCacheProductFile),
	}
	entry, err := BackupFor(TargetCLI, backupPaths, "切换 CodeBuddy CLI 账号到 "+displayName(acc))
	if err != nil {
		return nil, fmt.Errorf("备份失败，已放弃切换（宁可不切也不能写坏）: %w", err)
	}
	notes = append(notes, "已备份 settings.json 与 3 个缓存文件 → "+entry.ID)

	// ---- 2. 读现有 settings.json（保留 hooks 等未知键）----
	doc := map[string]any{}
	if raw, readErr := os.ReadFile(settings); readErr == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("现有 settings.json 不是有效 JSON，已放弃（宁可不动）: %w", err)
		}
	}

	env, _ := doc["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["CODEBUDDY_AUTH_TOKEN"] = token

	if acc.Site == "intl" {
		env["CODEBUDDY_INTERNET_ENVIRONMENT"] = cliEnvAI
		env["CODEBUDDY_BASE_URL"] = cliBaseURLAI
		notes = append(notes, "档位标记：CODEBUDDY_INTERNET_ENVIRONMENT=public、CODEBUDDY_BASE_URL="+cliBaseURLAI)
	} else {
		env["CODEBUDDY_INTERNET_ENVIRONMENT"] = cliEnvCN
		// 国内站要**删除** BASE_URL：留着国际站的值会让 CLI 继续打国际站。
		if _, had := env["CODEBUDDY_BASE_URL"]; had {
			delete(env, "CODEBUDDY_BASE_URL")
			notes = append(notes, "已移除 CODEBUDDY_BASE_URL（国内站不使用该键）")
		}
		notes = append(notes, "档位标记：CODEBUDDY_INTERNET_ENVIRONMENT=internal")
	}
	doc["env"] = env

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := atomicWrite(settings, append(data, '\n')); err != nil {
		return nil, fmt.Errorf("写入 settings.json 失败: %w", err)
	}
	notes = append(notes, fmt.Sprintf("已写入 %s（token %s，其余配置原样保留）",
		settings, maskSecret(token)))

	// ---- 3. 写后回读校验 ----
	// 不能只看「写没报错」：权限、磁盘、别的程序并发改写都会让内容与预期不一致。
	verifyDoc, err := readJSONMap(settings)
	if err != nil {
		return notes, fmt.Errorf("写后无法回读 settings.json（内容可能不完整，请用备份恢复）: %w", err)
	}
	got, _ := cliEnvToken(verifyDoc)
	if cleanToken(got) != token {
		return notes, fmt.Errorf("写后校验失败：settings.json 里的 token 与目标不一致。"+
			"已保留备份 %s，可一键恢复", entry.ID)
	}
	notes = append(notes, "写后校验通过（回读 token 与目标一致）")

	// ---- 4. 同步运行时缓存（仅在目录已存在时）----
	//
	// 目录不存在说明 CLI 从未运行过，此时创建它没有意义（CLI 首次启动会自己建），
	// 而且会在用户目录里凭空多出一个它可能不认识的结构。
	if _, statErr := os.Stat(storage); statErr == nil {
		envValue, endpoint := cliEnvCN, cliBaseURLCN
		if acc.Site == "intl" {
			envValue, endpoint = cliEnvAI, "https://www.codebuddy.ai"
		}
		if err := writeJSONStringCache(filepath.Join(storage, cliCacheEnvFile), envValue); err != nil {
			notes = append(notes, "警告：环境缓存写入失败 "+err.Error())
		}
		if err := writeJSONStringCache(filepath.Join(storage, cliCacheEndpointFile), endpoint); err != nil {
			notes = append(notes, "警告：端点缓存写入失败 "+err.Error())
		}
		// 产品包缓存按档位区分（product.internal.json / product.json），
		// 切档位必须丢掉它，否则会用到另一档的产品包定义。
		productPath := filepath.Join(storage, cliCacheProductFile)
		if err := os.Remove(productPath); err != nil && !os.IsNotExist(err) {
			notes = append(notes, "警告：产品包缓存未删除 "+err.Error())
		} else {
			notes = append(notes, "已同步档位缓存并清除产品包缓存")
		}
	} else {
		notes = append(notes, "未发现 local_storage 目录（CLI 尚未运行过），跳过档位缓存同步")
	}

	return notes, nil
}

// writeJSONStringCache 按 CLI 的格式写缓存：**JSON 字符串字面量**（带引号）。
//
// 写成裸字符串（`internal` 而不是 `"internal"`）会让 CLI 解析失败并静默回落，
// 表现是「设置看起来都对，但档位没变」。
func writeJSONStringCache(path, value string) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

// CLIStatus 汇总 CLI 的当前状态，供界面展示。
type CLIStatus struct {
	// EnvToken 是 settings.json 里当前的 token（已脱敏）。
	EnvTokenMasked string `json:"env_token_masked"`
	// EnvTokenSet 表示 settings.json 里有没有这个键。
	EnvTokenSet bool `json:"env_token_set"`
	// InternetEnv 是档位标记的当前值。
	InternetEnv string `json:"internet_env"`
	// BaseURL 是 CODEBUDDY_BASE_URL 的当前值（空表示未设置）。
	BaseURL string `json:"base_url"`
	// HasStorage 表示 local_storage 目录是否存在（缓存是否需要同步）。
	HasStorage bool `json:"has_storage"`
	// Hooks 是 settings.json 里保留的其他配置的键名（提示用户我们没动它们）。
	OtherKeys []string `json:"other_keys"`
}

// ReadCLIStatus 读取 CLI 当前配置。
func ReadCLIStatus(home string) (CLIStatus, error) {
	var st CLIStatus
	doc, err := readJSONMap(cliSettingsPathOf(home))
	if err != nil {
		return st, err
	}
	st.OtherKeys = []string{}
	for k := range doc {
		if k != "env" {
			st.OtherKeys = append(st.OtherKeys, k)
		}
	}
	if env, ok := doc["env"].(map[string]any); ok {
		if v, ok := env["CODEBUDDY_AUTH_TOKEN"].(string); ok && strings.TrimSpace(v) != "" {
			st.EnvTokenSet = true
			st.EnvTokenMasked = maskSecret(v)
		}
		if v, ok := env["CODEBUDDY_INTERNET_ENVIRONMENT"].(string); ok {
			st.InternetEnv = v
		}
		if v, ok := env["CODEBUDDY_BASE_URL"].(string); ok {
			st.BaseURL = v
		}
	}
	if _, err := os.Stat(cliStorageDirOf(home)); err == nil {
		st.HasStorage = true
	}
	return st, nil
}
