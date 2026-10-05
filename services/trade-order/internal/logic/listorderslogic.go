package logic

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type ListOrdersLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListOrdersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListOrdersLogic {
	return &ListOrdersLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营面订单查询（有界窗口）
//
// 无界扫描在这张表上就是锁风险，因此本方法的规则是「拒绝」而不是「裁剪」：
//  1. 给了 order_no 或 payment_no 的点位查询走唯一/二级索引，不需要时间窗；
//  2. mid>0 限定单用户，同上；
//  3. 跨用户（mid=0）必须同时满足：至少一个过滤条件 + 完整时间窗
//     （from_ts>0、to_ts>=from_ts、跨度不超过 MaxListWindowSeconds）；
//  4. 调用方传的 max_window_seconds 只能收紧窗口，不能放宽服务侧上限。
func (l *ListOrdersLogic) ListOrders(in *rpc.ListOrdersReq) (*rpc.ListOrdersReply, error) {
	offset, size, err := paginate(in.GetPage(), in.GetSize(), l.svcCtx.Config.TradeOrder.MaxPageSize)
	if err != nil {
		return nil, err
	}
	state := int32(in.GetState())
	if state != 0 && !model.ValidState(state) {
		return nil, fmt.Errorf("%w: state=%d", model.ErrInvalidStateTransition, state)
	}
	bizType := int32(in.GetBizType())
	if bizType != 0 && !model.ValidBizType(bizType) {
		return nil, fmt.Errorf("%w: biz_type=%d", model.ErrInvalidBizType, bizType)
	}
	payMethod := int32(in.GetPayMethod())
	if payMethod != 0 && !model.ValidPayMethod(payMethod) {
		return nil, fmt.Errorf("%w: pay_method=%d", model.ErrInvalidPayMethod, payMethod)
	}
	orderNo := strings.TrimSpace(in.GetOrderNo())
	paymentNo := strings.TrimSpace(in.GetPaymentNo())
	fromTs, toTs := in.GetFromTs(), in.GetToTs()
	if fromTs > 0 && toTs > 0 && toTs < fromTs {
		return nil, fmt.Errorf("%w: from_ts=%d > to_ts=%d", model.ErrListWindowTooLarge, fromTs, toTs)
	}

	filtered := orderNo != "" || paymentNo != "" || state != 0 || bizType != 0 || payMethod != 0
	hasWindow := fromTs > 0 && toTs > 0

	if in.GetMid() <= 0 && !filtered && !hasWindow {
		return nil, model.ErrFilterRequired
	}
	if in.GetMid() <= 0 && !hasWindow && orderNo == "" && paymentNo == "" {
		// 只有状态/类型过滤而没有时间窗，同样是全表扫（状态是低基数列，命中不了多少选择性）。
		return nil, model.ErrListWindowRequired
	}
	if in.GetMid() <= 0 && hasWindow {
		maxWindow := l.svcCtx.Config.TradeOrder.MaxListWindowSeconds
		if want := in.GetMaxWindowSeconds(); want > 0 && want < maxWindow {
			maxWindow = want // 只允许收紧
		}
		if maxWindow > 0 && toTs-fromTs > maxWindow {
			return nil, fmt.Errorf("%w: window=%ds, max=%ds", model.ErrListWindowTooLarge, toTs-fromTs, maxWindow)
		}
	}

	rows, total, err := l.svcCtx.Orders.ListByFilter(l.ctx, &model.OrderFilter{
		Mid:       in.GetMid(),
		State:     state,
		BizType:   bizType,
		PayMethod: payMethod,
		OrderNo:   orderNo,
		PaymentNo: paymentNo,
		FromTs:    fromTs,
		ToTs:      toTs,
		Offset:    offset,
		Limit:     size,
	})
	if err != nil {
		return nil, err
	}
	return &rpc.ListOrdersReply{
		Orders: orderInfos(rows),
		Total:  total,
		Page:   normalizePage(in.GetPage()),
		Size:   size,
	}, nil
}
