package localsessions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// 客户端运行门禁 + 操作日志/恢复 + 预览凭据的测试
//
// 全部用替身：**绝不能在跑测试的机器上真去关掉用户的 WorkBuddy 客户端**
// （TestMain 已装兜底替身，见 clientgate_test.go）。
// -----------------------------------------------------------------------------

// 复制门禁：目标账号正是该档位当前登录账号、且客户端在运行时**拒绝写入**。
//
// 这是与整组统一**刻意不同**的规则：写「非当前登录账号」的副本实测免关安全
// （客户端不使用那份数据），写「当前登录账号」必须等它停止写入。
func TestCopyBlockedWhenTargetClientRunning(t *testing.T) {
	s, _ := setupStore(t)
	w := newFakeClientWorld(t, &fakeClientWorld{
		running: map[string]bool{"cn": true},
		uids:    map[string]string{"cn": "uid-B"},
	})
	_, err := s.CopySession("src-1111", CopyOptions{TargetUID: "uid-B"})
	if err == nil || !strings.Contains(err.Error(), "目标账号正在 WorkBuddy 中使用") {
		t.Fatalf("目标账号正在客户端里用时应拒绝复制，实际 %v", err)
	}

	// 客户端在跑，但登录的是**别的**账号 → 放行（免关安全）。
	w.uids["cn"] = "uid-OTHER"
	if _, err := s.CopySession("src-1111", CopyOptions{TargetUID: "uid-B"}); err != nil {
		t.Fatalf("登录的是别的账号时应放行，实际 %v", err)
	}

	// 读不到登录态 → 保守拦截（宁可要求退出，不做无法判定的写入）。
	delete(w.uids, "cn")
	if _, err := s.CopySession("src-1111", CopyOptions{TargetUID: "uid-B"}); err == nil {
		t.Fatal("读不到登录态时应保守拦截")
	}
}

// 整组统一：只关闭「运行中且当前登录是写入目标」的档位，写完后重新打开。
func TestUnifyClosesAndReopensTargetClient(t *testing.T) {
	s, groupID := setupLinkStore(t)
	// uid-C 是组里的副本账号，把副本正文截成前缀造一个落后目标。
	store, _, _ := s.loadLinkStore()
	g := findGroupR(store, groupID)
	var srcM, behindM *LinkMember
	for i := range g.Members {
		if g.Members[i].UID == "uid-A" {
			srcM = &g.Members[i]
		}
		if g.Members[i].UID == "uid-C" {
			behindM = &g.Members[i]
		}
	}
	if srcM == nil || behindM == nil {
		t.Fatalf("夹具应有 uid-A / uid-C 两个成员：%+v", g.Members)
	}
	body, ok := s.bodyPathIn(s.libByVariant(behindM.Variant), behindM.SessionID, "D:\\ws")
	if !ok {
		t.Fatal("找不到副本正文")
	}
	if err := os.WriteFile(body, []byte(buildJSONL(behindM.SessionID, "a")), 0o600); err != nil {
		t.Fatal(err)
	}

	w := newFakeClientWorld(t, &fakeClientWorld{
		running: map[string]bool{"cn": true},
		uids:    map[string]string{"cn": "uid-C"},
	})
	preview := mustPreviewToken(t, s, groupID, srcM.MemberID, behindM.MemberID)
	report, err := s.UnifyGroup(groupID, srcM.MemberID, map[string]UnifyTarget{
		behindM.MemberID: {Mode: ModeFastForward, PreviewToken: preview},
	})
	if err != nil {
		t.Fatalf("统一失败: %v", err)
	}
	if len(report.Synced) != 1 {
		t.Fatalf("应有 1 项写入，实际 %+v", report)
	}
	if len(w.closed) != 1 || w.closed[0] != "cn" {
		t.Fatalf("应关闭 cn 档客户端，实际 %v", w.closed)
	}
	if len(w.opened) != 1 || w.opened[0] != "cn" {
		t.Fatalf("写完后应重新打开 cn 档客户端，实际 %v", w.opened)
	}
	if len(report.RestartedVariants) != 1 || report.RestartedVariants[0] != "cn" {
		t.Fatalf("报告里应带重开的档位，实际 %+v", report.RestartedVariants)
	}
}

// 客户端**关不掉**时放弃写入（不「假装写入」），且不把已关的客户端留在关着的状态。
func TestUnifyAbortsWhenClientCannotClose(t *testing.T) {
	s, groupID := setupLinkStore(t)
	store, _, _ := s.loadLinkStore()
	g := findGroupR(store, groupID)
	var srcM, behindM *LinkMember
	for i := range g.Members {
		if g.Members[i].UID == "uid-A" {
			srcM = &g.Members[i]
		}
		if g.Members[i].UID == "uid-C" {
			behindM = &g.Members[i]
		}
	}
	body, _ := s.bodyPathIn(s.libByVariant(behindM.Variant), behindM.SessionID, "D:\\ws")
	before, _ := os.ReadFile(body)

	w := newFakeClientWorld(t, &fakeClientWorld{
		running:  map[string]bool{"cn": true},
		uids:     map[string]string{"cn": "uid-C"},
		closeErr: os.ErrPermission,
	})
	preview := mustPreviewToken(t, s, groupID, srcM.MemberID, behindM.MemberID)
	_, err := s.UnifyGroup(groupID, srcM.MemberID, map[string]UnifyTarget{
		behindM.MemberID: {Mode: ModeFastForward, PreviewToken: preview},
	})
	if err == nil || !strings.Contains(err.Error(), "关闭") {
		t.Fatalf("关不掉客户端时应报错，实际 %v", err)
	}
	after, _ := os.ReadFile(body)
	if string(before) != string(after) {
		t.Fatal("关不掉客户端时不该写入任何内容")
	}
	if len(w.opened) != 0 {
		t.Fatalf("关不掉时不该重开任何客户端，实际 %v", w.opened)
	}
}

// 未完成的复制会拒绝同一请求重复创建（避免写出第二个副本）。
func TestPendingOperationBlocksDuplicateCopy(t *testing.T) {
	s, _ := setupStore(t)
	// 手工造一条未完成操作（真实崩溃现场：正文已写、行未插）。
	srcBody := filepath.Join(s.Root, "projects", "d-software-workbuddy-gateway", "src-1111.jsonl")
	text, err := os.ReadFile(srcBody)
	if err != nil {
		t.Fatal(err)
	}
	norm, err := normalizeJSONL(string(text), "src-1111")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ensureLinkStoreReady(); err != nil {
		t.Fatal(err)
	}
	op := &Operation{
		Version: OperationVersion, OperationID: mustNewUUID(), Kind: OperationKindCopy,
		Variant: "cn", GroupID: mustNewUUID(),
		Source:                OperationMember{UID: "uid-A", SessionID: "src-1111"},
		Target:                OperationMember{UID: "uid-B", SessionID: "half-done"},
		ExpectedContentDigest: norm.TotalDigest, ExpectedRecordCount: norm.RecordCount,
		// 阶段停在「已预分配、尚未写正文」——这是「写完操作日志就被杀」的真实现场。
		Phase: PhasePrepared, CreatedAt: nowMillis(), UpdatedAt: nowMillis(),
	}
	if err := s.saveOperation(op); err != nil {
		t.Fatal(err)
	}
	// 再次发起同一请求：复制入口**先恢复**再判重 —— 上一次没做完的那份被补齐，
	// 于是本次变成「已有关联副本」的幂等结果，**不会写出第二个副本**。
	_, err = s.CopySession("src-1111", CopyOptions{TargetUID: "uid-B"})
	var already *AlreadyLinkedError
	if !errors.As(err, &already) {
		t.Fatalf("应先补齐未完成操作再报「已有关联副本」，实际 %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "projects", "d-software-workbuddy-gateway", "half-done.jsonl")); err != nil {
		t.Fatalf("恢复应补写出正文：%v", err)
	}
	owner, err := s.sessionRowOwner(s.libByVariant("cn"), "half-done")
	if err != nil || owner != "uid-B" {
		t.Fatalf("恢复应补插会话行并归属 uid-B，实际 %q err=%v", owner, err)
	}
	// 补齐后不再有待恢复项，且组内 uid-B 只有一个有效成员（没有第二个副本）。
	if left := s.pendingOperations("cn"); len(left) != 0 {
		t.Fatalf("补齐后不该还有未完成操作：%+v", left)
	}
	store, _, _ := s.loadLinkStore()
	g := findGroupForIdentity(store, "cn", "uid-A", "src-1111")
	if g == nil {
		t.Fatal("恢复应把关联也补上")
	}
	n := 0
	for _, m := range g.Members {
		if m.UID == "uid-B" && m.State == MemberStateActive {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("uid-B 应恰好有 1 个有效成员，实际 %d", n)
	}
}

// 恢复不了的未完成操作会让复制直接失败（不绕过 pending 去重写第二个副本）。
func TestUnrecoverablePendingBlocksCopy(t *testing.T) {
	s, _ := setupStore(t)
	if err := s.ensureLinkStoreReady(); err != nil {
		t.Fatal(err)
	}
	op := &Operation{
		Version: OperationVersion, OperationID: mustNewUUID(), Kind: OperationKindCopy,
		Variant: "cn", GroupID: mustNewUUID(),
		Source:                OperationMember{UID: "uid-A", SessionID: "src-1111"},
		Target:                OperationMember{UID: "uid-B", SessionID: "vanished"},
		ExpectedContentDigest: "abc", ExpectedRecordCount: 1,
		Phase: PhaseBodyWritten, CreatedAt: nowMillis(), UpdatedAt: nowMillis(),
	}
	if err := s.saveOperation(op); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CopySession("src-1111", CopyOptions{TargetUID: "uid-B"}); err == nil {
		t.Fatal("存在恢复不了的未完成操作时应拒绝复制")
	}
}

// 阶段说「正文已写」但正文不见了 → 停止恢复（不猜、不重放）。
func TestRecoverRefusesWhenBodyVanished(t *testing.T) {
	s, _ := setupStore(t)
	if err := s.ensureLinkStoreReady(); err != nil {
		t.Fatal(err)
	}
	op := &Operation{
		Version: OperationVersion, OperationID: mustNewUUID(), Kind: OperationKindCopy,
		Variant: "cn", GroupID: mustNewUUID(),
		Source:                OperationMember{UID: "uid-A", SessionID: "src-1111"},
		Target:                OperationMember{UID: "uid-B", SessionID: "never-written"},
		ExpectedContentDigest: "abc", ExpectedRecordCount: 1,
		Phase: PhaseBodyWritten, CreatedAt: nowMillis(), UpdatedAt: nowMillis(),
	}
	if err := s.saveOperation(op); err != nil {
		t.Fatal(err)
	}
	report := s.RecoverPendingOperations("cn")
	if len(report.NeedsRecovery) != 1 || !strings.Contains(report.NeedsRecovery[0].Reason, "目标内容丢失") {
		t.Fatalf("正文丢失应停止恢复，实际 %+v", report)
	}
}

// 恢复拒绝覆盖被改动的中间产物（只上报 needsRecovery，不猜）。
func TestRecoverRefusesModifiedProduct(t *testing.T) {
	s, _ := setupStore(t)
	if err := s.ensureLinkStoreReady(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Root, "projects", "d-software-workbuddy-gateway")
	// 造一个「正文已写，但内容与操作记录不一致」的现场。
	tampered := filepath.Join(dir, "tampered.jsonl")
	if err := os.WriteFile(tampered, []byte(buildJSONL("tampered", "x")), 0o600); err != nil {
		t.Fatal(err)
	}
	op := &Operation{
		Version: OperationVersion, OperationID: mustNewUUID(), Kind: OperationKindCopy,
		Variant: "cn", GroupID: mustNewUUID(),
		Source:                OperationMember{UID: "uid-A", SessionID: "src-1111"},
		Target:                OperationMember{UID: "uid-B", SessionID: "tampered"},
		ExpectedContentDigest: "0000000000000000000000000000000000000000000000000000000000000000",
		ExpectedRecordCount:   1,
		Phase:                 PhaseBodyWritten, CreatedAt: nowMillis(), UpdatedAt: nowMillis(),
	}
	if err := s.saveOperation(op); err != nil {
		t.Fatal(err)
	}
	report := s.RecoverPendingOperations("cn")
	if len(report.NeedsRecovery) != 1 {
		t.Fatalf("被改动的中间产物应上报 needsRecovery，实际 %+v", report)
	}
	if report.NeedsRecovery[0].Retryable {
		t.Fatal("中间产物被改动必须标记为不可重试（不得盲目重放）")
	}
	if report.Clean() {
		t.Fatal("needsRecovery 时 Clean() 必须为 false（宿主据此暂停重开客户端）")
	}
	// 内容未被覆盖。
	after, _ := os.ReadFile(tampered)
	if !strings.Contains(string(after), "x") {
		t.Fatal("恢复不该覆盖无法确认的内容")
	}
}

// 无法解析的操作日志同样阻断恢复（不能当成「没有未完成操作」继续写）。
func TestUnparseableOperationBlocksRecovery(t *testing.T) {
	s, _ := setupStore(t)
	if err := s.ensureLinkStoreReady(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.operationsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.operationsDir(), "broken.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := s.RecoverPendingOperations("cn")
	if len(report.NeedsRecovery) != 1 || !strings.Contains(report.NeedsRecovery[0].Reason, UnparseableOperationReason) {
		t.Fatalf("解析失败必须上报阻断项，实际 %+v", report)
	}
	if _, err := s.CopySession("src-1111", CopyOptions{TargetUID: "uid-B"}); err == nil {
		t.Fatal("存在无法解析的操作记录时应拒绝复制（避免写出第二个副本）")
	}
}

// 组结构指纹：成员替换/新增会改变指纹；无关组的变化不影响它。
func TestGroupFingerprintTracksMembers(t *testing.T) {
	g := &LinkGroupR{
		ID: "g1", Variant: "cn",
		Members: []LinkMember{
			{MemberID: "m1", UID: "u1", SessionID: "s1", State: MemberStateActive},
			{MemberID: "m2", UID: "u2", SessionID: "s2", State: MemberStateActive},
		},
		PairBases: []PairBase{{MemberIDs: [2]string{"m1", "m2"}, BaselineRef: "b1", NormalizationVersion: NormalizationVersion}},
	}
	base := groupFingerprint(g)
	// 顺序无关。
	reordered := *g
	reordered.Members = []LinkMember{g.Members[1], g.Members[0]}
	if groupFingerprint(&reordered) != base {
		t.Fatal("成员顺序变化不该改变指纹")
	}
	// 新增成员必须改变指纹。
	added := *g
	added.Members = append(append([]LinkMember{}, g.Members...), LinkMember{MemberID: "m3", UID: "u3", SessionID: "s3", State: MemberStateActive})
	if groupFingerprint(&added) == base {
		t.Fatal("新增成员必须改变指纹")
	}
	// 基线引用变化必须改变指纹。
	rebased := *g
	rebased.PairBases = []PairBase{{MemberIDs: [2]string{"m1", "m2"}, BaselineRef: "b2", NormalizationVersion: NormalizationVersion}}
	if groupFingerprint(&rebased) == base {
		t.Fatal("基线变化必须改变指纹")
	}
}

// 伪造/不存在的凭据 id 一律读不到（不能靠构造 id 扩大权限）。
func TestPreviewTokenRejectsForgedIDs(t *testing.T) {
	s, groupID := setupLinkStore(t)
	if s.loadPreviewToken("../../etc/passwd") != nil {
		t.Fatal("非法 id 必须读不到")
	}
	if s.loadPreviewToken("11111111-2222-3333-4444-555555555555") != nil {
		t.Fatal("不存在的 id 必须读不到")
	}
	store, _, _ := s.loadLinkStore()
	g := findGroupR(store, groupID)
	p, err := s.PreviewPair(groupID, g.Members[0].MemberID, g.Members[1].MemberID)
	if err != nil {
		t.Fatal(err)
	}
	if s.loadPreviewToken(p.PreviewToken) == nil {
		t.Fatal("自己签发的凭据必须能读回")
	}
}
