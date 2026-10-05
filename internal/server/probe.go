// probe.go 是 /admin/probe：**指定账号 × 模型**的免费/收费属性探测。
//
// 与 /panel/api/models/probe（目录页的手动价格探测）不是同一件事，容易混：
//
//	/panel/api/models/probe  面板按钮，按 (站点, 模型) 探测，账号由系统按额度挑；
//	/admin/probe             给「本机管理员 / CLI」用，可**指定账号**，
//	                         专治从未被调度过的账号（典型是余额耗尽的国际站号）——
//	                         它们永远排不进按额度挑选的候选，价格台账因此恒为空。
//
// 为什么探测要跑在 serve 进程里、而不是让 CLI 独立直连上游：
// 免费/收费台账保存在 serve 进程内存中，独立进程写出的结论会被运行中的服务覆盖。
// 所以 probe 子命令只作为**客户端**调用本接口，真正执行探测的是运行中的服务。
//
// 安全：只接受回环来源（探测会真实消耗账号额度，不该对局域网开放）；
// 服务设置了 api-key 时，外层鉴权中间件仍会要求携带该密钥。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy-gateway/internal/pool"
)

// probeRequest 是 /admin/probe 的请求体。
type probeRequest struct {
	// Account 是账号 ID（凭据文件名）；空表示全部可用账号。
	Account string `json:"account,omitempty"`
	// Models 是指定模型；空表示目录前 limit 个。
	Models []string `json:"models,omitempty"`
	// Limit 是 Models 为空时的最大模型数（默认 5，上限 50）。
	Limit int `json:"limit,omitempty"`
}

// probeResult 是单次探测结果。
type probeResult struct {
	Account string  `json:"account"`
	Site    string  `json:"site"`
	Model   string  `json:"model"`
	Status  string  `json:"status"` // free | paid | unknown | quota | rate_limited | error | skipped
	Credit  float64 `json:"credit"`
	Tokens  int64   `json:"tokens"`
	Detail  string  `json:"detail,omitempty"`
}

// probeResponse 是 /admin/probe 的响应。
type probeResponse struct {
	Results []probeResult  `json:"results"`
	Summary map[string]int `json:"summary"`
}

// isLoopbackRequest 判定请求是否来自回环地址（复用 openurl.go 的同一实现）。

// handleAdminProbe 执行一次指定账号的模型属性探测（POST /admin/probe）。
func (s *Server) handleAdminProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeAPIError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}
	if !isLoopbackRequest(r) {
		s.logf("[探测] 拒绝非回环来源的探测请求: %s", r.RemoteAddr)
		s.writeAPIError(w, r, http.StatusForbidden, "probe_local_only", "探测接口仅允许本机回环地址调用")
		return
	}
	if s.cat == nil || s.pool == nil {
		s.writeAPIError(w, r, http.StatusServiceUnavailable, "probe_unavailable", "模型目录或账号池未启用，无法探测")
		return
	}

	var req probeRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			s.writeAPIError(w, r, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
			return
		}
	}
	if req.Limit <= 0 {
		req.Limit = 5
	}
	if req.Limit > 50 {
		req.Limit = 50
	}

	// 目标账号：默认跳过已禁用的（对它们探测没有意义）。
	accounts := s.probeTargets(req.Account)
	if len(accounts) == 0 {
		s.writeAPIError(w, r, http.StatusNotFound, "no_account",
			"没有匹配的账号（检查 account 是否为账号 ID / 凭据文件名）")
		return
	}

	models := req.Models
	if len(models) == 0 {
		all := s.cat.ModelIDs()
		if len(all) > req.Limit {
			all = all[:req.Limit]
		}
		models = all
	}
	if len(models) == 0 {
		s.writeAPIError(w, r, http.StatusNotFound, "no_model", "模型目录为空，先刷新模型列表")
		return
	}

	s.logf("[探测] 开始探测：账号数=%d 模型数=%d 账号=%s 模型=%s",
		len(accounts), len(models), s.probeAccountNames(accounts), strings.Join(models, ","))

	// 探测串行且会消耗额度：用请求上下文 + 上限保护，避免误点打光额度。
	ctx, cancel := contextWithTimeout(r, 15*time.Minute)
	defer cancel()

	resp := probeResponse{Results: []probeResult{}, Summary: map[string]int{}}
	for _, acc := range accounts {
		for _, model := range models {
			if ctx.Err() != nil {
				break
			}
			if allowed, reason := s.pool.ModelAccountAllowed(model, acc); !allowed {
				res := probeResult{
					Account: acc.ID(), Site: acc.Site(), Model: model,
					Status: "skipped", Detail: "账号名单排除：" + reason,
				}
				resp.Results = append(resp.Results, res)
				resp.Summary[res.Status]++
				continue
			}
			res := s.probeAccountModel(ctx, acc, model)
			resp.Results = append(resp.Results, res)
			resp.Summary[res.Status]++
			s.logf("[探测] 账号=%s 模型=%s → %s（%s）", res.Account, model, res.Status, res.Detail)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// probeTargets 解析目标账号：account 为空时取全部未禁用账号。
func (s *Server) probeTargets(account string) []*pool.Account {
	want := strings.TrimSpace(account)
	var out []*pool.Account
	for _, acc := range s.pool.Accounts() {
		if acc.IsDisabled() {
			continue
		}
		if want != "" && acc.ID() != want && !strings.EqualFold(filepathBase(acc.ID()), filepathBase(want)) {
			continue
		}
		out = append(out, acc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// probeAccountNames 是日志里的账号列表。
func (s *Server) probeAccountNames(accs []*pool.Account) string {
	names := make([]string, 0, len(accs))
	for _, acc := range accs {
		names = append(names, acc.ID())
	}
	return strings.Join(names, ",")
}

// probeAccountModel 探测单个 (账号, 模型) 并归类结果状态。
func (s *Server) probeAccountModel(ctx context.Context, acc *pool.Account, model string) probeResult {
	res := probeResult{Account: acc.ID(), Site: acc.Site(), Model: model}
	probe, err := s.cat.ProbeWithAccount(ctx, acc.ID(), model)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "额度已耗尽"):
			res.Status = "quota"
		case strings.Contains(msg, "模型级限流"):
			res.Status = "rate_limited"
		case strings.Contains(msg, "没有可用的") || strings.Contains(msg, "不探测"):
			res.Status = "skipped"
		default:
			res.Status = "error"
		}
		res.Detail = msg
		return res
	}
	res.Credit = probe.Credit
	res.Tokens = probe.Tokens
	res.Detail = probe.Detail
	switch probe.Verdict {
	case "free":
		res.Status = "free"
	case "paid":
		res.Status = "paid"
	default:
		res.Status = "unknown"
	}
	return res
}

// filepathBase 是「取路径最后一段」的本地实现。
//
// 不直接用 filepath.Base：凭据文件名里可能出现 `/`（用户从别处拷来的相对路径），
// 而这里要的是「按文件名匹配」这个宽松语义，path 包在 Windows 上会把 `\` 当普通字符。
func filepathBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
