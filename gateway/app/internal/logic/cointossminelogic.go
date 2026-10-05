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

type CoinTossMineLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的投币记录
func NewCoinTossMineLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinTossMineLogic {
	return &CoinTossMineLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinTossMine 只读本人投币记录（mid 必填且为正，不存在跨用户语义）。
// state 枚举位原样透传（0 不按状态过滤），是否只列生效记录由 coin 服务判定；
// page/page_size 原样透传，0 表示由服务取默认并裁剪。投币记录是流水，TTL 0。
func (l *CoinTossMineLogic) CoinTossMine(req *types.ParamCoinTossMine) (resp *types.CoinTossMineResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errors.New("coin service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Coin.ListMyTosses(l.ctx, &coinrpc.ListMyTossesReq{
		Mid:   req.Mid,
		State: coinrpc.TossState(req.State),
		Page:  int64(req.Page),
		Size:  int64(req.PageSize),
	})
	if err != nil {
		l.Errorf("gateway/app/coinTossMine: mid=%d state=%d page=%d err=%v", req.Mid, req.State, req.Page, err)
		return nil, err
	}
	return &types.CoinTossMineResponse{
		Code:    0,
		Message: "ok",
		Data: types.CoinTossMineData{
			Tosses:   coinTossesToAPI(reply.GetTosses()),
			Total:    reply.GetTotal(),
			Page:     reply.GetPage(),
			PageSize: reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
