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

type BindReplayAssetLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBindReplayAssetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BindReplayAssetLogic {
	return &BindReplayAssetLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 回填回放产物与 asset/稿件的引用关系（只存引用，不推进稿件状态）
//
// 硬约束（proto line 559-562、AGENTS.md §5/§8）：本方法只写 live_replay_asset_ref 的引用列与
// live_replay_task 的主键引用列，**不写 asset_meta、不写 video_submission、不调任何推进稿件状态的路径**，
// 也不发任何「已发布」类事件；回放的审核与发布事实源永远是 video/moderation-orchestrator。
// 因此 AssetRPC/VideoRPC 为 nil 也不影响本方法：它登记的是那两个服务返回的主键。
//
// asset_id=0 是「尚未登记媒资」的占位值（model/live_replay_task.go:250 的注释），
// 它不能进任何反查条件，所以绑定动作要求 asset_id>0 且 aid>0（ErrInvalidAssetID/ErrInvalidAid）：
// 只给其中一个就没法把「产物—稿件」对齐，投影通道也无从反查。
//
// 幂等与冲突（三个唯一键 uniq_replay_id / uniq_asset_id / uniq_aid）：
//  1. 同一 (asset_id, aid) 且产物与展示字段完全一致 → 直接返回既有引用行（同值重放是成功，
//     不是冲突，也不 ++version）；
//  2. 同一回放要改绑到**不同**的 asset_id/aid → model.ErrAssetRefConflict（一条回放只能对应
//     一份产物一份稿件；唯一键是最终防线，logic 先给出可读结论）；
//  3. 同一 asset/aid 下刷新 bvid/产物引用/时长 → 允许（video 侧稍后才补齐 bvid 是正常链路）。
//
// 前置状态门：产物必须先存在。PENDING/MERGING/UPLOADING 时绑定等于给不存在的产物建引用
// （ErrInvalidTransition）；FAILED/CANCELLED 的终态不回补引用。
//
// 事务：引用行 UPSERT + 回放任务主键回填同事务提交（model 的 Tx 变体已就位），
// 不会出现「引用行有 asset_id、任务行还是 0」的半提交状态。
func (l *BindReplayAssetLogic) BindReplayAsset(in *rpc.BindReplayAssetReq) (*rpc.ReplayAssetRefInfo, error) {
	replayID := in.GetReplayId()
	if err := checkPositive("replay_id", replayID, model.ErrReplayTaskNotFound); err != nil {
		return nil, err
	}
	assetID := in.GetAssetId()
	if err := checkAssetID(assetID); err != nil {
		return nil, err
	}
	aid := in.GetAid()
	if err := checkAid(aid); err != nil {
		return nil, err
	}
	if in.GetDurationMs() <= 0 {
		// 绑定即声明「产物已知」，产物时长必须为正：0 会让回放播放器算不出进度条。
		return nil, fmt.Errorf("live-media: replay_id=%d duration_ms=%d must be positive: %w",
			replayID, in.GetDurationMs(), model.ErrInvalidBucketRef)
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	requestID := normalizedRequestID(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())
	bucket, objectKey, err := checkObjectRef(in.GetBucket(), in.GetObjectKey(), true, maxObjectKeyRunes)
	if err != nil {
		return nil, err
	}
	bvid, err := checkOptionalBoundedRef("bvid", in.GetBvid(), maxBvidRunes)
	if err != nil {
		return nil, err
	}

	ref, err := l.svcCtx.ReplayRefs.FindByReplayID(l.ctx, replayID)
	if err != nil {
		return nil, err
	}
	if ref != nil && (ref.AssetId != assetID || ref.Aid != aid) {
		return nil, fmt.Errorf("live-media: replay_id=%d already bound to asset_id=%d aid=%d, rejected %d/%d: %w",
			replayID, ref.AssetId, ref.Aid, assetID, aid, model.ErrAssetRefConflict)
	}
	if ref != nil && bindingUnchanged(ref, bucket, objectKey, bvid, in.GetDurationMs()) {
		// 同值重放：不回写、不 ++version，直接返回既有结论。
		l.Infof("livemedia/BindReplayAsset: replay_id=%d already bound asset_id=%d aid=%d request_id=%s",
			replayID, assetID, aid, requestID)
		return refInfo(ref), nil
	}

	task, err := l.svcCtx.ReplayTasks.FindOne(l.ctx, replayID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("live-media: replay_id=%d: %w", replayID, model.ErrReplayTaskNotFound)
	}
	if !allowBindAssetStates[task.State] {
		if model.IsReplayTerminal(task.State) {
			return nil, fmt.Errorf("live-media: replay_id=%d state=%d is terminal, ref must be bound before completion: %w",
				replayID, task.State, model.ErrTerminalState)
		}
		return nil, fmt.Errorf("live-media: replay_id=%d state=%d is before REGISTERED, product not uploaded: %w",
			replayID, task.State, model.ErrInvalidTransition)
	}
	// 产物引用一致性：ReportReplayProgress(REGISTERED) 已登记过 output_bucket/output_key，
	// 两处指向不同文件说明 Worker 串了任务；收下就会让引用指向没上传过的对象。
	if task.OutputBucket != "" && task.OutputKey != "" &&
		(task.OutputBucket != bucket || task.OutputKey != objectKey) {
		return nil, fmt.Errorf("live-media: replay_id=%d task output %s/%s differs from bound %s/%s: %w",
			replayID, task.OutputBucket, task.OutputKey, bucket, objectKey, model.ErrInvalidBucketRef)
	}
	if in.GetDurationMs() > 0 && task.DurationMs > 0 && in.GetDurationMs() < task.DurationMs {
		return nil, fmt.Errorf("live-media: replay_id=%d duration_ms=%d shrinks recorded %d: %w",
			replayID, in.GetDurationMs(), task.DurationMs, model.ErrInvalidTransition)
	}

	row := &model.LiveReplayAssetRef{
		RoomId:      task.RoomId,
		LiveSession: task.LiveSession,
		ReplayId:    replayID,
		RecordId:    task.RecordId,
		AssetId:     assetID,
		Aid:         aid,
		Bvid:        bvid,
		AnchorMid:   task.AnchorMid,
		Bucket:      bucket,
		ObjectKey:   objectKey,
		DurationMs:  in.GetDurationMs(),
		// 区间与缺口取任务行快照，不让 Worker 重复计算（引用行是「这份回放由哪些切片组成」的证据）。
		SegmentFromSeq: task.FromSeq,
		SegmentToSeq:   task.ToSeq,
		GapCount:       task.GapCount,
		// 投影列（review_state/published_at/last_event_id/source）留空：
		// UpsertTx 的 ON DUPLICATE 分支不覆盖它们，写入权归 ApplyReplayContentState 通道。
		RetentionState: model.RefRetentionStateNormal,
		RequestId:      requestID,
		TraceId:        traceID,
	}

	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		if _, upErr := l.svcCtx.ReplayRefs.UpsertTx(ctx, sess, row); upErr != nil {
			return upErr
		}
		patch := model.ReplayPatch{
			State:      task.State, // 自更新：只回填主键引用，不越权推进状态
			AssetId:    i64p(assetID),
			Aid:        i64p(aid),
			DurationMs: i64p(in.GetDurationMs()),
			TraceID:    strp(traceID),
		}
		if bvid != "" {
			patch.Bvid = strp(bvid)
		}
		if task.OutputBucket == "" {
			patch.OutputBucket = strp(bucket)
			patch.OutputKey = strp(objectKey)
		}
		aff, updErr := l.svcCtx.ReplayTasks.UpdateStateTx(ctx, sess, replayID, []int32{task.State}, 0, patch)
		if updErr != nil {
			return updErr
		}
		if aff == 0 {
			// fromStates 就是刚读到的状态：0 行只可能是并发把它推走了（上报或投影通道）。
			// 引用行已经写对，任务态交给持有新状态的一方，这里以冲突退出而不是猜。
			return classifyReplayZeroRow(ctx, l.svcCtx.ReplayTasks, replayID, 0)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 提交后回读引用行：Upsert 走 ON DUPLICATE 分支时 LastInsertId 不可信，replay_id 才是天然键。
	latest, err := l.svcCtx.ReplayRefs.FindByReplayID(l.ctx, replayID)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, fmt.Errorf("live-media: replay_id=%d: %w", replayID, model.ErrReplayRefNotFound)
	}
	return refInfo(latest), nil
}

// bindingUnchanged 判断一次绑定是否与既有引用行完全同值（产物、稿件号、展示字段与时长）。
// 同值才允许走幂等返回；任何一个字段不同都说明调用方想改点什么，必须进事务。
func bindingUnchanged(ref *model.LiveReplayAssetRef, bucket, objectKey, bvid string, durationMs int64) bool {
	return ref.Bucket == bucket && ref.ObjectKey == objectKey && ref.Bvid == bvid && ref.DurationMs == durationMs
}
