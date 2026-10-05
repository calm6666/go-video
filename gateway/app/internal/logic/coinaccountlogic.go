// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	coinrpc "go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CoinAccountLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的硬币账户（含今日剩余额度）
func NewCoinAccountLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinAccountLogic {
	return &CoinAccountLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinAccount 读余额与今日额度。found=false 表示从未有过硬币账户（balance 为 0），
// 这是结论不是错误。额度（today_limit / per_target_limit / cancel_window_seconds）是服务侧
// 配置的当次快照，会被运营改动，因此余额类结论一律 TTL 0，端上不得缓存。
func (l *CoinAccountLogic) CoinAccount(req *types.ParamCoinAccount) (resp *types.CoinAccountResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errors.New("coin service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Coin.GetCoinAccount(l.ctx, &coinrpc.GetCoinAccountReq{
		Mid: req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/app/coinAccount: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.CoinAccountResponse{
		Code:    0,
		Message: "ok",
		Data: types.CoinAccountData{
			Found:   reply.GetFound(),
			Account: coinAccountToAPI(reply.GetAccount()),
		},
		TTL: 0,
	}, nil
}
