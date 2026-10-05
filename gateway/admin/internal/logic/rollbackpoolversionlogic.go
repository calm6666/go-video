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

type RollbackPoolVersionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 回滚池版本到历史版本（运营回滚开关，与 publish 同一响应形态）
func NewRollbackPoolVersionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RollbackPoolVersionLogic {
	return &RollbackPoolVersionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RollbackPoolVersion 转发 recommend-recall RollbackPoolVersion。
// 与 publish 分属两个权限点：回滚是「把某个旧版本重新变成线上事实」，授权面比正常发布更窄
// （能发布不等于能把刚上线的版本撤掉）。
// target_version 是否存在、是否已完成写入（BUILDING/FAILED 不可回滚）都由服务判定，
// 网关不查版本状态再自己决定要不要发这个请求——那会把服务的判定复制成两套。
// operator/reason/idempotency_key 门槛与 publish 相同。
func (l *RollbackPoolVersionLogic) RollbackPoolVersion(req *types.ParamRecommendPoolVersionRollback) (resp *types.RecommendPoolVersionSwitchResponse, err error) {
	if l.svcCtx.RecommendRecall == nil {
		return nil, errRecallServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	operator, err := recommendOperator(l.ctx, "rollbackPoolVersion")
	if err != nil {
		return nil, err
	}
	pool, err := recommendPoolRef(req.Pool.Source, req.Pool.PoolKey)
	if err != nil {
		return nil, err
	}
	if err := recommendNonNeg("target_version", req.TargetVersion); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRecall.RollbackPoolVersion(l.ctx, &recallrpc.RollbackPoolVersionReq{
		Pool:           pool,
		TargetVersion:  req.TargetVersion,
		Operator:       operator,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/rollbackPoolVersion: source=%d pool_key=%s target_version=%d operator=%s idempotency_key=%s err=%v",
			req.Pool.Source, req.Pool.PoolKey, req.TargetVersion, operator, req.IdempotencyKey, err)
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
