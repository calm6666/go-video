package logic

import (
	"context"
	"errors"
	"strings"
	"time"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type CreatePaymentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreatePaymentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreatePaymentLogic {
	return &CreatePaymentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreatePayment 受理支付（供 trade-order 使用），建单即终态。
//
// 判定口径：
//   - amount_minor <= 0 直接拒绝；币种必须是配置里的单一币种，不换汇；
//   - 只受理 BALANCE 与 SANDBOX_CHANNEL 两种方式，其余（含 UNSPECIFIED）一律拒绝；
//     SANDBOX_CHANNEL 还要过渠道门禁，被关掉时返回 not-configured；
//   - biz_order_no 唯一 = 一单一支付：
//     同 request_id 重放 → 返回首单 + duplicated=true；
//     同单号、同金额、同币种（换了 request_id）→ 仍返回首单 + duplicated=true；
//     同单号但金额或币种不同 → AlreadyExists 冲突错误，绝不静默改价；
//   - BALANCE 走条件扣减（WHERE balance_minor >= ?），0 行即余额不足 FailedPrecondition，
//     并且此时不写支付单、不写流水；
//   - 「扣余额 + 写 pm_payment + 写 pm_flow」在同一事务，任何一步失败整体回滚；
//   - SANDBOX_CHANNEL 受理即 PAID，但不写余额流水（钱没经过余额账户），
//     回复里的 wallet 是未变动的余额快照。
func (l *CreatePaymentLogic) CreatePayment(in *rpc.CreatePaymentReq) (*rpc.CreatePaymentReply, error) {
	cfg := l.svcCtx.Config.Payment
	if strings.TrimSpace(in.BizOrderNo) == "" {
		return nil, model.ErrBizOrderNoRequired
	}
	if err := requireMaxLength("biz_order_no", in.BizOrderNo, 64); err != nil {
		return nil, err
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.AmountMinor <= 0 {
		return nil, model.ErrAmountNotPositive
	}
	currency, err := resolveCurrency(cfg, in.Currency)
	if err != nil {
		return nil, err
	}
	if err := requireRequestID(in.RequestId); err != nil {
		return nil, err
	}
	if err := requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if err := requireMaxLength("subject", in.Subject, maxTextRunes); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if in.ExpireAt < 0 || (in.ExpireAt > 0 && in.ExpireAt <= now) {
		return nil, model.ErrExpireInPast
	}

	switch in.Method {
	case rpc.PayMethod_PAY_METHOD_BALANCE, rpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL:
	default:
		return nil, model.ErrDetail(model.ErrInvalidMethod, "method="+in.Method.String())
	}
	if in.Method == rpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL &&
		!cfg.AllowsChannel(channelName(rpc.PayChannel_PAY_CHANNEL_SANDBOX)) {
		return nil, model.ErrChannelNotConfigured
	}

	m := l.svcCtx.Models
	// 幂等重放优先于一切：同一 request_id 只会有一张支付单。
	byRequest, err := m.Payment.FindByRequestID(l.ctx, in.RequestId)
	if err != nil {
		l.Errorf("payment/CreatePayment: request lookup request_id=%s err=%v", in.RequestId, err)
		return nil, err
	}
	if byRequest != nil {
		return l.reply(byRequest, true)
	}
	// 一单一支付：同订单号已受理过就先给结论，避免并发下重复扣款。
	byOrder, err := m.Payment.FindByBizOrderNo(l.ctx, in.BizOrderNo)
	if err != nil {
		l.Errorf("payment/CreatePayment: order lookup biz_order_no=%s err=%v", in.BizOrderNo, err)
		return nil, err
	}
	if byOrder != nil {
		if byOrder.AmountMinor != in.AmountMinor || byOrder.Currency != currency {
			return nil, model.ErrPaymentOrderConflict
		}
		return l.reply(byOrder, true)
	}

	paymentNo, err := newDocumentNo(prefixPayment)
	if err != nil {
		return nil, err
	}
	row := &model.Payment{
		PaymentNo:   paymentNo,
		BizOrderNo:  in.BizOrderNo,
		RequestId:   in.RequestId,
		Mid:         in.Mid,
		AmountMinor: in.AmountMinor,
		Currency:    currency,
		Method:      int32(in.Method),
		State:       model.PaymentStatePaid,
		Subject:     in.Subject,
		Operator:    in.Operator,
		PaidAt:      now,
		ExpireAt:    in.ExpireAt,
	}

	err = m.Tx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		if in.Method == rpc.PayMethod_PAY_METHOD_BALANCE {
			// 条件扣减在前：余额不足时这里直接返回错误，后面两步都不会执行。
			wallet, err := m.Wallet.ApplyDeltaTx(ctx, session, in.Mid, currency, -in.AmountMinor)
			if err != nil {
				return err
			}
			if _, err := m.Payment.InsertTx(ctx, session, row); err != nil {
				return err
			}
			_, err = m.Flow.InsertTx(ctx, session, &model.Flow{
				Mid:               in.Mid,
				BizType:           model.FlowBizPayment,
				BizNo:             paymentNo,
				DeltaMinor:        -in.AmountMinor,
				BalanceAfterMinor: wallet.BalanceMinor,
				Currency:          currency,
				Remark:            "balance payment",
				Operator:          in.Operator,
				RequestId:         in.RequestId,
			})
			return err
		}
		// SANDBOX_CHANNEL：受理即成功，不动余额、不写余额流水。
		_, err := m.Payment.InsertTx(ctx, session, row)
		return err
	})
	if err != nil {
		if errors.Is(err, model.ErrInsufficientBalance) || errors.Is(err, model.ErrUnsupportedCurrency) {
			l.Infof("payment/CreatePayment: rejected biz_order_no=%s mid=%d amount_minor=%d reason=%s",
				in.BizOrderNo, in.Mid, in.AmountMinor, err.Error())
			return nil, err
		}
		if m.Payment.IsDuplicate(err) || m.Flow.IsDuplicate(err) {
			// 事务已整体回滚：回读把并发收敛成「重放首单」或「冲突」两种确定结论。
			resolved, resolveErr := l.resolveConcurrent(in.BizOrderNo, in.RequestId, currency, in.AmountMinor)
			if resolveErr != nil {
				return nil, resolveErr
			}
			return resolved, nil
		}
		l.Errorf("payment/CreatePayment: settle failed biz_order_no=%s request_id=%s err=%v", in.BizOrderNo, in.RequestId, err)
		return nil, err
	}

	return l.reply(row, false)
}

// resolveConcurrent 唯一键冲突后的回读判定：宁可报冲突，也不返回未提交的数据。
func (l *CreatePaymentLogic) resolveConcurrent(bizOrderNo, requestID, currency string, amountMinor int64) (*rpc.CreatePaymentReply, error) {
	m := l.svcCtx.Models
	byRequest, err := m.Payment.FindByRequestID(l.ctx, requestID)
	if err != nil {
		l.Errorf("payment/CreatePayment: conflict lookup request_id=%s err=%v", requestID, err)
		return nil, err
	}
	if byRequest != nil {
		return l.reply(byRequest, true)
	}
	byOrder, err := m.Payment.FindByBizOrderNo(l.ctx, bizOrderNo)
	if err != nil {
		l.Errorf("payment/CreatePayment: conflict lookup biz_order_no=%s err=%v", bizOrderNo, err)
		return nil, err
	}
	if byOrder != nil {
		if byOrder.AmountMinor != amountMinor || byOrder.Currency != currency {
			return nil, model.ErrPaymentOrderConflict
		}
		return l.reply(byOrder, true)
	}
	// 单据都不存在：只可能是流水唯一键撞了，台账未变动，要求换新请求号。
	return nil, model.ErrRequestIDReused
}

// reply 回读余额快照后组装响应。
// BALANCE 拿到的是扣减后的余额；SANDBOX_CHANNEL 未动余额，等价于受理前快照。
func (l *CreatePaymentLogic) reply(p *model.Payment, duplicated bool) (*rpc.CreatePaymentReply, error) {
	wallet, err := l.svcCtx.Models.Wallet.FindOne(l.ctx, p.Mid)
	if err != nil {
		l.Errorf("payment/CreatePayment: wallet snapshot mid=%d err=%v", p.Mid, err)
		return nil, err
	}
	return &rpc.CreatePaymentReply{
		Duplicated: duplicated,
		Payment:    paymentInfo(p),
		Wallet:     walletInfo(wallet, p.Mid, p.Currency),
	}, nil
}
