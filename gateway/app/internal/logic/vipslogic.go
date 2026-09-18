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

type VipsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量查询用户会员信息
func NewVipsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VipsLogic {
	return &VipsLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 批量查询用户会员信息：聚合 account Vips3 RPC。
func (l *VipsLogic) Vips(req *types.ParamMids) (resp *types.VipsResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Vips3(l.ctx, &accountrpc.MidsReq{Mids: req.Mids})
	if err != nil {
		l.Errorf("gateway/app/vips: err=%v", err)
		return nil, err
	}
	return &types.VipsResponse{
		Code:    0,
		Message: "ok",
		Data:    types.VipsData{Vips: toVipInfos(reply.GetVips())},
		TTL:     0,
	}, nil
}
