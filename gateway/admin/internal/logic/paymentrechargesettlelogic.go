// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	paymentrpc "go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PaymentRechargeSettleLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 结算沙箱充值单（置 SUCCESS 并入账；只动本地台账，无渠道回调）
func NewPaymentRechargeSettleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PaymentRechargeSettleLogic {
	return &PaymentRechargeSettleLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PaymentRechargeSettle 转发 payment SettleSandboxRecharge（结算一张已存在的沙箱充值单）。
//
// 这一步在沙箱里就是「钱到账」的唯一入口：真实渠道下它由渠道异步回调驱动，本项目没有回调，
// 也不接任何第三方支付网关，因此**没有**「等回调再到账」的中间态，更没有伪造成功——
// 入账要同时满足「单据存在 + 状态是 PENDING + 渠道没被关掉」，三条都由服务判：
//   - 单据不存在 → NotFound（ErrRechargeNotFound），网关不把它变成 duplicated 或空钱包成功；
//   - CANCELLED/FAILED 单 → FailedPrecondition（钱从未进账也无从作废）；
//   - 已 SUCCESS 单 → duplicated=true 且回首次结论（单据 + 当前余额 + 原流水），
//     这是**成功响应**不是错误，网关不折叠成 500、也不因为重复就把 flow_id 归零；
//   - 「改单据 + 加余额 + 写流水」在同一个事务里，任何一步失败整体回滚；
//     request_id 被别的台账行用掉 → ErrRequestIDReused（AlreadyExists，什么都没入账）。
//
// 网关只挡必填（recharge_no、reason、idempotency_key）与身份：
// operator 由会话渲染成 gateway/admin:<admin_id>（表单自报的只做日志线索），
// 幂等键原样进 request_id（改一个字符等于换一次入账）。
//
// 口径差异（有意更严，已记录）：服务对 settle 的 reason 只做长度校验（可为空，
// 因为自动结算方是 "cron"），而 admin.api 把这一位标成必填（无 optional）。
// 人工点按钮时必须写下理由才进台账，故网关按 .api 的必填口径挡。
func (l *PaymentRechargeSettleLogic) PaymentRechargeSettle(req *types.ParamPaymentRechargeSettle) (resp *types.PaymentRechargeSettleResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errPaymentServiceNotConfigured
	}
	if req == nil {
		return nil, errPaymentRequestMissing
	}
	operator, err := paymentOperator(l.ctx, "paymentRechargeSettle", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("recharge_no", req.RechargeNo); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.SettleSandboxRecharge(l.ctx, &paymentrpc.SettleSandboxRechargeReq{
		RechargeNo: req.RechargeNo,
		Operator:   operator,
		RequestId:  req.IdempotencyKey,
		Reason:     req.Reason,
	})
	if err != nil {
		// trace_id 只进日志（SettleSandboxRechargeReq 没有该字段可下传）。
		l.Errorf("gateway/admin/paymentRechargeSettle: recharge_no=%s operator=%s trace_id=%s err=%v",
			req.RechargeNo, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/paymentRechargeSettle: recharge_no=%s state=%d duplicated=%t flow_id=%d balance_minor=%d operator=%s",
		req.RechargeNo, reply.GetRecharge().GetState(), reply.GetDuplicated(), reply.GetFlowId(),
		reply.GetWallet().GetBalanceMinor(), operator)
	return &types.PaymentRechargeSettleResponse{
		Code:    0,
		Message: "ok",
		Data: types.PaymentRechargeSettleData{
			Duplicated: reply.GetDuplicated(),
			FlowId:     reply.GetFlowId(),
			Recharge:   paymentRechargeToAPI(reply.GetRecharge()),
			Wallet:     paymentWalletToAPI(reply.GetWallet()),
		},
		TTL: 0,
	}, nil
}
