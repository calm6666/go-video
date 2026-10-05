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

type LiveRoomInfoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 观众面：按房间 ID 读取直播间
func NewLiveRoomInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomInfoLogic {
	return &LiveRoomInfoLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomInfo 房间读取不要求登录（观众面公开读），但网关不裁剪房间状态字段：
// 禁播/审核状态由 live-room 决定，BANNED/FINISHED 等状态原样透出供端上渲染占位。
func (l *LiveRoomInfoLogic) LiveRoomInfo(req *types.ParamLiveRoom) (resp *types.LiveRoomResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	reply, err := l.svcCtx.LiveRoom.GetRoom(l.ctx, &liveroomrpc.GetRoomReq{
		RoomId:            req.RoomId,
		WithSetting:       req.WithSetting,
		WithActiveSession: req.WithActiveSession,
	})
	if err != nil {
		l.Errorf("gateway/app/liveRoomInfo: room_id=%d err=%v", req.RoomId, err)
		return nil, err
	}
	if reply.GetRoom() == nil {
		// 契约规定房间不存在由 gRPC code 表达；成功却无 room 属于异常，不投影成零值房间。
		l.Errorf("gateway/app/liveRoomInfo: room_id=%d 返回空 room，违反 GetRoom 契约", req.RoomId)
		return nil, errors.New("live-room: 房间不存在或不可读")
	}
	return &types.LiveRoomResponse{
		Code:    0,
		Message: "ok",
		Data:    liveRoomDataFromReply(reply),
		TTL:     0,
	}, nil
}
