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

type OpsConfigListVersionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询某配置键的不可变版本历史（含当前正式版本号）
func NewOpsConfigListVersionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsConfigListVersionsLogic {
	return &OpsConfigListVersionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsConfigListVersionsLogic) OpsConfigListVersions(req *types.ParamOpsListConfigVersions) (resp *types.OpsConfigVersionsResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	// 版本历史必须按 (cfg_key[, scope]) 定位：不带键的全库版本列表没有可解释的次序。
	if err := requireNonEmpty("cfg_key", req.CfgKey); err != nil {
		return nil, err
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeOpsPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.OpsConfig.ListConfigVersions(l.ctx, &opsconfigrpc.ListConfigVersionsReq{
		Ctx:    callCtx,
		CfgKey: req.CfgKey,
		Scope:  req.Scope,
		Pn:     pn,
		Ps:     ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsConfigListVersions: operator=%d cfg_key=%s scope=%s pn=%d ps=%d err=%v",
			callCtx.GetOperatorId(), req.CfgKey, req.Scope, pn, ps, err)
		return nil, err
	}
	return &types.OpsConfigVersionsResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsConfigVersionsData{
			Items:         opsConfigVersionsToAPI(reply.GetItems()),
			Total:         reply.GetTotal(),
			LatestVersion: reply.GetLatestVersion(),
		},
		TTL: 0,
	}, nil
}
