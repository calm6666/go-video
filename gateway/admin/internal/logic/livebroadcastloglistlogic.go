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

type LiveBroadcastLogListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 广播审计流水（按房间；只有载荷摘要，没有正文）
func NewLiveBroadcastLogListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveBroadcastLogListLogic {
	return &LiveBroadcastLogListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveBroadcastLogList 聚合 live-gateway ListBroadcastLogs。
//
// room_id 必填：服务按房间分片存审计，不带房间的查法是跨房间全扫，运营页不该发起这种查询。
//
// 这条流水只有 payload_digest 与 payload_bytes——**没有正文字段**（弹幕与私信正文不落
// live-gateway，这是契约自带的隐私边界），因此后台在这里看不到用户消息内容，
// 网关也没有任何可以「把正文补回来」的下游可调（不读-as-user，见 README 的边界说明）。
// state/drop_reason 按服务口径原样回显（1 已下发、2 已丢弃、3 越权拒绝、4 重复丢弃），
// 网关不替它判断「这条该不该被丢」。
func (l *LiveBroadcastLogListLogic) LiveBroadcastLogList(req *types.ParamLiveBroadcastLogList) (resp *types.LiveBroadcastLogListResponse, err error) {
	if l.svcCtx.LiveGateway == nil {
		return nil, errLiveGatewayNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("kind", req.Kind); err != nil {
		return nil, err
	}
	if err := liveNonNeg("sender_mid", req.SenderMid); err != nil {
		return nil, err
	}
	page, err := liveGatewayPage(req.Pn, req.Ps)
	if err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveGateway.ListBroadcastLogs(l.ctx, &livegatewayrpc.ListBroadcastLogsReq{
		RoomId:      req.RoomId,
		Kind:        livegatewayrpc.BroadcastKind(req.Kind),
		SenderMid:   req.SenderMid,
		OnlyDropped: req.OnlyDropped,
		Page:        page,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveBroadcastLogList: room_id=%d kind=%d only_dropped=%t err=%v",
			req.RoomId, req.Kind, req.OnlyDropped, err)
		return nil, err
	}
	return &types.LiveBroadcastLogListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveBroadcastLogListData{
			List:  liveBroadcastLogsToAPI(reply.GetLogs()),
			Total: livePageTotal(reply.GetPage()),
		},
		TTL: 0,
	}, nil
}
