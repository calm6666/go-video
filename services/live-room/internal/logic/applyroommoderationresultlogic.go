package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ApplyRoomModerationResultLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewApplyRoomModerationResultLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ApplyRoomModerationResultLogic {
	return &ApplyRoomModerationResultLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 资料审核结论回写（moderation.result.v1 消费者入口），按合法状态机推进
//
// 结论真值恒在 moderation-orchestrator，本服务只保存投影：
//   - event_id 命中 kind=IdempotencyKindEvent 即重复投递，回 applied=false，不重复推进；
//   - task_id 必须等于 live_room.moderation_task_id，否则是陈旧结论（ErrTaskMismatch），
//     不因为「晚到」就把它覆盖到新任务上；
//   - verify_state 与房间业务状态是两条矩阵：资料通过只把 PENDING 抬到 READY，
//     驳回只把 READY 降回 PENDING；房间在 LIVING 时没有 Living->Pending 这条合法边，
//     此时只回写审核结论并留日志，绝不伪造一次状态迁移；
//   - BANNED 房间不因资料审核通过而解封（只有 LiftBan 能改）。
func (l *ApplyRoomModerationResultLogic) ApplyRoomModerationResult(in *rpc.ApplyRoomModerationResultReq) (*rpc.ApplyRoomModerationResultReply, error) {
	if in == nil {
		return nil, model.ErrEventIDRequired
	}
	if err := checkEventID(in.GetEventId()); err != nil {
		return nil, err
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if in.GetTaskId() <= 0 {
		return nil, fmt.Errorf("%w: task_id=%d", model.ErrTaskMismatch, in.GetTaskId())
	}
	reason, err := checkReason("reason", in.GetReason(), false)
	if err != nil {
		return nil, err
	}
	if int32(in.GetVerdict()) == model.VerifyStateUnspecified && in.GetVerdict() == rpc.ModerationVerdict_VERDICT_UNSPECIFIED {
		return nil, fmt.Errorf("%w", model.ErrVerdictInvalid)
	}
	eventID := strings.TrimSpace(in.GetEventId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcApplyRoomModerationResult, eventID,
		model.IdempotencyKindEvent, in.GetRoomId(), 0, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		return l.replayDuplicate(eventID, in.GetRoomId())
	}

	room, err := l.svcCtx.Rooms.FindOne(l.ctx, in.GetRoomId())
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, model.ErrRoomNotFound
	}
	if room.ModerationTaskID != in.GetTaskId() {
		return nil, fmt.Errorf("%w: room_task=%d event_task=%d", model.ErrTaskMismatch, room.ModerationTaskID, in.GetTaskId())
	}

	target, changed, err := verifyTargetForVerdict(room.VerifyState, in.GetVerdict())
	if err != nil {
		return nil, err
	}
	roomTo, canMoveRoom := roomStateForVerifyResult(target, room.State)

	reply := &rpc.ApplyRoomModerationResultReply{RoomId: room.RoomID}
	switch {
	case !changed && !canMoveRoom:
		// REVIEW 落在 REVIEWING 上，或结论与现状一致：无事可做，明确回 applied=false。
		reply.VerifyState = rpc.VerifyState(room.VerifyState)
		reply.RoomState = rpc.RoomState(room.State)
		reply.Applied = false
		reply.Message = "结论与当前状态一致，未产生迁移"
	default:
		if err := l.apply(room, target, roomTo, canMoveRoom, reason, eventID, in.GetOperator(), traceID); err != nil {
			return nil, err
		}
		fresh, err := l.svcCtx.Rooms.FindOne(l.ctx, room.RoomID)
		if err != nil {
			return nil, err
		}
		if fresh == nil {
			return nil, model.ErrRoomNotFound
		}
		reply.VerifyState = rpc.VerifyState(fresh.VerifyState)
		reply.RoomState = rpc.RoomState(fresh.State)
		reply.Applied = true
		if !canMoveRoom {
			reply.Message = "资料结论已回写；房间当前状态不允许业务状态迁移"
		}
		invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)
	}
	saveDedupResult(l.ctx, l.svcCtx, eventID, reply, l.Logger)
	return reply, nil
}

// apply 把「资料审核结论」与「房间业务状态迁移」写进同一事务（AGENTS.md §8 留审计证据）。
// 房间不需要迁移时只回写 verify_state/reject_reason，不伪造状态变化。
func (l *ApplyRoomModerationResultLogic) apply(room *model.LiveRoom, verifyTarget, roomTo int32,
	canMoveRoom bool, reason, eventID string, operator int64, traceID string) error {
	reject := ""
	if verifyTarget == model.VerifyStateRejected {
		reject = reason
	}
	return l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		if canMoveRoom {
			patch := model.RoomPatch{VerifyState: i32Ptr(verifyTarget), RejectReason: strPtr(reject)}
			ok, err := l.svcCtx.Rooms.TransitionTx(ctx, tx, room.RoomID, room.State, roomTo, room.StateVersion, patch)
			if err != nil {
				return err
			}
			if !ok {
				return model.ErrConcurrentUpdate
			}
			if _, err := l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
				RoomID: room.RoomID, StateType: model.LogTypeRoomState,
				FromState: room.State, ToState: roomTo, OperatorMid: operator,
				Source: model.SourceModerationResult, EventID: eventID,
				Reason: truncateRunes(reason, maxReasonRunes), TraceID: traceID,
			}); err != nil {
				return err
			}
		} else {
			ok, err := l.svcCtx.Rooms.SetVerifyResultTx(ctx, tx, room.RoomID, verifyTarget, reject, []int32{room.State})
			if err != nil {
				return err
			}
			if !ok {
				return model.ErrConcurrentUpdate
			}
		}
		if _, err := l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
			RoomID: room.RoomID, StateType: model.LogTypeVerifyState,
			FromState: room.VerifyState, ToState: verifyTarget, OperatorMid: operator,
			Source: model.SourceModerationResult, EventID: eventID,
			Reason: truncateRunes(reason, maxReasonRunes), TraceID: traceID,
		}); err != nil {
			return err
		}
		return nil
	})
}

// replayDuplicate 回读当前投影并把 applied 归 false：重复投递不得再推进状态。
func (l *ApplyRoomModerationResultLogic) replayDuplicate(eventID string, roomID int64) (*rpc.ApplyRoomModerationResultReply, error) {
	rec, err := dedupRecord(l.ctx, l.svcCtx, rpcApplyRoomModerationResult, eventID)
	if err != nil {
		return nil, err
	}
	room, err := l.svcCtx.Rooms.FindOne(l.ctx, roomID)
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, model.ErrRoomNotFound
	}
	reply := &rpc.ApplyRoomModerationResultReply{
		RoomId:      room.RoomID,
		VerifyState: rpc.VerifyState(room.VerifyState),
		RoomState:   rpc.RoomState(room.State),
		Applied:     false,
		Message:     "同一 event_id 重复投递，未产生迁移",
	}
	_ = rec
	return reply, nil
}
