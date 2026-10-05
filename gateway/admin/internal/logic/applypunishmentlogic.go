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

type ApplyPunishmentLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 下发处罚（operator_id 与 idempotency_key 必填）
func NewApplyPunishmentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ApplyPunishmentLogic {
	return &ApplyPunishmentLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 下发处罚：聚合 risk-control ApplyPunishment RPC。
// 处罚是人工决策，operator_id 与 idempotency_key 都是硬门槛（服务端 repository 同样校验）；
// 同一 (mid, scope) 已有生效处罚时服务端返回明确错误，网关不做「先解除再下发」的自动编排。
func (l *ApplyPunishmentLogic) ApplyPunishment(req *types.ParamApplyPunishment) (resp *types.RiskApplyPunishmentResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	operatorID, err := adminOperatorID(l.ctx, "applyPunishment", req.OperatorId)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	scope, err := riskGuardedAction(req.Scope, true) // scope=0 表示全域处罚
	if err != nil {
		return nil, err
	}
	decision, err := riskPunitiveDecision(req.Decision, "decision")
	if err != nil {
		return nil, err
	}
	if req.DurationSeconds < 0 {
		return nil, errors.New("gateway/admin: duration_seconds must be >= 0 (0 = permanent)")
	}
	reply, err := l.svcCtx.RiskControl.ApplyPunishment(l.ctx, &riskcontrolrpc.ApplyPunishmentReq{
		Mid:             req.Mid,
		Scope:           scope,
		Decision:        decision,
		Reason:          req.Reason,
		ReasonCode:      req.ReasonCode,
		Operator:        operatorID,
		DurationSeconds: req.DurationSeconds,
		IdempotencyKey:  req.IdempotencyKey,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/applyPunishment: mid=%d scope=%d decision=%d operator_id=%d idempotency_key=%s err=%v",
			req.Mid, req.Scope, req.Decision, operatorID, req.IdempotencyKey, err)
		return nil, err
	}
	return &types.RiskApplyPunishmentResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskApplyPunishmentData{
			Punishment: riskPunishmentToAPI(reply.GetPunishment()),
			Created:    reply.GetCreated(),
		},
		TTL: 0,
	}, nil
}
