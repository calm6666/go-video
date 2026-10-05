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

type LiveAccessQuotaUpsertLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/更新接入与广播配额（修改者取会话身份，版本 CAS）
func NewLiveAccessQuotaUpsertLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveAccessQuotaUpsertLogic {
	return &LiveAccessQuotaUpsertLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveAccessQuotaUpsert 聚合 live-gateway UpsertAccessQuota（配额新建/更新）。
//
// operator 由会话生成（admin:<admin_id>），AccessQuotaInput 里没有 updated_by 位——
// 「谁改的」由服务按 operator 落账，后台自报修改者只会污染审计。
//
// expected_version 原样透传：0 表示新建（行已存在则由服务回冲突），非 0 是 CAS。
// 网关既不改写版本，也不在冲突后自动重读再重试——配额直接影响所有下发与限流，
// 静默重试会把「两个人同时改同一层配额」藏成一次看似成功的写入。
//
// 取值范围与各 scope/scope_id 的合法组合（GLOBAL 必须 0、TTL 上下限、MaxPayloadBytes 上限等）
// 都由 live-gateway 判定，网关只挡负数与未指定 scope，不复算业务校验。
func (l *LiveAccessQuotaUpsertLogic) LiveAccessQuotaUpsert(req *types.ParamLiveAccessQuotaUpsert) (resp *types.LiveAccessQuotaUpsertResponse, err error) {
	if l.svcCtx.LiveGateway == nil {
		return nil, errLiveGatewayNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	operator, err := liveGatewayOperator(l.ctx, "liveAccessQuotaUpsert")
	if err != nil {
		return nil, err
	}
	if err := liveGatewayIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID32("quota.scope", req.Quota.Scope); err != nil {
		return nil, err
	}
	if err := liveNonNeg("quota.scope_id", req.Quota.ScopeId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("expected_version", req.ExpectedVersion); err != nil {
		return nil, err
	}
	if err := liveQuotaNonNeg(req.Quota); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveGateway.UpsertAccessQuota(l.ctx, &livegatewayrpc.UpsertAccessQuotaReq{
		Quota:           liveAccessQuotaForRPC(req.Quota),
		ExpectedVersion: req.ExpectedVersion,
		Operator:        operator,
		RequestId:       req.RequestId,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveAccessQuotaUpsert: scope=%d scope_id=%d expected_version=%d request_id=%s err=%v",
			req.Quota.Scope, req.Quota.ScopeId, req.ExpectedVersion, req.RequestId, err)
		return nil, err
	}
	return &types.LiveAccessQuotaUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveAccessQuotaUpsertData{
			Quota: liveAccessQuotaToAPI(reply),
		},
		TTL: 0,
	}, nil
}
