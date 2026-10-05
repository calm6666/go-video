// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	livegatewayrpc "go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveRoomConnectionListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 房间在线连接列表（Redis 视图；不回显重连票据）
func NewLiveRoomConnectionListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomConnectionListLogic {
	return &LiveRoomConnectionListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomConnectionList 聚合 live-gateway ListRoomConnections。
//
// 只读面，不挂 AdminPermission（与 live-room / live-ingest 的 list 同一口径）。
// ListRoomConnectionsReq 没有 operator 位，角色与状态由服务按租约判定，网关不复算。
//
// 投影刻意丢掉 reconnect_ticket（一次性重连凭据，回显到后台就是让控制台当凭据通道），
// 保留 lease_id / conn_id：/connection/kick 需要它们定位单条连接，而服务端仍会校验三元组。
// snapshot_from_cache=true 表示 Redis 不可用、这份视图偏旧，必须回传给后台而不是被当成实时在线数。
func (l *LiveRoomConnectionListLogic) LiveRoomConnectionList(req *types.ParamLiveRoomConnectionList) (resp *types.LiveRoomConnectionListResponse, err error) {
	if l.svcCtx.LiveGateway == nil {
		return nil, errLiveGatewayNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("role", req.Role); err != nil {
		return nil, err
	}
	page, err := liveGatewayPage(req.Pn, req.Ps)
	if err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveGateway.ListRoomConnections(l.ctx, &livegatewayrpc.ListRoomConnectionsReq{
		RoomId: req.RoomId,
		Mid:    req.Mid,
		Role:   livegatewayrpc.ConnRole(req.Role),
		Page:   page,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveRoomConnectionList: room_id=%d mid=%d role=%d err=%v",
			req.RoomId, req.Mid, req.Role, err)
		return nil, err
	}
	return &types.LiveRoomConnectionListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomConnectionListData{
			List:              liveConnectionLeasesToAPI(reply.GetLeases()),
			Total:             livePageTotal(reply.GetPage()),
			SnapshotFromCache: reply.GetSnapshotFromCache(),
		},
		TTL: 0,
	}, nil
}
