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

type OpsListRolloutRulesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询灰度规则（cfg_key/version/state 过滤）
func NewOpsListRolloutRulesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsListRolloutRulesLogic {
	return &OpsListRolloutRulesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsListRolloutRulesLogic) OpsListRolloutRules(req *types.ParamOpsListRolloutRules) (resp *types.OpsRolloutRulesResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeOpsPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.OpsConfig.ListRolloutRules(l.ctx, &opsconfigrpc.ListRolloutRulesReq{
		Ctx:     callCtx,
		CfgKey:  req.CfgKey,
		Scope:   req.Scope,
		Version: req.Version,
		State:   req.State,
		Pn:      pn,
		Ps:      ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsListRolloutRules: operator=%d cfg_key=%s version=%d state=%d pn=%d ps=%d err=%v",
			callCtx.GetOperatorId(), req.CfgKey, req.Version, req.State, pn, ps, err)
		return nil, err
	}
	return &types.OpsRolloutRulesResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsRolloutRulesData{
			Items: opsRolloutRulesToAPI(reply.GetItems()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
