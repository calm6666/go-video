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

type PaymentRefundListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 退款台账分页（只回退款单，不发起退款）
func NewPaymentRefundListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PaymentRefundListLogic {
	return &PaymentRefundListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PaymentRefundList 转发 payment ListRefunds（退款台账分页）。
//
// **本路由不发起退款**：RefundPayment 的唯一驱动方是 POST /admin/order/refund/approve
// 背后的 trade-order 状态机（§5、§1：沙箱只支持退回余额，「原路退回渠道」在服务侧是
// not-configured，一律如实透出，网关不得美化成可重试错误）。这里只做已发生退款的对账读取。
//
// 网关只挡形状：mid/page/size 非负、payment_no 的列宽与状态取值交服务判。
//
// 契约缺口（只报不改）：payment.proto 的 ListRefundsReq 有 from_ts/to_ts（服务侧
// requireListBounds 靠它们给跨用户查询收口），但 admin.api 的 ParamPaymentRefundList
// 没有时间位，本轮禁止改 .api，所以下传时两侧恒为 0。后果是：mid=0 的跨用户退款审计
// 只有同时给 payment_no 才通得过服务（ListRefunds 对该情形放行），否则原样收到
// "payment: cross-user listing (mid=0) requires both from_ts and to_ts"。
// 网关**不**代填时间窗、也不把它折叠成空列表——那是服务的真实拒绝，按 InvalidArgument 透出。
func (l *PaymentRefundListLogic) PaymentRefundList(req *types.ParamPaymentRefundList) (resp *types.PaymentRefundListResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errPaymentServiceNotConfigured
	}
	if req == nil {
		return nil, errPaymentRequestMissing
	}
	if err := paymentNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := paymentNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := paymentNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.ListRefunds(l.ctx, &paymentrpc.ListRefundsReq{
		Mid:       req.Mid,
		PaymentNo: req.PaymentNo,
		Page:      req.Page,
		Size:      req.Size,
		// from_ts/to_ts 刻意不填：后台表单没有这两个位（见上面的契约缺口说明），
		// 网关凭空造一个窗口等于替运营决定「审计只看最近 N 天」。
	})
	if err != nil {
		l.Errorf("gateway/admin/paymentRefundList: mid=%d payment_no=%q page=%d size=%d err=%v",
			req.Mid, req.PaymentNo, req.Page, req.Size, err)
		return nil, err
	}
	return &types.PaymentRefundListResponse{
		Code:    0,
		Message: "ok",
		Data: types.PaymentRefundListData{
			List:  paymentRefundsToAPI(reply.GetRefunds()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
