package logic

import (
	"context"
	"fmt"

	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RollbackPoolVersionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRollbackPoolVersionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RollbackPoolVersionLogic {
	return &RollbackPoolVersionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 回滚到历史版本（运营回滚开关）
//
// 与 PublishPoolVersion 共用 switchArgs.run（poolswitch.go）：事务内 EnsureRow + 指针 CAS +
// 版本状态推进 + Outbox.Insert + Idempotency.MarkSucceeded 一次提交。
// 差别只有两处，且都是可追溯性差别而非流程差别：
//  1. 目标状态来源是 RETIRED（回滚候选），不可回滚时报 ErrRollbackTargetInvalid 而不是 ErrVersionNotReady；
//  2. 事件 payload 里 rollback=true，下游（feed/rank）据此区分"新批次上线"与"退回旧批次"。
//
// 被替换下去的版本置 RETIRED 而非删除 —— 否则一次回滚就把前滚的可能性永久删掉了。
// 幂等作用域与上线分开：同一把键不应既能解释成上线又能解释成回滚。
func (l *RollbackPoolVersionLogic) RollbackPoolVersion(in *rpc.RollbackPoolVersionReq) (*rpc.RollbackPoolVersionReply, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: RollbackPoolVersionReq", model.ErrRequestRequired)
	}
	source, poolKey, err := requirePool(in.GetPool())
	if err != nil {
		return nil, err
	}
	if in.GetTargetVersion() <= 0 {
		return nil, fmt.Errorf("%w: target_version=%d", model.ErrInvalidVersion, in.GetTargetVersion())
	}
	operator, err := requiredRef("operator", in.GetOperator(), colRefID, model.ErrOperatorRequired)
	if err != nil {
		return nil, err
	}
	// reason 必填是硬要求：回滚是运营决策，没有原因的切换事后无法追溯是谁为什么退回了哪一批。
	reason, err := requiredRef("reason", in.GetReason(), colNote, model.ErrReasonRequired)
	if err != nil {
		return nil, err
	}
	idemKey, err := requiredRef("idempotency_key", in.GetIdempotencyKey(), colIdempotencyKey,
		model.ErrIdempotencyKeyRequired)
	if err != nil {
		return nil, err
	}

	out, dedup, err := (&switchArgs{
		repo:      l.svcCtx.Repository,
		scope:     model.IdempotencyScopeRollbackPoolVersion,
		source:    source,
		poolKey:   poolKey,
		version:   in.GetTargetVersion(),
		operator:  operator,
		reason:    reason,
		idemKey:   idemKey,
		rollback:  true,
		logger:    l.Logger,
		notSwitch: model.ErrRollbackTargetInvalid,
	}).run(l.ctx)
	if err != nil {
		return nil, err
	}
	return &rpc.RollbackPoolVersionReply{
		Switched:        out.Switched,
		PreviousVersion: out.PreviousVersion,
		CurrentVersion:  out.CurrentVersion,
		Deduplicated:    dedup,
		EventId:         out.EventID,
	}, nil
}
