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

type LiftPunishmentLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 解除处罚（幂等，已终态返回当前状态）
func NewLiftPunishmentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiftPunishmentLogic {
	return &LiftPunishmentLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 解除处罚：聚合 risk-control LiftPunishment RPC。
// punishment_id 优先；为 0 时按 (mid, scope) 取最新生效处罚，所以两者至少给一个，
// 避免「谁都能顺手解除一条自己没指定的处罚」。
func (l *LiftPunishmentLogic) LiftPunishment(req *types.ParamLiftPunishment) (resp *types.RiskLiftPunishmentResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	operatorID, err := adminOperatorID(l.ctx, "liftPunishment", req.OperatorId)
	if err != nil {
		return nil, err
	}
	// 契约缺口：riskcontrol.v1.LiftPunishmentReq.idempotency_key 在服务端只用于日志关联，
	// 不做重复解除去重（真正幂等来自处罚状态机）；网关仍要求非空，便于运营重试可追溯。
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if req.PunishmentId <= 0 && req.Mid <= 0 {
		return nil, errors.New("gateway/admin: punishment_id or mid required")
	}
	scope, err := riskGuardedAction(req.Scope, true) // scope=0 表示全域处罚
	if err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RiskControl.LiftPunishment(l.ctx, &riskcontrolrpc.LiftPunishmentReq{
		PunishmentId:   req.PunishmentId,
		Mid:            req.Mid,
		Scope:          scope,
		Operator:       operatorID,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/liftPunishment: punishment_id=%d mid=%d scope=%d operator_id=%d err=%v",
			req.PunishmentId, req.Mid, req.Scope, operatorID, err)
		return nil, err
	}
	return &types.RiskLiftPunishmentResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskLiftPunishmentData{
			Punishment: riskPunishmentToAPI(reply.GetPunishment()),
			Changed:    reply.GetChanged(),
		},
		TTL: 0,
	}, nil
}
