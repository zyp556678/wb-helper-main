package server

import (
	"os"
	"strconv"
	"strings"

	"workbuddy-gateway/internal/eventlog"
	"workbuddy-gateway/internal/outbound"
)

// prepareOutboundBody 执行出站改写管线，并把实际改动记入事件日志。
//
// degrade 为真表示「降级重试」：无论配置成哪种模式，都换成中性提示词。
func (s *Server) prepareOutboundBody(body []byte, degrade bool) ([]byte, outbound.Result) {
	mode := s.config().PromptMode()
	prompt, err := outbound.ResolveSystemPrompt(mode, s.config().Prompt.Text, s.config().Prompt.File, os.ReadFile)
	if err != nil {
		// 配置错了要立刻暴露在面板上，但不能让请求失败——退回内置提示词继续服务。
		s.logf("[提示词] 读取提示词文件失败，本次改用内置默认提示词: %v", err)
		if s.events != nil {
			s.events.Warn(eventlog.ChannelOutbound, "prompt_file_error",
				"读取提示词文件失败，本次已改用内置默认提示词",
				map[string]any{"file": s.config().Prompt.File, "error": err.Error()})
		}
		prompt = outbound.DefaultSystemPrompt
	}

	efforts, defaults := s.cat.EffortTables()
	res := outbound.Prepare(body, outbound.Options{
		PromptMode:        mode,
		SystemPrompt:      prompt,
		Sanitize:          s.config().PromptSanitize(),
		StrictFirstSystem: s.config().PromptStrictFirstSystem(),
		SupportedEfforts:  efforts,
		DefaultEfforts:    defaults,
		Degrade:           degrade,
	})
	return res.Body, res
}

// EffectivePrompt 返回当前实际生效的提示词（面板展示用，让用户确认到底在用什么）。
func (s *Server) EffectivePrompt() string {
	mode := s.config().PromptMode()
	prompt, err := outbound.ResolveSystemPrompt(mode, s.config().Prompt.Text, s.config().Prompt.File, os.ReadFile)
	if err != nil {
		return "(读取失败: " + err.Error() + ")"
	}
	return prompt
}

// logOutbound 把出站改写的实际动作记入事件日志。
//
// 只在「真的改动了什么」时才记：绝大多数请求的改写结果是无操作，
// 每条都记会让日志页淹没在噪音里，反而看不出真正发生的事。
func (s *Server) logOutbound(modelName, account, trace string, r outbound.Result) {
	if s.events == nil {
		return
	}
	changed := r.PromptApplied || r.SystemInjected || r.Sanitized ||
		r.ToolRepair.Changed() || r.EffortFrom != "" || r.ThinkingInjected
	if !changed {
		return
	}

	parts := make([]string, 0, 5)
	switch {
	case r.PromptApplied && r.PromptMode == outbound.PromptCustom:
		parts = append(parts, "提示词替换（删除客户端 system "+strconv.Itoa(r.DroppedSystemMessages)+" 条）")
	case r.PromptApplied:
		parts = append(parts, "提示词追加（保留客户端 system）")
	}
	if r.SystemInjected {
		parts = append(parts, "补保底 system")
	}
	if r.ToolRepair.Changed() {
		parts = append(parts, "工具序列自愈")
	}
	if r.EffortFrom != "" {
		parts = append(parts, "档位降级 "+r.EffortFrom+"→"+r.EffortTo)
	}
	if r.Sanitized {
		parts = append(parts, "指纹脱敏")
	}

	fields := map[string]any{
		"trace_id":        trace,
		"model":           modelName,
		"account":         account,
		"prompt_mode":     r.PromptMode,
		"sanitized":       r.Sanitized,
		"system_injected": r.SystemInjected,
	}
	if r.PromptApplied {
		fields["dropped_system_messages"] = r.DroppedSystemMessages
	}
	if r.ToolRepair.Changed() {
		fields["messages_before"] = r.ToolRepair.OriginalMessages
		fields["messages_after"] = r.ToolRepair.FinalMessages
		fields["parallel_batches"] = r.ToolRepair.ParallelBatches
		fields["dropped_calls"] = r.ToolRepair.DroppedCalls
		fields["dropped_outputs"] = r.ToolRepair.DroppedOutputs
		fields["topology_after"] = r.ToolRepair.TopologyAfter
	}
	if r.EffortFrom != "" {
		fields["effort_from"] = r.EffortFrom
		fields["effort_to"] = r.EffortTo
	}
	s.events.Info(eventlog.ChannelOutbound, "outbound_rewrite",
		"出站改写："+strings.Join(parts, "、"), fields)
}
