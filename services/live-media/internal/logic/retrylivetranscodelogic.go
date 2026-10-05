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

type RetryLiveTranscodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRetryLiveTranscodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryLiveTranscodeLogic {
	return &RetryLiveTranscodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 重试失败任务（FAILED→PENDING，attempt+1，受 max_attempts 限制）
//
// 重试预算：attempt+1 > max_attempts 直接 model.ErrAttemptExhausted，不改库、不写事件
// —— 直播转码是常驻进程，无限拉起会打满机器（残留产物由 SubmitRetentionTask 清理）。
// 幂等口径：request_id 不能覆写（它是登记幂等键 uniq_request_id），因此重放判定用 trace_id
// —— 同一次重试的重投必然带同一 trace_id，此时行已是 PENDING，原样返回、attempt 不再 +1。
func (l *RetryLiveTranscodeLogic) RetryLiveTranscode(in *rpc.RetryLiveTranscodeReq) (*rpc.LiveTranscodeTaskInfo, error) {
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
	// 重试说明只进日志与事件：reason 列（int32）保留首次失败原因，两者语义不能互相覆盖。
	note, err := checkAuditText("retry note", in.GetReason(), false, maxReasonRunes)
	if err != nil {
		return nil, err
	}
	traceID := sanitizeTraceID(in.GetTraceId())
	if traceID == "" {
		return nil, fmt.Errorf("live-media: trace_id is required to make retry idempotent: %w", model.ErrEmptyRequestID)
	}

	cur, err := l.svcCtx.TranscodeTasks.FindOne(l.ctx, taskID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, model.ErrTranscodeTaskNotFound
	}
	if cur.State == model.TranscodeStatePending && cur.TraceId == traceID {
		// 同一次重试的重放：不再 ++attempt、不再写事件。
		return transcodeInfo(cur), nil
	}
	if model.IsTranscodeTerminal(cur.State) {
		return nil, model.ErrTerminalState
	}
	if cur.State != model.TranscodeStateFailed {
		return nil, model.ErrInvalidTransition
	}
	if !model.IsValidTranscodeTransition(cur.State, model.TranscodeStatePending) {
		return nil, model.ErrInvalidTransition
	}
	newAttempt := cur.Attempt + 1
	if newAttempt > cur.MaxAttempts {
		l.Errorf("livemedia/RetryLiveTranscode: task_id=%d attempt=%d exceeds max_attempts=%d, operator=%s",
			taskID, newAttempt, cur.MaxAttempts, operator)
		return nil, fmt.Errorf("attempt=%d max_attempts=%d: %w", newAttempt, cur.MaxAttempts, model.ErrAttemptExhausted)
	}

	// heartbeat_at 一并重置为本次重试时刻：与 timeout_at 成对写入，
	// 使「无心跳超时秒数」快照在后续心跳里仍可按时顺延（见 timeoutBudget）。
	now := model.NowUnix()
	budget := timeoutBudget(cur.HeartbeatAt, cur.TimeoutAt)
	if budget <= 0 {
		budget = l.svcCtx.Config.LiveMedia.DefaultTranscodeTimeoutSeconds
	}
	patch := model.TranscodePatch{
		Attempt:     i32p(newAttempt),
		HeartbeatAt: i64p(now),
		TimeoutAt:   i64p(nextTimeoutAt(now, budget)),
		Errno:       i32p(0),
		ErrMsg:      strp(""),
		TraceID:     strp(traceID),
	}
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		aff, updErr := l.svcCtx.TranscodeTasks.UpdateStateTx(ctx, sess, taskID,
			[]int32{model.TranscodeStateFailed}, in.GetExpectedVersion(), model.TranscodeStatePending, patch)
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
				"state":           model.TranscodeStatePending,
				"attempt":         newAttempt,
				"max_attempts":    cur.MaxAttempts,
				"operator":        operator,
				"note":            note,
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
		// 见 StopLiveTranscode 的同处注释：缺这道门禁会返回 (typed nil, nil)，
		// 一次已成功的重试会被读成「task_id=0 的任务」。
		return nil, model.ErrTranscodeTaskNotFound
	}
	return transcodeInfo(latest), nil
}
