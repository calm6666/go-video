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

type WalletBalanceLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的余额（现金台账，沙箱）
func NewWalletBalanceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WalletBalanceLogic {
	return &WalletBalanceLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// WalletBalance 读 payment 的沙箱现金台账（与硬币两套账，不互换）。
// currency 原样透传（空串由服务按默认币种 CNY 处理，网关不填死）。
// frozen_minor 恒为 0 是服务侧事实，网关不把它当可用余额、也不做「余额-冻结」的减法。
// 余额是可随时变化的结论，TTL 固定 0：端上缓存余额等于让它按旧值下单。
func (l *WalletBalanceLogic) WalletBalance(req *types.ParamWalletBalance) (resp *types.WalletBalanceResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errors.New("payment service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.GetWallet(l.ctx, &paymentrpc.GetWalletReq{
		Mid:      req.Mid,
		Currency: req.Currency,
	})
	if err != nil {
		l.Errorf("gateway/app/walletBalance: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.WalletBalanceResponse{
		Code:    0,
		Message: "ok",
		Data: types.WalletBalanceData{
			Wallet: walletToAPI(reply.GetWallet()),
		},
		TTL: 0,
	}, nil
}
