package logic

import (
	"context"

	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiftPunishmentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLiftPunishmentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiftPunishmentLogic {
	return &LiftPunishmentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 解除处罚（幂等）。
// 定位方式二选一：punishment_id 优先；为 0 时按 (mid, scope) 精确匹配当前生效处罚，
// 命中多条会返回 ErrAmbiguousPunishment —— 解除绝不能靠「猜一条」，
// 否则运营以为解除了动作级处罚、实际解除了全域封禁是灾难性后果。
// 已是终态（已解除/已过期）时 changed=false 并回传当前记录，不报错。
func (l *LiftPunishmentLogic) LiftPunishment(in *rpc.LiftPunishmentReq) (*rpc.LiftPunishmentReply, error) {
	scope := int32(in.GetScope())
	if !model.ValidRuleAction(scope) {
		return nil, model.ErrInvalidTarget
	}
	if len(in.GetReason()) > maxReasonLen {
		return nil, model.ErrInvalidTarget
	}

	punishment, changed, err := l.svcCtx.Repository.LiftPunishment(l.ctx,
		in.GetPunishmentId(), in.GetMid(), scope, in.GetOperator(), in.GetReason())
	if err != nil {
		l.Errorf("risk-control/LiftPunishment: rejected punishment_id=%d mid=%d scope=%d operator=%d err=%v",
			in.GetPunishmentId(), in.GetMid(), scope, in.GetOperator(), err)
		return nil, err
	}
	if changed {
		l.Infof("risk-control/LiftPunishment: lifted punishment=%d mid=%d scope=%d operator=%d idempotency_key=%s",
			punishment.PunishmentID, punishment.Mid, punishment.Scope, in.GetOperator(),
			model.TruncateHash(sanitizeShortString(in.GetIdempotencyKey(), 64), 16))
	}
	return &rpc.LiftPunishmentReply{Punishment: punishmentToProto(punishment), Changed: changed}, nil
}
