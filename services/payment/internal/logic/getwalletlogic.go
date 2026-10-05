package logic

import (
	"context"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetWalletLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetWalletLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetWalletLogic {
	return &GetWalletLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetWallet 查询余额。
//
// 口径：
//   - 只读，不建行——账户不存在就是 0 余额（proto WalletInfo 注释要求），
//     建行只发生在真正要动钱的写入路径里；
//   - frozen_minor 恒为 0，可用余额就是 balance_minor；
//   - 读穿 MySQL，不走 Redis：缓存余额会带来「按旧余额判定」的重复入账风险；
//   - 数据库读失败一律上抛错误，绝不折叠成 0 余额冒充「查过了」。
func (l *GetWalletLogic) GetWallet(in *rpc.GetWalletReq) (*rpc.GetWalletReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	currency, err := resolveCurrency(l.svcCtx.Config.Payment, in.Currency)
	if err != nil {
		return nil, err
	}

	wallet, err := l.svcCtx.Models.Wallet.FindOne(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("payment/GetWallet: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.GetWalletReply{Wallet: walletInfo(wallet, in.Mid, currency)}, nil
}
