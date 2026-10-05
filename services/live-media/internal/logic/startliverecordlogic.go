package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type StartLiveRecordLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewStartLiveRecordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StartLiveRecordLogic {
	return &StartLiveRecordLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 登记录制任务（PENDING），request_id 幂等
//
// 幂等三层（与 StartLiveTranscode 同口径）：request_id 命中既有行 → 原样返回；
// 同 (room, session) 已有 PENDING/RECORDING/STOPPING 任务 → 复用（同场次双录会让
// uniq_record_seq 上的切片序号互相覆盖）；Insert 撞 uniq_request_id → 回读既有行，不当失败。
//
// 录制按场次建模：live_session_id 必填，否则回放无法归属到某一场。
// start_at/end_at 是「期望」区间，实际的 record_start_at/record_end_at 由 Worker 上报，
// 两者分离是回放区间可信的前提。产物只登记 bucket + key 前缀引用，凭据不入库、不入事件、不入日志。
func (l *StartLiveRecordLogic) StartLiveRecord(in *rpc.StartLiveRecordReq) (*rpc.LiveRecordTaskInfo, error) {
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkSessionID(in.GetLiveSessionId()); err != nil {
		return nil, err
	}
	if in.GetStartAt() < 0 || in.GetEndAt() < 0 {
		return nil, fmt.Errorf("live-media: start_at=%d end_at=%d must not be negative: %w",
			in.GetStartAt(), in.GetEndAt(), model.ErrInvalidSegmentRange)
	}
	if in.GetStartAt() > 0 && in.GetEndAt() > 0 && in.GetEndAt() <= in.GetStartAt() {
		return nil, fmt.Errorf("live-media: end_at=%d <= start_at=%d: %w",
			in.GetEndAt(), in.GetStartAt(), model.ErrInvalidSegmentRange)
	}
	cfg := l.svcCtx.Config.LiveMedia
	segSeconds, err := segmentSeconds(cfg, in.GetSegmentSeconds())
	if err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	requestID := normalizedRequestID(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())
	bucket, prefix, err := checkObjectRef(in.GetOutputBucket(), in.GetOutputPrefix(), true, maxPrefixRunes)
	if err != nil {
		return nil, err
	}
	timeoutSeconds := positiveInt32(in.GetTimeoutSeconds(), cfg.DefaultRecordTimeoutSeconds)

	// 录制源是自家转码任务时必须可查：source_task_id 指向不存在的任务，
	// 后面排查「录到了什么」就只剩一个悬空数字。0 表示录原画源（合法）。
	if srcID := in.GetSourceTaskId(); srcID > 0 {
		src, findErr := l.svcCtx.TranscodeTasks.FindOne(l.ctx, srcID)
		if findErr != nil {
			return nil, findErr
		}
		if src == nil {
			return nil, fmt.Errorf("live-media: source_task_id=%d: %w", srcID, model.ErrTranscodeTaskNotFound)
		}
		if src.RoomId != in.GetRoomId() {
			return nil, fmt.Errorf("live-media: source_task_id=%d belongs to room %d, not %d: %w",
				srcID, src.RoomId, in.GetRoomId(), model.ErrInvalidRoomID)
		}
	} else if in.GetSourceTaskId() < 0 {
		return nil, fmt.Errorf("live-media: source_task_id=%d must be 0 (source stream) or positive: %w",
			in.GetSourceTaskId(), model.ErrTranscodeTaskNotFound)
	}

	// 1. 幂等回放。
	if existed, findErr := l.svcCtx.RecordTasks.FindByRequestID(l.ctx, requestID); findErr != nil {
		return nil, findErr
	} else if existed != nil {
		return recordInfo(existed), nil
	}

	// 2. 同场次双录保护。
	if active, findErr := l.svcCtx.RecordTasks.FindActiveBySession(l.ctx, in.GetRoomId(), in.GetLiveSessionId()); findErr != nil {
		return nil, findErr
	} else if active != nil {
		l.Infof("livemedia/StartLiveRecord: reuse active record_id=%d state=%d room=%d session=%d",
			active.RecordId, active.State, active.RoomId, active.LiveSession)
		return recordInfo(active), nil
	}

	now := model.NowUnix()
	task := &model.LiveRecordTask{
		RoomId:         in.GetRoomId(),
		LiveSession:    in.GetLiveSessionId(),
		SourceTaskId:   in.GetSourceTaskId(),
		State:          model.RecordStatePending,
		StartAt:        in.GetStartAt(),
		EndAt:          in.GetEndAt(),
		SegmentSeconds: segSeconds,
		OutputBucket:   bucket,
		OutputPrefix:   prefix,
		// heartbeat_at/timeout_at 成对登记：差值即「无心跳超时秒数」快照（见 timeoutBudget）。
		HeartbeatAt: now,
		TimeoutAt:   nextTimeoutAt(now, timeoutSeconds),
		Version:     1,
		RequestId:   requestID,
		TraceId:     traceID,
	}

	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		id, insertErr := l.svcCtx.RecordTasks.InsertTx(ctx, sess, task)
		if insertErr != nil {
			return insertErr
		}
		// InsertTx 只返回自增主键、不回填结构体；不写回则事件 aggregate_id 与提交后回读都拿到 0。
		task.RecordId = id
		return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeRecordStateChanged,
			model.AggregateRecordTask, task.RecordId, task.RoomId, map[string]any{
				"record_id":       task.RecordId,
				"room_id":         task.RoomId,
				"live_session_id": task.LiveSession,
				"source_task_id":  task.SourceTaskId,
				"prev_state":      int32(0),
				"state":           task.State,
				"segment_seconds": task.SegmentSeconds,
				"start_at":        task.StartAt,
				"end_at":          task.EndAt,
				"output_bucket":   task.OutputBucket,
				"output_prefix":   task.OutputPrefix,
			}, traceID)
	})
	if err != nil {
		if errors.Is(err, model.ErrRequestIdDuplicated) {
			existed, findErr := l.svcCtx.RecordTasks.FindByRequestID(l.ctx, requestID)
			if findErr != nil {
				return nil, findErr
			}
			if existed != nil {
				return recordInfo(existed), nil
			}
			return nil, err
		}
		return nil, err
	}

	created, err := l.svcCtx.RecordTasks.FindOne(l.ctx, task.RecordId)
	if err != nil {
		return nil, err
	}
	if created == nil {
		return nil, model.ErrRecordTaskNotFound
	}
	return recordInfo(created), nil
}
