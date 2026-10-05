// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RevenueEnrollmentListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 参与名单分页（ENROLLED/LEFT/SUSPENDED；含本人确认过的规则版本）
func NewRevenueEnrollmentListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueEnrollmentListLogic {
	return &RevenueEnrollmentListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueEnrollmentList 转发 creator-revenue ListEnrollments（参与名单检索，只读）。
//
// 只读路由，无 operator、不看会话；GetEnrollment（单个人的参与状态）刻意不开后台口，
// 那是创作者本人视角，归 gateway/app（见 admin.api 的「刻意不开的路由」段）。
//
// 网关只挡三位形状：state 非负、page/size 非负。判定一条都不接管
// （§5 参与关系归 creator-revenue 持有）：
//   - state=0 是「不按状态过滤」，必须能看到 LEFT 与 SUSPENDED —— 「这个人被暂停了几期」
//     本身就是运营要读的事实，网关预先排除等于把它藏起来；
//   - 「4 是不是一个参与状态」由服务回 ErrInvalidRuleState/合法集合判定，网关不挑值；
//   - 这一页里 operator 是 "user"（自助参加）还是 "gateway/admin:<id>"（代操作/处置）
//     原样转达，网关不改写也不折叠——参加是不是本人签的，只有这一位能回答。
//
// agreed_rule_version 逐行转达：没有它就无法证明参加者当时看到的是哪版单价。
// total/page/size 照抄服务回显；下游报错原样上抛，绝不投影成「没有作者参加」。
func (l *RevenueEnrollmentListLogic) RevenueEnrollmentList(req *types.ParamRevenueEnrollmentList) (resp *types.RevenueEnrollmentListResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	if err := revenueNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.ListEnrollments(l.ctx, &creatorrevenuerpc.ListEnrollmentsReq{
		State: creatorrevenuerpc.EnrollmentState(req.State),
		Page:  req.Page,
		Size:  req.Size,
	})
	if err != nil {
		l.Errorf("gateway/admin/revenueEnrollmentList: state=%d page=%d size=%d err=%v",
			req.State, req.Page, req.Size, err)
		return nil, err
	}
	return &types.RevenueEnrollmentListResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueEnrollmentListData{
			List:  revenueEnrollmentsToAPI(reply.GetEnrollments()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
