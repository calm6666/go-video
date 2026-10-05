package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RevokeMembershipLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRevokeMembershipLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevokeMembershipLogic {
	return &RevokeMembershipLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RevokeMembership 收回（退款回收/运营纠错）。
//
// 口径：
//  1. reason 必填：收回是有后果的动作，无理由不受理（proto 注释同义）；
//  2. clear_remaining 与 delta_days 二选一：true 表示立即失效（此时 delta_days 必须为 0，
//     否则两个字段会互相矛盾），false 表示只扣回 delta_days 天；
//  3. 扣减下限是 now：扣过头一律停在 now，不制造「早于今天的负余额」，
//     那会让后续 NextExpireAt 从错误的过去时刻起算，等于白送时长；
//  4. 已经过期的身份：动作合法但无时长可扣，照实记一条 before==after 的 REVOKE 台账
//     并原样返回身份行——退款流程因此不会被一个「收回失败」的错误卡住；
//  5. paid_month_count 不回退：它是「历史上真实付过多少个月」的单调快照，
//     回收权益不该篡改付费事实，风控与统计都依赖它的单调性；
//  6. 收回后若已无有效时长，auto_renew 签约位一并清零：
//     「已收回却被自动续费重新延长」是自相矛盾的状态；
//  7. 身份行与台账同一事务提交，request_id 唯一索引做幂等，重放不再扣一次；
//  8. plan_id / biz_order_no / payment_no 是追溯位，全空即视为运营手工收回；
//     带单号的台账才可能被退款流程对账——source 列只有 ADMIN_OPS 一个值，
//     「退款回收」与「运营纠错」的区分靠这三个引用，不能丢。
func (l *RevokeMembershipLogic) RevokeMembership(in *rpc.RevokeMembershipReq) (*rpc.RevokeMembershipReply, error) {
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
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}
	bizOrderNo := strings.TrimSpace(in.BizOrderNo)
	paymentNo := strings.TrimSpace(in.PaymentNo)
	if err := checkLen("biz_order_no", bizOrderNo, model.MaxBizNoLength); err != nil {
		return nil, err
	}
	if err := checkLen("payment_no", paymentNo, model.MaxBizNoLength); err != nil {
		return nil, err
	}
	planID := revokePlanID(in.PlanId)

	var ledgerDelta int32 // 台账记「请求意图」的带符号天数；真实影响看 before/after_expire_at。
	if in.ClearRemaining {
		if in.DeltaDays != 0 {
			return nil, fmt.Errorf("%w: clear_remaining=true must carry delta_days=0, got %d",
				model.ErrInvalidGrantDelta, in.DeltaDays)
		}
	} else {
		if in.DeltaDays <= 0 {
			return nil, fmt.Errorf("%w: RevokeMembership requires positive delta_days or clear_remaining=true, got %d",
				model.ErrInvalidGrantDelta, in.DeltaDays)
		}
		if err := checkGrantDelta(cfg, in.DeltaDays); err != nil {
			return nil, err
		}
		ledgerDelta = -in.DeltaDays
	}

	vipType := int32(in.VipType)
	operator := strings.TrimSpace(in.Operator)
	now := model.NowUnix()

	// 幂等快路径：同一 request_id 已经扣过，就绝不扣第二次。
	first, err := l.svcCtx.Grant.FindByRequestID(l.ctx, in.RequestId)
	if err != nil {
		l.Errorf("membership/RevokeMembership: idempotency lookup request_id=%s err=%v", in.RequestId, err)
		return nil, err
	}
	if first != nil {
		return l.replay(first, in, ledgerDelta)
	}

	var (
		applied *model.Membership
		grantID int64
		before  int64
		after   int64
	)
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		cur, err := l.svcCtx.Membership.FindOneTx(ctx, session, in.Mid, vipType)
		if err != nil {
			return err
		}
		if cur == nil {
			// 没开通过就无所谓收回：这与「收回结果为 0 天」是两回事，必须报错。
			return model.ErrMembershipNotFound
		}
		row := *cur
		before = row.ExpireAt
		after = revokeResult(before, in.ClearRemaining, in.DeltaDays, now)
		row.ExpireAt = after
		row.Source = model.GrantSourceAdminOps
		if !row.IsActive(now) {
			row.AutoRenew = 0
			row.AutoRenewChannel = ""
			row.AutoRenewSignedAt = now // 解约时间：与签约同一列，语义按 proto 是「最近一次变更」
		}
		ok, err := l.svcCtx.Membership.UpdateTx(ctx, session, &row)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}

		g := &model.Grant{
			Mid:            in.Mid,
			VipType:        vipType,
			Action:         model.ActionRevoke,
			DeltaDays:      ledgerDelta,
			Source:         model.GrantSourceAdminOps,
			PlanID:         planID,
			BizOrderNo:     bizOrderNo,
			PaymentNo:      paymentNo,
			BeforeExpireAt: before,
			AfterExpireAt:  after,
			Operator:       operator,
			RequestID:      in.RequestId,
			Reason:         strings.TrimSpace(in.Reason),
			Ctime:          now,
		}
		id, err := l.svcCtx.Grant.InsertTx(ctx, session, g)
		if err != nil {
			return err
		}
		grantID = id
		applied = &row
		return nil
	})
	if err != nil {
		if l.svcCtx.Grant.IsDuplicate(err) {
			// 只有 request_id 可能撞：身份行是 UPDATE 而非 INSERT。
			rival, ferr := l.svcCtx.Grant.FindByRequestID(l.ctx, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			if rival != nil {
				return l.replay(rival, in, ledgerDelta)
			}
			return nil, err
		}
		if errors.Is(err, model.ErrMembershipNotFound) {
			l.Infof("membership/RevokeMembership: no identity row mid=%d vip_type=%d request_id=%s", in.Mid, vipType, in.RequestId)
			return nil, err
		}
		l.Errorf("membership/RevokeMembership: apply failed mid=%d vip_type=%d request_id=%s err=%v",
			in.Mid, vipType, in.RequestId, err)
		return nil, err
	}

	if after == before {
		l.Infof("membership/RevokeMembership: nothing to deduct mid=%d vip_type=%d expire_at=%d grant_id=%d",
			in.Mid, vipType, after, grantID)
	} else {
		l.Infof("membership/RevokeMembership: revoked mid=%d vip_type=%d expire_at %d -> %d removed_days=%d grant_id=%d",
			in.Mid, vipType, before, after, -daysRemoved(before-after), grantID)
	}
	return &rpc.RevokeMembershipReply{
		Duplicated: false,
		Membership: membershipToRPC(applied),
		GrantId:    grantID,
	}, nil
}

// revokeResult 计算收回后的到期时间。
//
// 已过期（before <= now）保持原值：不推进也不回拉一个已过期的时间戳，
// 否则 mb_membership.expire_at 会被改写成一个「更晚」的值而无台账依据。
func revokeResult(before int64, clearRemaining bool, deltaDays int32, now int64) int64 {
	if before <= now {
		return before
	}
	if clearRemaining {
		return now
	}
	after := before - int64(deltaDays)*model.SecondsPerDay
	if after < now {
		// 扣过头停在 now：余额不足时扣到刚好用完，而不是造一个过去的负余额。
		after = now
	}
	return after
}

// revokePlanID 归一化追溯位：plan_id 与 GrantMembership 一样只在 >0 时有意义，
// 负数按「未指定」落账，不把非法值写进 mb_grant.plan_id 让对账脚本自己去猜。
func revokePlanID(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// replay 命中同一 request_id：先比对关键参数，一致才把首次结果还给调用方。
func (l *RevokeMembershipLogic) replay(first *model.Grant, in *rpc.RevokeMembershipReq, ledgerDelta int32) (*rpc.RevokeMembershipReply, error) {
	if !grantMatchesRequest(first, in.Mid, int32(in.VipType), model.ActionRevoke, ledgerDelta,
		revokePlanID(in.PlanId), model.GrantSourceAdminOps, in.BizOrderNo, in.PaymentNo) {
		l.Errorf("membership/RevokeMembership: request_id=%s reused with different parameters (revoked mid=%d vip_type=%d delta=%d)",
			in.RequestId, first.Mid, first.VipType, first.DeltaDays)
		return nil, model.ErrRequestIdReused
	}
	row, err := l.svcCtx.Membership.FindOne(l.ctx, first.Mid, first.VipType)
	if err != nil {
		l.Errorf("membership/RevokeMembership: replay reread mid=%d vip_type=%d err=%v", first.Mid, first.VipType, err)
		return nil, err
	}
	if row == nil {
		l.Errorf("membership/RevokeMembership: grant_id=%d has no membership row, mid=%d vip_type=%d",
			first.GrantID, first.Mid, first.VipType)
		return nil, model.ErrMembershipNotFound
	}
	l.Infof("membership/RevokeMembership: replay request_id=%s mid=%d grant_id=%d, duration not deducted twice",
		in.RequestId, in.Mid, first.GrantID)
	return &rpc.RevokeMembershipReply{
		Duplicated: true,
		Membership: membershipToRPC(row),
		GrantId:    first.GrantID,
	}, nil
}
