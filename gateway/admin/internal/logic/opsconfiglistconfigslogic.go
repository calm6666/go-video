// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	opsconfigrpc "go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpsConfigListConfigsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询配置项（scope/keyword/state 过滤，ps 上限 100）
func NewOpsConfigListConfigsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsConfigListConfigsLogic {
	return &OpsConfigListConfigsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpsConfigListConfigs 分页查询配置项。scope/keyword/state 全部可空（空=不过滤），
// 过滤语义与 ps 上限由 ops-config 判定，网关只收敛分页与补主体。
func (l *OpsConfigListConfigsLogic) OpsConfigListConfigs(req *types.ParamOpsListConfigs) (resp *types.OpsConfigsResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeOpsPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.OpsConfig.ListConfigs(l.ctx, &opsconfigrpc.ListConfigsReq{
		Ctx:     callCtx,
		Scope:   req.Scope,
		Keyword: req.Keyword,
		State:   req.State,
		Pn:      pn,
		Ps:      ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsConfigListConfigs: operator=%d scope=%s keyword=%s state=%d pn=%d ps=%d err=%v",
			callCtx.GetOperatorId(), req.Scope, req.Keyword, req.State, pn, ps, err)
		return nil, err
	}
	return &types.OpsConfigsResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsConfigsData{
			Items: opsConfigItemsToAPI(reply.GetItems()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
