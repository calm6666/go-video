// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	paymentrpc "go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PaymentFlowListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 资金流水分页（只增台账；充值/消费/退款/运营调整四类）
func NewPaymentFlowListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PaymentFlowListLogic {
	return &PaymentFlowListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PaymentFlowList 转发 payment ListFlows（资金流水分页）。
//
// 流水是 append-only 台账的唯一读出口：没有更新、没有删除，本路由也只有读。
// 需要「修正」时服务只接受再记一条 ADMIN_ADJUST 反向流水（/balance/adjust），
// 网关不得提供任何改写历史行的口子，也不把 delta_minor 取绝对值后「汇总」成余额。
//
// 网关只挡形状：mid/biz_type/page/size/from_ts/to_ts 非负、窗口不倒着给。
// biz_no 的列宽（服务侧 64 rune）与 biz_type 的合法集合（1..4）都在服务判：
// 服务对「查错类型」是**报错**而不是空列表（ErrInvalidBizType，注释写明「否则运营会把
// 查错类型当成没有资金变动」），所以网关更不该自己吞掉；biz_type=0 是「四类都要」的
// 合法哨兵，网关不代填。跨用户（mid=0）必须给完整窗口同样是服务的判定，见上一条路由。
// total/page/size 照抄服务回显。
func (l *PaymentFlowListLogic) PaymentFlowList(req *types.ParamPaymentFlowList) (resp *types.PaymentFlowListResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errPaymentServiceNotConfigured
	}
	if req == nil {
		return nil, errPaymentRequestMissing
	}
	if err := paymentNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := paymentNonNeg("biz_type", int64(req.BizType)); err != nil {
		return nil, err
	}
	if err := paymentTimeWindow(req.FromTs, req.ToTs); err != nil {
		return nil, err
	}
	if err := paymentNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := paymentNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.ListFlows(l.ctx, &paymentrpc.ListFlowsReq{
		Mid:     req.Mid,
		BizType: paymentrpc.FlowBizType(req.BizType),
		BizNo:   req.BizNo,
		FromTs:  req.FromTs,
		ToTs:    req.ToTs,
		Page:    req.Page,
		Size:    req.Size,
	})
	if err != nil {
		l.Errorf("gateway/admin/paymentFlowList: mid=%d biz_type=%d biz_no=%q from_ts=%d to_ts=%d page=%d size=%d err=%v",
			req.Mid, req.BizType, req.BizNo, req.FromTs, req.ToTs, req.Page, req.Size, err)
		return nil, err
	}
	return &types.PaymentFlowListResponse{
		Code:    0,
		Message: "ok",
		Data: types.PaymentFlowListData{
			List:  paymentFlowsToAPI(reply.GetFlows()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
