// 客户端指纹事件链的执行器（切片 18）。
//
// 只在配置 `tasks.desktop_events_enabled = true` 时才会被走到
// （见 Manager.actionFor 的过滤）。这里只负责把事件链发出去，
// 「该不该发」的判断不在这一层。
package tasks

import (
	"context"
	"fmt"
	"time"

	"workbuddy-gateway/internal/upstream"
)

// desktopThemes 是换肤任务用的主题 resource_key。
//
// 取参考实现实测的那个（和平精英联名主题）—— 它是**唯一有实测点亮记录**的值，
// 换成别的 key 是否计分没有任何依据。
const desktopTheme = "theme-tkmw7j"

// desktopBuddyAppID / desktopBuddyAppName 是「进入 Buddy 应用」五连事件用的应用。
//
// 固定用企鹅教师助手：它同时是 Buddy_App_QQ 的判据应用，一组事件覆盖两个任务
// （Buddy_App 只要求「进入任一应用」）。
const (
	desktopBuddyAppID   = "cb_y5Dy46tPQGGWtueMxXbe"
	desktopBuddyAppName = "企鹅教师助手"
)

// desktopAutomationName 是「定时任务创建成功」事件里的任务名（上游不校验，取像真人的名字）。
const desktopAutomationName = "每日整理工作日志"

// desktopLibraryDocURL 是资料库介绍的页面地址（事件里的 pageURL/Referer 用它）。
const desktopLibraryDocURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"

// desktopTemplates 是「使用模板创建任务」用的五个模板。
//
// 上游**不校验 template_id 的真实性**（参考实现实测结论），所以这里用序号即可；
// 名字取常见的模板名，让事件看起来像真人用过的东西。
var desktopTemplates = [][2]string{
	{"1", "深度研究"},
	{"2", "周报生成"},
	{"3", "竞品分析"},
	{"4", "活动策划"},
	{"5", "代码评审"},
}

// desktopPlaybookCase 是「灵感案例做同款」用的案例（id 与名称）。
const (
	desktopPlaybookCaseID   = "pm-gtm-launch-plan"
	desktopPlaybookCaseName = "新产品上市 GTM 发布计划一页纸"
)

// runDesktopTask 按动作的 desktop 子类型发出对应的事件链。
//
// 每条事件组都以一段 id 前缀区分，便于在日志与抓包里按前缀定位是哪一类。
func (m *Manager) runDesktopTask(ctx context.Context, tg target, a *action) string {
	switch a.desktop {
	case "template":
		return m.desktopTemplate(ctx, tg)
	case "playbook":
		return m.desktopPlaybook(ctx, tg)
	case "canvas":
		return m.desktopCanvas(ctx, tg)
	case "library":
		return m.desktopLibrary(ctx, tg)
	case "appearance":
		return m.desktopAppearance(ctx, tg)
	case "buddyapp":
		return m.desktopBuddyApp(ctx, tg)
	case "automation":
		return m.desktopAutomation(ctx, tg)
	default:
		// 未知子类型**不猜**：发一条看不懂的事件不如什么都不发。
		return "未知的桌面事件类型：" + a.desktop
	}
}

// desktopTemplate 「使用模板创建任务」×5。
func (m *Manager) desktopTemplate(ctx context.Context, tg target) string {
	sent := 0
	for i, tp := range desktopTemplates {
		if ctx.Err() != nil {
			return fmt.Sprintf("已上报 %d/%d 组模板事件后被取消", sent, len(desktopTemplates))
		}
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wbgw-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wbgw-tpl-req-%d-%d", ms, i)
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick, events...); err != nil {
			if sent == 0 {
				return "失败：" + err.Error()
			}
			return fmt.Sprintf("部分成功：已上报 %d/%d 组，之后失败：%v", sent, len(desktopTemplates), err)
		}
		sent++
		// 组间隔速：连发 5 组同一形状的事件在时序上过于整齐，
		// 参考实现的脚本用的也是几百毫秒级间隔。
		if !sleepCtx(ctx, 300*time.Millisecond) {
			break
		}
	}
	return fmt.Sprintf("已上报 template_used ×%d", sent)
}

// desktopPlaybook 「灵感案例做同款」事件组。
func (m *Manager) desktopPlaybook(ctx context.Context, tg target) string {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wbgw-pb-%d", ms)
	req := fmt.Sprintf("wbgw-pb-req-%d", ms)
	events := upstream.DesktopPlaybookPromptSequence(conv, req, desktopPlaybookCaseID, desktopPlaybookCaseName)
	if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick, events...); err != nil {
		return "失败：" + err.Error()
	}
	return "已上报 playbook_prompt 事件组"
}

// desktopCanvas 「设计创意画布创建」事件组。
func (m *Manager) desktopCanvas(ctx context.Context, tg target) string {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wbgw-canvas-%d", ms)
	req := fmt.Sprintf("wbgw-canvas-req-%d", ms)
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick, events...); err != nil {
		return "失败：" + err.Error()
	}
	return "已上报 create_canvas 事件组"
}

// desktopBuddyApp 「进入 Buddy 应用」五连事件（同时覆盖 Buddy_App 与 Buddy_App_QQ）。
func (m *Manager) desktopBuddyApp(ctx context.Context, tg target) string {
	events := upstream.DesktopBuddyAppSequence(desktopBuddyAppID, desktopBuddyAppName)
	if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick, events...); err != nil {
		return "失败：" + err.Error()
	}
	return "已上报 buddyapp 进入五连事件（同时覆盖 Buddy_App 与 Buddy_App_QQ）"
}

// desktopAutomation 「定时任务创建成功」事件（automation_1）。
func (m *Manager) desktopAutomation(ctx context.Context, tg target) string {
	event := upstream.DesktopAutomationCreateEvent(desktopAutomationName)
	if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick, event); err != nil {
		return "失败：" + err.Error()
	}
	return "已上报 automated_task_create_suc（automation_1）"
}

// desktopLibrary 「读资料库介绍」Web 事件。
func (m *Manager) desktopLibrary(ctx context.Context, tg target) string {
	err := m.client.ReportWebEvent(ctx, tg.Cred, tg.Prof, tg.Nick, "web_element_click",
		desktopLibraryDocURL, "library_doc_intro_click", "WorkBuddy资料库介绍")
	if err != nil {
		return "失败：" + err.Error()
	}
	return "已上报资料库介绍阅读事件"
}

// desktopAppearance 换肤：先调 set API 留痕，再报「皮肤生效」事件。
//
// 两步都要，而且**顺序不能反**：参考实现记载单独的 set 不计分 ——
// 当时的错误结论正是「只调了 set 没发事件」。这里先 set、等主题生效，
// 再发 appearance_skin_apply（语义是「客户端在主题生效状态下离开设置页」）。
func (m *Manager) desktopAppearance(ctx context.Context, tg target) string {
	if err := m.client.SetAppearanceTheme(ctx, tg.Cred, tg.Prof, desktopTheme); err != nil {
		return "失败（设置主题）：" + err.Error()
	}
	sleepCtx(ctx, 2*time.Second)
	ev := map[string]any{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": desktopTheme, "vipLevel": 0, "series": "", "type": "unknown",
	}
	if err := m.client.ReportDesktopEvent(ctx, tg.Cred, tg.Prof, tg.Nick, upstream.DesktopEvent(ev)); err != nil {
		return "失败（上报换肤事件）：" + err.Error()
	}
	return "已设置主题并上报 appearance_skin_apply"
}
