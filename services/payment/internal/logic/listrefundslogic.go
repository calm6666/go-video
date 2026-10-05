package logic

import (
	"context"
	"errors"
	"strings"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRefundsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRefundsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRefundsLogic {
	return &ListRefundsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListRefunds 退款台账分页。
//
// 有界性与 ListPayments 同口径：跨用户（mid=0）查询必须自己收敛集合——给完整时间窗且不超过
// Payment.MaxListWindowSeconds；给了 payment_no 时按单号收敛，可以省掉时间窗。两者都没有即拒绝，
// 而不是无界扫 pm_refund；page/size 仍受 Payment.MaxPageSize/MaxListOffset 约束。
func (l *ListRefundsLogic) ListRefunds(in *rpc.ListRefundsReq) (*rpc.ListRefundsReply, error) {
	cfg := l.svcCtx.Config.Payment
	if in.Mid < 0 {
		return nil, model.ErrInvalidMid
	}
	paymentNo := strings.TrimSpace(in.PaymentNo)
	if err := requireMaxLength("payment_no", paymentNo, 40); err != nil {
		return nil, err
	}
	if err := requireListBounds(cfg, in.Mid, in.FromTs, in.ToTs); err != nil {
		if paymentNo == "" || !errors.Is(err, model.ErrListWindowRequired) {
			return nil, err
		}
	}
	page, size, err := normalizePage(cfg, in.Page, in.Size)
	if err != nil {
		return nil, err
	}

	q := model.RefundListQuery{
		Mid:       in.Mid,
		PaymentNo: paymentNo,
		FromTs:    in.FromTs,
		ToTs:      in.ToTs,
		Page:      listPage(page, size),
	}
	total, err := l.svcCtx.Models.Refund.Count(l.ctx, q)
	if err != nil {
		l.Errorf("payment/ListRefunds: count mid=%d payment_no=%s err=%v", in.Mid, paymentNo, err)
		return nil, err
	}
	rows, err := l.svcCtx.Models.Refund.List(l.ctx, q)
	if err != nil {
		l.Errorf("payment/ListRefunds: list mid=%d payment_no=%s err=%v", in.Mid, paymentNo, err)
		return nil, err
	}
	return &rpc.ListRefundsReply{
		Refunds: refundInfos(rows),
		Total:   total,
		Page:    page,
		Size:    size,
	}, nil
}
