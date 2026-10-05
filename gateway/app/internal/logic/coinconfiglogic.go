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

type CoinConfigLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 投币限额参数（客户端不写死日限/单片上限）
func NewCoinConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinConfigLogic {
	return &CoinConfigLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinConfig 取服务端生效的投币参数（无入参，也不涉及具体用户）。
// 这些值改动很慢，客户端可缓存 300 秒（与 /coin/account 的当次额度快照分开口径）。
func (l *CoinConfigLogic) CoinConfig() (resp *types.CoinTossConfigResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errors.New("coin service not configured")
	}
	reply, err := l.svcCtx.Coin.GetTossConfig(l.ctx, &coinrpc.GetTossConfigReq{})
	if err != nil {
		l.Errorf("gateway/app/coinConfig: err=%v", err)
		return nil, err
	}
	return &types.CoinTossConfigResponse{
		Code:    0,
		Message: "ok",
		Data: types.CoinTossConfigData{
			DailyLimit:          reply.GetDailyLimit(),
			PerTargetLimit:      reply.GetPerTargetLimit(),
			CancelWindowSeconds: reply.GetCancelWindowSeconds(),
			MinBalanceToToss:    reply.GetMinBalanceToToss(),
			InitialBalance:      reply.GetInitialBalance(),
		},
		TTL: 300,
	}, nil
}
