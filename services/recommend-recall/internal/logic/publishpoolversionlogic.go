package logic

import (
	"context"
	"fmt"

	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PublishPoolVersionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPublishPoolVersionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PublishPoolVersionLogic {
	return &PublishPoolVersionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 原子切换池的当前生效版本（写审计 + 发事件）
//
// 本方法只做入参校验与结果投影，切换本身在 poolswitch.go 的 switchArgs.run：
// 上线与回滚是同一个动作（把 recall_pool_current 从 X 移到 Y），实现必须共用一份，
// 否则其中一侧会漏掉 CAS 或漏掉 Outbox（见 poolswitch.go 头注释）。
//
// 不变量落点：
//   - 已发布版本不可变 —— 本路径只改指针与版本 state，绝不写 recall_pool 条目；
//   - CAS —— ErrSwitchConflict 整事务回滚并原样返回，绝不"冲突就无条件覆盖"；
//   - 幂等 —— 同键重放回放原结果并置 deduplicated=true，不产生第二次切换、不产生第二个事件。
func (l *PublishPoolVersionLogic) PublishPoolVersion(in *rpc.PublishPoolVersionReq) (*rpc.PublishPoolVersionReply, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: PublishPoolVersionReq", model.ErrRequestRequired)
	}
	source, poolKey, err := requirePool(in.GetPool())
	if err != nil {
		return nil, err
	}
	if in.GetVersion() <= 0 {
		return nil, fmt.Errorf("%w: version=%d", model.ErrInvalidVersion, in.GetVersion())
	}
	operator, err := requiredRef("operator", in.GetOperator(), colRefID, model.ErrOperatorRequired)
	if err != nil {
		return nil, err
	}
	// reason 进 recall_pool_current.note / recall_pool_version.note（VARCHAR(255)）：
	// 超长一律拒绝，截断后的切换原因在审计里是一条读起来通顺但事实错了一半的记录。
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
		repo:     l.svcCtx.Repository,
		scope:    model.IdempotencyScopePublishPoolVersion,
		source:   source,
		poolKey:  poolKey,
		version:  in.GetVersion(),
		operator: operator,
		reason:   reason,
		idemKey:  idemKey,
		logger:   l.Logger,
		// 上线只接受 READY（新批次正常上线）；BUILDING/FAILED -> ErrVersionNotReady。
		// CURRENT 走幂等无操作分支，RETIRED 被允许是为了"重新前滚到旧批次"。
		notSwitch: model.ErrVersionNotReady,
	}).run(l.ctx)
	if err != nil {
		return nil, err
	}
	return &rpc.PublishPoolVersionReply{
		Switched:        out.Switched,
		PreviousVersion: out.PreviousVersion,
		CurrentVersion:  out.CurrentVersion,
		Deduplicated:    dedup,
		EventId:         out.EventID,
	}, nil
}
