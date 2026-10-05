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
	paymentrpc "go-video/services/payment/rpc"

	"google.golang.org/grpc"
)

// 本文件只锁 gateway/admin 面向 payment 的口径，刻意不断言资金域的业务结论
// （AGENTS.md §1/§5/§9）：状态机能不能推进、余额够不够扣、单次调整是否超上限、
// 币种是否支持、幂等号有没有被别的台账行用掉、渠道有没有被运营关掉——全部由
// services/payment 判定，网关自己复算一遍只会和服务口径漂移。
//
// 锁死的五条是网关自己的责任边界：
//  1. 不伪造成功/成功台账：没配 payment 客户端时八条路由一律 not configured，
//     绝不回空台账、0 余额或「渠道列表为空」；下游错误原样上抛
//     （duplicated=true 例外，那是首次结论而不是错误）。
//  2. 主体不信任请求体：operator 一律由会话渲染成 gateway/admin:<admin_id>，
//     表单自称的 operator 只能进日志当线索；无会话即 fail-closed，一次调用都不发。
//  3. 幂等键与 0 哨兵原样下传：idempotency_key→request_id 不改写；
//     page/size/mid=0/currency=""/state=0/biz_type=0 都是合法哨兵，网关不代填、不裁剪，
//     分页三元组照抄服务回显而不是回显请求值。
//  4. 真实资金能力不开口也不美化：not-configured / FailedPrecondition 一律如实透出，
//     DescribeChannels 逐字转达 sandbox_only 与每渠道 real_money，网关一侧都不硬编码。
//  5. 投影逐字段不丢：台账的 operator/request_id/reason 与 *_minor 是审计与资金口径的
//     证据位，裁掉一位后台就得靠猜。
//
// 打桩方式与 membership/private-message 一致：内嵌生成的 client 接口 + 只覆盖本域用到的
// 8 个方法，其余 6 个方法（OpenRecharge/CancelRecharge/CreatePayment/GetPayment/
// ClosePayment/RefundPayment）一旦被调用会 panic 在 nil 接口上——这正是 admin.api 里
// 「刻意不开的路由」那条边界的机器可检表达：充值单由用户自己开与取消，支付与退款的
// 唯一驱动方是 trade-order 的订单状态机，后台直连会造出第二个写主。不建 gRPC 连接、不碰数据库。

var errCommercePaymentDownstream = errors.New("commerce payment: downstream unavailable")

// errCommercePaymentWindowRequired 逐字照抄 services/payment/model.ErrListWindowRequired 的
// 消息文本，用来验证「服务的拒绝」在网关这一侧不被吞掉。不 import 该包：payment 服务正在
// 被并行改动，测试只关心消息原样透出这一件事，本地副本就够（也避免测试随服务代码漂移）。
var errCommercePaymentWindowRequired = errors.New(
	"payment: cross-user listing (mid=0) requires both from_ts and to_ts")

type commercePaymentFake struct {
	paymentrpc.PaymentClient

	calls    int
	lastCall string
	err      error

	walletReq    *paymentrpc.GetWalletReq
	wallet       *paymentrpc.GetWalletReply
	rechargesReq *paymentrpc.ListRechargesReq
	recharges    *paymentrpc.ListRechargesReply
	paymentsReq  *paymentrpc.ListPaymentsReq
	payments     *paymentrpc.ListPaymentsReply
	refundsReq   *paymentrpc.ListRefundsReq
	refunds      *paymentrpc.ListRefundsReply
	flowsReq     *paymentrpc.ListFlowsReq
	flows        *paymentrpc.ListFlowsReply
	channelsReq  *paymentrpc.DescribeChannelsReq
	channels     *paymentrpc.DescribeChannelsReply

	settleReq *paymentrpc.SettleSandboxRechargeReq
	settle    *paymentrpc.SettleSandboxRechargeReply
	adjustReq *paymentrpc.AdjustBalanceReq
	adjust    *paymentrpc.AdjustBalanceReply
}

// record 记一次调用；返回 true 表示这次要模拟下游失败。
func (f *commercePaymentFake) record(call string) bool {
	f.calls++
	f.lastCall = call
	return f.err != nil
}

func (f *commercePaymentFake) GetWallet(_ context.Context, in *paymentrpc.GetWalletReq,
	_ ...grpc.CallOption) (*paymentrpc.GetWalletReply, error) {
	f.walletReq = in
	if f.record("GetWallet") {
		return nil, f.err
	}
	return f.wallet, nil
}

func (f *commercePaymentFake) ListRecharges(_ context.Context, in *paymentrpc.ListRechargesReq,
	_ ...grpc.CallOption) (*paymentrpc.ListRechargesReply, error) {
	f.rechargesReq = in
	if f.record("ListRecharges") {
		return nil, f.err
	}
	return f.recharges, nil
}

func (f *commercePaymentFake) ListPayments(_ context.Context, in *paymentrpc.ListPaymentsReq,
	_ ...grpc.CallOption) (*paymentrpc.ListPaymentsReply, error) {
	f.paymentsReq = in
	if f.record("ListPayments") {
		return nil, f.err
	}
	return f.payments, nil
}

func (f *commercePaymentFake) ListRefunds(_ context.Context, in *paymentrpc.ListRefundsReq,
	_ ...grpc.CallOption) (*paymentrpc.ListRefundsReply, error) {
	f.refundsReq = in
	if f.record("ListRefunds") {
		return nil, f.err
	}
	return f.refunds, nil
}

func (f *commercePaymentFake) ListFlows(_ context.Context, in *paymentrpc.ListFlowsReq,
	_ ...grpc.CallOption) (*paymentrpc.ListFlowsReply, error) {
	f.flowsReq = in
	if f.record("ListFlows") {
		return nil, f.err
	}
	return f.flows, nil
}

func (f *commercePaymentFake) DescribeChannels(_ context.Context, in *paymentrpc.DescribeChannelsReq,
	_ ...grpc.CallOption) (*paymentrpc.DescribeChannelsReply, error) {
	f.channelsReq = in
	if f.record("DescribeChannels") {
		return nil, f.err
	}
	return f.channels, nil
}

func (f *commercePaymentFake) SettleSandboxRecharge(_ context.Context, in *paymentrpc.SettleSandboxRechargeReq,
	_ ...grpc.CallOption) (*paymentrpc.SettleSandboxRechargeReply, error) {
	f.settleReq = in
	if f.record("SettleSandboxRecharge") {
		return nil, f.err
	}
	return f.settle, nil
}

func (f *commercePaymentFake) AdjustBalance(_ context.Context, in *paymentrpc.AdjustBalanceReq,
	_ ...grpc.CallOption) (*paymentrpc.AdjustBalanceReply, error) {
	f.adjustReq = in
	if f.record("AdjustBalance") {
		return nil, f.err
	}
	return f.adjust, nil
}

func commercePaymentSvc(fake paymentrpc.PaymentClient) *svc.ServiceContext {
	return &svc.ServiceContext{Payment: fake}
}

// commercePaymentSession 是中间件判定通过后写入 context 的会话身份。
// admin_id=77 与表单里自称的 operator=9001 故意不同，用来验证「谁赢」。
func commercePaymentSession() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77, Roles: []string{"payment_operator"},
	})
}

// --- 台账样例（每位都非零，投影丢一位就能被 diff 点名）---

func commercePaymentFullWallet() *paymentrpc.WalletInfo {
	return &paymentrpc.WalletInfo{
		Mid: 10001, BalanceMinor: 12800, FrozenMinor: 0, // 本项目无预授权：frozen 恒 0
		Currency: "CNY", Version: 6, Ctime: 1700000000, Mtime: 1700000600,
	}
}

func commercePaymentFullRecharge() *paymentrpc.RechargeInfo {
	return &paymentrpc.RechargeInfo{
		RechargeNo: "RC01HZZZZZZZZZZZZZZZZZZZZZ", Mid: 10001, AmountMinor: 5000, Currency: "CNY",
		Channel: paymentrpc.PayChannel_PAY_CHANNEL_SANDBOX, State: paymentrpc.RechargeState_RECHARGE_STATE_SUCCESS,
		Operator: "gateway/admin:77", RequestId: "k-recharge-1", Reason: "沙箱人工结算",
		SettledAt: 1700000600, Ctime: 1700000000, Mtime: 1700000600,
	}
}

func commercePaymentFullLedger() *paymentrpc.PaymentInfo {
	return &paymentrpc.PaymentInfo{
		PaymentNo: "PM01HZZZZZZZZZZZZZZZZZZZZZ", BizOrderNo: "BO20260922001", Mid: 10001,
		AmountMinor: 2500, RefundedMinor: 500, Currency: "CNY",
		Method:  paymentrpc.PayMethod_PAY_METHOD_BALANCE,
		State:   paymentrpc.PaymentState_PAYMENT_STATE_PARTIALLY_REFUNDED,
		Subject: "大会员月卡", PaidAt: 1700000300, ExpireAt: 0, RequestId: "k-pay-1",
		Ctime: 1700000000, Mtime: 1700000600,
		// 契约缺口：这两位在 admin.api 的 PaymentPaymentItem 里没有落点，
		// 样例里照旧填上，投影测试只校验 14 个有落点的位（见 conv_payment_commerce.go 注释）。
		Operator: "trade-order", Remark: "订单履约扣款",
	}
}

func commercePaymentFullRefund() *paymentrpc.RefundInfo {
	return &paymentrpc.RefundInfo{
		RefundNo: "RF01HZZZZZZZZZZZZZZZZZZZZZ", PaymentNo: "PM01HZZZZZZZZZZZZZZZZZZZZZ",
		BizOrderNo: "BO20260922001", Mid: 10001, AmountMinor: 500, Currency: "CNY",
		State:       paymentrpc.RefundState_REFUND_STATE_SUCCEEDED,
		Destination: "BALANCE", Operator: "gateway/admin:77",
		RequestId: "k-refund-1", Reason: "订单退款审批通过", Ctime: 1700000700,
	}
}

func commercePaymentFullFlow() *paymentrpc.FlowInfo {
	return &paymentrpc.FlowInfo{
		FlowId: 9007, Mid: 10001, BizType: paymentrpc.FlowBizType_FLOW_BIZ_TYPE_ADMIN_ADJUST,
		BizNo: "AJ01HZZZZZZZZZZZZZZZZZZZZZ", DeltaMinor: -300, BalanceAfterMinor: 12500,
		Currency: "CNY", Remark: "沙箱差错更正", Operator: "gateway/admin:77",
		RequestId: "k-adjust-1", Ctime: 1700000800,
	}
}

func commercePaymentFullChannels() *paymentrpc.DescribeChannelsReply {
	return &paymentrpc.DescribeChannelsReply{
		SandboxOnly: true,
		Channels: []*paymentrpc.ChannelState{{
			Channel: paymentrpc.PayChannel_PAY_CHANNEL_SANDBOX, Enabled: true, RealMoney: false,
			Note: "沙箱台账渠道：不产生真实资金移动",
		}},
		CurrencyDefault: "CNY",
	}
}

// --- 两条写路由的合法入参样例（每个用例取新副本，避免互相污染）---

func commercePaymentGoodSettle() *types.ParamPaymentRechargeSettle {
	return &types.ParamPaymentRechargeSettle{
		RechargeNo:     "RC01HZZZZZZZZZZZZZZZZZZZZZ",
		Reason:         "沙箱单据卡在 PENDING，人工核对后入账",
		Operator:       9001,
		IdempotencyKey: "k-settle-1",
		TraceId:        "t-settle-1",
	}
}

func commercePaymentGoodAdjust() *types.ParamPaymentBalanceAdjust {
	return &types.ParamPaymentBalanceAdjust{
		Mid: 10001, DeltaMinor: -300, Currency: "CNY", Reason: "测试数据差错更正",
		Operator: 9001, IdempotencyKey: "k-adjust-1", TraceId: "t-adjust-1",
	}
}

// --- 1. 不伪造成功 ---

// commercePaymentRoutes 把八条路由收成一个表：nil 客户端、下游错误两条门禁都要逐条覆盖，
// 漏一条就等于留了一个「后台看到空资金台账」的口子。
var commercePaymentRoutes = []struct {
	name string
	run  func(ctx context.Context, svcCtx *svc.ServiceContext) error
}{
	{"wallet/get", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewPaymentWalletGetLogic(ctx, s).PaymentWalletGet(&types.ParamPaymentWalletGet{Mid: 10001})
		return err
	}},
	{"recharge/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewPaymentRechargeListLogic(ctx, s).
			PaymentRechargeList(&types.ParamPaymentRechargeList{Page: 1, Size: 20})
		return err
	}},
	{"payment/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewPaymentListLogic(ctx, s).PaymentList(&types.ParamPaymentList{Page: 1, Size: 20})
		return err
	}},
	{"refund/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewPaymentRefundListLogic(ctx, s).
			PaymentRefundList(&types.ParamPaymentRefundList{PaymentNo: "PM01", Page: 1, Size: 20})
		return err
	}},
	{"flow/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewPaymentFlowListLogic(ctx, s).PaymentFlowList(&types.ParamPaymentFlowList{Page: 1, Size: 20})
		return err
	}},
	{"channel/describe", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewPaymentChannelDescribeLogic(ctx, s).PaymentChannelDescribe(&types.ParamPaymentChannelDescribe{})
		return err
	}},
	{"recharge/settle", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewPaymentRechargeSettleLogic(ctx, s).PaymentRechargeSettle(commercePaymentGoodSettle())
		return err
	}},
	{"balance/adjust", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewPaymentBalanceAdjustLogic(ctx, s).PaymentBalanceAdjust(commercePaymentGoodAdjust())
		return err
	}},
}

func TestCommercePaymentAllRoutes_NilClientDoesNotFakeLedger(t *testing.T) {
	empty := &svc.ServiceContext{} // Payment 未配置
	ctx := commercePaymentSession()
	for _, route := range commercePaymentRoutes {
		err := route.run(ctx, empty)
		if !errors.Is(err, errPaymentServiceNotConfigured) {
			t.Fatalf("%s: 未配置客户端必须回 not configured 而不是空台账/0 余额，实际 %v", route.name, err)
		}
		if !strings.Contains(err.Error(), "payment") {
			t.Fatalf("%s: 错误消息要点名是哪个下游没接，实际 %s", route.name, err.Error())
		}
	}
}

func TestCommercePaymentAllRoutes_DownstreamErrorPropagatesVerbatim(t *testing.T) {
	bad := &commercePaymentFake{err: errCommercePaymentDownstream}
	ctx := commercePaymentSession()
	for i, route := range commercePaymentRoutes {
		err := route.run(ctx, commercePaymentSvc(bad))
		if !errors.Is(err, errCommercePaymentDownstream) {
			t.Fatalf("%s: 下游错误必须原样上抛（不得折成成功或空数据），实际 %v", route.name, err)
		}
		if bad.calls != i+1 {
			t.Fatalf("%s: 每条路由都要真打一次下游（累计 %d 次，lastCall=%s）", route.name, bad.calls, bad.lastCall)
		}
	}
	if bad.calls != len(commercePaymentRoutes) {
		t.Fatalf("八条路由各一次，实际 %d", bad.calls)
	}
}

// --- 2. 操作者身份 ---

// TestCommercePaymentWriteRoutes_OperatorAlwaysFromSession 覆盖两条写路由：
// 进 RPC 的 operator 永远是会话渲染值，request_id 永远是幂等键原值
// （payment.proto 的 req 没有 trace 字段，trace_id 只进日志）。
func TestCommercePaymentWriteRoutes_OperatorAlwaysFromSession(t *testing.T) {
	fake := &commercePaymentFake{
		settle: &paymentrpc.SettleSandboxRechargeReply{
			Duplicated: false, Recharge: commercePaymentFullRecharge(),
			Wallet: commercePaymentFullWallet(), FlowId: 9001,
		},
		adjust: &paymentrpc.AdjustBalanceReply{
			Duplicated: false, Wallet: commercePaymentFullWallet(), FlowId: 9007,
		},
	}
	svcCtx := commercePaymentSvc(fake)
	ctx := commercePaymentSession()
	if _, err := NewPaymentRechargeSettleLogic(ctx, svcCtx).PaymentRechargeSettle(commercePaymentGoodSettle()); err != nil {
		t.Fatalf("recharge/settle: %v", err)
	}
	if _, err := NewPaymentBalanceAdjustLogic(ctx, svcCtx).PaymentBalanceAdjust(commercePaymentGoodAdjust()); err != nil {
		t.Fatalf("balance/adjust: %v", err)
	}

	wantOperator := "gateway/admin:77" // 会话 admin_id=77；表单自称的 9001 必须作废
	cases := []struct {
		route     string
		operator  string
		requestID string
		want      string
	}{
		{"recharge/settle", fake.settleReq.GetOperator(), fake.settleReq.GetRequestId(), "k-settle-1"},
		{"balance/adjust", fake.adjustReq.GetOperator(), fake.adjustReq.GetRequestId(), "k-adjust-1"},
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

func TestCommercePaymentWriteRoutes_FailClosedWithoutSession(t *testing.T) {
	// 拿不到会话身份 = 这条路由没被 AdminPermission 保护（权限表/挂载漂移），
	// 必须在下传前拒绝，一次 RPC 都不能发。AdminID<=0 与「完全没有身份」都要挡住。
	sessions := []struct {
		name string
		ctx  context.Context
	}{
		{"无身份", context.Background()},
		{"身份非法（admin_id=0）", middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: 0})},
	}
	for _, route := range commercePaymentRoutes {
		if !commercePaymentIsWriteRoute(route.name) {
			continue // 只跑两条写路由
		}
		for _, session := range sessions {
			fake := &commercePaymentFake{}
			err := route.run(session.ctx, commercePaymentSvc(fake))
			if !errors.Is(err, errPaymentSessionRequired) {
				t.Fatalf("%s/%s: 缺会话身份必须 fail-closed，实际 %v", route.name, session.name, err)
			}
			if fake.calls != 0 {
				t.Fatalf("%s/%s: fail-closed 却已经打了下游 %d 次（lastCall=%s）",
					route.name, session.name, fake.calls, fake.lastCall)
			}
		}
	}
}

// commercePaymentIsWriteRoute 判本域两条写路由：admin.api 里只有它们挂了 AdminPermission。
func commercePaymentIsWriteRoute(name string) bool {
	switch name {
	case "recharge/settle", "balance/adjust":
		return true
	default:
		return false
	}
}

func TestCommercePaymentWriteRoutes_RequiredFieldsGateBeforeDownstream(t *testing.T) {
	ctx := commercePaymentSession()
	cases := []struct {
		name string
		run  func(svcCtx *svc.ServiceContext) error
		want string
	}{
		{"结算缺单号", func(s *svc.ServiceContext) error {
			r := commercePaymentGoodSettle()
			r.RechargeNo = "  "
			_, err := NewPaymentRechargeSettleLogic(ctx, s).PaymentRechargeSettle(r)
			return err
		}, "recharge_no"},
		{"结算缺幂等键", func(s *svc.ServiceContext) error {
			r := commercePaymentGoodSettle()
			r.IdempotencyKey = ""
			_, err := NewPaymentRechargeSettleLogic(ctx, s).PaymentRechargeSettle(r)
			return err
		}, "idempotency_key"},
		{"结算无理由（.api 必填，比服务更严）", func(s *svc.ServiceContext) error {
			r := commercePaymentGoodSettle()
			r.Reason = " "
			_, err := NewPaymentRechargeSettleLogic(ctx, s).PaymentRechargeSettle(r)
			return err
		}, "reason"},
		{"结算 operator 位非法", func(s *svc.ServiceContext) error {
			r := commercePaymentGoodSettle()
			r.Operator = 0
			_, err := NewPaymentRechargeSettleLogic(ctx, s).PaymentRechargeSettle(r)
			return err
		}, "operator"},
		{"调整缺幂等键", func(s *svc.ServiceContext) error {
			r := commercePaymentGoodAdjust()
			r.IdempotencyKey = ""
			_, err := NewPaymentBalanceAdjustLogic(ctx, s).PaymentBalanceAdjust(r)
			return err
		}, "idempotency_key"},
		{"调整无理由", func(s *svc.ServiceContext) error {
			r := commercePaymentGoodAdjust()
			r.Reason = ""
			_, err := NewPaymentBalanceAdjustLogic(ctx, s).PaymentBalanceAdjust(r)
			return err
		}, "reason"},
		{"调整 mid=0（资金接口没有 0 号账号）", func(s *svc.ServiceContext) error {
			r := commercePaymentGoodAdjust()
			r.Mid = 0
			_, err := NewPaymentBalanceAdjustLogic(ctx, s).PaymentBalanceAdjust(r)
			return err
		}, "mid"},
		{"调整 delta=0（无意义流水）", func(s *svc.ServiceContext) error {
			r := commercePaymentGoodAdjust()
			r.DeltaMinor = 0
			_, err := NewPaymentBalanceAdjustLogic(ctx, s).PaymentBalanceAdjust(r)
			return err
		}, "delta_minor"},
	}
	for _, c := range cases {
		fake := &commercePaymentFake{}
		err := c.run(commercePaymentSvc(fake))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误消息要点名字段 %s，实际 %v", c.name, c.want, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 已被拒的入参不该打到下游（lastCall=%s）", c.name, fake.lastCall)
		}
	}
}

// TestCommercePaymentAdjust_MoneyAndCurrencyPassThroughUntouched 锁「不做二次判定」：
// 负 delta 是合法的扣减方向，超上限/扣成负余额/币种不支持全部由服务拒；网关既不取绝对值、
// 不夹区间，也不给空币种代填默认值。
func TestCommercePaymentAdjust_MoneyAndCurrencyPassThroughUntouched(t *testing.T) {
	fake := &commercePaymentFake{adjust: &paymentrpc.AdjustBalanceReply{
		Duplicated: false, Wallet: commercePaymentFullWallet(), FlowId: 9008,
	}}
	req := commercePaymentGoodAdjust()
	req.DeltaMinor = -99999999999 // 绝对值上限归服务判（ErrAdjustAmountOutOfRange）
	req.Currency = ""             // 空 = 服务按默认币种归一，网关不代填 "CNY"
	resp, err := NewPaymentBalanceAdjustLogic(commercePaymentSession(), commercePaymentSvc(fake)).
		PaymentBalanceAdjust(req)
	if err != nil {
		t.Fatalf("区间与币种合法性归服务，网关不得预先改写: %v", err)
	}
	got := fake.adjustReq
	if got.GetDeltaMinor() != -99999999999 {
		t.Fatalf("delta_minor 被网关改写（不得取绝对值或夹区间）: %+v", got)
	}
	if got.GetCurrency() != "" {
		t.Fatalf("空币种被网关代填成 %q，默认值只属于服务配置", got.GetCurrency())
	}
	if resp.Data.Wallet.BalanceMinor != 12800 || resp.Data.FlowId != 9008 {
		t.Fatalf("余额与流水号要转达服务回读值: %+v", resp.Data)
	}
}

// --- 3. 分页 / 时间窗 / 0 哨兵 ---

func TestCommercePaymentRechargeList_PagingPassesThroughAndEchoesServiceValues(t *testing.T) {
	fake := &commercePaymentFake{recharges: &paymentrpc.ListRechargesReply{
		Recharges: []*paymentrpc.RechargeInfo{commercePaymentFullRecharge()},
		Total:     37, Page: 2, Size: 100, // 服务把请求值归一后回显（超上限时它是拒绝而不是悄悄裁）
	}}
	resp, err := NewPaymentRechargeListLogic(context.Background(), commercePaymentSvc(fake)).
		PaymentRechargeList(&types.ParamPaymentRechargeList{
			State: 2, Mid: 10001, FromTs: 1700000000, ToTs: 1700000900, Page: 2, Size: 500,
		})
	if err != nil {
		t.Fatalf("翻页不该被网关拒绝: %v", err)
	}
	if fake.rechargesReq.GetSize() != 500 || fake.rechargesReq.GetPage() != 2 {
		t.Fatalf("page/size 被网关改写: %+v", fake.rechargesReq)
	}
	if fake.rechargesReq.GetState() != paymentrpc.RechargeState_RECHARGE_STATE_SUCCESS ||
		fake.rechargesReq.GetMid() != 10001 ||
		fake.rechargesReq.GetFromTs() != 1700000000 || fake.rechargesReq.GetToTs() != 1700000900 {
		t.Fatalf("过滤条件没按契约下传: %+v", fake.rechargesReq)
	}
	if resp.Data.Total != 37 || resp.Data.Page != 2 || resp.Data.Size != 100 {
		t.Fatalf("分页三元组必须照抄服务回显: %+v", resp.Data)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封必须固定 code=0/message=ok/ttl=0: %+v", resp)
	}
}

// TestCommercePaymentLists_ZeroMeansNoFilterOrCrossUser 锁 0 哨兵：
// mid=0 是运营面的跨用户查询、state/method/biz_type=0 是「不按该位过滤」、
// page/size=0 是「用服务默认页」，网关一个都不代填；空结果投影成 [] 而不是 null。
func TestCommercePaymentLists_ZeroMeansNoFilterOrCrossUser(t *testing.T) {
	fake := &commercePaymentFake{
		recharges: &paymentrpc.ListRechargesReply{},
		payments:  &paymentrpc.ListPaymentsReply{},
		refunds:   &paymentrpc.ListRefundsReply{},
		flows:     &paymentrpc.ListFlowsReply{},
	}
	svcCtx := commercePaymentSvc(fake)
	ctx := context.Background()
	rechargeResp, err := NewPaymentRechargeListLogic(ctx, svcCtx).PaymentRechargeList(&types.ParamPaymentRechargeList{})
	if err != nil {
		t.Fatalf("全 0 的充值台账查询是合法请求: %v", err)
	}
	paymentResp, err := NewPaymentListLogic(ctx, svcCtx).PaymentList(&types.ParamPaymentList{})
	if err != nil {
		t.Fatalf("全 0 的支付台账查询是合法请求: %v", err)
	}
	refundResp, err := NewPaymentRefundListLogic(ctx, svcCtx).PaymentRefundList(&types.ParamPaymentRefundList{})
	if err != nil {
		t.Fatalf("全 0 的退款台账查询是合法请求: %v", err)
	}
	flowResp, err := NewPaymentFlowListLogic(ctx, svcCtx).PaymentFlowList(&types.ParamPaymentFlowList{})
	if err != nil {
		t.Fatalf("全 0 的流水查询是合法请求: %v", err)
	}

	// 0 位一律原样下传（不代填 mid=1、不代填 size=20、不把 UNSPECIFIED 改成 1）。
	for _, req := range []interface {
		GetMid() int64
		GetPage() int64
		GetSize() int64
	}{fake.rechargesReq, fake.paymentsReq, fake.refundsReq, fake.flowsReq} {
		if req.GetMid() != 0 || req.GetPage() != 0 || req.GetSize() != 0 {
			t.Fatalf("%T: 0 是合法哨兵，网关不得代填: mid=%d page=%d size=%d",
				req, req.GetMid(), req.GetPage(), req.GetSize())
		}
	}
	if fake.rechargesReq.GetState() != paymentrpc.RechargeState_RECHARGE_STATE_UNSPECIFIED ||
		fake.paymentsReq.GetState() != paymentrpc.PaymentState_PAYMENT_STATE_UNSPECIFIED ||
		fake.paymentsReq.GetMethod() != paymentrpc.PayMethod_PAY_METHOD_UNSPECIFIED ||
		fake.flowsReq.GetBizType() != paymentrpc.FlowBizType_FLOW_BIZ_TYPE_UNSPECIFIED {
		t.Fatalf("过滤位被网关代填: %+v %+v %+v", fake.rechargesReq, fake.paymentsReq, fake.flowsReq)
	}
	if rechargeResp.Data.List == nil || paymentResp.Data.List == nil ||
		refundResp.Data.List == nil || flowResp.Data.List == nil {
		t.Fatalf("空列表要投影成 []，客户端才能直接遍历: %#v %#v %#v %#v",
			rechargeResp.Data.List, paymentResp.Data.List, refundResp.Data.List, flowResp.Data.List)
	}
}

func TestCommercePaymentAllRoutes_RejectImpossibleShapes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		run  func(svcCtx *svc.ServiceContext) error
		want string
	}{
		{"余额查询 mid=0", func(s *svc.ServiceContext) error {
			_, err := NewPaymentWalletGetLogic(ctx, s).PaymentWalletGet(&types.ParamPaymentWalletGet{Mid: 0})
			return err
		}, "mid"},
		{"余额查询 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewPaymentWalletGetLogic(ctx, s).PaymentWalletGet(&types.ParamPaymentWalletGet{Mid: -1})
			return err
		}, "mid"},
		{"充值台账 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewPaymentRechargeListLogic(ctx, s).PaymentRechargeList(&types.ParamPaymentRechargeList{Mid: -1})
			return err
		}, "mid"},
		{"充值台账 state 为负", func(s *svc.ServiceContext) error {
			_, err := NewPaymentRechargeListLogic(ctx, s).PaymentRechargeList(&types.ParamPaymentRechargeList{State: -1})
			return err
		}, "state"},
		{"充值台账页码为负", func(s *svc.ServiceContext) error {
			_, err := NewPaymentRechargeListLogic(ctx, s).PaymentRechargeList(&types.ParamPaymentRechargeList{Page: -1})
			return err
		}, "page"},
		{"充值台账窗口为负", func(s *svc.ServiceContext) error {
			_, err := NewPaymentRechargeListLogic(ctx, s).PaymentRechargeList(&types.ParamPaymentRechargeList{ToTs: -1})
			return err
		}, "to_ts"},
		{"充值台账窗口倒置", func(s *svc.ServiceContext) error {
			_, err := NewPaymentRechargeListLogic(ctx, s).PaymentRechargeList(&types.ParamPaymentRechargeList{
				FromTs: 200, ToTs: 100,
			})
			return err
		}, "from_ts"},
		{"支付台账 method 为负", func(s *svc.ServiceContext) error {
			_, err := NewPaymentListLogic(ctx, s).PaymentList(&types.ParamPaymentList{Method: -1})
			return err
		}, "method"},
		{"支付台账 state 为负", func(s *svc.ServiceContext) error {
			_, err := NewPaymentListLogic(ctx, s).PaymentList(&types.ParamPaymentList{State: -1})
			return err
		}, "state"},
		{"支付台账页大小为负", func(s *svc.ServiceContext) error {
			_, err := NewPaymentListLogic(ctx, s).PaymentList(&types.ParamPaymentList{Size: -1})
			return err
		}, "size"},
		{"退款台账 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewPaymentRefundListLogic(ctx, s).PaymentRefundList(&types.ParamPaymentRefundList{Mid: -1})
			return err
		}, "mid"},
		{"流水台账 biz_type 为负", func(s *svc.ServiceContext) error {
			_, err := NewPaymentFlowListLogic(ctx, s).PaymentFlowList(&types.ParamPaymentFlowList{BizType: -1})
			return err
		}, "biz_type"},
		{"流水台账窗口倒置", func(s *svc.ServiceContext) error {
			_, err := NewPaymentFlowListLogic(ctx, s).PaymentFlowList(&types.ParamPaymentFlowList{
				FromTs: 200, ToTs: 100,
			})
			return err
		}, "from_ts"},
	}
	for _, c := range cases {
		fake := &commercePaymentFake{}
		err := c.run(commercePaymentSvc(fake))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 要在下传前拒掉并点名字段 %s，实际 %v", c.name, c.want, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 已被拒的入参不该打到下游（lastCall=%s）", c.name, fake.lastCall)
		}
	}
}

// TestCommercePaymentRefundList_AdminAPHasNoTimeWindowSlots 记录本域的 .api 缺口：
// ListRefundsReq 有 from_ts/to_ts，后台表单没有这两格（本轮禁止改 .api），
// 因此下传恒为 0；跨用户查询会被服务按有界性规则拒绝，而网关必须把那句拒绝如实透出，
// 既不自造时间窗，也不把它折叠成「没有退款记录」的成功。
func TestCommercePaymentRefundList_AdminAPIHasNoTimeWindowSlots(t *testing.T) {
	fake := &commercePaymentFake{err: errCommercePaymentWindowRequired}
	_, err := NewPaymentRefundListLogic(context.Background(), commercePaymentSvc(fake)).
		PaymentRefundList(&types.ParamPaymentRefundList{Page: 1, Size: 20})
	if !errors.Is(err, errCommercePaymentWindowRequired) {
		t.Fatalf("服务的有界性拒绝必须原样透出: %v", err)
	}
	if fake.refundsReq.GetFromTs() != 0 || fake.refundsReq.GetToTs() != 0 {
		t.Fatalf("网关不得凭空造时间窗: %+v", fake.refundsReq)
	}
}

// --- 4. 真实资金能力不开口：如实转达，不美化 ---

func TestCommercePaymentChannelDescribe_ReportsSandboxFactsVerbatim(t *testing.T) {
	fake := &commercePaymentFake{channels: commercePaymentFullChannels()}
	resp, err := NewPaymentChannelDescribeLogic(context.Background(), commercePaymentSvc(fake)).
		PaymentChannelDescribe(&types.ParamPaymentChannelDescribe{})
	if err != nil {
		t.Fatalf("channel/describe: %v", err)
	}
	if !resp.Data.SandboxOnly || len(resp.Data.Channels) != 1 {
		t.Fatalf("服务说了只有沙箱，网关就要如实转达: %+v", resp.Data)
	}
	got := resp.Data.Channels[0]
	if got.Channel != int32(paymentrpc.PayChannel_PAY_CHANNEL_SANDBOX) || !got.Enabled ||
		got.RealMoney || got.Note == "" {
		t.Fatalf("real_money/note 是「没有真实资金」的证据位，一位都不能改: %+v", got)
	}
}

// TestCommercePaymentChannelDescribe_DoesNotFabricateCapabilities 是上一条的反面：
// 网关不硬编码 sandbox_only、不替渠道补条目。服务回什么就是什么——包括回
// 「沙箱被运营关掉」（enabled=false）与「一个渠道都没有」，这两种情况都必须让后台
// 看得见，而不是被美化成可用渠道列表或成功空结果。
func TestCommercePaymentChannelDescribe_DoesNotFabricateCapabilities(t *testing.T) {
	fake := &commercePaymentFake{channels: &paymentrpc.DescribeChannelsReply{
		SandboxOnly: false, // 例如：下游被换成真接渠道的构建，网关不得覆盖这个结论
		Channels: []*paymentrpc.ChannelState{{
			Channel: paymentrpc.PayChannel_PAY_CHANNEL_SANDBOX, Enabled: false, RealMoney: false,
			Note: "渠道被运营关闭",
		}},
		CurrencyDefault: "CNY",
	}}
	resp, err := NewPaymentChannelDescribeLogic(context.Background(), commercePaymentSvc(fake)).
		PaymentChannelDescribe(&types.ParamPaymentChannelDescribe{})
	if err != nil {
		t.Fatalf("channel/describe: %v", err)
	}
	if resp.Data.SandboxOnly {
		t.Fatalf("sandbox_only 被网关硬编码成 true 了: %+v", resp.Data)
	}
	if len(resp.Data.Channels) != 1 || resp.Data.Channels[0].Enabled {
		t.Fatalf("enabled=false 必须透出（关掉沙箱时不能假装可用）: %+v", resp.Data.Channels)
	}

	empty := &commercePaymentFake{channels: &paymentrpc.DescribeChannelsReply{}}
	got, err := NewPaymentChannelDescribeLogic(context.Background(), commercePaymentSvc(empty)).
		PaymentChannelDescribe(&types.ParamPaymentChannelDescribe{})
	if err != nil {
		t.Fatalf("空渠道列表是合法结论，不是错误: %v", err)
	}
	if got.Data.Channels == nil || len(got.Data.Channels) != 0 {
		t.Fatalf("空渠道列表投影成 []（但网关没有替它补一个沙箱条目）: %#v", got.Data.Channels)
	}
}

func TestCommercePaymentSettle_NotConfiguredChannelPropagatesAsFailedPrecondition(t *testing.T) {
	// 服务的 ErrChannelNotConfigured / ErrRechargeNotFound 是「钱没到账」的真实结论，
	// 网关既不退化成 duplicated=true，也不回一个 0 余额的成功信封。
	notConfigured := errors.New("payment: payment channel not configured")
	fake := &commercePaymentFake{err: notConfigured}
	_, err := NewPaymentRechargeSettleLogic(commercePaymentSession(), commercePaymentSvc(fake)).
		PaymentRechargeSettle(commercePaymentGoodSettle())
	if !errors.Is(err, notConfigured) {
		t.Fatalf("渠道未配置必须原样透出: %v", err)
	}
	if fake.calls != 1 || fake.lastCall != "SettleSandboxRecharge" {
		t.Fatalf("要真打一次下游才知道没配（calls=%d lastCall=%s）", fake.calls, fake.lastCall)
	}
}

func TestCommercePaymentSettle_ReplayIsSuccessWithFirstResult(t *testing.T) {
	fake := &commercePaymentFake{settle: &paymentrpc.SettleSandboxRechargeReply{
		Duplicated: true, Recharge: commercePaymentFullRecharge(),
		Wallet: commercePaymentFullWallet(), FlowId: 9001,
	}}
	resp, err := NewPaymentRechargeSettleLogic(commercePaymentSession(), commercePaymentSvc(fake)).
		PaymentRechargeSettle(commercePaymentGoodSettle())
	if err != nil {
		t.Fatalf("命中幂等键重放是成功结论: %v", err)
	}
	if !resp.Data.Duplicated || resp.Data.FlowId != 9001 ||
		resp.Data.Recharge.State != int32(paymentrpc.RechargeState_RECHARGE_STATE_SUCCESS) ||
		resp.Data.Wallet.Version != 6 {
		t.Fatalf("首次结论被丢了: %+v", resp.Data)
	}
}

// TestCommercePaymentWalletGet_NoFoundSlotInContract 锁住 §1 的读侧口径：
// 契约里 GetWalletReply 没有 found，服务又明写「账户不存在就是 0 余额」，
// 所以网关只能转达那一行，既不补 found=true 暗示查到了账户，也不把未配置或
// 读失败折叠成 0 余额（后者由前两条门禁覆盖）。
func TestCommercePaymentWalletGet_NoFoundSlotInContract(t *testing.T) {
	fake := &commercePaymentFake{wallet: &paymentrpc.GetWalletReply{Wallet: &paymentrpc.WalletInfo{
		Mid: 10001, Currency: "CNY", // 从未开过户：服务回的就是这一行全 0 余额
	}}}
	resp, err := NewPaymentWalletGetLogic(context.Background(), commercePaymentSvc(fake)).
		PaymentWalletGet(&types.ParamPaymentWalletGet{Mid: 10001})
	if err != nil {
		t.Fatalf("0 余额不是错误: %v", err)
	}
	if resp.Data.Wallet.BalanceMinor != 0 || resp.Data.Wallet.FrozenMinor != 0 ||
		resp.Data.Wallet.Mid != 10001 {
		t.Fatalf("余额位要原样转达: %+v", resp.Data.Wallet)
	}
	if fake.walletReq.GetCurrency() != "" {
		t.Fatalf("空币种要交给服务归一，网关不代填: %+v", fake.walletReq)
	}
}

// --- 5. reply → API 投影（逐字段） ---

func TestCommercePaymentProjection_FieldByField(t *testing.T) {
	cases := []struct {
		name  string
		diffs []string
	}{
		{"WalletInfo", commercePaymentWalletDiff(paymentWalletToAPI(commercePaymentFullWallet()), commercePaymentFullWallet())},
		{"RechargeInfo", commercePaymentRechargeDiff(paymentRechargeToAPI(commercePaymentFullRecharge()), commercePaymentFullRecharge())},
		{"PaymentInfo", commercePaymentLedgerDiff(paymentLedgerToAPI(commercePaymentFullLedger()), commercePaymentFullLedger())},
		{"RefundInfo", commercePaymentRefundDiff(paymentRefundToAPI(commercePaymentFullRefund()), commercePaymentFullRefund())},
		{"FlowInfo", commercePaymentFlowDiff(paymentFlowToAPI(commercePaymentFullFlow()), commercePaymentFullFlow())},
		{"ChannelState", commercePaymentChannelDiff(paymentChannelToAPI(commercePaymentFullChannels().GetChannels()[0]),
			commercePaymentFullChannels().GetChannels()[0])},
		{"nil WalletInfo", commercePaymentWalletDiff(paymentWalletToAPI(nil), &paymentrpc.WalletInfo{})},
		{"nil RechargeInfo", commercePaymentRechargeDiff(paymentRechargeToAPI(nil), &paymentrpc.RechargeInfo{})},
		{"nil PaymentInfo", commercePaymentLedgerDiff(paymentLedgerToAPI(nil), &paymentrpc.PaymentInfo{})},
		{"nil RefundInfo", commercePaymentRefundDiff(paymentRefundToAPI(nil), &paymentrpc.RefundInfo{})},
		{"nil FlowInfo", commercePaymentFlowDiff(paymentFlowToAPI(nil), &paymentrpc.FlowInfo{})},
		{"nil ChannelState", commercePaymentChannelDiff(paymentChannelToAPI(nil), &paymentrpc.ChannelState{})},
	}
	for _, c := range cases {
		if len(c.diffs) > 0 {
			t.Fatalf("%s 投影丢字段/改字段: %v", c.name, c.diffs)
		}
	}

	// 列表投影：nil 行给零值而不是丢掉，顺序与条数保持。
	if got := paymentRechargesToAPI([]*paymentrpc.RechargeInfo{nil, commercePaymentFullRecharge()}); len(got) != 2 {
		t.Fatalf("充值列表投影条数不对: %d", len(got))
	} else if got[1].RechargeNo != "RC01HZZZZZZZZZZZZZZZZZZZZZ" {
		t.Fatalf("充值列表行与单行投影不一致: %+v", got[1])
	}
	if got := paymentLedgersToAPI([]*paymentrpc.PaymentInfo{nil}); len(got) != 1 || got[0].PaymentNo != "" {
		t.Fatalf("支付列表 nil 行要零值而不是 panic: %+v", got)
	}
	if got := paymentRefundsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空退款列表要投影成 []: %#v", got)
	}
	if got := paymentFlowsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空流水列表要投影成 []: %#v", got)
	}
	if got := paymentChannelsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空渠道列表要投影成 []: %#v", got)
	}
}

// TestCommercePaymentProjection_NilReplyDoesNotPanic 覆盖「下游回了 nil 但没报错」：
// protobuf 的 Get* 访问器是 nil 安全的，投影必须落到空集合/零值而不是 panic。
// 这条只保证不崩——真实服务不会回 (nil, nil)，回了我也不加业务判断去补数。
func TestCommercePaymentProjection_NilReplyDoesNotPanic(t *testing.T) {
	ctx := commercePaymentSession()
	svcCtx := commercePaymentSvc(&commercePaymentFake{})
	for _, route := range commercePaymentRoutes {
		if err := route.run(ctx, svcCtx); err != nil {
			t.Fatalf("%s: nil reply 不该变成错误: %v", route.name, err)
		}
	}
}

// --- 投影 diff 助手：字段名直接进失败消息，避免「少投影一位」只报两句不等的字符串 ---

func commercePaymentEq(diffs *[]string, name string, got, want any) {
	if got != want {
		*diffs = append(*diffs, fmt.Sprintf("%s: got %#v want %#v", name, got, want))
	}
}

func commercePaymentWalletDiff(got types.PaymentWalletItem, want *paymentrpc.WalletInfo) []string {
	var diffs []string
	commercePaymentEq(&diffs, "mid", got.Mid, want.GetMid())
	commercePaymentEq(&diffs, "balance_minor", got.BalanceMinor, want.GetBalanceMinor())
	commercePaymentEq(&diffs, "frozen_minor", got.FrozenMinor, want.GetFrozenMinor())
	commercePaymentEq(&diffs, "currency", got.Currency, want.GetCurrency())
	commercePaymentEq(&diffs, "version", got.Version, want.GetVersion())
	commercePaymentEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	commercePaymentEq(&diffs, "mtime", got.Mtime, want.GetMtime())
	return diffs
}

func commercePaymentRechargeDiff(got types.PaymentRechargeItem, want *paymentrpc.RechargeInfo) []string {
	var diffs []string
	commercePaymentEq(&diffs, "recharge_no", got.RechargeNo, want.GetRechargeNo())
	commercePaymentEq(&diffs, "mid", got.Mid, want.GetMid())
	commercePaymentEq(&diffs, "amount_minor", got.AmountMinor, want.GetAmountMinor())
	commercePaymentEq(&diffs, "currency", got.Currency, want.GetCurrency())
	commercePaymentEq(&diffs, "channel", got.Channel, int32(want.GetChannel()))
	commercePaymentEq(&diffs, "state", got.State, int32(want.GetState()))
	commercePaymentEq(&diffs, "operator", got.Operator, want.GetOperator())
	commercePaymentEq(&diffs, "request_id", got.RequestId, want.GetRequestId())
	commercePaymentEq(&diffs, "reason", got.Reason, want.GetReason())
	commercePaymentEq(&diffs, "settled_at", got.SettledAt, want.GetSettledAt())
	commercePaymentEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	commercePaymentEq(&diffs, "mtime", got.Mtime, want.GetMtime())
	return diffs
}

// commercePaymentLedgerDiff 只比 PaymentPaymentItem 有的 14 位。
// PaymentInfo.operator/remark 在本类型里没有落点（契约缺口，见 conv_payment_commerce.go），
// 这里刻意不比：一旦哪天 .api 补了字段，漏投影就会被 diff 抓到。
func commercePaymentLedgerDiff(got types.PaymentPaymentItem, want *paymentrpc.PaymentInfo) []string {
	var diffs []string
	commercePaymentEq(&diffs, "payment_no", got.PaymentNo, want.GetPaymentNo())
	commercePaymentEq(&diffs, "biz_order_no", got.BizOrderNo, want.GetBizOrderNo())
	commercePaymentEq(&diffs, "mid", got.Mid, want.GetMid())
	commercePaymentEq(&diffs, "amount_minor", got.AmountMinor, want.GetAmountMinor())
	commercePaymentEq(&diffs, "refunded_minor", got.RefundedMinor, want.GetRefundedMinor())
	commercePaymentEq(&diffs, "currency", got.Currency, want.GetCurrency())
	commercePaymentEq(&diffs, "method", got.Method, int32(want.GetMethod()))
	commercePaymentEq(&diffs, "state", got.State, int32(want.GetState()))
	commercePaymentEq(&diffs, "subject", got.Subject, want.GetSubject())
	commercePaymentEq(&diffs, "paid_at", got.PaidAt, want.GetPaidAt())
	commercePaymentEq(&diffs, "expire_at", got.ExpireAt, want.GetExpireAt())
	commercePaymentEq(&diffs, "request_id", got.RequestId, want.GetRequestId())
	commercePaymentEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	commercePaymentEq(&diffs, "mtime", got.Mtime, want.GetMtime())
	return diffs
}

func commercePaymentRefundDiff(got types.PaymentRefundItem, want *paymentrpc.RefundInfo) []string {
	var diffs []string
	commercePaymentEq(&diffs, "refund_no", got.RefundNo, want.GetRefundNo())
	commercePaymentEq(&diffs, "payment_no", got.PaymentNo, want.GetPaymentNo())
	commercePaymentEq(&diffs, "biz_order_no", got.BizOrderNo, want.GetBizOrderNo())
	commercePaymentEq(&diffs, "mid", got.Mid, want.GetMid())
	commercePaymentEq(&diffs, "amount_minor", got.AmountMinor, want.GetAmountMinor())
	commercePaymentEq(&diffs, "currency", got.Currency, want.GetCurrency())
	commercePaymentEq(&diffs, "state", got.State, int32(want.GetState()))
	commercePaymentEq(&diffs, "destination", got.Destination, want.GetDestination())
	commercePaymentEq(&diffs, "operator", got.Operator, want.GetOperator())
	commercePaymentEq(&diffs, "request_id", got.RequestId, want.GetRequestId())
	commercePaymentEq(&diffs, "reason", got.Reason, want.GetReason())
	commercePaymentEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	return diffs
}

func commercePaymentFlowDiff(got types.PaymentFlowItem, want *paymentrpc.FlowInfo) []string {
	var diffs []string
	commercePaymentEq(&diffs, "flow_id", got.FlowId, want.GetFlowId())
	commercePaymentEq(&diffs, "mid", got.Mid, want.GetMid())
	commercePaymentEq(&diffs, "biz_type", got.BizType, int32(want.GetBizType()))
	commercePaymentEq(&diffs, "biz_no", got.BizNo, want.GetBizNo())
	commercePaymentEq(&diffs, "delta_minor", got.DeltaMinor, want.GetDeltaMinor())
	commercePaymentEq(&diffs, "balance_after_minor", got.BalanceAfterMinor, want.GetBalanceAfterMinor())
	commercePaymentEq(&diffs, "currency", got.Currency, want.GetCurrency())
	commercePaymentEq(&diffs, "remark", got.Remark, want.GetRemark())
	commercePaymentEq(&diffs, "operator", got.Operator, want.GetOperator())
	commercePaymentEq(&diffs, "request_id", got.RequestId, want.GetRequestId())
	commercePaymentEq(&diffs, "ctime", got.Ctime, want.GetCtime())
	return diffs
}

func commercePaymentChannelDiff(got types.PaymentChannelItem, want *paymentrpc.ChannelState) []string {
	var diffs []string
	commercePaymentEq(&diffs, "channel", got.Channel, int32(want.GetChannel()))
	commercePaymentEq(&diffs, "enabled", got.Enabled, want.GetEnabled())
	commercePaymentEq(&diffs, "real_money", got.RealMoney, want.GetRealMoney())
	commercePaymentEq(&diffs, "note", got.Note, want.GetNote())
	return diffs
}
