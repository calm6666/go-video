// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	riskcontrolrpc "go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPunishmentsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询处罚记录
func NewListPunishmentsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPunishmentsLogic {
	return &ListPunishmentsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 处罚列表：聚合 risk-control ListPunishments RPC。
// 分页口径与服务端一致（ps 上限 50，越界截断到 50）；
// 响应含运营内部 reason，只允许出现在 /admin 链路，不得被终端接口复用。
func (l *ListPunishmentsLogic) ListPunishments(req *types.ParamListPunishments) (resp *types.RiskPunishmentsResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	if err := requireOperatorID(req.OperatorId); err != nil {
		return nil, err
	}
	scope, err := riskGuardedAction(req.Scope, true) // 0 表示不按 scope 过滤
	if err != nil {
		return nil, err
	}
	state, err := riskPunishmentState(req.State) // 0 表示不按状态过滤
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeRiskPage(req.Pn, req.Ps)
	// 契约缺口：riskcontrol.v1.ListPunishmentsReq 没有 operator 字段，
	// 处罚查询人只能记在网关日志里。
	reply, err := l.svcCtx.RiskControl.ListPunishments(l.ctx, &riskcontrolrpc.ListPunishmentsReq{
		Mid:        req.Mid,
		Scope:      scope,
		State:      state,
		OnlyActive: req.OnlyActive,
		Pn:         pn,
		Ps:         ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listPunishments: mid=%d scope=%d state=%d only_active=%v pn=%d ps=%d operator_id=%d err=%v",
			req.Mid, req.Scope, req.State, req.OnlyActive, pn, ps, req.OperatorId, err)
		return nil, err
	}
	return &types.RiskPunishmentsResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskPunishmentsData{
			Total:       reply.GetTotal(),
			Pn:          reply.GetPn(),
			Ps:          reply.GetPs(),
			Punishments: riskPunishmentsToAPI(reply.GetPunishments()),
		},
		TTL: 0,
	}, nil
}
