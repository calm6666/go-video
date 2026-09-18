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

type V1VipLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// v1 老客户端：查询用户会员信息
func NewV1VipLogic(ctx context.Context, svcCtx *svc.ServiceContext) *V1VipLogic {
	return &V1VipLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// v1 老客户端：查询用户会员信息（Vip3 → V1Vip 字段转换）。
func (l *V1VipLogic) V1Vip(req *types.ParamMid) (resp *types.V1VipResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Vip3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/v1Vip: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	vip := &accountrpc.VipInfo{}
	if reply != nil {
		vip.Type = reply.Type
		vip.Status = reply.Status
		vip.DueDate = reply.DueDate
		vip.VipPayType = reply.VipPayType
	}
	return &types.V1VipResponse{
		Code:    0,
		Message: "ok",
		Data:    toV1Vip(vip),
		TTL:     0,
	}, nil
}
