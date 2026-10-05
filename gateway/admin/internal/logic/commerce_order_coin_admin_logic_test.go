package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	coinrpc "go-video/services/coin/rpc"
	tradeorderrpc "go-video/services/trade-order/rpc"

	"google.golang.org/grpc"
)

// 本文件只锁 gateway/admin 面向 trade-order / coin 的口径，刻意不断言两个域的业务结论
// （AGENTS.md §1/§5/§7）：订单状态机能不能推进、这单有没有绑支付单、还剩多少可退、
// 权益回收成没成、硬币 |delta| 是否超上限、会不会把余额扣成负数、幂等指纹是否一致、
// 跨用户查询有没有给窗口——全部由 services/trade-order 与 services/coin 判定，
// 网关自己复算一遍只会和服务口径漂移（本轮抓到并留在注释里的两条最容易复算过头的规则：
// 「按 flow_type 分叉的条件必填」与「expected_version=0」，都刻意**不**在网关挡）。
//
// 锁死的是网关自己的责任边界：
//  1. 不伪造成功/成功台账：没配客户端时 10 条路由一律 not configured，绝不回空订单列表、
//     found=false、全 0 投币参数或「没有卡单」；下游错误原样上抛（duplicated=true 例外，
//     那是首次结论而不是错误）。nil 客户端下一调用就会在空接口上 panic，
//     所以「跑完并拿到哨兵错误」就是「零 RPC」的机器可检证明。
//  2. 主体不信任请求体：三条写路由（refund/approve、refund/reject、coin/grant）的 operator
//     一律由会话渲染成 gateway/admin:<admin_id>，表单自称的 9001 只能进日志当线索；
//     无会话或 AdminID<=0 即 fail-closed，一次调用都不发。
//  3. 幂等键与 0 哨兵原样下传：idempotency_key→request_id 不改写；
//     page/size/mid=0/flow_type=0/states=[]/older_than_seconds=0/max_window_seconds=0
//     都是合法哨兵，网关不代填、不裁剪，分页三元组照抄服务回显而不是回显请求值。
//  4. 契约里没有的位不伪造：ListStuckOrders 没有分页 → 响应就只有 list；
//     ApproveRefundReq 没有金额位 → 网关不提供部分退款；退到的是沙箱余额而不是银行卡。
//  5. 投影逐字段不丢：台账的 operator/request_id/reason、订单的 version/*_minor、
//     账户的三项限额是审计与对账的证据位，裁掉一位后台就得靠猜。
//
// 打桩方式与 membership/payment 一致：内嵌生成的 client 接口 + 只覆盖本批用到的方法，
// 其余方法（CreateOrder/ListMyOrders/CancelOrder/BindPayment/FulfillOrder/RequestRefund 与
// TossCoin/CancelToss/ListMyTosses/Get*/ListTargetTossers）一旦被调用会 panic 在 nil 接口上——
// 这正是 admin.api 里「刻意不开的路由」那条边界的机器可检表达：买家的单只能由买家自己
// 发起/取消/申退，投币与撤币是终端用户动作，后台直连会造出第二个写主并污染互动特征。
// 不建 gRPC 连接、不碰数据库。

var errCommerceOrderDownstream = errors.New("commerce order: downstream unavailable")
var errCommerceCoinDownstream = errors.New("commerce coin: downstream unavailable")

// 下面几条逐字照抄服务侧哨兵错误的消息文本，用来验证「服务的拒绝」在网关这一侧
// 不被吞掉、不被折叠成空结果。不 import 服务包：这两个服务正在被并行改动，测试只关心
// 消息原样透出这一件事，本地副本就够（也避免测试随服务代码漂移）。
var errCommerceOrderFilterRequired = errors.New("trade-order: at least one filter required for listing")
var errCommerceOrderStuckStateNotAllowed = errors.New("trade-order: stuck scan only accepts non-terminal states")
var errCommerceOrderExpectedVersionRequired = errors.New("trade-order: expected_version required")
var errCommerceOrderRefundNotRequested = errors.New("trade-order: order has no pending refund request")
var errCommerceCoinUnboundedQuery = errors.New("coin: cross-user flow query requires time window or biz_no")
var errCommerceCoinGrantReasonRequired = errors.New("coin: admin grant requires reason")
var errCommerceCoinGrantBizNoRequired = errors.New("coin: order pack grant requires biz_no")

// --- trade-order fake ---

type commerceOrderFake struct {
	tradeorderrpc.TradeOrderClient

	calls    int
	lastCall string
	err      error

	listReq    *tradeorderrpc.ListOrdersReq
	list       *tradeorderrpc.ListOrdersReply
	getReq     *tradeorderrpc.GetOrderReq
	get        *tradeorderrpc.GetOrderReply
	eventsReq  *tradeorderrpc.ListOrderEventsReq
	events     *tradeorderrpc.ListOrderEventsReply
	stuckReq   *tradeorderrpc.ListStuckOrdersReq
	stuck      *tradeorderrpc.ListStuckOrdersReply
	approveReq *tradeorderrpc.ApproveRefundReq
	approve    *tradeorderrpc.ApproveRefundReply
	rejectReq  *tradeorderrpc.RejectRefundReq
	reject     *tradeorderrpc.RejectRefundReply
}

// record 记一次调用；返回 true 表示这次要模拟下游失败。
func (f *commerceOrderFake) record(call string) bool {
	f.calls++
	f.lastCall = call
	return f.err != nil
}

func (f *commerceOrderFake) ListOrders(_ context.Context, in *tradeorderrpc.ListOrdersReq,
	_ ...grpc.CallOption) (*tradeorderrpc.ListOrdersReply, error) {
	f.listReq = in
	if f.record("ListOrders") {
		return nil, f.err
	}
	return f.list, nil
}

func (f *commerceOrderFake) GetOrder(_ context.Context, in *tradeorderrpc.GetOrderReq,
	_ ...grpc.CallOption) (*tradeorderrpc.GetOrderReply, error) {
	f.getReq = in
	if f.record("GetOrder") {
		return nil, f.err
	}
	return f.get, nil
}

func (f *commerceOrderFake) ListOrderEvents(_ context.Context, in *tradeorderrpc.ListOrderEventsReq,
	_ ...grpc.CallOption) (*tradeorderrpc.ListOrderEventsReply, error) {
	f.eventsReq = in
	if f.record("ListOrderEvents") {
		return nil, f.err
	}
	return f.events, nil
}

func (f *commerceOrderFake) ListStuckOrders(_ context.Context, in *tradeorderrpc.ListStuckOrdersReq,
	_ ...grpc.CallOption) (*tradeorderrpc.ListStuckOrdersReply, error) {
	f.stuckReq = in
	if f.record("ListStuckOrders") {
		return nil, f.err
	}
	return f.stuck, nil
}

func (f *commerceOrderFake) ApproveRefund(_ context.Context, in *tradeorderrpc.ApproveRefundReq,
	_ ...grpc.CallOption) (*tradeorderrpc.ApproveRefundReply, error) {
	f.approveReq = in
	if f.record("ApproveRefund") {
		return nil, f.err
	}
	return f.approve, nil
}

func (f *commerceOrderFake) RejectRefund(_ context.Context, in *tradeorderrpc.RejectRefundReq,
	_ ...grpc.CallOption) (*tradeorderrpc.RejectRefundReply, error) {
	f.rejectReq = in
	if f.record("RejectRefund") {
		return nil, f.err
	}
	return f.reject, nil
}

func commerceOrderSvc(fake tradeorderrpc.TradeOrderClient) *svc.ServiceContext {
	return &svc.ServiceContext{TradeOrder: fake}
}

// --- coin fake ---

type commerceCoinFake struct {
	coinrpc.CoinClient

	calls    int
	lastCall string
	err      error

	accountReq *coinrpc.GetCoinAccountReq
	account    *coinrpc.GetCoinAccountReply
	flowsReq   *coinrpc.ListCoinFlowsReq
	flows      *coinrpc.ListCoinFlowsReply
	tossReq    *coinrpc.GetTossConfigReq
	toss       *coinrpc.GetTossConfigReply
	grantReq   *coinrpc.GrantCoinReq
	grant      *coinrpc.GrantCoinReply
}

func (f *commerceCoinFake) record(call string) bool {
	f.calls++
	f.lastCall = call
	return f.err != nil
}

func (f *commerceCoinFake) GetCoinAccount(_ context.Context, in *coinrpc.GetCoinAccountReq,
	_ ...grpc.CallOption) (*coinrpc.GetCoinAccountReply, error) {
	f.accountReq = in
	if f.record("GetCoinAccount") {
		return nil, f.err
	}
	return f.account, nil
}

func (f *commerceCoinFake) ListCoinFlows(_ context.Context, in *coinrpc.ListCoinFlowsReq,
	_ ...grpc.CallOption) (*coinrpc.ListCoinFlowsReply, error) {
	f.flowsReq = in
	if f.record("ListCoinFlows") {
		return nil, f.err
	}
	return f.flows, nil
}

func (f *commerceCoinFake) GetTossConfig(_ context.Context, in *coinrpc.GetTossConfigReq,
	_ ...grpc.CallOption) (*coinrpc.GetTossConfigReply, error) {
	f.tossReq = in
	if f.record("GetTossConfig") {
		return nil, f.err
	}
	return f.toss, nil
}

func (f *commerceCoinFake) GrantCoin(_ context.Context, in *coinrpc.GrantCoinReq,
	_ ...grpc.CallOption) (*coinrpc.GrantCoinReply, error) {
	f.grantReq = in
	if f.record("GrantCoin") {
		return nil, f.err
	}
	return f.grant, nil
}

func commerceCoinSvc(fake coinrpc.CoinClient) *svc.ServiceContext {
	return &svc.ServiceContext{Coin: fake}
}

// --- 会话身份 ---

// commerceOrderSession / commerceCoinSession 是中间件判定通过后写入 context 的会话身份。
// admin_id=77 与表单里自称的 operator=9001 故意不同，用来验证「谁赢」。
func commerceOrderSession() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77, Roles: []string{"order_operator"},
	})
}

func commerceCoinSession() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77, Roles: []string{"coin_operator"},
	})
}

// --- 台账样例（每位都非零，投影丢一位就能被 diff 点名）---

func commerceOrderFull() *tradeorderrpc.OrderInfo {
	return &tradeorderrpc.OrderInfo{
		OrderNo: "to_01HZZZZZZZZZZZZZZZZZZZZZ", Mid: 10001,
		BizType: tradeorderrpc.OrderBizType_ORDER_BIZ_TYPE_MEMBERSHIP,
		PlanId:  3001, PlanCode: "vip_month", Title: "大会员月卡",
		Quantity: 2, DurationDays: 60, CoinAmount: 0, // 会员单没有硬币数；硬币包单反过来
		UnitPriceMinor: 2500, AmountMinor: 5000, RefundedMinor: 0, Currency: "CNY",
		PayMethod:       tradeorderrpc.PayMethod_PAY_METHOD_BALANCE,
		State:           tradeorderrpc.OrderState_ORDER_STATE_REFUND_REQUESTED,
		FulfillState:    tradeorderrpc.FulfillState_FULFILL_STATE_SUCCEEDED,
		FulfillAttempts: 1, FulfillDetail: "上次履约超时摘要（不含堆栈与 PII）",
		PaymentNo: "PM01HZZZZZZZZZZZZZZZZZZZZZ", GrantRef: "membership_grant:8801",
		ExpireAt: 1700000900, ClientTraceId: "ct-order-1",
		Platform:  tradeorderrpc.Platform_PLATFORM_HARMONY,
		RequestId: "k-create-1", Version: 7,
		CreatedAt: 1700000000, UpdatedAt: 1700000600, PaidAt: 1700000300,
		FulfilledAt: 1700000400, ClosedAt: 0,
	}
}

func commerceOrderFullEvent() *tradeorderrpc.OrderEventInfo {
	return &tradeorderrpc.OrderEventInfo{
		EventId: 4001, OrderNo: "to_01HZZZZZZZZZZZZZZZZZZZZZ",
		FromState: tradeorderrpc.OrderState_ORDER_STATE_FULFILLED,
		ToState:   tradeorderrpc.OrderState_ORDER_STATE_REFUND_REQUESTED,
		Operator:  "gateway/admin:77", Reason: "用户申退：买错档位", Ctime: 1700000600,
	}
}

func commerceCoinFullAccount() *coinrpc.CoinAccountInfo {
	return &coinrpc.CoinAccountInfo{
		Mid: 10001, Balance: 320, TotalTossed: 180, TodayTossed: 2,
		TodayLimit: 100, PerTargetLimit: 10, CancelWindowSeconds: 600,
		Version: 12, Ctime: 1700000000, Mtime: 1700000600,
	}
}

func commerceCoinFullFlow() *coinrpc.CoinFlowInfo {
	return &coinrpc.CoinFlowInfo{
		FlowId: 7007, Mid: 10001,
		FlowType: coinrpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT,
		Delta:    -50, BalanceAfter: 320, TargetAid: 0, // 投币类流水才带 aid
		BizNo: "WO-2026-0922-01", Operator: "gateway/admin:77",
		RequestId: "k-grant-1", Remark: "运营扣回违规发放", Ctime: 1700000800,
	}
}

func commerceCoinFullTossConfig() *coinrpc.GetTossConfigReply {
	return &coinrpc.GetTossConfigReply{
		DailyLimit: 100, PerTargetLimit: 10, CancelWindowSeconds: 600,
		MinBalanceToToss: 1, InitialBalance: 5,
	}
}

// --- 写路由的合法入参样例（每个用例取新副本，避免互相污染）---

func commerceOrderGoodApprove() *types.ParamOrderRefundApprove {
	return &types.ParamOrderRefundApprove{
		OrderNo:         "to_01HZZZZZZZZZZZZZZZZZZZZZ",
		ExpectedVersion: 7,
		Reason:          "客服核实为重复购买，全额退回余额",
		Operator:        9001,
		IdempotencyKey:  "k-approve-1",
		TraceId:         "t-approve-1",
	}
}

func commerceOrderGoodReject() *types.ParamOrderRefundReject {
	return &types.ParamOrderRefundReject{
		OrderNo:        "to_01HZZZZZZZZZZZZZZZZZZZZZ",
		Reason:         "已消耗完权益，按条款不予退款",
		Operator:       9001,
		IdempotencyKey: "k-reject-1",
		TraceId:        "t-reject-1",
	}
}

func commerceCoinGoodGrant() *types.ParamCoinGrant {
	return &types.ParamCoinGrant{
		Mid:            10001,
		Delta:          200,
		FlowType:       int32(coinrpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT),
		BizNo:          "WO-2026-0922-01",
		Reason:         "活动补偿（工单 WO-2026-0922-01）",
		Operator:       9001,
		IdempotencyKey: "k-grant-1",
		TraceId:        "t-grant-1",
	}
}

// --- 路由表 ---

type commerceRoute struct {
	name string
	run  func(ctx context.Context, s *svc.ServiceContext) error
}

var commerceOrderRoutes = []commerceRoute{
	{"list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewOrderListLogic(ctx, s).OrderList(&types.ParamOrderList{Page: 1, Size: 20})
		return err
	}},
	{"get", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewOrderGetLogic(ctx, s).OrderGet(&types.ParamOrderGet{
			OrderNo: "to_01HZZZZZZZZZZZZZZZZZZZZZ",
		})
		return err
	}},
	{"event/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewOrderEventListLogic(ctx, s).OrderEventList(&types.ParamOrderEventList{
			OrderNo: "to_01HZZZZZZZZZZZZZZZZZZZZZ", Page: 1, Size: 20,
		})
		return err
	}},
	{"stuck/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewOrderStuckListLogic(ctx, s).OrderStuckList(&types.ParamOrderStuckList{
			OlderThanSeconds: 1800,
		})
		return err
	}},
	{"refund/approve", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewOrderRefundApproveLogic(ctx, s).OrderRefundApprove(commerceOrderGoodApprove())
		return err
	}},
	{"refund/reject", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewOrderRefundRejectLogic(ctx, s).OrderRefundReject(commerceOrderGoodReject())
		return err
	}},
}

var commerceCoinRoutes = []commerceRoute{
	{"account/get", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewCoinAccountGetLogic(ctx, s).CoinAccountGet(&types.ParamCoinAccountGet{Mid: 10001})
		return err
	}},
	{"flow/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewCoinFlowListLogic(ctx, s).CoinFlowList(&types.ParamCoinFlowList{Page: 1, Size: 20})
		return err
	}},
	{"toss/config", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewCoinTossConfigLogic(ctx, s).CoinTossConfig(&types.ParamCoinTossConfig{})
		return err
	}},
	{"grant", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewCoinGrantLogic(ctx, s).CoinGrant(commerceCoinGoodGrant())
		return err
	}},
}

// commerceOrderCoinIsWriteRoute 只认真正挂了 AdminPermission 的三条写路由（admin.api 写面段）。
func commerceOrderCoinIsWriteRoute(name string) bool {
	switch name {
	case "refund/approve", "refund/reject", "grant":
		return true
	default:
		return false
	}
}

// runWithoutClient 跑一条「客户端位为 nil」的路由。TradeOrder/Coin 是接口字段，
// nil 接口上一被调用就会 panic，所以能正常返回错误就等价于「一次 RPC 都没发」——
// 这比在 fake 上数计数器更硬（计数器版本见 fail-closed 用例）。
func runWithoutClient(t *testing.T, route commerceRoute, ctx context.Context) error {
	t.Helper()
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("%s: 未配客户端却试图下传 RPC（panic: %v）", route.name, p)
			}
		}()
		err = route.run(ctx, &svc.ServiceContext{})
	}()
	return err
}

// --- 1. 不伪造成功 ---

func TestCommerceOrderAllRoutes_NilClientDoesNotFakeLedger(t *testing.T) {
	ctx := commerceOrderSession()
	for _, route := range commerceOrderRoutes {
		err := runWithoutClient(t, route, ctx)
		if !errors.Is(err, errOrderServiceNotConfigured) {
			t.Fatalf("%s: 未配置客户端必须回 not configured 而不是空列表/found=false，实际 %v", route.name, err)
		}
		if !strings.Contains(err.Error(), "trade-order") {
			t.Fatalf("%s: 错误消息要点名是哪个下游没接，实际 %s", route.name, err.Error())
		}
	}
}

func TestCommerceCoinAllRoutes_NilClientDoesNotFakeLedger(t *testing.T) {
	ctx := commerceCoinSession()
	for _, route := range commerceCoinRoutes {
		err := runWithoutClient(t, route, ctx)
		if !errors.Is(err, errCoinServiceNotConfigured) {
			t.Fatalf("%s: 未配置客户端必须回 not configured 而不是 0 余额/全 0 参数，实际 %v", route.name, err)
		}
		if !strings.Contains(err.Error(), "coin") {
			t.Fatalf("%s: 错误消息要点名是哪个下游没接，实际 %s", route.name, err.Error())
		}
	}
}

func TestCommerceOrderAllRoutes_DownstreamErrorPropagatesVerbatim(t *testing.T) {
	bad := &commerceOrderFake{err: errCommerceOrderDownstream}
	ctx := commerceOrderSession()
	for i, route := range commerceOrderRoutes {
		err := route.run(ctx, commerceOrderSvc(bad))
		if !errors.Is(err, errCommerceOrderDownstream) {
			t.Fatalf("%s: 下游错误必须原样上抛（不得折成成功或空数据），实际 %v", route.name, err)
		}
		if bad.calls != i+1 {
			t.Fatalf("%s: 每条路由都要真打一次下游（累计 %d 次，lastCall=%s）", route.name, bad.calls, bad.lastCall)
		}
	}
	if bad.calls != len(commerceOrderRoutes) {
		t.Fatalf("六条路由各一次，实际 %d", bad.calls)
	}
	wantCalls := []string{"ListOrders", "GetOrder", "ListOrderEvents", "ListStuckOrders", "ApproveRefund", "RejectRefund"}
	if bad.lastCall != wantCalls[len(wantCalls)-1] {
		t.Fatalf("最后一条路由该调 %s，实际 %s", wantCalls[len(wantCalls)-1], bad.lastCall)
	}
}

func TestCommerceCoinAllRoutes_DownstreamErrorPropagatesVerbatim(t *testing.T) {
	bad := &commerceCoinFake{err: errCommerceCoinDownstream}
	ctx := commerceCoinSession()
	for i, route := range commerceCoinRoutes {
		err := route.run(ctx, commerceCoinSvc(bad))
		if !errors.Is(err, errCommerceCoinDownstream) {
			t.Fatalf("%s: 下游错误必须原样上抛（不得折成 0 余额或空台账），实际 %v", route.name, err)
		}
		if bad.calls != i+1 {
			t.Fatalf("%s: 每条路由都要真打一次下游（累计 %d 次，lastCall=%s）", route.name, bad.calls, bad.lastCall)
		}
	}
	if bad.calls != len(commerceCoinRoutes) {
		t.Fatalf("四条路由各一次，实际 %d", bad.calls)
	}
}

// --- 2. 操作者身份 ---

// TestCommerceOrderWriteRoutes_OperatorAlwaysFromSession 覆盖两条退款裁决：
// 进 RPC 的 operator 永远是会话渲染值，request_id 永远是幂等键原值
// （ApproveRefundReq/RejectRefundReq 都没有 trace 字段，trace_id 只进日志）。
func TestCommerceOrderWriteRoutes_OperatorAlwaysFromSession(t *testing.T) {
	fake := &commerceOrderFake{
		approve: &tradeorderrpc.ApproveRefundReply{
			Duplicated: false, Order: commerceOrderFull(),
			RefundNo: "RF01HZZZZZZZZZZZZZZZZZZZZZ", RevokeDetail: "会员权益已回收（剩余时长作废）",
		},
		reject: &tradeorderrpc.RejectRefundReply{Duplicated: false, Order: commerceOrderFull()},
	}
	svcCtx := commerceOrderSvc(fake)
	ctx := commerceOrderSession()
	if _, err := NewOrderRefundApproveLogic(ctx, svcCtx).OrderRefundApprove(commerceOrderGoodApprove()); err != nil {
		t.Fatalf("refund/approve: %v", err)
	}
	if _, err := NewOrderRefundRejectLogic(ctx, svcCtx).OrderRefundReject(commerceOrderGoodReject()); err != nil {
		t.Fatalf("refund/reject: %v", err)
	}

	wantOperator := "gateway/admin:77" // 会话 admin_id=77；表单自称的 9001 必须作废
	cases := []struct {
		route     string
		operator  string
		requestID string
		want      string
	}{
		{"refund/approve", fake.approveReq.GetOperator(), fake.approveReq.GetRequestId(), "k-approve-1"},
		{"refund/reject", fake.rejectReq.GetOperator(), fake.rejectReq.GetRequestId(), "k-reject-1"},
	}
	for _, c := range cases {
		if c.operator != wantOperator {
			t.Fatalf("%s: operator 必须来自会话而不是请求体，实际 %q", c.route, c.operator)
		}
		if c.requestID != c.want {
			t.Fatalf("%s: idempotency_key 要原样映射成 request_id，实际 %q want %q", c.route, c.requestID, c.want)
		}
	}
}

func TestCommerceCoinGrant_OperatorAndIdempotencyKeyFromSession(t *testing.T) {
	fake := &commerceCoinFake{grant: &coinrpc.GrantCoinReply{
		Duplicated: false, Account: commerceCoinFullAccount(), FlowId: 7007,
	}}
	resp, err := NewCoinGrantLogic(commerceCoinSession(), commerceCoinSvc(fake)).
		CoinGrant(commerceCoinGoodGrant())
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	got := fake.grantReq
	if got.GetOperator() != "gateway/admin:77" {
		t.Fatalf("operator 必须来自会话而不是请求体，实际 %q", got.GetOperator())
	}
	if got.GetRequestId() != "k-grant-1" {
		t.Fatalf("idempotency_key 要原样映射成 request_id，实际 %q", got.GetRequestId())
	}
	if resp.Data.Duplicated || resp.Data.FlowId != 7007 || resp.Data.Account.Balance != 320 {
		t.Fatalf("发放结论要转达服务回读值: %+v", resp.Data)
	}
}

// TestCommerceCoinGrant_FlowTypePassesThroughUntouched 锁「不替调用方补默认档位」：
// 两个合法档位各自原样下传，网关不改成 ADMIN_GRANT、也不因为 delta 是负数就拒掉方向。
func TestCommerceCoinGrant_FlowTypePassesThroughUntouched(t *testing.T) {
	for _, flowType := range []int32{
		int32(coinrpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK),
		int32(coinrpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT),
	} {
		fake := &commerceCoinFake{grant: &coinrpc.GrantCoinReply{FlowId: 7008}}
		req := commerceCoinGoodGrant()
		req.FlowType = flowType
		req.Delta = -200 // 扣回是同一口的合法方向；上限与「会不会扣成负余额」归服务判
		if _, err := NewCoinGrantLogic(commerceCoinSession(), commerceCoinSvc(fake)).CoinGrant(req); err != nil {
			t.Fatalf("flow_type=%d 负 delta: %v", flowType, err)
		}
		if fake.grantReq.GetFlowType() != coinrpc.CoinFlowType(flowType) || fake.grantReq.GetDelta() != -200 {
			t.Fatalf("档位或方向被网关改写（不得补默认值、不得取绝对值）: %+v", fake.grantReq)
		}
	}
}

// TestCommerceOrderWriteRoutes_FailClosedWithoutSession / Coin 版：
// 拿不到会话身份 = 这条路由没被 AdminPermission 保护（权限表/挂载漂移），
// 必须在下传前拒绝，一次 RPC 都不能发。AdminID<=0 与「完全没有身份」都要挡住。
func TestCommerceWriteRoutes_FailClosedWithoutSession(t *testing.T) {
	sessions := []struct {
		name string
		ctx  context.Context
	}{
		{"无身份", context.Background()},
		{"身份非法（admin_id=0）", middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: 0})},
	}
	all := []struct {
		domain   string
		routes   []commerceRoute
		sentinel error
	}{
		{"order", commerceOrderRoutes, errOrderSessionRequired},
		{"coin", commerceCoinRoutes, errCoinSessionRequired},
	}
	for _, group := range all {
		for _, route := range group.routes {
			if !commerceOrderCoinIsWriteRoute(route.name) {
				continue // 只跑三条写路由
			}
			for _, session := range sessions {
				orderFake := &commerceOrderFake{}
				coinFake := &commerceCoinFake{}
				var s *svc.ServiceContext
				if group.domain == "order" {
					s = commerceOrderSvc(orderFake)
				} else {
					s = commerceCoinSvc(coinFake)
				}
				err := route.run(session.ctx, s)
				if !errors.Is(err, group.sentinel) {
					t.Fatalf("%s/%s: 缺会话身份必须 fail-closed，实际 %v", route.name, session.name, err)
				}
				if orderFake.calls+coinFake.calls != 0 {
					t.Fatalf("%s/%s: fail-closed 却已经打了下游（order=%d coin=%d lastCall=%s）",
						route.name, session.name, orderFake.calls, coinFake.calls,
						fmt.Sprintf("%s/%s", orderFake.lastCall, coinFake.lastCall))
				}
			}
		}
	}
}

// --- 3. 必填与不可能形状：逐个点名，且被拒的入参一次都不打到下游 ---

func TestCommerceOrderWriteRoutes_RequiredFieldsGateBeforeDownstream(t *testing.T) {
	ctx := commerceOrderSession()
	cases := []struct {
		name string
		run  func(svcCtx *svc.ServiceContext) error
		want string
	}{
		{"审批缺单号", func(s *svc.ServiceContext) error {
			r := commerceOrderGoodApprove()
			r.OrderNo = "  "
			_, err := NewOrderRefundApproveLogic(ctx, s).OrderRefundApprove(r)
			return err
		}, "order_no"},
		{"审批无理由", func(s *svc.ServiceContext) error {
			r := commerceOrderGoodApprove()
			r.Reason = " "
			_, err := NewOrderRefundApproveLogic(ctx, s).OrderRefundApprove(r)
			return err
		}, "reason"},
		{"审批缺幂等键", func(s *svc.ServiceContext) error {
			r := commerceOrderGoodApprove()
			r.IdempotencyKey = ""
			_, err := NewOrderRefundApproveLogic(ctx, s).OrderRefundApprove(r)
			return err
		}, "idempotency_key"},
		{"审批 operator 位非法（表单没填审计主体）", func(s *svc.ServiceContext) error {
			r := commerceOrderGoodApprove()
			r.Operator = 0
			_, err := NewOrderRefundApproveLogic(ctx, s).OrderRefundApprove(r)
			return err
		}, "operator"},
		{"审批 CAS 版本为负（不可能出现在订单行上）", func(s *svc.ServiceContext) error {
			r := commerceOrderGoodApprove()
			r.ExpectedVersion = -1
			_, err := NewOrderRefundApproveLogic(ctx, s).OrderRefundApprove(r)
			return err
		}, "expected_version"},
		{"驳回缺单号", func(s *svc.ServiceContext) error {
			r := commerceOrderGoodReject()
			r.OrderNo = ""
			_, err := NewOrderRefundRejectLogic(ctx, s).OrderRefundReject(r)
			return err
		}, "order_no"},
		{"驳回无理由（用户可感知的结论，必须留证）", func(s *svc.ServiceContext) error {
			r := commerceOrderGoodReject()
			r.Reason = ""
			_, err := NewOrderRefundRejectLogic(ctx, s).OrderRefundReject(r)
			return err
		}, "reason"},
		{"驳回缺幂等键", func(s *svc.ServiceContext) error {
			r := commerceOrderGoodReject()
			r.IdempotencyKey = "   "
			_, err := NewOrderRefundRejectLogic(ctx, s).OrderRefundReject(r)
			return err
		}, "idempotency_key"},
	}
	for _, c := range cases {
		fake := &commerceOrderFake{}
		err := c.run(commerceOrderSvc(fake))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误消息要点名字段 %s，实际 %v", c.name, c.want, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 已被拒的入参不该打到下游（lastCall=%s）", c.name, fake.lastCall)
		}
	}
}

func TestCommerceCoinGrant_RequiredFieldsGateBeforeDownstream(t *testing.T) {
	ctx := commerceCoinSession()
	cases := []struct {
		name string
		run  func(svcCtx *svc.ServiceContext) error
		want string
	}{
		{"发放缺幂等键", func(s *svc.ServiceContext) error {
			r := commerceCoinGoodGrant()
			r.IdempotencyKey = ""
			_, err := NewCoinGrantLogic(ctx, s).CoinGrant(r)
			return err
		}, "idempotency_key"},
		{"发放 mid=0（硬币账户没有游客号）", func(s *svc.ServiceContext) error {
			r := commerceCoinGoodGrant()
			r.Mid = 0
			_, err := NewCoinGrantLogic(ctx, s).CoinGrant(r)
			return err
		}, "mid"},
		{"发放 mid 为负", func(s *svc.ServiceContext) error {
			r := commerceCoinGoodGrant()
			r.Mid = -1
			_, err := NewCoinGrantLogic(ctx, s).CoinGrant(r)
			return err
		}, "mid"},
		{"发放 delta=0（无记账意义）", func(s *svc.ServiceContext) error {
			r := commerceCoinGoodGrant()
			r.Delta = 0
			_, err := NewCoinGrantLogic(ctx, s).CoinGrant(r)
			return err
		}, "delta"},
	}
	for _, c := range cases {
		fake := &commerceCoinFake{}
		err := c.run(commerceCoinSvc(fake))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误消息要点名字段 %s，实际 %v", c.name, c.want, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 已被拒的入参不该打到下游（lastCall=%s）", c.name, fake.lastCall)
		}
	}
}

// TestCommerceCoinGrant_FlowTypeWhitelistRejectsNonGrantTypes 锁 §1/§7：
// TOSS/CANCEL_TOSS 是终端投币链路自己写的流水，EXPIRE 本项目未开启，UNSPECIFIED 与
// 未知编号没有语义 —— 全部在下传前拒掉，且**不改写、不降级成 ADMIN_GRANT**。
func TestCommerceCoinGrant_FlowTypeWhitelistRejectsNonGrantTypes(t *testing.T) {
	ctx := commerceCoinSession()
	illegal := []int32{
		0,
		int32(coinrpc.CoinFlowType_COIN_FLOW_TYPE_TOSS),
		int32(coinrpc.CoinFlowType_COIN_FLOW_TYPE_CANCEL_TOSS),
		int32(coinrpc.CoinFlowType_COIN_FLOW_TYPE_EXPIRE),
		6, 999, -1,
	}
	for _, flowType := range illegal {
		fake := &commerceCoinFake{}
		r := commerceCoinGoodGrant()
		r.FlowType = flowType
		_, err := NewCoinGrantLogic(ctx, commerceCoinSvc(fake)).CoinGrant(r)
		if err == nil || !strings.Contains(err.Error(), "flow_type") {
			t.Fatalf("flow_type=%d 必须被拒并点名，实际 %v", flowType, err)
		}
		if fake.calls != 0 {
			t.Fatalf("flow_type=%d: 非法档位被发到下游了（lastCall=%s）", flowType, fake.lastCall)
		}
	}
}

// TestCommerceCoinGrant_ConditionalRequiredFieldsStayOnService 是刻意的「不挡」用例：
// 「ADMIN_GRANT 必须带 reason」「ORDER_PACK 必须带 biz_no」是按 flow_type 分叉的条件必填，
// admin.api 把 reason 标成 optional 并写明「由服务判定」，因此网关不做第二处规则源：
// 请求照常下传，服务的哨兵拒绝逐字透出（既不预拒、也不折叠成成功）。
func TestCommerceCoinGrant_ConditionalRequiredFieldsStayOnService(t *testing.T) {
	ctx := commerceCoinSession()

	noReason := &commerceCoinFake{err: errCommerceCoinGrantReasonRequired}
	r1 := commerceCoinGoodGrant()
	r1.Reason = ""
	if _, err := NewCoinGrantLogic(ctx, commerceCoinSvc(noReason)).CoinGrant(r1); !errors.Is(err, errCommerceCoinGrantReasonRequired) {
		t.Fatalf("ADMIN_GRANT 缺 reason 要由服务拒并原样透出: %v", err)
	}
	if noReason.calls != 1 || noReason.grantReq.GetReason() != "" {
		t.Fatalf("网关不得替调用方补 reason: %+v", noReason.grantReq)
	}

	noBizNo := &commerceCoinFake{err: errCommerceCoinGrantBizNoRequired}
	r2 := commerceCoinGoodGrant()
	r2.FlowType = int32(coinrpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK)
	r2.BizNo = ""
	if _, err := NewCoinGrantLogic(ctx, commerceCoinSvc(noBizNo)).CoinGrant(r2); !errors.Is(err, errCommerceCoinGrantBizNoRequired) {
		t.Fatalf("ORDER_PACK 缺 biz_no 要由服务拒并原样透出: %v", err)
	}
	if noBizNo.grantReq.GetBizNo() != "" {
		t.Fatalf("网关不得替调用方造 biz_no: %+v", noBizNo.grantReq)
	}
}

// TestCommerceOrderApprove_NoAmountSlotAndCasZeroStaysOnService 锁退款语义：
//   - ApproveRefundReq 与 ParamOrderRefundApprove 都没有金额位 → 网关不提供部分退款，
//     也不按比例换算（全额退是服务与 payment 之间的既有约束）；
//   - expected_version=0 原样下传（.api 标 optional），由服务按「审批类写接口必须带 CAS
//     版本」拒掉；网关不自己去查当前版本补号（那会把乐观锁换成「保证不冲突」）。
//   - 「这单现在能不能批」是服务的 ErrRefundNotRequested，逐字透出，不退化成成功。
func TestCommerceOrderApprove_NoAmountSlotAndCasZeroStaysOnService(t *testing.T) {
	fake := &commerceOrderFake{approve: &tradeorderrpc.ApproveRefundReply{
		Duplicated: false, Order: commerceOrderFull(),
		RefundNo: "RF01HZZZZZZZZZZZZZZZZZZZZZ", RevokeDetail: "款已退、权益未回收",
	}}
	svcCtx := commerceOrderSvc(fake)

	// 契约上的五位在请求体里逐一点名（protobuf 文本格式会省略 0 值标量，
	// 所以用非 0 样本来断言「网关只加了这五位，没有多出金额位」）。
	if _, err := NewOrderRefundApproveLogic(commerceOrderSession(), svcCtx).
		OrderRefundApprove(commerceOrderGoodApprove()); err != nil {
		t.Fatalf("正常审批: %v", err)
	}
	reqText := fake.approveReq.String()
	for _, slot := range []string{"order_no", "operator", "request_id", "reason", "expected_version"} {
		if !strings.Contains(reqText, slot) {
			t.Fatalf("ApproveRefundReq 少了 %s 位，网关没把必填项发下去: %s", slot, reqText)
		}
	}
	for _, forbidden := range []string{"amount", "refund_amount", "minor", "ratio", "trace_id"} {
		if strings.Contains(reqText, forbidden) {
			t.Fatalf("网关不得塞进契约没有的 %s 位（部分退款/自建比例/伪造溯源都不在契约里）: %s", forbidden, reqText)
		}
	}
	if fake.approveReq.GetExpectedVersion() != 7 {
		t.Fatalf("非 0 的 CAS 版本要原样下传: %+v", fake.approveReq)
	}
	if resp, err := NewOrderRefundApproveLogic(commerceOrderSession(), svcCtx).
		OrderRefundApprove(commerceOrderGoodApprove()); err != nil ||
		resp.Data.RefundNo != "RF01HZZZZZZZZZZZZZZZZZZZZZ" ||
		resp.Data.RevokeDetail != "款已退、权益未回收" {
		t.Fatalf("退款单号与回收结论要原样转达: resp=%+v err=%v", resp.Data, err)
	}

	// expected_version=0：网关不代填、不去查当前版本，交给服务的
	// ErrExpectedVersionRequired 拒（见 CasConflictPropagatesWithoutRetry 用例）。
	zeroFake := &commerceOrderFake{approve: &tradeorderrpc.ApproveRefundReply{Order: commerceOrderFull()}}
	zeroReq := commerceOrderGoodApprove()
	zeroReq.ExpectedVersion = 0
	if _, err := NewOrderRefundApproveLogic(commerceOrderSession(), commerceOrderSvc(zeroFake)).
		OrderRefundApprove(zeroReq); err != nil {
		t.Fatalf("expected_version=0 该交给服务判: %v", err)
	}
	if zeroFake.approveReq.GetExpectedVersion() != 0 {
		t.Fatalf("网关不得代填 CAS 版本: %+v", zeroFake.approveReq)
	}
}

func TestCommerceOrderApprove_InvalidPreStatePropagates(t *testing.T) {
	fake := &commerceOrderFake{err: errCommerceOrderRefundNotRequested}
	_, err := NewOrderRefundApproveLogic(commerceOrderSession(), commerceOrderSvc(fake)).
		OrderRefundApprove(commerceOrderGoodApprove())
	if !errors.Is(err, errCommerceOrderRefundNotRequested) {
		t.Fatalf("非法前态必须由服务拒绝并原样透出: %v", err)
	}
	if fake.calls != 1 || fake.lastCall != "ApproveRefund" {
		t.Fatalf("要真打一次才知道前态不合法（calls=%d lastCall=%s）", fake.calls, fake.lastCall)
	}
}

func TestCommerceOrderApprove_CasConflictPropagatesWithoutRetry(t *testing.T) {
	// 服务的 ErrExpectedVersionRequired 对应「0 位」；ErrConcurrentUpdate 对应版本过期。
	// 两条都只准打一次：网关换个号或补版本重放，就等于把一次冲突变成两次退款尝试。
	conflict := errors.New("trade-order: concurrent state update")
	fake := &commerceOrderFake{err: conflict}
	_, err := NewOrderRefundApproveLogic(commerceOrderSession(), commerceOrderSvc(fake)).
		OrderRefundApprove(commerceOrderGoodApprove())
	if !errors.Is(err, conflict) {
		t.Fatalf("CAS 冲突要原样透出: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("写路由不得自动重放（calls=%d）", fake.calls)
	}

	zero := &commerceOrderFake{err: errCommerceOrderExpectedVersionRequired}
	if _, err := NewOrderRefundApproveLogic(commerceOrderSession(), commerceOrderSvc(zero)).
		OrderRefundApprove(&types.ParamOrderRefundApprove{
			OrderNo: "to_x", Reason: "r", Operator: 9001, IdempotencyKey: "k-1",
		}); !errors.Is(err, errCommerceOrderExpectedVersionRequired) {
		t.Fatalf("expected_version=0 的拒绝要逐字透出: %v", err)
	}
}

// --- 4. 分页 / 窗口 / 0 哨兵 ---

func TestCommerceOrderList_PagingPassesThroughAndEchoesServiceValues(t *testing.T) {
	fake := &commerceOrderFake{list: &tradeorderrpc.ListOrdersReply{
		Orders: []*tradeorderrpc.OrderInfo{commerceOrderFull()},
		Total:  41, Page: 3, Size: 100, // 服务归一后的回显；网关照抄
	}}
	resp, err := NewOrderListLogic(context.Background(), commerceOrderSvc(fake)).
		OrderList(&types.ParamOrderList{
			Mid: 10001, State: 8, BizType: 1, PayMethod: 1,
			OrderNo: "to_01H", PaymentNo: "PM01H",
			FromTs: 1700000000, ToTs: 1700000900, Page: 3, Size: 500, MaxWindowSeconds: 3600,
		})
	if err != nil {
		t.Fatalf("翻页不该被网关拒绝: %v", err)
	}
	got := fake.listReq
	if got.GetPage() != 3 || got.GetSize() != 500 {
		t.Fatalf("page/size 被网关改写或裁剪: %+v", got)
	}
	if got.GetState() != tradeorderrpc.OrderState_ORDER_STATE_REFUND_REQUESTED ||
		got.GetBizType() != tradeorderrpc.OrderBizType_ORDER_BIZ_TYPE_MEMBERSHIP ||
		got.GetPayMethod() != tradeorderrpc.PayMethod_PAY_METHOD_BALANCE ||
		got.GetMaxWindowSeconds() != 3600 || got.GetFromTs() != 1700000000 || got.GetToTs() != 1700000900 {
		t.Fatalf("过滤条件没按契约下传: %+v", got)
	}
	if resp.Data.Total != 41 || resp.Data.Page != 3 || resp.Data.Size != 100 {
		t.Fatalf("分页三元组必须照抄服务回显: %+v", resp.Data)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封必须固定 code=0/message=ok/ttl=0: %+v", resp)
	}
}

// TestCommerceOrderList_ZeroMeansNoFilterOrCrossUser 锁 0 哨兵：mid/state/biz_type/
// pay_method/page/size/max_window_seconds 的 0 各有含义（不过滤 / 默认页 / 默认窗口），
// 网关一个都不代填；空结果投影成 []，但服务的有界性拒绝不得被折成空列表。
func TestCommerceOrderList_ZeroMeansNoFilterOrCrossUser(t *testing.T) {
	fake := &commerceOrderFake{list: &tradeorderrpc.ListOrdersReply{}}
	resp, err := NewOrderListLogic(context.Background(), commerceOrderSvc(fake)).OrderList(&types.ParamOrderList{})
	if err != nil {
		t.Fatalf("全 0 的检索请求本身合法（有界性由服务判）: %v", err)
	}
	got := fake.listReq
	if got.GetMid() != 0 || got.GetPage() != 0 || got.GetSize() != 0 || got.GetMaxWindowSeconds() != 0 {
		t.Fatalf("0 是合法哨兵，网关不得代填: %+v", got)
	}
	if got.GetState() != tradeorderrpc.OrderState_ORDER_STATE_UNSPECIFIED ||
		got.GetBizType() != tradeorderrpc.OrderBizType_ORDER_BIZ_TYPE_UNSPECIFIED ||
		got.GetPayMethod() != tradeorderrpc.PayMethod_PAY_METHOD_UNSPECIFIED {
		t.Fatalf("过滤位被网关代填: %+v", got)
	}
	if resp.Data.List == nil {
		t.Fatalf("空列表要投影成 []，客户端才能直接遍历: %#v", resp.Data.List)
	}
	if len(resp.Data.List) != 0 || resp.Data.Total != 0 {
		t.Fatalf("服务回空就是空，网关不补条目: %+v", resp.Data)
	}

	bounded := &commerceOrderFake{err: errCommerceOrderFilterRequired}
	if _, err := NewOrderListLogic(context.Background(), commerceOrderSvc(bounded)).
		OrderList(&types.ParamOrderList{Page: 1, Size: 20}); !errors.Is(err, errCommerceOrderFilterRequired) {
		t.Fatalf("服务的「至少一个过滤条件」拒绝必须原样透出而不是回空列表: %v", err)
	}
}

func TestCommerceOrderAllRoutes_RejectImpossibleShapes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		run  func(svcCtx *svc.ServiceContext) error
		want string
	}{
		{"检索 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderListLogic(ctx, s).OrderList(&types.ParamOrderList{Mid: -1})
			return err
		}, "mid"},
		{"检索 state 为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderListLogic(ctx, s).OrderList(&types.ParamOrderList{State: -1})
			return err
		}, "state"},
		{"检索 biz_type 为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderListLogic(ctx, s).OrderList(&types.ParamOrderList{BizType: -1})
			return err
		}, "biz_type"},
		{"检索 pay_method 为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderListLogic(ctx, s).OrderList(&types.ParamOrderList{PayMethod: -1})
			return err
		}, "pay_method"},
		{"检索窗口为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderListLogic(ctx, s).OrderList(&types.ParamOrderList{ToTs: -1})
			return err
		}, "to_ts"},
		{"检索窗口倒置", func(s *svc.ServiceContext) error {
			_, err := NewOrderListLogic(ctx, s).OrderList(&types.ParamOrderList{FromTs: 200, ToTs: 100})
			return err
		}, "from_ts"},
		{"检索窗口上限为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderListLogic(ctx, s).OrderList(&types.ParamOrderList{MaxWindowSeconds: -1})
			return err
		}, "max_window_seconds"},
		{"检索页码为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderListLogic(ctx, s).OrderList(&types.ParamOrderList{Page: -1})
			return err
		}, "page"},
		{"详情缺单号", func(s *svc.ServiceContext) error {
			_, err := NewOrderGetLogic(ctx, s).OrderGet(&types.ParamOrderGet{OrderNo: "  "})
			return err
		}, "order_no"},
		{"详情 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderGetLogic(ctx, s).OrderGet(&types.ParamOrderGet{OrderNo: "to_1", Mid: -1})
			return err
		}, "mid"},
		{"台账缺单号", func(s *svc.ServiceContext) error {
			_, err := NewOrderEventListLogic(ctx, s).OrderEventList(&types.ParamOrderEventList{})
			return err
		}, "order_no"},
		{"台账页大小为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderEventListLogic(ctx, s).OrderEventList(&types.ParamOrderEventList{
				OrderNo: "to_1", Size: -1,
			})
			return err
		}, "size"},
		{"卡单扫描 older_than 为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderStuckListLogic(ctx, s).OrderStuckList(&types.ParamOrderStuckList{
				OlderThanSeconds: -1,
			})
			return err
		}, "older_than_seconds"},
		{"卡单扫描 limit 为负", func(s *svc.ServiceContext) error {
			_, err := NewOrderStuckListLogic(ctx, s).OrderStuckList(&types.ParamOrderStuckList{
				OlderThanSeconds: 1800, Limit: -1,
			})
			return err
		}, "limit"},
		{"卡单扫描 states 含负编号", func(s *svc.ServiceContext) error {
			_, err := NewOrderStuckListLogic(ctx, s).OrderStuckList(&types.ParamOrderStuckList{
				OlderThanSeconds: 1800, States: []int32{2, -3},
			})
			return err
		}, "states[1]"},
	}
	for _, c := range cases {
		fake := &commerceOrderFake{}
		err := c.run(commerceOrderSvc(fake))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 要在下传前拒掉并点名字段 %s，实际 %v", c.name, c.want, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 已被拒的入参不该打到下游（lastCall=%s）", c.name, fake.lastCall)
		}
	}
}

func TestCommerceCoinAllRoutes_RejectImpossibleShapes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		run  func(svcCtx *svc.ServiceContext) error
		want string
	}{
		{"账户查询 mid=0", func(s *svc.ServiceContext) error {
			_, err := NewCoinAccountGetLogic(ctx, s).CoinAccountGet(&types.ParamCoinAccountGet{Mid: 0})
			return err
		}, "mid"},
		{"账户查询 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewCoinAccountGetLogic(ctx, s).CoinAccountGet(&types.ParamCoinAccountGet{Mid: -5})
			return err
		}, "mid"},
		{"流水台账 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewCoinFlowListLogic(ctx, s).CoinFlowList(&types.ParamCoinFlowList{Mid: -1})
			return err
		}, "mid"},
		{"流水台账 flow_type 为负", func(s *svc.ServiceContext) error {
			_, err := NewCoinFlowListLogic(ctx, s).CoinFlowList(&types.ParamCoinFlowList{FlowType: -1})
			return err
		}, "flow_type"},
		{"流水台账窗口为负", func(s *svc.ServiceContext) error {
			_, err := NewCoinFlowListLogic(ctx, s).CoinFlowList(&types.ParamCoinFlowList{FromTs: -1})
			return err
		}, "from_ts"},
		{"流水台账窗口倒置", func(s *svc.ServiceContext) error {
			_, err := NewCoinFlowListLogic(ctx, s).CoinFlowList(&types.ParamCoinFlowList{FromTs: 200, ToTs: 100})
			return err
		}, "from_ts"},
		{"流水台账页码为负", func(s *svc.ServiceContext) error {
			_, err := NewCoinFlowListLogic(ctx, s).CoinFlowList(&types.ParamCoinFlowList{Page: -1})
			return err
		}, "page"},
	}
	for _, c := range cases {
		fake := &commerceCoinFake{}
		err := c.run(commerceCoinSvc(fake))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 要在下传前拒掉并点名字段 %s，实际 %v", c.name, c.want, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 已被拒的入参不该打到下游（lastCall=%s）", c.name, fake.lastCall)
		}
	}
}

// --- 5. 卡单扫描的真实形态：契约里没有分页 ---

// TestCommerceOrderStuckList_ContractHasNoPaging 如实投影 ListStuckOrders 的形状：
// req 只有 older_than_seconds/states/limit，reply 只有 orders，**没有** total/page/size。
// 网关不伪造分页三元组、不在本地截断、也不因为「看起来像分页接口」就补默认页码；
// 一次扫描最多 StuckScanMaxLimit 条、超出部分怎么续扫目前契约里无解（已上报为缺口）。
func TestCommerceOrderStuckList_ContractHasNoPaging(t *testing.T) {
	fake := &commerceOrderFake{stuck: &tradeorderrpc.ListStuckOrdersReply{
		Orders: []*tradeorderrpc.OrderInfo{commerceOrderFull(), commerceOrderFull()},
	}}
	resp, err := NewOrderStuckListLogic(context.Background(), commerceOrderSvc(fake)).
		OrderStuckList(&types.ParamOrderStuckList{
			OlderThanSeconds: 1800,
			States:           []int32{2, 3, 4},
			Limit:            200,
		})
	if err != nil {
		t.Fatalf("stuck/list: %v", err)
	}
	got := fake.stuckReq
	if got.GetOlderThanSeconds() != 1800 || got.GetLimit() != 200 {
		t.Fatalf("扫描阈值与 limit 要原样下传（上限归服务拒）: %+v", got)
	}
	if len(got.GetStates()) != 3 || got.GetStates()[2] != tradeorderrpc.OrderState_ORDER_STATE_FULFILLING {
		t.Fatalf("states 要逐个 cast 且不去重不排序不裁剪: %v", got.GetStates())
	}
	// 响应结构里没有 page/size/total 可写：这里断言能拿到的只有 list 一位（编译期即事实），
	// 以及条数原样保持。
	if len(resp.Data.List) != 2 || resp.Data.List[0].OrderNo != "to_01HZZZZZZZZZZZZZZZZZZZZZ" {
		t.Fatalf("卡单列表要逐条转达，不截断: %+v", resp.Data.List)
	}

	// older_than_seconds=0 / limit=0 都是合法哨兵（服务用配置默认），网关不代填。
	zeroFake := &commerceOrderFake{stuck: &tradeorderrpc.ListStuckOrdersReply{}}
	zeroResp, err := NewOrderStuckListLogic(context.Background(), commerceOrderSvc(zeroFake)).
		OrderStuckList(&types.ParamOrderStuckList{})
	if err != nil {
		t.Fatalf("全 0 的扫描请求是合法请求（默认阈值归服务）: %v", err)
	}
	if zeroFake.stuckReq.GetOlderThanSeconds() != 0 || zeroFake.stuckReq.GetLimit() != 0 {
		t.Fatalf("0 位被网关代填: %+v", zeroFake.stuckReq)
	}
	if len(zeroFake.stuckReq.GetStates()) != 0 {
		t.Fatalf("states 为空时网关不得替它补默认集合: %v", zeroFake.stuckReq.GetStates())
	}
	if zeroResp.Data.List == nil || len(zeroResp.Data.List) != 0 {
		t.Fatalf("没有卡单要投影成 []（这是真实结论，不是错误）: %#v", zeroResp.Data.List)
	}

	// 服务拒绝「扫已结案件」时逐字透出，不退化成默认集合再扫一次。
	badStates := &commerceOrderFake{err: errCommerceOrderStuckStateNotAllowed}
	if _, err := NewOrderStuckListLogic(context.Background(), commerceOrderSvc(badStates)).
		OrderStuckList(&types.ParamOrderStuckList{OlderThanSeconds: 1, States: []int32{6}}); !errors.Is(err, errCommerceOrderStuckStateNotAllowed) {
		t.Fatalf("服务的 stuck 状态拒绝必须原样透出: %v", err)
	}
	if badStates.calls != 1 {
		t.Fatalf("被拒的扫描不得重试（calls=%d）", badStates.calls)
	}
}

// --- 6. found / 空结果的真实语义 ---

// TestCommerceOrderGet_NotFoundIsNotErrorAndNotFabricatedOrder：越权与不存在都被服务合并成
// found=false（防订单号枚举探测），网关逐字转达，既不报 500 也不填一个看起来真的订单。
func TestCommerceOrderGet_NotFoundIsNotError(t *testing.T) {
	fake := &commerceOrderFake{get: &tradeorderrpc.GetOrderReply{Found: false}}
	resp, err := NewOrderGetLogic(context.Background(), commerceOrderSvc(fake)).
		OrderGet(&types.ParamOrderGet{OrderNo: "to_missing", Mid: 10001})
	if err != nil {
		t.Fatalf("found=false 是真实读结论，不是错误: %v", err)
	}
	if resp.Data.Found {
		t.Fatalf("found 位被网关翻成 true 了: %+v", resp.Data)
	}
	if resp.Data.Order != (types.OrderItem{}) {
		t.Fatalf("没有订单时不得渲染出任何字段（连 mid 都不能猜）: %+v", resp.Data.Order)
	}
	if fake.getReq.GetMid() != 10001 {
		t.Fatalf("mid 要原样下传（归属校验在服务）: %+v", fake.getReq)
	}

	hit := &commerceOrderFake{get: &tradeorderrpc.GetOrderReply{Found: true, Order: commerceOrderFull()}}
	got, err := NewOrderGetLogic(context.Background(), commerceOrderSvc(hit)).
		OrderGet(&types.ParamOrderGet{OrderNo: "to_01HZZZZZZZZZZZZZZZZZZZZZ"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Data.Found || got.Data.Order.Version != 7 || got.Data.Order.State != 8 {
		t.Fatalf("详情要转达当前状态与 CAS 位点（后台下一轮审批要带回去）: %+v", got.Data.Order)
	}
	if hit.getReq.GetMid() != 0 {
		t.Fatalf("mid=0 表示不做归属校验，网关不得代填: %+v", hit.getReq)
	}
}

// TestCommerceCoinAccountGet_FoundFalseKeepsLimits：没账户时服务仍回三项生效限额，
// 网关不整行清零（那会把「上限是多少枚」这个仍然成立的事实抹掉），也不把读失败折成 0 余额。
func TestCommerceCoinAccountGet_FoundFalseKeepsLimits(t *testing.T) {
	fake := &commerceCoinFake{account: &coinrpc.GetCoinAccountReply{
		Found: false,
		Account: &coinrpc.CoinAccountInfo{
			Mid: 10001, TodayLimit: 100, PerTargetLimit: 10, CancelWindowSeconds: 600,
		},
	}}
	resp, err := NewCoinAccountGetLogic(context.Background(), commerceCoinSvc(fake)).
		CoinAccountGet(&types.ParamCoinAccountGet{Mid: 10001})
	if err != nil {
		t.Fatalf("没建过户不是错误: %v", err)
	}
	if resp.Data.Found {
		t.Fatalf("found=false 被翻成 true 了: %+v", resp.Data)
	}
	if resp.Data.Account.Balance != 0 || resp.Data.Account.TodayTossed != 0 || resp.Data.Account.Version != 0 {
		t.Fatalf("未建仓时余额位应为服务给的 0: %+v", resp.Data.Account)
	}
	if resp.Data.Account.TodayLimit != 100 || resp.Data.Account.PerTargetLimit != 10 ||
		resp.Data.Account.CancelWindowSeconds != 600 {
		t.Fatalf("限额是生效配置投影，不得因为没账户就清零: %+v", resp.Data.Account)
	}

	rerr := &commerceCoinFake{err: errCommerceCoinDownstream}
	if _, err := NewCoinAccountGetLogic(context.Background(), commerceCoinSvc(rerr)).
		CoinAccountGet(&types.ParamCoinAccountGet{Mid: 10001}); !errors.Is(err, errCommerceCoinDownstream) {
		t.Fatalf("读失败必须上抛，不能显示成「余额 0」: %v", err)
	}
}

// TestCommerceCoinFlowList_UnboundedQueryPropagates：跨用户查台账没给窗口时，服务回
// ErrUnboundedLedgerQuery；网关把它原样透出，绝不渲染成「这个人没有过硬币变动」。
func TestCommerceCoinFlowList_UnboundedQueryPropagates(t *testing.T) {
	fake := &commerceCoinFake{err: errCommerceCoinUnboundedQuery}
	_, err := NewCoinFlowListLogic(context.Background(), commerceCoinSvc(fake)).
		CoinFlowList(&types.ParamCoinFlowList{Page: 1, Size: 20})
	if !errors.Is(err, errCommerceCoinUnboundedQuery) {
		t.Fatalf("服务的有界性拒绝必须原样透出: %v", err)
	}
	if fake.flowsReq.GetFromTs() != 0 || fake.flowsReq.GetToTs() != 0 || fake.flowsReq.GetBizNo() != "" {
		t.Fatalf("网关不得凭空造时间窗或订单号: %+v", fake.flowsReq)
	}

	// 空台账（total=0）不是错误，但也不能被渲染成「查到了数据」。
	zero := &commerceCoinFake{flows: &coinrpc.ListCoinFlowsReply{}}
	resp, err := NewCoinFlowListLogic(context.Background(), commerceCoinSvc(zero)).
		CoinFlowList(&types.ParamCoinFlowList{Mid: 10001, FlowType: 0, Page: 0, Size: 0})
	if err != nil {
		t.Fatalf("空台账是合法结论: %v", err)
	}
	if resp.Data.List == nil || len(resp.Data.List) != 0 || resp.Data.Total != 0 {
		t.Fatalf("空列表要投影成 [] 且 total 照抄: %#v", resp.Data)
	}
	if zero.flowsReq.GetMid() != 10001 || zero.flowsReq.GetFlowType() != coinrpc.CoinFlowType_COIN_FLOW_TYPE_UNSPECIFIED {
		t.Fatalf("mid/flow_type 的 0 位与请求值都要原样下传: %+v", zero.flowsReq)
	}
}

// TestCommerceCoinTossConfig_ReportsServiceValuesVerbatim 锁 §6：五位参数逐字转达，
// 包括「服务全回 0」（配置被清空）这种难看但真实的结论，网关都不美化。
func TestCommerceCoinTossConfig_ReportsServiceValuesVerbatim(t *testing.T) {
	fake := &commerceCoinFake{toss: commerceCoinFullTossConfig()}
	resp, err := NewCoinTossConfigLogic(context.Background(), commerceCoinSvc(fake)).
		CoinTossConfig(&types.ParamCoinTossConfig{})
	if err != nil {
		t.Fatalf("toss/config: %v", err)
	}
	if resp.Data != (types.CoinTossConfigData{DailyLimit: 100, PerTargetLimit: 10,
		CancelWindowSeconds: 600, MinBalanceToToss: 1, InitialBalance: 5}) {
		t.Fatalf("生效参数被网关改写: %+v", resp.Data)
	}

	zeroed := &commerceCoinFake{toss: &coinrpc.GetTossConfigReply{}}
	got, err := NewCoinTossConfigLogic(context.Background(), commerceCoinSvc(zeroed)).
		CoinTossConfig(&types.ParamCoinTossConfig{})
	if err != nil {
		t.Fatalf("全 0 是合法结论（不是错误）: %v", err)
	}
	if got.Data != (types.CoinTossConfigData{}) {
		t.Fatalf("网关不得替缺失参数补「常见值」: %+v", got.Data)
	}
}

// TestCommerceOrderEventList_DuplicateLedgerRowsKept：同态台账行（REFUND_APPROVED→
// REFUND_APPROVED）是「款已退、权益未回收」的留证，网关不折叠、不排序。
func TestCommerceOrderEventList_DuplicateLedgerRowsKept(t *testing.T) {
	same := &tradeorderrpc.OrderEventInfo{
		EventId: 4002, OrderNo: "to_01HZZZZZZZZZZZZZZZZZZZZZ",
		FromState: tradeorderrpc.OrderState_ORDER_STATE_REFUND_APPROVED,
		ToState:   tradeorderrpc.OrderState_ORDER_STATE_REFUND_APPROVED,
		Operator:  "gateway/admin:77", Reason: "revoke failed: coin 扣回失败", Ctime: 1700000700,
	}
	fake := &commerceOrderFake{events: &tradeorderrpc.ListOrderEventsReply{
		Events: []*tradeorderrpc.OrderEventInfo{commerceOrderFullEvent(), same},
		Total:  2, Page: 1, Size: 20,
	}}
	resp, err := NewOrderEventListLogic(context.Background(), commerceOrderSvc(fake)).
		OrderEventList(&types.ParamOrderEventList{OrderNo: "to_01HZZZZZZZZZZZZZZZZZZZZZ", Page: 1, Size: 20})
	if err != nil {
		t.Fatalf("event/list: %v", err)
	}
	if len(resp.Data.List) != 2 || resp.Data.List[1].FromState != 9 || resp.Data.List[1].ToState != 9 {
		t.Fatalf("同态台账行被网关折叠了: %+v", resp.Data.List)
	}
	if resp.Data.List[1].Reason == "" || resp.Data.List[1].Operator == "" {
		t.Fatalf("台账的 operator/reason 是证据链本体，一位都不能裁: %+v", resp.Data.List[1])
	}
	if resp.Data.Total != 2 || resp.Data.Page != 1 || resp.Data.Size != 20 {
		t.Fatalf("分页三元组要照抄服务回显: %+v", resp.Data)
	}
}

// --- 7. 幂等重放与部分成功都是结论，不是错误 ---

func TestCommerceOrderRefundReplayIsSuccessWithFirstResult(t *testing.T) {
	fake := &commerceOrderFake{
		approve: &tradeorderrpc.ApproveRefundReply{
			Duplicated: true, Order: commerceOrderFull(),
			RefundNo:     "RF01HZZZZZZZZZZZZZZZZZZZZZ",
			RevokeDetail: "退款已完成，refunded_minor=5000/5000，本次为幂等重放",
		},
		reject: &tradeorderrpc.RejectRefundReply{Duplicated: true, Order: commerceOrderFull()},
	}
	svcCtx := commerceOrderSvc(fake)
	ctx := commerceOrderSession()
	approved, err := NewOrderRefundApproveLogic(ctx, svcCtx).OrderRefundApprove(commerceOrderGoodApprove())
	if err != nil {
		t.Fatalf("命中幂等键重放是成功结论: %v", err)
	}
	if !approved.Data.Duplicated || approved.Data.RefundNo == "" || approved.Data.Order.Version != 7 {
		t.Fatalf("首次结论被丢了: %+v", approved.Data)
	}
	rejected, err := NewOrderRefundRejectLogic(ctx, svcCtx).OrderRefundReject(commerceOrderGoodReject())
	if err != nil {
		t.Fatalf("驳回重放同样是成功结论: %v", err)
	}
	if !rejected.Data.Duplicated || rejected.Data.Order.OrderNo == "" {
		t.Fatalf("驳回首次结论被丢了: %+v", rejected.Data)
	}
}

// TestCommerceOrderApprove_PartialSuccessNotBeautified 是本域最容易被「美化」的一位：
// 服务把订单停在 REFUND_APPROVED 并**带结论正常返回**（款已退、权益没回收）。
// 网关必须把 revoke_detail 与订单当前状态一起转达，而不是报成功「全部完成」或反过来报错。
func TestCommerceOrderApprove_PartialSuccessNotBeautified(t *testing.T) {
	partial := commerceOrderFull()
	partial.State = tradeorderrpc.OrderState_ORDER_STATE_REFUND_APPROVED
	partial.FulfillDetail = "款已退、权益未回收：coin 扣回会超余额"
	partial.RefundedMinor = 5000
	fake := &commerceOrderFake{approve: &tradeorderrpc.ApproveRefundReply{
		Duplicated: false, Order: partial,
		RefundNo:     "RF01HZZZZZZZZZZZZZZZZZZZZZ",
		RevokeDetail: "coin.GrantCoin 扣回失败（硬币可能已消耗），未回收硬币",
	}}
	resp, err := NewOrderRefundApproveLogic(commerceOrderSession(), commerceOrderSvc(fake)).
		OrderRefundApprove(commerceOrderGoodApprove())
	if err != nil {
		t.Fatalf("部分成功是正常回复（带结论），网关不得自己造错: %v", err)
	}
	if resp.Data.RevokeDetail == "" {
		t.Fatalf("revoke_detail 是差异的唯一出口: %+v", resp.Data)
	}
	if resp.Data.Order.State != int32(tradeorderrpc.OrderState_ORDER_STATE_REFUND_APPROVED) {
		t.Fatalf("订单停在 REFUND_APPROVED 这一事实被改写了: %+v", resp.Data.Order)
	}
	if resp.Data.Order.RefundedMinor != 5000 {
		t.Fatalf("已退金额要原样转达（钱确实动了）: %+v", resp.Data.Order)
	}
}

func TestCommerceCoinGrant_ReplayIsSuccessWithFirstFlow(t *testing.T) {
	fake := &commerceCoinFake{grant: &coinrpc.GrantCoinReply{
		Duplicated: true, Account: commerceCoinFullAccount(), FlowId: 7007,
	}}
	resp, err := NewCoinGrantLogic(commerceCoinSession(), commerceCoinSvc(fake)).
		CoinGrant(commerceCoinGoodGrant())
	if err != nil {
		t.Fatalf("发放重放是成功结论: %v", err)
	}
	if !resp.Data.Duplicated || resp.Data.FlowId != 7007 || resp.Data.Account.Balance != 320 {
		t.Fatalf("首次结论被丢了: %+v", resp.Data)
	}
}

// --- 8. 投影（reply → API，逐字段） ---

func TestCommerceOrderProjection_FieldByField(t *testing.T) {
	full, fullEvent := commerceOrderFull(), commerceOrderFullEvent()
	cases := []struct {
		name  string
		diffs []string
	}{
		{"OrderInfo", commerceOrderDiff(orderToAPI(full), full)},
		{"OrderInfo(nil)", commerceOrderDiff(orderToAPI(nil), &tradeorderrpc.OrderInfo{})},
		{"OrderEventInfo", commerceOrderEventDiff(orderEventToAPI(fullEvent), fullEvent)},
		{"OrderEventInfo(nil)", commerceOrderEventDiff(orderEventToAPI(nil), &tradeorderrpc.OrderEventInfo{})},
	}
	for _, c := range cases {
		if len(c.diffs) > 0 {
			t.Fatalf("%s 投影丢字段/改字段: %v", c.name, c.diffs)
		}
	}
	if got := ordersToAPI([]*tradeorderrpc.OrderInfo{nil, full}); len(got) != 2 || got[0] != (types.OrderItem{}) {
		t.Fatalf("nil 订单行要给零值而不是丢掉或 panic: %+v", got)
	} else if got[1].OrderNo != full.GetOrderNo() {
		t.Fatalf("列表行与单行投影不一致: %+v", got[1])
	}
	if got := ordersToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空订单列表要投影成 []: %#v", got)
	}
	if got := orderEventsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空台账列表要投影成 []: %#v", got)
	}
	if got := orderStatesToRPC(nil); len(got) != 0 {
		t.Fatalf("空 states 要给空切片而不是默认集合: %v", got)
	}
}

func TestCommerceCoinProjection_FieldByField(t *testing.T) {
	acc, flow, toss := commerceCoinFullAccount(), commerceCoinFullFlow(), commerceCoinFullTossConfig()
	cases := []struct {
		name  string
		diffs []string
	}{
		{"CoinAccountInfo", commerceCoinAccountDiff(coinAccountToAPI(acc), acc)},
		{"CoinAccountInfo(nil)", commerceCoinAccountDiff(coinAccountToAPI(nil), &coinrpc.CoinAccountInfo{})},
		{"CoinFlowInfo", commerceCoinFlowDiff(coinFlowToAPI(flow), flow)},
		{"CoinFlowInfo(nil)", commerceCoinFlowDiff(coinFlowToAPI(nil), &coinrpc.CoinFlowInfo{})},
		{"GetTossConfigReply", commerceCoinTossDiff(coinTossConfigToAPI(toss), toss)},
		{"GetTossConfigReply(nil)", commerceCoinTossDiff(coinTossConfigToAPI(nil), &coinrpc.GetTossConfigReply{})},
	}
	for _, c := range cases {
		if len(c.diffs) > 0 {
			t.Fatalf("%s 投影丢字段/改字段: %v", c.name, c.diffs)
		}
	}
	if got := coinFlowsToAPI([]*coinrpc.CoinFlowInfo{nil, flow}); len(got) != 2 || got[0].FlowId != 0 {
		t.Fatalf("nil 流水行要给零值而不是 panic: %+v", got)
	} else if got[1].RequestId != flow.GetRequestId() {
		t.Fatalf("列表行与单行投影不一致: %+v", got[1])
	}
	if got := coinFlowsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空流水列表要投影成 []: %#v", got)
	}
	if got := coinTossConfigToAPI(nil); got != (types.CoinTossConfigData{}) {
		t.Fatalf("nil 参数回复要给零值而不是默认值: %+v", got)
	}
}

// TestCommerceCommerceOrderCoinProjection_NilReplyDoesNotPanic 覆盖「下游回了 nil 但没报错」：
// protobuf 的 Get* 访问器是 nil 安全的，投影必须落到空集合/零值而不是 panic。
// 这条只保证不崩——真实服务不会回 (nil, nil)，回了网关也不加业务判断去补数。
func TestCommerceOrderCoinProjection_NilReplyDoesNotPanic(t *testing.T) {
	orderCtx := commerceOrderSession()
	orderSvcCtx := commerceOrderSvc(&commerceOrderFake{})
	for _, route := range commerceOrderRoutes {
		if err := route.run(orderCtx, orderSvcCtx); err != nil {
			t.Fatalf("order %s: nil reply 不该变成错误: %v", route.name, err)
		}
	}
	coinCtx := commerceCoinSession()
	coinSvcCtx := commerceCoinSvc(&commerceCoinFake{})
	for _, route := range commerceCoinRoutes {
		if err := route.run(coinCtx, coinSvcCtx); err != nil {
			t.Fatalf("coin %s: nil reply 不该变成错误: %v", route.name, err)
		}
	}
}

// --- 投影 diff 助手：字段名直接进失败消息，避免「少投影一位」只报两句不等的字符串 ---

func commerceEq(diffs *[]string, name string, got, want any) {
	if got != want {
		*diffs = append(*diffs, fmt.Sprintf("%s: got %#v want %#v", name, got, want))
	}
}

func commerceOrderDiff(got types.OrderItem, want *tradeorderrpc.OrderInfo) []string {
	var diffs []string
	commerceEq(&diffs, "order_no", got.OrderNo, want.GetOrderNo())
	commerceEq(&diffs, "mid", got.Mid, want.GetMid())
	commerceEq(&diffs, "biz_type", got.BizType, int32(want.GetBizType()))
	commerceEq(&diffs, "plan_id", got.PlanId, want.GetPlanId())
	commerceEq(&diffs, "plan_code", got.PlanCode, want.GetPlanCode())
	commerceEq(&diffs, "title", got.Title, want.GetTitle())
	commerceEq(&diffs, "quantity", got.Quantity, want.GetQuantity())
	commerceEq(&diffs, "duration_days", got.DurationDays, want.GetDurationDays())
	commerceEq(&diffs, "coin_amount", got.CoinAmount, want.GetCoinAmount())
	commerceEq(&diffs, "unit_price_minor", got.UnitPriceMinor, want.GetUnitPriceMinor())
	commerceEq(&diffs, "amount_minor", got.AmountMinor, want.GetAmountMinor())
	commerceEq(&diffs, "refunded_minor", got.RefundedMinor, want.GetRefundedMinor())
	commerceEq(&diffs, "currency", got.Currency, want.GetCurrency())
	commerceEq(&diffs, "pay_method", got.PayMethod, int32(want.GetPayMethod()))
	commerceEq(&diffs, "state", got.State, int32(want.GetState()))
	commerceEq(&diffs, "fulfill_state", got.FulfillState, int32(want.GetFulfillState()))
	commerceEq(&diffs, "fulfill_attempts", got.FulfillAttempts, want.GetFulfillAttempts())
	commerceEq(&diffs, "fulfill_detail", got.FulfillDetail, want.GetFulfillDetail())
	commerceEq(&diffs, "payment_no", got.PaymentNo, want.GetPaymentNo())
	commerceEq(&diffs, "grant_ref", got.GrantRef, want.GetGrantRef())
	commerceEq(&diffs, "expire_at", got.ExpireAt, want.GetExpireAt())
	commerceEq(&diffs, "client_trace_id", got.ClientTraceId, want.GetClientTraceId())
	commerceEq(&diffs, "platform", got.Platform, int32(want.GetPlatform()))
	commerceEq(&diffs, "request_id", got.RequestId, want.GetRequestId())
	commerceEq(&diffs, "version", got.Version, want.GetVersion())
	commerceEq(&diffs, "created_at", got.CreatedAt, want.GetCreatedAt())
	commerceEq(&diffs, "updated_at", got.UpdatedAt, want.GetUpdatedAt())
	commerceEq(&diffs, "paid_at", got.PaidAt, want.GetPaidAt())
	commerceEq(&diffs, "fulfilled_at", got.FulfilledAt, want.GetFulfilledAt())
	commerceEq(&diffs, "closed_at", got.ClosedAt, want.GetClosedAt())
	return diffs
}

func commerceOrderEventDiff(got types.OrderEventItem, want *tradeorderrpc.OrderEventInfo) []string {
	var diffs []string
	commerceEq(&diffs, "event_id", got.EventId, want.GetEventId())
	commerceEq(&diffs, "order_no", got.OrderNo, want.GetOrderNo())
	commerceEq(&diffs, "from_state", got.FromState, int32(want.GetFromState()))
	commerceEq(&diffs, "to_state", got.ToState, int32(want.GetToState()))
	commerceEq(&diffs, "operator", got.Operator, want.GetOperator())
	commerceEq(&diffs, "reason", got.Reason, want.GetReason())
	commerceEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	return diffs
}

func commerceCoinAccountDiff(got types.CoinAccountItem, want *coinrpc.CoinAccountInfo) []string {
	var diffs []string
	commerceEq(&diffs, "mid", got.Mid, want.GetMid())
	commerceEq(&diffs, "balance", got.Balance, want.GetBalance())
	commerceEq(&diffs, "total_tossed", got.TotalTossed, want.GetTotalTossed())
	commerceEq(&diffs, "today_tossed", got.TodayTossed, want.GetTodayTossed())
	commerceEq(&diffs, "today_limit", got.TodayLimit, want.GetTodayLimit())
	commerceEq(&diffs, "per_target_limit", got.PerTargetLimit, want.GetPerTargetLimit())
	commerceEq(&diffs, "cancel_window_seconds", got.CancelWindowSeconds, want.GetCancelWindowSeconds())
	commerceEq(&diffs, "version", got.Version, want.GetVersion())
	commerceEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	commerceEq(&diffs, "mtime", got.Mtime, want.GetMtime())
	return diffs
}

func commerceCoinFlowDiff(got types.CoinFlowItem, want *coinrpc.CoinFlowInfo) []string {
	var diffs []string
	commerceEq(&diffs, "flow_id", got.FlowId, want.GetFlowId())
	commerceEq(&diffs, "mid", got.Mid, want.GetMid())
	commerceEq(&diffs, "flow_type", got.FlowType, int32(want.GetFlowType()))
	commerceEq(&diffs, "delta", got.Delta, want.GetDelta())
	commerceEq(&diffs, "balance_after", got.BalanceAfter, want.GetBalanceAfter())
	commerceEq(&diffs, "target_aid", got.TargetAid, want.GetTargetAid())
	commerceEq(&diffs, "biz_no", got.BizNo, want.GetBizNo())
	commerceEq(&diffs, "operator", got.Operator, want.GetOperator())
	commerceEq(&diffs, "request_id", got.RequestId, want.GetRequestId())
	commerceEq(&diffs, "remark", got.Remark, want.GetRemark())
	commerceEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	return diffs
}

// commerceCoinTossDiff 锁「生效参数逐位转达」：五位里任何一位被网关写死成「常见值」，
// 后台就会显示成一份并不存在的规则（AGENTS.md §6）。
func commerceCoinTossDiff(got types.CoinTossConfigData, want *coinrpc.GetTossConfigReply) []string {
	var diffs []string
	commerceEq(&diffs, "daily_limit", got.DailyLimit, want.GetDailyLimit())
	commerceEq(&diffs, "per_target_limit", got.PerTargetLimit, want.GetPerTargetLimit())
	commerceEq(&diffs, "cancel_window_seconds", got.CancelWindowSeconds, want.GetCancelWindowSeconds())
	commerceEq(&diffs, "min_balance_to_toss", got.MinBalanceToToss, want.GetMinBalanceToToss())
	commerceEq(&diffs, "initial_balance", got.InitialBalance, want.GetInitialBalance())
	return diffs
}
