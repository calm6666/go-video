// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	openplatformrpc "go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpenWebhookDeliveryRetryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 死信重放（只重置既有记录的 attempt，不注入新事件）
func NewOpenWebhookDeliveryRetryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenWebhookDeliveryRetryLogic {
	return &OpenWebhookDeliveryRetryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenWebhookDeliveryRetry 转发 open-platform RetryWebhookDelivery（把既有投递记录重新排队）。
//
// 这条与刻意不开服的 EnqueueWebhookEvent 的分工是「重置既有记录」与「制造新事实」的区别：
// 重放不产生新事件，因此可以安全地给后台；入队不行。
// ignore_dead=true 允许重放已判死信的记录，属「明知上游还没修好也要再打一次」，
// 所以本入口 reason 必填（服务同样 requireReason + requireOperator）；
// 能不能重放（状态机、端点是否还在、是否已验证）由服务判，网关不预读台账去猜结论。
// 响应里的 state 是服务给的真实状态而不是「已入队」的美化值。
func (l *OpenWebhookDeliveryRetryLogic) OpenWebhookDeliveryRetry(req *types.ParamOpenWebhookDeliveryRetry) (resp *types.OpenWebhookDeliveryRetryResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openIDGate("delivery_id", req.DeliveryId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := openOperatorGate(l.ctx, "openWebhookDeliveryRetry", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.RetryWebhookDelivery(l.ctx, &openplatformrpc.RetryWebhookDeliveryReq{
		DeliveryId:  req.DeliveryId,
		OperatorMid: req.OperatorMid,
		IgnoreDead:  req.IgnoreDead,
		Reason:      req.Reason,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/openWebhookDeliveryRetry: delivery_id=%d ignore_dead=%t operator_mid=%d err=%v",
			req.DeliveryId, req.IgnoreDead, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenWebhookDeliveryRetryResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenWebhookDeliveryRetryData{
			DeliveryId:  reply.GetDeliveryId(),
			State:       int32(reply.GetState()),
			NextRetryAt: reply.GetNextRetryAt(),
			Replayed:    reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
