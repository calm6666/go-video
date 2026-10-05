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

type UpsertRiskRuleLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新增/更新风控规则（版本递增，operator_id 必填）
func NewUpsertRiskRuleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertRiskRuleLogic {
	return &UpsertRiskRuleLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 规则变更：聚合 risk-control UpsertRule RPC。
// 规则名唯一性、指标是否已实现、窗口取值等规则由 risk-control 校验，网关不重复实现；
// 网关只挡住未知枚举与缺主体/缺幂等键的写请求（AGENTS.md §5 规则数据归 risk-control）。
func (l *UpsertRiskRuleLogic) UpsertRiskRule(req *types.ParamUpsertRiskRule) (resp *types.RiskUpsertRuleResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	operatorID, err := adminOperatorID(l.ctx, "upsertRiskRule", req.OperatorId)
	if err != nil {
		return nil, err
	}
	// 契约缺口：riskcontrol.v1.UpsertRuleReq.idempotency_key 本期只做日志关联，
	// 服务端不据此跳过版本递增（见 upsertrulelogic 注释）；网关要求非空是为了先留住运营重试批次。
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.RuleId == 0 {
		if err := requireNonEmpty("name", req.Name); err != nil {
			return nil, err
		}
	}
	actionType, err := riskGuardedAction(req.ActionType, true) // 0 表示适用全部动作
	if err != nil {
		return nil, err
	}
	metric, err := riskMetric(req.Metric)
	if err != nil {
		return nil, err
	}
	op, err := riskCompareOp(req.Op)
	if err != nil {
		return nil, err
	}
	decision, err := riskPunitiveDecision(req.Decision, "decision")
	if err != nil {
		return nil, err
	}
	if req.WindowSeconds <= 0 {
		return nil, errors.New("gateway/admin: window_seconds must be > 0")
	}
	if req.State != 0 && req.State != 1 {
		return nil, errors.New("gateway/admin: state must be 0(禁用)/1(启用)")
	}
	reply, err := l.svcCtx.RiskControl.UpsertRule(l.ctx, &riskcontrolrpc.UpsertRuleReq{
		RuleId:         req.RuleId,
		Name:           req.Name,
		ActionType:     actionType,
		Metric:         metric,
		Op:             op,
		Threshold:      req.Threshold,
		WindowSeconds:  req.WindowSeconds,
		Decision:       decision,
		Priority:       req.Priority,
		State:          req.State,
		Operator:       operatorID,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/upsertRiskRule: rule_id=%d name=%s metric=%d op=%d operator_id=%d err=%v",
			req.RuleId, req.Name, req.Metric, req.Op, operatorID, err)
		return nil, err
	}
	return &types.RiskUpsertRuleResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskUpsertRuleData{
			Rule:    riskRuleToAPI(reply.GetRule()),
			Created: reply.GetCreated(),
		},
		TTL: 0,
	}, nil
}
