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

type LiveRoomRouteDrainLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 排空房间路由（expected_version 乐观校验，节点优雅下线）
func NewLiveRoomRouteDrainLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomRouteDrainLogic {
	return &LiveRoomRouteDrainLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomRouteDrain 聚合 live-gateway DrainRoomRoute（节点优雅下线）。
//
// 操作者由会话生成（admin:<admin_id>），表单不得声明 operator；request_id 非空且原样透传。
//
// expected_version 原样透传，网关既不改写也不在冲突后自动重读版本再重试：
// 版本不符是「有人在你看页之后动了这条路由」的信号，代为重试会把并发发布场景下的误伤藏起来。
// 期望值应从 /route/list 读视图回读（那条路由存在的意义之一就在这里）。
//
// target_node_id 为空表示只排空不指定去处，迁移到哪、连接如何收敛全由服务判定；
// reason 的取值合法性（node_shutdown/deploy/scale）也在服务侧，网关不做字符串白名单。
func (l *LiveRoomRouteDrainLogic) LiveRoomRouteDrain(req *types.ParamLiveRoomRouteDrain) (resp *types.LiveRoomRouteDrainResponse, err error) {
	if l.svcCtx.LiveGateway == nil {
		return nil, errLiveGatewayNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	operator, err := liveGatewayOperator(l.ctx, "liveRoomRouteDrain")
	if err != nil {
		return nil, err
	}
	if err := liveGatewayIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveRequiredText("node_id", req.NodeId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("expected_version", req.ExpectedVersion); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveGateway.DrainRoomRoute(l.ctx, &livegatewayrpc.DrainRoomRouteReq{
		RoomId:          req.RoomId,
		NodeId:          req.NodeId,
		ExpectedVersion: req.ExpectedVersion,
		TargetNodeId:    req.TargetNodeId,
		Reason:          req.Reason,
		RequestId:       req.RequestId,
		Operator:        operator,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveRoomRouteDrain: room_id=%d node_id=%s expected_version=%d request_id=%s err=%v",
			req.RoomId, req.NodeId, req.ExpectedVersion, req.RequestId, err)
		return nil, err
	}
	return &types.LiveRoomRouteDrainResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomRouteDrainData{
			Route: liveRoomRouteToAPI(reply),
		},
		TTL: 0,
	}, nil
}
