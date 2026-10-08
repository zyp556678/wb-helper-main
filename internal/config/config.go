// Package config 负责网关运行配置：命令行选项 + 工作目录 config.json 覆盖。
//
// 配置分两层：
//   - 启动层（Addr/Port/APIKey/凭据来源）：只能由命令行指定，改动需重启。
//   - 运行层（上游超时/调试日志/模型黑白名单）：可由工作目录 config.json 覆盖，
//     属于装配期字段，同样需要重启生效；后续切片会补齐面板在线改配置的热生效。
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 默认超时（与 wb-gateway 保持一致，反代层以它的实现为基准）。
const (
	// DefaultUpstreamHeaderTimeout 限定「请求发送完成 → 收到上游响应头」的等待上限。
	// 上游对 3MB+ 大请求的排队与预处理本身可能接近 1 分钟，早期 60s 会误杀正常慢请求。
	DefaultUpstreamHeaderTimeout = 5 * time.Minute

	// DefaultUpstreamIdleTimeout 是流式响应的空闲读超时：只要持续有数据就永不超时，
	// 超过该时长没有任何新数据才判定上游卡死并中断。
	DefaultUpstreamIdleTimeout = 120 * time.Second

	// DefaultControlTimeout 用于令牌刷新、模型目录等控制类短请求。
	DefaultControlTimeout = 60 * time.Second

	// DefaultTransientRetries 是「请求体发送阶段」遇到瞬时网络错误时的额外重试次数。
	//
	// 背景：网关与上游 CDN 边缘节点之间的单条 TCP 连接可能被对端重置
	// （connection reset by peer）、被关闭（use of closed network connection），
	// 或命中已被回收的 keep-alive 连接。这类错误属于瞬时故障，与请求体大小无关
	// （实测 >5MB 请求 95% 成功，而 0.5MB 请求也会偶发失败）。
	//
	// 只有确认**请求头尚未写出**时才重试：没有收到响应头不能证明上游没处理 POST，
	// 重放一个可能已经执行过的生成会重复计费。见 upstream.doChatWithRetry。
	DefaultTransientRetries = 2

	// DefaultTransientRetryBackoff 是两次重试之间的等待，给上游边缘节点留出恢复时间。
	DefaultTransientRetryBackoff = 300 * time.Millisecond
)

// DefaultServerReadTimeout 是入站请求读取（含 body 上传）总时长的默认上限。
//
// 旧实现硬编码 120s：大上下文 / 文件块请求经反代链转发时，慢速上传会被掐成
// 400 "read body: ... i/o timeout"，而客户端看不出是网关掐的。参考实现把该值
// 提到 300s 并开放配置（issue #100）；"0" = 不限制（慢速 body 可无限占用连接，
// 自担风险），负值/不可解析一律拒绝启动。
const DefaultServerReadTimeout = "300s"

// ServerSection 是入站 HTTP 服务参数。
type ServerSection struct {
	// ReadTimeout 是入站请求读取（含 body 上传）总时长上限，形如 "300s"。
	// 空值回落 300s；"0" = 不限制；负值无语义（静默钳 0 会把保护悄悄关掉）。
	// 该字段属装配期：http.Server 只在启动时构造，改动需重启进程。
	ReadTimeout string `json:"read_timeout"`
}

// runtimeFile 是 config.json 的结构（工作目录下可选）。
type runtimeFile struct {
	Upstream struct {
		// 两种拼写都接受：面板保存时写 snake_case（与 /panel/api/config 的契约一致），
		// 而人工手写的 config.json 一直是 camelCase。只认一种会让另一侧配的值
		// 在重启后被静默忽略——这类「保存了但没生效」最难排查。snake 优先。
		HeaderTimeoutSeconds      int `json:"headerTimeoutSeconds"`
		IdleTimeoutSeconds        int `json:"idleTimeoutSeconds"`
		HeaderTimeoutSecondsSnake int `json:"header_timeout_seconds"`
		IdleTimeoutSecondsSnake   int `json:"idle_timeout_seconds"`
		// TransientRetries 是瞬时网络错误重试次数；**显式 0 表示禁用**，
		// 所以不能沿用 firstPositive（那会把 0 当成「未配置」而落回默认值）。
		TransientRetries      *int `json:"transientRetries"`
		TransientRetriesSnake *int `json:"transient_retries"`
	} `json:"upstream"`
	Debug struct {
		Enabled bool `json:"enabled"`
	} `json:"debug"`
	Models struct {
		Blocklist []string `json:"blocklist"`
		Allowlist []string `json:"allowlist"`
		// Accounts 是**模型专属**的凭据文件黑白名单，与上面的模型黑白名单
		// （控制模型能否调用）互不替代：这里控制的是「这个模型能用哪些账号」。
		Accounts map[string]struct {
			Allowlist []string `json:"allowlist"`
			Blocklist []string `json:"blocklist"`
		} `json:"accounts"`
	} `json:"models"`

	// 切片 3 新增
	ModelsNPM   *bool `json:"models_npm"`
	ModelsProbe *bool `json:"models_probe"`

	// 切片 2 新增
	Cooldown      CooldownSection `json:"cooldown"`
	Pool          PoolSection     `json:"pool"`
	SessionSticky StickySection   `json:"session_sticky"`
	Schedule      ScheduleSection `json:"schedule"`
	Prompt        PromptSection   `json:"prompt"`
	Local         LocalSection    `json:"local"`
	Tasks         TasksSection    `json:"tasks"`

	// Server 是入站 HTTP 服务参数（目前只有读取上限）。
	Server ServerSection `json:"server"`

	// Logging 是请求级观测的归档开关（内存指标始终启用）。
	Logging struct {
		// RequestArchiveEnabled 请求元数据 JSONL 归档开关，缺省 true。
		RequestArchiveEnabled *bool `json:"request_archive_enabled"`
		// RequestRetentionDays 归档保留天数，缺省 7。
		RequestRetentionDays int `json:"request_retention_days"`
		// RequestArchiveMaxMB 归档总上限（MiB），缺省 100。
		RequestArchiveMaxMB int `json:"request_archive_max_mb"`
	} `json:"logging"`
}

// Clone 返回配置的一份独立副本，供「写时复制」使用。
//
// 为什么需要它：面板保存配置是**写**，而 chat 热路径在**读**同一个 *Config。
// 原地改写会让读侧看到撕裂的中间态 —— slice header 是三个字、string 是两个字，
// 并发读写时都可能被拆开读到（`go test -race` 会直接报）。
// 改成「克隆 → 在克隆上改 → 原子换指针」后，读侧永远看到某个**完整版本**。
//
// 只做浅拷贝 + 顶层引用类型的复制：applyTo / ApplyDefaults 都是**整体赋值**
// （`c.ModelBlock = patch.Models.Blocklist`），不会原地改已有切片；
// 这里复制一份是为了让「旧配置」彻底冻结 —— 将来若有人加了原地改的代码，
// 也不会污染正在被并发读取的那份。
//
// 刻意**不复制** Control / Chat（*http.Client）：连接池本就该共享，
// 复制会凭空多出一套连接。
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	dst := *c
	dst.ModelBlock = append([]string(nil), c.ModelBlock...)
	dst.ModelAllow = append([]string(nil), c.ModelAllow...)
	if c.ModelAccountRules != nil {
		rules := make(map[string]ModelAccountRule, len(c.ModelAccountRules))
		for k, v := range c.ModelAccountRules {
			rules[k] = v
		}
		dst.ModelAccountRules = rules
	}
	return &dst
}

// ModelAccountRule 是某个模型的凭据文件黑白名单。
//
// 语义（与 models.allowlist/blocklist 完全不同，别混）：
//   - models.allowlist/blocklist 控制「这个模型能不能被调用」；
//   - ModelAccountRule 控制「这个模型能用哪些**账号**」。
//
// 判定顺序：黑名单优先；白名单为空表示不限制。只接受凭据**文件名**
// （如 intl-a.json），不接受路径或通配符 —— 配了路径却按文件名匹配，
// 会让规则静默失效。
type ModelAccountRule struct {
	Allow map[string]bool
	Block map[string]bool
}

// Configured 表示该规则是否真的设置了名单（两个都空等于没配）。
func (r ModelAccountRule) Configured() bool {
	return len(r.Allow) > 0 || len(r.Block) > 0
}

// ModelAccountRuleFor 取某模型的账号名单规则。
func (c *Config) ModelAccountRuleFor(model string) (ModelAccountRule, bool) {
	if len(c.ModelAccountRules) == 0 {
		return ModelAccountRule{}, false
	}
	r, ok := c.ModelAccountRules[normalizeModelKey(model)]
	return r, ok
}

// normalizeModelKey 是模型名归一化（小写 + 去首尾空格），用作规则表的键。
func normalizeModelKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// Config 是网关的完整运行配置。
type Config struct {
	setFlags

	// ---- 启动层 ----
	Addr   string // 监听地址，默认 127.0.0.1
	Port   int    // 监听端口，默认 8317
	APIKey string // 为空表示不校验 Bearer
	// AdminKey 是**管理面板专用**的密钥。为空时面板沿用 APIKey（向后兼容：
	// 老部署与桌面壳零改动）。配了它，就能把模型 Key 分发给别人而不连带管理面。
	AdminKey     string
	AuthFile     string // 显式指定的凭据文件（可逗号分隔多个）
	AuthDir      string // 凭据目录：加载目录下所有凭据文件
	AuthExplicit bool   // 用户是否显式指定了 -auth
	ProxyURL     string // 上游出口代理
	Verbose      bool

	// ---- 运行层（config.json 可覆盖）----
	ReloadInterval int // 凭据热加载扫描间隔（秒），0 关闭
	ModelsRefresh  int // 模型目录刷新间隔（分钟），0 关闭

	HeaderTimeout time.Duration
	IdleTimeout   time.Duration
	// TransientRetries 是瞬时网络错误重试次数（0 表示禁用）。
	TransientRetries int
	// TransientRetryBackoff 是重试间隔（测试可调小）。
	TransientRetryBackoff time.Duration
	DebugEnabled          bool
	ModelBlock            []string
	ModelAllow            []string
	// ModelAccountRules 是「模型 → 可用凭据文件」名单，键为小写去空格的模型名。
	// 未配置的模型不在表里，视为不限制。
	ModelAccountRules map[string]ModelAccountRule

	// ---- 请求级观测（切片：reqlog）----
	// 内存指标（最近 100 条 + 进程级计数）始终启用，归档是可选增强。
	RequestArchiveEnabled bool
	RequestRetentionDays  int
	RequestArchiveMaxMB   int

	// ---- 治理与调度（切片 2，可由面板热改）----
	Cooldown CooldownSection
	Pool     PoolSection
	Sticky   StickySection
	Schedule ScheduleSection
	Prompt   PromptSection
	Local    LocalSection
	Tasks    TasksSection
	// Server 是入站 HTTP 服务参数：**装配期字段**，改动需重启进程
	// （http.Server 只在启动时构造一次）。
	Server ServerSection
	// ServerReadTimeout 是 Server.ReadTimeout 解析后的时长（0 = 不限制）。
	ServerReadTimeout time.Duration

	scheduleErr error
	serverErr   error
	poolPresent bool
	// ModelsNPM / ModelsProbe 控制目录的两项可选行为（nil = 默认启用）。
	ModelsNPM   *bool
	ModelsProbe *bool

	// ---- 运行期装配 ----
	WorkDir    string
	ConfigFile string
	Control    *http.Client // 控制类短请求（有总时长上限）
	Chat       *http.Client // 对话流（无总时长上限，只受空闲看门狗约束）
}

// Load 构建配置：opt 由命令行填充，随后读取工作目录 config.json 覆盖运行层。
// workDir 为空时使用当前工作目录。
func Load(opt *Config) (*Config, error) {
	cfg := opt
	if cfg.AuthFile == "" {
		cfg.AuthFile = "workbuddy.json"
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 8317
	}
	if cfg.ReloadInterval == 0 && !opt.reloadSet {
		cfg.ReloadInterval = 5
	}
	if cfg.ModelsRefresh == 0 && !opt.modelsSet {
		cfg.ModelsRefresh = 60
	}

	dir := cfg.WorkDir
	if dir == "" {
		// 数据目录优先取 WB_GATEWAY_DATA_DIR：桌面壳与 npm 入口都用它把凭据、config.json、
		// 模型缓存、统计固定到同一处（Windows 为 %LOCALAPPDATA%\wb-gateway），
		// 否则「启动时恰好 cd 到哪儿」就换一份账号池 —— 这是最难自查的一类问题。
		//
		// 不读这个变量的代价是实打实的：壳启动后端时把子进程 cwd 设成了自己的 bin 目录，
		// 于是桌面版把 <app>\bin 当成数据目录，扫不到任何凭据、账号池恒为空，
		// 面板上的账号数、配额、官方积分/用量全都是空的。
		dir = strings.TrimSpace(os.Getenv("WB_GATEWAY_DATA_DIR"))
	}
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	cfg.WorkDir = dir
	cfg.ConfigFile = filepath.Join(dir, "config.json")

	// 默认值
	cfg.HeaderTimeout = DefaultUpstreamHeaderTimeout
	cfg.IdleTimeout = DefaultUpstreamIdleTimeout
	cfg.TransientRetries = DefaultTransientRetries
	cfg.TransientRetryBackoff = DefaultTransientRetryBackoff
	// 请求归档默认开：只写脱敏元数据，且队列满会丢弃而非阻塞请求，
	// 所以「默认开」的风险很低，而默认关会让排障时没有历史可查。
	cfg.RequestArchiveEnabled = true
	cfg.RequestRetentionDays = 7
	cfg.RequestArchiveMaxMB = 100

	if raw, err := os.ReadFile(cfg.ConfigFile); err == nil {
		var f runtimeFile
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, errors.New("config.json 解析失败: " + err.Error())
		}
		// 记录哪些段在文件里真实出现过：max_in_flight 的 0 是合法值（表示不限），
		// 因此必须区分「未配置」与「显式配成 0」，否则默认值无法正确落地。
		var rawMap map[string]any
		_ = json.Unmarshal(raw, &rawMap)
		cfg.poolPresent = hasNestedKey(rawMap, "pool", "max_in_flight")
		if sec := firstPositive(f.Upstream.HeaderTimeoutSecondsSnake, f.Upstream.HeaderTimeoutSeconds); sec > 0 {
			cfg.HeaderTimeout = time.Duration(sec) * time.Second
		}
		if sec := firstPositive(f.Upstream.IdleTimeoutSecondsSnake, f.Upstream.IdleTimeoutSeconds); sec > 0 {
			cfg.IdleTimeout = time.Duration(sec) * time.Second
		}
		// 显式 0 是合法值（禁用重试），所以先取 snake 再取 camel，两者都为 nil 才用默认。
		if n := firstPtr(f.Upstream.TransientRetriesSnake, f.Upstream.TransientRetries); n != nil && *n >= 0 {
			cfg.TransientRetries = *n
		}
		cfg.DebugEnabled = f.Debug.Enabled
		cfg.ModelBlock = f.Models.Blocklist
		cfg.ModelAllow = f.Models.Allowlist
		cfg.ModelAccountRules = parseModelAccountRules(f.Models.Accounts)
		// 归档开关用指针：显式 false（关掉归档）与「没配」必须区分开，
		// 否则用户明确关掉的归档会在下次启动被默认值重新打开。
		if f.Logging.RequestArchiveEnabled != nil {
			cfg.RequestArchiveEnabled = *f.Logging.RequestArchiveEnabled
		}
		if f.Logging.RequestRetentionDays > 0 {
			cfg.RequestRetentionDays = f.Logging.RequestRetentionDays
		}
		if f.Logging.RequestArchiveMaxMB > 0 {
			cfg.RequestArchiveMaxMB = f.Logging.RequestArchiveMaxMB
		}
		cfg.Cooldown = f.Cooldown
		cfg.Pool = f.Pool
		cfg.Sticky = f.SessionSticky
		cfg.Schedule = f.Schedule
		cfg.Server = f.Server
		cfg.Prompt = f.Prompt
		cfg.Tasks = f.Tasks
		cfg.Local = f.Local
		cfg.ModelsNPM = f.ModelsNPM
		cfg.ModelsProbe = f.ModelsProbe
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("读取 config.json 失败: " + err.Error())
	}

	cfg.ApplyDefaults()
	if err := cfg.ScheduleErr(); err != nil {
		return nil, err
	}
	if err := cfg.ServerErr(); err != nil {
		return nil, err
	}

	if err := cfg.buildClients(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// buildClients 装配两个 http.Client：控制类有总时长上限，对话流不设总时长。
// 两者共用同一 Transport，因此上游超时策略只有一份实现。
func (c *Config) buildClients() error {
	transport := &http.Transport{
		// 响应头等待上限：只约束「发出请求 → 收到响应头」，不约束流式响应体。
		ResponseHeaderTimeout: c.HeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	}
	if strings.TrimSpace(c.ProxyURL) != "" {
		u, err := url.Parse(c.ProxyURL)
		if err != nil {
			return errors.New("无效的代理地址 " + c.ProxyURL + ": " + err.Error())
		}
		transport.Proxy = http.ProxyURL(u)
	}
	c.Control = &http.Client{Transport: transport, Timeout: DefaultControlTimeout}
	// 对话流没有总时长上限：长思考/长输出不会被掐断，改由空闲读超时兜底。
	c.Chat = &http.Client{Transport: transport, Timeout: 0}
	return nil
}

// ListenAddr 返回 host:port。
func (c *Config) ListenAddr() string {
	return c.Addr + ":" + itoa(c.Port)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ModelDisabled 判断模型是否被配置禁用；返回 (是否禁用, 中文原因)。
// 黑名单优先：命中黑名单直接禁用，即使同时出现在白名单里。
// 白名单非空时只放行列表内模型。两个列表都为空时不做限制。
func (c *Config) ModelDisabled(model string) (bool, string) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false, ""
	}
	for _, b := range c.ModelBlock {
		if strings.ToLower(strings.TrimSpace(b)) == name {
			return true, "模型 " + model + " 已被网关禁用（命中黑名单），请联系管理员调整 config.json"
		}
	}
	if len(c.ModelAllow) == 0 {
		return false, ""
	}
	for _, a := range c.ModelAllow {
		if strings.ToLower(strings.TrimSpace(a)) == name {
			return false, ""
		}
	}
	return true, "模型 " + model + " 不在网关白名单内，请联系管理员调整 config.json"
}

// setFlags 记录两个开关是否被命令行显式指定，用于区分
// 「用户传了 0 想关闭」与「用户没传，应取默认值」。
type setFlags struct {
	reloadSet bool
	modelsSet bool
}

// SetFlagPresence 由 main 在解析命令行后调用，标记这两个开关是否被显式指定。
func (c *Config) SetFlagPresence(reloadSet, modelsSet bool) {
	c.reloadSet = reloadSet
	c.modelsSet = modelsSet
}

// firstPositive 返回第一个正数（用于兼容同一字段的两种拼写）。
func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// parseModelAccountRules 把 config.json 的 models.accounts 归一化成规则表。
//
// 模型名归一化（小写去空格）后作键：上游模型名大小写不统一，而用户手写配置时
// 也不会在意大小写 —— 严格区分会让「写了规则却不生效」变成静默故障。
// 同名模型在文件里写两次时后者覆盖前者（JSON 对象本身不允许重复键，这里只是兜底）。
func parseModelAccountRules(in map[string]struct {
	Allowlist []string `json:"allowlist"`
	Blocklist []string `json:"blocklist"`
}) map[string]ModelAccountRule {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]ModelAccountRule, len(in))
	for model, rule := range in {
		key := normalizeModelKey(model)
		if key == "" {
			continue
		}
		r := ModelAccountRule{}
		if len(rule.Allowlist) > 0 {
			r.Allow = make(map[string]bool, len(rule.Allowlist))
			for _, name := range rule.Allowlist {
				if n := strings.TrimSpace(name); n != "" {
					r.Allow[n] = true
				}
			}
		}
		if len(rule.Blocklist) > 0 {
			r.Block = make(map[string]bool, len(rule.Blocklist))
			for _, name := range rule.Blocklist {
				if n := strings.TrimSpace(name); n != "" {
					r.Block[n] = true
				}
			}
		}
		// 两个名单都空的条目视为没配，不进表 —— 否则「空规则」会被当成
		// 「配了但谁都不允许」，把该模型的所有账号都挡掉。
		if r.Configured() {
			out[key] = r
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// firstPtr 返回第一个非 nil 的指针。
//
// 与 firstPositive 的区别：这里要保留「显式 0」。重试次数 0 是合法配置
// （表示禁用重试），用 firstPositive 会把 0 当成「没配」而落回默认值 2 ——
// 用户明确关掉的东西又被打开，且没有任何提示。
func firstPtr(vals ...*int) *int {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}
