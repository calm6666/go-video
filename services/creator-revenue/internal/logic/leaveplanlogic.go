package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type LeavePlanLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLeavePlanLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LeavePlanLogic {
	return &LeavePlanLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 退出计划
//
// 判定口径：
//   - 已 LEFT → duplicated=true 且不刷新 left_at：重复退出不能把「上次退出时间」推后，
//     那会让「退出后还有没有收益」的边界被事后改写；
//   - ENROLLED 与 SUSPENDED 都允许退出（暂停中也可以解约），但从未参加（无行）拒绝：
//     没有参与关系就没有「退出」这回事，硬造一行 LEFT 会让名单里凭空多出一个
//     从没参加过的人；
//   - 运营代操作（operator != "user"）必须给 reason 并落到 remark：
//     代他人解约是要能被追问的动作。
func (l *LeavePlanLogic) LeavePlan(in *rpc.LeavePlanReq) (*rpc.LeavePlanReply, error) {
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	mid, err := normalizeMid(in.Mid)
	if err != nil {
		return nil, err
	}
	operator, err := requireOperator(in.Operator)
	if err != nil {
		return nil, err
	}
	if _, err := requireRequestID(in.RequestId, model.MaxRequestIDBytes); err != nil {
		return nil, err
	}
	remark := ""
	if operator != selfOperator {
		// 只把「没给原因」翻译成人话；原因超长等其它拒绝理由必须原样上抛，
		// 否则调用方明明写了原因却被告知「必须说明原因」，改不动也查不出。
		reason, err := requireReason(in.Reason)
		if err != nil {
			if errors.Is(err, model.ErrReasonRequired) {
				return nil, fmt.Errorf("%w: 运营代操作退出计划必须说明原因", model.ErrReasonRequired)
			}
			return nil, err
		}
		remark = reason
	} else if in.Reason != "" {
		remark = trunc(strings.TrimSpace(in.Reason), model.MaxRemarkBytes)
	}

	var (
		out     *model.Enrollment
		changed bool
	)
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		e, ch, err := applyEnrollmentTransition(ctx, tx, &enrollmentAction{
			Mid:             mid,
			ToState:         model.EnrollmentStateLeft,
			AllowFrom:       []int32{model.EnrollmentStateEnrolled, model.EnrollmentStateSuspended},
			TouchLeftAt:     true,
			Operator:        operator,
			Remark:          remark,
			TransitionLabel: "退出分成计划",
		})
		if err != nil {
			return err
		}
		out, changed = e, ch
		return nil
	})
	if err != nil {
		l.Errorf("leave plan mid=%d operator=%s: %v", mid, operator, err)
		return nil, err
	}
	return &rpc.LeavePlanReply{Duplicated: !changed, Enrollment: enrollmentInfo(out)}, nil
}
