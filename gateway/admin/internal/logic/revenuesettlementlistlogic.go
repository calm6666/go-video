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

type RevenueSettlementListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 结算单分页（payout_state 恒 NOT_PAYABLE：本期无出金通道）
func NewRevenueSettlementListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueSettlementListLogic {
	return &RevenueSettlementListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueSettlementList 转发 creator-revenue ListSettlements（结算单检索，只读）。
//
// 只读路由，无 operator、不看会话。
//
// 网关只挡形状（mid/state/page/size 非负），判定一条都不接管（§5 结算台账归 creator-revenue）：
//   - period 的格式与「是不是未来周期」由服务判（ErrInvalidPeriod / ErrFuturePeriod），
//     网关不预先拒、不代填当前周期；
//   - 「period 与 mid 至少给一个」是服务的有界性判定（ErrQueryScopeRequired），逐字透出；
//   - state=0 是「不按状态过滤」，必须能看到 VOIDED —— 被强制作废的旧单正是审计要看的，
//     越界编号由服务回 ErrInvalidRuleState，网关不替它挑状态；
//   - page/size 越上限由服务拒绝，网关不夹。
//
// 结算语义如实转达：每行的 payout_state 是**服务给的值**（本期恒 NOT_PAYABLE），
// 网关不写死、不推断、不因为 state=CONFIRMED 就显示成「钱已付出」；
// amount_minor 是应计合计而不是已支付，cap_applied_minor 与 metric_count 一起转达，
// 让「这一单由几条台账折算、被封顶砍了多少」在后台能对齐。
func (l *RevenueSettlementListLogic) RevenueSettlementList(req *types.ParamRevenueSettlementList) (resp *types.RevenueSettlementListResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	if err := revenueNonNeg("mid", req.Mid); err != nil {
		return nil, err
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
	reply, err := l.svcCtx.CreatorRevenue.ListSettlements(l.ctx, &creatorrevenuerpc.ListSettlementsReq{
		Period: req.Period,
		Mid:    req.Mid,
		State:  creatorrevenuerpc.SettlementState(req.State),
		Page:   req.Page,
		Size:   req.Size,
	})
	if err != nil {
		l.Errorf("gateway/admin/revenueSettlementList: period=%q mid=%d state=%d page=%d size=%d err=%v",
			req.Period, req.Mid, req.State, req.Page, req.Size, err)
		return nil, err
	}
	return &types.RevenueSettlementListResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueSettlementListData{
			List:  revenueSettlementsToAPI(reply.GetSettlements()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
