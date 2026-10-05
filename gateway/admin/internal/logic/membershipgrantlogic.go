// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	membershiprpc "go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MembershipGrantLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营手工开通/延长会员（沙箱台账之外的独立来源；不扣钱，reason 必填）
func NewMembershipGrantLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembershipGrantLogic {
	return &MembershipGrantLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MembershipGrant 转发 membership GrantMembership（运营手工开通/延长）。
//
// 本路由不扣钱、不建支付单：它只写会员身份与授予台账（§1 资金语义）。所以
// 「后台不该用付费来源补数」这件事不在网关里判 —— source 的取值合法性、
// 「付费来源必须能回溯到 biz_order_no 或 payment_no」、「ADMIN_OPS/EXPERIENCE 必须写 reason」
// 三条都是 membership 的硬规则，网关自己 check 一遍只会和服务的口径漂移。
// 同理 delta_days 的正负与 ±MaxGrantDeltaDays 区间、plan_id 与档位是否一致、
// 套餐是否还是 DRAFT 全部原样交给服务。
//
// 网关只挡主体位（mid>0、vip_type/source 非 UNSPECIFIED）、幂等键与空的必填字符串位；
// request_id 重放回查首次结论并带 duplicated=true，那是**成功响应**而不是错误，
// 网关不把它折叠成 500，也不因为 duplicated 就把 grant_id 归零。
func (l *MembershipGrantLogic) MembershipGrant(req *types.ParamMembershipGrant) (resp *types.MembershipGrantResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errMembershipServiceNotConfigured
	}
	if req == nil {
		return nil, errMembershipRequestMissing
	}
	operator, err := membershipOperator(l.ctx, "membershipGrant", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := membershipPositive("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := membershipEnum("vip_type", req.VipType); err != nil {
		return nil, err
	}
	if err := membershipEnum("source", req.Source); err != nil {
		return nil, err
	}
	if err := membershipNonNeg("plan_id", req.PlanId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.GrantMembership(l.ctx, &membershiprpc.GrantMembershipReq{
		Mid:        req.Mid,
		VipType:    membershiprpc.VipType(req.VipType),
		PlanId:     req.PlanId,
		DeltaDays:  req.DeltaDays,
		Source:     membershiprpc.GrantSource(req.Source),
		BizOrderNo: req.BizOrderNo,
		PaymentNo:  req.PaymentNo,
		Operator:   operator,
		RequestId:  req.IdempotencyKey,
		Reason:     req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/membershipGrant: mid=%d vip_type=%d plan_id=%d delta_days=%d source=%d biz_order_no=%q operator=%s trace_id=%s err=%v",
			req.Mid, req.VipType, req.PlanId, req.DeltaDays, req.Source, req.BizOrderNo, operator, req.TraceId, err)
		return nil, err
	}
	// 加时长是对外生效的动作，成功也要留一条网关侧证据（服务侧记的是自己的账，两边编号空间不同）。
	l.Infof("gateway/admin/membershipGrant: mid=%d vip_type=%d grant_id=%d duplicated=%t expire_at=%d operator=%s",
		req.Mid, req.VipType, reply.GetGrantId(), reply.GetDuplicated(), reply.GetMembership().GetExpireAt(), operator)
	return &types.MembershipGrantResponse{
		Code:    0,
		Message: "ok",
		Data: types.MembershipGrantData{
			Duplicated: reply.GetDuplicated(),
			GrantId:    reply.GetGrantId(),
			Membership: membershipMemberToAPI(reply.GetMembership()),
		},
		TTL: 0,
	}, nil
}
