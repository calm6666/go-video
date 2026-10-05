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

type ListLiveAnchorsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 观众面：房间主播绑定列表
func NewListLiveAnchorsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLiveAnchorsLogic {
	return &ListLiveAnchorsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListLiveAnchors 只读展示房间的房主/联合主播/房管，绑定记录主键 id 不下发终端；
// 绑定与解绑走主播面 mutateLiveAnchor，单主播房间数上限由 live-room 校验。
func (l *ListLiveAnchorsLogic) ListLiveAnchors(req *types.ParamLiveAnchors) (resp *types.LiveAnchorsResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	reply, err := l.svcCtx.LiveRoom.ListAnchors(l.ctx, &liveroomrpc.ListAnchorsReq{
		RoomId:      req.RoomId,
		Role:        liveroomrpc.AnchorRole(req.Role),
		OnlyEnabled: req.OnlyEnabled,
		Page:        req.Page,
		PageSize:    req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/app/listLiveAnchors: room_id=%d role=%d err=%v", req.RoomId, req.Role, err)
		return nil, err
	}
	return &types.LiveAnchorsResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveAnchorsData{
			Anchors: liveAnchorsToAPI(reply.GetAnchors()),
			Total:   reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
