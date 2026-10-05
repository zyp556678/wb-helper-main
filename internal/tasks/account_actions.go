package tasks

// 账号级任务动作（面板「一键完成」）。
//
// 与队列执行（Run / runQueue）的分工：
//   - 队列执行是「跨账号批量」：按扫描结果组装待办，串行跑完，进度看队列状态；
//   - 账号级动作是「单账号点一下」：面板在某个账号上点「一键完成」，
//     需要**同步拿到逐项结果**（哪些做了、哪些跳过、领了多少），直接展示在弹窗里。
//
// 两者共用同一套动作实现与同一把 per-account 锁 —— 同一账号不允许同时跑两条链路
//（动作本身幂等，但专家/真实对话类会消耗上游配额，并发重跑是纯浪费）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"workbuddy-gateway/internal/upstream"
)

// ErrAccountBusy 表示该账号已有任务动作在执行中。
var ErrAccountBusy = errors.New("该账号有任务动作正在执行中")

// AccountTaskResult 是账号级任务动作的逐项结果（面板弹窗直接展示）。
type AccountTaskResult struct {
	TaskCode       string `json:"task_code"`
	Desc           string `json:"description,omitempty"`
	Status         string `json:"status"` // done | skipped | error
	Message        string `json:"message"`
	ProgressBefore string `json:"progress_before,omitempty"`
	ProgressAfter  string `json:"progress_after,omitempty"`
	Claimable      bool   `json:"claimable,omitempty"`
	Claimed        bool   `json:"claimed,omitempty"`
	Credit         int64  `json:"credit,omitempty"`
	Energy         int64  `json:"energy,omitempty"`
	ClaimError     string `json:"claim_error,omitempty"`
	// Attempt 标记「尝试型」动作：上游未证实可脚本化，跑了也可能不点亮。
	Attempt bool `json:"attempt,omitempty"`
}

// TryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (m *Manager) TryLockAccount(id string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy == nil {
		m.busy = map[string]bool{}
	}
	if m.busy[id] {
		return false
	}
	m.busy[id] = true
	return true
}

// UnlockAccount 释放账号任务锁（与 TryLockAccount 配对）。
func (m *Manager) UnlockAccount(id string) {
	m.mu.Lock()
	delete(m.busy, id)
	m.mu.Unlock()
}

// RunAccountTask 对单个账号执行单个任务的自动动作，返回逐项结果。
//
// 幂等：已领取的任务直接跳过（不消耗上游调用）。动作执行后回读进度并在达标时
// 自动领奖 —— 与队列执行同一套闭环。
func (m *Manager) RunAccountTask(ctx context.Context, accountID, taskCode string) (AccountTaskResult, error) {
	tg, err := m.one(accountID)
	if err != nil {
		return AccountTaskResult{}, err
	}
	taskCode = strings.TrimSpace(taskCode)
	if taskCode == "" {
		return AccountTaskResult{}, precondition("缺少 task_code")
	}
	a := m.actionFor(taskCode)
	if a == nil {
		if gatedActionExists(taskCode) {
			return AccountTaskResult{}, precondition(
				"该任务需要打开「客户端事件上报」开关才能自动完成（设置 → 任务中心）")
		}
		return AccountTaskResult{}, precondition(
			"该任务需要客户端内交互，没有对应接口，无法自动完成；请按任务说明在官方客户端操作")
	}
	if !m.TryLockAccount(tg.ID) {
		return AccountTaskResult{}, ErrAccountBusy
	}
	defer m.UnlockAccount(tg.ID)

	t, err := m.taskByCode(ctx, tg, taskCode)
	if err != nil {
		return AccountTaskResult{}, fmt.Errorf("读取任务失败: %w", err)
	}
	if t == nil {
		return AccountTaskResult{}, precondition("该账号没有此任务（%s）", taskCode)
	}
	res := AccountTaskResult{
		TaskCode: taskCode, Desc: a.desc, Attempt: taskCode == "black_cat",
		ProgressBefore: progressText(t),
	}
	if t.Claimed {
		res.Status, res.Message = "skipped", "该任务已领取过奖励"
		return res, nil
	}
	if a.window != nil && !a.window(time.Now()) {
		res.Status, res.Message = "skipped", a.windowHint
		return res, nil
	}
	msg, rerr := m.runAction(ctx, tg, t, a)
	if rerr != nil {
		return AccountTaskResult{}, rerr
	}
	res.Message = msg
	res.Status = "done"

	after, aerr := m.taskByCodeWaiting(ctx, tg, taskCode, t.MPOnly)
	if aerr == nil && after != nil {
		res.ProgressAfter = progressText(after)
		res.Claimable = after.Claimable
		if after.Claimable {
			credit, energy, cerr := m.client.ClaimGrowthTask(ctx, tg.Cred, tg.Prof, taskCode, after.MPOnly)
			if cerr == nil {
				res.Claimed = true
				res.Credit, res.Energy = credit, energy
				if credit > 0 || energy > 0 {
					res.Message = msg + fmt.Sprintf("；已自动领奖 +%d 积分 +%d 能量", credit, energy)
				} else {
					res.Message = msg + "；奖励此前已领取"
				}
			} else {
				res.ClaimError = cerr.Error()
				res.Message = msg + "；已达标但领奖失败，可在任务列表手动点「领取」重试"
			}
		}
	}
	m.logf("[任务] 单任务动作 账号=%s 任务=%s 进度 %s → %s 达标=%v 已领=%v",
		tg.ID, taskCode, res.ProgressBefore, res.ProgressAfter, res.Claimable, res.Claimed)
	return res, nil
}

// RunAccountAll 对该账号依次执行所有可自动任务，返回逐项结果。
//
// 流程对照参考实现：先批量 accept（规范状态机；accept 不是进度产生的必要条件，
// 但让后续状态流转规范），再逐项执行行为链路。单项失败不影响后续项。
func (m *Manager) RunAccountAll(ctx context.Context, accountID string) ([]AccountTaskResult, error) {
	tg, err := m.one(accountID)
	if err != nil {
		return nil, err
	}
	if !m.TryLockAccount(tg.ID) {
		return nil, ErrAccountBusy
	}
	defer m.UnlockAccount(tg.ID)

	var out []AccountTaskResult

	// 阶段 0：批量接受尚未接受的任务（PC 口径 + 小程序口径各一次）。
	// 失败不阻塞 —— 行为事件才是进度唯一判据。
	if list, lerr := m.listTasks(ctx, tg); lerr == nil {
		var pcCodes, mpCodes []string
		for _, t := range list {
			if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
				continue
			}
			if t.MPOnly {
				mpCodes = append(mpCodes, t.TaskCode)
			} else {
				pcCodes = append(pcCodes, t.TaskCode)
			}
		}
		if len(pcCodes) > 0 {
			if err := m.client.AcceptGrowthTasks(ctx, tg.Cred, tg.Prof, pcCodes, false); err != nil {
				out = append(out, AccountTaskResult{
					TaskCode: "(批量报名)", Status: "error",
					Message: "接受任务失败（不影响后续）: " + err.Error(),
				})
			} else {
				out = append(out, AccountTaskResult{
					TaskCode: "(批量报名)", Status: "done",
					Message: fmt.Sprintf("已接受 %d 个任务", len(pcCodes)),
				})
			}
			if !sleepCtx(ctx, actionGap) {
				return out, ctx.Err()
			}
		}
		if len(mpCodes) > 0 {
			if err := m.client.AcceptGrowthTasks(ctx, tg.Cred, tg.Prof, mpCodes, true); err != nil {
				out = append(out, AccountTaskResult{
					TaskCode: "(批量报名-小程序)", Status: "error",
					Message: "接受小程序任务失败（不影响后续）: " + err.Error(),
				})
			} else {
				out = append(out, AccountTaskResult{
					TaskCode: "(批量报名-小程序)", Status: "done",
					Message: fmt.Sprintf("已接受 %d 个小程序任务", len(mpCodes)),
				})
			}
			if !sleepCtx(ctx, actionGap) {
				return out, ctx.Err()
			}
		}
	}

	// 阶段 1：逐项执行动作表（顺序即依赖序）。
	for i := range autoActions {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		a := &autoActions[i]
		if gatedMode(a.mode) && !m.config().TaskDesktopEventsEnabled() {
			continue
		}
		item := AccountTaskResult{TaskCode: a.code, Desc: a.desc, Attempt: a.code == "black_cat"}
		t, terr := m.taskByCode(ctx, tg, a.code)
		if terr != nil {
			item.Status, item.Message = "error", "查询失败: "+terr.Error()
			out = append(out, item)
			continue
		}
		if t == nil {
			item.Status, item.Message = "skipped", "该账号无此任务"
			out = append(out, item)
			continue
		}
		item.ProgressBefore = progressText(t)
		if t.Claimed || (t.Target > 0 && t.Current >= t.Target) {
			item.Status, item.Message = "skipped", "已完成（"+progressText(t)+"）"
			out = append(out, item)
			continue
		}
		if a.window != nil && !a.window(time.Now()) {
			item.Status, item.Message = "skipped", a.windowHint
			out = append(out, item)
			continue
		}
		msg, rerr := m.runAction(ctx, tg, t, a)
		if rerr != nil {
			item.Status, item.Message = "error", rerr.Error()
			out = append(out, item)
			continue
		}
		item.Status, item.Message = "done", msg
		if after, aerr := m.taskByCodeWaiting(ctx, tg, a.code, t.MPOnly); aerr == nil && after != nil {
			item.ProgressAfter = progressText(after)
			if after.Claimable {
				item.Claimable = true
				credit, energy, cerr := m.client.ClaimGrowthTask(ctx, tg.Cred, tg.Prof, a.code, after.MPOnly)
				if cerr == nil {
					item.Claimed = true
					item.Credit, item.Energy = credit, energy
					if credit > 0 || energy > 0 {
						item.Message = msg + fmt.Sprintf("；已自动领奖 +%d 积分 +%d 能量", credit, energy)
					} else {
						item.Message = msg + "；奖励此前已领取"
					}
				} else {
					item.ClaimError = cerr.Error()
					item.Message = msg + "；已达标但领奖失败，可在任务列表手动重试"
				}
			}
		}
		out = append(out, item)
		// 项间节流：上游对连续写操作敏感。
		if !sleepCtx(ctx, actionGap) {
			return out, ctx.Err()
		}
	}
	m.logf("[任务] 一键完成 账号=%s 共 %d 项", tg.ID, len(out))
	return out, nil
}

// runAction 执行单个动作（供账号级动作与队列共用）。//
// 三档：自定义链路（run）、桌面/小程序事件链（runDesktopTask 或 mp 分派）、
// 纯上报（modeReport，按差额补条数）。
func (m *Manager) runAction(ctx context.Context, tg target, t *upstream.GrowthTask, a *action) (string, error) {
	if a.run != nil {
		return a.run(ctx, m, tg, t)
	}
	switch a.mode {
	case modeDesktopEvent:
		return m.runDesktopTask(ctx, tg, a), nil
	case modeMP:
		// modeMP 的动作全部带 run；走到这里说明动作表登记不完整。
		return "", fmt.Errorf("小程序任务 %s 缺少动作实现", a.code)
	}
	n := int64(1)
	if a.times != nil {
		n = a.times(*t)
	}
	if n <= 0 {
		return "进度已达标，无需上报", nil
	}
	done := int64(0)
	for k := int64(0); k < n; k++ {
		if !sleepCtx(ctx, chatEventPause()) {
			return fmt.Sprintf("已上报 %d/%d 条后被取消", done, n), nil
		}
		cid := fmt.Sprintf("wbgw-%s-%d-%d", a.code, time.Now().UnixMilli(), k)
		if err := m.client.ReportChatActivity(ctx, tg.Cred, tg.Prof, cid, "", a.model, a.modelName); err != nil {
			if done == 0 {
				return "", err
			}
			return fmt.Sprintf("部分成功：已上报 %d/%d 条，之后失败：%v", done, n, err), nil
		}
		done++
	}
	return fmt.Sprintf("已上报 %d 条对话事件", done), nil
}
