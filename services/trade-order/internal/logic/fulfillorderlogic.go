package logic

import (
	"context"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type FulfillOrderLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewFulfillOrderLogic(ctx context.Context, svcCtx *svc.ServiceContext) *FulfillOrderLogic {
	return &FulfillOrderLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 执行/重试履约（幂等，可重试）
//
// 本方法只做参数收敛与回复渲染，真正的迁移顺序在 runFulfill：
// 事务内 CAS 到 FULFILLING（attempts+1 + 台账）→ 提交后调下游发放 → 成功再 CAS 到 FULFILLED。
// 顺序不能反：先调下游再落库，一旦本地事务回滚就会出现「权益已发出、订单还停在 PAID」，
// 下一轮重试就是一次超发。
//
// 下游的幂等键是 order_no 派生的（grant_<order_no> / coinpack_<order_no>），
// 所以重试同一单不会重复发放；下游回 duplicated=true 也按成功处理（幂等自愈）。
// 下游未配置或发放失败：订单保持 FULFILLING/FAILED 并把摘要写进 fulfill_detail，
// 绝不返回 fulfilled=true（AGENTS.md §9 禁止伪成功）。
func (l *FulfillOrderLogic) FulfillOrder(in *rpc.FulfillOrderReq) (*rpc.FulfillOrderReply, error) {
	orderNo := strings.TrimSpace(in.GetOrderNo())
	if orderNo == "" {
		return nil, model.ErrOrderNoRequired
	}
	operator := strings.TrimSpace(in.GetOperator())
	if operator == "" {
		operator = "system"
	}
	requestID := strings.TrimSpace(in.GetRequestId())

	res, err := runFulfill(l.ctx, l.svcCtx, l.Logger, orderNo, operator, requestID)
	if err != nil {
		// 失败时不回 reply：gRPC 语义下带错误的回复会被丢弃，
		// 而且订单已被推进到 FAILED（或留在 FULFILLING），调用方重查即可拿到真实状态。
		return nil, err
	}
	if res == nil {
		return nil, model.ErrFulfillNotAccepted
	}
	return &rpc.FulfillOrderReply{
		Fulfilled:  res.fulfilled,
		Duplicated: res.duplicated,
		Order:      toOrderInfo(res.order),
		Detail:     truncate(res.detail, maxDetailLen),
	}, nil
}
