package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type StopLiveTranscodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewStopLiveTranscodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StopLiveTranscodeLogic {
	return &StopLiveTranscodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 请求停止（RUNNING→STOPPING，Worker 收尾后 STOPPED）
//
// 幂等口径以状态为准：已在 STOPPING 的重复请求返回当前行，不重复推进、不再写 Outbox。
// request_id 不参与判定——它是 live_transcode_task 的登记幂等键（uniq_request_id），
// 状态推进不允许覆写它，否则 StartLiveTranscode 的回放会失效；本方法只做归因与日志。
// 终态（STOPPED/CANCELLED）不能被停止请求改写，FAILED 只能走 RetryLiveTranscode。
func (l *StopLiveTranscodeLogic) StopLiveTranscode(in *rpc.StopLiveTranscodeReq) (*rpc.LiveTranscodeTaskInfo, error) {
	taskID := in.GetTaskId()
	if err := checkPositive("task_id", taskID, model.ErrTranscodeTaskNotFound); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	if err := checkFailureReason(int32(in.GetReason())); err != nil {
		return nil, err
	}
	traceID := sanitizeTraceID(in.GetTraceId())

	cur, err := l.svcCtx.TranscodeTasks.FindOne(l.ctx, taskID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, model.ErrTranscodeTaskNotFound
	}
	switch cur.State {
	case model.TranscodeStateStopping:
		// 停止指令已下发过：重放返回当前行。
		return transcodeInfo(cur), nil
	case model.TranscodeStateStopped, model.TranscodeStateCancelled:
		return nil, model.ErrTerminalState
	case model.TranscodeStatePending:
		// 还没拉起就无需「停止」：进程没在写分片，走 Cancel 才是合法终态。
		return nil, model.ErrInvalidTransition
	case model.TranscodeStateFailed:
		return nil, model.ErrInvalidTransition
	}
	if !model.IsValidTranscodeTransition(cur.State, model.TranscodeStateStopping) {
		return nil, model.ErrInvalidTransition
	}

	patch := model.TranscodePatch{
		Reason:  i32p(int32(in.GetReason())),
		TraceID: strp(traceID),
	}
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		aff, updErr := l.svcCtx.TranscodeTasks.UpdateStateTx(ctx, sess, taskID,
			[]int32{model.TranscodeStateRunning}, in.GetExpectedVersion(), model.TranscodeStateStopping, patch)
		if updErr != nil {
			return updErr
		}
		if aff == 0 {
			return classifyTranscodeZeroRow(ctx, l.svcCtx.TranscodeTasks, taskID, in.GetExpectedVersion())
		}
		return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeTranscodeStateChanged,
			model.AggregateTranscodeTask, taskID, cur.RoomId, map[string]any{
				"task_id":         taskID,
				"room_id":         cur.RoomId,
				"live_session_id": cur.LiveSession,
				"prev_state":      cur.State,
				"state":           model.TranscodeStateStopping,
				"reason":          int32(in.GetReason()),
				"operator":        optionalOperator(in.GetOperator()),
			}, traceID)
	})
	if err != nil {
		return nil, err
	}

	latest, err := l.svcCtx.TranscodeTasks.FindOne(l.ctx, taskID)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		// 提交成功后回读不到自己那一行（路由到旧副本 / 并发删除）：model 的 FindOne
		// 「查无此行」返回 (nil, nil)，少了这道门禁就会返回 (typed nil, nil) ——
		// gRPC 编成 OK + 空消息，调用方把一个成功的停止指令读成「task_id=0 的任务」。
		return nil, model.ErrTranscodeTaskNotFound
	}
	return transcodeInfo(latest), nil
}
