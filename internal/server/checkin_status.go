package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"workbuddy-gateway/internal/pool"
)

// -----------------------------------------------------------------------------
// 今日签到状态（批量查询）
//
// 为什么需要单独一个接口：`Checkin` 是幂等的 —— 重复调用只会返回「今天已签到」，
// 所以**光靠签到接口无法区分「还没签」和「签过了」**。界面要在点之前就告诉用户
// 状态，只能单独查。上游的 `checkin-activity-status`（回退 `checkin-status`）
// 提供 data.today_checked_in。
//
// 为什么是**批量**而不是每张卡各自请求：状态查询是「每账号一次上游请求」，
// 让 N 张卡各发一次，就是把 N 个并发请求从浏览器铺到上游。收拢到一个批量端点后，
// 服务端可以（a）限制并发、（b）加 TTL 缓存、（c）只对支持签到的站点发请求。
// -----------------------------------------------------------------------------

// checkinStatusTTL 是状态缓存的有效期。
//
// 签到是**日级事件**，5 分钟的陈旧窗口不会造成误判（真跨了零点也只是晚几分钟
// 才显示「今日未签到」），却能让「来回切页面」不再反复打上游。
const checkinStatusTTL = 5 * time.Minute

// checkinStatusConcurrency 是批量查询时的并发上限。
//
// 取 4 是为了不把账号池上游连接一次性占满 —— 面板刷新的同时用户可能正在对话，
// 那才是主链路，不该被一个状态查询挤掉。
const checkinStatusConcurrency = 4

// CheckinStatusView 是单个账号的签到状态。
type CheckinStatusView struct {
	// TodayCheckedIn 为真表示今天已经签过。
	TodayCheckedIn bool `json:"today_checked_in"`
	// Unsupported 为真表示该站点不提供签到（国际站）—— **不是失败**。
	Unsupported bool `json:"unsupported"`
	// Error 非空表示这次没查成（鉴权失败、网络错误等）。此时 TodayCheckedIn 无意义。
	Error string `json:"error,omitempty"`
	// CheckedAt 是这次结果的时间戳（Unix 秒）；命中缓存时是**原查询**的时间。
	CheckedAt int64 `json:"checked_at"`
}

// checkinStatusCache 是按账号 ID 缓存的状态表。
//
// 刻意**不放进 pool.Account**：签到状态是「上游事实的快照」，不是账号的治理状态，
// 混进 Account 会让「账号池快照」这个对外契约里多出一个需要解释的字段。
type checkinStatusCache struct {
	mu      sync.Mutex
	entries map[string]CheckinStatusView
	// workDir 非空时缓存会落盘（空表示纯内存，单测用）。
	workDir string
	logf    func(string, ...any)
}

func newCheckinStatusCache() *checkinStatusCache {
	return &checkinStatusCache{entries: map[string]CheckinStatusView{}}
}

// cachePath 是持久化位置（在网关自己的工作目录里，不污染用户主目录）。
func (c *checkinStatusCache) cachePath(workDir string) string {
	return filepath.Join(workDir, "wb-checkin-status.json")
}

// loadFrom 从磁盘恢复缓存。
//
// **为什么值得持久化**：签到状态是「日级」信息，但缓存只有 5 分钟 ——
// 网关每次重启（升级、改配置、崩溃重拉）之后，第一次打开账号页都会对
// 每个国内站账号各打一次上游。池子大时这就是几十个请求。
// 读一份 JSON 就能省掉，代价可以忽略。
//
// 过期项直接丢弃（不做过期清理任务）：签到状态是**带时间戳的**，
// get() 本来就会按 TTL 判过期，读进来一堆陈旧项只是占点内存。
func (c *checkinStatusCache) loadFrom(workDir string) {
	raw, err := os.ReadFile(c.cachePath(workDir))
	if err != nil {
		return // 首次运行没有这个文件，属正常
	}
	var stored map[string]CheckinStatusView
	if err := json.Unmarshal(raw, &stored); err != nil {
		return // 文件坏了就当没有，不影响功能
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for id, v := range stored {
		if now.Sub(time.Unix(v.CheckedAt, 0)) <= checkinStatusTTL {
			c.entries[id] = v
		}
	}
}

// saveTo 落盘。**失败只记录、不返回错误**：缓存是加速手段，
// 写不进去不该让「查签到状态」这个功能失败。
func (c *checkinStatusCache) saveTo(workDir string, logf func(string, ...any)) {
	c.mu.Lock()
	snapshot := make(map[string]CheckinStatusView, len(c.entries))
	for k, v := range c.entries {
		snapshot[k] = v
	}
	c.mu.Unlock()

	data, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	if err := os.WriteFile(c.cachePath(workDir), data, 0o600); err != nil && logf != nil {
		logf("[签到状态] 缓存落盘失败（不影响功能）: %v", err)
	}
}

func (c *checkinStatusCache) get(id string, now time.Time) (CheckinStatusView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.entries[id]
	if !ok {
		return CheckinStatusView{}, false
	}
	if now.Sub(time.Unix(v.CheckedAt, 0)) > checkinStatusTTL {
		return CheckinStatusView{}, false
	}
	return v, true
}

func (c *checkinStatusCache) put(id string, v CheckinStatusView) {
	c.mu.Lock()
	c.entries[id] = v
	c.mu.Unlock()
	// 落盘在解锁之后：文件 IO 不该占着锁，否则并发查询会互相等磁盘。
	if c.workDir != "" {
		c.saveTo(c.workDir, c.logf)
	}
}

// invalidate 让某个账号的缓存立刻失效（签到成功后调用）。
//
// 不失效的话，用户刚点完「手动签到」，卡片还会显示「今日未签到」最长 5 分钟 ——
// 一个「操作成功了但界面说没成功」的自相矛盾。
func (c *checkinStatusCache) invalidate(id string) {
	c.mu.Lock()
	delete(c.entries, id)
	c.mu.Unlock()
	if c.workDir != "" {
		c.saveTo(c.workDir, c.logf)
	}
}

// handleAccountsCheckinStatus 批量查询今日签到状态
// （POST /panel/api/accounts/checkin-status，无请求体）。
//
// 国际站账号**不发任何上游请求**，直接返回 Unsupported=true。
func (s *Server) handleAccountsCheckinStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errBody("仅支持 POST"))
		return
	}

	ctx, cancel := contextWithTimeout(r, 45*time.Second)
	defer cancel()

	accs := s.pool.Accounts()
	now := time.Now()
	result := make(map[string]CheckinStatusView, len(accs))

	// 先吃掉缓存命中与「不支持」这两类，剩下的才需要真发请求。
	type pending struct {
		id  string
		acc *pool.Account
	}
	var todo []pending
	for _, a := range accs {
		id := a.Cred.AccountID()
		if v, ok := s.checkinCache.get(id, now); ok {
			result[id] = v
			continue
		}
		if !a.Profile().SupportsCheckin() {
			v := CheckinStatusView{Unsupported: true, CheckedAt: now.Unix()}
			s.checkinCache.put(id, v)
			result[id] = v
			continue
		}
		todo = append(todo, pending{id: id, acc: a})
	}

	// 有界并发：信号量限流，结果写回前统一加锁。
	sem := make(chan struct{}, checkinStatusConcurrency)
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, item := range todo {
		wg.Add(1)
		go func(id string, acc *pool.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			st, err := s.client.CheckinStatus(ctx, acc.View(), acc.Profile())
			v := CheckinStatusView{CheckedAt: time.Now().Unix()}
			switch {
			case err != nil:
				// 查不到就**如实报错**，绝不退化成一个「今日未签到」——
				// 那会把「没查到」讲成「还没签」，用户照着提示去点签到，
				// 拿到的却是「今天已签到」，一个自相矛盾的来回。
				v.Error = err.Error()
			default:
				v.TodayCheckedIn = st.TodayCheckedIn
				v.Unsupported = st.Unsupported
			}
			// 失败不进缓存：下一轮应该重试，而不是把错误钉住 5 分钟。
			if v.Error == "" {
				s.checkinCache.put(id, v)
			}
			mu.Lock()
			result[id] = v
			mu.Unlock()
		}(item.id, item.acc)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": result,
		"ttl":      int(checkinStatusTTL / time.Second),
	})
}
