// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	collectorrpc "go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CollectorDeliveryRetryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 推进到期未发送事件（轮次幂等；行层租约才是重复投递的围栏）
func NewCollectorDeliveryRetryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorDeliveryRetryLogic {
	return &CollectorDeliveryRetryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorDeliveryRetry 转发 event-collector RetryPendingDelivery。
// operator 由会话渲染成 gateway/admin:<admin_id>，表单不得声明自己是谁；
// idempotency_key 必填并原样透传（改一个字符就等于换了执行权）。
//
// 网关只做三件门槛：会话身份、幂等键非空、now/limit 非负（0 是「服务当前时间 / 默认批量」
// 的合法哨兵）。topic 形状、租约围栏、重试上限与「dispatcher 未启用」的 fail-closed
// 全在服务侧（retrypendingdeliverylogic.go）：那里才知道 ec_pending_delivery 的租约归属，
// 网关若在报错前自己判一次，只会多一处会说谎的判断。
//
// dead>0 不是本路由的失败：它是「这一轮里有事件超过重试上限、已转死信」的事实，
// 用四字段信封原样回带，由后台按 collector:deadletter 权限点去处置。
func (l *CollectorDeliveryRetryLogic) CollectorDeliveryRetry(req *types.ParamCollectorDeliveryRetry) (resp *types.CollectorDeliveryRetryResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	if req == nil {
		return nil, errCollectorRequestMissing
	}
	operator, err := collectorOperator(l.ctx, "collectorDeliveryRetry")
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := collectorNonNeg("now", req.Now); err != nil {
		return nil, err
	}
	if err := collectorNonNeg32("limit", req.Limit); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.EventCollector.RetryPendingDelivery(l.ctx, &collectorrpc.RetryPendingDeliveryReq{
		Topic:          req.Topic,
		Now:            req.Now,
		Limit:          req.Limit,
		IdempotencyKey: req.IdempotencyKey,
		Operator:       operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorDeliveryRetry: topic=%s limit=%d operator=%s err=%v",
			req.Topic, req.Limit, operator, err)
		return nil, err
	}
	return &types.CollectorDeliveryRetryResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorDeliveryRetryData{
			Scanned:   reply.GetScanned(),
			Sent:      reply.GetSent(),
			Retrying:  reply.GetRetrying(),
			Dead:      reply.GetDead(),
			NextRunAt: reply.GetNextRunAt(),
		},
		TTL: 0,
	}, nil
}
