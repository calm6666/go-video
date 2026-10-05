// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	membershiprpc "go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MbMyLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的会员状态（含服务端时钟与可得权益码）
func NewMbMyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MbMyLogic {
	return &MbMyLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MbMy 读「我的会员」。found=false 表示从未开通，是结论不是错误，照常返回 Code:0。
// server_now 必须投影：端上用它和 expire_at 比较判过期，网关不替客户端取时钟、
// 也不把「已过期」折成 found=false（那是两种文案）。
// Entitlements 只是当前档位可得权益码的展示投影，真正的准入判定走 /membership/entitlements。
// 授权类结论不允许端上缓存，TTL 固定 0。
func (l *MbMyLogic) MbMy(req *types.ParamMbMy) (resp *types.MbMyResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errors.New("membership service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.GetMembership(l.ctx, &membershiprpc.GetMembershipReq{
		Mid:     req.Mid,
		VipType: membershiprpc.VipType(req.VipType),
	})
	if err != nil {
		l.Errorf("gateway/app/mbMy: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.MbMyResponse{
		Code:    0,
		Message: "ok",
		Data: types.MbMyData{
			Found:        reply.GetFound(),
			Membership:   mbMembershipToAPI(reply.GetMembership()),
			ServerNow:    reply.GetServerNow(),
			Entitlements: mbEntitlementBriefsToAPI(reply.GetGrantedEntitlements()),
		},
		TTL: 0,
	}, nil
}
