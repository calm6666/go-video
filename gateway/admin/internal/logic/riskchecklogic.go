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

type RiskCheckLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 调试用同步裁决一次受保护动作（返回可解释裁决与命中明细）
func NewRiskCheckLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RiskCheckLogic {
	return &RiskCheckLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 裁决调试：聚合 risk-control CheckAction RPC，供运营复核规则命中过程。
// 网关不做任何风险判定，也不改写裁决；依赖故障时服务端返回带 degraded 的裁决而不是错误，
// 这里原样透出。日志只记脱敏维度（mid/action/request_id），不打印 device_id 与 ip_hash。
func (l *RiskCheckLogic) RiskCheck(req *types.ParamRiskCheck) (resp *types.RiskCheckResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	action, err := riskGuardedAction(req.Action, false)
	if err != nil {
		return nil, err
	}
	// 契约缺口：riskcontrol.v1.CheckActionReq 面向终端动作，没有 operator 字段，
	// 后台调试人无法进入 risk_check_log，只能记在网关日志里做二次关联。
	operatorID, err := adminOperatorID(l.ctx, "riskCheck", req.OperatorId)
	if err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RiskControl.CheckAction(l.ctx, &riskcontrolrpc.CheckActionReq{
		Mid:            req.Mid,
		Action:         action,
		DeviceId:       req.DeviceId,
		IpHash:         req.IpHash,
		Platform:       req.Platform,
		AppVersion:     req.AppVersion,
		RequestContext: req.RequestContext,
		RequestId:      req.RequestId,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/riskCheck: mid=%d action=%d request_id=%s operator_id=%d err=%v",
			req.Mid, req.Action, req.RequestId, operatorID, err)
		return nil, err
	}
	return &types.RiskCheckResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskCheckData{
			RequestId:           reply.GetRequestId(),
			Decision:            int32(reply.GetDecision()),
			Score:               reply.GetScore(),
			HitRuleIds:          riskInt64Ids(reply.GetHitRuleIds()),
			RuleHits:            riskRuleHitsToAPI(reply.GetRuleHits()),
			HasPunishment:       reply.GetPunishment() != nil,
			Punishment:          riskPunishmentSnapshotToAPI(reply.GetPunishment()),
			ActionCode:          reply.GetActionCode(),
			ChallengeTtlSeconds: reply.GetChallengeTtlSeconds(),
			Basis:               reply.GetBasis(),
			SkippedRuleIds:      riskInt64Ids(reply.GetSkippedRuleIds()),
			Evaluated:           reply.GetEvaluated(),
			Degraded:            reply.GetDegraded(),
		},
		TTL: 0,
	}, nil
}
