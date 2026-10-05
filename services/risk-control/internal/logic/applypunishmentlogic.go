package logic

import (
	"context"
	"fmt"
	"time"

	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ApplyPunishmentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewApplyPunishmentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ApplyPunishmentLogic {
	return &ApplyPunishmentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// maxReasonLen 与 risk_punishment.reason 列宽对齐，超长直接拒绝而不是截断，
// 因为被截断的处罚说明会让后续申诉无法复核。
const maxReasonLen = 255

// maxIdempotencyKeyLen 与 risk_punishment.idempotency_key 列宽（VARCHAR(64)）对齐。
// 幂等键上有唯一索引，静默裁到列宽会让两次不同的处罚共用同一键，
// 后一次被误判成「重试」而丢弃，因此只能拒绝。
const maxIdempotencyKeyLen = 64

// 运营/审核下发处罚。
// 约束（AGENTS.md §5 数据所有权 + §8 状态机）：
//   - operator 必填且 >0：处罚是人工决策，必须可追溯到具体运营；
//   - idempotency_key 必填：运营重试不得产生第二条处罚；
//   - 同一 (mid, scope) 已有生效处罚时返回 ErrPunishmentAlreadyActive，
//     需要先 LiftPunishment 再下发，避免裁决解释出现两条冲突处罚；
//   - decision 只能是 CHALLENGE/BLOCK/REVIEW，ALLOW 不构成处罚。
func (l *ApplyPunishmentLogic) ApplyPunishment(in *rpc.ApplyPunishmentReq) (*rpc.ApplyPunishmentReply, error) {
	scope := int32(in.GetScope())
	if !model.ValidRuleAction(scope) {
		return nil, fmt.Errorf("%w: scope=%d", model.ErrInvalidTarget, scope)
	}
	decision := int32(in.GetDecision())
	if decision != model.DecisionBlock && decision != model.DecisionChallenge && decision != model.DecisionReview {
		return nil, fmt.Errorf("%w: punishment decision must be CHALLENGE/BLOCK/REVIEW", model.ErrInvalidTarget)
	}
	if len(in.GetReason()) > maxReasonLen {
		return nil, fmt.Errorf("%w: reason too long", model.ErrInvalidTarget)
	}
	if in.GetDurationSeconds() < 0 {
		return nil, fmt.Errorf("%w: duration_seconds must be >= 0 (0 = permanent)", model.ErrInvalidTarget)
	}
	// 先规范化（去空白与控制符）再判长度：唯一键上的截断会把两次不同处罚并成一次。
	idempotencyKey := sanitizeShortString(in.GetIdempotencyKey(), 0)
	if len(idempotencyKey) > maxIdempotencyKeyLen {
		return nil, fmt.Errorf("%w: idempotency_key longer than %d", model.ErrInvalidTarget, maxIdempotencyKeyLen)
	}

	now := time.Now().Unix()
	startAt := now
	endAt := int64(0)
	if d := in.GetDurationSeconds(); d > 0 {
		endAt = startAt + d
	}

	punishment, created, err := l.svcCtx.Repository.ApplyPunishment(l.ctx, &model.RiskPunishment{
		Mid:            in.GetMid(),
		Scope:          scope,
		Decision:       decision,
		Reason:         in.GetReason(),
		ReasonCode:     sanitizeShortString(in.GetReasonCode(), 64),
		Operator:       in.GetOperator(),
		StartAt:        startAt,
		EndAt:          endAt,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		// 幂等命中（同 key 重试）与「已存在生效处罚」是运营侧需要区分两种处置的错误，
		// 原样返回哨兵错误，由上层按 code 渲染，这里只留审计日志。
		l.Errorf("risk-control/ApplyPunishment: rejected mid=%d scope=%d operator=%d err=%v",
			in.GetMid(), scope, in.GetOperator(), err)
		return nil, err
	}
	if created {
		l.Infof("risk-control/ApplyPunishment: applied punishment=%d mid=%d scope=%d decision=%d permanent=%t",
			punishment.PunishmentID, punishment.Mid, punishment.Scope, punishment.Decision, punishment.EndAt == 0)
	}
	return &rpc.ApplyPunishmentReply{Punishment: punishmentToProto(punishment), Created: created}, nil
}
