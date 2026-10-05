package logic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	memberrpc "go-video/services/membership/rpc"
	paymentrpc "go-video/services/payment/rpc"
	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

type CreateOrderLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateOrderLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateOrderLogic {
	return &CreateOrderLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 下单（服务端重算金额，沙箱下内联受理）
//
// 本方法是全站唯一的建单入口，四条硬口径：
//  1. 金额只信服务端向 membership.GetPlan 重算出来的值：
//     unit_price = prom_price_minor>0 ? prom_price_minor : price_minor，
//     amount = unit_price × quantity；客户端上报的 amount_minor 只做一致性校验，
//     不为 0 且与重算值不一致直接拒绝（错误文本里给出服务端值），绝不参与扣款。
//  2. 套餐必须 ON_SALE、对本端可见、档位与 biz_type 匹配，三者缺一不建单。
//  3. 下游缺失分级失败：membership 未配置不取价 → 不建单；payment 未配置 → 不建单。
//     宁可拒绝这次下单，也不落一张「看起来已支付」或「永远付不了」的订单。
//  4. 内联推进 CREATED→PAYING→PAID→FULFILLING→FULFILLED 每一步都单独 CAS + 写台账，
//     不允许一步跳到 FULFILLED（否则状态机不可审计）。
func (l *CreateOrderLogic) CreateOrder(in *rpc.CreateOrderReq) (*rpc.CreateOrderReply, error) {
	cfg := l.svcCtx.Config.TradeOrder

	mid := in.GetMid()
	if mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	bizType := int32(in.GetBizType())
	if !model.ValidBizType(bizType) {
		return nil, fmt.Errorf("%w: biz_type=%d", model.ErrInvalidBizType, bizType)
	}
	if in.GetPlanId() <= 0 && strings.TrimSpace(in.GetPlanCode()) == "" {
		return nil, model.ErrPlanRequired
	}
	requestID := strings.TrimSpace(in.GetRequestId())
	if requestID == "" {
		return nil, model.ErrRequestIdRequired
	}
	payMethod := int32(in.GetPayMethod())
	if !model.ValidPayMethod(payMethod) {
		return nil, fmt.Errorf("%w: pay_method=%d", model.ErrInvalidPayMethod, payMethod)
	}
	platform := int32(in.GetPlatform())
	if !model.ValidPlatform(platform) {
		return nil, fmt.Errorf("%w: platform=%d", model.ErrInvalidPlatform, platform)
	}
	clientTraceID := truncate(in.GetClientTraceId(), maxClientTraceLen)

	quantity := in.GetQuantity()
	if quantity <= 0 {
		quantity = 1 // proto 注释：<=0 视为 1
	}
	if cfg.MaxQuantityPerOrder > 0 && quantity > cfg.MaxQuantityPerOrder {
		// 按上限裁剪（proto 注释「上限由服务侧配置裁剪」）。裁剪后金额与客户端上报值不冲突时
		// 才允许成交：下面的金额对撞会把「按 11 份的钱买 10 份」这类请求挡掉。
		l.Infof("trade-order/CreateOrder: mid=%d quantity=%d clamped to max=%d", mid, quantity, cfg.MaxQuantityPerOrder)
		quantity = cfg.MaxQuantityPerOrder
	}

	// --- 幂等重放：同一 request_id 只可能有一张订单 ---
	existing, err := l.svcCtx.Orders.FindByRequestID(l.ctx, requestID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		// 首次请求可能停在 CREATED/PAYING（进程崩溃、payment 抖动）。
		// 这里重新驱动受理而不是直接回 duplicated：下游幂等键由 order_no 派生，
		// 重驱动不会重复扣钱，但能让用户重试真正自愈。
		res, rerr := l.driveAccept(existing)
		if rerr != nil {
			return nil, rerr
		}
		return &rpc.CreateOrderReply{
			Duplicated:   true,
			Order:        toOrderInfo(res.order),
			Accepted:     res.accepted,
			RejectReason: res.rejectReason,
		}, nil
	}

	// --- 下游前置：取价与受理都依赖，缺了就不建单 ---
	if l.svcCtx.Membership == nil {
		return nil, model.ErrMembershipNotConfigured
	}
	if l.svcCtx.Payment == nil {
		return nil, model.ErrPaymentNotConfigured
	}

	// --- 服务端重算金额 ---
	quote, err := l.priceFromPlan(in, bizType, platform, quantity, cfg.DefaultCurrency)
	if err != nil {
		return nil, err
	}
	unitPrice := quote.unitPrice
	durationDays := quote.durationDays
	coinAmount := quote.coinAmount
	amount := unitPrice * int64(quantity)
	if amount <= 0 || amount > math.MaxInt64/1024 {
		return nil, fmt.Errorf("%w: amount=%d", model.ErrPlanPriceUnavailable, amount)
	}
	if reported := in.GetAmountMinor(); reported != 0 && reported != amount {
		// 防前端改价：文本里给出服务端重算值，调用方可以据此纠正而不是继续猜。
		return nil, fmt.Errorf("%w: server_amount_minor=%d client_amount_minor=%d unit_price_minor=%d quantity=%d",
			model.ErrAmountMismatch, amount, reported, unitPrice, quantity)
	}

	orderNo, err := newOrderNo()
	if err != nil {
		return nil, fmt.Errorf("trade-order: generate order_no: %w", err)
	}
	now := model.NowUnix()
	expireAt := now
	if cfg.OrderExpireSeconds > 0 {
		expireAt += cfg.OrderExpireSeconds
	}

	order := &model.Order{
		OrderNo:        truncate(orderNo, maxOrderNoLen),
		RequestID:      truncate(requestID, maxRequestIDLen),
		Mid:            mid,
		BizType:        bizType,
		PlanID:         quote.planID,
		PlanCode:       quote.planCode,
		Title:          quote.title,
		Quantity:       quantity,
		DurationDays:   durationDays,
		CoinAmount:     coinAmount,
		UnitPriceMinor: unitPrice,
		AmountMinor:    amount,
		Currency:       quote.currency,
		PayMethod:      payMethod,
		State:          model.StateCreated,
		FulfillState:   model.FulfillPending,
		PaymentNo:      "",
		ExpireAt:       expireAt,
		Platform:       platform,
		ClientTraceID:  clientTraceID,
		Version:        1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// --- 建单：主表行 + 出生台账，同事务 ---
	created, replayed, err := l.insertOrder(order)
	if err != nil {
		return nil, err
	}

	res, err := l.driveAccept(created)
	if err != nil {
		return nil, err
	}
	return &rpc.CreateOrderReply{
		// 并发撞 uniq_request_id 时回查到的其实是别人的单：必须如实回 duplicated，
		// 否则调用方无法区分「我建成了」与「我晚了一步，拿到的是首单」。
		Duplicated:   replayed,
		Order:        toOrderInfo(res.order),
		Accepted:     res.accepted,
		RejectReason: res.rejectReason,
	}, nil
}

// insertOrder 写入订单与「出生」台账行（from_state=0 → CREATED）。
// 命中 uniq_request_id 说明并发重复建单：回查首次订单并按重放返回（replayed=true）。
func (l *CreateOrderLogic) insertOrder(order *model.Order) (*model.Order, bool, error) {
	err := l.svcCtx.Transact(l.ctx, func(txCtx context.Context, tx sqlSession) error {
		if _, ierr := l.svcCtx.Orders.InsertTx(txCtx, tx, order); ierr != nil {
			return ierr
		}
		_, ierr := l.svcCtx.OrderEvents.InsertTx(txCtx, tx, &model.OrderEvent{
			OrderNo:   order.OrderNo,
			FromState: 0, // 建单前不存在状态，0 = UNSPECIFIED 是这一行的合法取值
			ToState:   model.StateCreated,
			Operator:  "user",
			Reason: truncate(fmt.Sprintf("order created, biz_type=%d quantity=%d amount_minor=%d",
				order.BizType, order.Quantity, order.AmountMinor), maxReasonLen),
			RequestID: order.RequestID,
		})
		return ierr
	})
	if err != nil {
		if errors.Is(err, model.ErrDuplicateRequest) {
			first, ferr := l.svcCtx.Orders.FindByRequestID(l.ctx, order.RequestID)
			if ferr != nil {
				return nil, false, ferr
			}
			if first == nil {
				return nil, false, err
			}
			return first, true, nil
		}
		return nil, false, err
	}
	return order, false, nil
}

// acceptResult 是「把订单推到已受理（并最终尝试履约）」的结论。
type acceptResult struct {
	order        *model.Order
	accepted     bool
	rejectReason string
}

// driveAccept 内联推进 CREATED→PAYING→PAID→FULFILLING→FULFILLED。
//
// 每一步都是「独立事务里的 CAS + 台账」，不做一步跳：状态机可读性是退款与运营排障的前提。
// 每一步之前都重新读行而不是沿用内存里的 version：本方法可能被并发请求与 cron 同时驱动，
// 读—CAS—失败重读是唯一能同时保证不重复扣款与不覆盖别人结论的做法。
// 下游幂等键（pay_/grant_/coinpack_ + order_no）保证重驱动不会重复出钱或重复发放。
func (l *CreateOrderLogic) driveAccept(order *model.Order) (*acceptResult, error) {
	current := order
	for {
		fresh, err := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
		if err != nil {
			return nil, err
		}
		if fresh == nil {
			return nil, model.ErrOrderNotFound
		}
		current = fresh
		switch current.State {
		case model.StateCreated:
			if err = l.advance(current, model.StateCreated, model.StatePaying, "user",
				current.RequestID, "sandbox accept begins", nil); err != nil {
				if errors.Is(err, model.ErrConcurrentUpdate) {
					continue // 别人推了一步，重读再走
				}
				return nil, err
			}
			continue
		case model.StatePaying:
			return l.acceptPayment(current)
		default:
			// 已经过了 PAYING：钱已受理，补齐履约即可。
			if model.IsPaidOrLater(current.State) && current.State != model.StatePaid {
				if current.State == model.StateFulfilling || current.State == model.StateFailed {
					// 上一轮履约没走完：交给 FulfillOrder 的口径处理，这里不重复驱动，
					// 避免建单重放路径绕过 fulfill 重试频率护栏。
					return &acceptResult{order: current, accepted: true,
						rejectReason: fulfillmentNote(current)}, nil
				}
				return &acceptResult{order: current, accepted: true}, nil
			}
			return &acceptResult{order: current, accepted: false,
				rejectReason: "order state " + model.StateName(current.State) + " cannot be accepted"}, nil
		}
	}
}

// acceptPayment 调 payment 受理，成功后推进到 PAID 并尝试履约。
func (l *CreateOrderLogic) acceptPayment(order *model.Order) (*acceptResult, error) {
	if l.svcCtx.Payment == nil {
		return nil, model.ErrPaymentNotConfigured
	}
	method, err := mapPayMethod(order.PayMethod)
	if err != nil {
		return nil, err
	}
	reply, cerr := l.svcCtx.Payment.CreatePayment(l.ctx, &paymentrpc.CreatePaymentReq{
		BizOrderNo:  order.OrderNo,
		Mid:         order.Mid,
		AmountMinor: order.AmountMinor, // 服务端重算值；客户端上报值永远走不到这里
		Currency:    order.Currency,
		Method:      method,
		Subject:     truncate(order.Title, maxTitleLen),
		ExpireAt:    order.ExpireAt,
		RequestId:   deriveKey("pay", order.OrderNo),
		Operator:    "user",
	})
	if cerr != nil {
		// 受理失败不推进状态：订单留在 PAYING，用户重试同一 request_id 会重新驱动这里。
		reason := paymentRejectNote(cerr)
		l.Errorf("trade-order/CreateOrder: order %s payment rejected: %v", order.OrderNo, cerr)
		return &acceptResult{order: order, accepted: false, rejectReason: reason}, nil
	}
	payment := reply.GetPayment()
	if payment == nil {
		return &acceptResult{order: order, accepted: false,
			rejectReason: "payment returned no payment record"}, nil
	}
	if payment.GetState() != paymentrpc.PaymentState_PAYMENT_STATE_PAID {
		// 沙箱下 BALANCE/SANDBOX_CHANNEL 都应当同步成功；没成功就不假装 PAID，
		// 留在 PAYING 等 cron 用 BindPayment 兜（这也是唯一能推进它的路径）。
		return &acceptResult{order: order, accepted: false,
			rejectReason: "payment not settled yet: " + payment.GetState().String()}, nil
	}
	if payment.GetAmountMinor() != order.AmountMinor {
		// 金额对不上绝不能继续：本服务的扣款口径与资金台账出现了分歧。
		l.Errorf("trade-order/CreateOrder: order %s amount drift: order=%d payment=%d",
			order.OrderNo, order.AmountMinor, payment.GetAmountMinor())
		return &acceptResult{order: order, accepted: false,
			rejectReason: "payment amount drift, manual reconciliation required"}, nil
	}

	if err = l.advance(order, model.StatePaying, model.StatePaid, "user",
		order.RequestID, "payment settled: "+payment.GetPaymentNo(),
		model.NewOrderUpdate().PaymentNo(truncate(payment.GetPaymentNo(), maxPaymentNoLen)).PaidAt(model.NowUnix())); err != nil {
		if !errors.Is(err, model.ErrConcurrentUpdate) {
			return nil, err
		}
	}

	paid, ferr := l.svcCtx.Orders.FindByOrderNo(l.ctx, order.OrderNo)
	if ferr != nil {
		return nil, ferr
	}
	if paid == nil {
		return nil, model.ErrOrderNotFound
	}
	if paid.State != model.StatePaid {
		// 并发请求已把订单推得更远：不重复履约，直接如实返回。
		return &acceptResult{order: paid, accepted: true, rejectReason: fulfillmentNote(paid)}, nil
	}

	// --- 内联履约：失败不回滚已完成的支付，如实报告 ---
	res, fuerr := runFulfill(l.ctx, l.svcCtx, l.Logger, paid.OrderNo, "user", paid.RequestID)
	if fuerr != nil {
		latest, _ := l.svcCtx.Orders.FindByOrderNo(l.ctx, paid.OrderNo)
		if latest == nil {
			latest = paid
		}
		return &acceptResult{order: latest, accepted: true,
			rejectReason: truncate("paid but fulfillment pending: "+sanitize(fuerr.Error()), maxReasonLen)}, nil
	}
	out := res.order
	if out == nil {
		out = paid
	}
	return &acceptResult{order: out, accepted: true}, nil
}

// advance 在独立事务里做一次 CAS 状态推进 + 台账写入（委托给共享入口，
// 保证建单链路与其它写接口的台账口径完全一致）。
func (l *CreateOrderLogic) advance(order *model.Order, from, to int32, operator, requestID, reason string,
	upd *model.OrderUpdate,
) error {
	return transitionWithEvent(l.ctx, l.svcCtx, order.OrderNo, from, to, order.Version,
		operator, requestID, reason, upd)
}

// planQuote 是服务端从套餐重算出来、并在建单时冻结的快照。
// 快照的意义在于「之后套餐改价改名不影响历史单」——履约与退款都只认这一行。
type planQuote struct {
	planID       int64
	planCode     string
	title        string
	currency     string
	unitPrice    int64
	durationDays int32
	coinAmount   int32
}

// priceFromPlan 向 membership 取套餐并重算单价/时长/枚数/币种/商品名快照。
func (l *CreateOrderLogic) priceFromPlan(in *rpc.CreateOrderReq, bizType, platform, quantity int32,
	defaultCurrency string,
) (*planQuote, error) {
	reply, gerr := l.svcCtx.Membership.GetPlan(l.ctx, &memberrpc.GetPlanReq{
		PlanId:   in.GetPlanId(),
		PlanCode: strings.TrimSpace(in.GetPlanCode()),
	})
	if gerr != nil {
		return nil, fmt.Errorf("membership.GetPlan: %w", gerr)
	}
	if !reply.GetFound() || reply.GetPlan() == nil {
		return nil, model.ErrPlanNotFound
	}
	plan := reply.GetPlan()
	quote := &planQuote{
		planID:   plan.GetPlanId(),
		planCode: truncate(strings.TrimSpace(plan.GetPlanCode()), maxRequestIDLen),
	}

	if plan.GetState() != memberrpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE {
		return nil, fmt.Errorf("%w: plan_state=%d", model.ErrPlanNotOnSale, plan.GetState())
	}
	if !planVisibleOnPlatform(plan.GetPlatforms(), platform) {
		return nil, fmt.Errorf("%w: platform=%d plan_code=%s visible_platforms=%d",
			model.ErrPlanNotVisibleOnPlatform, platform, plan.GetPlanCode(), len(plan.GetPlatforms()))
	}

	unitPrice := plan.GetPromPriceMinor()
	if unitPrice <= 0 {
		unitPrice = plan.GetPriceMinor()
	}
	if unitPrice <= 0 {
		return nil, model.ErrPlanPriceUnavailable
	}
	quote.unitPrice = unitPrice

	switch bizType {
	case model.BizMembership:
		vip := plan.GetVipType()
		if vip != memberrpc.VipType_VIP_TYPE_PREMIUM && vip != memberrpc.VipType_VIP_TYPE_PREMIUM_PLUS {
			return nil, fmt.Errorf("%w: membership plan vip_type=%d", model.ErrPlanTierMismatch, vip)
		}
		if plan.GetDurationDays() <= 0 {
			return nil, fmt.Errorf("%w: duration_days=%d", model.ErrPlanTierMismatch, plan.GetDurationDays())
		}
		unitDays, err := checkedMul(int64(plan.GetDurationDays()), int64(plan.GetUnitCount()))
		if err != nil {
			return nil, err
		}
		total, err := checkedMul(unitDays, int64(quantity))
		if err != nil {
			return nil, err
		}
		quote.durationDays = int32(total)
	case model.BizCoinPack:
		// 契约缺口（README 与交付报告已记）：membership.PlanInfo 没有「一枚枚数」字段，
		// 硬币包只能用 unit_count 承载「每份约定枚数」，且 PlanInfo 上没有 SKU 类型位，
		// 无法校验「这个套餐确实是硬币包」，档位匹配在这里退化为枚数为正。
		coins, err := checkedMul(int64(plan.GetUnitCount()), int64(quantity))
		if err != nil {
			return nil, err
		}
		if coins <= 0 {
			return nil, fmt.Errorf("%w: coin pack unit_count=%d", model.ErrPlanTierMismatch, plan.GetUnitCount())
		}
		quote.coinAmount = int32(coins)
	default:
		return nil, fmt.Errorf("%w: biz_type=%d", model.ErrInvalidBizType, bizType)
	}

	currency := strings.ToUpper(strings.TrimSpace(plan.GetCurrency()))
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(defaultCurrency))
	}
	if currency == "" {
		currency = "CNY"
	}
	quote.currency = truncate(currency, 8)

	// 商品名快照：优先套餐名，套餐没名字时才用调用方传的值（且不信任它超过列宽）。
	title := strings.TrimSpace(plan.GetName())
	if title == "" {
		title = strings.TrimSpace(in.GetTitle())
	}
	quote.title = truncate(title, maxTitleLen)

	return quote, nil
}

// checkedMul 做「先乘后判上界」而不是「先转 int32 再判」，避免乘法溢出成负数后蒙混过关。
func checkedMul(a, b int64) (int64, error) {
	if b <= 0 {
		b = 1 // unit_count 未填视为 1 份（年卡 unit_count=12，普通月卡常留 0）
	}
	res := a * b
	if res > math.MaxInt32 || res < 0 {
		return 0, fmt.Errorf("%w: computed value %d overflows int32", model.ErrPlanTierMismatch, res)
	}
	return res, nil
}

// planVisibleOnPlatform 判断套餐是否对下单端可见。
// platforms 为空按「不可见」处理（fail closed）：这是资金入口，
// 宁可漏放行让运营去补套餐配置，也不能让任何端都能买到没配平台的套餐。
func planVisibleOnPlatform(platforms []memberrpc.PlanPlatform, platform int32) bool {
	want := memberrpc.PlanPlatform(platform) // 两端枚举取值刻意一致（1..5），见两个 proto 的 Platform/PlanPlatform
	for _, p := range platforms {
		if p == want {
			return true
		}
	}
	return false
}

// mapPayMethod 把订单侧支付方式映射到资金域枚举。两档语义一致，但必须显式列出来：
// 未来加第三档（真实渠道）时这里会编译期报错，而不是静默把未知值当沙箱处理。
func mapPayMethod(v int32) (paymentrpc.PayMethod, error) {
	switch v {
	case model.PayBalance:
		return paymentrpc.PayMethod_PAY_METHOD_BALANCE, nil
	case model.PaySandbox:
		return paymentrpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL, nil
	default:
		return paymentrpc.PayMethod_PAY_METHOD_UNSPECIFIED,
			fmt.Errorf("%w: pay_method=%d", model.ErrInvalidPayMethod, v)
	}
}

// paymentRejectNote 把 payment 的错误降级成给客户端看的可读结论（不是错误码）。
// 只提取「余额不足」这类可自助修复的结论，其余一律给通用文案：
// 下游原始错误可能带账号或流水号，不能原样吐给终端。
func paymentRejectNote(err error) string {
	msg := strings.ToLower(sanitize(err.Error()))
	switch {
	case strings.Contains(msg, "balance"), strings.Contains(msg, "insufficient"):
		return "balance not enough, recharge first"
	case strings.Contains(msg, "not configured"):
		return "payment channel not configured"
	default:
		return "payment rejected, please retry or contact support"
	}
}

// fulfillmentNote 给「钱已受理、权益还没给到」的场景一句可读结论。
func fulfillmentNote(order *model.Order) string {
	if order == nil {
		return ""
	}
	switch order.State {
	case model.StateFulfilling, model.StateFailed:
		return truncate("paid, fulfillment pending: "+order.FulfillDetail, maxReasonLen)
	default:
		return ""
	}
}
