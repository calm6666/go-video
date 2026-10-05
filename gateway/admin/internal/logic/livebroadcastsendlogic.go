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

type LiveBroadcastSendLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 房间内公告/系统事件下发（运营身份由网关声明，可丢弃但原因必须可解释）
func NewLiveBroadcastSendLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveBroadcastSendLogic {
	return &LiveBroadcastSendLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveBroadcastSend 聚合 live-gateway BroadcastToRoom（房间内公告/系统事件下发）。
//
// 网关固定的两个位（都**不是**表单字段）：sender_role=OPERATOR、sender_mid=0。
// 后台发的就是运营消息；若把这两项开放给表单，live-gateway 的权限矩阵会按用户态消息要求
// 租约与票据（后台拿不到，也不该拿），或者反过来让后台能伪装成主播/观众发言。
// sender_lease_id / sender_ticket 因此在 .api 里根本不存在——那是客户端凭据。
//
// 幂等键是 message_id（(room_id, message_id) 唯一），原样透传不改写；BroadcastToRoomReq 没有
// request_id 位，因此本路由不接受第二个幂等键。契约缺口：该请求也**没有 operator 位**，
// 后台身份只能落在网关日志与 trace_id 上，广播台账里看不到「哪个运营账号发的」——
// 补法是先给 proto 增加运营主体字段（本轮不改 services/**）。
//
// payload 按字节原样交给服务（大小上限、是否 JSON、去重与限流都由服务判定）；
// accepted=false 时 drop_reason 必须有值，这是服务的可解释性承诺，网关不改写成成功。
// require_reliable=true 且下发通道未接线时服务显式失败，网关原样上抛，不兜成「已发送」。
func (l *LiveBroadcastSendLogic) LiveBroadcastSend(req *types.ParamLiveBroadcastSend) (resp *types.LiveBroadcastSendResponse, err error) {
	if l.svcCtx.LiveGateway == nil {
		return nil, errLiveGatewayNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if _, err := liveGatewayOperator(l.ctx, "liveBroadcastSend"); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("message_id", req.MessageId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveRequiredID32("kind", req.Kind); err != nil {
		return nil, err
	}
	if err := liveNonNeg("expire_at", req.ExpireAt); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("priority", req.Priority); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveGateway.BroadcastToRoom(l.ctx, &livegatewayrpc.BroadcastToRoomReq{
		RoomId:          req.RoomId,
		Kind:            livegatewayrpc.BroadcastKind(req.Kind),
		MessageId:       req.MessageId,
		SenderMid:       0, // 固定：系统消息（proto 注明系统消息为 0）
		SenderRole:      livegatewayrpc.ConnRole_CONN_ROLE_OPERATOR,
		Payload:         []byte(req.Payload),
		TargetRoles:     req.TargetRoles,
		TargetTopics:    req.TargetTopics,
		ExpireAt:        req.ExpireAt,
		Priority:        req.Priority,
		RequireReliable: req.RequireReliable,
		TraceId:         req.TraceId,
	})
	if err != nil {
		// 不打 payload：正文与字节数都不进日志（AGENTS.md §4）。
		l.Errorf("gateway/admin/liveBroadcastSend: room_id=%d kind=%d message_id=%s err=%v",
			req.RoomId, req.Kind, req.MessageId, err)
		return nil, err
	}
	return &types.LiveBroadcastSendResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveBroadcastSendData{
			Accepted:            reply.GetAccepted(),
			MessageId:           reply.GetMessageId(),
			DropReason:          int32(reply.GetDropReason()),
			FanoutNodes:         reply.GetFanoutNodes(),
			TargetedConnections: reply.GetTargetedConnections(),
			EnqueuedAt:          reply.GetEnqueuedAt(),
			Duplicated:          reply.GetDuplicated(),
			RateRemaining:       reply.GetRateRemaining(),
		},
		TTL: 0,
	}, nil
}
