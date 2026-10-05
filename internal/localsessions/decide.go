package localsessions

// -----------------------------------------------------------------------------
// 同步判定（对照 wb-switch 的 decide_sync，逐分支移植，顺序不可调换）：
//
//	1. 成员/文件无效或内容不可验证 → Unknown；
//	2. 双方有序一致 → Identical；
//	3. 目标是来源的严格有序前缀 → FastForward（不依赖基线，追加零覆盖）；
//	4. 无可验证基线：来源是目标的严格有序前缀 → Ahead（不依赖基线、不授权写入），
//	   其余 → Unknown；
//	5. 来源等于基线、目标已变化 → Ahead；
//	6. 来源是目标的严格有序前缀、目标已偏离基线 → Ahead（镜像规则；目标恰好停在
//	   基线属于来源侧被删减/回滚，保持下一条的显式覆盖入口）；
//	7. 其余 → Diverge。
//
// 纯函数：不读文件、不写文件、无时间依赖。
// 文案逐字对齐 switch（用户可见）。
// -----------------------------------------------------------------------------

import "fmt"

// 同步判定结论。
const (
	VerdictIdentical   = "identical"
	VerdictFastForward = "fastForward"
	VerdictAhead       = "ahead"
	VerdictDiverge     = "diverge"
	VerdictUnknown     = "unknown"
)

// 同步写入模式（design §6 的 mode）。
const (
	ModeFastForward    = "fastForward"
	ModeOverwrite      = "overwrite"
	ModeUnifyOverwrite = "unifyOverwrite"
)

// modeAllows 判定「该结论允许哪种写入模式」。unknown 不匹配任何模式——
// 覆盖不能绕过未知（与 switch 的 SyncVerdict::allows 一致）。
func modeAllows(verdict, mode string) bool {
	switch mode {
	case ModeFastForward:
		return verdict == VerdictFastForward
	case ModeOverwrite:
		return verdict == VerdictDiverge
	case ModeUnifyOverwrite:
		return verdict == VerdictAhead
	}
	return false
}

// availableModes 是前端可选的写入模式（空表示不可勾选）。
func availableModes(verdict string) []string {
	switch verdict {
	case VerdictFastForward:
		return []string{ModeFastForward}
	case VerdictDiverge:
		return []string{ModeOverwrite}
	}
	return []string{}
}

// parseMode 解析前端传入的模式；未知值直接拒绝，不回落默认值。
func parseMode(raw string) (string, error) {
	switch raw {
	case ModeFastForward:
		return ModeFastForward, nil
	case ModeOverwrite:
		return ModeOverwrite, nil
	case ModeUnifyOverwrite:
		return ModeUnifyOverwrite, nil
	}
	return "", fmt.Errorf("未知的同步模式：%s", raw)
}

// contentCounts 是解释性记录数：来源独有 / 目标独有 / 共同。
type contentCounts struct {
	SourceOnly int
	TargetOnly int
	Common     int
}

// multisetCounts 数两边摘要的多重集关系（对照 switch 的 multiset_counts）。
func multisetCounts(source, target []string) contentCounts {
	sc := map[string]int{}
	for _, d := range source {
		sc[d]++
	}
	tc := map[string]int{}
	for _, d := range target {
		tc[d]++
	}
	common := 0
	for d, n := range sc {
		if m := tc[d]; m > 0 {
			common += min(n, m)
		}
	}
	return contentCounts{
		SourceOnly: len(source) - common,
		TargetOnly: len(target) - common,
		Common:     common,
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// mirrorAheadReason 镜像前缀（目标已完整包含来源）的 ahead 文案。
func mirrorAheadReason(counts contentCounts) string {
	return fmt.Sprintf("目标账号已包含当前账号的全部内容，另多出 %d 条，本次不会同步过去", counts.TargetOnly)
}

// SyncDecision2 是一次判定的完整结果（verdict + 文案 + 记录数 + 可用模式）。
//
// 命名带 2 是为了与旧的 SyncDecision 区分（旧结构随内容推导模型一起退役）。
type SyncDecision2 struct {
	Verdict    string        `json:"verdict"`
	Reason     string        `json:"reason"`
	Counts     contentCounts `json:"-"`
	SourceOnly int           `json:"source_only"`
	TargetOnly int           `json:"target_only"`
	// AvailableModes 是前端可勾选的写入模式（组级统一可显式覆盖 ahead）。
	AvailableModes []string `json:"available_modes"`
}

func decideUnknown(reason string) SyncDecision2 {
	return SyncDecision2{Verdict: VerdictUnknown, Reason: reason, AvailableModes: []string{}}
}

func decideSyncVerdict(verdict, reason string, counts contentCounts) SyncDecision2 {
	return SyncDecision2{
		Verdict:        verdict,
		Reason:         reason,
		Counts:         counts,
		SourceOnly:     counts.SourceOnly,
		TargetOnly:     counts.TargetOnly,
		AvailableModes: availableModes(verdict),
	}
}

// 内容状态：Ready（可读）/ Missing（正文不存在）/ Unverifiable（读到了但解析失败）。
type contentState struct {
	ready  *NormalizedContent
	miss   bool
	reason string
}

func contentReady(n *NormalizedContent) contentState { return contentState{ready: n} }
func contentMissing() contentState                   { return contentState{miss: true} }
func contentUnverifiable(reason string) contentState { return contentState{reason: reason} }

// decideSync2 判定「把来源内容同步到目标」应当怎么做。
// source/target 为nil时按 Missing 处理（调用方把解析错误折算进 contentState）。
func decideSync2(source, target contentState, baseline *BaselineRecord, baselineState string, baselineUnusable string) SyncDecision2 {
	if source.miss {
		return decideUnknown("当前账号的内容不存在，无法确认")
	}
	if source.reason != "" {
		return decideUnknown("当前账号的内容无法确认：" + source.reason)
	}
	if target.miss {
		return decideUnknown("目标账号的内容不存在，无法确认")
	}
	if target.reason != "" {
		return decideUnknown("目标账号的内容无法确认：" + target.reason)
	}

	counts := multisetCounts(source.ready.LineDigests, target.ready.LineDigests)

	if equalDigests(source.ready.LineDigests, target.ready.LineDigests) {
		return decideSyncVerdict(VerdictIdentical,
			fmt.Sprintf("两边的内容一致（各 %d 条），不需要同步", source.ready.RecordCount), counts)
	}

	if isStrictOrderedExtension(target.ready.LineDigests, source.ready.LineDigests) {
		added := source.ready.RecordCount - target.ready.RecordCount
		return decideSyncVerdict(VerdictFastForward,
			fmt.Sprintf("目标账号没有独有改动，来源账号新增 %d 条，可以直接同步", added), counts)
	}

	if baseline == nil {
		// 无可验证基线：来源是目标的严格有序前缀 → ahead（不依赖基线、不授权写入）。
		if isStrictOrderedExtension(source.ready.LineDigests, target.ready.LineDigests) {
			return decideSyncVerdict(VerdictAhead, mirrorAheadReason(counts), counts)
		}
		if baselineState == "unverifiable" {
			return decideUnknown(baselineUnusable)
		}
		return decideUnknown("找不到双方上次一致的内容，暂时无法同步")
	}

	if equalDigests(source.ready.LineDigests, baseline.LineDigests) {
		return decideSyncVerdict(VerdictAhead,
			fmt.Sprintf("只有目标账号新增 %d 条，这次不会同步过去", counts.TargetOnly), counts)
	}

	// 来源是目标的严格有序前缀且目标已偏离基线 → ahead（镜像规则）；
	// 目标恰好停在基线 → 来源侧被删减/回滚，维持 diverge 的显式覆盖入口。
	if isStrictOrderedExtension(source.ready.LineDigests, target.ready.LineDigests) &&
		!equalDigests(target.ready.LineDigests, baseline.LineDigests) {
		return decideSyncVerdict(VerdictAhead, mirrorAheadReason(counts), counts)
	}

	return decideSyncVerdict(VerdictDiverge,
		fmt.Sprintf("两边都有改动（目标账号独有的 %d 条会被替换）；覆盖会替换目标账号的完整内容", counts.TargetOnly), counts)
}
