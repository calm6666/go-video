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

type MembershipPlanListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 套餐分页（含草稿与已下架；后台口径，不等于终端在售列表）
func NewMembershipPlanListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembershipPlanListLogic {
	return &MembershipPlanListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MembershipPlanList 转发 membership ListPlansAdmin。
//
// 后台口径（含 DRAFT 与 OFF_SALE）与终端口径（ListPlans 只回在售且平台可见）是两条
// 独立 RPC，网关不把前者当后者用，也不在这里过滤 state（§5：目录事实归 membership）。
// state/vip_type 的 0 是「不过滤」的合法哨兵、page/size 的 0 是「用服务默认页」，
// 只挡负数；超限裁剪由服务 clampPage 做并把实际生效值回显在 reply 里，
// 因此分页三元组照抄 reply 而不是回显请求值——后台要看见真正取到的那一页。
func (l *MembershipPlanListLogic) MembershipPlanList(req *types.ParamMembershipPlanList) (resp *types.MembershipPlanListResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errMembershipServiceNotConfigured
	}
	if req == nil {
		return nil, errMembershipRequestMissing
	}
	if err := membershipNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := membershipNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.ListPlansAdmin(l.ctx, &membershiprpc.ListPlansAdminReq{
		Page:    req.Page,
		Size:    req.Size,
		State:   membershiprpc.PlanSaleState(req.State),
		VipType: membershiprpc.VipType(req.VipType),
		Keyword: req.Keyword,
	})
	if err != nil {
		l.Errorf("gateway/admin/membershipPlanList: state=%d vip_type=%d keyword=%q page=%d size=%d err=%v",
			req.State, req.VipType, req.Keyword, req.Page, req.Size, err)
		return nil, err
	}
	return &types.MembershipPlanListResponse{
		Code:    0,
		Message: "ok",
		Data: types.MembershipPlanListData{
			List:  membershipPlansToAPI(reply.GetPlans()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
