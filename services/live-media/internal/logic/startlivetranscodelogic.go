package logic

import (
	"context"
	"errors"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type StartLiveTranscodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewStartLiveTranscodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StartLiveTranscodeLogic {
	return &StartLiveTranscodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 登记直播转码任务（PENDING），request_id 幂等；不在此调用 FFmpeg
//
// 幂等三层：request_id 命中既有行 → 原样返回；同 (room,session,level,protocol) 已有活跃任务
// → 复用（同档双开会产出两路互相覆盖的分片，属禁止状态）；Insert 撞 uniq_request_id →
// 回读既有行，不当失败。任务行与 livemedia.transcode.state.changed 同事务提交（AGENTS.md §5）。
// max_attempts / timeout_seconds 是登记时刻的快照，之后不随配置变化回写历史行。
func (l *StartLiveTranscodeLogic) StartLiveTranscode(in *rpc.StartLiveTranscodeReq) (*rpc.LiveTranscodeTaskInfo, error) {
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkSessionID(in.GetLiveSessionId()); err != nil {
		return nil, err
	}
	if err := checkTemplateID(in.GetTemplateId()); err != nil {
		return nil, err
	}
	if err := checkBitrateLevel(int32(in.GetBitrateLevel())); err != nil {
		return nil, err
	}
	if err := checkProtocol(int32(in.GetProtocol())); err != nil {
		return nil, err
	}
	sourceRef, err := checkSourceRef(in.GetSourceRef())
	if err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	requestID := normalizedRequestID(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	cfg := l.svcCtx.Config.LiveMedia
	maxAttempts := positiveInt32(in.GetMaxAttempts(), cfg.DefaultMaxAttempts)
	timeoutSeconds := positiveInt32(in.GetTimeoutSeconds(), cfg.DefaultTranscodeTimeoutSeconds)

	// 1. 幂等回放：同 request_id 直接返回首次登记的任务（不走复用判定、不新增行、不写 Outbox）。
	if existed, findErr := l.svcCtx.TranscodeTasks.FindByRequestID(l.ctx, requestID); findErr != nil {
		return nil, findErr
	} else if existed != nil {
		return transcodeInfo(existed), nil
	}

	// 2. 双开保护：同档位已有活跃任务即复用。
	if active, findErr := l.svcCtx.TranscodeTasks.FindActiveByRoom(l.ctx, in.GetRoomId(), in.GetLiveSessionId(),
		int32(in.GetBitrateLevel()), int32(in.GetProtocol())); findErr != nil {
		return nil, findErr
	} else if active != nil {
		l.Infof("livemedia/StartLiveTranscode: reuse active task_id=%d state=%d room=%d level=%d",
			active.TaskId, active.State, active.RoomId, active.BitrateLevel)
		return transcodeInfo(active), nil
	}

	now := model.NowUnix()
	task := &model.LiveTranscodeTask{
		RoomId:       in.GetRoomId(),
		LiveSession:  in.GetLiveSessionId(),
		TemplateId:   in.GetTemplateId(),
		BitrateLevel: int32(in.GetBitrateLevel()),
		Protocol:     int32(in.GetProtocol()),
		SourceRef:    sourceRef,
		AnchorMid:    in.GetAnchorMid(),
		State:        model.TranscodeStatePending,
		MaxAttempts:  maxAttempts,
		// heartbeat_at/timeout_at 成对登记：timeout_at - heartbeat_at 就是「无心跳超时秒数」
		// 的快照（表里没有独立的秒数字段），后续心跳按同一时长顺延，不回读配置。
		HeartbeatAt: now,
		TimeoutAt:   nextTimeoutAt(now, timeoutSeconds),
		Version:     1,
		RequestId:   requestID,
		TraceId:     traceID,
	}

	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		id, insertErr := l.svcCtx.TranscodeTasks.InsertTx(ctx, sess, task)
		if insertErr != nil {
			return insertErr
		}
		// InsertTx 只返回自增主键、不回填结构体；不写回则事件 aggregate_id 与提交后回读都拿到 0。
		task.TaskId = id
		return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeTranscodeStateChanged,
			model.AggregateTranscodeTask, task.TaskId, task.RoomId, map[string]any{
				"task_id":         task.TaskId,
				"room_id":         task.RoomId,
				"live_session_id": task.LiveSession,
				"template_id":     task.TemplateId,
				"bitrate_level":   task.BitrateLevel,
				"protocol":        task.Protocol,
				"prev_state":      int32(0),
				"state":           task.State,
				"attempt":         task.Attempt,
				"reason":          task.Reason,
			}, traceID)
	})
	if err != nil {
		if errors.Is(err, model.ErrRequestIdDuplicated) {
			// 并发下同一 request_id 已被他方登记：回读既有行，与重放语义等价。
			existed, findErr := l.svcCtx.TranscodeTasks.FindByRequestID(l.ctx, requestID)
			if findErr != nil {
				return nil, findErr
			}
			if existed != nil {
				return transcodeInfo(existed), nil
			}
			return nil, err
		}
		return nil, err
	}

	created, err := l.svcCtx.TranscodeTasks.FindOne(l.ctx, task.TaskId)
	if err != nil {
		return nil, err
	}
	if created == nil {
		return nil, model.ErrTranscodeTaskNotFound
	}
	return transcodeInfo(created), nil
}
