package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ApplyReplayContentStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewApplyReplayContentStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ApplyReplayContentStateLogic {
	return &ApplyReplayContentStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 同步 video 侧审核/发布投影（单向：video → live-media）
//
// 方向单一（proto line 612-614、model/live_replay_asset_ref.go 头部）：本方法只把 video 的事实
// 写进本地投影列（review_state/review_state_at/published_at/last_event_id/source），
// 永不调用 video 的状态推进接口；rpc.ReviewState 只是可读副本，事实源在 video。
//
// 三类路径都要真实可达：
//   - 幂等：同一 event_id 重放（last_event_id 已等于它）→ 回当前投影，不重复刷新、不再发事件；
//   - 挡旧值：review_state_at 比本次事件更新的行（旧事件迟到）→ 保留新投影、记 error，
//     不把已发布改回审核中；
//   - 越界失败关闭：投影取值本身有迁移表（replaypolicy.go），已删除是终态，
//     DELETED→PUBLISHED 这类「复活已删稿件」直接 ErrInvalidTransition，而不是静默忽略；
//     已发布态必须带 video 侧发布时刻，否则运营无法回答「何时上线」。
//
// 联动（都在同一事务里，AGENTS.md §5）：
//  1. 投影为 PUBLISHED 且回放任务处于 REVIEW_SUBMITTED → 推进 COMPLETED。这是 COMPLETED 的
//     **唯一**入口（Worker 侧 ReportReplayProgress 上报 COMPLETED 会被判非法迁移），
//     本服务依然没有「把稿件写成已发布」的能力——它只是把 video 已发布这件事投影到自己的任务行上；
//  2. 投影为 DELETED → 引用行生命周期 Normal→Pending（待回收）。真删由 SubmitRetentionTask 登记、
//     Worker 执行，本方法不删任何对象（AGENTS.md §8 保留审计证据）；
//  3. REJECTED/OFFLINE 不推进任务状态：稿件被驳回/下架不等于拼接失败。
//
// 引用行还不存在时（事件早于 BindReplayAsset 到达）返回 ErrReplayRefNotFound 让 MQ 退避重试，
// 绝不静默丢弃——丢掉一条 video 事实事件就等于本地投影永久错着。
func (l *ApplyReplayContentStateLogic) ApplyReplayContentState(in *rpc.ApplyReplayContentStateReq) (*rpc.ReplayAssetRefInfo, error) {
	replayID := in.GetReplayId()
	assetID := in.GetAssetId()
	if replayID < 0 || assetID < 0 {
		return nil, fmt.Errorf("live-media: replay_id=%d asset_id=%d must not be negative: %w",
			replayID, assetID, model.ErrReplayRefNotFound)
	}
	if replayID <= 0 && assetID <= 0 {
		return nil, fmt.Errorf("live-media: replay_id or asset_id is required: %w", model.ErrReplayRefNotFound)
	}
	target := int32(in.GetReviewState())
	if err := checkReviewState(target); err != nil {
		return nil, err
	}
	if err := checkEventID(in.GetEventId()); err != nil {
		return nil, err
	}
	eventID := normalizedEventID(in.GetEventId())
	if in.GetPublishedAt() < 0 {
		return nil, fmt.Errorf("live-media: published_at=%d must be 0 (不刷新) or positive: %w",
			in.GetPublishedAt(), model.ErrInvalidReviewState)
	}
	if target == model.ReviewStatePublished && in.GetPublishedAt() <= 0 {
		return nil, fmt.Errorf("live-media: review_state=PUBLISHED requires video published_at: %w",
			model.ErrInvalidReviewState)
	}
	source := strings.TrimSpace(in.GetSource())
	if !reviewProjectionSource[source] {
		return nil, fmt.Errorf("live-media: source=%q is not one of content.published.v1/video.rpc/manual: %w",
			source, model.ErrInvalidTransition)
	}
	if source == "manual" {
		// 人工刷新绕过了事件流水线，必须留下可归因的告警（谁改的投影在事件流里查不到）。
		l.Errorf("livemedia/ApplyReplayContentState: manual projection override replay_id=%d asset_id=%d state=%d event_id=%s trace_id=%s",
			replayID, assetID, target, eventID, sanitizeTraceID(in.GetTraceId()))
	}
	traceID := sanitizeTraceID(in.GetTraceId())
	stateAt := model.NowUnix()

	ref, err := l.locateRef(in, replayID, assetID)
	if err != nil {
		return nil, err
	}
	if ref == nil {
		return nil, fmt.Errorf("live-media: replay_id=%d asset_id=%d: %w", replayID, assetID, model.ErrReplayRefNotFound)
	}
	if assetID > 0 && ref.AssetId > 0 && ref.AssetId != assetID {
		return nil, fmt.Errorf("live-media: replay_id=%d is bound to asset_id=%d, event carries %d: %w",
			ref.ReplayId, ref.AssetId, assetID, model.ErrAssetRefConflict)
	}
	if !isValidReviewProjectionTransition(ref.ReviewState, target) {
		return nil, fmt.Errorf("live-media: replay_id=%d review projection %d->%d: %w",
			ref.ReplayId, ref.ReviewState, target, model.ErrInvalidTransition)
	}
	if ref.LastEventId == eventID {
		// 事件级幂等在读侧就能判定：连 UPDATE 都不用发（UPDATE 里还有同样的条件做最终防线）。
		l.Infof("livemedia/ApplyReplayContentState: event_id=%s already applied to ref id=%d", eventID, ref.Id)
		return refInfo(ref), nil
	}

	// 任务终态与回收标记的前置读必须在事务外：主连接看不见本事务未提交的写入。
	task, err := l.svcCtx.ReplayTasks.FindOne(l.ctx, ref.ReplayId)
	if err != nil {
		return nil, err
	}
	driveCompleted := target == model.ReviewStatePublished && task != nil &&
		task.State == model.ReplayStateReviewSubmitted
	markPending := target == model.ReviewStateDeleted && ref.RetentionState == model.RefRetentionStateNormal
	if target == model.ReviewStatePublished && task != nil && task.State != model.ReplayStateReviewSubmitted &&
		task.State != model.ReplayStateCompleted {
		// video 已发布但本地任务还没走到 REVIEW_SUBMITTED：引用回填与送审的顺序被打乱了，
		// 投影照样落（它是事实），但任务态不越级推进（合法边只有 REVIEW_SUBMITTED→COMPLETED）。
		l.Errorf("livemedia/ApplyReplayContentState: replay_id=%d published but task state=%d not REVIEW_SUBMITTED, projection kept",
			ref.ReplayId, task.State)
	}

	applied := false
	// duplicate 标记「同事件已应用」或「旧事件迟到被挡」：两种都当幂等收敛处理——
	// 事务照常提交（没有写入），但不驱动任务、不发事件、不返回错误，
	// 否则 MQ 会无限退避重试一条已经落地的投影。
	duplicate := false
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		aff, updErr := l.svcCtx.ReplayRefs.ApplyContentStateTx(ctx, sess, model.ContentStateApply{
			ReplayId:    ref.ReplayId,
			AssetId:     ref.AssetId,
			ReviewState: target,
			PublishedAt: in.GetPublishedAt(),
			StateAt:     stateAt,
			EventId:     eventID,
			Source:      source,
			TraceId:     traceID,
		})
		if updErr != nil {
			return updErr
		}
		if aff == 0 {
			if clsErr := l.classifyProjectionZeroRow(ctx, ref.Id, eventID, stateAt); clsErr != nil {
				return clsErr
			}
			duplicate = true
			return nil
		}
		applied = true

		if driveCompleted {
			tAff, tErr := l.svcCtx.ReplayTasks.UpdateStateTx(ctx, sess, ref.ReplayId,
				[]int32{model.ReplayStateReviewSubmitted}, 0, model.ReplayPatch{
					State:   model.ReplayStateCompleted,
					TraceID: strp(traceID),
				})
			if tErr != nil {
				return tErr
			}
			if tAff == 0 {
				// 并发把任务推走（例如 Worker 同时上报 FAILED）：不复活、不改判，
				// 但本次投影仍是既成事实，事务不回滚，冲突由 Worker 的下一次上报或运营收敛。
				l.Errorf("livemedia/ApplyReplayContentState: replay_id=%d completed drive lost race, projection kept", ref.ReplayId)
			}
		}
		if markPending {
			rAff, rErr := l.svcCtx.ReplayRefs.MarkRetentionStateTx(ctx, sess, ref.Id,
				model.RefRetentionStateNormal, model.RefRetentionStatePending)
			if rErr != nil {
				return rErr
			}
			if rAff == 0 {
				// 值未变化也会 0 行（MySQL 语义）：生命周期已被并发推进，按幂等处理。
				l.Infof("livemedia/ApplyReplayContentState: ref id=%d retention already advanced", ref.Id)
			}
		}
		return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeReplayContentStateChanged,
			model.AggregateReplayAssetRef, ref.Id, ref.RoomId, map[string]any{
				"id":                ref.Id,
				"replay_id":         ref.ReplayId,
				"record_id":         ref.RecordId,
				"asset_id":          ref.AssetId,
				"aid":               ref.Aid,
				"room_id":           ref.RoomId,
				"live_session_id":   ref.LiveSession,
				"prev_review_state": ref.ReviewState,
				"review_state":      target,
				"review_state_at":   stateAt,
				"published_at":      in.GetPublishedAt(),
				"event_id":          eventID,
				"source":            source,
				"task_completed":    driveCompleted,
				"retention_pending": markPending,
			}, traceID)
	})
	if err != nil {
		return nil, err
	}
	if !applied && !duplicate {
		// 防御分支：既没落地写入也没被判定为幂等，说明事务被提前中断，不能回成功。
		return nil, model.ErrReplayRefNotFound
	}
	if duplicate {
		l.Infof("livemedia/ApplyReplayContentState: event_id=%s not re-applied to ref id=%d, current projection kept",
			eventID, ref.Id)
	}

	latest, err := l.svcCtx.ReplayRefs.FindOne(l.ctx, ref.Id)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, fmt.Errorf("live-media: ref id=%d: %w", ref.Id, model.ErrReplayRefNotFound)
	}
	return refInfo(latest), nil
}

// locateRef 按「replay_id 优先，其次 asset_id」定位引用行。
// 两个入口都给时以 replay_id 为准，媒资归属在调用点交叉校验（防止把别的回放的投影刷过来）。
func (l *ApplyReplayContentStateLogic) locateRef(in *rpc.ApplyReplayContentStateReq,
	replayID, assetID int64) (*model.LiveReplayAssetRef, error) {
	if replayID > 0 {
		return l.svcCtx.ReplayRefs.FindByReplayID(l.ctx, replayID)
	}
	// asset_id=0 是「未登记媒资」的占位值，不能当反查条件（会命中所有未绑定引用）。
	return l.svcCtx.ReplayRefs.FindByAssetID(l.ctx, assetID)
}

// classifyProjectionZeroRow 区分投影 UPDATE 命中 0 行的两种成因：
// 同一事件已应用（幂等）/ 更小的时间戳被更新的投影挡住（旧事件迟到）。
// 两者都不该重投，因此回当前投影而不是报错；真正找不到行才失败关闭。
func (l *ApplyReplayContentStateLogic) classifyProjectionZeroRow(_ context.Context, refID int64,
	eventID string, stateAt int64) error {
	latest, err := l.svcCtx.ReplayRefs.FindOne(l.ctx, refID)
	if err != nil {
		return err
	}
	if latest == nil {
		return model.ErrReplayRefNotFound
	}
	if latest.LastEventId == eventID {
		return nil // 幂等重放：外层不再发事件，直接回当前投影
	}
	if latest.ReviewStateAt > stateAt {
		l.Errorf("livemedia/ApplyReplayContentState: stale event_id=%s (state_at=%d) rejected, projection kept at state=%d at=%d",
			eventID, stateAt, latest.ReviewState, latest.ReviewStateAt)
		return nil
	}
	return fmt.Errorf("live-media: ref id=%d projection update matched 0 rows with last_event_id=%s at=%d: %w",
		refID, latest.LastEventId, latest.ReviewStateAt, model.ErrInvalidTransition)
}
