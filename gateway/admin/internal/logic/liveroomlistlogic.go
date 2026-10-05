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

type LiveRoomListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 房间分页检索（状态/分区/房主过滤）
func NewLiveRoomListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomListLogic {
	return &LiveRoomListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomList 聚合 live-room ListRooms。
// 用 POST + JSON body 而不是 GET query，与 /admin/cron 的列表口径一致：过滤条件会增长，
// 长度受 URL 限制的查询串不适合做运营检索。
// state/order 的 0 值就是「不过滤 / 默认排序」，页大小上限与越界判定归 live-room
// （它按自己的配置截断并回传实际 page_size），网关只挡住负数这种无对应语义的输入。
func (l *LiveRoomListLogic) LiveRoomList(req *types.ParamLiveRoomList) (resp *types.LiveRoomListResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveNonNeg("owner_mid", req.OwnerMid); err != nil {
		return nil, err
	}
	if err := liveNonNeg("area_id", req.AreaId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("order", req.Order); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("page", req.Page); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("page_size", req.PageSize); err != nil {
		return nil, err
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
		l.Errorf("gateway/admin/liveRoomList: owner_mid=%d area_id=%d state=%d page=%d err=%v",
			req.OwnerMid, req.AreaId, req.State, req.Page, err)
		return nil, err
	}
	return &types.LiveRoomListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomListData{
			List:     liveRoomsToAPI(reply.GetRooms()),
			Total:    reply.GetTotal(),
			Page:     reply.GetPage(),
			PageSize: reply.GetPageSize(),
		},
		TTL: 0,
	}, nil
}
