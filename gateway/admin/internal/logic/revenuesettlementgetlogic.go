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

type RevenueSettlementGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 结算单详情（含按来源拆的分项，让作者侧质疑时能一行行对）
func NewRevenueSettlementGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueSettlementGetLogic {
	return &RevenueSettlementGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueSettlementGet 转发 creator-revenue GetSettlement（单张结算单 + 按来源分项，只读）。
//
// 只读路由，无 operator、不看会话；这是争议复核的主证据口，越是有人质疑「这钱怎么算的」
// 时越要能立刻读到，因此网关一侧不加任何判定门槛。
//
// 网关只挡两位形状：settlement_no 必填（.api 无 optional，空串什么都定位不到）、
// mid 非负。刻意**不接管**（§5 结算台账归 creator-revenue）：
//   - 单号长度（MaxSettlementNoBytes）与格式由服务判，网关不预先裁；
//   - mid=0 是「不校验归属」的合法读法（后台复核本来就要跨人看单），非 0 时归属校验是
//     服务的 ErrForbidden ——「这张单不是你的」是真实拒绝，网关不折叠成 found=false，
//     也不换个 mid 重试；
//   - found=false（单号不存在）与分项为空都是**真实读结论**，不是错误，网关不加错误也不补条目。
//
// 分项（items）与合计**不做配平**：分项 amount_minor 之和与 settlement.amount_minor 之间的
// 差额由 cap_applied_minor 解释，网关既不补「其它」行、也不因为对不上就少回几条。
// payout_state 照抄服务值（本期恒 NOT_PAYABLE），确认位 confirmed_at/confirmed_by 原样转达：
// 确认=金额冻结，不等于钱已付出。
func (l *RevenueSettlementGetLogic) RevenueSettlementGet(req *types.ParamRevenueSettlementGet) (resp *types.RevenueSettlementGetResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	if err := requireNonEmpty("settlement_no", req.SettlementNo); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.GetSettlement(l.ctx, &creatorrevenuerpc.GetSettlementReq{
		SettlementNo: req.SettlementNo,
		Mid:          req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/admin/revenueSettlementGet: settlement_no=%q mid=%d err=%v", req.SettlementNo, req.Mid, err)
		return nil, err
	}
	return &types.RevenueSettlementGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueSettlementGetData{
			Found:      reply.GetFound(),
			Settlement: revenueSettlementToAPI(reply.GetSettlement()),
			Items:      revenueSettlementItemsToAPI(reply.GetItems()),
		},
		TTL: 0,
	}, nil
}
