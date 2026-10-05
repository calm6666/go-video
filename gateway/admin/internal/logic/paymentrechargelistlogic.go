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

type PaymentRechargeListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 充值台账分页（mid=0 跨用户）
func NewPaymentRechargeListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PaymentRechargeListLogic {
	return &PaymentRechargeListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PaymentRechargeList 转发 payment ListRecharges（充值台账分页）。
//
// 纯读面，没有任何写能力：本路由不结算、不取消，「钱到账」只在 /recharge/settle 与
// 用户自己端上的开单流程里发生（§5 资金台账归 payment）。
//
// 网关只挡形状：mid/page/size/from_ts/to_ts 非负、窗口不倒着给。三件事一个都不接管：
//   - mid=0 的跨用户查询「必须给完整时间窗且不超 Payment.MaxListWindowSeconds」是服务的
//     有界性判定（服务给的错误消息会点名配置项，网关自己复算只会多一套口径）；
//   - state=0 是「不按状态过滤」的合法哨兵，不是 UNSPECIFIED 错误；具体哪些状态能查由服务判；
//   - page/size 由服务归一并回显（page<1→1、size<=0→默认 20、超 MaxPageSize/MaxListOffset
//     直接拒绝而不是悄悄改小），total/page/size 一律照抄 reply。
//
// 空列表 + total=0 只在服务真查过才是结论；Count/List 任一失败都被原样上抛，
// 绝不折叠成空台账冒充「这个人没有充值记录」。
func (l *PaymentRechargeListLogic) PaymentRechargeList(req *types.ParamPaymentRechargeList) (resp *types.PaymentRechargeListResponse, err error) {
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
	if err := paymentTimeWindow(req.FromTs, req.ToTs); err != nil {
		return nil, err
	}
	if err := paymentNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := paymentNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.ListRecharges(l.ctx, &paymentrpc.ListRechargesReq{
		Mid:    req.Mid,
		State:  paymentrpc.RechargeState(req.State),
		FromTs: req.FromTs,
		ToTs:   req.ToTs,
		Page:   req.Page,
		Size:   req.Size,
	})
	if err != nil {
		l.Errorf("gateway/admin/paymentRechargeList: mid=%d state=%d from_ts=%d to_ts=%d page=%d size=%d err=%v",
			req.Mid, req.State, req.FromTs, req.ToTs, req.Page, req.Size, err)
		return nil, err
	}
	return &types.PaymentRechargeListResponse{
		Code:    0,
		Message: "ok",
		Data: types.PaymentRechargeListData{
			List:  paymentRechargesToAPI(reply.GetRecharges()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
