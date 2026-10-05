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

type LiveSessionGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单场直播：按 session_id，或按房间取最近第 offset+1 场
func NewLiveSessionGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveSessionGetLogic {
	return &LiveSessionGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveSessionGet 聚合 live-room GetSession。
// session_id 与 room_id 至少要给一个（同 GetRoom 的理由：全 0 会让下游去查不存在的主键，
// 回「场次不存在」而不是「参数缺失」）。offset 是「从最近一场往前数」的偏移，
// 上限与越界由 live-room 判定。
func (l *LiveSessionGetLogic) LiveSessionGet(req *types.ParamLiveSessionGet) (resp *types.LiveSessionResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveNonNeg("session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("offset", req.Offset); err != nil {
		return nil, err
	}
	if req.SessionId == 0 && req.RoomId == 0 {
		return nil, errLiveSessionSubjectRequired
	}
	reply, err := l.svcCtx.LiveRoom.GetSession(l.ctx, &liveroomrpc.GetSessionReq{
		SessionId: req.SessionId,
		RoomId:    req.RoomId,
		Offset:    req.Offset,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveSessionGet: session_id=%d room_id=%d offset=%d err=%v",
			req.SessionId, req.RoomId, req.Offset, err)
		return nil, err
	}
	return &types.LiveSessionResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveSessionData{
			Session: liveSessionToAPI(reply.GetSession()),
		},
		TTL: 0,
	}, nil
}
