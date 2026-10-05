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

type MembershipGrantRevokeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 收回会员（立即失效或按天扣回；重复提交回首次结论）
func NewMembershipGrantRevokeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembershipGrantRevokeLogic {
	return &MembershipGrantRevokeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MembershipGrantRevoke 转发 membership RevokeMembership（收回会员）。
//
// clear_remaining 与 delta_days 是两种差别很大的语义（立即失效 / 只扣回若干天），
// 服务侧还有一条互斥规则（clear_remaining=true 必须带 delta_days=0）与「扣过头停在 now」
// 的口径：这些都是收回结论的一部分，网关一个都不复算，也不给任何一位填默认值。
// reason 在服务侧无条件必填，所以这里提前挡住空串并点名字段。
//
// 两处口径（2026-09-22 收口，见 `docs/roadmap.md` 的 .api 补丁轮）：
//   - `vip_type` 是**必填**位（早先注释误写成「0 表示由服务按最高档处理」）。服务侧
//     requireVipType 对 0 是硬错误，网关不把 0 悄悄改成 1（那等于替用户挑一档来收回），
//     显式传 0 时原样下传，让调用方收到真实的 ErrInvalidVipType；
//   - plan_id/biz_order_no/payment_no 是追溯位，之前表单没有 → 本路由只能落「运营手工收回」。
//     现在三位原样下传：网关不填默认、不猜单号、不 trim（悄悄改写等于换一笔引用）。
//     服务口径是三位全空即手工收回，而 source 列只有 ADMIN_OPS 一个值，
//     「退款回收」（trade-order 直连带单号）与「运营纠错」的区分就靠这三个引用；
//     单号长度上限、套餐是否存在都由服务判，网关只挡掉含空白的单号——
//     跨服务引用靠精确匹配，带空格的单号是一条永远对不上账的台账。
func (l *MembershipGrantRevokeLogic) MembershipGrantRevoke(req *types.ParamMembershipRevoke) (resp *types.MembershipRevokeResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errMembershipServiceNotConfigured
	}
	if req == nil {
		return nil, errMembershipRequestMissing
	}
	operator, err := membershipOperator(l.ctx, "membershipGrantRevoke", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := membershipPositive("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := membershipNonNeg("plan_id", req.PlanId); err != nil {
		return nil, err
	}
	if err := requireNoWhitespace("biz_order_no", req.BizOrderNo); err != nil {
		return nil, err
	}
	if err := requireNoWhitespace("payment_no", req.PaymentNo); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.RevokeMembership(l.ctx, &membershiprpc.RevokeMembershipReq{
		Mid:            req.Mid,
		VipType:        membershiprpc.VipType(req.VipType),
		ClearRemaining: req.ClearRemaining,
		DeltaDays:      req.DeltaDays,
		Operator:       operator,
		RequestId:      req.IdempotencyKey,
		Reason:         req.Reason,
		PlanId:         req.PlanId,
		BizOrderNo:     req.BizOrderNo,
		PaymentNo:      req.PaymentNo,
	})
	if err != nil {
		l.Errorf("gateway/admin/membershipGrantRevoke: mid=%d vip_type=%d clear_remaining=%t delta_days=%d plan_id=%d biz_order_no=%s payment_no=%s operator=%s trace_id=%s err=%v",
			req.Mid, req.VipType, req.ClearRemaining, req.DeltaDays, req.PlanId, req.BizOrderNo, req.PaymentNo, operator, req.TraceId, err)
		return nil, err
	}
	// 收回会立刻掐断用户已付费的能力，成功必须留下「谁在什么时候以什么口径收的」这条网关证据。
	l.Infof("gateway/admin/membershipGrantRevoke: mid=%d vip_type=%d grant_id=%d duplicated=%t expire_at=%d plan_id=%d biz_order_no=%s payment_no=%s operator=%s",
		req.Mid, req.VipType, reply.GetGrantId(), reply.GetDuplicated(), reply.GetMembership().GetExpireAt(),
		req.PlanId, req.BizOrderNo, req.PaymentNo, operator)
	return &types.MembershipRevokeResponse{
		Code:    0,
		Message: "ok",
		Data: types.MembershipRevokeData{
			Duplicated: reply.GetDuplicated(),
			GrantId:    reply.GetGrantId(),
			Membership: membershipMemberToAPI(reply.GetMembership()),
		},
		TTL: 0,
	}, nil
}
