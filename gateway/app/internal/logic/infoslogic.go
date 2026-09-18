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

type InfosLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量查询用户基础信息
func NewInfosLogic(ctx context.Context, svcCtx *svc.ServiceContext) *InfosLogic {
	return &InfosLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 批量查询用户基础信息：聚合 account Infos3 RPC。
func (l *InfosLogic) Infos(req *types.ParamMids) (resp *types.InfosResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Infos3(l.ctx, &accountrpc.MidsReq{Mids: req.Mids})
	if err != nil {
		l.Errorf("gateway/app/infos: err=%v", err)
		return nil, err
	}
	return &types.InfosResponse{
		Code:    0,
		Message: "ok",
		Data:    types.InfosData{Infos: toInfos(reply.GetInfos())},
		TTL:     0,
	}, nil
}
