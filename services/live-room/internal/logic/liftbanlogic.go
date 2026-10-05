package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type LiftBanLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLiftBanLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiftBanLogic {
	return &LiftBanLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 解除禁播：BANNED → READY/PENDING
//
// 落点由 verify_state 决定（审核通过回 READY，否则回 PENDING），不是无条件回 READY：
// 一个资料都没过审的房间被禁播后又「自动可开播」是状态机漏洞。
// 永久禁播（end_at=0）只能由本方法解除，到期扫描永不自动放行。
func (l *LiftBanLogic) LiftBan(in *rpc.LiftBanReq) (*rpc.LiftBanReply, error) {
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
	now := model.NowUnix()

	ban, err := l.resolveBan(in, room.RoomID, now)
	if err != nil {
		return nil, err
	}
	if ban == nil {
		// 无生效记录：明确回 ban_id=0 + 说明，不报「解除成功」。
		return &rpc.LiftBanReply{
			BanId:   0,
			State:   rpc.RoomState(room.State),
			Message: "该房间当前没有生效禁播记录",
		}, nil
	}
	if ban.State != model.BanStateActive {
		return &rpc.LiftBanReply{
			BanId:   ban.BanID,
			State:   rpc.RoomState(room.State),
			Message: fmt.Sprintf("禁播记录已不生效（state=%d）", ban.State),
		}, nil
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcLiftBan, reqID, model.IdempotencyKindRequest,
		room.RoomID, 0, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcLiftBan, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.LiftBanReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		reply.Replayed = true
		return reply, nil
	}

	var (
		target         = room.State
		concurrentLift bool
	)
	if room.State == model.RoomStateBanned {
		target = roomStateAfterBanLift(room.VerifyState)
	}
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		ok, err := l.svcCtx.Bans.LiftTx(ctx, tx, ban.BanID, in.GetOperatorMid(), reason, model.NowUnix())
		if err != nil {
			return err
		}
		if !ok {
			// 记录已被并发解除：不再写，按幂等重放口径回结果（两次解除不是两次成功）。
			concurrentLift = true
			return errNoOp
		}
		if room.State == model.RoomStateBanned {
			moved, err := l.svcCtx.Rooms.TransitionTx(ctx, tx, room.RoomID, model.RoomStateBanned,
				target, room.StateVersion, model.RoomPatch{BanUntil: i64Ptr(0)})
			if err != nil {
				return err
			}
			if !moved {
				return model.ErrConcurrentUpdate
			}
			if _, err := l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
				RoomID: room.RoomID, StateType: model.LogTypeRoomState,
				FromState: model.RoomStateBanned, ToState: target,
				OperatorMid: in.GetOperatorMid(), Source: model.SourceRPCAdmin,
				RequestID: reqID, TraceID: traceID, Reason: reason,
			}); err != nil {
				return err
			}
			return nil
		}
		// 房间不在 BANNED（例如按主播下发的禁播），记录解除后只清到期投影。
		if room.BanUntil == 0 {
			return nil
		}
		cleared, err := l.svcCtx.Rooms.SetBanUntilTx(ctx, tx, room.RoomID, 0, []int32{room.State})
		if err != nil {
			return err
		}
		if !cleared {
			return model.ErrConcurrentUpdate
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNoOp) {
		return nil, err
	}
	invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)

	fresh, err := l.svcCtx.Rooms.FindOne(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		return nil, model.ErrRoomNotFound
	}
	reply := &rpc.LiftBanReply{
		BanId: ban.BanID,
		State: rpc.RoomState(fresh.State),
	}
	switch {
	case concurrentLift:
		reply.Replayed = true
		reply.Message = "记录已被并发解除，本次未重复写入"
	case room.State == model.RoomStateBanned && fresh.State != target:
		// 回读与目标不一致说明并发下有别的入口改过状态：如实回当前状态，不声称迁到了 target。
		reply.Message = "房间状态已被并发推进，返回当前真实状态"
	default:
		reply.Message = "禁播已解除"
	}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}

// resolveBan 解析要解除的记录：ban_id=0 表示「解除当前生效记录」。
func (l *LiftBanLogic) resolveBan(in *rpc.LiftBanReq, roomID, now int64) (*model.LiveRoomBan, error) {
	if in.GetBanId() <= 0 {
		return l.svcCtx.Bans.FindActiveByRoom(l.ctx, roomID, now)
	}
	ban, err := l.svcCtx.Bans.FindOne(l.ctx, in.GetBanId())
	if err != nil {
		return nil, err
	}
	if ban == nil {
		return nil, fmt.Errorf("%w: ban_id=%d", model.ErrBanNotFound, in.GetBanId())
	}
	if ban.RoomID != roomID {
		return nil, fmt.Errorf("%w: ban_id=%d 属于房间 %d，不属于 room_id=%d",
			model.ErrBanNotFound, ban.BanID, ban.RoomID, roomID)
	}
	return ban, nil
}
