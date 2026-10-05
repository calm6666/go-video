package logic

import (
	"context"
	"strings"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ClosePaymentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewClosePaymentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ClosePaymentLogic {
	return &ClosePaymentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// closeAction 是关闭支付单前对当前状态的判定结果。
type closeAction int

const (
	// closeActionProceed 处于 PENDING，可以推进到 CLOSED。
	closeActionProceed closeAction = iota
	// closeActionReplay 已是 CLOSED，本次调用按重放返回。
	closeActionReplay
	// closeActionReject 状态不允许关闭，附带的 error 就是结论。
	closeActionReject
)

// classifyClose 把状态映射成明确结论，拒绝原因里指明该走哪条路。
func classifyClose(state int32) (closeAction, error) {
	switch state {
	case model.PaymentStatePending:
		return closeActionProceed, nil
	case model.PaymentStateClosed:
		return closeActionReplay, nil
	case model.PaymentStatePaid, model.PaymentStateRefunded, model.PaymentStatePartiallyRefunded:
		return closeActionReject, model.ErrPaymentAlreadyPaid
	default:
		return closeActionReject, model.ErrDetail(model.ErrPaymentNotPending,
			"payment_state="+rpc.PaymentState(state).String())
	}
}

// ClosePayment 关闭未支付支付单。
//
// 判定口径：
//   - reason 必填（订单作废要留证据），operator、request_id 必填；
//   - 只有 PENDING 可关。本服务两条受理路径都落同步终态，所以沙箱下这里的常见结果
//     就是「已 PAID，拒绝」；保留 PENDING 分支是为了未来接入真实渠道的中间态；
//   - 已 PAID / 已退款的单绝不能靠关闭回滚：钱已收，退回只能走 RefundPayment，
//     错误消息里写明这条路；
//   - 已 CLOSED 重复关闭 → duplicated=true（天然幂等）；
//   - 关闭不动余额也不写流水：PENDING 单从未扣过款（扣款与置 PAID 在同一事务内）。
func (l *ClosePaymentLogic) ClosePayment(in *rpc.ClosePaymentReq) (*rpc.ClosePaymentReply, error) {
	if strings.TrimSpace(in.PaymentNo) == "" {
		return nil, model.ErrPaymentNoRequired
	}
	if err := requireMaxLength("payment_no", in.PaymentNo, 40); err != nil {
		return nil, err
	}
	if err := requireRequestID(in.RequestId); err != nil {
		return nil, err
	}
	if err := requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}

	m := l.svcCtx.Models
	payment, err := m.Payment.FindOne(l.ctx, in.PaymentNo)
	if err != nil {
		l.Errorf("payment/ClosePayment: lookup payment_no=%s err=%v", in.PaymentNo, err)
		return nil, err
	}
	if payment == nil {
		return nil, model.ErrPaymentNotFound
	}
	action, err := classifyClose(payment.State)
	if err != nil {
		return nil, err
	}
	if action == closeActionReplay {
		return &rpc.ClosePaymentReply{Duplicated: true, Payment: paymentInfo(payment)}, nil
	}

	ok, err := m.Payment.CloseTx(l.ctx, nil, in.PaymentNo, in.Operator, in.RequestId, in.Reason)
	if err != nil {
		l.Errorf("payment/ClosePayment: close payment_no=%s err=%v", in.PaymentNo, err)
		return nil, err
	}
	fresh, err := m.Payment.FindOne(l.ctx, in.PaymentNo)
	if err != nil {
		l.Errorf("payment/ClosePayment: re-read payment_no=%s err=%v", in.PaymentNo, err)
		return nil, err
	}
	if fresh == nil {
		return nil, model.ErrPaymentNotFound
	}
	if !ok {
		// 条件未命中：并发已推进状态，按真实状态给结论，不假装关闭成功。
		after, classifyErr := classifyClose(fresh.State)
		if classifyErr != nil {
			return nil, classifyErr
		}
		if after == closeActionReplay {
			return &rpc.ClosePaymentReply{Duplicated: true, Payment: paymentInfo(fresh)}, nil
		}
		return nil, model.ErrConcurrentUpdate
	}
	return &rpc.ClosePaymentReply{Payment: paymentInfo(fresh)}, nil
}
