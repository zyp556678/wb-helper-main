// 夜猫子（black_cat）与「真实对话」类任务的判据载体。
//
// 与 chat_5 那类「只发一条活跃上报」不同，这几个任务的判据是
// **真的有一次对话发生**：上游既看 chat_request_send 事件，也看这次对话本身
// （黑猫、Model_chat_GLM5.2 都属于这一类）。所以这里必须先发一次真实对话、
// 再补上报，只发事件是不够的。
//
// 对话内容取「1+1等于几？直接回答。」：极短，消耗可忽略，且不会因为内容被
// 安全策略拦截而让整条链路失败。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// chatProbePrompt 是任务用真实对话的固定提示词。
const chatProbePrompt = "1+1等于几？直接回答。"

// InNightWindow 报告当前是否处于夜猫子计数窗口（23:00–08:00 本地时区）。
func InNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// RunSimpleChat 发一次真实对话并读干流。
//
// 读干流是必须的：不读完会在上游留下悬空连接，而对话本身才是判据的一部分。
func (c *Client) RunSimpleChat(ctx context.Context, cred *CredentialView, p *Profile, model, prompt string) error {
	if model == "" {
		model = "fast-model"
	}
	if prompt == "" {
		prompt = chatProbePrompt
	}
	body, err := json.Marshal(map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
		"stream":   true,
	})
	if err != nil {
		return err
	}
	resp, err := c.Chat(ctx, cred, p, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &HTTPStatusError{Status: resp.StatusCode, Body: string(b)}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// BlackcatNeed 查 black_cat 任务还差几次对话。
//
// 返回 0 表示「无需再做」（任务不存在、已领取或已达标）；拉取失败返回错误。
func (c *Client) BlackcatNeed(ctx context.Context, cred *CredentialView, p *Profile) (int64, error) {
	tasks, err := c.ListGrowthTasks(ctx, cred, p, false)
	if err != nil {
		return 0, err
	}
	for _, t := range tasks {
		if t.TaskCode != "black_cat" {
			continue
		}
		if t.Claimed || (t.Target > 0 && t.Current >= t.Target) {
			return 0, nil
		}
		if t.Target <= 0 {
			return 1, nil
		}
		return t.Target - t.Current, nil
	}
	return 0, nil
}

// RunNightChats 在夜间窗口内完成 need 次「glm-5.2 真实对话 + 活跃上报」。
//
// 返回已完成次数；中途失败时把已完成次数与错误一起返回（调用方据此报告进度）。
// 每次之间隔 4 秒：这是参考实现实测的节奏，避免连发被上游按机器特征处理。
func (c *Client) RunNightChats(ctx context.Context, cred *CredentialView, p *Profile, need int) (int64, error) {
	var done int64
	for i := 0; i < need; i++ {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		if err := c.RunSimpleChat(ctx, cred, p, "glm-5.2", chatProbePrompt); err != nil {
			return done, fmt.Errorf("第 %d 次对话失败: %w", i+1, err)
		}
		conv := fmt.Sprintf("wbgw-night-%d-%d", time.Now().UnixMilli(), i)
		if err := c.ReportChatActivity(ctx, cred, p, conv, "", "glm-5.2", "GLM-5.2"); err != nil {
			return done, fmt.Errorf("第 %d 次上报失败: %w", i+1, err)
		}
		done++
		select {
		case <-ctx.Done():
			return done, ctx.Err()
		case <-time.After(4 * time.Second):
		}
	}
	return done, nil
}

// RunModelChat 完成「用某个模型真实对话一次 + 按该模型上报」的链路。
//
// 返回上报是否成功；对话本身成功而上报失败时返回 (false, nil) 之外的可读说明
// 由调用方拼装 —— 这里保持「对话失败 = error，上报失败 = 布尔」的区分，
// 因为两者的处理方式不同：对话失败要重试整条链路，上报失败只需重报。
func (c *Client) RunModelChat(ctx context.Context, cred *CredentialView, p *Profile, modelID, modelName string) error {
	if err := c.RunSimpleChat(ctx, cred, p, modelID, chatProbePrompt); err != nil {
		return err
	}
	conv := fmt.Sprintf("wbgw-model-%d", time.Now().UnixMilli())
	return c.ReportChatActivity(ctx, cred, p, conv, "", modelID, modelName)
}

// SchoolVoucher 是开学季抽奖抽中的第三方券码（KFC/瑞幸/酷狗等）。
type SchoolVoucher struct {
	GrantID   int64  `json:"grant_id"`
	DrawUUID  string `json:"draw_uuid,omitempty"`
	SKUCode   string `json:"sku_code,omitempty"`
	PrizeName string `json:"prize_name,omitempty"`
	Code      string `json:"code"`
	ValidFrom string `json:"valid_from,omitempty"`
	ValidTo   string `json:"valid_to,omitempty"`
	GrantedAt string `json:"granted_at,omitempty"`
}

// SchoolVouchers 查询账号的开学季券码列表（只读，单次拉全、无分页）。
//
// 抽到积分的记录不在这里（那属于奖励流水），本端点只返回第三方券码。
func (c *Client) SchoolVouchers(ctx context.Context, cred *CredentialView, p *Profile) ([]SchoolVoucher, error) {
	data, err := c.growthCall(ctx, http.MethodGet, p.Origin+schoolVoucherPath, webHeaders(cred, p, "mp"), nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []SchoolVoucher `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
