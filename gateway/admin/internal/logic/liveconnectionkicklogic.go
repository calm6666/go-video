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

type LiveConnectionKickLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 强制下线（可撤销重连票据并写禁止重连窗口）
func NewLiveConnectionKickLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveConnectionKickLogic {
	return &LiveConnectionKickLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveConnectionKick 聚合 live-gateway KickConnection（风控/审核处置）。
//
// 操作者由会话生成：ParamLiveConnectionKick 里没有 operator 字段，网关按 admin:<admin_id> 填入，
// 后台每一次踢人都能追到具体账号（本域的操作者是审计字符串，不是用户 mid 空间，
// 因此不存在 live-room / live-ingest 那种编号空间歧义，也不给表单留自报身份的口）。
//
// mid 与 lease_id 至少给一个（否则服务无从判定断哪条连接）；conn_id 为空表示断该用户在该房间的
// 全部连接——这个「扩大范围」的语义由服务实现，网关不代为补默认值。
// ban_seconds>0 会写禁止重连窗口，属于比普通踢人更重的动作，因此本路由单独占一个权限点。
//
// kicked=false 时服务会回 deny_reason（可解释的拒绝原因），这是正常回包不是错误，
// 网关不把它兜成成功，也不复算权限矩阵。
func (l *LiveConnectionKickLogic) LiveConnectionKick(req *types.ParamLiveConnectionKick) (resp *types.LiveConnectionKickResponse, err error) {
	if l.svcCtx.LiveGateway == nil {
		return nil, errLiveGatewayNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	operator, err := liveGatewayOperator(l.ctx, "liveConnectionKick")
	if err != nil {
		return nil, err
	}
	if err := liveGatewayIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveGatewayKickSubjectGate(req.Mid, req.LeaseId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("ban_seconds", req.BanSeconds); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveGateway.KickConnection(l.ctx, &livegatewayrpc.KickConnectionReq{
		RoomId:        req.RoomId,
		Mid:           req.Mid,
		LeaseId:       req.LeaseId,
		ConnId:        req.ConnId,
		Reason:        req.Reason,
		BanSeconds:    req.BanSeconds,
		RevokeTickets: req.RevokeTickets,
		Operator:      operator,
		RequestId:     req.RequestId,
		TraceId:       req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveConnectionKick: room_id=%d mid=%d ban_seconds=%d request_id=%s err=%v",
			req.RoomId, req.Mid, req.BanSeconds, req.RequestId, err)
		return nil, err
	}
	return &types.LiveConnectionKickResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveConnectionKickData{
			Kicked:            reply.GetKicked(),
			KickedConnections: reply.GetKickedConnections(),
			RevokedTickets:    reply.GetRevokedTickets(),
			BanUntil:          reply.GetBanUntil(),
			DenyReason:        int32(reply.GetDenyReason()),
		},
		TTL: 0,
	}, nil
}
