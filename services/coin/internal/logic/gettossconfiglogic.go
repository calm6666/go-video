package logic

import (
	"context"

	"go-video/services/coin/internal/svc"
	"go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTossConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetTossConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTossConfigLogic {
	return &GetTossConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 生效参数读取。
//
// 这里必须回显**运行时真正生效**的那份配置（svc 已做过 Sanitize 收敛），
// 而不是在响应里写死一套数字：客户端按本结果决定按钮与文案，写死会让
// 「改了配置但客户端还在按旧上限提示」变成常态故障（AGENTS.md §6）。
// 因此本方法读的是 svcCtx.Config.Coin —— 与 TossCoin 门禁用的是同一个结构体。
func (l *GetTossConfigLogic) GetTossConfig(in *rpc.GetTossConfigReq) (*rpc.GetTossConfigReply, error) {
	coin := l.svcCtx.Coin()

	return &rpc.GetTossConfigReply{
		DailyLimit:          coin.DailyLimit,
		PerTargetLimit:      coin.PerTargetLimit,
		CancelWindowSeconds: coin.CancelWindowSeconds,
		MinBalanceToToss:    coin.MinBalanceToToss,
		InitialBalance:      coin.InitialBalance,
	}, nil
}
