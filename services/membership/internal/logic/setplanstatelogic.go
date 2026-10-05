package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type SetPlanStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetPlanStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetPlanStateLogic {
	return &SetPlanStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// SetPlanState 运营面上下架。
//
// 状态机（AGENTS.md §8：回调/写接口只能推进合法状态）：
//
//	DRAFT → ON_SALE
//	ON_SALE → OFF_SALE
//	OFF_SALE → ON_SALE
//
// 其余一律 ErrInvalidPlanStateTransition，包括「同状态再上一次」（幂等重试由 request_id
// 重放路径负责，不靠放松状态机来兜）。改价不走这里：必须新建 DRAFT 版本再切换，
// 否则改完的价格会对已下单用户追溯生效。
// reason 必填并进变更台账；expected_version 不匹配返回 ErrConcurrentUpdate。
func (l *SetPlanStateLogic) SetPlanState(in *rpc.SetPlanStateReq) (*rpc.PlanInfo, error) {
	if in.PlanId <= 0 {
		return nil, model.ErrPlanIdentifierRequired
	}
	if err := requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if err := requireRequestId(in.RequestId); err != nil {
		return nil, err
	}
	// 上下架是有后果的动作（终端立刻可见/立刻不可见），无理由不受理。
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}

	target := int32(in.TargetState)
	if !model.ValidPlanState(target) {
		return nil, model.ErrInvalidPlanState
	}

	fp := model.Fingerprint("SetPlanState", fmt.Sprintf("%d", in.PlanId), fmt.Sprintf("%d", target), fmt.Sprintf("%d", in.ExpectedVersion))

	// 幂等重放优先于状态机校验：重试请求的「当前状态」已经是目标态，
	// 若先校验迁移会把它误判成非法转移（ON_SALE→ON_SALE）。
	cl, err := l.svcCtx.PlanLog.FindByRequestID(l.ctx, in.RequestId)
	if err != nil {
		l.Errorf("membership/SetPlanState: idempotency lookup request_id=%s err=%v", in.RequestId, err)
		return nil, err
	}
	if cl != nil {
		return l.replayResult(cl, fp, in)
	}

	plan, err := l.svcCtx.Plan.FindOne(l.ctx, in.PlanId)
	if err != nil {
		l.Errorf("membership/SetPlanState: read plan_id=%d err=%v", in.PlanId, err)
		return nil, err
	}
	if plan == nil {
		return nil, model.ErrPlanNotFound
	}
	if !canMovePlanState(plan.State, target) {
		l.Errorf("membership/SetPlanState: illegal transition plan_id=%d from=%d to=%d operator=%s",
			plan.PlanID, plan.State, target, in.Operator)
		return nil, fmt.Errorf("%w: %d -> %d", model.ErrInvalidPlanStateTransition, plan.State, target)
	}
	if in.ExpectedVersion <= 0 {
		return nil, fmt.Errorf("%w: expected_version=%d", model.ErrConcurrentUpdate, in.ExpectedVersion)
	}

	operator := strings.TrimSpace(in.Operator)
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		ok, err := l.svcCtx.Plan.CASStateTx(ctx, session, plan.PlanID, plan.State, target, in.ExpectedVersion, operator)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		_, err = l.svcCtx.PlanLog.InsertTx(ctx, session, &model.PlanChangeLog{
			PlanID:             plan.PlanID,
			ChangeType:         model.PlanChangeState,
			FromState:          plan.State,
			ToState:            target,
			FromPriceMinor:     plan.PriceMinor,
			ToPriceMinor:       plan.PriceMinor,
			FromPromPriceMinor: plan.PromPriceMinor,
			ToPromPriceMinor:   plan.PromPriceMinor,
			Operator:           operator,
			Reason:             in.Reason,
			RequestID:          in.RequestId,
			ParamsFingerprint:  fp,
		})
		return err
	})
	if err != nil {
		if l.svcCtx.PlanLog.IsDuplicate(err) {
			// 并发同 request_id：整体回滚后回查首次结果，绝不留下「改了状态但没台账」。
			cl, findErr := l.svcCtx.PlanLog.FindByRequestID(l.ctx, in.RequestId)
			if findErr != nil {
				return nil, findErr
			}
			if cl == nil {
				l.Errorf("membership/SetPlanState: duplicate without log row request_id=%s err=%v", in.RequestId, err)
				return nil, err
			}
			return l.replayResult(cl, fp, in)
		}
		l.Errorf("membership/SetPlanState: apply failed plan_id=%d request_id=%s err=%v", in.PlanId, in.RequestId, err)
		return nil, err
	}

	fresh, err := l.svcCtx.Plan.FindOne(l.ctx, plan.PlanID)
	if err != nil {
		l.Errorf("membership/SetPlanState: reread plan_id=%d err=%v", plan.PlanID, err)
		return nil, err
	}
	return planToRPC(fresh), nil
}

// replayResult 回查首次结果：指纹不一致必须报冲突，一致才把当时那次的结果还给调用方。
func (l *SetPlanStateLogic) replayResult(cl *model.PlanChangeLog, fp string, in *rpc.SetPlanStateReq) (*rpc.PlanInfo, error) {
	if cl.ParamsFingerprint != fp {
		l.Errorf("membership/SetPlanState: request_id=%s reused with different parameters", in.RequestId)
		return nil, model.ErrRequestIdReused
	}
	p, err := l.svcCtx.Plan.FindOne(l.ctx, cl.PlanID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, model.ErrPlanNotFound
	}
	return planToRPC(p), nil
}

// canMovePlanState 上下架状态机：只放开三条边，DRAFT 不可回退（草稿就是草稿）。
func canMovePlanState(from, to int32) bool {
	switch from {
	case model.PlanStateDraft:
		return to == model.PlanStateOnSale
	case model.PlanStateOnSale:
		return to == model.PlanStateOffSale
	case model.PlanStateOffSale:
		return to == model.PlanStateOnSale
	default:
		return false
	}
}
