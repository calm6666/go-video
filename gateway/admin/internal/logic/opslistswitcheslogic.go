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

type OpsListSwitchesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询客户端开关（platform/switch_key/enabled 过滤）
func NewOpsListSwitchesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsListSwitchesLogic {
	return &OpsListSwitchesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsListSwitchesLogic) OpsListSwitches(req *types.ParamOpsListSwitches) (resp *types.OpsSwitchesResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeOpsPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.OpsConfig.ListClientSwitches(l.ctx, &opsconfigrpc.ListClientSwitchesReq{
		Ctx:       callCtx,
		Platform:  opsconfigrpc.ClientPlatform(req.Platform),
		SwitchKey: req.SwitchKey,
		Enabled:   req.Enabled,
		Pn:        pn,
		Ps:        ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsListSwitches: operator=%d platform=%d switch_key=%s enabled=%d pn=%d ps=%d err=%v",
			callCtx.GetOperatorId(), req.Platform, req.SwitchKey, req.Enabled, pn, ps, err)
		return nil, err
	}
	return &types.OpsSwitchesResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsSwitchesData{
			Items: opsClientSwitchesToAPI(reply.GetItems()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
