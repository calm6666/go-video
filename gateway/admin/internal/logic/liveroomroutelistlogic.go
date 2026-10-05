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

type LiveRoomRouteListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 房间路由分页（按节点/状态过滤，含排空中与已下线）
func NewLiveRoomRouteListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomRouteListLogic {
	return &LiveRoomRouteListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomRouteList 聚合 live-gateway ListRoomRoutes。
//
// 这是 /route/drain 的前置读视图：排空要带 expected_version，运营必须先从这儿回读当前版本，
// 因此本路由的存在意义之一就是让乐观锁的「期望值」有可信来源，而不是让人凭记忆填。
//
// state=0 由服务解释为「不过滤」（含排空中与已下线），网关不改写取值；
// 分页上限（ps ≤ 50）由 live-gateway 夹取，回读的 total 是服务给的口径，网关不二次计数。
func (l *LiveRoomRouteListLogic) LiveRoomRouteList(req *types.ParamLiveRoomRouteList) (resp *types.LiveRoomRouteListResponse, err error) {
	if l.svcCtx.LiveGateway == nil {
		return nil, errLiveGatewayNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	page, err := liveGatewayPage(req.Pn, req.Ps)
	if err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveGateway.ListRoomRoutes(l.ctx, &livegatewayrpc.ListRoomRoutesReq{
		NodeId: req.NodeId,
		State:  livegatewayrpc.RouteState(req.State),
		Page:   page,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveRoomRouteList: node_id=%s state=%d err=%v", req.NodeId, req.State, err)
		return nil, err
	}
	return &types.LiveRoomRouteListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomRouteListData{
			List:  liveRoomRoutesToAPI(reply.GetRoutes()),
			Total: livePageTotal(reply.GetPage()),
		},
		TTL: 0,
	}, nil
}
