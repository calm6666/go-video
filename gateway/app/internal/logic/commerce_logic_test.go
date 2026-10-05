package logic

import (
	"context"
	"strings"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"
	paymentrpc "go-video/services/payment/rpc"
	tradeorderrpc "go-video/services/trade-order/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向商业化五域（membership / coin / payment / trade-order / creator-revenue）
// 的口径：闸门拦在哪、幂等键怎么透传、operator 怎么渲染、可选过滤位怎么原样传递、
// 下游结论（found / duplicated / accepted）怎么投影成 Code:0 信封。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。
// 金额、状态机推进、权益判定都不在此（金额与状态归 trade-order/payment，权益归 membership）。

// commerceOrderInfo 一条典型的已支付会员单，供多个用例复用。
func commerceOrderInfo() *tradeorderrpc.OrderInfo {
	return &tradeorderrpc.OrderInfo{
		OrderNo:     "O100",
		BizType:     tradeorderrpc.OrderBizType_ORDER_BIZ_TYPE_MEMBERSHIP,
		AmountMinor: 1200,
		State:       tradeorderrpc.OrderState_ORDER_STATE_REFUND_REQUESTED,
		PayMethod:   tradeorderrpc.PayMethod_PAY_METHOD_BALANCE,
	}
}

type fakeTradeOrderClient struct {
	tradeorderrpc.TradeOrderClient

	err   error
	calls int

	createReq   *tradeorderrpc.CreateOrderReq
	createReply *tradeorderrpc.CreateOrderReply
	createCall  int

	refundReq   *tradeorderrpc.RequestRefundReq
	refundReply *tradeorderrpc.RequestRefundReply
	refundCall  int

	getReq    *tradeorderrpc.GetOrderReq
	getReply  *tradeorderrpc.GetOrderReply
	getCall   int
	eventsReq *tradeorderrpc.ListOrderEventsReq
	eventsRep *tradeorderrpc.ListOrderEventsReply
	eventsCal int
}

func (f *fakeTradeOrderClient) CreateOrder(_ context.Context, in *tradeorderrpc.CreateOrderReq,
	_ ...grpc.CallOption) (*tradeorderrpc.CreateOrderReply, error) {
	f.calls++
	f.createCall++
	f.createReq = in
	return f.createReply, f.err
}

func (f *fakeTradeOrderClient) RequestRefund(_ context.Context, in *tradeorderrpc.RequestRefundReq,
	_ ...grpc.CallOption) (*tradeorderrpc.RequestRefundReply, error) {
	f.calls++
	f.refundCall++
	f.refundReq = in
	return f.refundReply, f.err
}

func (f *fakeTradeOrderClient) GetOrder(_ context.Context, in *tradeorderrpc.GetOrderReq,
	_ ...grpc.CallOption) (*tradeorderrpc.GetOrderReply, error) {
	f.calls++
	f.getCall++
	f.getReq = in
	return f.getReply, f.err
}

func (f *fakeTradeOrderClient) ListOrderEvents(_ context.Context, in *tradeorderrpc.ListOrderEventsReq,
	_ ...grpc.CallOption) (*tradeorderrpc.ListOrderEventsReply, error) {
	f.calls++
	f.eventsCal++
	f.eventsReq = in
	return f.eventsRep, f.err
}

type fakeCreatorRevenueClient struct {
	creatorrevenuerpc.CreatorRevenueClient

	err   error
	calls int

	summaryReq   *creatorrevenuerpc.GetRevenueSummaryReq
	summaryReply *creatorrevenuerpc.GetRevenueSummaryReply

	rulesReq   *creatorrevenuerpc.ListRevenueRulesReq
	rulesReply *creatorrevenuerpc.ListRevenueRulesReply

	getEnrollReq   *creatorrevenuerpc.GetEnrollmentReq
	getEnrollReply *creatorrevenuerpc.GetEnrollmentReply

	enrollReq   *creatorrevenuerpc.EnrollCreatorReq
	enrollReply *creatorrevenuerpc.EnrollCreatorReply

	leaveReq   *creatorrevenuerpc.LeavePlanReq
	leaveReply *creatorrevenuerpc.LeavePlanReply

	metricsReq   *creatorrevenuerpc.ListRevenueMetricsReq
	metricsReply *creatorrevenuerpc.ListRevenueMetricsReply

	settlesReq   *creatorrevenuerpc.ListSettlementsReq
	settlesReply *creatorrevenuerpc.ListSettlementsReply

	settleReq   *creatorrevenuerpc.GetSettlementReq
	settleReply *creatorrevenuerpc.GetSettlementReply
}

func (f *fakeCreatorRevenueClient) GetRevenueSummary(_ context.Context, in *creatorrevenuerpc.GetRevenueSummaryReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.GetRevenueSummaryReply, error) {
	f.calls++
	f.summaryReq = in
	return f.summaryReply, f.err
}

func (f *fakeCreatorRevenueClient) ListRevenueRules(_ context.Context, in *creatorrevenuerpc.ListRevenueRulesReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.ListRevenueRulesReply, error) {
	f.calls++
	f.rulesReq = in
	return f.rulesReply, f.err
}

func (f *fakeCreatorRevenueClient) GetEnrollment(_ context.Context, in *creatorrevenuerpc.GetEnrollmentReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.GetEnrollmentReply, error) {
	f.calls++
	f.getEnrollReq = in
	return f.getEnrollReply, f.err
}

func (f *fakeCreatorRevenueClient) EnrollCreator(_ context.Context, in *creatorrevenuerpc.EnrollCreatorReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.EnrollCreatorReply, error) {
	f.calls++
	f.enrollReq = in
	return f.enrollReply, f.err
}

func (f *fakeCreatorRevenueClient) LeavePlan(_ context.Context, in *creatorrevenuerpc.LeavePlanReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.LeavePlanReply, error) {
	f.calls++
	f.leaveReq = in
	return f.leaveReply, f.err
}

func (f *fakeCreatorRevenueClient) ListRevenueMetrics(_ context.Context, in *creatorrevenuerpc.ListRevenueMetricsReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.ListRevenueMetricsReply, error) {
	f.calls++
	f.metricsReq = in
	return f.metricsReply, f.err
}

func (f *fakeCreatorRevenueClient) ListSettlements(_ context.Context, in *creatorrevenuerpc.ListSettlementsReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.ListSettlementsReply, error) {
	f.calls++
	f.settlesReq = in
	return f.settlesReply, f.err
}

func (f *fakeCreatorRevenueClient) GetSettlement(_ context.Context, in *creatorrevenuerpc.GetSettlementReq,
	_ ...grpc.CallOption) (*creatorrevenuerpc.GetSettlementReply, error) {
	f.calls++
	f.settleReq = in
	return f.settleReply, f.err
}

type fakePaymentClient struct {
	paymentrpc.PaymentClient

	err   error
	calls int

	openReq   *paymentrpc.OpenRechargeReq
	openReply *paymentrpc.OpenRechargeReply
}

func (f *fakePaymentClient) OpenRecharge(_ context.Context, in *paymentrpc.OpenRechargeReq,
	_ ...grpc.CallOption) (*paymentrpc.OpenRechargeReply, error) {
	f.calls++
	f.openReq = in
	return f.openReply, f.err
}

// ---------- trade-order ----------

func TestCommerceOrderCreatePassesIdempotencyKeyVerbatim(t *testing.T) {
	// 幂等键前后各一个空格：TrimSpace 只用于判空，透传必须是原值。
	// 改一个字符等于换键，会造成重复扣款 / 重复发放。
	const requestID = " req-create-001 "
	fake := &fakeTradeOrderClient{createReply: &tradeorderrpc.CreateOrderReply{
		Duplicated:   true,
		Accepted:     false,
		RejectReason: "余额不足",
		Order:        commerceOrderInfo(),
	}}
	resp, err := NewOrderCreateLogic(context.Background(), &svc.ServiceContext{TradeOrder: fake}).
		OrderCreate(&types.ParamOrderCreate{
			Mid: 777, BizType: 1, PlanId: 9, PayMethod: 1, AmountMinor: 1200, RequestId: requestID,
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.createReq.GetRequestId() != requestID {
		t.Fatalf("request_id = %q, want 原样 %q", fake.createReq.GetRequestId(), requestID)
	}
	// 客户端上报金额原样交给服务做一致性校验，网关不重算也不裁剪。
	if fake.createReq.GetAmountMinor() != 1200 {
		t.Fatalf("amount_minor = %d, want 1200", fake.createReq.GetAmountMinor())
	}
	// accepted=false + reject_reason 是业务结论而不是错误：信封仍回 Code:0。
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	if resp.Data.Accepted || resp.Data.RejectReason != "余额不足" || !resp.Data.Duplicated {
		t.Fatalf("结论位 = accepted=%t reason=%q duplicated=%t",
			resp.Data.Accepted, resp.Data.RejectReason, resp.Data.Duplicated)
	}
	if resp.Data.Order.OrderNo != "O100" || resp.Data.Order.AmountMinor != 1200 {
		t.Fatalf("order = %+v", resp.Data.Order)
	}

	// 空 request_id 在网关拒绝，不拿去问下游。
	zero := &fakeTradeOrderClient{}
	if _, err := NewOrderCreateLogic(context.Background(), &svc.ServiceContext{TradeOrder: zero}).
		OrderCreate(&types.ParamOrderCreate{Mid: 777, BizType: 1, PlanId: 9, PayMethod: 1, RequestId: "   "}); err == nil {
		t.Fatal("空 request_id 应被拒绝")
	}
	if zero.calls != 0 {
		t.Fatalf("闸门未过不得调下游，实际 %d 次", zero.calls)
	}
}

func TestCommerceOrderRefundRequestGatesAndSelfOperator(t *testing.T) {
	fake := &fakeTradeOrderClient{refundReply: &tradeorderrpc.RequestRefundReply{
		Duplicated: true,
		Order:      commerceOrderInfo(),
	}}
	resp, err := NewOrderRefundLogic(context.Background(), &svc.ServiceContext{TradeOrder: fake}).
		OrderRefund(&types.ParamOrderRefundRequest{
			Mid: 777, OrderNo: "O100", Reason: "买错档", RequestId: " req-refund-001 ",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.refundReq.GetOperator() != commerceSelfOperator {
		t.Fatalf("operator = %q, want 自助身份 %q", fake.refundReq.GetOperator(), commerceSelfOperator)
	}
	if fake.refundReq.GetRequestId() != " req-refund-001 " {
		t.Fatalf("request_id = %q, want 原样透传", fake.refundReq.GetRequestId())
	}
	// amount_minor 未给：保持 0（=全额），网关不补默认值。
	if fake.refundReq.GetAmountMinor() != 0 {
		t.Fatalf("amount_minor = %d, want 0（0 才是「全额」语义）", fake.refundReq.GetAmountMinor())
	}
	// 申请退款 ≠ 已退款：投影服务给的 state，网关不猜。
	if resp.Code != 0 || !resp.Data.Duplicated || resp.Data.Order.State != int32(tradeorderrpc.OrderState_ORDER_STATE_REFUND_REQUESTED) {
		t.Fatalf("resp = %+v", resp)
	}

	// 四个必填位逐个挡：缺任何都不打下游。
	cases := []struct {
		name string
		req  *types.ParamOrderRefundRequest
		want string
	}{
		{"mid", &types.ParamOrderRefundRequest{OrderNo: "O1", Reason: "r", RequestId: "q"}, "mid"},
		{"order_no", &types.ParamOrderRefundRequest{Mid: 777, Reason: "r", RequestId: "q"}, "order_no"},
		{"request_id", &types.ParamOrderRefundRequest{Mid: 777, OrderNo: "O1", Reason: "r"}, "request_id"},
		// 退款要进变更台账与对账，不能无理由。
		{"reason", &types.ParamOrderRefundRequest{Mid: 777, OrderNo: "O1", RequestId: "q"}, "reason"},
	}
	for _, tc := range cases {
		blocked := &fakeTradeOrderClient{}
		_, err := NewOrderRefundLogic(context.Background(), &svc.ServiceContext{TradeOrder: blocked}).
			OrderRefund(tc.req)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s 闸门 = %v, want 点名 %s", tc.name, err, tc.want)
		}
		if blocked.calls != 0 {
			t.Fatalf("%s 闸门未过不得调下游，实际 %d 次", tc.name, blocked.calls)
		}
	}
}

func TestCommerceOrderEventsChecksOwnershipBeforeLedger(t *testing.T) {
	// ListOrderEventsReq 没有 mid 位（服务侧注释「不校验归属：本方法是运营/排障入口」），
	// 因此终端必须先按 mid 归属校验，再读台账。
	fake := &fakeTradeOrderClient{
		getReply: &tradeorderrpc.GetOrderReply{Found: true, Order: commerceOrderInfo()},
		eventsRep: &tradeorderrpc.ListOrderEventsReply{
			Events: []*tradeorderrpc.OrderEventInfo{{
				EventId: 1, OrderNo: "O100", Reason: "支付成功", Ctime: 111,
			}},
			Total: 1, Page: 1, Size: 20,
		},
	}
	resp, err := NewOrderEventsLogic(context.Background(), &svc.ServiceContext{TradeOrder: fake}).
		OrderEvents(&types.ParamOrderEvents{Mid: 777, OrderNo: "O100", Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.getCall != 1 || fake.eventsCal != 1 {
		t.Fatalf("calls get=%d events=%d, want 各 1 次", fake.getCall, fake.eventsCal)
	}
	if fake.getReq.GetMid() != 777 {
		t.Fatalf("归属校验 mid = %d, want 777", fake.getReq.GetMid())
	}
	if fake.eventsReq.GetOrderNo() != "O100" || fake.eventsReq.GetPage() != 1 || fake.eventsReq.GetSize() != 20 {
		t.Fatalf("台账请求 = %+v", fake.eventsReq)
	}
	if len(resp.Data.Events) != 1 || resp.Data.Events[0].EventId != 1 || resp.Data.Events[0].Reason != "支付成功" {
		t.Fatalf("events = %+v", resp.Data.Events)
	}
	if resp.Data.Total != 1 || resp.Data.Page != 1 || resp.Data.PageSize != 20 || resp.TTL != 0 {
		t.Fatalf("分页/信封 = %+v ttl=%d", resp.Data, resp.TTL)
	}

	// 非本人 / 不存在：不回台账，也不区分两种情况（服务侧刻意回 not-found）。
	other := &fakeTradeOrderClient{getReply: &tradeorderrpc.GetOrderReply{Found: false}}
	notFound, err := NewOrderEventsLogic(context.Background(), &svc.ServiceContext{TradeOrder: other}).
		OrderEvents(&types.ParamOrderEvents{Mid: 777, OrderNo: "O100", Page: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if other.eventsCal != 0 {
		t.Fatal("归属不通过时不得读别人的台账")
	}
	if notFound.Code != 0 || notFound.Data.Events == nil || len(notFound.Data.Events) != 0 || notFound.Data.Total != 0 {
		t.Fatalf("not-found 应回空台账 + Code:0，got %+v", notFound)
	}
	if notFound.Data.Page != 2 {
		t.Fatalf("page = %d, want 回显请求页 2", notFound.Data.Page)
	}
}

// ---------- payment ----------

func TestCommerceWalletRechargeOpenKeepsSandboxSemantics(t *testing.T) {
	fake := &fakePaymentClient{openReply: &paymentrpc.OpenRechargeReply{
		Duplicated: false,
		Recharge: &paymentrpc.RechargeInfo{
			RechargeNo: "R1", AmountMinor: 5000, Currency: "CNY", State: paymentrpc.RechargeState_RECHARGE_STATE_PENDING,
		},
	}}
	resp, err := NewWalletRechargeOpenLogic(context.Background(), &svc.ServiceContext{Payment: fake}).
		WalletRechargeOpen(&types.ParamWalletRechargeOpen{
			Mid: 777, AmountMinor: 5000, Currency: "CNY", RequestId: " req-open-001 ",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.openReq.GetRequestId() != " req-open-001 " {
		t.Fatalf("request_id = %q, want 原样透传", fake.openReq.GetRequestId())
	}
	// 金额位由 payment 记账，网关不换算；这里只做原值投影。
	if resp.Data.Recharge.RechargeNo != "R1" || resp.Data.Recharge.AmountMinor != 5000 {
		t.Fatalf("recharge = %+v", resp.Data.Recharge)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
}

// ---------- creator-revenue ----------

func TestCommerceRevSummaryProjectsAccrualNotPayout(t *testing.T) {
	cases := []struct {
		state        creatorrevenuerpc.EnrollmentState
		wantEnrolled bool
	}{
		{creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_UNSPECIFIED, false},
		{creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_ENROLLED, true},
		{creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_LEFT, false},
		// 暂停仍算「在计划里」，只是本周期不结算：口径靠 enrollment_state 区分，不能压成 false。
		{creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_SUSPENDED, true},
	}
	for _, tc := range cases {
		fake := &fakeCreatorRevenueClient{summaryReply: &creatorrevenuerpc.GetRevenueSummaryReply{
			Enrollment:           &creatorrevenuerpc.EnrollmentInfo{Mid: 777, State: tc.state},
			CurrentPeriod:        "202609",
			CurrentEstimateMinor: 8800,
			TotalConfirmedMinor:  21000,
			LastSettledPeriod:    202608,
			PayoutAvailable:      false,
			PayoutNote:           "本项目未接出金通道",
		}}
		resp, err := NewRevSummaryLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: fake}).
			RevSummary(&types.ParamRevSummary{Mid: 777})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Data.Enrolled != tc.wantEnrolled || resp.Data.EnrollmentState != int32(tc.state) {
			t.Fatalf("state=%d -> enrolled=%t state=%d, want %t/%d",
				tc.state, resp.Data.Enrolled, resp.Data.EnrollmentState, tc.wantEnrolled, tc.state)
		}
		if resp.Data.CurrentPeriod != "202609" || resp.Data.CurrentEstimateMinor != 8800 ||
			resp.Data.TotalConfirmedMinor != 21000 || resp.Data.LastSettledPeriod != 202608 {
			t.Fatalf("金额位 = %+v", resp.Data)
		}
		// 「已确认应计」不等于「已到账」：payout 两位必须原样透出、不得省略或推断。
		if resp.Data.PayoutAvailable || resp.Data.PayoutNote == "" {
			t.Fatalf("payout 语义丢失 = available=%t note=%q", resp.Data.PayoutAvailable, resp.Data.PayoutNote)
		}
		if resp.TTL != 0 {
			t.Fatalf("ttl = %d, want 0（预估会随更正变动）", resp.TTL)
		}
	}

	blocked := &fakeCreatorRevenueClient{}
	if _, err := NewRevSummaryLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: blocked}).
		RevSummary(&types.ParamRevSummary{}); err == nil {
		t.Fatal("mid 缺失应被拒绝")
	}
	if blocked.calls != 0 {
		t.Fatalf("闸门未过不得调下游，实际 %d 次", blocked.calls)
	}
}

func TestCommerceRevRulesPinsActiveStateOnly(t *testing.T) {
	fake := &fakeCreatorRevenueClient{rulesReply: &creatorrevenuerpc.ListRevenueRulesReply{
		Rules: []*creatorrevenuerpc.RevenueRuleInfo{{
			RuleCode:               "vip_watch_minute",
			SourceType:             creatorrevenuerpc.RevenueSourceType_REVENUE_SOURCE_VIP_WATCH,
			UnitPricePer_1000Minor: 120,
			State:                  creatorrevenuerpc.RuleState_RULE_STATE_ACTIVE,
			Version:                7,
		}},
		Total: 1, Page: 1, Size: 20,
	}}
	resp, err := NewRevRulesLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: fake}).
		RevRules(&types.ParamRevRules{SourceType: 1, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 终端面固定只读 ACTIVE：DRAFT/ARCHIVED 单价泄漏出去就是「把没生效的价当承诺价」。
	if fake.rulesReq.GetState() != creatorrevenuerpc.RuleState_RULE_STATE_ACTIVE {
		t.Fatalf("state = %d, want 固定 ACTIVE", fake.rulesReq.GetState())
	}
	if fake.rulesReq.GetSourceType() != creatorrevenuerpc.RevenueSourceType_REVENUE_SOURCE_VIP_WATCH {
		t.Fatalf("source_type = %d, want 原样透传", fake.rulesReq.GetSourceType())
	}
	if len(resp.Data.Rules) != 1 || resp.Data.Rules[0].UnitPricePer1000Minor != 120 || resp.Data.Rules[0].Version != 7 {
		t.Fatalf("rules = %+v", resp.Data.Rules)
	}
	if resp.TTL != 60 {
		t.Fatalf("ttl = %d, want 60（公开目录数据，与套餐列表同口径）", resp.TTL)
	}
}

func TestCommerceRevEnrollmentNotFoundIsConclusionNotError(t *testing.T) {
	fake := &fakeCreatorRevenueClient{getEnrollReply: &creatorrevenuerpc.GetEnrollmentReply{Found: false}}
	resp, err := NewRevEnrollmentLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: fake}).
		RevEnrollment(&types.ParamRevEnrollment{Mid: 777})
	if err != nil {
		t.Fatalf("没参加过不是错误: %v", err)
	}
	if fake.getEnrollReq.GetMid() != 777 {
		t.Fatalf("mid = %d, want 777", fake.getEnrollReq.GetMid())
	}
	if resp.Code != 0 || resp.Found || resp.Data.Mid != 0 || resp.Data.State != 0 || resp.TTL != 0 {
		t.Fatalf("resp = %+v", resp)
	}

	yes := &fakeCreatorRevenueClient{getEnrollReply: &creatorrevenuerpc.GetEnrollmentReply{
		Found: true,
		Enrollment: &creatorrevenuerpc.EnrollmentInfo{
			Mid: 777, State: creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_ENROLLED,
			AgreedRuleVersion: 7, EnrolledAt: 100,
			Operator: "user", Remark: "运营内部备注（含工号）",
		},
	}}
	yesResp, err := NewRevEnrollmentLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: yes}).
		RevEnrollment(&types.ParamRevEnrollment{Mid: 777})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !yesResp.Found || yesResp.Data.AgreedRuleVersion != 7 || yesResp.Data.EnrolledAt != 100 {
		t.Fatalf("yesResp = %+v", yesResp)
	}
	// types.RevEnrollment 没有 operator / remark 位：运营工号与内部备注不透给终端，
	// 下游即使填了也只能停在投影函数之外。
	if got := revEnrollmentToAPI(yes.getEnrollReply.GetEnrollment()); got.Mid != 777 || got.State != 1 || got.LeftAt != 0 {
		t.Fatalf("投影 = %+v", got)
	}
}

func TestCommerceRevEnrollGatesRuleVersionAndOperator(t *testing.T) {
	fake := &fakeCreatorRevenueClient{enrollReply: &creatorrevenuerpc.EnrollCreatorReply{
		Duplicated: true,
		Enrollment: &creatorrevenuerpc.EnrollmentInfo{Mid: 777, State: creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_ENROLLED, AgreedRuleVersion: 7},
	}}
	resp, err := NewRevEnrollLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: fake}).
		RevEnroll(&types.ParamRevEnroll{Mid: 777, AgreedRuleVersion: 7, RequestId: " req-enroll-001 "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.enrollReq.GetOperator() != commerceSelfOperator {
		t.Fatalf("operator = %q, want 自助身份", fake.enrollReq.GetOperator())
	}
	if fake.enrollReq.GetRequestId() != " req-enroll-001 " {
		t.Fatalf("request_id = %q, want 原样透传", fake.enrollReq.GetRequestId())
	}
	if fake.enrollReq.GetAgreedRuleVersion() != 7 {
		t.Fatalf("agreed_rule_version = %d, want 7", fake.enrollReq.GetAgreedRuleVersion())
	}
	if !resp.Data.Duplicated || resp.Data.Enrollment.AgreedRuleVersion != 7 || resp.TTL != 0 {
		t.Fatalf("resp = %+v", resp)
	}

	// agreed_rule_version 是「用户确认过哪版条款」的凭据：0/负数挡下，也不替客户端补最新版本号
	//（补一个就等于网关代用户确认了他没看过的条款）。
	for _, v := range []int64{0, -7} {
		blocked := &fakeCreatorRevenueClient{}
		_, err := NewRevEnrollLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: blocked}).
			RevEnroll(&types.ParamRevEnroll{Mid: 777, AgreedRuleVersion: v, RequestId: "q"})
		if err == nil || !strings.Contains(err.Error(), "agreed_rule_version") {
			t.Fatalf("version=%d 闸门 = %v", v, err)
		}
		if blocked.calls != 0 {
			t.Fatalf("version=%d 闸门未过不得调下游", v)
		}
	}
}

func TestCommerceRevLeaveSelfServiceNeedsNoReason(t *testing.T) {
	fake := &fakeCreatorRevenueClient{leaveReply: &creatorrevenuerpc.LeavePlanReply{
		Duplicated: false,
		Enrollment: &creatorrevenuerpc.EnrollmentInfo{Mid: 777, State: creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_LEFT, LeftAt: 200},
	}}
	resp, err := NewRevLeaveLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: fake}).
		RevLeave(&types.ParamRevLeave{Mid: 777, RequestId: " req-leave-001 "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.leaveReq.GetOperator() != commerceSelfOperator {
		t.Fatalf("operator = %q, want 自助身份", fake.leaveReq.GetOperator())
	}
	// reason 在契约里是「运营代操作必填」：自助退出没有理由位，网关留空而不是代客户端编一个。
	if fake.leaveReq.GetReason() != "" {
		t.Fatalf("reason = %q, want 空串", fake.leaveReq.GetReason())
	}
	if fake.leaveReq.GetRequestId() != " req-leave-001 " {
		t.Fatalf("request_id = %q, want 原样透传", fake.leaveReq.GetRequestId())
	}
	if resp.Data.Enrollment.State != int32(creatorrevenuerpc.EnrollmentState_ENROLLMENT_STATE_LEFT) ||
		resp.Data.Enrollment.LeftAt != 200 || resp.Data.Duplicated {
		t.Fatalf("resp = %+v", resp)
	}

	blocked := &fakeCreatorRevenueClient{}
	if _, err := NewRevLeaveLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: blocked}).
		RevLeave(&types.ParamRevLeave{Mid: 777}); err == nil {
		t.Fatal("request_id 缺失应被拒绝")
	}
	if blocked.calls != 0 {
		t.Fatalf("闸门未过不得调下游，实际 %d 次", blocked.calls)
	}
}

func TestCommerceRevLedgersPassFiltersAndProjectEmptyLists(t *testing.T) {
	metrics := &fakeCreatorRevenueClient{metricsReply: &creatorrevenuerpc.ListRevenueMetricsReply{}}
	mResp, err := NewRevMetricsLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: metrics}).
		RevMetrics(&types.ParamRevMetrics{Mid: 777})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 可选过滤位不过滤时原样传空值，网关不补当前月、不改 0 为 1。
	if metrics.metricsReq.GetPeriod() != "" || metrics.metricsReq.GetAid() != 0 ||
		metrics.metricsReq.GetSourceType() != creatorrevenuerpc.RevenueSourceType_REVENUE_SOURCE_TYPE_UNSPECIFIED {
		t.Fatalf("metrics 过滤位 = %+v", metrics.metricsReq)
	}
	if metrics.metricsReq.GetMid() != 777 {
		t.Fatalf("metrics mid = %d, want 777（只读本人台账）", metrics.metricsReq.GetMid())
	}
	if mResp.Data.Metrics == nil || len(mResp.Data.Metrics) != 0 {
		t.Fatalf("metrics = %v, want 空数组（JSON 不能出 null）", mResp.Data.Metrics)
	}

	settles := &fakeCreatorRevenueClient{settlesReply: &creatorrevenuerpc.ListSettlementsReply{
		Settlements: []*creatorrevenuerpc.SettlementInfo{{
			SettlementNo: "S1", Period: "202608", AmountMinor: 9000,
			State:       creatorrevenuerpc.SettlementState_SETTLEMENT_STATE_CONFIRMED,
			PayoutState: creatorrevenuerpc.PayoutState_PAYOUT_STATE_NOT_PAYABLE,
		}},
		Total: 1, Page: 1, Size: 20,
	}}
	sResp, err := NewRevSettlementsLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: settles}).
		RevSettlements(&types.ParamRevSettlements{Mid: 777, Period: "202608", State: 2, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if settles.settlesReq.GetMid() != 777 || settles.settlesReq.GetPeriod() != "202608" ||
		settles.settlesReq.GetState() != creatorrevenuerpc.SettlementState_SETTLEMENT_STATE_CONFIRMED {
		t.Fatalf("settlements 请求 = %+v", settles.settlesReq)
	}
	if len(sResp.Data.Settlements) != 1 {
		t.Fatalf("settlements = %+v", sResp.Data.Settlements)
	}
	got := sResp.Data.Settlements[0]
	// state=CONFIRMED 只是「金额冻结不再重算」，payout_state 必须原样透出，不能被推断成已到账。
	if got.State != int32(creatorrevenuerpc.SettlementState_SETTLEMENT_STATE_CONFIRMED) ||
		got.PayoutState != int32(creatorrevenuerpc.PayoutState_PAYOUT_STATE_NOT_PAYABLE) {
		t.Fatalf("结算单状态位 = %+v", got)
	}
	if got.AmountMinor != 9000 || got.Period != "202608" || sResp.TTL != 0 {
		t.Fatalf("结算单金额/周期 = %+v ttl=%d", got, sResp.TTL)
	}
}

func TestCommerceRevSettlementDetailAlwaysCarriesMid(t *testing.T) {
	fake := &fakeCreatorRevenueClient{settleReply: &creatorrevenuerpc.GetSettlementReply{
		Found: true,
		Settlement: &creatorrevenuerpc.SettlementInfo{
			SettlementNo: "S1", Period: "202608", AmountMinor: 9000, CapAppliedMinor: 1000,
			Currency: "CNY", MetricCount: 2,
			State:       creatorrevenuerpc.SettlementState_SETTLEMENT_STATE_DRAFT,
			PayoutState: creatorrevenuerpc.PayoutState_PAYOUT_STATE_NOT_PAYABLE,
		},
		Items: []*creatorrevenuerpc.SettlementItem{
			{SourceType: creatorrevenuerpc.RevenueSourceType_REVENUE_SOURCE_VIP_WATCH, RuleCode: "vip_watch_minute", Quantity: 5000, AmountMinor: 600},
		},
	}}
	resp, err := NewRevSettlementLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: fake}).
		RevSettlement(&types.ParamRevSettlement{Mid: 777, SettlementNo: "S1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// GetSettlementReq.mid 标注「非 0 时校验归属」：本路由 mid 必填且为正，校验恒生效。
	if fake.settleReq.GetMid() != 777 || fake.settleReq.GetSettlementNo() != "S1" {
		t.Fatalf("settlement 请求 = %+v", fake.settleReq)
	}
	if !resp.Data.Found || resp.Data.Settlement.SettlementNo != "S1" ||
		resp.Data.Settlement.CapAppliedMinor != 1000 || resp.Data.Settlement.Currency != "CNY" {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Data.Items) != 1 || resp.Data.Items[0].RuleCode != "vip_watch_minute" ||
		resp.Data.Items[0].AmountMinor != 600 {
		t.Fatalf("items = %+v", resp.Data.Items)
	}

	empty := &fakeCreatorRevenueClient{settleReply: &creatorrevenuerpc.GetSettlementReply{Found: false}}
	eResp, err := NewRevSettlementLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: empty}).
		RevSettlement(&types.ParamRevSettlement{Mid: 777, SettlementNo: "S404"})
	if err != nil {
		t.Fatalf("not-found 不是错误: %v", err)
	}
	if eResp.Code != 0 || eResp.Data.Found || eResp.Data.Items == nil || len(eResp.Data.Items) != 0 {
		t.Fatalf("not-found 投影 = %+v", eResp)
	}

	blocked := &fakeCreatorRevenueClient{}
	if _, err := NewRevSettlementLogic(context.Background(), &svc.ServiceContext{CreatorRevenue: blocked}).
		RevSettlement(&types.ParamRevSettlement{Mid: 777}); err == nil {
		t.Fatal("settlement_no 缺失应被拒绝")
	}
	if blocked.calls != 0 {
		t.Fatalf("闸门未过不得调下游，实际 %d 次", blocked.calls)
	}
}

// ---------- 统一闸门：客户端未配置下游时不得静默回空数据 ----------

func TestCommerceRoutesFailLoudlyWithoutClient(t *testing.T) {
	cases := []struct {
		name string
		call func(s *svc.ServiceContext) error
	}{
		{"orderRefund", func(s *svc.ServiceContext) error {
			_, err := NewOrderRefundLogic(context.Background(), s).OrderRefund(&types.ParamOrderRefundRequest{
				Mid: 777, OrderNo: "O1", Reason: "r", RequestId: "q",
			})
			return err
		}},
		{"orderEvents", func(s *svc.ServiceContext) error {
			_, err := NewOrderEventsLogic(context.Background(), s).OrderEvents(&types.ParamOrderEvents{Mid: 777, OrderNo: "O1"})
			return err
		}},
		{"revSummary", func(s *svc.ServiceContext) error {
			_, err := NewRevSummaryLogic(context.Background(), s).RevSummary(&types.ParamRevSummary{Mid: 777})
			return err
		}},
		{"revRules", func(s *svc.ServiceContext) error {
			_, err := NewRevRulesLogic(context.Background(), s).RevRules(&types.ParamRevRules{})
			return err
		}},
		{"revEnrollment", func(s *svc.ServiceContext) error {
			_, err := NewRevEnrollmentLogic(context.Background(), s).RevEnrollment(&types.ParamRevEnrollment{Mid: 777})
			return err
		}},
		{"revEnroll", func(s *svc.ServiceContext) error {
			_, err := NewRevEnrollLogic(context.Background(), s).RevEnroll(&types.ParamRevEnroll{Mid: 777, AgreedRuleVersion: 7, RequestId: "q"})
			return err
		}},
		{"revLeave", func(s *svc.ServiceContext) error {
			_, err := NewRevLeaveLogic(context.Background(), s).RevLeave(&types.ParamRevLeave{Mid: 777, RequestId: "q"})
			return err
		}},
		{"revMetrics", func(s *svc.ServiceContext) error {
			_, err := NewRevMetricsLogic(context.Background(), s).RevMetrics(&types.ParamRevMetrics{Mid: 777})
			return err
		}},
		{"revSettlements", func(s *svc.ServiceContext) error {
			_, err := NewRevSettlementsLogic(context.Background(), s).RevSettlements(&types.ParamRevSettlements{Mid: 777})
			return err
		}},
		{"revSettlement", func(s *svc.ServiceContext) error {
			_, err := NewRevSettlementLogic(context.Background(), s).RevSettlement(&types.ParamRevSettlement{Mid: 777, SettlementNo: "S1"})
			return err
		}},
	}
	for _, tc := range cases {
		err := tc.call(&svc.ServiceContext{})
		if err == nil {
			t.Fatalf("%s: 客户端未配置时必须报错，不能回 Code:0 空数据", tc.name)
		}
		if !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("%s: err = %v, want 点名未配置的服务", tc.name, err)
		}
	}
}
