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

type OpsListSlotsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询推荐位定义（page/state/platform 过滤）
func NewOpsListSlotsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsListSlotsLogic {
	return &OpsListSlotsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsListSlotsLogic) OpsListSlots(req *types.ParamOpsListSlots) (resp *types.OpsSlotsResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeOpsPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.OpsConfig.ListSlots(l.ctx, &opsconfigrpc.ListSlotsReq{
		Ctx:      callCtx,
		Page:     req.Page,
		State:    req.State,
		Platform: opsconfigrpc.ClientPlatform(req.Platform),
		Pn:       pn,
		Ps:       ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsListSlots: operator=%d page=%s state=%d platform=%d pn=%d ps=%d err=%v",
			callCtx.GetOperatorId(), req.Page, req.State, req.Platform, pn, ps, err)
		return nil, err
	}
	return &types.OpsSlotsResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsSlotsData{
			Items: opsSlotsToAPI(reply.GetItems()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
