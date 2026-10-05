package logic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"
)

// 本文件钉住「rpc 枚举编号 == model 常量」这条隐式契约。
//
// 为什么必须钉：落库读的是 model.* 常量，跨进程传的是 rpc 枚举，
// 两者一旦漂移（比如有人给 rpc 中间插了一个新值），历史 rank_decision_log 的
// source/degraded/state 就会被解释成另一个含义 —— 审计数据集体失真，
// 而且这种 bug 在编译期完全看不出来。
//
// 与 recommend-recall 的同名编号对齐（rank.RankSource ↔ recall.Source、
// rank.Platform ↔ recall.Platform）由两边的常量值共同保证；
// 本测试不 import 兄弟服务的 rpc 包（AGENTS.md §5：跨服务只传主键，不共享生成代码），
// 对齐口径写死在下面表格中，任何一侧改号都会有一边的测试先红。

func TestRankSourceMatchesModelConstants(t *testing.T) {
	cases := []struct {
		name string
		rpc  int32
		want int32
	}{
		{"hot", int32(rpc.RankSource_RANK_SOURCE_HOT), model.SourceHot},
		{"follow", int32(rpc.RankSource_RANK_SOURCE_FOLLOW), model.SourceFollow},
		{"tag", int32(rpc.RankSource_RANK_SOURCE_TAG), model.SourceTag},
		{"collab", int32(rpc.RankSource_RANK_SOURCE_COLLAB), model.SourceCollab},
		{"vector", int32(rpc.RankSource_RANK_SOURCE_VECTOR), model.SourceVector},
		{"cold", int32(rpc.RankSource_RANK_SOURCE_COLD), model.SourceCold},
	}
	// 与 services/recommend-recall/rpc/recall.proto 的 SOURCE_* 逐一对齐（同名同值）。
	recallAligned := map[string]int32{"hot": 1, "follow": 2, "tag": 3, "collab": 4, "vector": 5, "cold": 6}
	for _, c := range cases {
		if c.rpc != c.want {
			t.Errorf("rpc RankSource(%s)=%d 与 model.Source*=%d 不一致", c.name, c.rpc, c.want)
		}
		if got := recallAligned[c.name]; got != c.want {
			t.Errorf("model.Source(%s) = %d 与 recommend-recall 的 SOURCE_%s=%d 不一致", c.name, c.want, c.name, got)
		}
		if !model.ValidSource(c.want) {
			t.Errorf("model.ValidSource(%s) 应为 true", c.name)
		}
	}
	if model.ValidSource(0) || model.ValidSource(7) {
		t.Error("UNSPECIFIED 与越界来源必须判定为非法，否则候选来源无法解释")
	}
}

func TestSourcePriorityMatchesRecall(t *testing.T) {
	// 兜底顺序（回退召回原序）必须与 recommend-recall 的 SourcePriority 完全同序，
	// 否则同一次请求在两个服务里会得到两条不同的「原序」，降级结果无法解释。
	want := []struct {
		source   int32
		priority int
	}{
		{model.SourceFollow, 1},
		{model.SourceCollab, 2},
		{model.SourceVector, 3},
		{model.SourceTag, 4},
		{model.SourceCold, 5},
		{model.SourceHot, 6},
	}
	if len(want) != 6 {
		t.Fatalf("候选来源共 6 种，本测试覆盖 %d 种", len(want))
	}
	seen := make(map[int]struct{}, len(want))
	for _, c := range want {
		if got := model.SourcePriority(c.source); got != c.priority {
			t.Errorf("SourcePriority(%d) = %d, want %d", c.source, got, c.priority)
		}
		seen[c.priority] = struct{}{}
	}
	if len(seen) != len(want) {
		t.Error("各路优先级必须严格互不相同，否则兜底顺序无法确定")
	}
	if got := model.SourcePriority(0); got != 99 {
		t.Errorf("未知来源的优先级应为 99（排最后），得到 %d", got)
	}
}

func TestStateEnumsMatchModel(t *testing.T) {
	modelCases := []struct {
		rpc  rpc.ModelVersionState
		want int32
	}{
		{rpc.ModelVersionState_MODEL_VERSION_STATE_DRAFT, model.ModelStateDraft},
		{rpc.ModelVersionState_MODEL_VERSION_STATE_READY, model.ModelStateReady},
		{rpc.ModelVersionState_MODEL_VERSION_STATE_ACTIVE, model.ModelStateActive},
		{rpc.ModelVersionState_MODEL_VERSION_STATE_RETIRED, model.ModelStateRetired},
	}
	for _, c := range modelCases {
		if int32(c.rpc) != c.want {
			t.Errorf("ModelVersionState_%d 与 model 常量 %d 不一致", int32(c.rpc), c.want)
		}
	}
	expCases := []struct {
		rpc  rpc.ExperimentState
		want int32
	}{
		{rpc.ExperimentState_EXPERIMENT_STATE_DRAFT, model.ExpStateDraft},
		{rpc.ExperimentState_EXPERIMENT_STATE_RUNNING, model.ExpStateRunning},
		{rpc.ExperimentState_EXPERIMENT_STATE_PAUSED, model.ExpStatePaused},
		{rpc.ExperimentState_EXPERIMENT_STATE_STOPPED, model.ExpStateStopped},
	}
	for _, c := range expCases {
		if int32(c.rpc) != c.want {
			t.Errorf("ExperimentState_%d 与 model 常量 %d 不一致", int32(c.rpc), c.want)
		}
	}
	subjectCases := []struct {
		rpc  rpc.SubjectType
		want int32
	}{
		{rpc.SubjectType_SUBJECT_TYPE_MID, model.SubjectMid},
		{rpc.SubjectType_SUBJECT_TYPE_DEVICE, model.SubjectDevice},
	}
	for _, c := range subjectCases {
		if int32(c.rpc) != c.want {
			t.Errorf("SubjectType_%d 与 model 常量 %d 不一致", int32(c.rpc), c.want)
		}
	}
	platformCases := []struct {
		rpc  rpc.Platform
		want int32
	}{
		{rpc.Platform_PLATFORM_ANDROID, model.PlatformAndroid},
		{rpc.Platform_PLATFORM_IOS, model.PlatformIOS},
		{rpc.Platform_PLATFORM_HARMONY, model.PlatformHarmony},
		{rpc.Platform_PLATFORM_DESKTOP, model.PlatformDesktop},
	}
	for _, c := range platformCases {
		if int32(c.rpc) != c.want {
			t.Errorf("Platform_%d 与 model 常量 %d 不一致（与 recommend-recall 的 Platform 也必须同值）", int32(c.rpc), c.want)
		}
	}
}

func TestDegradeAndFallbackEnumsMatchModel(t *testing.T) {
	degradeCases := []struct {
		rpc  rpc.RankDegradeReason
		want string
	}{
		{rpc.RankDegradeReason_RANK_DEGRADE_REASON_UNSPECIFIED, model.DegradeReasonNone},
		{rpc.RankDegradeReason_RANK_DEGRADE_REASON_MODEL_UNAVAILABLE, model.DegradeReasonModelUnavailable},
		{rpc.RankDegradeReason_RANK_DEGRADE_REASON_FEATURE_UNAVAILABLE, model.DegradeReasonFeatureUnavailable},
		{rpc.RankDegradeReason_RANK_DEGRADE_REASON_STORE_UNAVAILABLE, model.DegradeReasonStoreUnavailable},
		{rpc.RankDegradeReason_RANK_DEGRADE_REASON_BUDGET_EXHAUSTED, model.DegradeReasonBudgetExhausted},
		{rpc.RankDegradeReason_RANK_DEGRADE_REASON_SAFETY_UNAVAILABLE, model.DegradeReasonSafetyUnavailable},
		{rpc.RankDegradeReason_RANK_DEGRADE_REASON_EXPERIMENT_UNAVAILABLE, model.DegradeReasonExperimentUnavailable},
		{rpc.RankDegradeReason_RANK_DEGRADE_REASON_EMPTY_CANDIDATES, model.DegradeReasonEmptyCandidates},
	}
	for i, c := range degradeCases {
		if int32(i) != int32(c.rpc) {
			t.Fatalf("降级原因枚举编号必须从 0 连续递增（rpc=%d 位次=%d）", int32(c.rpc), i)
		}
		if !model.ValidDegradeReason(c.want) {
			t.Errorf("rpc 降级原因 %d 找不到对应的 model 受控 key", i)
		}
	}
	fallbackCases := []struct {
		rpc  rpc.FallbackStrategy
		want string
	}{
		{rpc.FallbackStrategy_FALLBACK_STRATEGY_UNSPECIFIED, model.FallbackNone},
		{rpc.FallbackStrategy_FALLBACK_STRATEGY_RECALL_ORDER, model.FallbackRecallOrder},
		{rpc.FallbackStrategy_FALLBACK_STRATEGY_PREVIOUS_MODEL, model.FallbackPreviousModel},
		{rpc.FallbackStrategy_FALLBACK_STRATEGY_SAFETY_ONLY, model.FallbackSafetyOnly},
	}
	for i, c := range fallbackCases {
		if int32(i) != int32(c.rpc) {
			t.Fatalf("兜底策略枚举编号必须从 0 连续递增（rpc=%d 位次=%d）", int32(c.rpc), i)
		}
		if !model.ValidFallback(c.want) {
			t.Errorf("rpc 兜底策略 %d 找不到对应的 model 受控 key", i)
		}
	}
}

// TestEveryRpcMethodFailsClosedOnNilRequest 是跨轮保留的验收哨兵：
// 10 个 rpc 方法全部存在，且**空请求绝不能被当成一次成功调用**。
// 契约轮阶段它钉的是「都返回 ErrNotImplemented」；实现落地后口径换成更严的一条——
// 不许返回「看起来成功」的空 reply（那会让网关把空结果当成合法排序下发给客户端）。
func TestEveryRpcMethodFailsClosedOnNilRequest(t *testing.T) {
	svcCtx := newTestServiceContext(t)
	cases := []struct {
		name string
		call func() error
	}{
		{"RankCandidates", func() error { _, err := NewRankCandidatesLogic(testCtx(), svcCtx).RankCandidates(nil); return err }},
		{"GetRankDecision", func() error { _, err := NewGetRankDecisionLogic(testCtx(), svcCtx).GetRankDecision(nil); return err }},
		{"ListRankDecisions", func() error {
			_, err := NewListRankDecisionsLogic(testCtx(), svcCtx).ListRankDecisions(nil)
			return err
		}},
		{"UpsertModelVersion", func() error {
			_, err := NewUpsertModelVersionLogic(testCtx(), svcCtx).UpsertModelVersion(nil)
			return err
		}},
		{"SetModelVersionState", func() error {
			_, err := NewSetModelVersionStateLogic(testCtx(), svcCtx).SetModelVersionState(nil)
			return err
		}},
		{"UpsertFeatureConfig", func() error {
			_, err := NewUpsertFeatureConfigLogic(testCtx(), svcCtx).UpsertFeatureConfig(nil)
			return err
		}},
		{"UpsertExperiment", func() error { _, err := NewUpsertExperimentLogic(testCtx(), svcCtx).UpsertExperiment(nil); return err }},
		{"SetExperimentState", func() error {
			_, err := NewSetExperimentStateLogic(testCtx(), svcCtx).SetExperimentState(nil)
			return err
		}},
		{"GetExperimentAssignment", func() error {
			_, err := NewGetExperimentAssignmentLogic(testCtx(), svcCtx).GetExperimentAssignment(nil)
			return err
		}},
		{"GetRankRuntimeConfig", func() error {
			_, err := NewGetRankRuntimeConfigLogic(testCtx(), svcCtx).GetRankRuntimeConfig(nil)
			return err
		}},
	}
	if len(cases) != 10 {
		t.Fatalf("rpc.Rank 有 10 个方法，本测试覆盖 %d 个", len(cases))
	}
	for _, c := range cases {
		if err := c.call(); err == nil {
			t.Errorf("%s：nil 请求本该失败，却回了成功（空结果会被网关当成合法排序）", c.name)
		}
	}
}

// TestNoLogicFileStillStubsOut 把「实现轮不许留空桩」钉在源码上：
// 只跑一遍调用点无法覆盖所有分支，而 `return nil, model.ErrNotImplemented`
// 留在 *logic.go 里就意味着某条 RPC 其实没接上。
func TestNoLogicFileStillStubsOut(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("扫描 logic 目录：%v", err)
	}
	var stubs []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || !strings.HasSuffix(f, "logic.go") {
			continue
		}
		src, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("读 %s：%v", f, rerr)
		}
		if strings.Contains(string(src), "return nil, model.ErrNotImplemented") {
			stubs = append(stubs, f)
		}
	}
	if len(stubs) != 0 {
		t.Errorf("这些 logic 仍是空桩：%v", stubs)
	}
}
