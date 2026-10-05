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

type PaymentWalletGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 余额查询（frozen_minor 恒 0，可用余额只看 balance_minor）
func NewPaymentWalletGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PaymentWalletGetLogic {
	return &PaymentWalletGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PaymentWalletGet 转发 payment GetWallet（余额查询）。
//
// 只读、不建行、不写台账：本路由没有任何写能力，服务侧也明确「账户不存在就是 0 余额」，
// 因此网关既不开户也不补数。网关只做三件事：
//  1. 客户端未配置一律回 errPaymentServiceNotConfigured，不回 0 余额空钱包（§1 资金语义：
//     「payment 没接」绝不能被后台读成「这个用户一分钱没有」）；
//  2. 主体门槛：mid<=0 在服务侧是硬错误（ErrInvalidMid，资金接口不支持 0 号账号），
//     在下传前点名字段拒掉；
//  3. currency 原样转达，空串交给服务按 Payment.DefaultCurrency 归一——网关不代填默认币种，
//     也不预查「这个币种支不支持」（服务只维护单一币种台账，隐式换汇是禁止的）。
//
// 契约缺口（只报不改）：GetWalletReply 只有 wallet 一位、没有 found，
// 因此 0 余额无法区分「从未开户」与「真的花光」；本投影不伪造 found。
func (l *PaymentWalletGetLogic) PaymentWalletGet(req *types.ParamPaymentWalletGet) (resp *types.PaymentWalletGetResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errPaymentServiceNotConfigured
	}
	if req == nil {
		return nil, errPaymentRequestMissing
	}
	if err := paymentPositive("mid", req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.GetWallet(l.ctx, &paymentrpc.GetWalletReq{
		Mid:      req.Mid,
		Currency: req.Currency,
	})
	if err != nil {
		l.Errorf("gateway/admin/paymentWalletGet: mid=%d currency=%q err=%v", req.Mid, req.Currency, err)
		return nil, err
	}
	return &types.PaymentWalletGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.PaymentWalletGetData{
			Wallet: paymentWalletToAPI(reply.GetWallet()),
		},
		TTL: 0,
	}, nil
}
