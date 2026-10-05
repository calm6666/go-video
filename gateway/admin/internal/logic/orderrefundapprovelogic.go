// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	tradeorderrpc "go-video/services/trade-order/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OrderRefundApproveLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 审批通过退款（先退余额再回收权益；部分成功原样投影在 revoke_detail）
func NewOrderRefundApproveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OrderRefundApproveLogic {
	return &OrderRefundApproveLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OrderRefundApprove 转发 trade-order ApproveRefund（审批通过并回收权益）。
//
// 这是本域最重的写口：它一次触发「payment 退余额 + membership 回收时长 / coin 扣回硬币 +
// 订单状态推进 + 台账留证」四件事（§5 只有 trade-order 能这样驱动）。因此网关一侧
// 只有主体、必填与不可能形状三类门槛，**一个业务判定都不复算**：
//   - 「这单现在能不能批」（只有 REFUND_REQUESTED/REFUND_APPROVED 可批）→ 服务的
//     ErrRefundNotRequested；
//   - 「退款有没有绑定支付单」（ErrPaymentNoRequired）、「还剩多少可退」
//     （RefundableMinor 在审批那一刻重算，ErrRefundNothingToRefund）→ 服务；
//   - 「钱有没有真的退掉」→ 服务读 payment 回的 refund state，未成功直接报错并回滚语义；
//   - CAS 冲突（ErrConcurrentUpdate）→ 服务，调用方带最新 version 重试，网关不自动重放写。
//
// **不提供部分退款**：ApproveRefundReq 没有任何金额位，网关也就没有 amount_minor 字段可填；
// 服务侧当前只能整单全额退（下游 RefundPaymentReq 不带 biz_order_no、也没有「按单已消费额度」
// 查询能力，见 ErrRefundAmountInvalid）。网关不得按比例换算、不得替调用方造一个可退金额。
// 退款的落点是**沙箱余额**（refundToBalance 里 ToBalance=true），不是银行卡——
// 「原路退回渠道」在本项目不存在（AGENTS.md §1），网关不出现「退到卡」这类字段或文案。
//
// revoke_detail 是「部分成功」的唯一出口：款已退而权益未回收时，服务把订单停在
// REFUND_APPROVED 并**带着结论正常返回**（不是错误）。网关逐字转达，绝不美化成
// 「全部成功」，也不因为订单还没进 REFUNDED 就自己造个错误——那会把一次真实的退款说成没发生。
//
// duplicated=true（已 REFUNDED 的幂等重放）是**成功结论**并带首次退款单号，网关不折叠成 500。
func (l *OrderRefundApproveLogic) OrderRefundApprove(req *types.ParamOrderRefundApprove) (resp *types.OrderRefundApproveResponse, err error) {
	if l.svcCtx.TradeOrder == nil {
		return nil, errOrderServiceNotConfigured
	}
	if req == nil {
		return nil, errOrderRequestMissing
	}
	operator, err := orderOperator(l.ctx, "orderRefundApprove", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("order_no", req.OrderNo); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	// expected_version 是 CAS 位点：负数不可能出现在订单行上，先挡掉；
	// 0 原样下传，由服务按「审批类写接口必须带 CAS 版本」拒绝
	// （ErrExpectedVersionRequired）。网关不替它去查当前版本——那一读一写之间版本
	// 早就可能变了，替调用方补号等于把乐观锁换成「保证不冲突」的假承诺。
	if err := orderNonNeg("expected_version", req.ExpectedVersion); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.TradeOrder.ApproveRefund(l.ctx, &tradeorderrpc.ApproveRefundReq{
		OrderNo:         req.OrderNo,
		Operator:        operator,
		RequestId:       req.IdempotencyKey,
		Reason:          req.Reason,
		ExpectedVersion: req.ExpectedVersion,
	})
	if err != nil {
		// trace_id 只进日志（ApproveRefundReq 没有该字段可下传）。
		l.Errorf("gateway/admin/orderRefundApprove: order_no=%q expected_version=%d operator=%s trace_id=%s err=%v",
			req.OrderNo, req.ExpectedVersion, operator, req.TraceId, err)
		return nil, err
	}
	// 钱已经动了，成功侧必须留下网关这一层的证据（服务记的是它自己的台账，两边编号空间不同）；
	// 尤其「已退但未回收」这种部分成功，日志里要能一眼看出订单停在哪。
	l.Infof("gateway/admin/orderRefundApprove: order_no=%q duplicated=%t refund_no=%q state=%d fulfill_detail=%q revoke_detail=%q operator=%s",
		req.OrderNo, reply.GetDuplicated(), reply.GetRefundNo(),
		int32(reply.GetOrder().GetState()), reply.GetOrder().GetFulfillDetail(),
		reply.GetRevokeDetail(), operator)
	return &types.OrderRefundApproveResponse{
		Code:    0,
		Message: "ok",
		Data: types.OrderRefundApproveData{
			Duplicated:   reply.GetDuplicated(),
			RefundNo:     reply.GetRefundNo(),
			RevokeDetail: reply.GetRevokeDetail(),
			Order:        orderToAPI(reply.GetOrder()),
		},
		TTL: 0,
	}, nil
}
