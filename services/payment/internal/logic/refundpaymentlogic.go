package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RefundPaymentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRefundPaymentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RefundPaymentLogic {
	return &RefundPaymentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RefundPayment 退款。本项目只支持退回到余额（destination=BALANCE）。
//
// 判定口径：
//   - to_balance=false 就是要求原路退回渠道 → FailedPrecondition
//     "payment: refund to original channel not configured"，绝不返回「已退回」；
//   - 退余额只对 BALANCE 支付方式成立：沙箱渠道支付的单从未占用余额，
//     把它退成余额等于凭空给台账加钱，因此同样拒绝并说明原因；
//   - PENDING/FAILED/CLOSED 单没有真实收账，不可退；
//   - 累计退款不得超过 amount_minor：守卫写在 UPDATE 的 WHERE 里
//     （refunded_minor + ? <= amount_minor AND state IN (PAID, PARTIALLY_REFUNDED)），
//     不查后改；超退或并发挤占 → 影响行数 0 → 整体回滚；
//   - 「写 pm_refund + 更新 pm_payment + 加余额 + 写 pm_flow」同一事务；
//   - amount_minor=0 表示全额剩余可退；reason、operator、request_id 必填；
//   - 幂等：uniq_request_id 命中重放时返回原退款单并置 duplicated=true。
func (l *RefundPaymentLogic) RefundPayment(in *rpc.RefundPaymentReq) (*rpc.RefundPaymentReply, error) {
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
	if in.AmountMinor < 0 {
		return nil, model.ErrRefundAmountNotPositive
	}
	if !in.ToBalance {
		return nil, model.ErrRefundToChannelNotConfigured
	}

	m := l.svcCtx.Models
	payment, err := m.Payment.FindOne(l.ctx, in.PaymentNo)
	if err != nil {
		l.Errorf("payment/RefundPayment: lookup payment_no=%s err=%v", in.PaymentNo, err)
		return nil, err
	}
	if payment == nil {
		return nil, model.ErrPaymentNotFound
	}
	if err := checkRefundable(payment); err != nil {
		return nil, err
	}

	refundMinor := in.AmountMinor
	if refundMinor == 0 {
		refundMinor = payment.RefundableMinor()
	}
	if refundMinor <= 0 {
		return nil, model.ErrRefundExceedsAmount
	}
	if refundMinor > payment.RefundableMinor() {
		return nil, model.ErrDetail(model.ErrRefundExceedsAmount,
			fmt.Sprintf("refundable_minor=%d", payment.RefundableMinor()))
	}

	// 幂等重放：同一 request_id 只能有一张退款单。
	existing, err := m.Refund.FindByRequestID(l.ctx, in.RequestId)
	if err != nil {
		l.Errorf("payment/RefundPayment: replay lookup request_id=%s err=%v", in.RequestId, err)
		return nil, err
	}
	if existing != nil {
		return l.reply(existing.RefundNo, existing, true)
	}

	refundNo, err := newDocumentNo(prefixRefund)
	if err != nil {
		return nil, err
	}
	row := &model.Refund{
		RefundNo:    refundNo,
		RequestId:   in.RequestId,
		PaymentNo:   payment.PaymentNo,
		BizOrderNo:  payment.BizOrderNo,
		Mid:         payment.Mid,
		AmountMinor: refundMinor,
		Currency:    payment.Currency,
		State:       model.RefundStateSucceeded,
		Destination: model.DestinationBalance,
		Operator:    in.Operator,
		Reason:      in.Reason,
	}

	var dupKeyErr bool
	err = m.Tx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		if _, err := m.Refund.InsertTx(ctx, session, row); err != nil {
			return err
		}
		ok, err := m.Payment.RefundTx(ctx, session, payment.PaymentNo, refundMinor,
			in.Operator, in.RequestId, in.Reason)
		if err != nil {
			return err
		}
		if !ok {
			// 累计超退或状态被并发推进：整笔回滚，退款单也不留。
			return model.ErrRefundExceedsAmount
		}
		wallet, err := m.Wallet.ApplyDeltaTx(ctx, session, payment.Mid, payment.Currency, refundMinor)
		if err != nil {
			return err
		}
		_, err = m.Flow.InsertTx(ctx, session, &model.Flow{
			Mid:               payment.Mid,
			BizType:           model.FlowBizRefund,
			BizNo:             refundNo,
			DeltaMinor:        refundMinor,
			BalanceAfterMinor: wallet.BalanceMinor,
			Currency:          payment.Currency,
			Remark:            "refund to balance",
			Operator:          in.Operator,
			RequestId:         in.RequestId,
		})
		return err
	})
	if err != nil {
		switch {
		case m.Refund.IsDuplicate(err) || m.Flow.IsDuplicate(err):
			dupKeyErr = true
		case errors.Is(err, model.ErrRefundExceedsAmount):
			fresh, lookupErr := m.Payment.FindOne(l.ctx, in.PaymentNo)
			if lookupErr != nil {
				l.Errorf("payment/RefundPayment: re-read after reject payment_no=%s err=%v", in.PaymentNo, lookupErr)
				return nil, lookupErr
			}
			if fresh == nil {
				return nil, model.ErrPaymentNotFound
			}
			return nil, model.ErrDetail(model.ErrRefundExceedsAmount,
				fmt.Sprintf("refundable_minor=%d", fresh.RefundableMinor()))
		default:
			l.Errorf("payment/RefundPayment: refund failed payment_no=%s request_id=%s err=%v",
				in.PaymentNo, in.RequestId, err)
			return nil, err
		}
	}
	if dupKeyErr {
		// 事务已回滚：request_id 被别的台账行占用，要求换号，不猜是哪一笔。
		concurrent, lookupErr := m.Refund.FindByRequestID(l.ctx, in.RequestId)
		if lookupErr == nil && concurrent != nil {
			return l.reply(concurrent.RefundNo, concurrent, true)
		}
		l.Errorf("payment/RefundPayment: request_id reused payment_no=%s request_id=%s", in.PaymentNo, in.RequestId)
		return nil, model.ErrRequestIDReused
	}
	return l.reply(refundNo, nil, false)
}

// checkRefundable 支付方式与状态门禁；不可退时给出可执行的说明。
func checkRefundable(p *model.Payment) error {
	if p.Method != model.MethodBalance {
		return model.ErrRefundRequiresBalancePayment
	}
	switch p.State {
	case model.PaymentStatePaid, model.PaymentStatePartiallyRefunded, model.PaymentStateRefunded:
		if p.RefundableMinor() <= 0 {
			return model.ErrDetail(model.ErrRefundExceedsAmount, "payment fully refunded")
		}
		return nil
	default:
		return model.ErrDetail(model.ErrPaymentNotRefundable, "payment_state="+rpc.PaymentState(p.State).String())
	}
}

// reply 回读退款单与支付单的最新状态组装响应（绝不回显未提交数据）。
func (l *RefundPaymentLogic) reply(refundNo string, known *model.Refund, duplicated bool) (*rpc.RefundPaymentReply, error) {
	m := l.svcCtx.Models
	refund := known
	if refund == nil {
		var err error
		refund, err = m.Refund.FindOne(l.ctx, refundNo)
		if err != nil {
			l.Errorf("payment/RefundPayment: read back refund_no=%s err=%v", refundNo, err)
			return nil, err
		}
		if refund == nil {
			// 事务已提交却读不到退款单：台账不一致，必须报错而不是返回空成功。
			l.Errorf("payment/RefundPayment: refund row missing after commit refund_no=%s", refundNo)
			return nil, model.ErrConcurrentUpdate
		}
	}
	payment, err := m.Payment.FindOne(l.ctx, refund.PaymentNo)
	if err != nil {
		l.Errorf("payment/RefundPayment: payment snapshot payment_no=%s err=%v", refund.PaymentNo, err)
		return nil, err
	}
	wallet, err := m.Wallet.FindOne(l.ctx, refund.Mid)
	if err != nil {
		l.Errorf("payment/RefundPayment: wallet snapshot mid=%d err=%v", refund.Mid, err)
		return nil, err
	}
	return &rpc.RefundPaymentReply{
		Duplicated: duplicated,
		Refund:     refundInfo(refund),
		Payment:    paymentInfo(payment),
		Wallet:     walletInfo(wallet, refund.Mid, refund.Currency),
	}, nil
}
