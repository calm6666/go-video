package logic

import (
	"context"
	"strings"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetPaymentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetPaymentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetPaymentLogic {
	return &GetPaymentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetPayment 支付单读取：payment_no 与 biz_order_no 二选一。
//
// 读不到就是 found=false（订单域经常要问「这单有没有支付过」，这不是错误）；
// 但查询本身失败必须上抛错误——把 DB 故障说成「没有支付单」会让订单侧误判可重试。
func (l *GetPaymentLogic) GetPayment(in *rpc.GetPaymentReq) (*rpc.GetPaymentReply, error) {
	paymentNo := strings.TrimSpace(in.PaymentNo)
	bizOrderNo := strings.TrimSpace(in.BizOrderNo)
	if paymentNo != "" && bizOrderNo != "" {
		return nil, model.ErrGetPaymentKeyExclusive
	}
	if paymentNo == "" && bizOrderNo == "" {
		return nil, model.ErrGetPaymentKeyRequired
	}

	m := l.svcCtx.Models
	var (
		p   *model.Payment
		err error
	)
	switch {
	case paymentNo != "":
		if err = requireMaxLength("payment_no", paymentNo, 40); err != nil {
			return nil, err
		}
		p, err = m.Payment.FindOne(l.ctx, paymentNo)
	default:
		if err = requireMaxLength("biz_order_no", bizOrderNo, 64); err != nil {
			return nil, err
		}
		p, err = m.Payment.FindByBizOrderNo(l.ctx, bizOrderNo)
	}
	if err != nil {
		l.Errorf("payment/GetPayment: payment_no=%q biz_order_no=%q err=%v", paymentNo, bizOrderNo, err)
		return nil, err
	}
	if p == nil {
		return &rpc.GetPaymentReply{Found: false}, nil
	}
	return &rpc.GetPaymentReply{Found: true, Payment: paymentInfo(p)}, nil
}
