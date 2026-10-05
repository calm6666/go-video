// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	membershiprpc "go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MembershipMemberGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单用户会员身份 + 当前档位可得权益码（found=false 表示从未开通）
func NewMembershipMemberGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembershipMemberGetLogic {
	return &MembershipMemberGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MembershipMemberGet 转发 membership GetMembership。
//
// found=false 是「该用户一行会员记录都没有」的真实读结论，不是错误，也不是默认值；
// 有过但已过期是 found=true，是否生效由响应里的 server_now 与 expire_at 比较决定，
// 网关不自己取时钟、也不把「读不到」折叠成未开通（§1 资金语义）。
// vip_type=0 表示「取当前生效的最高档」，由服务判定；granted_entitlements 只是详情页
// 的只读投影，不替代服务间权益判定 RPC（CheckEntitlement 刻意不开放后台路由）。
func (l *MembershipMemberGetLogic) MembershipMemberGet(req *types.ParamMembershipMemberGet) (resp *types.MembershipMemberResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errMembershipServiceNotConfigured
	}
	if req == nil {
		return nil, errMembershipRequestMissing
	}
	if err := membershipPositive("mid", req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.GetMembership(l.ctx, &membershiprpc.GetMembershipReq{
		Mid:     req.Mid,
		VipType: membershiprpc.VipType(req.VipType),
	})
	if err != nil {
		l.Errorf("gateway/admin/membershipMemberGet: mid=%d vip_type=%d err=%v", req.Mid, req.VipType, err)
		return nil, err
	}
	return &types.MembershipMemberResponse{
		Code:    0,
		Message: "ok",
		Data: types.MembershipMemberData{
			Found:               reply.GetFound(),
			Membership:          membershipMemberToAPI(reply.GetMembership()),
			ServerNow:           reply.GetServerNow(),
			GrantedEntitlements: membershipEntitlementsToAPI(reply.GetGrantedEntitlements()),
		},
		TTL: 0,
	}, nil
}
