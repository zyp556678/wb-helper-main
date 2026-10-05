package localapps

import (
	"os"
	"path/filepath"
	"strings"
)

// -----------------------------------------------------------------------------
// 与 wb-switch 的共存冲突检测
//
// 为什么需要它：本机可能**同时装着 wb-switch**，而它管的账号与我们同一批、
// 写的是**同一批文件**（`~/.codebuddy/settings.json` 与 WorkBuddy 客户端的
// 认证文件）。两个工具写同一目标 = **后写覆盖先写**，
// 表现是「我明明切了，一会儿又变回去了」——
// 这类问题用户几乎不可能自己归因，所以必须主动检测并说清。
//
// 本包**不做**「抢占/加锁」：那需要一个双方都遵守的协议，而 wb-switch 不认识我们。
// 能做的、也足够的是：把冲突**指出来**，并给出可执行的处置办法。
// -----------------------------------------------------------------------------

// SwitchConflict 描述与 wb-switch 的共存情况。
type SwitchConflict struct {
	// Present 表示检测到 wb-switch 的数据目录。
	Present bool `json:"present"`
	// Root 是它的数据目录（展示给用户便于自查）。
	Root string `json:"root"`
	// HookActive 表示 CodeBuddy CLI 的 settings.json 里挂的 hooks 指向 wb-switch。
	// 为真时它的 hook 会在 CLI 停止时参与写/改登录相关状态。
	HookActive bool `json:"hook_active"`
	// HookCommands 是命中的 hook 命令原文（脱敏后展示，便于用户确认）。
	HookCommands []string `json:"hook_commands"`
	// Contended 列出双方都会写的文件 —— 这才是冲突的实质。
	Contended []string `json:"contended"`
	// Detail 是给用户看的说明与处置建议。
	Detail string `json:"detail"`
}

// DetectSwitchConflict 检测 wb-switch 是否共存、是否与本工具争抢同一批文件。
func DetectSwitchConflict() SwitchConflict {
	var c SwitchConflict
	c.Contended = []string{}
	c.HookCommands = []string{}

	home := homeDir()
	if home == "" {
		return c
	}

	// 1. 数据目录是否存在（这是最直接的证据）。
	root := filepath.Join(home, ".wb-switch")
	if st, err := os.Stat(root); err == nil && st.IsDir() {
		c.Present = true
		c.Root = root
	}

	// 2. settings.json 里的 hooks 是否指向它。
	//
	// 这一条比「目录存在」更重要：目录在只说明装过，
	// **hook 挂在配置里才说明它现在还会动手**。
	settings := cliSettingsPathOf(home)
	if doc, err := readJSONMap(settings); err == nil {
		c.HookCommands = collectHookCommands(doc)
		for _, cmd := range c.HookCommands {
			if strings.Contains(strings.ToLower(cmd), "wb-switch") {
				c.HookActive = true
				c.Present = true
				break
			}
		}
	}

	if !c.Present {
		return c
	}

	// 3. 争抢的文件：本工具会写的、且 wb-switch 也写的那几个。
	candidates := []string{
		settings,
		workBuddyAuthPath("cn"),
		workBuddyAuthPath("intl"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			c.Contended = append(c.Contended, p)
		}
	}

	c.Detail = buildConflictDetail(c)
	return c
}

// buildConflictDetail 生成给用户看的说明。**必须给出可执行的下一步**，
// 只说「有冲突」等于把问题丢回给用户。
func buildConflictDetail(c SwitchConflict) string {
	var b strings.Builder
	b.WriteString("检测到本机还装着 wb-switch")
	if c.Root != "" {
		b.WriteString("（" + c.Root + "）")
	}
	b.WriteString("，它管理的账号与本工具是同一批，并且会写同一批文件。")
	if c.HookActive {
		b.WriteString("其中 CodeBuddy CLI 的 settings.json 里挂的 hooks 指向 wb-switch，")
		b.WriteString("**它的 hook 会在 CLI 停止时参与改写登录相关状态**。")
	}
	b.WriteString("两个工具写同一目标时后写的会覆盖先写的，表现为「切了之后又变回去」。")
	b.WriteString("建议二选一：要么停用 wb-switch 的 hook（从 settings.json 的 hooks 里移除），")
	b.WriteString("要么固定只用其中一个工具做切换。")
	return b.String()
}

// collectHookCommands 把 settings.json 里 hooks 的 command 全捞出来。
//
// 结构是嵌套的（hooks.<Event>[].hooks[].command），而且**事件名与层数都不固定**
// （本机见到 FinalStop / Stop），所以这里做**无差别深度遍历**而不是按已知键取 ——
// 硬编码键名会在 switch 加一个新事件时静默漏检。
func collectHookCommands(v any) []string {
	var out []string
	var walk func(any)
	walk = func(node any) {
		switch t := node.(type) {
		case map[string]any:
			for k, val := range t {
				if k == "command" {
					if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
						out = append(out, strings.TrimSpace(s))
					}
					continue
				}
				walk(val)
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(v)
	return out
}
