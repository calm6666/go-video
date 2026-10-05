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

type MembershipGrantListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 授予/变更台账分页（mid=0 为跨用户查；追溯每一行时长是谁动的）
func NewMembershipGrantListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembershipGrantListLogic {
	return &MembershipGrantListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MembershipGrantList 转发 membership ListGrants（授予/变更台账）。
//
// mid=0 才是跨用户查询，这只有后台面才该拿到，因此本路由在读组但**不**放宽任何条件：
// 「哪些组合算无界扫描」「翻页上限」「biz_order_no 列宽」全由服务判定（§5 数据所有权）。
// from_ts/to_ts 是事件时间闭区间，单边 0 表示该侧不设界；total/page/size 一律照抄
// reply——台账只增不删，查不到就是没发生过，服务回的 total=0 与网关自造的空列表
// 长得一样，但只有前者是有依据的结论。
func (l *MembershipGrantListLogic) MembershipGrantList(req *types.ParamMembershipGrantList) (resp *types.MembershipGrantListResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errMembershipServiceNotConfigured
	}
	if req == nil {
		return nil, errMembershipRequestMissing
	}
	if err := membershipNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := membershipTimeWindow(req.FromTs, req.ToTs); err != nil {
		return nil, err
	}
	if err := membershipNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := membershipNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.ListGrants(l.ctx, &membershiprpc.ListGrantsReq{
		Mid:        req.Mid,
		VipType:    membershiprpc.VipType(req.VipType),
		Source:     membershiprpc.GrantSource(req.Source),
		BizOrderNo: req.BizOrderNo,
		FromTs:     req.FromTs,
		ToTs:       req.ToTs,
		Page:       req.Page,
		Size:       req.Size,
	})
	if err != nil {
		l.Errorf("gateway/admin/membershipGrantList: mid=%d vip_type=%d source=%d biz_order_no=%q from_ts=%d to_ts=%d page=%d size=%d err=%v",
			req.Mid, req.VipType, req.Source, req.BizOrderNo, req.FromTs, req.ToTs, req.Page, req.Size, err)
		return nil, err
	}
	return &types.MembershipGrantListResponse{
		Code:    0,
		Message: "ok",
		Data: types.MembershipGrantListData{
			List:  membershipGrantsToAPI(reply.GetGrants()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
