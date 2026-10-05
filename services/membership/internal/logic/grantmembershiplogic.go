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

type GrantMembershipLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGrantMembershipLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GrantMembershipLogic {
	return &GrantMembershipLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GrantMembership 开通/续期。只由订单履约（trade-order）或运营授权调用。
//
// 这是会员权益唯一的写入通道，也是 CheckEntitlement 敢说自己结论真实的前提：
//  1. 到期时间换算只走 model.NextExpireAt：未过期在原 expire_at 上顺延，
//     已过期从 now 重新起算，绝不从旧 expire_at 往回减；
//  2. 身份行与 mb_grant 台账在同一事务提交，不会出现「加时长无台账」或「有台账未加时长」；
//  3. request_id 唯一索引幂等：命中重放回查首次结果并置 duplicated=true（不再次加时长），
//     同 request_id 换关键参数返回 ErrRequestIdReused；
//  4. 付费来源（沙箱购买/自动续费/迁移）必须能回溯到 biz_order_no 或 payment_no，
//     运营手工与体验发放必须写 reason；
//  5. CAS 版本不匹配返回 ErrConcurrentUpdate，由调用方重读后重试，不覆盖并发写入。
func (l *GrantMembershipLogic) GrantMembership(in *rpc.GrantMembershipReq) (*rpc.GrantMembershipReply, error) {
	cfg := l.svcCtx.Config.Membership
	if err := requireMid(in.Mid); err != nil {
		return nil, err
	}
	if err := requireVipType(in.VipType); err != nil {
		return nil, err
	}
	if err := requireRequestId(in.RequestId); err != nil {
		return nil, err
	}
	if err := requireOperator(in.Operator); err != nil {
		return nil, err
	}
	// REVOKE 走单独接口：GrantMembership 只加长，写负数一定是上游算错了。
	if in.DeltaDays <= 0 {
		return nil, fmt.Errorf("%w: GrantMembership requires positive delta_days, got %d", model.ErrInvalidGrantDelta, in.DeltaDays)
	}
	if err := checkGrantDelta(cfg, in.DeltaDays); err != nil {
		return nil, err
	}

	source := int32(in.Source)
	if !model.ValidGrantSource(source) {
		return nil, model.ErrGrantSourceRequired
	}
	bizOrderNo := strings.TrimSpace(in.BizOrderNo)
	paymentNo := strings.TrimSpace(in.PaymentNo)
	if err := checkLen("biz_order_no", bizOrderNo, model.MaxBizNoLength); err != nil {
		return nil, err
	}
	if err := checkLen("payment_no", paymentNo, model.MaxBizNoLength); err != nil {
		return nil, err
	}
	if model.NeedsPaymentTrace(source) && bizOrderNo == "" && paymentNo == "" {
		// 无凭据的付费开通等于凭空造权益（AGENTS.md §1 资金语义）。
		return nil, model.ErrGrantSourceNeedsOrder
	}
	if source == model.GrantSourceAdminOps || source == model.GrantSourceExperience {
		// 运营手工与体验发放是本服务唯一「无支付流水」的两类授权，必须有理由。
		if err := requireReason(in.Reason); err != nil {
			return nil, err
		}
	} else if err := optionalReason(in.Reason); err != nil {
		return nil, err
	}

	vipType := int32(in.VipType)
	operator := strings.TrimSpace(in.Operator)
	now := model.NowUnix()

	if in.PlanId > 0 {
		// 挂套餐的授予必须与套餐档位一致：买大会员的订单不能开出超级大会员。
		plan, err := l.svcCtx.Plan.FindOne(l.ctx, in.PlanId)
		if err != nil {
			l.Errorf("membership/GrantMembership: read plan_id=%d err=%v", in.PlanId, err)
			return nil, err
		}
		if plan == nil {
			return nil, model.ErrPlanNotFound
		}
		if plan.VipType != vipType {
			return nil, fmt.Errorf("%w: plan %d is tier %d but granting tier %d",
				model.ErrInvalidVipType, plan.PlanID, plan.VipType, vipType)
		}
		if plan.State == model.PlanStateDraft {
			// DRAFT 不可下单，履约也就无从发生（proto：DRAFT 不对外可见也不可下单）。
			return nil, fmt.Errorf("%w: plan %d is still DRAFT", model.ErrInvalidPlanStateTransition, plan.PlanID)
		}
	}

	// 幂等快路径：同一 request_id 已经记过账，就绝不第二次加时长。
	first, err := l.svcCtx.Grant.FindByRequestID(l.ctx, in.RequestId)
	if err != nil {
		l.Errorf("membership/GrantMembership: idempotency lookup request_id=%s err=%v", in.RequestId, err)
		return nil, err
	}
	if first != nil {
		return l.replay(first, in)
	}

	var (
		applied *model.Membership
		grantID int64
		action  string
	)
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		cur, err := l.svcCtx.Membership.FindOneTx(ctx, session, in.Mid, vipType)
		if err != nil {
			return err
		}

		deltaSeconds := int64(in.DeltaDays) * model.SecondsPerDay
		row := cur
		action = model.ActionGrant
		var before int64
		if row == nil {
			row = &model.Membership{
				Mid:      in.Mid,
				VipType:  vipType,
				StartAt:  now,
				ExpireAt: model.NextExpireAt(0, deltaSeconds, now),
			}
		} else {
			action = model.ActionExtend
			before = row.ExpireAt
			row.ExpireAt = model.NextExpireAt(row.ExpireAt, deltaSeconds, now)
		}
		row.Source = source
		if model.IsPaidSource(source) {
			// 只有付费来源累加付费月数；运营赠送/体验不计入（它是「付了多久」的快照）。
			row.PaidMonthCount += model.MonthsForDeltaDays(in.DeltaDays)
		}

		if cur == nil {
			if _, err := l.svcCtx.Membership.InsertTx(ctx, session, row); err != nil {
				return err
			}
		} else {
			ok, err := l.svcCtx.Membership.UpdateTx(ctx, session, row)
			if err != nil {
				return err
			}
			if !ok {
				return model.ErrConcurrentUpdate
			}
		}

		g := &model.Grant{
			Mid:            in.Mid,
			VipType:        vipType,
			Action:         action,
			DeltaDays:      in.DeltaDays,
			PlanID:         in.PlanId,
			Source:         source,
			BizOrderNo:     bizOrderNo,
			PaymentNo:      paymentNo,
			BeforeExpireAt: before,
			AfterExpireAt:  row.ExpireAt,
			Operator:       operator,
			RequestID:      in.RequestId,
			Reason:         in.Reason,
			Ctime:          now,
		}
		id, err := l.svcCtx.Grant.InsertTx(ctx, session, g)
		if err != nil {
			return err
		}
		grantID = id
		applied = row
		return nil
	})
	if err != nil {
		if l.svcCtx.Grant.IsDuplicate(err) {
			// 两个唯一索引都可能撞：request_id（重放）或 (mid,vip_type)（并发首次开通）。
			rival, ferr := l.svcCtx.Grant.FindByRequestID(l.ctx, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			if rival != nil {
				return l.replay(rival, in)
			}
			l.Infof("membership/GrantMembership: identity created concurrently mid=%d vip_type=%d request_id=%s",
				in.Mid, vipType, in.RequestId)
			return nil, model.ErrConcurrentUpdate
		}
		l.Errorf("membership/GrantMembership: apply failed mid=%d vip_type=%d request_id=%s err=%v",
			in.Mid, vipType, in.RequestId, err)
		return nil, err
	}

	l.Infof("membership/GrantMembership: granted mid=%d vip_type=%d action=%s delta_days=%d grant_id=%d source=%d",
		in.Mid, vipType, action, in.DeltaDays, grantID, source)
	return &rpc.GrantMembershipReply{
		Duplicated: false,
		Membership: membershipToRPC(applied),
		GrantId:    grantID,
	}, nil
}

// replay 命中同一 request_id：先比对关键参数，一致才把首次结果还给调用方。
func (l *GrantMembershipLogic) replay(first *model.Grant, in *rpc.GrantMembershipReq) (*rpc.GrantMembershipReply, error) {
	if !grantMatchesRequest(first, in.Mid, int32(in.VipType), first.Action, in.DeltaDays,
		in.PlanId, int32(in.Source), in.BizOrderNo, in.PaymentNo) {
		l.Errorf("membership/GrantMembership: request_id=%s reused with different parameters (granted mid=%d vip_type=%d delta=%d)",
			in.RequestId, first.Mid, first.VipType, first.DeltaDays)
		return nil, model.ErrRequestIdReused
	}
	row, err := l.svcCtx.Membership.FindOne(l.ctx, first.Mid, first.VipType)
	if err != nil {
		l.Errorf("membership/GrantMembership: replay reread mid=%d vip_type=%d err=%v", first.Mid, first.VipType, err)
		return nil, err
	}
	if row == nil {
		// 台账在、身份行没了：数据被人工删过，此时返回伪成功会掩盖事故。
		l.Errorf("membership/GrantMembership: grant_id=%d has no membership row, mid=%d vip_type=%d",
			first.GrantID, first.Mid, first.VipType)
		return nil, model.ErrMembershipNotFound
	}
	l.Infof("membership/GrantMembership: replay request_id=%s mid=%d grant_id=%d, duration not applied twice",
		in.RequestId, in.Mid, first.GrantID)
	return &rpc.GrantMembershipReply{
		Duplicated: true,
		Membership: membershipToRPC(row),
		GrantId:    first.GrantID,
	}, nil
}
