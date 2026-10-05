// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	recallrpc "go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PublishPoolVersionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 原子切换池的当前生效版本（READY→CURRENT，写审计并发 recall.pool.published 事件）
func NewPublishPoolVersionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PublishPoolVersionLogic {
	return &PublishPoolVersionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PublishPoolVersion 转发 recommend-recall PublishPoolVersion。
// operator 由会话渲染成 gateway/admin:<admin_id>，表单不得声明（否则请求体能自称任意后台账号）；
// reason 与 idempotency_key 都是契约必填项，幂等键原样透传不改写。
// 「目标版本是否处于 READY」「切换是否被拒绝」都在服务侧（AGENTS.md §8 状态机口径），
// 网关不预判也不重试切换；event_id 是 outbox 事件 ID，回传后后台可与下游投影对账。
func (l *PublishPoolVersionLogic) PublishPoolVersion(req *types.ParamRecommendPoolVersionPublish) (resp *types.RecommendPoolVersionSwitchResponse, err error) {
	if l.svcCtx.RecommendRecall == nil {
		return nil, errRecallServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	operator, err := recommendOperator(l.ctx, "publishPoolVersion")
	if err != nil {
		return nil, err
	}
	pool, err := recommendPoolRef(req.Pool.Source, req.Pool.PoolKey)
	if err != nil {
		return nil, err
	}
	if err := recommendNonNeg("version", req.Version); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRecall.PublishPoolVersion(l.ctx, &recallrpc.PublishPoolVersionReq{
		Pool:           pool,
		Version:        req.Version,
		Operator:       operator,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/publishPoolVersion: source=%d pool_key=%s version=%d operator=%s idempotency_key=%s err=%v",
			req.Pool.Source, req.Pool.PoolKey, req.Version, operator, req.IdempotencyKey, err)
		return nil, err
	}
	return &types.RecommendPoolVersionSwitchResponse{
		Code:    0,
		Message: "ok",
		Data: recallSwitchData(reply.GetSwitched(), reply.GetPreviousVersion(),
			reply.GetCurrentVersion(), reply.GetDeduplicated(), reply.GetEventId()),
		TTL: 0,
	}, nil
}
