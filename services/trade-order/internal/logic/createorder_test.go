package logic

// CreateOrder 是全站唯一建单入口，也是唯一会「一路内联推到 FULFILLED」的写口。
// 本文件锁四件事：
//  1. 金额只信 membership.GetPlan 的重算值，客户端上报值只做防改价校验，绝不参与扣款；
//  2. 建单主表行与出生台账同事务（失败一起回滚，不留半张单）；
//  3. 内联推进 CREATED→PAYING→PAID→FULFILLING→FULFILLED，每步一条台账，不许跳步；
//  4. 下游缺失/失败只如实报告，绝不伪造「已支付」「已发放」。

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	coinrpc "go-video/services/coin/rpc"
	memberrpc "go-video/services/membership/rpc"
	paymentrpc "go-video/services/payment/rpc"
	"go-video/services/trade-order/internal/svc"
	"go-video/services/trade-order/model"
	"go-video/services/trade-order/rpc"
)

// createFixture 是一个 CreateOrder 用例需要的四件套：被测 logic + 内存库 + 假下游。
type createFixture struct {
	l    *CreateOrderLogic
	svc  *svc.ServiceContext
	db   *fakeDB
	mem  *fakeMembership
	pay  *fakePayment
	coin *fakeCoin
}

func newCreateFixture(t *testing.T, plan *memberrpc.PlanInfo) *createFixture {
	t.Helper()
	svcCtx, db := newTestSvc(t)
	mem := newFakeMembership()
	if plan != nil {
		mem.withPlan(plan)
	}
	pay := newFakePayment()
	coin := &fakeCoin{}
	wireDownstream(svcCtx, mem, pay, coin)
	return &createFixture{
		l:    NewCreateOrderLogic(t.Context(), svcCtx),
		svc:  svcCtx,
		db:   db,
		mem:  mem,
		pay:  pay,
		coin: coin,
	}
}

func membershipReq(planID int64, requestID string) *rpc.CreateOrderReq {
	return &rpc.CreateOrderReq{
		Mid:       1001,
		BizType:   rpc.OrderBizType_ORDER_BIZ_TYPE_MEMBERSHIP,
		PlanId:    planID,
		Quantity:  1,
		PayMethod: rpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL,
		RequestId: requestID,
		Platform:  rpc.Platform_PLATFORM_ANDROID,
	}
}

// TestCreateOrderHappyPathWritesFullAuditableChain 正单：一次调用推到 FULFILLED，
// 且每一步都在台账里留下对应行（AGENTS.md §5：状态与台账同事务、不许跳步）。
func TestCreateOrderHappyPathWritesFullAuditableChain(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))

	in := membershipReq(77, "req-create-ok")
	in.Quantity = 2
	in.Title = "客户端自报商品名" // 应被套餐名快照覆盖
	in.ClientTraceId = "trace-abc"

	reply, err := fx.l.CreateOrder(in)
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if reply.GetDuplicated() {
		t.Error("首次下单不该 duplicated")
	}
	if !reply.GetAccepted() || reply.GetRejectReason() != "" {
		t.Errorf("受理结论 = (%v,%q)，期望 (true,\"\")", reply.GetAccepted(), reply.GetRejectReason())
	}

	orderNo := reply.GetOrder().GetOrderNo()
	got := mustOrder(t, fx.db, orderNo)

	if got.State != model.StateFulfilled || got.FulfillState != model.FulfillDone {
		t.Errorf("state/fulfill_state = (%s,%d)，期望 (FULFILLED,%d)",
			model.StateName(got.State), got.FulfillState, model.FulfillDone)
	}
	if got.UnitPriceMinor != 3000 || got.AmountMinor != 6000 {
		t.Errorf("unit/amount = (%d,%d)，期望 (3000,6000)", got.UnitPriceMinor, got.AmountMinor)
	}
	if got.DurationDays != 62 || got.Quantity != 2 || got.CoinAmount != 0 {
		t.Errorf("duration/quantity/coin = (%d,%d,%d)，期望 (62,2,0)",
			got.DurationDays, got.Quantity, got.CoinAmount)
	}
	if got.Title != "大会员月卡" || got.Currency != "CNY" || got.PlanCode != "code_77" || got.PlanID != 77 {
		t.Errorf("快照字段 = (%q,%q,%q,%d)，期望 (\"大会员月卡\",\"CNY\",\"code_77\",77)",
			got.Title, got.Currency, got.PlanCode, got.PlanID)
	}
	if got.PaymentNo != "pay_"+orderNo || got.GrantRef != "membership_grant:7001" {
		t.Errorf("payment_no/grant_ref = (%q,%q)，期望 (%q,%q)",
			got.PaymentNo, got.GrantRef, "pay_"+orderNo, "membership_grant:7001")
	}
	// version=5：建单写 1，其后四次 CAS 各加一。数字变了说明少推/多推了一步。
	if got.Version != 5 {
		t.Errorf("version = %d，期望 5（建单 1 + 四次 CAS）", got.Version)
	}
	if got.RefundedMinor != 0 || got.ClosedAt != 0 {
		t.Errorf("未退款却写了 refunded_minor=%d closed_at=%d", got.RefundedMinor, got.ClosedAt)
	}
	if got.PaidAt == 0 || got.FulfilledAt < got.PaidAt {
		t.Errorf("paid_at/fulfilled_at = (%d,%d)，时间戳顺序不对", got.PaidAt, got.FulfilledAt)
	}
	if got.ExpireAt != got.CreatedAt+1800 {
		t.Errorf("expire_at = %d，期望 created_at(%d)+OrderExpireSeconds(1800)", got.ExpireAt, got.CreatedAt)
	}
	if got.FulfillAttempts != 1 {
		t.Errorf("fulfill_attempts = %d，期望 1", got.FulfillAttempts)
	}
	if got.ClientTraceID != "trace-abc" || got.PayMethod != model.PaySandbox ||
		got.Platform != model.PlatformAndroid || got.RequestID != "req-create-ok" || got.Mid != 1001 {
		t.Errorf("回显字段不符: %+v", got)
	}
	if got.BizType != model.BizMembership {
		t.Errorf("biz_type = %d", got.BizType)
	}

	wantPairs := [][2]string{
		{"UNKNOWN(0)", "CREATED"}, // 建单前不存在状态，0 是出生行的合法取值
		{"CREATED", "PAYING"},
		{"PAYING", "PAID"},
		{"PAID", "FULFILLING"},
		{"FULFILLING", "FULFILLED"},
	}
	pairs := ledgerPairs(fx.db, orderNo)
	if len(pairs) != len(wantPairs) {
		t.Fatalf("台账行数 = %d %v，期望 %d 行", len(pairs), pairs, len(wantPairs))
	}
	for i, w := range wantPairs {
		if pairs[i] != w {
			t.Errorf("台账第 %d 行 = %v，期望 %v", i, pairs[i], w)
		}
	}
	rows := ledgerOf(fx.db, orderNo)
	if rows[0].Operator != "user" || !strings.Contains(rows[0].Reason, "amount_minor=6000") {
		t.Errorf("出生台账 = (%q,%q)，要记录操作者与建单金额", rows[0].Operator, rows[0].Reason)
	}
	if rows[0].RequestID != "req-create-ok" {
		t.Errorf("出生台账 request_id = %q，应与建单幂等键一致", rows[0].RequestID)
	}
	if rows[1].Reason != "sandbox accept begins" {
		t.Errorf("CREATED->PAYING 理由 = %q", rows[1].Reason)
	}
	if !strings.HasPrefix(rows[2].Reason, "payment settled: pay_"+orderNo) {
		t.Errorf("PAID 台账理由 = %q，要能定位到具体 payment_no", rows[2].Reason)
	}
	if !strings.HasPrefix(rows[3].Reason, "fulfill attempt 1, biz_type=1") {
		t.Errorf("FULFILLING 台账理由 = %q", rows[3].Reason)
	}
	if rows[4].Reason != "fulfilled, grant_ref=membership_grant:7001" {
		t.Errorf("FULFILLED 台账理由 = %q", rows[4].Reason)
	}

	// --- 下游调用参数：幂等键由 order_no 派生，绝不用调用方 request_id 透传 ---
	if len(fx.pay.createCalls) != 1 {
		t.Fatalf("CreatePayment 调用次数 = %d，期望 1", len(fx.pay.createCalls))
	}
	cp := fx.pay.createCalls[0]
	if cp.GetRequestId() != "pay_"+orderNo || cp.GetBizOrderNo() != orderNo {
		t.Errorf("CreatePayment request_id/biz_order_no = (%q,%q)，期望 (%q,%q)",
			cp.GetRequestId(), cp.GetBizOrderNo(), "pay_"+orderNo, orderNo)
	}
	if cp.GetAmountMinor() != 6000 || cp.GetCurrency() != "CNY" {
		t.Errorf("CreatePayment 金额/币种 = (%d,%q)，期望 (6000,\"CNY\")", cp.GetAmountMinor(), cp.GetCurrency())
	}
	if cp.GetMethod() != paymentrpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL || cp.GetExpireAt() != got.ExpireAt {
		t.Errorf("CreatePayment method/expire_at = (%d,%d)，期望 (%d,%d)",
			cp.GetMethod(), cp.GetExpireAt(), paymentrpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL, got.ExpireAt)
	}
	if cp.GetSubject() != "大会员月卡" || cp.GetOperator() != "user" || cp.GetMid() != 1001 {
		t.Errorf("CreatePayment subject/operator/mid = (%q,%q,%d)", cp.GetSubject(), cp.GetOperator(), cp.GetMid())
	}
	if len(fx.mem.grantCalls) != 1 {
		t.Fatalf("GrantMembership 调用次数 = %d，期望 1", len(fx.mem.grantCalls))
	}
	gc := fx.mem.grantCalls[0]
	if gc.GetRequestId() != "grant_"+orderNo || gc.GetBizOrderNo() != orderNo {
		t.Errorf("GrantMembership request_id/biz_order_no = (%q,%q)", gc.GetRequestId(), gc.GetBizOrderNo())
	}
	if gc.GetDeltaDays() != 62 || gc.GetVipType() != memberrpc.VipType_VIP_TYPE_PREMIUM {
		t.Errorf("GrantMembership delta_days/vip_type = (%d,%d)", gc.GetDeltaDays(), gc.GetVipType())
	}
	if gc.GetSource() != memberrpc.GrantSource_GRANT_SOURCE_SANDBOX_PURCHASE ||
		gc.GetPaymentNo() != "pay_"+orderNo || gc.GetPlanId() != 77 || gc.GetMid() != 1001 {
		t.Errorf("GrantMembership source/payment_no/plan/mid = (%d,%q,%d,%d)",
			gc.GetSource(), gc.GetPaymentNo(), gc.GetPlanId(), gc.GetMid())
	}
	if len(fx.coin.grantCalls) != 0 {
		t.Errorf("会员单却调了 GrantCoin %d 次", len(fx.coin.grantCalls))
	}
	// 契约缺口固化：to_order 没有 vip_type 快照列，所以履约必须回查一次 GetPlan。
	// 这条断言让「加了快照列却没删这次回查」变得可见，而不是静默留在代码里。
	if fx.mem.planCalls != 2 {
		t.Errorf("GetPlan 调用次数 = %d，期望 2（取价 + 履约回查档位，见 helpers.planVipType 注释）", fx.mem.planCalls)
	}
	assertOnlyLegalTransitions(t, fx.db)
}

// TestCreateOrderRejectsInvalidInput 入参非法必须直接报错：不建单、不写台账、不碰下游。
func TestCreateOrderRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rpc.CreateOrderReq)
		want   error
	}{
		{"mid=0", func(in *rpc.CreateOrderReq) { in.Mid = 0 }, model.ErrInvalidMid},
		{"mid 负数", func(in *rpc.CreateOrderReq) { in.Mid = -1 }, model.ErrInvalidMid},
		{"biz_type 未指定", func(in *rpc.CreateOrderReq) {
			in.BizType = rpc.OrderBizType_ORDER_BIZ_TYPE_UNSPECIFIED
		}, model.ErrInvalidBizType},
		{"biz_type 未知值", func(in *rpc.CreateOrderReq) { in.BizType = rpc.OrderBizType(9) }, model.ErrInvalidBizType},
		{"既无 plan_id 也无 plan_code", func(in *rpc.CreateOrderReq) {
			in.PlanId = 0
			in.PlanCode = "  "
		}, model.ErrPlanRequired},
		{"缺 request_id", func(in *rpc.CreateOrderReq) { in.RequestId = "   " }, model.ErrRequestIdRequired},
		{"pay_method 未指定", func(in *rpc.CreateOrderReq) {
			in.PayMethod = rpc.PayMethod_PAY_METHOD_UNSPECIFIED
		}, model.ErrInvalidPayMethod},
		{"pay_method 未知值", func(in *rpc.CreateOrderReq) { in.PayMethod = rpc.PayMethod(9) }, model.ErrInvalidPayMethod},
		{"platform 未指定", func(in *rpc.CreateOrderReq) {
			in.Platform = rpc.Platform_PLATFORM_UNSPECIFIED
		}, model.ErrInvalidPlatform},
		{"platform 未知值", func(in *rpc.CreateOrderReq) { in.Platform = rpc.Platform(99) }, model.ErrInvalidPlatform},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
			in := membershipReq(77, "req-bad-input")
			tc.mutate(in)
			if _, err := fx.l.CreateOrder(in); !errors.Is(err, tc.want) {
				t.Fatalf("CreateOrder() error = %v，期望 %v", err, tc.want)
			}
			if len(fx.db.orders) != 0 || len(fx.db.events) != 0 {
				t.Errorf("入参非法却落了 %d 张单 / %d 行台账", len(fx.db.orders), len(fx.db.events))
			}
			if len(fx.pay.createCalls) != 0 || len(fx.mem.grantCalls) != 0 {
				t.Error("入参非法却碰了下游资金/权益接口")
			}
			// 入参校验发生在读套餐之前：非法请求不该给 membership 造成压力
			if fx.mem.planCalls != 0 {
				t.Errorf("入参非法却查了 %d 次套餐", fx.mem.planCalls)
			}
		})
	}
}

// TestCreateOrderRejectsAmountTamperingWithoutCreatingOrder 防前端改价：
// 上报值与重算值不一致直接拒，并且错误文本给出服务端重算值（否则调用方只能猜）。
func TestCreateOrderRejectsAmountTamperingWithoutCreatingOrder(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	in := membershipReq(77, "req-drift")
	in.Quantity = 2
	in.AmountMinor = 1 // 想花 1 分买 6000 的东西

	_, err := fx.l.CreateOrder(in)
	if !errors.Is(err, model.ErrAmountMismatch) {
		t.Fatalf("CreateOrder() error = %v，期望 %v", err, model.ErrAmountMismatch)
	}
	for _, want := range []string{"server_amount_minor=6000", "client_amount_minor=1", "unit_price_minor=3000", "quantity=2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文本缺少 %q: %v", want, err)
		}
	}
	if len(fx.db.orders) != 0 || len(fx.db.events) != 0 {
		t.Error("金额被拒后仍留下订单或台账，等于用拒单做了一次建单")
	}
	if len(fx.pay.createCalls) != 0 {
		t.Error("金额被拒后仍受理了支付")
	}

	// 恰好等于重算值：放行
	in2 := membershipReq(77, "req-drift-ok")
	in2.Quantity = 2
	in2.AmountMinor = 6000
	reply, err := fx.l.CreateOrder(in2)
	if err != nil {
		t.Fatalf("一致金额应放行: %v", err)
	}
	if reply.GetOrder().GetAmountMinor() != 6000 {
		t.Errorf("入账金额 = %d，期望 6000", reply.GetOrder().GetAmountMinor())
	}

	// 上报 0 表示「不参与校验」（网关侧不感知价格的调用方）
	in3 := membershipReq(77, "req-drift-zero")
	in3.AmountMinor = 0
	if _, err := fx.l.CreateOrder(in3); err != nil {
		t.Errorf("amount_minor=0 应视为不校验，实得 %v", err)
	}
	assertOnlyLegalTransitions(t, fx.db)
}

// TestCreateOrderPromPriceWinsAndQuantityClamped 促销价优先于原价；份数超上限按配置裁剪，
// 裁剪后与客户端上报值不一致仍然拒单（否则就是「按 11 份的钱买 10 份」）。
func TestCreateOrderPromPriceWinsAndQuantityClamped(t *testing.T) {
	plan := defaultPlan(88, memberrpc.VipType_VIP_TYPE_PREMIUM_PLUS)
	plan.PriceMinor = 3000
	plan.PromPriceMinor = 2500
	plan.DurationDays = 30
	fx := newCreateFixture(t, plan)

	in := membershipReq(88, "req-clamp")
	in.Quantity = 11
	in.AmountMinor = 2500 * 10
	reply, err := fx.l.CreateOrder(in)
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	got := mustOrder(t, fx.db, reply.GetOrder().GetOrderNo())
	if got.Quantity != 10 || got.UnitPriceMinor != 2500 || got.AmountMinor != 25000 {
		t.Errorf("quantity/unit/amount = (%d,%d,%d)，期望 (10,2500,25000)",
			got.Quantity, got.UnitPriceMinor, got.AmountMinor)
	}
	if got.DurationDays != 300 || got.State != model.StateFulfilled {
		t.Errorf("duration_days/state = (%d,%s)", got.DurationDays, model.StateName(got.State))
	}
	if got.BizType != model.BizMembership {
		t.Errorf("biz_type = %d", got.BizType)
	}
	// 促销价是「下单那一刻」的重算结果，落库后就是不可变快照
	if fx.pay.createCalls[0].GetAmountMinor() != 25000 {
		t.Errorf("受理金额 = %d，应为裁剪后的 25000", fx.pay.createCalls[0].GetAmountMinor())
	}

	// 客户端按未裁剪的 11 份付钱 → 拒单
	in2 := membershipReq(88, "req-clamp-mismatch")
	in2.Quantity = 11
	in2.AmountMinor = 2500 * 11
	if _, err := fx.l.CreateOrder(in2); !errors.Is(err, model.ErrAmountMismatch) {
		t.Errorf("按 11 份付钱买 10 份应拒单，实得 %v", err)
	}

	// quantity <= 0 视为 1 份
	in3 := membershipReq(88, "req-qty-zero")
	in3.Quantity = 0
	r3, err := fx.l.CreateOrder(in3)
	if err != nil {
		t.Fatalf("quantity=0 应视为 1: %v", err)
	}
	if r3.GetOrder().GetQuantity() != 1 || r3.GetOrder().GetAmountMinor() != 2500 {
		t.Errorf("quantity=0 结果 = (%d,%d)，期望 (1,2500)",
			r3.GetOrder().GetQuantity(), r3.GetOrder().GetAmountMinor())
	}
	assertOnlyLegalTransitions(t, fx.db)
}

// TestCreateOrderPlanGates 套餐闸门：在售、本端可见、档位匹配、价格可用，缺一不建单。
func TestCreateOrderPlanGates(t *testing.T) {
	cases := []struct {
		name  string
		tweak func(*memberrpc.PlanInfo)
		want  error
	}{
		{"套餐已下架", func(p *memberrpc.PlanInfo) {
			p.State = memberrpc.PlanSaleState_PLAN_SALE_STATE_OFF_SALE
		}, model.ErrPlanNotOnSale},
		{"套餐未配任何端（fail closed）", func(p *memberrpc.PlanInfo) { p.Platforms = nil }, model.ErrPlanNotVisibleOnPlatform},
		{"套餐只对 Web 可见", func(p *memberrpc.PlanInfo) {
			p.Platforms = []memberrpc.PlanPlatform{memberrpc.PlanPlatform_PLAN_PLATFORM_WEB}
		}, model.ErrPlanNotVisibleOnPlatform},
		{"原价非正且无促销价", func(p *memberrpc.PlanInfo) { p.PriceMinor = 0 }, model.ErrPlanPriceUnavailable},
		{"原价非正但促销价可用", func(p *memberrpc.PlanInfo) {
			p.PriceMinor = -1
			p.PromPriceMinor = 100
		}, nil}, // 放行：促销价优先
		{"会员套餐时长非正", func(p *memberrpc.PlanInfo) { p.DurationDays = 0 }, model.ErrPlanTierMismatch},
		{"会员档位不是 PREMIUM 系（枚举里只有 UNSPECIFIED 是非法档）", func(p *memberrpc.PlanInfo) {
			p.VipType = memberrpc.VipType_VIP_TYPE_UNSPECIFIED
		}, model.ErrPlanTierMismatch},
		{"会员档位超定义（残留枚举值）", func(p *memberrpc.PlanInfo) {
			p.VipType = memberrpc.VipType(9)
		}, model.ErrPlanTierMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM)
			tc.tweak(plan)
			fx := newCreateFixture(t, plan)
			reply, err := fx.l.CreateOrder(membershipReq(77, "req-gate"))
			if tc.want == nil {
				if err != nil {
					t.Fatalf("闸门应当放行: %v", err)
				}
				if reply.GetOrder().GetUnitPriceMinor() != 100 {
					t.Errorf("unit_price_minor = %d，期望促销价 100", reply.GetOrder().GetUnitPriceMinor())
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("CreateOrder() error = %v，期望 %v", err, tc.want)
			}
			if len(fx.db.orders) != 0 || len(fx.db.events) != 0 {
				t.Error("闸门未过却建单/写台账")
			}
			if len(fx.pay.createCalls) != 0 {
				t.Error("闸门未过却受理了支付")
			}
		})
	}
}

// TestCreateOrderCoinPackTierGates 硬币包档位：unit_count 承载「每份枚数」，
// 非正即拒；同时锁住已知缺口 —— PlanInfo 没有 SKU 类型位，所以「这真是硬币包吗」无法校验。
func TestCreateOrderCoinPackTierGates(t *testing.T) {
	t.Run("每份枚数非正", func(t *testing.T) {
		plan := defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM)
		plan.UnitCount = 0
		fx := newCreateFixture(t, plan)
		in := membershipReq(77, "req-coin-gate")
		in.BizType = rpc.OrderBizType_ORDER_BIZ_TYPE_COIN_PACK
		if _, err := fx.l.CreateOrder(in); !errors.Is(err, model.ErrPlanTierMismatch) {
			t.Fatalf("CreateOrder() error = %v，期望 %v", err, model.ErrPlanTierMismatch)
		}
		if len(fx.db.orders) != 0 {
			t.Error("枚数非正却建单")
		}
	})

	t.Run("硬币包档位退化为「枚数为正」（已知缺口）", func(t *testing.T) {
		plan := defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM)
		plan.UnitCount = 600 // 每份 600 枚：硬币包语义
		fx := newCreateFixture(t, plan)
		in := membershipReq(77, "req-tier-degenerate")
		in.Quantity = 2
		in.BizType = rpc.OrderBizType_ORDER_BIZ_TYPE_COIN_PACK
		reply, err := fx.l.CreateOrder(in)
		if err != nil {
			t.Fatalf("CreateOrder() error = %v", err)
		}
		got := mustOrder(t, fx.db, reply.GetOrder().GetOrderNo())
		if got.CoinAmount != 1200 || got.DurationDays != 0 {
			t.Errorf("coin_amount/duration = (%d,%d)，期望 (1200,0)", got.CoinAmount, got.DurationDays)
		}
		// 缺口固化：PlanInfo 上没有 SKU 类型位，所以「这个套餐到底是不是硬币包」
		// 在本服务无从校验 —— 拿一张会员月卡当硬币包下单会成功（只发 600 枚）。
		if got.PlanID != 77 || got.BizType != model.BizCoinPack {
			t.Errorf("biz_type/plan_id = (%d,%d)", got.BizType, got.PlanID)
		}
	})
}

// TestCreateOrderPropagatesGetPlanFailure 取价失败必须是错误，而不是「建一张默认价订单」。
func TestCreateOrderPropagatesGetPlanFailure(t *testing.T) {
	t.Run("下游报错原样上抛", func(t *testing.T) {
		fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
		fx.mem.getPlanErr = status.Error(codes.Unavailable, "membership is down")
		_, err := fx.l.CreateOrder(membershipReq(77, "req-plan-err"))
		if err == nil {
			t.Fatal("GetPlan 失败却返回成功")
		}
		if !strings.Contains(err.Error(), "membership.GetPlan") {
			t.Errorf("错误文本 = %v，要能看出是哪个下游失败", err)
		}
		if status.Code(err) != codes.Unavailable {
			t.Errorf("下游 gRPC code 被吞掉了: %v", err)
		}
		if len(fx.db.orders) != 0 || len(fx.db.events) != 0 {
			t.Error("取价失败却留下订单或台账")
		}
	})

	t.Run("套餐不存在不是免费下单", func(t *testing.T) {
		fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
		if _, err := fx.l.CreateOrder(membershipReq(999, "req-plan-missing")); !errors.Is(err, model.ErrPlanNotFound) {
			t.Errorf("CreateOrder() error = %v，期望 %v", err, model.ErrPlanNotFound)
		}
		if len(fx.db.orders) != 0 {
			t.Error("套餐不存在却建单")
		}
	})
}

// TestCreateOrderWithoutDownstreamClientsRefusesToCreate 下游缺位时宁可拒单：
// 落了单却无处受理，就会造出一张既付不了也退不掉的死单。
func TestCreateOrderWithoutDownstreamClientsRefusesToCreate(t *testing.T) {
	t.Run("无 membership 取不到价", func(t *testing.T) {
		fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
		fx.svc.Membership = nil
		if _, err := fx.l.CreateOrder(membershipReq(77, "req-no-mem")); !errors.Is(err, model.ErrMembershipNotConfigured) {
			t.Fatalf("CreateOrder() error = %v，期望 %v", err, model.ErrMembershipNotConfigured)
		}
		if len(fx.db.orders) != 0 || len(fx.db.events) != 0 {
			t.Error("membership 缺位却建单")
		}
	})

	t.Run("无 payment 收不了钱", func(t *testing.T) {
		fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
		fx.svc.Payment = nil
		if _, err := fx.l.CreateOrder(membershipReq(77, "req-no-pay")); !errors.Is(err, model.ErrPaymentNotConfigured) {
			t.Fatalf("CreateOrder() error = %v，期望 %v", err, model.ErrPaymentNotConfigured)
		}
		if len(fx.db.orders) != 0 {
			t.Error("payment 缺位却建单（会留下永远付不了的订单）")
		}
	})
}

// TestCreateOrderReplayOnFulfilledOrderDoesNotRecharge 幂等重放：同 request_id 再来一次，
// 回同一张单，且绝不再次扣款/发放。
func TestCreateOrderReplayOnFulfilledOrderDoesNotRecharge(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	seed := seedOrder(t, fx.db, &model.Order{
		OrderNo: "to_replay1", RequestID: "req-replay", Mid: 1001, BizType: model.BizMembership,
		PlanID: 77, State: model.StateFulfilled, FulfillState: model.FulfillDone,
		AmountMinor: 3000, UnitPriceMinor: 3000, Quantity: 1, DurationDays: 31,
		PaymentNo: "pay_to_replay1", GrantRef: "membership_grant:7001", Version: 5,
	})
	before := len(fx.db.events)

	reply, err := fx.l.CreateOrder(membershipReq(77, "req-replay"))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if !reply.GetDuplicated() {
		t.Error("重放必须 duplicated=true")
	}
	if !reply.GetAccepted() || reply.GetRejectReason() != "" {
		t.Errorf("重放结论 = (%v,%q)", reply.GetAccepted(), reply.GetRejectReason())
	}
	if reply.GetOrder().GetOrderNo() != seed.OrderNo {
		t.Errorf("重放回的单号 = %q，期望原单 %q", reply.GetOrder().GetOrderNo(), seed.OrderNo)
	}
	if len(fx.pay.createCalls) != 0 || len(fx.mem.grantCalls) != 0 {
		t.Errorf("重放又调了下游: payment=%d grant=%d", len(fx.pay.createCalls), len(fx.mem.grantCalls))
	}
	if len(fx.db.orders) != 1 {
		t.Errorf("重放又建了单，现有 %d 张", len(fx.db.orders))
	}
	if len(fx.db.events) != before {
		t.Errorf("重放多写了 %d 行台账", len(fx.db.events)-before)
	}
	if got := mustOrder(t, fx.db, seed.OrderNo); got.Version != 5 || got.State != model.StateFulfilled {
		t.Errorf("重放改动了订单: state=%s version=%d", model.StateName(got.State), got.Version)
	}
}

// TestCreateOrderReplayDoesNotDetectChangedParams 锁定现状（契约缺口）：
// 同 request_id 换套餐/换份数不会被判「参数冲突」，而是原样重放首单，
// 连套餐都不会再读一次。调用方若以为「换个金额重发就会改单」就错了。
func TestCreateOrderReplayDoesNotDetectChangedParams(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	fx.mem.withPlan(defaultPlan(88, memberrpc.VipType_VIP_TYPE_PREMIUM_PLUS))
	seedOrder(t, fx.db, &model.Order{
		OrderNo: "to_replay2", RequestID: "req-same-key", Mid: 1001, BizType: model.BizMembership,
		PlanID: 77, PlanCode: "code_77", State: model.StateFulfilled, FulfillState: model.FulfillDone,
		AmountMinor: 3000, UnitPriceMinor: 3000, Quantity: 1, DurationDays: 31, Version: 5,
	})

	in := membershipReq(88, "req-same-key") // 同 request_id，换套餐、换份数、换金额
	in.Quantity = 5
	in.AmountMinor = 999999
	reply, err := fx.l.CreateOrder(in)
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if !reply.GetDuplicated() {
		t.Fatal("期望走幂等重放")
	}
	if reply.GetOrder().GetPlanId() != 77 || reply.GetOrder().GetAmountMinor() != 3000 {
		t.Errorf("重放却换了单内容: plan=%d amount=%d",
			reply.GetOrder().GetPlanId(), reply.GetOrder().GetAmountMinor())
	}
	if fx.mem.planCalls != 0 {
		t.Errorf("重放路径不该再读套餐，实际读了 %d 次", fx.mem.planCalls)
	}
	if len(fx.db.orders) != 1 {
		t.Errorf("重放又建单: %d 张", len(fx.db.orders))
	}
}

// TestCreateOrderReplayRedrivesStuckPayingOrder 首次请求停在 PAYING（进程崩溃/payment 抖动）时，
// 同一 request_id 重试要真正自愈，而不是回一句 duplicated 就完事。
func TestCreateOrderRedriveStuckPayingOrderOnReplay(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	seedOrder(t, fx.db, &model.Order{
		OrderNo: "to_stuck", RequestID: "req-redrive", Mid: 1001, BizType: model.BizMembership,
		PlanID: 77, PlanCode: "code_77", Title: "大会员月卡", Currency: "CNY",
		Quantity: 1, DurationDays: 31, UnitPriceMinor: 3000, AmountMinor: 3000,
		PayMethod: model.PaySandbox, Platform: model.PlatformAndroid,
		State: model.StatePaying, FulfillState: model.FulfillPending, Version: 2,
	})
	seedLedger(t, fx.db, &model.OrderEvent{OrderNo: "to_stuck", FromState: 0, ToState: model.StateCreated, Operator: "user", Reason: "order created", RequestID: "req-redrive"})
	seedLedger(t, fx.db, &model.OrderEvent{OrderNo: "to_stuck", FromState: model.StateCreated, ToState: model.StatePaying, Operator: "user", Reason: "sandbox accept begins", RequestID: "req-redrive"})

	reply, err := fx.l.CreateOrder(membershipReq(77, "req-redrive"))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if !reply.GetDuplicated() || !reply.GetAccepted() {
		t.Errorf("重驱动结论 = (dup=%v,accepted=%v)", reply.GetDuplicated(), reply.GetAccepted())
	}
	got := mustOrder(t, fx.db, "to_stuck")
	if got.State != model.StateFulfilled {
		t.Fatalf("重驱动后 state = %s，期望 FULFILLED", model.StateName(got.State))
	}
	if got.Version != 5 {
		t.Errorf("version = %d，期望 5（PAYING(2) + PAID/FULFILLING/FULFILLED 三次 CAS）", got.Version)
	}
	if got.PaymentNo != "pay_to_stuck" || got.GrantRef != "membership_grant:7001" {
		t.Errorf("payment_no/grant_ref = (%q,%q)", got.PaymentNo, got.GrantRef)
	}
	// 下游幂等键由 order_no 派生：重驱动用的键与首次一致，所以「不重复出钱」是可证的
	if fx.pay.createCalls[0].GetRequestId() != "pay_to_stuck" {
		t.Errorf("重驱动的 CreatePayment 幂等键 = %q，期望 pay_to_stuck", fx.pay.createCalls[0].GetRequestId())
	}
	if fx.mem.grantCalls[0].GetRequestId() != "grant_to_stuck" {
		t.Errorf("重驱动的 GrantMembership 幂等键 = %q，期望 grant_to_stuck", fx.mem.grantCalls[0].GetRequestId())
	}
	wantPairs := [][2]string{
		{"UNKNOWN(0)", "CREATED"}, {"CREATED", "PAYING"},
		{"PAYING", "PAID"}, {"PAID", "FULFILLING"}, {"FULFILLING", "FULFILLED"},
	}
	pairs := ledgerPairs(fx.db, "to_stuck")
	if len(pairs) != len(wantPairs) {
		t.Fatalf("台账链 = %v，期望 %v", pairs, wantPairs)
	}
	for i, w := range wantPairs {
		if pairs[i] != w {
			t.Errorf("台账第 %d 行 = %v，期望 %v", i, pairs[i], w)
		}
	}
	assertOnlyLegalTransitions(t, fx.db)
}

// TestCreateOrderPaymentFailureKeepsOrderInPaying 受理失败不推进状态：
// 留在 PAYING 等同一 request_id 重试自愈，也等 cron 用 BindPayment 兜。
func TestCreateOrderPaymentFailureKeepsOrderInPaying(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantReason string
	}{
		{"余额不足给出可自助的文案", status.Error(codes.FailedPrecondition, "insufficient balance"), "balance not enough, recharge first"},
		{"渠道未配置", status.Error(codes.Unimplemented, "sandbox channel not configured"), "payment channel not configured"},
		{"其它故障给通用文案", status.Error(codes.Unavailable, "payment is down"), "payment rejected, please retry or contact support"},
		{"下游错误含凭据时一个字都不外泄", errors.New("refund failed: user=root password=S3cr3t@tcp(10.0.0.1:3306)/db"),
			"payment rejected, please retry or contact support"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
			fx.pay.createErr = tc.err

			reply, err := fx.l.CreateOrder(membershipReq(77, "req-pay-fail"))
			if err != nil {
				t.Fatalf("受理失败不该让整个调用失败: %v", err)
			}
			if reply.GetDuplicated() || reply.GetAccepted() {
				t.Errorf("结论 = (dup=%v,accepted=%v)，期望 (false,false)", reply.GetDuplicated(), reply.GetAccepted())
			}
			if reply.GetRejectReason() != tc.wantReason {
				t.Errorf("reject_reason = %q，期望 %q", reply.GetRejectReason(), tc.wantReason)
			}
			for _, leak := range []string{"S3cr3t", "@tcp(", "password"} {
				if strings.Contains(reply.GetRejectReason(), leak) {
					t.Errorf("reject_reason 泄露下游内容 %q: %q", leak, reply.GetRejectReason())
				}
			}
			orderNo := reply.GetOrder().GetOrderNo()
			got := mustOrder(t, fx.db, orderNo)
			if got.State != model.StatePaying {
				t.Errorf("state = %s，期望留在 PAYING", model.StateName(got.State))
			}
			if got.Version != 2 || got.PaidAt != 0 || got.PaymentNo != "" {
				t.Errorf("未受理成功却推进了订单: version=%d paid_at=%d payment_no=%q",
					got.Version, got.PaidAt, got.PaymentNo)
			}
			if got.FulfillState != model.FulfillPending || got.FulfillAttempts != 0 {
				t.Errorf("未收款却开始履约: fulfill_state=%d attempts=%d", got.FulfillState, got.FulfillAttempts)
			}
			if len(fx.mem.grantCalls) != 0 {
				t.Error("支付失败却发放了权益")
			}
			pairs := ledgerPairs(fx.db, orderNo)
			if len(pairs) != 2 || pairs[1] != [2]string{"CREATED", "PAYING"} {
				t.Errorf("台账链 = %v，期望停在 CREATED->PAYING", pairs)
			}
			assertOnlyLegalTransitions(t, fx.db)
		})
	}
}

// TestCreateOrderPaymentNotSettledDoesNotFakePaid 沙箱下 payment 没回 PAID 就不假装已付。
func TestCreateOrderPaymentNotSettledDoesNotFakePaid(t *testing.T) {
	cases := []paymentrpc.PaymentState{
		paymentrpc.PaymentState_PAYMENT_STATE_PENDING,
		paymentrpc.PaymentState_PAYMENT_STATE_FAILED,
		paymentrpc.PaymentState_PAYMENT_STATE_CLOSED,
	}
	for _, st := range cases {
		t.Run(st.String(), func(t *testing.T) {
			fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
			fx.pay.createState = st

			reply, err := fx.l.CreateOrder(membershipReq(77, "req-unsettled"))
			if err != nil {
				t.Fatalf("CreateOrder() error = %v", err)
			}
			want := "payment not settled yet: " + st.String()
			if reply.GetAccepted() || reply.GetRejectReason() != want {
				t.Errorf("结论 = (%v,%q)，期望 (false,%q)", reply.GetAccepted(), reply.GetRejectReason(), want)
			}
			got := mustOrder(t, fx.db, reply.GetOrder().GetOrderNo())
			if got.State != model.StatePaying || got.PaymentNo != "" || got.PaidAt != 0 {
				t.Errorf("未结算却推进了: state=%s payment_no=%q paid_at=%d",
					model.StateName(got.State), got.PaymentNo, got.PaidAt)
			}
			if len(fx.mem.grantCalls) != 0 {
				t.Error("钱没到账却发放权益")
			}
		})
	}

	t.Run("payment 返回空对象", func(t *testing.T) {
		fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
		fx.pay.emptyPayment = true
		reply, err := fx.l.CreateOrder(membershipReq(77, "req-empty"))
		if err != nil {
			t.Fatalf("CreateOrder() error = %v", err)
		}
		if reply.GetAccepted() || reply.GetRejectReason() != "payment returned no payment record" {
			t.Errorf("结论 = (%v,%q)", reply.GetAccepted(), reply.GetRejectReason())
		}
	})
}

// TestCreateOrderPaymentAmountDriftStopsBeforeFulfillment 资金台账与本单金额分歧时：
// 停在 PAYING、报人工对账，绝不「就近取整」继续发放。
func TestCreateOrderPaymentAmountDriftStopsBeforeFulfillment(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	fx.pay.createAmount = 2999 // payment 少收了 1 分

	reply, err := fx.l.CreateOrder(membershipReq(77, "req-amount-drift"))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if reply.GetAccepted() || reply.GetRejectReason() != "payment amount drift, manual reconciliation required" {
		t.Errorf("结论 = (%v,%q)", reply.GetAccepted(), reply.GetRejectReason())
	}
	got := mustOrder(t, fx.db, reply.GetOrder().GetOrderNo())
	if got.State != model.StatePaying || got.PaymentNo != "" {
		t.Errorf("金额漂移却推进了: state=%s payment_no=%q", model.StateName(got.State), got.PaymentNo)
	}
	if got.AmountMinor != 3000 {
		t.Errorf("订单金额被下游改写了: %d", got.AmountMinor)
	}
	if len(fx.mem.grantCalls) != 0 {
		t.Error("金额不一致却发放权益")
	}
}

// TestCreateOrderFulfillmentFailureReportsPaidButPending 钱已受理、权益没给到：
// 订单进 FAILED 并如实报告，绝不回滚成未支付，也绝不伪造成 FULFILLED。
func TestCreateOrderFulfillmentFailureReportsPaidButPending(t *testing.T) {
	cases := []struct {
		name       string
		grantErr   error
		wantDetail string
	}{
		{"下游可读错误", status.Error(codes.Unavailable, "membership is draining"),
			"membership.GrantMembership: rpc error: code = Unavailable desc = membership is draining"},
		{"下游错误含凭据", errors.New("grant failed: password=topsecret token=abc bearer x"),
			"downstream error message redacted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
			fx.mem.grantErr = tc.grantErr

			reply, err := fx.l.CreateOrder(membershipReq(77, "req-fulfill-fail"))
			if err != nil {
				t.Fatalf("履约失败不该让建单接口报错（钱已收）: %v", err)
			}
			if !reply.GetAccepted() {
				t.Error("钱已受理，accepted 必须为 true")
			}
			if !strings.HasPrefix(reply.GetRejectReason(), "paid but fulfillment pending: ") {
				t.Errorf("reject_reason = %q，要说明「已付款但履约未完成」", reply.GetRejectReason())
			}
			orderNo := reply.GetOrder().GetOrderNo()
			got := mustOrder(t, fx.db, orderNo)
			if got.State != model.StateFailed || got.FulfillState != model.FulfillFailed {
				t.Errorf("state/fulfill_state = (%s,%d)，期望 (FAILED,%d)",
					model.StateName(got.State), got.FulfillState, model.FulfillFailed)
			}
			if got.FulfillDetail != tc.wantDetail {
				t.Errorf("fulfill_detail = %q，期望 %q", got.FulfillDetail, tc.wantDetail)
			}
			for _, leak := range []string{"topsecret", "bearer", "@tcp(", "token=abc"} {
				if strings.Contains(got.FulfillDetail, leak) {
					t.Errorf("fulfill_detail 泄露 %q: %q", leak, got.FulfillDetail)
				}
			}
			if got.GrantRef != "" {
				t.Errorf("发放失败却写了 grant_ref=%q", got.GrantRef)
			}
			if got.PaymentNo != "pay_"+orderNo || got.PaidAt == 0 || got.State != model.StateFailed {
				t.Error("履约失败把已完成的支付也回滚了")
			}
			if got.Version != 5 || got.FulfillAttempts != 1 {
				t.Errorf("version/attempts = (%d,%d)，期望 (5,1)", got.Version, got.FulfillAttempts)
			}
			pairs := ledgerPairs(fx.db, orderNo)
			wantTail := [][2]string{{"PAID", "FULFILLING"}, {"FULFILLING", "FAILED"}}
			if len(pairs) != 5 {
				t.Fatalf("台账链 = %v，期望 5 行", pairs)
			}
			for i, w := range wantTail {
				if pairs[3+i] != w {
					t.Errorf("台账尾部第 %d 行 = %v，期望 %v", i, pairs[3+i], w)
				}
			}
			last := ledgerOf(fx.db, orderNo)[4]
			if !strings.HasPrefix(last.Reason, "fulfill failed: ") {
				t.Errorf("FAILED 台账理由 = %q", last.Reason)
			}
			if last.RequestID != "fulfillfail_"+orderNo {
				t.Errorf("FAILED 台账 request_id = %q，期望 fulfillfail_%s", last.RequestID, orderNo)
			}
			assertOnlyLegalTransitions(t, fx.db)
		})
	}
}

// TestCreateOrderDoesNotAdvanceOnFulfillingReplay 建单重放路径对停在 FULFILLING/FAILED
// 的订单不再重复驱动，避免绕过 FulfillOrder 的重试频率护栏。
func TestCreateOrderDoesNotAdvanceOnFulfillingReplay(t *testing.T) {
	for _, st := range []int32{model.StateFulfilling, model.StateFailed} {
		t.Run(model.StateName(st), func(t *testing.T) {
			fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
			seedOrder(t, fx.db, &model.Order{
				OrderNo: "to_mid_fulfill", RequestID: "req-mid", Mid: 1001, BizType: model.BizMembership,
				PlanID: 77, State: st, FulfillState: model.FulfillPending, FulfillDetail: "boom",
				AmountMinor: 3000, UnitPriceMinor: 3000, Quantity: 1, DurationDays: 31,
				PaymentNo: "pay_to_mid_fulfill", Version: 4, FulfillAttempts: 1,
			})

			reply, err := fx.l.CreateOrder(membershipReq(77, "req-mid"))
			if err != nil {
				t.Fatalf("CreateOrder() error = %v", err)
			}
			if !reply.GetDuplicated() || !reply.GetAccepted() {
				t.Errorf("结论 = (dup=%v,accepted=%v)", reply.GetDuplicated(), reply.GetAccepted())
			}
			if reply.GetRejectReason() != "paid, fulfillment pending: boom" {
				t.Errorf("reject_reason = %q，期望带出原 fulfill_detail", reply.GetRejectReason())
			}
			if len(fx.pay.createCalls) != 0 || len(fx.mem.grantCalls) != 0 {
				t.Error("重放绕过了 FulfillOrder 护栏，直接又驱动了下游")
			}
			got := mustOrder(t, fx.db, "to_mid_fulfill")
			if got.State != st || got.Version != 4 {
				t.Errorf("重放改动了订单: state=%s version=%d", model.StateName(got.State), got.Version)
			}
			if len(fx.db.events) != 0 {
				t.Errorf("重放写了 %d 行台账，期望 0 行", len(fx.db.events))
			}
		})
	}
}

// TestCreateOrderCoinPackHappyPath 硬币包：金额仍由套餐重算，履约发的是社区硬币枚数，
// 幂等键是 coinpack_ + order_no（与会员的 grant_ 分开，避免撞同一个下游键）。
func TestCreateOrderCoinPackHappyPath(t *testing.T) {
	plan := defaultPlan(66, memberrpc.VipType_VIP_TYPE_PREMIUM)
	plan.UnitCount = 100
	fx := newCreateFixture(t, plan)

	in := membershipReq(66, "req-coin-ok")
	in.BizType = rpc.OrderBizType_ORDER_BIZ_TYPE_COIN_PACK
	in.Quantity = 3
	reply, err := fx.l.CreateOrder(in)
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	orderNo := reply.GetOrder().GetOrderNo()
	got := mustOrder(t, fx.db, orderNo)
	if got.CoinAmount != 300 || got.DurationDays != 0 || got.AmountMinor != 9000 {
		t.Errorf("coin_amount/duration/amount = (%d,%d,%d)，期望 (300,0,9000)",
			got.CoinAmount, got.DurationDays, got.AmountMinor)
	}
	if got.State != model.StateFulfilled || got.GrantRef != "coin_flow:5001" {
		t.Errorf("state/grant_ref = (%s,%q)", model.StateName(got.State), got.GrantRef)
	}
	if len(fx.coin.grantCalls) != 1 {
		t.Fatalf("GrantCoin 调用次数 = %d，期望 1", len(fx.coin.grantCalls))
	}
	gc := fx.coin.grantCalls[0]
	if gc.GetRequestId() != "coinpack_"+orderNo || gc.GetBizNo() != orderNo {
		t.Errorf("GrantCoin request_id/biz_no = (%q,%q)", gc.GetRequestId(), gc.GetBizNo())
	}
	if gc.GetDelta() != 300 || gc.GetFlowType() != coinrpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK {
		t.Errorf("GrantCoin delta/flow_type = (%d,%v)，期望 (300,ORDER_PACK)", gc.GetDelta(), gc.GetFlowType())
	}
	if len(fx.mem.grantCalls) != 0 {
		t.Error("硬币包却发放了会员")
	}
	if fx.mem.planCalls != 1 {
		t.Errorf("硬币包不需要回查档位，GetPlan 调用次数 = %d", fx.mem.planCalls)
	}
	assertOnlyLegalTransitions(t, fx.db)
}

// TestCreateOrderCoinPackWithoutCoinClientMarksFailed 没有 coin 客户端时硬币包履约必须失败，
// 而不是「订单成功但什么都没发」。
func TestCreateOrderCoinPackWithoutCoinClientMarksFailed(t *testing.T) {
	plan := defaultPlan(66, memberrpc.VipType_VIP_TYPE_PREMIUM)
	plan.UnitCount = 100
	fx := newCreateFixture(t, plan)
	fx.svc.Coin = nil

	in := membershipReq(66, "req-coin-noclient")
	in.BizType = rpc.OrderBizType_ORDER_BIZ_TYPE_COIN_PACK
	reply, err := fx.l.CreateOrder(in)
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	got := mustOrder(t, fx.db, reply.GetOrder().GetOrderNo())
	if got.State != model.StateFailed || got.FulfillState != model.FulfillFailed {
		t.Errorf("state/fulfill_state = (%s,%d)", model.StateName(got.State), got.FulfillState)
	}
	if !strings.Contains(got.FulfillDetail, "coin rpc not configured") {
		t.Errorf("fulfill_detail = %q，要说明缺哪个下游", got.FulfillDetail)
	}
	if got.GrantRef != "" {
		t.Errorf("什么都没发却写了 grant_ref=%q", got.GrantRef)
	}
	if reply.GetOrder().GetState() != rpc.OrderState_ORDER_STATE_FAILED {
		t.Errorf("回包状态 = %v，期望 FAILED", reply.GetOrder().GetState())
	}
}

// TestCreateOrderConcurrentDuplicateFallsBackToFirstOrder 并发建单：读时看不到、
// 写时撞 uniq_request_id，必须回查首单并按重放返回，而不是报 500 也不是建两张单。
func TestCreateOrderConcurrentDuplicateFallsBackToFirstOrder(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	first := seedOrder(t, fx.db, &model.Order{
		OrderNo: "to_first", RequestID: "req-race", Mid: 1001, BizType: model.BizMembership,
		PlanID: 77, State: model.StateFulfilled, FulfillState: model.FulfillDone,
		AmountMinor: 3000, UnitPriceMinor: 3000, Quantity: 1, DurationDays: 31, Version: 5,
	})
	// 只让首行读不到 request_id：InsertTx 仍会撞唯一键
	fx.db.hideRequestIDLookups = 1

	reply, err := fx.l.CreateOrder(membershipReq(77, "req-race"))
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if !reply.GetDuplicated() {
		t.Error("并发重放要如实回 duplicated=true")
	}
	if reply.GetOrder().GetOrderNo() != first.OrderNo {
		t.Errorf("返回了 %q，期望首单 %q", reply.GetOrder().GetOrderNo(), first.OrderNo)
	}
	if len(fx.db.orders) != 1 {
		t.Errorf("并发建单落了 %d 张单，期望 1 张", len(fx.db.orders))
	}
	if len(fx.pay.createCalls) != 0 || len(fx.mem.grantCalls) != 0 {
		t.Error("并发重放又扣了一次钱")
	}
}

// TestCreateOrderBirthLedgerFailureRollsBackOrderRow 「订单存在但没人知道它被建出来」
// 是不可审计的失效模式，所以出生台账与主表行必须同生共死。
func TestCreateOrderBirthLedgerFailureRollsBackOrderRow(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	fx.db.failOn = &failOn{statePair: pairName(0, model.StateCreated), attempts: 1}

	_, err := fx.l.CreateOrder(membershipReq(77, "req-rollback"))
	if err == nil {
		t.Fatal("台账写失败却返回成功")
	}
	if len(fx.db.orders) != 0 {
		t.Errorf("台账失败却留下 %d 张订单", len(fx.db.orders))
	}
	if len(fx.db.events) != 0 {
		t.Errorf("主表回滚却留下 %d 行台账", len(fx.db.events))
	}
	if fx.db.txRolls != 1 {
		t.Errorf("事务回滚次数 = %d，期望 1", fx.db.txRolls)
	}
	if len(fx.pay.createCalls) != 0 {
		t.Error("建单事务失败后仍受理了支付")
	}
}

// TestCreateOrderOrderInsertFailureDoesNotReachDownstream 主表写入失败（如 order_no 撞库）
// 直接上抛，不进入受理。
func TestCreateOrderOrderInsertFailureDoesNotReachDownstream(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	fx.db.insertErr = model.ErrOrderNoCollision
	if _, err := fx.l.CreateOrder(membershipReq(77, "req-collision")); !errors.Is(err, model.ErrOrderNoCollision) {
		t.Fatalf("CreateOrder() error = %v，期望 %v", err, model.ErrOrderNoCollision)
	}
	if len(fx.db.orders) != 0 || len(fx.db.events) != 0 {
		t.Error("建单失败却留下数据")
	}
	if len(fx.pay.createCalls) != 0 {
		t.Error("建单失败却受理了支付")
	}

	// 其它 model 错误（如连接不可用）也原样上抛，不降级成「成功但没单」
	fx2 := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	fx2.db.insertErr = errors.New("db is gone")
	if _, err := fx2.l.CreateOrder(membershipReq(77, "req-db-gone")); err == nil ||
		!strings.Contains(err.Error(), "db is gone") {
		t.Errorf("CreateOrder() error = %v，期望原样上抛", err)
	}
}

// TestCreateOrderFindByRequestIDErrorPropagates 幂等回查失败不能当「没有历史单」处理，
// 否则会重复建单。
func TestCreateOrderFindByRequestIDErrorPropagates(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	fx.db.findRequestErr = errors.New("lookup failed")
	if _, err := fx.l.CreateOrder(membershipReq(77, "req-lookup-err")); err == nil ||
		!strings.Contains(err.Error(), "lookup failed") {
		t.Fatalf("CreateOrder() error = %v，期望原样上抛回查错误", err)
	}
	if len(fx.db.orders) != 0 {
		t.Error("回查失败却建了单（可能重复建单）")
	}
}

// TestCreateOrderSnapshotsTitleAndPlanCode 商品名与套餐码快照：优先套餐名，
// 套餐没名字才用调用方传的（并按列宽裁），且按 plan_code 下单也能取到 plan_id。
func TestCreateOrderSnapshotsTitleAndPlanCode(t *testing.T) {
	plan := defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM)
	plan.Name = "   "
	fx := newCreateFixture(t, plan)

	in := membershipReq(0, "req-code")
	in.PlanId = 0
	in.PlanCode = "  code_77  "
	in.Title = strings.Repeat("会员", 200) // 400 rune，超 title 列宽 200

	reply, err := fx.l.CreateOrder(in)
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	got := mustOrder(t, fx.db, reply.GetOrder().GetOrderNo())
	if got.PlanID != 77 {
		t.Errorf("按 plan_code 下单却没快照 plan_id: %d", got.PlanID)
	}
	if got.PlanCode != "code_77" {
		t.Errorf("plan_code = %q，期望 trim 后的 code_77", got.PlanCode)
	}
	if len([]rune(got.Title)) != 200 || !strings.HasSuffix(got.Title, "...") {
		t.Errorf("title 未按列宽裁剪: len=%d", len([]rune(got.Title)))
	}
	if fx.pay.createCalls[0].GetSubject() != truncate(got.Title, maxTitleLen) {
		t.Error("受理标题应与订单快照一致")
	}

	// 调用方也不给名字：留空，不编造
	plan.Name = ""
	fx2 := newCreateFixture(t, plan)
	in2 := membershipReq(77, "req-no-title")
	in2.PlanId = 0
	in2.PlanCode = "code_77"
	r2, err := fx2.l.CreateOrder(in2)
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if r2.GetOrder().GetTitle() != "" {
		t.Errorf("title = %q，期望留空而不是编一个", r2.GetOrder().GetTitle())
	}
}

// TestCreateOrderCurrencyFallbackChain 币种：套餐币种大写优先，空则用配置默认，
// 再空兜底 CNY，并且按列宽 8 裁切。
func TestCreateOrderCurrencyFallbackChain(t *testing.T) {
	cases := []struct {
		name    string
		planCur string
		confDef string
		want    string
		maxLen  int
	}{
		{"套餐小写币种归一", "cny", "USD", "CNY", 8},
		{"套餐缺币种用配置", "  ", "usd", "USD", 8},
		{"都没有兜底 CNY", "", "", "CNY", 8},
		{"超长按列宽裁切", "DOLLARISE", "CNY", "DOLLA...", 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM)
			plan.Currency = tc.planCur
			fx := newCreateFixture(t, plan)
			fx.svc.Config.TradeOrder.DefaultCurrency = tc.confDef
			reply, err := fx.l.CreateOrder(membershipReq(77, "req-currency"))
			if err != nil {
				t.Fatalf("CreateOrder() error = %v", err)
			}
			if got := reply.GetOrder().GetCurrency(); got != tc.want {
				t.Errorf("currency = %q，期望 %q", got, tc.want)
			}
			if len(reply.GetOrder().GetCurrency()) > tc.maxLen {
				t.Errorf("currency 超列宽: %q", reply.GetOrder().GetCurrency())
			}
		})
	}
}

// TestCreateOrderBalancePayMethodMapsToBalanceChannel 另一档支付方式的映射
// （mapPayMethod 显式列两档，缺映射必须报错而不是当沙箱处理）。
func TestCreateOrderBalancePayMethodMapsToBalanceChannel(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	in := membershipReq(77, "req-balance")
	in.PayMethod = rpc.PayMethod_PAY_METHOD_BALANCE
	reply, err := fx.l.CreateOrder(in)
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	if got := fx.pay.createCalls[0].GetMethod(); got != paymentrpc.PayMethod_PAY_METHOD_BALANCE {
		t.Errorf("pay_method 映射 = %v，期望 BALANCE", got)
	}
	if got := mustOrder(t, fx.db, reply.GetOrder().GetOrderNo()); got.PayMethod != model.PayBalance {
		t.Errorf("落库 pay_method = %d，期望 %d", got.PayMethod, model.PayBalance)
	}
}

// TestCreateOrderLongRequestIDTruncatedToColumnWidth 幂等键超列宽时按 rune 裁切，
// 并且裁切后的值同时用于主表与台账（否则唯一索引会写坏）。
func TestCreateOrderLongRequestIDTruncatedToColumnWidth(t *testing.T) {
	fx := newCreateFixture(t, defaultPlan(77, memberrpc.VipType_VIP_TYPE_PREMIUM))
	in := membershipReq(77, strings.Repeat("请", 100))
	reply, err := fx.l.CreateOrder(in)
	if err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	got := mustOrder(t, fx.db, reply.GetOrder().GetOrderNo())
	if len([]rune(got.RequestID)) != maxRequestIDLen {
		t.Errorf("request_id 长度 = %d，期望 %d", len([]rune(got.RequestID)), maxRequestIDLen)
	}
	if !strings.HasSuffix(got.RequestID, "...") {
		t.Errorf("request_id 尾部 = %q，期望带截断标记", got.RequestID[len([]rune(got.RequestID))-3:])
	}
	for _, e := range ledgerOf(fx.db, got.OrderNo) {
		if len([]rune(e.RequestID)) > maxRequestIDLen {
			t.Errorf("台账 request_id 超列宽: %q", e.RequestID)
		}
	}
}
