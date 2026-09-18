// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	accountrpc "go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CardsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量查询用户名片
func NewCardsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CardsLogic {
	return &CardsLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 批量查询用户名片：聚合 account Cards3 RPC。
func (l *CardsLogic) Cards(req *types.ParamMids) (resp *types.CardsResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Cards3(l.ctx, &accountrpc.MidsReq{Mids: req.Mids})
	if err != nil {
		l.Errorf("gateway/app/cards: err=%v", err)
		return nil, err
	}
	return &types.CardsResponse{
		Code:    0,
		Message: "ok",
		Data:    types.CardsData{Cards: toCards(reply.GetCards())},
		TTL:     0,
	}, nil
}
