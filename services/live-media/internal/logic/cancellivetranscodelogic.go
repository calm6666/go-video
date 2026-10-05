package logic

import (
	"context"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type CancelLiveTranscodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCancelLiveTranscodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelLiveTranscodeLogic {
	return &CancelLiveTranscodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 取消未运行/停止中的任务（PENDING|STOPPING→CANCELLED 终态）
//
// 取消是终态写入：已是 CANCELLED 的行原样返回（重放不再 ++version）。
// RUNNING 不允许直接取消——进程正在写分片，必须先 StopLiveTranscode 让 Worker 收尾，
// 否则库里记成已取消而对象存储还在长出切片，留下无人认领的孤儿对象。
// 取消不回收任何产物：残留分片/档位由 SubmitRetentionTask 显式登记后执行（AGENTS.md §8）。
func (l *CancelLiveTranscodeLogic) CancelLiveTranscode(in *rpc.CancelLiveTranscodeReq) (*rpc.LiveTranscodeTaskInfo, error) {
	taskID := in.GetTaskId()
	if err := checkPositive("task_id", taskID, model.ErrTranscodeTaskNotFound); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	operator, err := sanitizeOperator(in.GetOperator())
	if err != nil {
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
	if cur.State == model.TranscodeStateCancelled {
		return transcodeInfo(cur), nil
	}
	if model.IsTranscodeTerminal(cur.State) {
		return nil, model.ErrTerminalState
	}
	fromStates := []int32{model.TranscodeStatePending, model.TranscodeStateStopping}
	if !containsState(fromStates, cur.State) {
		// RUNNING 走 Stop、FAILED 走 Retry：这里给出可归因的非法迁移而不是笼统冲突。
		return nil, model.ErrInvalidTransition
	}

	patch := model.TranscodePatch{
		StoppedAt: i64p(model.NowUnix()),
		Reason:    i32p(int32(in.GetReason())),
		TraceID:   strp(traceID),
	}
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		aff, updErr := l.svcCtx.TranscodeTasks.UpdateStateTx(ctx, sess, taskID, fromStates,
			in.GetExpectedVersion(), model.TranscodeStateCancelled, patch)
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
				"state":           model.TranscodeStateCancelled,
				"reason":          int32(in.GetReason()),
				"operator":        operator,
			}, traceID)
	})
	if err != nil {
		return nil, err
	}
	// stopped_at 记的是「取消落库时刻」，不是 Worker 真正退出时刻：终态行不再有心跳，
	// 该值只用于审计排序，不参与超时判定。

	latest, err := l.svcCtx.TranscodeTasks.FindOne(l.ctx, taskID)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		// 见 StopLiveTranscode 的同处注释：取消是终态写入，返回空消息等于告诉调用方
		// 「取消成功了，但没有这个任务」，运营台会按 task_id=0 再查一次并判定为数据丢失。
		return nil, model.ErrTranscodeTaskNotFound
	}
	return transcodeInfo(latest), nil
}
