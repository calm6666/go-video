// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	paymentrpc "go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type WalletRechargeOpenLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 开充值单（仅 SANDBOX 渠道）
func NewWalletRechargeOpenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WalletRechargeOpenLogic {
	return &WalletRechargeOpenLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// WalletRechargeOpen 只在沙箱台账上开一张待结算充值单，不动余额、不请求任何第三方支付。
// request_id 判空后原样透传（幂等键被改动就会重复建单）；amount_minor 与 channel 都不加工：
// 渠道取值（0/1 按 SANDBOX、其他由服务拒绝）和金额合法性都由 payment 判定，网关不做白名单、
// 不把 0 私自改成 1，也不做任何金额换算。
// 注意：payment.OpenRechargeReply 只有 duplicated + recharge，不带钱包快照，
// 而三条充值路由共用 WalletRechargeData，所以此处 Data.Wallet 保持零值——
// 网关不为此补一次旁路 GetWallet（写操作已成功时，附加读的失败会把成功伪装成失败）。
// 端上要看余额请读 /wallet/balance，不要用本路由返回的 wallet。TTL 0。
func (l *WalletRechargeOpenLogic) WalletRechargeOpen(req *types.ParamWalletRechargeOpen) (resp *types.WalletRechargeResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errors.New("payment service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireText("request_id", req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.OpenRecharge(l.ctx, &paymentrpc.OpenRechargeReq{
		Mid:           req.Mid,
		AmountMinor:   req.AmountMinor,
		Currency:      req.Currency,
		Channel:       paymentrpc.PayChannel(req.Channel),
		RequestId:     req.RequestId,
		ClientTraceId: req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/walletRechargeOpen: mid=%d amount_minor=%d err=%v", req.Mid, req.AmountMinor, err)
		return nil, err
	}
	return &types.WalletRechargeResponse{
		Code:    0,
		Message: "ok",
		Data: types.WalletRechargeData{
			Duplicated: reply.GetDuplicated(),
			Recharge:   walletRechargeToAPI(reply.GetRecharge()),
		},
		TTL: 0,
	}, nil
}
