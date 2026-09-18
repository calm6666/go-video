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

type InfoByNameLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按用户名批量查询用户基础信息
func NewInfoByNameLogic(ctx context.Context, svcCtx *svc.ServiceContext) *InfoByNameLogic {
	return &InfoByNameLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 按用户名批量查询：聚合 account InfosByName3 RPC。
func (l *InfoByNameLogic) InfoByName(req *types.ParamNames) (resp *types.InfosResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.InfosByName3(l.ctx, &accountrpc.NamesReq{Names: req.Names})
	if err != nil {
		l.Errorf("gateway/app/infoByName: err=%v", err)
		return nil, err
	}
	return &types.InfosResponse{
		Code:    0,
		Message: "ok",
		Data:    types.InfosData{Infos: toInfos(reply.GetInfos())},
		TTL:     0,
	}, nil
}
