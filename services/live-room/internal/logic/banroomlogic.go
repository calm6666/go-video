package logic

import (
	"context"
	"strings"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type BanRoomLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBanRoomLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BanRoomLogic {
	return &BanRoomLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 禁播：进入 BANNED 并终止进行中场次（运营/系统，需 operator_mid）
//
// 一个房间同一时刻只允许一条生效禁播记录：替换处置区间时同事务先 Lift 旧记录再 Insert 新记录，
// 否则「谁在被解除」会变成两条 Active 记录互相竞争。
// 本服务只落状态与审计证据，踢流由 live-gateway / live-ingest 执行（不直连 CDN）。
func (l *BanRoomLogic) BanRoom(in *rpc.BanRoomReq) (*rpc.BanRoomReply, error) {
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	if err := checkOperator(in.GetOperatorMid()); err != nil {
		return nil, err
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	reason, err := checkReason("reason", in.GetReason(), true)
	if err != nil {
		return nil, err
	}
	now := model.NowUnix()
	endAt, err := banEndAt(int32(in.GetBanType()), in.GetDurationSeconds(), now)
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
	if model.RoomStateIsTerminal(room.State) {
		return nil, model.ErrRoomFinished
	}
	owner, err := l.svcCtx.Anchors.FindOwner(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcBanRoom, reqID, model.IdempotencyKindRequest,
		room.RoomID, 0, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcBanRoom, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.BanRoomReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		reply.Replayed = true
		return reply, nil
	}

	// 已有生效禁播：本次是替换区间，旧记录必须先落到「已解除」终态。
	prev, err := l.svcCtx.Bans.FindActiveByRoom(l.ctx, room.RoomID, now)
	if err != nil {
		return nil, err
	}
	session, err := l.svcCtx.Sessions.FindActiveByRoom(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	ban := &model.LiveRoomBan{
		RoomID:      room.RoomID,
		Mid:         ownerMidOf(owner),
		BanType:     int32(in.GetBanType()),
		Reason:      reason,
		StartAt:     now,
		EndAt:       endAt,
		State:       model.BanStateActive,
		OperatorMid: in.GetOperatorMid(),
		TraceID:     traceID,
	}

	var (
		banID         int64
		terminatedID  int64
		needRoomState = room.State != model.RoomStateBanned
	)
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		if prev != nil {
			ok, err := l.svcCtx.Bans.LiftTx(ctx, tx, prev.BanID, in.GetOperatorMid(),
				"replaced by a new ban", now)
			if err != nil {
				return err
			}
			if !ok {
				return model.ErrConcurrentUpdate
			}
		}
		id, err := l.svcCtx.Bans.InsertTx(ctx, tx, ban)
		if err != nil {
			return err
		}
		banID = id
		if session != nil {
			ok, err := l.svcCtx.Sessions.TransitionTx(ctx, tx, session.SessionID, session.State,
				model.SessionStateTerminated, model.EndReasonBanned, model.NowUnix())
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
				OperatorMid: in.GetOperatorMid(), Source: model.SourceRPCAdmin,
				RequestID: reqID, TraceID: traceID, Reason: reason,
			}); err != nil {
				return err
			}
		}
		if needRoomState {
			ok, err := l.svcCtx.Rooms.TransitionTx(ctx, tx, room.RoomID, room.State,
				model.RoomStateBanned, room.StateVersion,
				model.RoomPatch{BanUntil: i64Ptr(endAt)})
			if err != nil {
				return err
			}
			if !ok {
				return model.ErrConcurrentUpdate
			}
			if _, err := l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
				RoomID: room.RoomID, SessionID: terminatedID, StateType: model.LogTypeRoomState,
				FromState: room.State, ToState: model.RoomStateBanned,
				OperatorMid: in.GetOperatorMid(), Source: model.SourceRPCAdmin,
				RequestID: reqID, TraceID: traceID, Reason: reason,
			}); err != nil {
				return err
			}
			return nil
		}
		// 已在 BANNED：矩阵无自边，只替换禁播记录并回写到期投影，不伪造一次状态迁移。
		ok, err := l.svcCtx.Rooms.SetBanUntilTx(ctx, tx, room.RoomID, endAt,
			[]int32{model.RoomStateBanned})
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		_, err = l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
			RoomID: room.RoomID, SessionID: terminatedID, StateType: model.LogTypeRoomState,
			FromState: model.RoomStateBanned, ToState: model.RoomStateBanned,
			OperatorMid: in.GetOperatorMid(), Source: model.SourceRPCAdmin,
			RequestID: reqID, TraceID: traceID, Reason: "ban window replaced",
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)

	reply := &rpc.BanRoomReply{
		BanId:               banID,
		State:               rpc.RoomState_ROOM_STATE_BANNED,
		TerminatedSessionId: terminatedID,
		EndAt:               endAt,
	}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}

// ownerMidOf 取生效房主 mid；无生效房主（异常数据）时回 0，禁播记录不归属任何主播。
func ownerMidOf(owner *model.LiveRoomAnchor) int64 {
	if owner == nil {
		return 0
	}
	return owner.Mid
}
