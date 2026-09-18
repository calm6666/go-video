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

type InfoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询单个用户基础信息
func NewInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *InfoLogic {
	return &InfoLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询单个用户基础信息：聚合 account Info3 RPC。
func (l *InfoLogic) Info(req *types.ParamMid) (resp *types.InfoResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Info3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/info: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.InfoResponse{
		Code:    0,
		Message: "ok",
		Data:    types.InfoData{Info: toInfo(reply.GetInfo())},
		TTL:     0,
	}, nil
}
