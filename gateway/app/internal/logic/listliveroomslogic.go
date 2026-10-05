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

type ListLiveRoomsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 观众面：房间分页浏览（发现页/主播主页）
func NewListLiveRoomsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLiveRoomsLogic {
	return &ListLiveRoomsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListLiveRooms page_size 为 0 时交给服务取默认并截断到上限（docs/api-and-events.md §2：
// 禁止客户端指定无界页大小）。返回空列表是合法结果（该分区暂无直播间），
// 与服务错误是两回事——错误一律上抛，不伪装成空列表。
func (l *ListLiveRoomsLogic) ListLiveRooms(req *types.ParamLiveRooms) (resp *types.LiveRoomsResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	reply, err := l.svcCtx.LiveRoom.ListRooms(l.ctx, &liveroomrpc.ListRoomsReq{
		OwnerMid: req.OwnerMid,
		AreaId:   req.AreaId,
		State:    liveroomrpc.RoomState(req.State),
		Order:    liveroomrpc.RoomOrder(req.Order),
		Page:     req.Page,
		PageSize: req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/app/listLiveRooms: owner_mid=%d area_id=%d state=%d err=%v",
			req.OwnerMid, req.AreaId, req.State, err)
		return nil, err
	}
	return &types.LiveRoomsResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomsData{
			Rooms:    liveRoomsToAPI(reply.GetRooms()),
			Total:    reply.GetTotal(),
			Page:     reply.GetPage(),
			PageSize: reply.GetPageSize(),
		},
		TTL: 0,
	}, nil
}
