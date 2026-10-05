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

type PaymentBalanceAdjustLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 余额差错更正（唯一直接改资金台账的后台口；必须 idempotency_key + operator + reason）
func NewPaymentBalanceAdjustLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PaymentBalanceAdjustLogic {
	return &PaymentBalanceAdjustLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PaymentBalanceAdjust 转发 payment AdjustBalance（余额差错更正）。
//
// **本仓库唯一能直接改写资金台账、且没有任何上游单据的后台口**，用途严格限定为沙箱
// 数对不上时的人工纠偏；它不是「送钱」功能，也不产生真实资金移动（§1）。
// 正因为没有单据可回溯，网关这一层的门槛全部是 fail-closed：
//   - 会话身份拿不到（AdminID<=0 或路由未被 AdminPermission 保护）→ 在下传前拒绝，
//     一次 RPC 都不发；operator 永远是 gateway/admin:<admin_id>，表单自称的值只做日志线索；
//   - idempotency_key 与 reason 必填：前者决定「手抖点两次是两笔还是一笔」，
//     后者是这笔流水在 pm_flow 里唯一的解释（服务侧 requireReason 同样必填，口径一致）。
//
// 判定一个都不接管（§5 资金台账归 payment，服务是唯一写主）：
//   - delta_minor 允许正负，只挡 0（无意义流水会污染对账，服务侧同样拒 0）；
//     绝对值上限 Payment.MaxAdjustMinor、余额被扣成负数（条件扣减 0 行 →
//     ErrInsufficientBalance，且不写任何流水）、币种与账户不一致全部由服务判；
//   - 幂等重放命中回 duplicated=true + 原流水 + 当前余额，这是**成功结论**；
//     同一 request_id 落在别的业务类型或别的 mid 上 → ErrRequestIDReused，事务已回滚，
//     网关原样透出，绝不换个号再打一次（那等于把冲突变成两笔调整）；
//   - currency 空串按服务默认币种归一，网关不代填。
//
// 加钱与扣钱走同一个 RPC，网关不做「只允许正向调整」这类猜测性收紧：
// 需要收紧的是权限点 payment:balance/adjust 的授予范围（与 recharge/settle 刻意分开）。
func (l *PaymentBalanceAdjustLogic) PaymentBalanceAdjust(req *types.ParamPaymentBalanceAdjust) (resp *types.PaymentBalanceAdjustResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errPaymentServiceNotConfigured
	}
	if req == nil {
		return nil, errPaymentRequestMissing
	}
	operator, err := paymentOperator(l.ctx, "paymentBalanceAdjust", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := paymentPositive("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := paymentDeltaNonZero(req.DeltaMinor); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.AdjustBalance(l.ctx, &paymentrpc.AdjustBalanceReq{
		Mid:        req.Mid,
		DeltaMinor: req.DeltaMinor,
		Currency:   req.Currency,
		Operator:   operator,
		RequestId:  req.IdempotencyKey,
		Reason:     req.Reason,
	})
	if err != nil {
		// trace_id 只进日志（AdjustBalanceReq 没有该字段可下传）。
		l.Errorf("gateway/admin/paymentBalanceAdjust: mid=%d delta_minor=%d currency=%q operator=%s trace_id=%s err=%v",
			req.Mid, req.DeltaMinor, req.Currency, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/paymentBalanceAdjust: mid=%d delta_minor=%d duplicated=%t flow_id=%d balance_minor=%d operator=%s",
		req.Mid, req.DeltaMinor, reply.GetDuplicated(), reply.GetFlowId(),
		reply.GetWallet().GetBalanceMinor(), operator)
	return &types.PaymentBalanceAdjustResponse{
		Code:    0,
		Message: "ok",
		Data: types.PaymentBalanceAdjustData{
			Duplicated: reply.GetDuplicated(),
			FlowId:     reply.GetFlowId(),
			Wallet:     paymentWalletToAPI(reply.GetWallet()),
		},
		TTL: 0,
	}, nil
}
