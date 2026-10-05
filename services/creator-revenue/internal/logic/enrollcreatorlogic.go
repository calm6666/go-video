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

type EnrollCreatorLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewEnrollCreatorLogic(ctx context.Context, svcCtx *svc.ServiceContext) *EnrollCreatorLogic {
	return &EnrollCreatorLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 参加计划（必须带已确认的规则版本）
//
// 判定口径：
//   - agreed_rule_version 必填且 >0：没确认过条款的人不能开始产生应计，
//     否则事后无法解释「他当时看到的是哪套单价」，争议只能靠猜；
//   - 已在计划内 → duplicated=true 且不改 enrolled_at（重复调用不会把参加时间推到今天）；
//     只有传入更靠后的规则版本才前移 agreed_rule_version（创作者重新确认新条款），
//     前移是单向的（model 侧 `agreed_rule_version < ?` 守卫），历史确认不会被降级覆盖；
//   - LEFT → ENROLLED 视为重新参加，enrolled_at 刷新、left_at 清零；
//   - SUSPENDED 拒绝自助参加：违规暂停只能由运营用 SetEnrollmentState 解除，
//     否则「暂停」等价于让创作者自己重进一次计划就失效了。
func (l *EnrollCreatorLogic) EnrollCreator(in *rpc.EnrollCreatorReq) (*rpc.EnrollCreatorReply, error) {
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	mid, err := normalizeMid(in.Mid)
	if err != nil {
		return nil, err
	}
	if in.AgreedRuleVersion <= 0 {
		return nil, fmt.Errorf("%w: mid=%d 传入 %d", model.ErrAgreedRuleVersionRequired, mid, in.AgreedRuleVersion)
	}
	operator, err := requireOperator(in.Operator)
	if err != nil {
		return nil, err
	}
	// 参与关系没有变更台账表，request_id 只落审计日志（见 README「幂等与状态机」）。
	requestID, err := requireRequestID(in.RequestId, model.MaxRequestIDBytes)
	if err != nil {
		return nil, err
	}

	cur, err := l.svcCtx.Enrollments.FindOne(l.ctx, mid)
	if err != nil {
		l.Errorf("enroll creator find mid=%d: %v", mid, err)
		return nil, err
	}
	if cur == nil {
		row, err := l.firstEnroll(mid, in.AgreedRuleVersion, operator, requestID)
		if err != nil {
			return nil, err
		}
		if row != nil {
			return &rpc.EnrollCreatorReply{Duplicated: false, Enrollment: enrollmentInfo(row)}, nil
		}
		// 并发首投撞了 uniq_mid：按「已参加」重新读一次，保持幂等结论。
		if cur, err = l.svcCtx.Enrollments.FindOne(l.ctx, mid); err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, fmt.Errorf("%w: mid=%d 建行后仍读不到", model.ErrConcurrentUpdate, mid)
		}
	}

	if cur.State == model.EnrollmentStateSuspended {
		return nil, fmt.Errorf("%w: mid=%d 违规暂停期间不能自助重新参加，请由运营解除暂停",
			model.ErrEnrollmentSuspended, mid)
	}

	var (
		out     *model.Enrollment
		changed bool
	)
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		e, ch, err := applyEnrollmentTransition(ctx, tx, &enrollmentAction{
			Mid:             mid,
			ToState:         model.EnrollmentStateEnrolled,
			AllowFrom:       []int32{model.EnrollmentStateLeft},
			AgreedVersion:   in.AgreedRuleVersion,
			TouchEnrolledAt: true,
			ClearLeftAt:     true,
			Operator:        operator,
			TransitionLabel: "参加分成计划",
		})
		if err != nil {
			return err
		}
		if !ch {
			// 已在计划内：只在「确认了更靠后的规则版本」时前移一次确认记录。
			if in.AgreedRuleVersion <= cur.AgreedRuleVersion {
				out = e
				return nil
			}
			models := model.NewEnrollmentModel(sqlx.NewSqlConnFromSession(tx))
			ok, err := models.UpdateAgreedVersion(ctx, mid, in.AgreedRuleVersion, operator)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: mid=%d 条款版本前移未命中，请重试", model.ErrConcurrentUpdate, mid)
			}
			after, err := models.FindOne(ctx, mid)
			if err != nil {
				return err
			}
			if after == nil {
				return fmt.Errorf("%w: mid=%d 前移后读不到", model.ErrEnrollmentNotFound, mid)
			}
			out = after
			return nil
		}
		out = e
		changed = true
		return nil
	})
	if err != nil {
		l.Errorf("enroll creator mid=%d request_id=%s: %v", mid, requestID, err)
		return nil, err
	}
	// duplicated 表达的是「参加这件事是不是第一次发生」：
	// 仅前移确认版本不算新参加，所以沿用 changed。
	return &rpc.EnrollCreatorReply{Duplicated: !changed, Enrollment: enrollmentInfo(out)}, nil
}

// firstEnroll 建参与关系首行；uniq_mid 冲突时返回 (nil, nil) 交给调用方按「已参加」复核。
func (l *EnrollCreatorLogic) firstEnroll(mid, agreedRuleVersion int64, operator, requestID string) (*model.Enrollment, error) {
	now := model.NowUnix()
	id, err := l.svcCtx.Enrollments.Insert(l.ctx, &model.Enrollment{
		Mid:               mid,
		State:             model.EnrollmentStateEnrolled,
		AgreedRuleVersion: agreedRuleVersion,
		EnrolledAt:        now,
		Operator:          operator,
		Ctime:             now,
		Mtime:             now,
	})
	if err != nil {
		if model.IsDuplicateErr(err) {
			return nil, nil
		}
		l.Errorf("enroll creator insert mid=%d request_id=%s: %v", mid, requestID, err)
		return nil, err
	}
	row, err := l.svcCtx.Enrollments.FindOne(l.ctx, mid)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("%w: 参与关系插入后读不到 enrollment_id=%d", model.ErrConcurrentUpdate, id)
	}
	return row, nil
}
