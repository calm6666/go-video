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

type CloseRoomLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCloseRoomLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CloseRoomLogic {
	return &CloseRoomLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 关闭房间：任意非终态 → FINISHED，强制终止进行中场次并保留审计
//
// FINISHED 是终态，因此这里是从未验证过的入口：from 用读到的当前状态、expectVersion 传 0
// （model 约定 0 表示「本服务内部串行路径不校验版本」），迁移本身仍以 state=from 为条件，
// 并发下第二条关闭语句命中 0 行就退回重读，不会写出两个终态。
// reason 只进 live_room_state_log，不进任何终端可见字段。
func (l *CloseRoomLogic) CloseRoom(in *rpc.CloseRoomReq) (*rpc.CloseRoomReply, error) {
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkOperator(in.GetOperatorMid()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	reason, err := checkReason("reason", in.GetReason(), false)
	if err != nil {
		return nil, err
	}
	room, err := l.svcCtx.Rooms.FindOne(l.ctx, in.GetRoomId())
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, model.ErrRoomNotFound
	}
	source := model.SourceRPCClient
	if in.GetAdmin() {
		source = model.SourceRPCAdmin
	}
	if model.RoomStateIsTerminal(room.State) {
		// 目标态已达成：按幂等重放语义回 replayed=true，不报「不可编辑」。
		return &rpc.CloseRoomReply{
			State:    rpc.RoomState(room.State),
			Replayed: true,
		}, nil
	}
	// admin=false 时只有生效房主能关自己的房间；运营侧由 gateway/admin 判定后带 admin=true 进来。
	if !in.GetAdmin() {
		owner, err := l.svcCtx.Anchors.FindOwner(l.ctx, room.RoomID)
		if err != nil {
			return nil, err
		}
		if owner == nil || owner.Mid != in.GetOperatorMid() {
			return nil, fmt.Errorf("%w: operator_mid=%d 不是该房间生效房主，关闭需 admin=true",
				model.ErrAnchorNotOwner, in.GetOperatorMid())
		}
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcCloseRoom, reqID, model.IdempotencyKindRequest,
		room.RoomID, 0, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcCloseRoom, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.CloseRoomReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		reply.Replayed = true
		return reply, nil
	}

	session, err := l.svcCtx.Sessions.FindActiveByRoom(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	var (
		terminatedID int64
		fromState    = room.State
	)
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		if session != nil {
			ok, err := l.svcCtx.Sessions.TransitionTx(ctx, tx, session.SessionID, session.State,
				model.SessionStateTerminated, model.EndReasonRoomClosed, model.NowUnix())
			if err != nil {
				return err
			}
			if !ok {
				return model.ErrConcurrentUpdate
			}
			terminatedID = session.SessionID
			if _, err := l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
				RoomID: room.RoomID, SessionID: session.SessionID, StateType: model.LogTypeSessionState,
				FromState: session.State, ToState: model.SessionStateTerminated,
				OperatorMid: in.GetOperatorMid(), Source: source,
				RequestID: reqID, TraceID: traceID, Reason: reason,
			}); err != nil {
				return err
			}
		}
		ok, err := l.svcCtx.Rooms.TransitionTx(ctx, tx, room.RoomID, fromState,
			model.RoomStateFinished, 0, clearActiveSessionPatch())
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		_, err = l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
			RoomID: room.RoomID, SessionID: terminatedID, StateType: model.LogTypeRoomState,
			FromState: fromState, ToState: model.RoomStateFinished,
			OperatorMid: in.GetOperatorMid(), Source: source,
			RequestID: reqID, TraceID: traceID, Reason: reason,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)

	reply := &rpc.CloseRoomReply{
		State:               rpc.RoomState_ROOM_STATE_FINISHED,
		TerminatedSessionId: terminatedID,
	}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}
