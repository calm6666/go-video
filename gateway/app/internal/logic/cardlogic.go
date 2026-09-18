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

type CardLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询单个用户名片
func NewCardLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CardLogic {
	return &CardLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询单个用户名片：聚合 account Card3 RPC。
func (l *CardLogic) Card(req *types.ParamMid) (resp *types.CardResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Card3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/card: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.CardResponse{
		Code:    0,
		Message: "ok",
		Data:    types.CardData{Card: toCard(reply.GetCard())},
		TTL:     0,
	}, nil
}
