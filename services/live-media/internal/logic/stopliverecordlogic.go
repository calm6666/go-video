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

type StopLiveRecordLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewStopLiveRecordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StopLiveRecordLogic {
	return &StopLiveRecordLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 停止录制（RECORDING→STOPPING，最后一片落库后 STOPPED）
//
// 幂等口径以状态为准（同 StopLiveTranscode）：已在 STOPPING 的重复请求返回当前行，
// 不重复推进、不再写 Outbox。request_id 只做归因与日志——它是 live_record_task 的登记
// 幂等键（uniq_request_id），状态推进不允许覆写它。
//
// 本方法只下发「停止指令」：end_at 写进期望终点列，实际结束时刻 record_end_at 与终态
// STOPPED 只能由 Worker 在最后一片落库后经 ReportLiveRecordProgress 上报。
// 提前把行标成 STOPPED 会让回放以为区间完整而尾部还在写。
// STOPPED 之后才允许 SubmitReplayTask；本方法不自动提交回放（发布决策归 video/运营）。
func (l *StopLiveRecordLogic) StopLiveRecord(in *rpc.StopLiveRecordReq) (*rpc.LiveRecordTaskInfo, error) {
	recordID := in.GetRecordId()
	if err := checkPositive("record_id", recordID, model.ErrRecordTaskNotFound); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	if err := checkFailureReason(int32(in.GetReason())); err != nil {
		return nil, err
	}
	traceID := sanitizeTraceID(in.GetTraceId())
	operator := optionalOperator(in.GetOperator())

	cur, err := l.svcCtx.RecordTasks.FindOne(l.ctx, recordID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, model.ErrRecordTaskNotFound
	}
	if in.GetEndAt() > 0 && cur.RecordStartAt > 0 && in.GetEndAt() < cur.RecordStartAt {
		// 提前截断合法（主播临时下播），但终点早于实际开始点不可能成立。
		return nil, fmt.Errorf("live-media: end_at=%d < record_start_at=%d: %w",
			in.GetEndAt(), cur.RecordStartAt, model.ErrInvalidSegmentRange)
	}

	switch cur.State {
	case model.RecordStateStopping:
		// 停止指令已下发过：重放返回当前行。
		return recordInfo(cur), nil
	case model.RecordStateStopped, model.RecordStateCancelled:
		return nil, model.ErrTerminalState
	case model.RecordStatePending:
		// 还没开录无需「停止」：本服务未提供 CancelLiveRecord 入口（见 README 已知缺口），
		// PENDING 的行由心跳超时清扫收敛，不接受停止指令。
		return nil, fmt.Errorf("live-media: record_id=%d still PENDING, stop needs RECORDING: %w",
			recordID, model.ErrInvalidTransition)
	case model.RecordStateFailed:
		// 失败后重新开录用 StartLiveRecord（last_seq 不回退，从断点续录）。
		return nil, model.ErrInvalidTransition
	}
	if !model.IsValidRecordTransition(cur.State, model.RecordStateStopping) {
		return nil, model.ErrInvalidTransition
	}

	patch := model.RecordPatch{
		Reason:  i32p(int32(in.GetReason())),
		TraceID: strp(traceID),
	}
	if in.GetEndAt() > 0 {
		// 只覆盖期望终点：实际结束时刻仍由 Worker 上报（见方法注释）。
		patch.EndAt = i64p(in.GetEndAt())
	}
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		stopPatch := patch
		stopPatch.State = model.RecordStateStopping
		aff, updErr := l.svcCtx.RecordTasks.UpdateStateTx(ctx, sess, recordID,
			[]int32{model.RecordStateRecording}, in.GetExpectedVersion(), stopPatch)
		if updErr != nil {
			return updErr
		}
		if aff == 0 {
			return classifyRecordZeroRow(ctx, l.svcCtx.RecordTasks, recordID, in.GetExpectedVersion())
		}
		return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeRecordStateChanged,
			model.AggregateRecordTask, recordID, cur.RoomId, map[string]any{
				"record_id":       recordID,
				"room_id":         cur.RoomId,
				"live_session_id": cur.LiveSession,
				"prev_state":      cur.State,
				"state":           model.RecordStateStopping,
				"expected_end_at": in.GetEndAt(),
				"last_seq":        cur.LastSeq,
				"reason":          int32(in.GetReason()),
				"operator":        operator,
			}, traceID)
	})
	if err != nil {
		return nil, err
	}

	latest, err := l.svcCtx.RecordTasks.FindOne(l.ctx, recordID)
	if err != nil {
		return nil, err
	}
	return recordInfo(latest), nil
}
