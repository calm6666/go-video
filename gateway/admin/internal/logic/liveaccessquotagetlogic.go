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

type LiveAccessQuotaGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 某作用域生效的接入/广播配额（含继承链解析结果）
func NewLiveAccessQuotaGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveAccessQuotaGetLogic {
	return &LiveAccessQuotaGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveAccessQuotaGet 聚合 live-gateway GetAccessQuota。
//
// 只挡 scope=0（QUOTA_SCOPE_UNSPECIFIED 没有对应语义，传下去只会换来一次无意义往返）；
// scope_id 与 scope 的合法组合（GLOBAL 必须 0、其余必须 >0）由服务判定，网关不复算。
//
// 注意本路由回的是**解析后的生效值**而不是某一行原始配置：服务按继承链覆盖并在无 GLOBAL 行时
// 回落到进程配置默认值，因此后台无法从这份响应区分「显式配置」与「继承默认」——
// 契约缺口（AccessQuotaInfo 没有 hit_scopes 字段），已在 .api 与 README 记录。
func (l *LiveAccessQuotaGetLogic) LiveAccessQuotaGet(req *types.ParamLiveAccessQuotaGet) (resp *types.LiveAccessQuotaResponse, err error) {
	if l.svcCtx.LiveGateway == nil {
		return nil, errLiveGatewayNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredID32("scope", req.Scope); err != nil {
		return nil, err
	}
	if err := liveNonNeg("scope_id", req.ScopeId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveGateway.GetAccessQuota(l.ctx, &livegatewayrpc.AccessQuotaReq{
		Scope:   livegatewayrpc.QuotaScope(req.Scope),
		ScopeId: req.ScopeId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveAccessQuotaGet: scope=%d scope_id=%d err=%v", req.Scope, req.ScopeId, err)
		return nil, err
	}
	return &types.LiveAccessQuotaResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveAccessQuotaData{
			Quota:    liveAccessQuotaToAPI(reply),
			HasQuota: reply != nil,
		},
		TTL: 0,
	}, nil
}
