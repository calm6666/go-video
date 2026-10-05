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

type PaymentListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 支付台账分页（按状态/支付方式/时间窗）
func NewPaymentListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PaymentListLogic {
	return &PaymentListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PaymentList 转发 payment ListPayments（支付台账分页）。
//
// 这一页只读，而且是**订单侧结论的资金对照面**：支付单由 trade-order 通过 CreatePayment
// 驱动（§5 商业订单状态机归 trade-order），后台既不能在这里建单、关单，也不能退款
// （/refund/list 同理只回退款单）。刻意不开 GetPayment 的后台路由，列表已按 mid/时间窗
// 收敛，避免形成两套读取口径（见 admin.api payment 段注释）。
//
// 网关只挡形状：mid/state/method/page/size/from_ts/to_ts 非负、窗口不倒着给；
// state=0/method=0 是「不按该位过滤」的合法哨兵，网关不代填也不改成 UNSPECIFIED 报错。
// 有界性（跨用户必须给窗口）、页上限与状态机取值合法性都在服务侧，
// total/page/size 照抄 reply。amount_minor/refunded_minor 原样转达，
// 网关不比对「退了多少还剩多少」（ErrRefundExceedsAmount 的判定归 payment）。
//
// 契约缺口（只报不改）：PaymentInfo.operator/remark 两位审计回显在 admin.api 的
// PaymentPaymentItem 里没有落点，本列表看不到是谁把单子推到当前态。
func (l *PaymentListLogic) PaymentList(req *types.ParamPaymentList) (resp *types.PaymentListResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errPaymentServiceNotConfigured
	}
	if req == nil {
		return nil, errPaymentRequestMissing
	}
	if err := paymentNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := paymentNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := paymentNonNeg("method", int64(req.Method)); err != nil {
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
	reply, err := l.svcCtx.Payment.ListPayments(l.ctx, &paymentrpc.ListPaymentsReq{
		Mid:    req.Mid,
		State:  paymentrpc.PaymentState(req.State),
		Method: paymentrpc.PayMethod(req.Method),
		FromTs: req.FromTs,
		ToTs:   req.ToTs,
		Page:   req.Page,
		Size:   req.Size,
	})
	if err != nil {
		l.Errorf("gateway/admin/paymentList: mid=%d state=%d method=%d from_ts=%d to_ts=%d page=%d size=%d err=%v",
			req.Mid, req.State, req.Method, req.FromTs, req.ToTs, req.Page, req.Size, err)
		return nil, err
	}
	return &types.PaymentListResponse{
		Code:    0,
		Message: "ok",
		Data: types.PaymentListData{
			List:  paymentLedgersToAPI(reply.GetPayments()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
