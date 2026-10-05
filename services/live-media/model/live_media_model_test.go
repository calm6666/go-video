package model

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 本文件只测不连库的纯逻辑：状态机迁移表、序号单调、分页边界、条件聚合缺口、
// 时钟注入下的超时判定，以及"条件 UPDATE 绝不退化成全表更新"的守卫。
// 任何需要真实 MySQL 的用例都不在这里（AGENTS.md §9：禁止用假实现让测试变绿）。

func TestTranscodeStateTransition(t *testing.T) {
	cases := []struct {
		name string
		from int32
		to   int32
		ok   bool
	}{
		{"worker 领取", TranscodeStatePending, TranscodeStateRunning, true},
		{"心跳自更新", TranscodeStateRunning, TranscodeStateRunning, true},
		{"请求停止", TranscodeStateRunning, TranscodeStateStopping, true},
		{"超时判失败", TranscodeStateRunning, TranscodeStateFailed, true},
		{"重试只回 PENDING", TranscodeStateFailed, TranscodeStatePending, true},
		{"RUNNING 不得直落终态", TranscodeStateRunning, TranscodeStateStopped, false},
		{"PENDING 不能停", TranscodeStatePending, TranscodeStateStopping, false},
		{"终态不可复活", TranscodeStateStopped, TranscodeStateRunning, false},
		{"取消终态不可复活", TranscodeStateCancelled, TranscodeStatePending, false},
		{"FAILED 不能直接 RUNNING", TranscodeStateFailed, TranscodeStateRunning, false},
	}
	for _, c := range cases {
		if got := IsValidTranscodeTransition(c.from, c.to); got != c.ok {
			t.Errorf("%s: IsValidTranscodeTransition(%d,%d)=%v 期望 %v", c.name, c.from, c.to, got, c.ok)
		}
	}
	if !IsTranscodeTerminal(TranscodeStateStopped) || !IsTranscodeTerminal(TranscodeStateCancelled) {
		t.Error("STOPPED/CANCELLED 必须是终态")
	}
	if IsTranscodeTerminal(TranscodeStateFailed) {
		t.Error("FAILED 不是终态：它只能经 RetryLiveTranscode 回 PENDING")
	}
	if got := TranscodeTerminalStates(); len(got) != 2 {
		t.Errorf("TranscodeTerminalStates()=%v 应为 2 个终态", got)
	}
}

func TestRecordStateTransition(t *testing.T) {
	if !IsValidRecordTransition(RecordStateFailed, RecordStateRecording) {
		t.Error("FAILED→RECORDING 必须允许（断点续录从 last_seq+1 继续）")
	}
	if !IsValidRecordTransition(RecordStateRecording, RecordStateRecording) {
		t.Error("RECORDING 心跳自更新必须允许")
	}
	if IsValidRecordTransition(RecordStateStopped, RecordStateRecording) {
		t.Error("STOPPED 是终态，不能被迟到心跳改回录制中")
	}
	if IsValidRecordTransition(RecordStatePending, RecordStateStopped) {
		t.Error("PENDING 不得直落 STOPPED：终态只能由 Worker 收尾上报")
	}
	if IsRecordTerminal(RecordStateStopping) {
		t.Error("STOPPING 不是终态")
	}
}

func TestSegmentStateTransition(t *testing.T) {
	// 正向推进与幂等重放
	if !IsValidSegmentTransition(SegmentStateUploading, SegmentStateUploaded) ||
		!IsValidSegmentTransition(SegmentStateUploaded, SegmentStateVerified) {
		t.Error("UPLOADING→UPLOADED→VERIFIED 必须允许")
	}
	if !IsValidSegmentTransition(SegmentStateVerified, SegmentStateVerified) {
		t.Error("同状态重放必须允许（ReportRecordSegment 幂等依赖这一点）")
	}
	// 绝不退化
	if IsValidSegmentTransition(SegmentStateVerified, SegmentStateUploaded) {
		t.Error("已校验切片不得退回未校验")
	}
	if IsValidSegmentTransition(SegmentStateCorrupt, SegmentStateMissing) {
		t.Error("已判损坏不得再改判缺失")
	}
	if IsValidSegmentTransition(SegmentStateCorrupt, SegmentStateVerified) {
		t.Error("损坏切片永不参与拼接，不得被改回可拼接")
	}
	// 补录：MISSING 可回 UPLOADING/UPLOADED，但不能直接 VERIFIED（必须重新校验）
	if !IsValidSegmentTransition(SegmentStateMissing, SegmentStateUploading) ||
		!IsValidSegmentTransition(SegmentStateMissing, SegmentStateUploaded) {
		t.Error("缺口补录必须允许")
	}
	if IsValidSegmentTransition(SegmentStateMissing, SegmentStateVerified) {
		t.Error("缺口补录不得跳过校验直接 VERIFIED")
	}
	if r := SegmentStateRank(SegmentStateMissing); r != -1 {
		t.Errorf("SegmentStateRank(MISSING)=%d 应为 -1（不参与前进排名）", r)
	}
	if SegmentStateRank(SegmentStateUploading) >= SegmentStateRank(SegmentStateVerified) {
		t.Error("正常链路的推进序必须递增")
	}
}

// TestSegmentFromStatesNoRegression 锁定「条件 UPDATE 的前置状态集合」与状态机表一致：
// 非法回退必须在 SQL 层就匹配 0 行，而不是靠 logic 事后判断。
func TestSegmentFromStatesNoRegression(t *testing.T) {
	// VERIFIED 只能由 UPLOADED 推进（外加幂等自更新，供 Worker 重复回报同一个切片）。
	// MISSING/UPLOADING 必须不在集合里：补录的切片不得跳过校验直接 VERIFIED。
	verifiedFroms := segmentFromStates(SegmentStateVerified)
	if len(verifiedFroms) != 2 {
		t.Fatalf("VERIFIED 的前置集合应为 {UPLOADED, VERIFIED(幂等)}，实际 %v", verifiedFroms)
	}
	for _, s := range verifiedFroms {
		if s != SegmentStateUploaded && s != SegmentStateVerified {
			t.Errorf("VERIFIED 的前置状态出现 %d，会把未校验切片放行拼接", s)
		}
	}
	// CORRUPT 的前置集合包含 VERIFIED：上传时校验通过、事后对象被删/损坏，必须允许改判，
	// 否则回放拼接会引用不存在的对象。反向不对称：CORRUPT 不得回 MISSING（见上一用例）。
	corruptFroms := segmentFromStates(SegmentStateCorrupt)
	if !containsState(corruptFroms, SegmentStateVerified) {
		t.Error("VERIFIED 必须在 CORRUPT 的前置集合里：对象事后丢失要能改判损坏")
	}
	if !containsState(corruptFroms, SegmentStateMissing) {
		t.Error("MISSING 必须在 CORRUPT 的前置集合里：补录到达后要能撤销缺口标记")
	}
	if got := segmentFromStates(99); len(got) != 0 {
		t.Errorf("未知目标状态应返回空集合（UpdateState 据此拒绝），实际 %v", got)
	}
}

func containsState(states []int32, want int32) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

func TestReplayStateTransitionKeepsPublishPathSingle(t *testing.T) {
	// 硬约束：COMPLETED 只能由 video 侧投影（ApplyReplayContentState）驱动，
	// 其它任何阶段都不允许一步到 COMPLETED。
	for _, from := range []int32{ReplayStatePending, ReplayStateMerging, ReplayStateUploading,
		ReplayStateRegistered, ReplayStateFailed, ReplayStateCancelled} {
		if IsValidReplayTransition(from, ReplayStateCompleted) {
			t.Errorf("state=%d 不得直达 COMPLETED", from)
		}
	}
	if !IsValidReplayTransition(ReplayStateReviewSubmitted, ReplayStateCompleted) {
		t.Error("REVIEW_SUBMITTED→COMPLETED 必须允许（唯一入口）")
	}
	if IsValidReplayTransition(ReplayStateUploading, ReplayStateReviewSubmitted) {
		t.Error("UPLOADING 不能跳过 REGISTERED 直接送审")
	}
	if !IsReplayTerminal(ReplayStateCompleted) || IsReplayTerminal(ReplayStateReviewSubmitted) {
		t.Error("COMPLETED 是终态、REVIEW_SUBMITTED 不是")
	}
}

func TestRetentionAndRefLifecycle(t *testing.T) {
	if !IsValidRetentionTransition(RetentionStatePending, RetentionStateRunning) ||
		!IsValidRetentionTransition(RetentionStateRunning, RetentionStateSucceeded) {
		t.Error("回收任务的 登记→执行→完成 主链路必须允许")
	}
	if IsValidRetentionTransition(RetentionStateSucceeded, RetentionStateRunning) {
		t.Error("终态不得被迟到上报改回执行中")
	}
	if !IsRetentionTerminal(RetentionStateFailed) || IsRetentionTerminal(RetentionStatePending) {
		t.Error("FAILED 是终态、PENDING 不是")
	}
	if ValidRetentionTarget(RetentionTargetStreamOutput+1) || ValidRetentionTarget(0) {
		t.Error("target_kind 取值边界判定错误")
	}
	if !IsValidRefRetentionTransition(RefRetentionStateNormal, RefRetentionStatePending) ||
		!IsValidRefRetentionTransition(RefRetentionStatePending, RefRetentionStateReclaimed) {
		t.Error("引用行生命周期 正常→待回收→已回收 必须允许")
	}
	if IsValidRefRetentionTransition(RefRetentionStateReclaimed, RefRetentionStateNormal) {
		t.Error("已回收的产物不可回到正常（只能由新回放产生新引用行）")
	}
	if ValidReviewState(ReviewStateUnsynced) || !ValidReviewState(ReviewStatePublished) ||
		ValidReviewState(ReviewStateDeleted+1) {
		t.Error("投影取值边界：0 不是合法同步值，1..5 合法")
	}
}

func TestSegmentStatsGaps(t *testing.T) {
	// 区间 [1,10]：登记 8 行（其中 2 个 MISSING）→ 2 个隐式空洞 + 2 个显式缺口 = 4
	st := SegmentStats{Registered: 8, Verified: 6, Missing: 2}
	if got := st.Gaps(10); got != 4 {
		t.Errorf("Gaps(10)=%d 期望 4", got)
	}
	// 登记数超过期望（不该发生，但绝不返回负数缺口）
	if got := (SegmentStats{Registered: 12}).Gaps(10); got != 0 {
		t.Errorf("Gaps 越界保护失效：%d", got)
	}
	if got := (SegmentStats{Registered: 10, Verified: 10}).Gaps(10); got != 0 {
		t.Errorf("完整区间缺口应为 0，实际 %d", got)
	}
}

func TestClampPageBoundaries(t *testing.T) {
	cases := []struct {
		pn, ps, maxPS int32
		wantLimit     int32
		wantOffset    int32
		why           string
	}{
		{1, 20, 50, 20, 0, "常规首页"},
		{0, 20, 50, 20, 0, "pn<1 归一到 1"},
		{-5, 20, 50, 20, 0, "负页码同样归一"},
		{3, 10, 50, 10, 20, "正常翻页 offset=(pn-1)*ps"},
		{1, 0, 50, 20, 0, "ps<=0 取默认 20"},
		{1, 9999, 50, 20, 0, "ps 超限夹到默认 20（proto：服务端夹取不报错）"},
		{1, 30, 0, 30, 0, "maxPS<=0 时内部按 50 兜底，30 合法保留"},
		{2, 25, 25, 25, 25, "ps 恰等于上限时不夹取"},
		{4, 10, 10, 10, 30, "ps=上限且翻页"},
	}
	for _, c := range cases {
		limit, offset := clampPage(c.pn, c.ps, c.maxPS)
		if limit != c.wantLimit || offset != c.wantOffset {
			t.Errorf("%s: clampPage(%d,%d,%d)=(%d,%d) 期望 (%d,%d)",
				c.why, c.pn, c.ps, c.maxPS, limit, offset, c.wantLimit, c.wantOffset)
		}
	}
	// maxPS 小于默认 20 时，limit 不能超过 maxPS
	if limit, _ := clampPage(1, 0, 5); limit != 5 {
		t.Errorf("maxPS=5 时默认页大小应被压到 5，实际 %d", limit)
	}
}

func TestBuildWhereNeverDegrades(t *testing.T) {
	where, args := buildWhere()
	if where != "WHERE 1=1" || len(args) != 0 {
		t.Errorf("无条件时应返回 WHERE 1=1 且无参数，实际 %q %v", where, args)
	}
	where, args = buildWhere(
		whereFragment{"room_id = ?", []any{}}.when(true, int64(7)),
		whereFragment{"state = ?", []any{}}.when(false, int32(2)),
		whereFragment{"live_session_id = ?", []any{}}.when(true, int64(9)),
	)
	if where != "WHERE 1=1 AND room_id = ? AND live_session_id = ?" {
		t.Errorf("条件拼接结果异常：%q", where)
	}
	if len(args) != 2 || args[0] != int64(7) || args[1] != int64(9) {
		t.Errorf("参数顺序必须与 SQL 片段一致，实际 %v", args)
	}
	if f := (whereFragment{"x = ?", []any{}}.when(false, 1)); !f.empty() {
		t.Error("when(false) 必须返回空片段")
	}
}

func TestStateInFragmentPlaceholderCount(t *testing.T) {
	f := stateInFragment([]int32{TranscodeStatePending, TranscodeStateRunning, TranscodeStateStopping})
	if got := countByte(f.SQL, '?'); got != 3 {
		t.Errorf("占位符数量 %d 与状态数量不符：%s", got, f.SQL)
	}
	if len(f.Args) != 3 {
		t.Errorf("参数数量 %d 与状态数量不符", len(f.Args))
	}
	if f.SQL != "state IN (?, ?, ?)" {
		t.Errorf("SQL 形态不符合预期：%s", f.SQL)
	}
}

// TestConditionalUpdateRefusesDangerousSQL 守住"条件 UPDATE 不退化成全表更新"：
// 这两种入参都必须在触碰连接之前被拒绝（conn 传 nil，一旦真的执行就会 panic）。
func TestConditionalUpdateRefusesDangerousSQL(t *testing.T) {
	if _, err := conditionalUpdate(context.Background(), nil, "live_transcode_task",
		nil, true, []whereFragment{{"task_id = ?", []any{int64(1)}}}); err == nil {
		t.Error("空 SET 必须被拒绝")
	}
	if _, err := conditionalUpdate(context.Background(), nil, "live_transcode_task",
		[]columnValue{{"state", TranscodeStateRunning}}, true, nil); err == nil {
		t.Error("空 WHERE 必须被拒绝（等于全表更新）")
	}
	if _, err := conditionalUpdate(context.Background(), nil, "live_transcode_task",
		[]columnValue{{"state", TranscodeStateRunning}}, true,
		[]whereFragment{{"", nil}}); err == nil {
		t.Error("只有空片段时同样必须拒绝")
	}
	// fromStates 为空的 UpdateState 走的是同一道防线（错误里带 ErrInvalidTransition）
	m := &defaultLiveTranscodeTaskModel{}
	if _, err := m.UpdateState(context.Background(), 1, nil, 0, TranscodeStateRunning, TranscodePatch{}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("UpdateState 空 fromStates 应返回 ErrInvalidTransition，实际 %v", err)
	}
	sm := &defaultLiveRecordSegmentModel{}
	if _, err := sm.UpdateState(context.Background(), 1, 1, 99, SegmentPatch{}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("未知目标状态应返回 ErrInvalidTransition，实际 %v", err)
	}
}

// TestModelGuardsRejectBadInput 覆盖不触库就返回错误的入参守卫（seq/引用主键/投影值/幂等键）。
func TestModelGuardsRejectBadInput(t *testing.T) {
	ctx := context.Background()
	seg := &defaultLiveRecordSegmentModel{}
	if _, err := seg.InsertIgnore(ctx, &LiveRecordSegment{Seq: 0, State: SegmentStateVerified,
		Bucket: "b", ObjectKey: "k"}); !errors.Is(err, ErrInvalidSeq) {
		t.Errorf("seq=0 应返回 ErrInvalidSeq，实际 %v", err)
	}
	if _, err := seg.InsertIgnore(ctx, &LiveRecordSegment{Seq: 1, State: SegmentStateVerified,
		Bucket: "", ObjectKey: ""}); !errors.Is(err, ErrInvalidBucketRef) {
		t.Errorf("VERIFIED 无引用应返回 ErrInvalidBucketRef，实际 %v", err)
	}
	// MISSING 行允许无引用（产物本就不存在）
	if _, err := seg.StatsInRange(ctx, 1, 5, 1); !errors.Is(err, ErrInvalidSegmentRange) {
		t.Errorf("to_seq<from_seq 应返回 ErrInvalidSegmentRange，实际 %v", err)
	}
	rt := &defaultLiveReplayTaskModel{}
	if _, err := rt.FindByAssetID(ctx, 0); !errors.Is(err, ErrInvalidAssetID) {
		t.Errorf("asset_id=0 是占位值，不得用于反查，实际 %v", err)
	}
	rr := &defaultLiveReplayAssetRefModel{}
	if _, err := rr.FindByAid(ctx, 0); !errors.Is(err, ErrInvalidAid) {
		t.Errorf("aid=0 不得用于反查，实际 %v", err)
	}
	if _, err := rr.ApplyContentState(ctx, ContentStateApply{ReplayId: 1, ReviewState: ReviewStateUnsynced,
		EventId: "e"}); !errors.Is(err, ErrInvalidReviewState) {
		t.Errorf("投影值 0 非法，实际 %v", err)
	}
	if _, err := rr.ApplyContentState(ctx, ContentStateApply{ReplayId: 1, ReviewState: ReviewStatePublished,
		EventId: ""}); !errors.Is(err, ErrEmptyEventID) {
		t.Errorf("缺 event_id 必须拒绝（无幂等依据），实际 %v", err)
	}
	if _, err := rr.ApplyContentState(ctx, ContentStateApply{ReviewState: ReviewStatePublished,
		EventId: "e"}); !errors.Is(err, ErrReplayRefNotFound) {
		t.Errorf("replay_id 与 asset_id 都缺时必须拒绝而不是全表刷新，实际 %v", err)
	}
	if _, _, err := (&defaultLiveStreamOutputModel{}).ListByRoom(ctx, 0, 0, false, 1, 20, 50); !errors.Is(err, ErrInvalidRoomID) {
		t.Errorf("档位列表必须限定房间，实际 %v", err)
	}
	if _, err := (&defaultLiveRetentionTaskModel{}).Insert(ctx, &LiveRetentionTask{TargetKind: 9,
		TargetId: 1, Reason: "r"}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("非法 target_kind 应被拒绝，实际 %v", err)
	}
	if _, err := (&defaultLiveRetentionTaskModel{}).Insert(ctx, &LiveRetentionTask{
		TargetKind: RetentionTargetSegment, TargetId: 0, ExpireBefore: 0, Reason: "r"}); !errors.Is(err, ErrRetentionTargetRequired) {
		t.Errorf("既无 target_id 又无 expire_before 必须拒绝，实际 %v", err)
	}
	if _, err := (&defaultLiveRetentionTaskModel{}).Insert(ctx, &LiveRetentionTask{
		TargetKind: RetentionTargetSegment, TargetId: 1, Reason: ""}); !errors.Is(err, ErrRetentionReasonRequired) {
		t.Errorf("回收原因为空必须拒绝（审计必填），实际 %v", err)
	}
	if err := (&defaultLiveMediaOutboxModel{}).Insert(ctx, nil, &LiveMediaOutbox{EventId: "e"}); !errors.Is(err, ErrEmptyEventPayload) {
		t.Errorf("事件缺类型/聚合必须拒绝，实际 %v", err)
	}
	// Insert 的纯校验分支必须在触库之前返回（conn==nil 若被使用会立刻 panic，测试即失败）
	if _, err := seg.InsertIgnoreMissing(ctx, []*LiveRecordSegment{{Seq: -1}}); !errors.Is(err, ErrInvalidSeq) {
		t.Errorf("批量补洞的非法序号应被拒绝，实际 %v", err)
	}
}

// TestClockInjectionDrivesTimeout 用注入时钟验证"心跳 + 超时秒数 = 超时判定时刻"：
// 超时清扫与续录判定全部依赖这个关系，时钟不同源就会误判。
func TestClockInjectionDrivesTimeout(t *testing.T) {
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	restore := SetClock(func() time.Time { return base })
	hb := nowUnix()
	if hb != base.Unix() {
		restore()
		t.Fatalf("注入时钟未生效：%d != %d", hb, base.Unix())
	}
	timeoutSeconds := int64(60) // 与配置 DefaultTranscodeTimeoutSeconds 同量级
	timeoutAt := hb + timeoutSeconds
	// 心跳后 59 秒：租约仍有效
	if now := addUnix(base, 59); now >= timeoutAt {
		t.Error("59 秒时不应判超时")
	}
	// 心跳后 61 秒：ListTimedOut 的 timeout_at < now 条件成立
	if now := addUnix(base, 61); now < timeoutAt {
		t.Error("61 秒时应判超时")
	}
	restore()
	if nowUnix() == hb && time.Now().Unix() != hb {
		t.Error("恢复系统时钟失败")
	}
}

func TestIsDuplicateErr(t *testing.T) {
	if isDuplicateErr(nil) {
		t.Error("nil 不是冲突")
	}
	if !isDuplicateErr(errors.New("Error 1062 (23000): Duplicate entry 'r-1' for key 'uniq_record_seq'")) {
		t.Error("MySQL 1062 必须识别为唯一索引冲突")
	}
	if !isDuplicateErr(errors.New("Duplicate entry for key PRIMARY")) {
		t.Error("按文本判定的分支未覆盖")
	}
	if isDuplicateErr(errors.New("Error 1213 Deadlock found")) {
		t.Error("死锁不是唯一索引冲突，不能走幂等回放分支")
	}
}

func TestErrNotImplementedIsDistinct(t *testing.T) {
	if ErrNotImplemented == nil || ErrNotImplemented.Error() != "live-media: not implemented" {
		t.Errorf("占位哨兵错误不符合约定：%v", ErrNotImplemented)
	}
	if errors.Is(ErrVersionConflict, ErrInvalidTransition) {
		t.Error("不同语义的哨兵必须可区分（gRPC 错误映射依赖 errors.Is）")
	}
}

func countByte(s string, b byte) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			n++
		}
	}
	return n
}

func addUnix(base time.Time, seconds int64) int64 {
	return base.Add(time.Duration(seconds) * time.Second).Unix()
}
