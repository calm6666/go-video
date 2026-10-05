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

type LiveRoomGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 房间详情：按 room_id 或房主 mid，可附带配置与进行中场次
func NewLiveRoomGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomGetLogic {
	return &LiveRoomGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomGet 聚合 live-room GetRoom。
// room_id 与 owner_mid 至少要给一个：两个都省略时下游会去查主键 0 的房间，
// 回给后台的是「房间不存在」而不是「参数缺失」，那是一次会把排障带偏的错误结论。
// 房间不存在、资料驳回原因、禁播到期都由 live-room 判定，网关只投影，不推断可见性。
func (l *LiveRoomGetLogic) LiveRoomGet(req *types.ParamLiveRoomGet) (resp *types.LiveRoomDetailResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveNonNeg("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("owner_mid", req.OwnerMid); err != nil {
		return nil, err
	}
	if req.RoomId == 0 && req.OwnerMid == 0 {
		return nil, errLiveRoomSubjectRequired
	}
	reply, err := l.svcCtx.LiveRoom.GetRoom(l.ctx, &liveroomrpc.GetRoomReq{
		RoomId:            req.RoomId,
		OwnerMid:          req.OwnerMid,
		WithSetting:       req.WithSetting,
		WithActiveSession: req.WithActiveSession,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveRoomGet: room_id=%d owner_mid=%d err=%v", req.RoomId, req.OwnerMid, err)
		return nil, err
	}
	return &types.LiveRoomDetailResponse{
		Code:    0,
		Message: "ok",
		Data:    liveRoomDetailFromReply(reply),
		TTL:     0,
	}, nil
}
