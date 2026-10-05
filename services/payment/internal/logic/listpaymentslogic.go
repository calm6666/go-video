package logic

import (
	"context"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPaymentsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListPaymentsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPaymentsLogic {
	return &ListPaymentsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListPayments 支付台账分页。
//
// 有界性与 ListRecharges 同口径：mid=0 的跨用户查询必须给完整时间窗且不超
// Payment.MaxListWindowSeconds；size/offset 各有上限；查询失败上抛错误，
// 不把故障折叠成空台账。
func (l *ListPaymentsLogic) ListPayments(in *rpc.ListPaymentsReq) (*rpc.ListPaymentsReply, error) {
	cfg := l.svcCtx.Config.Payment
	if in.Mid < 0 {
		return nil, model.ErrInvalidMid
	}
	if err := requireListBounds(cfg, in.Mid, in.FromTs, in.ToTs); err != nil {
		return nil, err
	}
	page, size, err := normalizePage(cfg, in.Page, in.Size)
	if err != nil {
		return nil, err
	}

	q := model.PaymentListQuery{
		Mid:    in.Mid,
		State:  int32(in.State),
		Method: int32(in.Method),
		FromTs: in.FromTs,
		ToTs:   in.ToTs,
		Page:   listPage(page, size),
	}
	total, err := l.svcCtx.Models.Payment.Count(l.ctx, q)
	if err != nil {
		l.Errorf("payment/ListPayments: count mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	rows, err := l.svcCtx.Models.Payment.List(l.ctx, q)
	if err != nil {
		l.Errorf("payment/ListPayments: list mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.ListPaymentsReply{
		Payments: paymentInfos(rows),
		Total:    total,
		Page:     page,
		Size:     size,
	}, nil
}
