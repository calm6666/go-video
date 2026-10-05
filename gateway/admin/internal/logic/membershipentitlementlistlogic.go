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

type MembershipEntitlementListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 权益码目录（enabled_only 可只看启用项）
func NewMembershipEntitlementListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembershipEntitlementListLogic {
	return &MembershipEntitlementListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MembershipEntitlementList 转发 membership ListEntitlements（权益码目录）。
//
// 无过滤条件、无分页：目录是全站能力闸的清单，服务一次性给全，网关不裁剪也不加
// 自己的排序（迟早漂移）。enabled_only=false 时下线的码仍然返回，因为判定侧要能
// 区分「码不存在」（调用方传错）与「码被运营关掉」（ENTITLEMENT_CODE_DISABLED），
// 这个区分是 membership 的结论，后台列表照抄才能和判定页面对得上。
func (l *MembershipEntitlementListLogic) MembershipEntitlementList(req *types.ParamMembershipEntitlementList) (resp *types.MembershipEntitlementListResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errMembershipServiceNotConfigured
	}
	if req == nil {
		return nil, errMembershipRequestMissing
	}
	reply, err := l.svcCtx.Membership.ListEntitlements(l.ctx, &membershiprpc.ListEntitlementsReq{
		EnabledOnly: req.EnabledOnly,
	})
	if err != nil {
		l.Errorf("gateway/admin/membershipEntitlementList: enabled_only=%t err=%v", req.EnabledOnly, err)
		return nil, err
	}
	return &types.MembershipEntitlementListResponse{
		Code:    0,
		Message: "ok",
		Data: types.MembershipEntitlementListData{
			List: membershipEntitlementsToAPI(reply.GetEntitlements()),
		},
		TTL: 0,
	}, nil
}
