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

type VipLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询单个用户会员信息
func NewVipLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VipLogic {
	return &VipLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询单个用户会员信息：聚合 account Vip3 RPC。
func (l *VipLogic) Vip(req *types.ParamMid) (resp *types.VipResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Vip3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/vip: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.VipResponse{
		Code:    0,
		Message: "ok",
		Data:    types.VipData{Vip: vipFromReply(reply)},
		TTL:     0,
	}, nil
}
