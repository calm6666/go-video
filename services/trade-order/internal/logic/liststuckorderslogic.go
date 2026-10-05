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

type ListStuckOrdersLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListStuckOrdersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListStuckOrdersLogic {
	return &ListStuckOrdersLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// defaultStuckStates 是 proto 注释点名的三档：卡在受理/支付结论/履约中间的订单。
// CANCELLED/REFUNDED/REFUND_REJECTED 是已结案，扫出来也没有自愈动作可做，
// 允许它们只会诱导 cron 去重试已结案件，所以直接拒绝（见 stuckAllowedStates）。
var defaultStuckStates = []int32{model.StatePaying, model.StatePaid, model.StateFulfilling}

// stuckAllowedStates 是「还能被推进」的状态集合：都有出边，超时才有意义。
// 特别地，FAILED 与 REFUND_APPROVED 也在集合内：
//   - FAILED：履约尝试耗尽后停在人工态，巡检必须还能把它捞出来（本轮契约没有
//     FAILED → FULFILLING 这条边，所以这里只负责暴露，不做重试，见 README 缺口）；
//   - REFUND_APPROVED：款已退、权益未回收的差异单，靠再次 ApproveRefund 自愈。
var stuckAllowedStates = map[int32]bool{
	model.StateCreated:         true,
	model.StatePaying:          true,
	model.StatePaid:            true,
	model.StateFulfilling:      true,
	model.StateFailed:          true,
	model.StateRefundRequested: true,
	model.StateRefundApproved:  true,
}

// cron：卡单扫描
//
// 这是 cron / 运营巡检的只读入口，本身不动任何状态：
// 扫出来的单由调用方按状态分别投递给 FulfillOrder / BindPayment / ApproveRefund，
// 那些写入口各自带幂等键与限频（MinSecondsBetweenFulfillRetry），
// 所以这里刻意不做「顺手重试」—— 扫描与修复分离，一次扫描失败不会造成资金动作。
//
// 口径：
//   - older_than_seconds <= 0 时退化为 TradeOrder.OrderExpireSeconds（未支付单的关单时限，
//     与 PAYING 卡单的自然阈值一致），不会退化成「全表扫」。
//   - states 为空时用 PAYING/PAID/FULFILLING；显式传入时逐个校验必须是已定义状态，
//     且属于 stuckAllowedStates，否则 InvalidArgument（拒绝静默裁剪，让 cron 作者立刻发现写错枚举）。
//   - limit 有硬上限 TradeOrder.StuckScanMaxLimit；越上限拒绝而不是裁剪，
//     裁剪会让 cron 误以为「就这些单」，把剩下的漏掉。
//   - 结果按 updated_at 升序（最老的先修），空结果返回非 nil 切片。
func (l *ListStuckOrdersLogic) ListStuckOrders(in *rpc.ListStuckOrdersReq) (*rpc.ListStuckOrdersReply, error) {
	olderThan := in.GetOlderThanSeconds()
	if olderThan <= 0 {
		olderThan = l.svcCtx.Config.TradeOrder.OrderExpireSeconds
	}
	if olderThan <= 0 {
		olderThan = 1800 // 配置被清空时的兜底，与 config 的 default 一致，绝不当成 0（=全表）。
	}

	states := defaultStuckStates
	if len(in.GetStates()) > 0 {
		states = make([]int32, 0, len(in.GetStates()))
		for _, s := range in.GetStates() {
			v := int32(s)
			if !model.ValidState(v) {
				return nil, fmt.Errorf("%w: state=%d 不是已定义状态", model.ErrStuckStateNotAllowed, v)
			}
			if !stuckAllowedStates[v] {
				return nil, fmt.Errorf("%w: state=%s 已结案，无自愈动作",
					model.ErrStuckStateNotAllowed, model.StateName(v))
			}
			states = append(states, v)
		}
	}

	limit := in.GetLimit()
	maxLimit := l.svcCtx.Config.TradeOrder.StuckScanMaxLimit
	if maxLimit <= 0 {
		maxLimit = 200 // 配置缺项时也要有界：无界扫描会锁表。
	}
	if limit <= 0 {
		limit = maxLimit
	}
	if limit > maxLimit {
		return nil, fmt.Errorf("%w: limit=%d, max=%d", model.ErrInvalidPage, limit, maxLimit)
	}

	rows, err := l.svcCtx.Orders.ListStuck(l.ctx, states, model.NowUnix()-olderThan, limit)
	if err != nil {
		return nil, err
	}
	orders := orderInfos(rows)
	l.Infof("trade-order/ListStuckOrders: states=%s older_than=%ds limit=%d matched=%d",
		stuckStateNames(states), olderThan, limit, len(orders))
	return &rpc.ListStuckOrdersReply{Orders: orders}, nil
}

// stuckStateNames 把状态数字翻成可读名进日志（日志里出现裸数字对排障没用）。
func stuckStateNames(states []int32) string {
	names := make([]string, 0, len(states))
	for _, s := range states {
		names = append(names, model.StateName(s))
	}
	return strings.Join(names, ",")
}
