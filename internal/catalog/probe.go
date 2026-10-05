package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"workbuddy-gateway/internal/auth"
	"workbuddy-gateway/internal/pool"
	"workbuddy-gateway/internal/upstream"
)

// probePrompt 是探测用的提示词。
//
// 为什么要刻意要求较长输出：判定「免费」需要足够大的样本（freeMinTokens），
// 太短的回复可能根本没触发计费，会把收费模型误判成免费。
const probePrompt = "请连续输出约 120 个汉字，内容随意，仅用于连通性与计费测试。"

// probeMaxTokens 是探测请求的输出上限。
const probeMaxTokens = 400

// quotaSettleDelay 是请求结束后、二次读余额前的等待时长。
//
// 存在的理由：实测发现计费存在延迟结算，请求刚结束就读余额会读不到本次扣费，
// 从而把收费模型误判成免费。等一小段能显著提高扣费可见性。
const quotaSettleDelay = 3 * time.Second

// probeMinDelta 是判定「余额发生变化」的最小差值。
// 额度字段有 8 位小数，正常计费一次远大于这个值；设一个极小阈值只是为了容忍浮点误差。
const probeMinDelta = 0.0001

// ProbeOne 对单个 (站点, 模型) 做一次余额差分价格探测。
//
// 流程：取该站点一个额度未耗尽的账号 → 读余额 → 发一次极短对话 → 再读余额 → 用差额判定。
// 为什么不用 usage.credit 直接判定：实测遇到过 credit=0（看似免费）但余额确实下降的模型，
// credit 并非所有模型的真实计费信号，只能作为参考值记录。
//
// 探测会真实消耗额度，因此串行执行（probeMu），且调用方必须限量。
func (c *Catalog) ProbeOne(ctx context.Context, site, model string) (Probe, error) {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()

	acc, release, ok := c.accts.AcquireForProbe(ctx, site)
	if !ok {
		return Probe{}, fmt.Errorf("没有可用的%s账号（或额度已耗尽）", auth.SiteLabel(site))
	}
	defer release()
	return c.probeAccountModel(ctx, acc, site, model)
}

// ProbeWithAccount 对**指定账号** + 模型做一次余额差分探测。
//
// 与 ProbeOne 的差别只在「用哪个账号探」：ProbeOne 按剩余额度挑一个
// （尽量不影响正在服务用户的账号），本方法强制用调用方指定的那个。
//
// 存在的理由是**从未被调度过的账号**（典型是余额耗尽的国际站号）也需要主动学习
// 某个模型对它是免费还是收费 —— 这类账号在 ProbeOne 的挑选里永远排不上
// （额度为 0 会被跳过），于是它的价格台账永远是空的，界面上也永远显示「未探测」。
func (c *Catalog) ProbeWithAccount(ctx context.Context, accountID, model string) (Probe, error) {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()

	acc, err := c.accts.Find(accountID)
	if err != nil {
		return Probe{}, err
	}
	// 独占该账号的串行锁：从这一刻起只能用 ...NoLock 系列方法，
	// 任何会自己加锁的调用都会自锁（IsDisabled 就是其中一个）。
	acc.Lock()
	defer acc.Unlock()
	if acc.DisabledNoLock() {
		return Probe{}, fmt.Errorf("账号 %s 已禁用，不探测", accountID)
	}
	if acc.ViewNoLock().AccessToken == "" {
		return Probe{}, fmt.Errorf("账号 %s 没有可用凭据，不探测", accountID)
	}
	return c.probeAccountModel(ctx, acc, acc.ProfileRef().Key, model)
}

// probeAccountModel 是探测的实际执行体。
//
// **调用方必须已持有 acc 的串行锁**：探测靠前后两次余额读数做差分，
// 期间若有并发请求用同一个账号，扣费就会算到本次探测头上（或被算走），差分失效。
func (c *Catalog) probeAccountModel(ctx context.Context, acc *pool.Account, site, model string) (Probe, error) {
	// 从这里到调用方释放为止都持有该账号的串行锁：
	// 只能用 ...NoLock 系列方法，任何会加锁的调用都会自锁。
	id := acc.ID()
	cred := acc.ViewNoLock()
	prof := acc.ProfileRef()

	before, err := c.accts.RefreshQuotaNoLock(ctx, acc)
	if err != nil {
		return Probe{}, fmt.Errorf("探测前查询余额失败: %w", err)
	}
	if before <= 0 {
		return Probe{}, fmt.Errorf("账号 %s 额度已耗尽，探测无意义（先等签到或补充额度）", id)
	}

	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": probeMaxTokens,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": probePrompt},
		},
	})

	probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp, err := c.client.Chat(probeCtx, cred, prof, payload)
	if err != nil {
		return Probe{}, fmt.Errorf("探测请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		class := upstream.Classify(resp.StatusCode, string(body))
		switch class.Kind {
		case upstream.ErrHardCredit:
			return Probe{}, fmt.Errorf("账号额度耗尽（14018），不覆盖原有结论")
		case upstream.ErrModelRate:
			return Probe{}, fmt.Errorf("模型级限流（6004），不覆盖原有结论")
		default:
			return Probe{}, fmt.Errorf("上游返回 %d（%s）: %s", resp.StatusCode, class.Kind, class.Message)
		}
	}

	credit, tokens := readUsageFromSSE(resp.Body)

	// 计费可能是**延迟结算**的：请求刚结束就立刻读余额，很可能读不到本次扣费。
	// 因此先等一小段再读，尽量让结算落地（这也是实测发现的问题：
	// 同一个收费模型，立刻读余额差分为 0，被误判成免费）。
	select {
	case <-ctx.Done():
	case <-time.After(quotaSettleDelay):
	}

	after, err := c.accts.RefreshQuotaNoLock(ctx, acc)
	if err != nil {
		return Probe{}, fmt.Errorf("探测后查询余额失败: %w", err)
	}
	// 额度恢复时顺手解冻被硬冷却的账号
	acc.ReviveIfCreditsRecoveredNoLock(after)

	p := Probe{
		LastProbeAt: time.Now().Unix(),
		Credit:      credit,
		Tokens:      tokens,
	}
	delta := before - after
	catalogFree := c.catalogSaysFree(site, model)

	switch {
	case delta > probeMinDelta:
		// 观测到扣费：结论最硬，直接判定收费
		p.Verdict = "paid"
		p.Cost = delta
		p.Detail = fmt.Sprintf("余额差分 %.4f（%.4f→%.4f），判定收费", delta, before, after)

	case tokens < freeMinTokens:
		// 余额没变但样本太小：可能只是没跑够
		p.Verdict = ""
		p.Detail = fmt.Sprintf("余额无变化但样本过小（%d tokens < %d），本次不判定", tokens, freeMinTokens)

	case !catalogFree:
		// 关键取舍：**余额差分只能证明收费，不能证明免费**。
		// 扣费可能延迟结算，读不到差值不代表不扣费；因此只有目录也明确免费时
		// （0 倍率且无过期促销、或限免促销生效中）才敢判定免费，
		// 否则保持未知——把收费模型误标成免费会让调度把流量全压到它上面，代价更大。
		p.Verdict = ""
		p.Cost = 0
		if e, ok := c.Entry(site, model); ok && e.HasMultiplier && e.Multiplier > 0 {
			p.Detail = fmt.Sprintf("余额未观测到变化（%d tokens），但目录倍率为 %.2fx，判为收费但不覆盖（可能是延迟结算）", tokens, e.Multiplier)
			p.Verdict = "paid"
			p.Cost = 0
		} else {
			p.Detail = fmt.Sprintf("余额未观测到变化（%d tokens），但目录未明确免费，保持未知", tokens)
		}

	default:
		p.Verdict = "free"
		p.Cost = 0
		p.Detail = fmt.Sprintf("余额差分无变化（%d tokens 样本）且目录明确免费，判定免费", tokens)
	}
	if credit > 0 && p.Verdict == "free" {
		// 参考信号与实测冲突时记下来便于排查（credit 并非所有模型的真实计费信号）
		p.Detail += fmt.Sprintf("；注意 usage.credit=%.4f 与实测免费不一致", credit)
	}

	c.recordProbe(site, model, p)
	c.logf("[价格探测] %s / %s → %s（%s）", auth.SiteLabel(site), model,
		ifEmpty(p.Verdict, "未判定"), p.Detail)
	c.saveCache()
	return p, nil
}

// recordProbe 写入探测结论。
func (c *Catalog) recordProbe(site, model string, p Probe) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.probes == nil {
		c.probes = map[string]Probe{}
	}
	c.probes[probeKey(site, model)] = p
}

// readUsageFromSSE 从 SSE 流里取最后一个 usage（含 credit 参考值）。
func readUsageFromSSE(r io.Reader) (credit float64, tokens int64) {
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			// 只在缓冲里向前找 data: 行，避免无上限增长
			if len(buf) > 1<<20 {
				buf = buf[len(buf)-1<<19:]
			}
		}
		if err != nil {
			break
		}
	}
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(raw), &chunk) != nil {
			continue
		}
		u, ok := chunk["usage"].(map[string]any)
		if !ok || u == nil {
			continue
		}
		if v, ok := numFromAny(u["credit"]); ok {
			credit = v
		}
		if v, ok := numFromAny(u["total_tokens"]); ok {
			tokens = int64(v)
		} else {
			pt, _ := numFromAny(u["prompt_tokens"])
			ct, _ := numFromAny(u["completion_tokens"])
			tokens = int64(pt + ct)
		}
	}
	return credit, tokens
}

func numFromAny(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// catalogSaysFree 判断目录是否**明确**认定该模型免费：
// 有倍率且为 0 且促销未过期，或者正处于限免促销中。
func (c *Catalog) catalogSaysFree(site, model string) bool {
	e, ok := c.Entry(site, model)
	if !ok {
		return false
	}
	if e.PromoFree && !e.PromoExpired {
		return true
	}
	return e.HasMultiplier && e.Multiplier == 0 && !e.PromoExpired
}

// -----------------------------------------------------------------------------
// 探测调度
// -----------------------------------------------------------------------------

// ProbeLoop 周期探测待确认的模型价格。
//
// 调度策略（与 wb-gateway 同口径）：
//   - 启动后延迟首轮，等账号池与目录就绪
//   - 首轮批量更大（快速补齐结论），收敛后每轮限量
//   - 待探测队列未清空时用短间隔追赶，清空后回到长间隔
//
// requestCount 用于判断「模型是否被真实请求过」：只探测在用的模型，避免无谓消耗额度。
func (c *Catalog) ProbeLoop(ctx context.Context, requestCount func(model string) int64) {
	if !c.ProbeEnabled {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(ProbeStartDelay):
	}

	initial := true
	for {
		done := c.probeOnce(ctx, requestCount, initial)
		initial = false

		interval := ProbeTick
		if !done {
			interval = 2 * time.Minute // 还有待探测项：短间隔追赶
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// probeOnce 执行一轮探测；返回是否已把待探测队列清空。
func (c *Catalog) probeOnce(ctx context.Context, requestCount func(model string) int64, initial bool) bool {
	batch := ProbeBatch
	if initial {
		batch = ProbeInitialBatch
	}

	type target struct {
		site, model string
	}
	var pending []target
	for _, site := range c.accts.Sites() {
		for _, m := range c.Merged() {
			if _, ok := c.Entry(site, m.ID); !ok {
				continue
			}
			if c.NeedsProbe(site, m.ID, requestCount(m.ID)) {
				pending = append(pending, target{site, m.ID})
			}
		}
	}
	if len(pending) == 0 {
		return true
	}

	limit := batch
	if limit > len(pending) {
		limit = len(pending)
	}
	okCount := 0
	for _, t := range pending[:limit] {
		if ctx.Err() != nil {
			return false
		}
		if _, err := c.ProbeOne(ctx, t.site, t.model); err != nil {
			c.logf("[价格探测] %s / %s 跳过: %v", auth.SiteLabel(t.site), t.model, err)
			continue
		}
		okCount++
	}
	c.logf("[价格探测] 本轮完成 %d/%d（待探测 %d）", okCount, limit, len(pending)-limit)
	return len(pending) <= limit
}

// ProbeModels 供面板批量探测：逐个执行，返回每个结果。
func (c *Catalog) ProbeModels(ctx context.Context, targets []struct{ Site, Model string }) []map[string]any {
	out := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		p, err := c.ProbeOne(ctx, t.Site, t.Model)
		row := map[string]any{"site": t.Site, "model": t.Model}
		if err != nil {
			row["verdict"] = ""
			row["detail"] = err.Error()
			row["error"] = true
		} else {
			row["verdict"] = p.Verdict
			row["cost"] = p.Cost
			row["credit"] = p.Credit
			row["tokens"] = p.Tokens
			row["detail"] = p.Detail
		}
		out = append(out, row)
	}
	return out
}

func ifEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
