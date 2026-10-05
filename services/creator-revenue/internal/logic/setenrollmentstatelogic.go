package logic

import (
	"context"
	"fmt"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type SetEnrollmentStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetEnrollmentStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetEnrollmentStateLogic {
	return &SetEnrollmentStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营面：暂停/恢复参与
//
// 判定口径：
//   - 只允许 ENROLLED↔SUSPENDED 两条边；LEFT 是合约关系（走 LeavePlan / EnrollCreator），
//     DRAFT 之类不参与，UNSPECIFIED 更不是可写入的状态；
//   - operator 不能是自助身份 "user"：暂停/恢复是违规处置，
//     让被处置者自己解除处置等于没有处置；
//   - reason 必填并写进 remark（本表没有独立变更台账，见 README「已知缺口」）；
//   - 已在目标状态 → duplicated=true 且零写入：重复暂停不会覆盖第一次的处置原因。
func (l *SetEnrollmentStateLogic) SetEnrollmentState(in *rpc.SetEnrollmentStateReq) (*rpc.SetEnrollmentStateReply, error) {
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	mid, err := normalizeMid(in.Mid)
	if err != nil {
		return nil, err
	}
	target := int32(in.TargetState)
	var allowFrom []int32
	switch target {
	case model.EnrollmentStateSuspended:
		allowFrom = []int32{model.EnrollmentStateEnrolled}
	case model.EnrollmentStateEnrolled:
		allowFrom = []int32{model.EnrollmentStateSuspended}
	default:
		return nil, fmt.Errorf("%w: 本接口只处理 ENROLLED↔SUSPENDED，target_state=%d",
			model.ErrEnrollmentStateTransition, target)
	}
	operator, err := requireOperator(in.Operator)
	if err != nil {
		return nil, err
	}
	if operator == selfOperator {
		return nil, fmt.Errorf("%w: 暂停/恢复是运营处置动作，不接受自助身份 user", model.ErrForbidden)
	}
	reason, err := requireReason(in.Reason)
	if err != nil {
		return nil, err
	}
	if _, err := requireRequestID(in.RequestId, model.MaxRequestIDBytes); err != nil {
		return nil, err
	}

	var (
		out     *model.Enrollment
		changed bool
	)
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		e, ch, err := applyEnrollmentTransition(ctx, tx, &enrollmentAction{
			Mid:             mid,
			ToState:         target,
			AllowFrom:       allowFrom,
			Operator:        operator,
			Remark:          trunc(reason, model.MaxRemarkBytes),
			TransitionLabel: "违规暂停/恢复参与",
		})
		if err != nil {
			return err
		}
		out, changed = e, ch
		return nil
	})
	if err != nil {
		l.Errorf("set enrollment state mid=%d target=%d operator=%s: %v", mid, target, operator, err)
		return nil, err
	}
	return &rpc.SetEnrollmentStateReply{Duplicated: !changed, Enrollment: enrollmentInfo(out)}, nil
}
