package logic

// 本文件是 trade-order logic 层的手写共享工具：投影转换、入参收敛、下游幂等键派生，
// 以及 CreateOrder 与 FulfillOrder 共用的履约内核 runFulfill。
// goctl 只生成每个方法的骨架（*logic.go），共享代码按 AGENTS.md §4 放在 internal/logic 内。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/idgen"
	coinrpc "go-video/services/coin/rpc"
	memberrpc "go-video/services/membership/rpc"
	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

// sqlSession 是事务会话的别名，让 logic 里的事务闭包签名读起来短一点。
type sqlSession = sqlx.Session

// 列宽与截断上限，与 deploy/migrations/trade-order/000001_create_trade_order_tables.sql 对齐。
// 超长会被 MySQL 严格模式拒绝（或更糟：静默截断成不可读的半句话），所以在入口就按 rune 裁。
const (
	orderNoPrefix     = "to" // order_no = "to_" + ULID，可读前缀便于人工排障
	maxOrderNoLen     = 64
	maxRequestIDLen   = 64
	maxDetailLen      = 500 // fulfill_detail
	maxReasonLen      = 500 // to_order_event.reason
	maxTitleLen       = 200
	maxOperatorLen    = 64
	maxPaymentNoLen   = 64
	maxGrantRefLen    = 64
	maxClientTraceLen = 64
)

// newOrderNo 用 common/idgen（ULID，时间有序 + 进程内单调）生成订单号。
// 全局唯一最终由 to_order.uniq_order_no 兜底：跨实例同毫秒碰撞会在建单时返回
// model.ErrOrderNoCollision，让调用方重试，而不是写坏数据。
func newOrderNo() (string, error) {
	return idgen.Prefixed(orderNoPrefix)
}

// deriveKey 从订单号派生下游幂等键。
//
// 必须确定性：履约/退款重试的是同一件事，若用每次请求的 request_id 透传给下游，
// 一次网络抖动就会造成「重复发放会员」或「重复退款」。
// purpose 区分同一订单上的不同动作，避免 grant 与 revoke 撞同一个键。
func deriveKey(purpose, orderNo string) string {
	return truncate(purpose+"_"+orderNo, maxRequestIDLen)
}

// truncate 按 rune 截断，避免把中文商品名切成半个字。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

// sanitize 把任意错误文本压成一列可存的单行摘要，并剥掉常见凭据/连接串片段。
//
// fulfill_detail 与 event.reason 会被运营页直接读到，绝不能落进 DSN、token、
// 手机号或堆栈（AGENTS.md §7 的脱敏口径同样适用于这里）。
func sanitize(s string) string {
	if s == "" {
		return ""
	}
	oneLine := strings.Join(strings.Fields(strings.NewReplacer(
		"\n", " ", "\r", " ", "\t", " ",
	).Replace(s)), " ")
	lower := strings.ToLower(oneLine)
	for _, marker := range []string{"@tcp(", "password", "token=", "secret", "bearer ", "-----begin"} {
		if strings.Contains(lower, marker) {
			return "downstream error message redacted"
		}
	}
	return truncate(oneLine, maxDetailLen)
}

// paginate 把 page/size 收敛成 offset/limit。
// size 越上限一律拒绝而不是裁剪：裁剪会让调用方误以为「这个用户就这么多订单」。
func paginate(page, size, maxSize int64) (offset, limit int64, err error) {
	if page < 0 || size < 0 {
		return 0, 0, model.ErrInvalidPage
	}
	if page == 0 {
		page = 1
	}
	if page < 1 {
		return 0, 0, model.ErrInvalidPage
	}
	if size == 0 {
		size = 20
	}
	if maxSize <= 0 {
		maxSize = 100
	}
	if size > maxSize {
		return 0, 0, fmt.Errorf("%w: size=%d, max=%d", model.ErrInvalidPage, size, maxSize)
	}
	return (page - 1) * size, size, nil
}

// toOrderInfo 把订单行投影成跨服务契约对象。
// 枚举数字与 rpc 枚举取值一一对应（model 常量即按 proto 编号定义），不做隐式映射。
func toOrderInfo(o *model.Order) *rpc.OrderInfo {
	if o == nil {
		return nil
	}
	return &rpc.OrderInfo{
		OrderNo:         o.OrderNo,
		Mid:             o.Mid,
		BizType:         rpc.OrderBizType(o.BizType),
		PlanId:          o.PlanID,
		PlanCode:        o.PlanCode,
		Title:           o.Title,
		Quantity:        o.Quantity,
		DurationDays:    o.DurationDays,
		CoinAmount:      o.CoinAmount,
		UnitPriceMinor:  o.UnitPriceMinor,
		AmountMinor:     o.AmountMinor,
		RefundedMinor:   o.RefundedMinor,
		Currency:        o.Currency,
		PayMethod:       rpc.PayMethod(o.PayMethod),
		State:           rpc.OrderState(o.State),
		FulfillState:    rpc.FulfillState(o.FulfillState),
		FulfillAttempts: o.FulfillAttempts,
		FulfillDetail:   o.FulfillDetail,
		PaymentNo:       o.PaymentNo,
		GrantRef:        o.GrantRef,
		ExpireAt:        o.ExpireAt,
		Platform:        rpc.Platform(o.Platform),
		ClientTraceId:   o.ClientTraceID,
		RequestId:       o.RequestID,
		Version:         o.Version,
		CreatedAt:       o.CreatedAt,
		UpdatedAt:       o.UpdatedAt,
		PaidAt:          o.PaidAt,
		FulfilledAt:     o.FulfilledAt,
		ClosedAt:        o.ClosedAt,
	}
}

func orderInfos(rows []*model.Order) []*rpc.OrderInfo {
	out := make([]*rpc.OrderInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, toOrderInfo(r))
	}
	return out
}

func toEventInfo(e *model.OrderEvent) *rpc.OrderEventInfo {
	if e == nil {
		return nil
	}
	return &rpc.OrderEventInfo{
		EventId:   e.EventID,
		OrderNo:   e.OrderNo,
		FromState: rpc.OrderState(e.FromState),
		ToState:   rpc.OrderState(e.ToState),
		Operator:  e.Operator,
		Reason:    e.Reason,
		Ctime:     e.Ctime,
	}
}

func eventInfos(rows []*model.OrderEvent) []*rpc.OrderEventInfo {
	out := make([]*rpc.OrderEventInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, toEventInfo(r))
	}
	return out
}

// requireOrder 按订单号读取，并在 mid>0 时校验归属。
//
// 越权与不存在返回同一个 ErrOrderNotFound（not-found 语义而不是 FORBIDDEN）：
// 否则调用方可以用错误码差异枚举出「哪些订单号存在」。
func (l *ServiceHelper) requireOrder(orderNo string, mid int64) (*model.Order, error) {
	if strings.TrimSpace(orderNo) == "" {
		return nil, model.ErrOrderNoRequired
	}
	o, err := l.svcCtx.Orders.FindByOrderNo(l.ctx, orderNo)
	if err != nil {
		return nil, err
	}
	if o == nil || (mid > 0 && o.Mid != mid) {
		return nil, ErrOrderNotFoundSilent
	}
	return o, nil
}

// ErrOrderNotFoundSilent 与 ErrOrderNotFound 同义，单独命名是为了让 logic 里
// 「查不到」与「不是你的」两条分支读起来是同一个出口（都不泄露订单是否存在）。
var ErrOrderNotFoundSilent = model.ErrOrderNotFound

// ServiceHelper 是 logic 之间共享依赖的窄封装：只带 ctx、svcCtx 与日志器，
// 让 runFulfill 这类内核函数不必接收十几个参数。
type ServiceHelper struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logger logx.Logger
}

func newHelper(ctx context.Context, svcCtx *svc.ServiceContext, logger logx.Logger) *ServiceHelper {
	return &ServiceHelper{ctx: ctx, svcCtx: svcCtx, logger: logger}
}

// fulfillResult 是一次履约尝试的结论。
type fulfillResult struct {
	order      *model.Order
	fulfilled  bool
	duplicated bool
	detail     string
}

// runFulfill 是履约内核，被 FulfillOrder 与 CreateOrder 的内联推进共用。
//
// 顺序不能改，这是本轮最关键的一段判定：
//  1. 事务内 CAS 到 FULFILLING（或同态重试）并把 fulfill_attempts+1，同事务写台账；
//  2. 提交后才调下游 —— 反过来会让下游发放成功但本地事务回滚，出现「钱给了权益没记上」；
//  3. 下游成功后再 CAS 到 FULFILLED 并落 grant_ref。
//
// 下游返回 duplicated=true（幂等重放）算成功：这正是重试不重复发放的自愈路径。
// 下游缺失或报错时订单保持 FULFILLING/FAILED 并写 fulfill_detail，绝不伪造成 FULFILLED。
func runFulfill(ctx context.Context, svcCtx *svc.ServiceContext, logger logx.Logger,
	orderNo, operator, requestID string,
) (*fulfillResult, error) {
	h := newHelper(ctx, svcCtx, logger)

	o, err := h.requireOrder(orderNo, 0)
	if err != nil {
		return nil, err
	}
	if o.State == model.StateFulfilled {
		return &fulfillResult{order: o, fulfilled: true, duplicated: true}, nil
	}
	if o.State != model.StatePaid && o.State != model.StateFulfilling {
		return nil, fmt.Errorf("%w: state=%s", model.ErrFulfillNotAccepted, model.StateName(o.State))
	}

	maxAttempts := svcCtx.Config.TradeOrder.FulfillMaxAttempts
	if maxAttempts > 0 && o.FulfillAttempts >= maxAttempts {
		return nil, fmt.Errorf("%w: attempts=%d, max=%d",
			model.ErrFulfillAttemptsExhausted, o.FulfillAttempts, maxAttempts)
	}
	// 同态重试才限频率：首次（PAID → FULFILLING）不该被上一条失败的时间戳挡住。
	if gap := svcCtx.Config.TradeOrder.MinSecondsBetweenFulfillRetry; gap > 0 && o.State == model.StateFulfilling {
		if model.NowUnix()-o.UpdatedAt < gap {
			return nil, fmt.Errorf("%w: last attempt %ds ago, min=%ds",
				model.ErrFulfillRetryTooSoon, model.NowUnix()-o.UpdatedAt, gap)
		}
	}
	if operator == "" {
		operator = "system"
	}
	if requestID == "" {
		requestID = deriveKey("fulfill", o.OrderNo)
	}

	attempt := o.FulfillAttempts + 1
	version := o.Version
	fromState := o.State

	// 步骤 1：CAS 到 FULFILLING + attempts+1 + 台账，一个事务。
	err = svcCtx.Transact(ctx, func(txCtx context.Context, tx sqlSession) error {
		ok, terr := svcCtx.Orders.TransitionTx(txCtx, tx, o.OrderNo, fromState, model.StateFulfilling, version,
			model.NewOrderUpdate().IncFulfillAttempts().FulfillState(model.FulfillPending))
		if terr != nil {
			return terr
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		_, terr = svcCtx.OrderEvents.InsertTx(txCtx, tx, &model.OrderEvent{
			OrderNo:   o.OrderNo,
			FromState: fromState,
			ToState:   model.StateFulfilling,
			Operator:  truncate(operator, maxOperatorLen),
			Reason:    truncate(fmt.Sprintf("fulfill attempt %d, biz_type=%d", attempt, o.BizType), maxReasonLen),
			RequestID: truncate(requestID, maxRequestIDLen),
		})
		return terr
	})
	if err != nil {
		return nil, err
	}
	version++ // 上一次 CAS 已把 version 自增，后续 CAS 基于新值。

	// 步骤 2：事务提交后才驱动下游。
	grantRef, dup, callErr := fulfillDownstream(ctx, svcCtx, logger, o, operator)
	if callErr != nil {
		detail := sanitize(callErr.Error())
		// 步骤 3a：失败 → CAS 到 FAILED，并把摘要写进 fulfill_detail（同事务写台账）。
		if uerr := svcCtx.Transact(ctx, func(txCtx context.Context, tx sqlSession) error {
			ok, terr := svcCtx.Orders.TransitionTx(txCtx, tx, o.OrderNo, model.StateFulfilling, model.StateFailed, version,
				model.NewOrderUpdate().FulfillState(model.FulfillFailed).FulfillDetail(detail))
			if terr != nil {
				return terr
			}
			if !ok {
				return model.ErrConcurrentUpdate
			}
			_, terr = svcCtx.OrderEvents.InsertTx(txCtx, tx, &model.OrderEvent{
				OrderNo:   o.OrderNo,
				FromState: model.StateFulfilling,
				ToState:   model.StateFailed,
				Operator:  truncate(operator, maxOperatorLen),
				Reason:    "fulfill failed: " + truncate(detail, maxReasonLen-16),
				RequestID: truncate(deriveKey("fulfillfail", o.OrderNo), maxRequestIDLen),
			})
			return terr
		}); uerr != nil {
			logger.Errorf("trade-order/fulfill: order %s 标记 FAILED 失败（需人工核对履约结果）: %v",
				o.OrderNo, uerr)
		}
		return &fulfillResult{order: nil, fulfilled: false, detail: detail}, callErr
	}

	// 步骤 3b：成功 → CAS 到 FULFILLED，落 grant_ref，清空历史失败摘要。
	if err = svcCtx.Transact(ctx, func(txCtx context.Context, tx sqlSession) error {
		ok, terr := svcCtx.Orders.TransitionTx(txCtx, tx, o.OrderNo, model.StateFulfilling, model.StateFulfilled, version,
			model.NewOrderUpdate().
				FulfillState(model.FulfillDone).
				GrantRef(truncate(grantRef, maxGrantRefLen)).
				FulfilledAt(model.NowUnix()).
				FulfillDetail(""))
		if terr != nil {
			return terr
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		_, terr = svcCtx.OrderEvents.InsertTx(txCtx, tx, &model.OrderEvent{
			OrderNo:   o.OrderNo,
			FromState: model.StateFulfilling,
			ToState:   model.StateFulfilled,
			Operator:  truncate(operator, maxOperatorLen),
			Reason:    truncate(fmt.Sprintf("fulfilled, grant_ref=%s%s", grantRef, dupMark(dup)), maxReasonLen),
			RequestID: truncate(deriveKey("fulfill", o.OrderNo), maxRequestIDLen),
		})
		return terr
	}); err != nil {
		// 下游已发放、本地未落 FULFILLED：这是最需要人看的一条日志。
		// 幂等键是 order_no 派生的，下一次重试会被下游判为 duplicated 而自愈。
		logger.Errorf("trade-order/fulfill: order %s 下游已发放(%s) 但本地推进 FULFILLED 失败: %v",
			o.OrderNo, grantRef, err)
		return &fulfillResult{fulfilled: false, detail: sanitize(err.Error())}, err
	}

	final, ferr := svcCtx.Orders.FindByOrderNo(ctx, o.OrderNo)
	if ferr != nil {
		return nil, ferr
	}
	return &fulfillResult{order: final, fulfilled: true, duplicated: dup, detail: ""}, nil
}

func dupMark(dup bool) string {
	if dup {
		return " (downstream duplicated)"
	}
	return ""
}

// transitionWithEvent 在一个事务里做「CAS 推进 + 写台账」。
//
// 所有状态推进必须走这一个入口：主表状态与台账行不同事务提交，
// 就会出现「订单是 REFUNDED 但没人知道是谁退的」这种不可审计的状态（AGENTS.md §5）。
// CAS 未命中返回 model.ErrConcurrentUpdate，调用方回读重试而不是直接覆盖。
func transitionWithEvent(ctx context.Context, svcCtx *svc.ServiceContext,
	orderNo string, from, to int32, expectedVersion int64, operator, requestID, reason string,
	upd *model.OrderUpdate,
) error {
	return svcCtx.Transact(ctx, func(txCtx context.Context, tx sqlSession) error {
		ok, err := svcCtx.Orders.TransitionTx(txCtx, tx, orderNo, from, to, expectedVersion, upd)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		_, err = svcCtx.OrderEvents.InsertTx(txCtx, tx, &model.OrderEvent{
			OrderNo:   orderNo,
			FromState: from,
			ToState:   to,
			Operator:  truncate(operator, maxOperatorLen),
			Reason:    truncate(reason, maxReasonLen),
			RequestID: truncate(requestID, maxRequestIDLen),
		})
		return err
	})
}

// ledgerDuplicated 用台账判定写接口是否已被同一个 request_id 应用过。
// 命中即幂等重放：调用方回 duplicated=true，不再动状态也不再调下游。
func ledgerDuplicated(ctx context.Context, svcCtx *svc.ServiceContext,
	orderNo, requestID string, toState int32,
) (bool, error) {
	e, err := svcCtx.OrderEvents.FindByRequestAndState(ctx, orderNo, requestID, toState)
	if err != nil {
		return false, err
	}
	return e != nil, nil
}

// fulfillDownstream 按业务类型驱动下游发放，返回履约产物引用。
// duplicated 透传下游的幂等重放标记：它代表「上一次已经发过了」，算成功。
func fulfillDownstream(ctx context.Context, svcCtx *svc.ServiceContext, logger logx.Logger,
	o *model.Order, operator string,
) (grantRef string, duplicated bool, err error) {
	switch o.BizType {
	case model.BizMembership:
		if svcCtx.Membership == nil {
			return "", false, model.ErrMembershipNotConfigured
		}
		vipType, perr := planVipType(ctx, svcCtx, o)
		if perr != nil {
			return "", false, perr
		}
		if o.DurationDays <= 0 {
			return "", false, fmt.Errorf("%w: duration_days=%d", model.ErrPlanTierMismatch, o.DurationDays)
		}
		reply, cerr := svcCtx.Membership.GrantMembership(ctx, &memberrpc.GrantMembershipReq{
			Mid:        o.Mid,
			VipType:    vipType,
			PlanId:     o.PlanID,
			DeltaDays:  o.DurationDays,
			Source:     memberrpc.GrantSource_GRANT_SOURCE_SANDBOX_PURCHASE,
			BizOrderNo: o.OrderNo,
			PaymentNo:  o.PaymentNo,
			Operator:   truncate(operator, maxOperatorLen),
			RequestId:  deriveKey("grant", o.OrderNo),
			Reason:     truncate("sandbox order fulfill "+o.OrderNo, maxReasonLen),
		})
		if cerr != nil {
			return "", false, fmt.Errorf("membership.GrantMembership: %w", cerr)
		}
		if reply == nil {
			return "", false, errors.New("membership.GrantMembership: empty reply")
		}
		logger.Infof("trade-order/fulfill: order %s granted membership grant_id=%d duplicated=%v",
			o.OrderNo, reply.GetGrantId(), reply.GetDuplicated())
		return "membership_grant:" + strconv.FormatInt(reply.GetGrantId(), 10), reply.GetDuplicated(), nil

	case model.BizCoinPack:
		if svcCtx.Coin == nil {
			return "", false, model.ErrCoinNotConfigured
		}
		if o.CoinAmount <= 0 {
			return "", false, fmt.Errorf("%w: coin_amount=%d", model.ErrPlanTierMismatch, o.CoinAmount)
		}
		reply, cerr := svcCtx.Coin.GrantCoin(ctx, &coinrpc.GrantCoinReq{
			Mid:       o.Mid,
			Delta:     int64(o.CoinAmount),
			FlowType:  coinrpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK,
			BizNo:     o.OrderNo,
			Operator:  "trade-order",
			RequestId: deriveKey("coinpack", o.OrderNo),
			Reason:    truncate("coin pack order "+o.OrderNo, maxReasonLen),
		})
		if cerr != nil {
			return "", false, fmt.Errorf("coin.GrantCoin: %w", cerr)
		}
		if reply == nil {
			return "", false, errors.New("coin.GrantCoin: empty reply")
		}
		logger.Infof("trade-order/fulfill: order %s granted coin flow_id=%d duplicated=%v",
			o.OrderNo, reply.GetFlowId(), reply.GetDuplicated())
		return "coin_flow:" + strconv.FormatInt(reply.GetFlowId(), 10), reply.GetDuplicated(), nil

	default:
		return "", false, fmt.Errorf("%w: biz_type=%d", model.ErrInvalidBizType, o.BizType)
	}
}

// planVipType 取回会员单要发放的档位。
//
// 契约缺口（已写进 README 与交付报告）：订单表与 OrderInfo 都没有 vip_type 字段，
// 而 membership.GrantMembership / RevokeMembership 都必须带 VipType，
// 因此这里回查 membership.GetPlan 拿档位。履约时套餐被改档会导致按新档发放，
// 正确解法是给 to_order 增列 vip_type 快照 —— 需要改 proto 与迁移，不在本轮授权范围内。
func planVipType(ctx context.Context, svcCtx *svc.ServiceContext, o *model.Order) (memberrpc.VipType, error) {
	if o.PlanID <= 0 {
		return memberrpc.VipType_VIP_TYPE_UNSPECIFIED, model.ErrPlanRequired
	}
	reply, err := svcCtx.Membership.GetPlan(ctx, &memberrpc.GetPlanReq{PlanId: o.PlanID})
	if err != nil {
		return memberrpc.VipType_VIP_TYPE_UNSPECIFIED, fmt.Errorf("membership.GetPlan: %w", err)
	}
	if !reply.GetFound() || reply.GetPlan() == nil {
		return memberrpc.VipType_VIP_TYPE_UNSPECIFIED, model.ErrPlanNotFound
	}
	vip := reply.GetPlan().GetVipType()
	if vip != memberrpc.VipType_VIP_TYPE_PREMIUM && vip != memberrpc.VipType_VIP_TYPE_PREMIUM_PLUS {
		return memberrpc.VipType_VIP_TYPE_UNSPECIFIED,
			fmt.Errorf("%w: plan vip_type=%d", model.ErrPlanTierMismatch, vip)
	}
	return vip, nil
}
