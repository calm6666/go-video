package logic

import (
	"context"
	"fmt"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ReportReplayProgressLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportReplayProgressLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportReplayProgressLogic {
	return &ReportReplayProgressLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Worker 上报回放进度（拼接/上传/登记/送审）
//
// 权限边界（proto line 79-89、README §4 line 133-140）：Worker 只能沿
// PENDING→MERGING→UPLOADING→REGISTERED→REVIEW_SUBMITTED 前进，任一阶段可 →FAILED。
// 三个状态从不上报路径产生，一律 ErrInvalidTransition 失败关闭，而不是静默忽略：
//   - PENDING：只由 SubmitReplayTask 写入（幂等键在那一侧）；
//   - CANCELLED：取消是调用方/运营动作，本服务不提供 Worker 代为取消的入口；
//   - COMPLETED：**唯一**入口是 ApplyReplayContentState（video 事实投影，README §4 line 140），
//     让 Worker 自证「回放已可用」等于本服务自己宣布发布，违反 AGENTS.md §5/§8。
//
// 幂等（README §2.4 line 79：幂等键 expected_version + 状态机）：
//   - 同值重放 = 目标态与当前态一致、产物引用一致、且未提供新的计数/时长（0 即不提供）
//     → 直接返回当前行，不 ++version、不发事件；
//   - 同状态但携带不同的字段：replayTransitions 里没有任何自环，这就是非法迁移，
//     回 ErrInvalidTransition 并提示「把字段挂在状态边上上报」；
//   - 迟到的 expected_version（高于当前 version，说明调用方看过更新的行）→ ErrVersionConflict；
//   - 已终态（FAILED/COMPLETED/CANCELLED）重投同一个归因 → 回当前行；换了个失败原因重投 →
//     ErrTerminalState，不覆盖已记录的归因（它可能已被运营用于统计）。
//
// 进度字段一致性（replayProgressMonotonic）：计数不得为负、不得超过 [from_seq,to_seq] 的片数、
// duration_ms 只增不减——更小的产物时长只可能是旧任务或串了 replay_id 的回执，
// 收下就会把已登记产物的时长改坏。
//
// 前置事实：REGISTERED 必须带 output_bucket+output_key（产物已在对象存储，只存引用，
// 签名地址与凭据一律拒绝）；REVIEW_SUBMITTED 要求 asset_id/aid 已由 BindReplayAsset 回填
// （proto line 83-85 的语义），否则送审事件里带的是 0，下游无从查。
//
// 事件：只有 REVIEW_SUBMITTED 这条边写 Outbox(livemedia.replay.review.submitted)，
// 与状态推进同事务（model 的 Tx 变体已就位）。MERGING/UPLOADING/REGISTERED/FAILED 不发事件：
// model/live_media_outbox.go 里没有登记任何通用 replay_state_changed 与失败事件类型，
// 本服务也**不为此伪造一个 topic/事件常量**（README §8.4 事件流未接通），
// 中间态靠 GetReplayTask/ListReplayTasks 读侧观测。
// asset_id/aid 的回填不发生在本方法：由 BindReplayAsset 写入并受 uniq_asset_id/uniq_aid 保护。
func (l *ReportReplayProgressLogic) ReportReplayProgress(in *rpc.ReportReplayProgressReq) (*rpc.LiveReplayTaskInfo, error) {
	replayID := in.GetReplayId()
	if err := checkPositive("replay_id", replayID, model.ErrReplayTaskNotFound); err != nil {
		return nil, err
	}
	target := int32(in.GetState())
	if err := checkReplayState(target); err != nil {
		return nil, err
	}
	reason := int32(in.GetReason())
	if err := checkFailureReason(reason); err != nil {
		return nil, err
	}
	if in.GetExpectedVersion() < 0 {
		return nil, fmt.Errorf("live-media: expected_version=%d must not be negative: %w",
			in.GetExpectedVersion(), model.ErrVersionConflict)
	}
	switch target {
	case model.ReplayStatePending:
		return nil, fmt.Errorf("live-media: PENDING is not a reportable replay state, it is created by SubmitReplayTask: %w",
			model.ErrInvalidTransition)
	case model.ReplayStateCancelled:
		return nil, fmt.Errorf("live-media: CANCELLED is not a reportable replay state, worker must not cancel on behalf of the caller: %w",
			model.ErrInvalidTransition)
	case model.ReplayStateCompleted:
		return nil, fmt.Errorf("live-media: COMPLETED is driven only by ApplyReplayContentState (video projection), not by worker reports: %w",
			model.ErrInvalidTransition)
	}
	if target == model.ReplayStateFailed && reason == model.ReasonUnspecified {
		return nil, fmt.Errorf("live-media: FAILED report needs a concrete reason (SOURCE_LOST/STORAGE/TIMEOUT/DATA_GAP): %w",
			model.ErrInvalidTransition)
	}
	workerID := sanitizeWorkerID(in.GetWorkerId())
	traceID := sanitizeTraceID(in.GetTraceId())

	cur, err := l.svcCtx.ReplayTasks.FindOne(l.ctx, replayID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("live-media: replay_id=%d: %w", replayID, model.ErrReplayTaskNotFound)
	}

	// 产物引用：空 = 本次不提供，沿用已登记值（与计数的「0 不提供」同一套约定）。
	// 一旦给了就必须是完整的「桶 + 相对 key」：半截引用既播不了也没法回收。
	bucket, objectKey := cur.OutputBucket, cur.OutputKey
	refSupplied := in.GetOutputBucket() != "" || in.GetOutputKey() != ""
	if refSupplied {
		b, k, refErr := checkObjectRef(in.GetOutputBucket(), in.GetOutputKey(), true, maxObjectKeyRunes)
		if refErr != nil {
			return nil, refErr
		}
		bucket, objectKey = b, k
	}
	if target == model.ReplayStateRegistered && (bucket == "" || objectKey == "") {
		return nil, fmt.Errorf("live-media: replay_id=%d REGISTERED requires output_bucket/output_key: %w",
			replayID, model.ErrInvalidBucketRef)
	}
	if target == model.ReplayStateReviewSubmitted && (cur.AssetId <= 0 || cur.Aid <= 0) {
		// 只允许从 REGISTERED 进来，正常链路里 BindReplayAsset 已经落好主键引用；
		// 缺引用说明顺序被打乱或有历史脏行，此时发「已送审」事件等于发一条下游查不到的空壳。
		return nil, fmt.Errorf("live-media: replay_id=%d REVIEW_SUBMITTED needs asset_id/aid bound by BindReplayAsset, got %d/%d: %w",
			replayID, cur.AssetId, cur.Aid, model.ErrInvalidAssetID)
	}

	// 计数字段的区间：拼接区间长度是这些计数的上界。
	var rangeLen int64
	if cur.FromSeq > 0 && cur.ToSeq >= cur.FromSeq {
		rangeLen = cur.ToSeq - cur.FromSeq + 1
	}
	segmentCount, gapCount, durationMs, err := replayProgressMonotonic(cur, rangeLen,
		in.GetSegmentCount(), in.GetGapCount(), in.GetDurationMs())
	if err != nil {
		return nil, err
	}

	if model.IsReplayTerminal(cur.State) {
		if cur.State == target {
			if reason != model.ReasonUnspecified && reason != cur.Reason {
				// 同一条边给出两个不同的失败归因：结论互相矛盾，留下哪个都是猜。
				// 不覆盖已记录的归因（它可能已被运营用于统计），失败关闭让上游收敛。
				return nil, fmt.Errorf("live-media: replay_id=%d already %d with reason=%d, incoming reason %d contradicts it: %w",
					replayID, cur.State, cur.Reason, reason, model.ErrTerminalState)
			}
			if progressKeepsRow(cur, target, bucket, objectKey, segmentCount, gapCount, durationMs) {
				// 终态同值重放：回执丢了就重投一次，读到既有结论即可（不改行、不发事件）。
				return replayInfo(cur), nil
			}
		}
		l.Errorf("livemedia/ReportReplayProgress: reject report on terminal replay_id=%d state=%d incoming=%d worker=%s",
			replayID, cur.State, target, workerID)
		return nil, fmt.Errorf("live-media: replay_id=%d state=%d is terminal, incoming %d: %w",
			replayID, cur.State, target, model.ErrTerminalState)
	}

	if cur.State == target {
		if progressKeepsRow(cur, target, bucket, objectKey, segmentCount, gapCount, durationMs) {
			if in.GetExpectedVersion() > cur.Version {
				// 调用方看过比当前行更新的版本：它的视图不可信，让它重读。
				return nil, fmt.Errorf("live-media: replay_id=%d expected_version=%d ahead of version=%d: %w",
					replayID, in.GetExpectedVersion(), cur.Version, model.ErrVersionConflict)
			}
			l.Infof("livemedia/ReportReplayProgress: replay_id=%d state=%d identical report (worker=%s), kept as is",
				replayID, target, workerID)
			return replayInfo(cur), nil
		}
		// replayTransitions 没有自环：同状态携带不同事实不是「刷新」，是越权改写。
		return nil, fmt.Errorf("live-media: replay_id=%d has no self transition for state=%d, report the new counts with the next state edge: %w",
			replayID, target, model.ErrInvalidTransition)
	}
	if !model.IsValidReplayTransition(cur.State, target) {
		return nil, fmt.Errorf("live-media: replay replay_id=%d state %d->%d: %w",
			replayID, cur.State, target, model.ErrInvalidTransition)
	}

	patch := model.ReplayPatch{
		State:        target,
		SegmentCount: segmentCount,
		GapCount:     gapCount,
		DurationMs:   durationMs,
		Errno:        i32p(in.GetErrno()),
		ErrMsg:       strp(sanitizeErrMsg(in.GetErrMsg())),
		TraceID:      strp(traceID),
	}
	if refSupplied && (bucket != cur.OutputBucket || objectKey != cur.OutputKey) {
		patch.OutputBucket = strp(bucket)
		patch.OutputKey = strp(objectKey)
	}
	if reason != model.ReasonUnspecified {
		patch.Reason = i32p(reason)
	}
	// 前进边不带失败信息：FAILED 的 reason/err_msg 才有意义，
	// 成功边上把上一次的失败摘要留在行里会误导排障。
	if target != model.ReplayStateFailed {
		patch.Errno = nil
		patch.ErrMsg = nil
	}

	// 事件里的计数是「本次生效值」：上报给了新值就用新值，没给沿用行内值。
	// 用事务前的 cur 值发事件会让下游拿到一条与状态推进不匹配的旧素材数。
	effSegments, effGaps, effDuration := cur.SegmentCount, cur.GapCount, cur.DurationMs
	if segmentCount != nil {
		effSegments = *segmentCount
	}
	if gapCount != nil {
		effGaps = *gapCount
	}
	if durationMs != nil {
		effDuration = *durationMs
	}

	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		aff, updErr := l.svcCtx.ReplayTasks.UpdateStateTx(ctx, sess, replayID,
			[]int32{cur.State}, in.GetExpectedVersion(), patch)
		if updErr != nil {
			return updErr
		}
		if aff == 0 {
			// 0 行 = 行被并发推走 / 版本不符 / 已被投影驱动到终态，交给分类器给可读结论。
			return classifyReplayZeroRow(ctx, l.svcCtx.ReplayTasks, replayID, in.GetExpectedVersion())
		}
		if target != model.ReplayStateReviewSubmitted {
			return nil
		}
		return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeReplayReviewSubmitted,
			model.AggregateReplayTask, replayID, cur.RoomId, map[string]any{
				"replay_id":       replayID,
				"record_id":       cur.RecordId,
				"room_id":         cur.RoomId,
				"live_session_id": cur.LiveSession,
				"asset_id":        cur.AssetId,
				"aid":             cur.Aid,
				"anchor_mid":      cur.AnchorMid,
				"from_seq":        cur.FromSeq,
				"to_seq":          cur.ToSeq,
				"segment_count":   effSegments,
				"gap_count":       effGaps,
				"duration_ms":     effDuration,
				"worker_id":       workerID,
			}, traceID)
	})
	if err != nil {
		return nil, err
	}

	latest, err := l.svcCtx.ReplayTasks.FindOne(l.ctx, replayID)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, fmt.Errorf("live-media: replay_id=%d: %w", replayID, model.ErrReplayTaskNotFound)
	}
	return replayInfo(latest), nil
}
