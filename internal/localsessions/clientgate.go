package localsessions

// -----------------------------------------------------------------------------
// 客户端运行门禁（对照 wb-switch 的 session.rs 写入门禁 + session_groups.rs 的
// sync_unify_batch_workbuddy 生命周期窗口）
//
// **两条规则刻意不同，不要合并**：
//
//   - **复制**（单会话写入，copy_one_session 的入口门禁）：目标账号正是该档位
//     「当前登录账号」、且客户端在运行时**直接拒绝**。实测依据：写「非当前登录
//     账号」的副本免关安全（客户端不使用那份数据，切号后自然可见）；写「当前登录
//     账号」必须等它停止写入 —— 运行中的实例看不到我们的写入，继续对话会让
//     会话状态分叉。
//
//   - **整组统一 / 单对同步**：先校验全部目标（校验不通过就不打断用户），再关掉
//     「运行中且当前登录是写入目标」的档位客户端，写完**重新打开**；只有本次关掉的
//     才重开。恢复不了（needsRecovery）时**暂停重开**并如实报告 —— 让客户端带着
//     半完成的会话数据启动是更坏的结果。
//
// 关不掉客户端时**放弃写入**（不「假装写入」）：客户端退出时会把内存里的会话状态
// 回写，此时写入会被它覆盖。
// -----------------------------------------------------------------------------

import (
	"fmt"
	"strings"
	"time"

	"workbuddy-gateway/internal/localapps"
)

// sessionCopyAppRunningMsg 是「目标账号正在客户端中使用」的统一文案
// （对照 SESSION_COPY_APP_RUNNING，逐字一致）。
const sessionCopyAppRunningMsg = "目标账号正在 WorkBuddy 中使用，已阻止修改会话数据；请先退出 WorkBuddy 后重试"

// sessionCloseTimeout 是等待客户端退出的预算（与账号切换一致）。
const sessionCloseTimeout = 20 * time.Second

// 门禁用的进程/登录态探针。
//
// **做成包级变量不是为了扩展性，是为了测试安全**：默认实现会真的去枚举、结束、
// 启动跑测试那台机器上的 WorkBuddy 客户端 —— 第一次加账号切换功能时就真的把用户
// 正在用的客户端关掉过一次。单测在 TestMain 里替换成替身，需要特定行为的用例
// 自己再换（见 clientgate_test.go）。
var (
	clientRunning  = localapps.IsWorkBuddyRunning
	clientLoginUID = localapps.CurrentLoginUID
	clientClose    = localapps.CloseWorkBuddyClient
	clientLaunch   = localapps.LaunchWorkBuddyClient
)

// closedClient 是本次被门禁关闭、需要在写完后重开的客户端。
type closedClient struct {
	Variant string
	ExeHint string
}

// targetWriteBlocked 报告「目标账号正在客户端中使用」的写入门禁。
//
// 读不到登录态时**保守拦截**：宁可要求退出客户端，也不做无法判定的写入。
func (s *Store) targetWriteBlocked(variant, targetUID string) (bool, string) {
	if !clientRunning(variant) {
		return false, ""
	}
	current, ok := clientLoginUID(variant)
	if !ok {
		return true, sessionCopyAppRunningMsg
	}
	if current == targetUID {
		return true, sessionCopyAppRunningMsg
	}
	return false, ""
}

// runningTargetNeedsRestart 判断「运行中且当前登录是写入目标」的档位是否需要关闭
// （对照 should_restart_running_target：读不到登录态时也关，因为无法排除它就是目标）。
func runningTargetNeedsRestart(variant string, targetUIDs []string) bool {
	if !clientRunning(variant) {
		return false
	}
	current, ok := clientLoginUID(variant)
	if !ok {
		return true
	}
	for _, uid := range targetUIDs {
		if uid == current {
			return true
		}
	}
	return false
}

// closeClientsForTargets 关闭「运行中且当前登录是写入目标」的档位客户端。
//
// 关闭顺序固定为 cn → intl（与 switch 的 WbVariant::ALL 一致）；中途失败时把此前
// 已关闭的客户端重新打开再返回错误（不能把用户的客户端留在关着的状态）。
func closeClientsForTargets(targets []LinkMember) ([]closedClient, error) {
	uidsByVariant := map[string][]string{}
	for _, m := range targets {
		uidsByVariant[m.Variant] = append(uidsByVariant[m.Variant], m.UID)
	}
	var closed []closedClient
	for _, variant := range []string{"cn", "intl"} {
		uids := uidsByVariant[variant]
		if len(uids) == 0 || !runningTargetNeedsRestart(variant, uids) {
			continue
		}
		_, _, hint, err := clientClose(variant, sessionCloseTimeout)
		if err != nil {
			suffix := ""
			if reopenErrs := reopenClients(closed); len(reopenErrs) > 0 {
				suffix = "；此前关闭的客户端重新打开失败：" + strings.Join(reopenErrs, "；")
			}
			return closed, fmt.Errorf("关闭 %s失败：%v%s", localapps.WorkBuddySiteLabel(variant), err, suffix)
		}
		closed = append(closed, closedClient{Variant: variant, ExeHint: hint})
	}
	return closed, nil
}

// reopenClients 重新打开本次关闭的客户端，返回逐条失败说明（成功为空）。
func reopenClients(closed []closedClient) []string {
	var errs []string
	for _, c := range closed {
		if _, err := clientLaunch(c.Variant, c.ExeHint); err != nil {
			errs = append(errs, fmt.Sprintf("%s：%v", localapps.WorkBuddySiteLabel(c.Variant), err))
		}
	}
	return errs
}

// recoveryBlocksStartup 报告恢复结果里是否存在阻碍「重开客户端」的问题
// （对照 recovery_blocks_startup：只有不可重试的问题才阻断）。
func recoveryBlocksStartup(report *RecoveryReport) bool {
	for _, issue := range report.NeedsRecovery {
		if !issue.Retryable {
			return true
		}
	}
	return false
}

// recoveryBlockingDetail 汇总阻碍启动的原因（带操作标识，便于用户自查）。
func recoveryBlockingDetail(report *RecoveryReport) string {
	var parts []string
	for _, issue := range report.NeedsRecovery {
		if !issue.Retryable {
			parts = append(parts, issue.OperationID+"："+issue.Reason)
		}
	}
	return strings.Join(parts, "；")
}

// settleClosedClients 在会话写入结束后决定「重开哪些客户端」。
//
// 顺序（对照 sync_unify_batch_workbuddy 的收尾）：
//  1. 本次写入留下未恢复一致的问题 → 暂停重开并如实报告；
//  2. 逐档位跑一次恢复，仍有阻断项 → 暂停重开；
//  3. 都没问题才重开，重开失败只报告、不改变写入结果。
func (s *Store) settleClosedClients(closed []closedClient, report *GroupSyncReport) []string {
	if len(closed) == 0 {
		return nil
	}
	if report != nil && report.NeedsRecovery {
		report.Errors = append(report.Errors, GroupSyncError{
			Error: "会话写入待恢复，已暂停重新打开客户端；请先处理恢复提示",
		})
		return nil
	}
	for _, c := range closed {
		recovery := s.RecoverPendingOperations(c.Variant)
		if recoveryBlocksStartup(recovery) {
			if report != nil {
				report.NeedsRecovery = true
				report.Errors = append(report.Errors, GroupSyncError{
					Error: fmt.Sprintf("%s仍有待恢复的会话写入（%s），已暂停重新打开客户端",
						localapps.WorkBuddySiteLabel(c.Variant), recoveryBlockingDetail(recovery)),
				})
			}
			return nil
		}
		if len(s.pendingOperations(c.Variant)) > 0 {
			if report != nil {
				report.NeedsRecovery = true
				report.Errors = append(report.Errors, GroupSyncError{
					Error: fmt.Sprintf("%s仍有待恢复的会话写入，已暂停重新打开客户端",
						localapps.WorkBuddySiteLabel(c.Variant)),
				})
			}
			return nil
		}
	}
	var reopened []string
	for _, c := range closed {
		if _, err := clientLaunch(c.Variant, c.ExeHint); err != nil {
			if report != nil {
				report.Errors = append(report.Errors, GroupSyncError{
					Error: fmt.Sprintf("%s会话已处理，但客户端重新打开失败：%v",
						localapps.WorkBuddySiteLabel(c.Variant), err),
				})
			}
			continue
		}
		reopened = append(reopened, c.Variant)
	}
	return reopened
}
