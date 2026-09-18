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

type V1InfosLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// v1 老客户端：批量查询用户基础信息
func NewV1InfosLogic(ctx context.Context, svcCtx *svc.ServiceContext) *V1InfosLogic {
	return &V1InfosLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// v1 老客户端：批量查询用户基础信息（Cards3 → V1Info 字段转换）。
func (l *V1InfosLogic) V1Infos(req *types.ParamMids) (resp *types.V1InfosResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Cards3(l.ctx, &accountrpc.MidsReq{Mids: req.Mids})
	if err != nil {
		l.Errorf("gateway/app/v1Infos: err=%v", err)
		return nil, err
	}
	return &types.V1InfosResponse{
		Code:    0,
		Message: "ok",
		Data:    toV1InfoMap(reply.GetCards()),
		TTL:     0,
	}, nil
}
