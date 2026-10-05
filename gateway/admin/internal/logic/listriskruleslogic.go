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

type ListRiskRulesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询风控规则
func NewListRiskRulesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRiskRulesLogic {
	return &ListRiskRulesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 规则列表：聚合 risk-control ListRules RPC。
// 三个过滤条件都留 0/-1 表示不过滤（与服务端语义一致），但未知枚举值先被网关挡掉；
// 分页口径与服务端一致（ps 上限 50）。规则命中要用规则 version 解释，
// 所以投影保留 version，不做字段裁剪。
func (l *ListRiskRulesLogic) ListRiskRules(req *types.ParamListRiskRules) (resp *types.RiskRulesResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	if err := requireOperatorID(req.OperatorId); err != nil {
		return nil, err
	}
	actionType, err := riskGuardedAction(req.ActionType, true) // 0 表示不按动作过滤
	if err != nil {
		return nil, err
	}
	metric, err := riskMetricFilter(req.Metric) // 0 表示不按指标过滤
	if err != nil {
		return nil, err
	}
	if err := riskStateFilter("state", req.State); err != nil {
		return nil, err
	}
	pn, ps := normalizeRiskPage(req.Pn, req.Ps)
	// 契约缺口：riskcontrol.v1.ListRulesReq 没有 operator 字段，
	// 谁在查规则只能留在网关日志里，风控侧审计看不到读取人。
	reply, err := l.svcCtx.RiskControl.ListRules(l.ctx, &riskcontrolrpc.ListRulesReq{
		ActionType: actionType,
		Metric:     metric,
		State:      req.State,
		Pn:         pn,
		Ps:         ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listRiskRules: action_type=%d metric=%d state=%d pn=%d ps=%d operator_id=%d err=%v",
			req.ActionType, req.Metric, req.State, pn, ps, req.OperatorId, err)
		return nil, err
	}
	return &types.RiskRulesResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskRulesData{
			Total: reply.GetTotal(),
			Pn:    reply.GetPn(),
			Ps:    reply.GetPs(),
			Rules: riskRulesToAPI(reply.GetRules()),
		},
		TTL: 0,
	}, nil
}
