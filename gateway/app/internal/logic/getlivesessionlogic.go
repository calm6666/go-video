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

type GetLiveSessionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 观众面：读单场直播（按场次 ID，或按房间取最近一场）
func NewGetLiveSessionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetLiveSessionLogic {
	return &GetLiveSessionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// GetLiveSession session_id 与 room_id 都为 0 是「空条件查询」，网关先拒（参数口径），
// 避免把无意义请求打到服务上得到不确定结果。
// 回放取流不在这里：本接口只给 record_asset_id / record_aid 引用，
// 播放地址必须走 playback 的短期签名入口（AGENTS.md §6）。
func (l *GetLiveSessionLogic) GetLiveSession(req *types.ParamLiveSession) (resp *types.LiveSessionResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	if req.SessionId == 0 && req.RoomId == 0 {
		return nil, errors.New("session_id 与 room_id 至少提供一个")
	}
	reply, err := l.svcCtx.LiveRoom.GetSession(l.ctx, &liveroomrpc.GetSessionReq{
		SessionId: req.SessionId,
		RoomId:    req.RoomId,
		Offset:    req.Offset,
	})
	if err != nil {
		l.Errorf("gateway/app/getLiveSession: session_id=%d room_id=%d offset=%d err=%v",
			req.SessionId, req.RoomId, req.Offset, err)
		return nil, err
	}
	if reply.GetSession() == nil {
		l.Errorf("gateway/app/getLiveSession: session_id=%d room_id=%d 服务返回空场次", req.SessionId, req.RoomId)
		return nil, errors.New("live-room: 场次不存在")
	}
	return &types.LiveSessionResponse{
		Code:    0,
		Message: "ok",
		Data:    types.LiveSessionData{Session: liveSessionToAPI(reply.GetSession())},
		TTL:     0,
	}, nil
}
