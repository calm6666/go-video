// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveRoomBanLiftLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 解除禁播：BANNED→READY，ban_id=0 表示解除当前生效记录
func NewLiveRoomBanLiftLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomBanLiftLogic {
	return &LiveRoomBanLiftLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomBanLift 聚合 live-room LiftBan。
// ban_id=0 是合法入参（解除当前生效记录），所以这里只做非负门槛，不强求指定记录；
// 「该 ban_id 是否属于这个房间」「无生效记录时是回 ban_id=0 还是报错」都由 live-room 判定，
// 网关不预判也不重放。reply.message 原样透出——它是「为什么这次解除没改动」的唯一解释。
func (l *LiveRoomBanLiftLogic) LiveRoomBanLift(req *types.ParamLiveRoomBanLift) (resp *types.LiveRoomBanLiftResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveOperatorGate(l.ctx, "liveRoomBanLift", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("ban_id", req.BanId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveRoom.LiftBan(l.ctx, &liveroomrpc.LiftBanReq{
		RoomId:      req.RoomId,
		BanId:       req.BanId,
		OperatorMid: req.OperatorMid,
		Reason:      req.Reason,
		RequestId:   req.RequestId,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveRoomBanLift: room_id=%d ban_id=%d operator_mid=%d request_id=%s err=%v",
			req.RoomId, req.BanId, req.OperatorMid, req.RequestId, err)
		return nil, err
	}
	return &types.LiveRoomBanLiftResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomBanLiftData{
			BanId:    reply.GetBanId(),
			State:    int32(reply.GetState()),
			Replayed: reply.GetReplayed(),
			Message:  reply.GetMessage(),
		},
		TTL: 0,
	}, nil
}
