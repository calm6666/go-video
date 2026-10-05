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

type WalletChannelsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 渠道能力自述（sandbox_only/real_money，让端上显式知道不是真实资金）
func NewWalletChannelsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WalletChannelsLogic {
	return &WalletChannelsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// WalletChannels 把「本项目只有沙箱渠道、不产生真实资金」说成可查询的事实而不是 README 里的一句话。
// sandbox_only 与每个渠道的 real_money 必须原样透出（即使全为 false 也不省略、不改写），
// 端上据此显式标注「非真实资金」。渠道是否可用由 payment 判定，网关不做二次过滤。
func (l *WalletChannelsLogic) WalletChannels() (resp *types.WalletChannelsResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errors.New("payment service not configured")
	}
	reply, err := l.svcCtx.Payment.DescribeChannels(l.ctx, &paymentrpc.DescribeChannelsReq{})
	if err != nil {
		l.Errorf("gateway/app/walletChannels: err=%v", err)
		return nil, err
	}
	return &types.WalletChannelsResponse{
		Code:    0,
		Message: "ok",
		Data: types.WalletChannelsData{
			SandboxOnly:     reply.GetSandboxOnly(),
			Channels:        walletChannelsToAPI(reply.GetChannels()),
			CurrencyDefault: reply.GetCurrencyDefault(),
		},
		TTL: 0,
	}, nil
}
