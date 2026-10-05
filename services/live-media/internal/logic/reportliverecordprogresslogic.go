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

type ReportLiveRecordProgressLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportLiveRecordProgressLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportLiveRecordProgressLogic {
	return &ReportLiveRecordProgressLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Worker 上报录制心跳与状态（含超时/断点续录）
//
// 本方法只推进「已产出到第几片」的水位与状态，不登记切片（切片走 ReportRecordSegment）。
// 三条不变量：
//  1. last_seq 单调：上报值小于行内水位即 model.ErrSeqNotMonotonic 直接拒绝。
//     UpdateState 内部另有 GREATEST(last_seq, ?) 兜底，但拒绝才能让 Worker 知道自己在重投旧状态，
//     否则断点续录的起点会被一次迟到的心跳悄悄改错。
//  2. 终态不可复活：STOPPED/CANCELLED 的行只接受同值重放（否则迟到心跳会产出重复切片）。
//  3. 心跳同状态重投时只顺延 heartbeat_at/timeout_at，放弃版本校验、不发事件。
//
// 计数列（segment_count/gap_count/recorded_duration_ms）是派生值：进入 STOPPED/FAILED 时
// 用 RefreshStats 从切片表重算，禁止按上报值累加（重放与并发都会双计）。
// record_end_at 只在 STOPPED 时写：FAILED 可续录，写结束时刻会留下假的终值。
func (l *ReportLiveRecordProgressLogic) ReportLiveRecordProgress(in *rpc.ReportLiveRecordProgressReq) (*rpc.LiveRecordTaskInfo, error) {
	recordID := in.GetRecordId()
	if err := checkPositive("record_id", recordID, model.ErrRecordTaskNotFound); err != nil {
		return nil, err
	}
	target := int32(in.GetState())
	if err := checkRecordState(target); err != nil {
		return nil, err
	}
	reason := int32(in.GetReason())
	if err := checkFailureReason(reason); err != nil {
		return nil, err
	}
	if in.GetLastSeq() < 0 {
		return nil, fmt.Errorf("live-media: last_seq=%d must not be negative: %w",
			in.GetLastSeq(), model.ErrInvalidSeq)
	}
	switch target {
	case model.RecordStatePending:
		// 登记态只由 StartLiveRecord 写入；FAILED→PENDING 的重开也走 Start（新建行、沿用水位）。
		return nil, fmt.Errorf("live-media: PENDING is not a reportable record state: %w", model.ErrInvalidTransition)
	case model.RecordStateCancelled:
		// 本服务未提供 CancelLiveRecord 入口（见 README 已知缺口），Worker 无权代为取消。
		return nil, fmt.Errorf("live-media: CANCELLED is not a reportable record state: %w", model.ErrInvalidTransition)
	}
	if target == model.RecordStateFailed && reason == model.ReasonUnspecified {
		return nil, fmt.Errorf("live-media: FAILED report needs a concrete reason (TIMEOUT/SOURCE_LOST/STORAGE): %w",
			model.ErrInvalidTransition)
	}
	workerID := sanitizeWorkerID(in.GetWorkerId())
	traceID := sanitizeTraceID(in.GetTraceId())

	cur, err := l.svcCtx.RecordTasks.FindOne(l.ctx, recordID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, model.ErrRecordTaskNotFound
	}
	if in.GetLastSeq() < cur.LastSeq {
		return nil, fmt.Errorf("live-media: record_id=%d reported last_seq=%d < current %d: %w",
			recordID, in.GetLastSeq(), cur.LastSeq, model.ErrSeqNotMonotonic)
	}

	if model.IsRecordTerminal(cur.State) {
		if cur.State == target {
			return recordInfo(cur), nil
		}
		l.Errorf("livemedia/ReportLiveRecordProgress: reject report on terminal record_id=%d state=%d incoming=%d worker=%s",
			recordID, cur.State, target, workerID)
		return nil, model.ErrTerminalState
	}
	if !model.IsValidRecordTransition(cur.State, target) {
		return nil, fmt.Errorf("live-media: record record_id=%d state %d->%d: %w",
			recordID, cur.State, target, model.ErrInvalidTransition)
	}

	// 心跳：Worker 给了就用它（复现时钟偏移），没给由服务端兜底。
	now := model.NowUnix()
	heartbeatAt := in.GetHeartbeatAt()
	if heartbeatAt <= 0 {
		heartbeatAt = now
	}
	budget := timeoutBudget(cur.HeartbeatAt, cur.TimeoutAt)
	lastSeq := in.GetLastSeq()

	// --- 纯心跳：状态与水位都未变化 ---
	if cur.State == target && cur.LastSeq == lastSeq {
		aff, updErr := l.svcCtx.RecordTasks.UpdateState(l.ctx, recordID, []int32{cur.State}, 0,
			model.RecordPatch{
				State:       cur.State,
				HeartbeatAt: i64p(heartbeatAt),
				TimeoutAt:   i64p(nextTimeoutAt(heartbeatAt, budget)),
				TraceID:     strp(traceID),
			})
		if updErr != nil {
			return nil, updErr
		}
		latest, findErr := l.svcCtx.RecordTasks.FindOne(l.ctx, recordID)
		if findErr != nil {
			return nil, findErr
		}
		if aff == 0 {
			if latest == nil {
				return nil, model.ErrRecordTaskNotFound
			}
			if latest.State != cur.State {
				return nil, fmt.Errorf("live-media: record heartbeat lost race record_id=%d state=%d: %w",
					recordID, latest.State, model.ErrInvalidTransition)
			}
			// 同一秒内重投：值未变化，MySQL 也返回 0 行，按幂等成功处理。
		}
		return recordInfo(latest), nil
	}

	patch := model.RecordPatch{
		State:       target,
		LastSeq:     i64p(lastSeq),
		HeartbeatAt: i64p(heartbeatAt),
		TimeoutAt:   i64p(nextTimeoutAt(heartbeatAt, budget)),
		Errno:       i32p(in.GetErrno()),
		ErrMsg:      strp(sanitizeErrMsg(in.GetErrMsg())),
		TraceID:     strp(traceID),
	}
	if reason != model.ReasonUnspecified {
		patch.Reason = i32p(reason)
	}
	if target == model.RecordStateRecording && cur.RecordStartAt == 0 {
		// 实际开始时刻只记首次，Worker 重启后的续录不覆盖它。
		patch.RecordStartAt = i64p(heartbeatAt)
	}
	if target == model.RecordStateStopped {
		patch.RecordEndAt = i64p(heartbeatAt)
	}

	// 缺口事实必须在事务前读：StatsInRange 走主连接，看不到本事务未提交的写入（见 recordedGaps）。
	settled := target == model.RecordStateStopped || target == model.RecordStateFailed
	var gaps, registered int64
	if settled {
		if gaps, registered, err = recordedGaps(l.ctx, l.svcCtx.Segments, recordID, lastSeq); err != nil {
			return nil, err
		}
	}

	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		aff, updErr := l.svcCtx.RecordTasks.UpdateStateTx(ctx, sess, recordID,
			[]int32{cur.State}, in.GetExpectedVersion(), patch)
		if updErr != nil {
			return updErr
		}
		if aff == 0 {
			return classifyRecordZeroRow(ctx, l.svcCtx.RecordTasks, recordID, in.GetExpectedVersion())
		}
		if settled {
			// 计数由切片表重算：上报的 last_seq 只是水位锚点，段数/时长/缺口都可重算。
			if _, refErr := l.svcCtx.RecordTasks.RefreshStatsTx(ctx, sess, recordID); refErr != nil {
				return refErr
			}
		}
		if target == model.RecordStateStopped {
			// 切片清单就绪：回放拼接的触发点。
			if evErr := appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeRecordStopped,
				model.AggregateRecordTask, recordID, cur.RoomId, map[string]any{
					"record_id":       recordID,
					"room_id":         cur.RoomId,
					"live_session_id": cur.LiveSession,
					"last_seq":        lastSeq,
					"gap_count":       gaps,
					"registered":      registered,
					"record_end_at":   heartbeatAt,
					"worker_id":       workerID,
				}, traceID); evErr != nil {
				return evErr
			}
		} else if cur.State != target {
			if evErr := appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeRecordStateChanged,
				model.AggregateRecordTask, recordID, cur.RoomId, map[string]any{
					"record_id":       recordID,
					"room_id":         cur.RoomId,
					"live_session_id": cur.LiveSession,
					"prev_state":      cur.State,
					"state":           target,
					"last_seq":        lastSeq,
					"reason":          reason,
					"errno":           in.GetErrno(),
					"worker_id":       workerID,
				}, traceID); evErr != nil {
				return evErr
			}
		}
		if gaps > 0 && target == model.RecordStateStopped {
			// 收尾仍有洞：交给回放侧决策是否允许带洞拼接（SubmitReplayTask.allow_gaps）。
			// 只在 STOPPED 发，FAILED 不发：FAILED 可续录（水位还会往前推），
			// 而 SubmitReplayTask 只接受 STOPPED，此时报洞等于给一个走不到的决策送一份临时数字。
			return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeRecordGapDetected,
				model.AggregateRecordTask, recordID, cur.RoomId, map[string]any{
					"record_id":       recordID,
					"room_id":         cur.RoomId,
					"live_session_id": cur.LiveSession,
					"state":           target,
					"expected":        lastSeq,
					"registered":      registered,
					"gap_count":       gaps,
					"worker_id":       workerID,
				}, traceID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	latest, err := l.svcCtx.RecordTasks.FindOne(l.ctx, recordID)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, model.ErrRecordTaskNotFound
	}
	return recordInfo(latest), nil
}
