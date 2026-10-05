package logic

import (
	"context"

	"go-video/services/coin/internal/svc"
	"go-video/services/coin/model"
	"go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetCoinAccountLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetCoinAccountLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetCoinAccountLogic {
	return &GetCoinAccountLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 我的硬币账户（含今日额度）。
//
// 读路径不写库：没有账户行就回 found=false（proto 注释已锁定），
// 绝不在这里顺手建仓 —— 否则「看一眼余额」就会凭空产生一笔初始币发放流水，
// 一个用户被遍历查询一次就多送 5 枚，硬币发行量失控。
// 今日额度取自 cn_daily_toss 的当日行，缺行即 0（跨日天然重置，见 model/cn_daily_toss.go）。
func (l *GetCoinAccountLogic) GetCoinAccount(in *rpc.GetCoinAccountReq) (*rpc.GetCoinAccountReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	coin := l.svcCtx.Coin()

	acc, err := l.svcCtx.Accounts.FindOne(l.ctx, in.Mid)
	if err != nil {
		// 查询失败必须上抛：折叠成 found=false 会让客户端显示「余额 0」，
		// 用户以为币被扣光了，这是比报错严重得多的故障表现。
		return nil, err
	}

	today, err := l.svcCtx.Daily.FindOne(l.ctx, in.Mid, model.TodayDayNo())
	if err != nil {
		return nil, err
	}
	var todayTossed int64
	if today != nil {
		todayTossed = int64(today.Tossed)
	}

	return &rpc.GetCoinAccountReply{
		Found:   acc != nil,
		Account: accountInfo(in.Mid, acc, todayTossed, coin),
	}, nil
}
