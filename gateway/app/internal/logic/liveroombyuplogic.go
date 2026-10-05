// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveRoomByUpLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 观众面：按 UP 主读取其生效中的直播间
func NewLiveRoomByUpLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomByUpLogic {
	return &LiveRoomByUpLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomByUp 「按 UP 主查直播间」复用同一个 GetRoom RPC：owner_mid 走
// live_room_anchor 生效房主反查，房间归属与是否可看由 live-room 判定（AGENTS.md §5）。
// 主播无生效房间时服务返回 NotFound，网关不降级成「room_id=0 的空成功」。
func (l *LiveRoomByUpLogic) LiveRoomByUp(req *types.ParamLiveRoomByUp) (resp *types.LiveRoomResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	reply, err := l.svcCtx.LiveRoom.GetRoom(l.ctx, &liveroomrpc.GetRoomReq{
		OwnerMid:          req.OwnerMid,
		WithSetting:       req.WithSetting,
		WithActiveSession: req.WithActiveSession,
	})
	if err != nil {
		l.Errorf("gateway/app/liveRoomByUp: owner_mid=%d err=%v", req.OwnerMid, err)
		return nil, err
	}
	if reply.GetRoom() == nil {
		l.Errorf("gateway/app/liveRoomByUp: owner_mid=%d 服务返回空房间，按未找到处理", req.OwnerMid)
		return nil, errors.New("该 UP 主当前没有生效中的直播间")
	}
	return &types.LiveRoomResponse{
		Code:    0,
		Message: "ok",
		Data:    liveRoomDataFromReply(reply),
		TTL:     0,
	}, nil
}
