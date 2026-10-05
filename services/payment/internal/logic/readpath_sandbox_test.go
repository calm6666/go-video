package logic

// 沙箱语义与读侧接口。
//
// 这个文件要证明两件在无库条件下完全可以证明的事：
//  1. 「只有沙箱、没有真实资金」是可查询的事实，不是一句 README 承诺：
//     DescribeChannels 必须显式声明 sandbox_only / real_money=false，
//     并把未配置的出金能力点名列出；任何接口都不因配置写错而放行真实渠道，
//     也不得在渠道不可用时「假装受理成功」；
//  2. 读侧不改动台账，且跨用户查询必须自带边界（时间窗 + page/size），
//     读不到单据是 found=false 或空列表，但 DB 故障不能被折叠成空结果冒充成功。

import (
	"context"
	"strings"
	"testing"

	"go-video/services/payment/internal/config"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"google.golang.org/grpc/codes"
)

// --- DescribeChannels：沙箱语义的可查询事实 ---

func TestDescribeChannelsNeverClaimsRealMoney(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())

	reply, err := NewDescribeChannelsLogic(context.Background(), svcCtx).
		DescribeChannels(&rpc.DescribeChannelsReq{})
	if err != nil {
		t.Fatalf("DescribeChannels: %v", err)
	}
	if !reply.SandboxOnly {
		t.Fatal("sandbox_only 必须为 true：本服务不存在真实资金通道")
	}
	if len(reply.Channels) != 1 {
		t.Fatalf("渠道条目 = %d，只能自述一个沙箱渠道", len(reply.Channels))
	}
	ch := reply.Channels[0]
	if ch.Channel != rpc.PayChannel_PAY_CHANNEL_SANDBOX {
		t.Fatalf("channel = %s，枚举里没有也不该出现真实渠道", ch.Channel)
	}
	if ch.RealMoney {
		t.Fatal("real_money 必须恒为 false，前端据此标注「非真实资金」")
	}
	if !ch.Enabled {
		t.Fatal("默认配置下沙箱应可用")
	}
	if reply.CurrencyDefault != "CNY" {
		t.Fatalf("currency_default = %q，应取配置币种", reply.CurrencyDefault)
	}
	// 出金能力必须点名「未配置」，含糊表述会让调用方以为只是暂时不可用。
	for _, item := range []string{"渠道回调验签", "原路退回银行卡", "提现", "打款出金", "对账", "发票"} {
		if !strings.Contains(ch.Note, item) {
			t.Fatalf("note 未声明 %q 未配置：%s", item, ch.Note)
		}
	}
	if !strings.Contains(ch.Note, "不产生真实资金移动") {
		t.Fatalf("note 必须写明不产生真实资金移动：%s", ch.Note)
	}
	// 自述是纯读操作：不建账户、不留流水。
	if len(db.wallets) != 0 || len(db.flows) != 0 {
		t.Fatalf("DescribeChannels 写了台账：wallets=%d flows=%d", len(db.wallets), len(db.flows))
	}
}

func TestDescribeChannelsEnabledTracksChannelGate(t *testing.T) {
	cfg := defaultPaymentConf()
	cfg.AllowedChannels = []string{"ALIPAY"} // 运营把沙箱关掉，写了个没接入的渠道名
	cfg.DefaultCurrency = " usd "
	svcCtx, _ := newTestSvc(t, cfg)

	reply, err := NewDescribeChannelsLogic(context.Background(), svcCtx).
		DescribeChannels(&rpc.DescribeChannelsReq{})
	if err != nil {
		t.Fatalf("DescribeChannels: %v", err)
	}
	if reply.Channels[0].Enabled {
		t.Fatal("沙箱被关掉时 enabled 必须是 false —— 调用方不必等写入失败才知道不可用")
	}
	if reply.Channels[0].RealMoney || !reply.SandboxOnly {
		t.Fatal("渠道被关掉不得把服务自述成「接了真实渠道」")
	}
	if reply.CurrencyDefault != "USD" {
		t.Fatalf("currency_default = %q，应归一配置币种", reply.CurrencyDefault)
	}
}

// TestNotConfiguredPathsNeverFakeSuccess 「未配置」一律显式报错的跨方法契约表。
// 每条都是资金口径：宁可失败，也不能返回一个没有台账支撑的成功。
func TestNotConfiguredPathsNeverFakeSuccess(t *testing.T) {
	t.Run("充值只认沙箱渠道", func(t *testing.T) {
		svcCtx, db := newTestSvc(t, defaultPaymentConf())
		for _, ch := range []rpc.PayChannel{rpc.PayChannel_PAY_CHANNEL_UNSPECIFIED, rpc.PayChannel(99)} {
			in := openReq(7, 100, "req-ch-"+ch.String())
			in.Channel = ch
			_, err := NewOpenRechargeLogic(context.Background(), svcCtx).OpenRecharge(in)
			requireStatus(t, err, codes.InvalidArgument, "channel="+ch.String())
		}
		if len(db.recharges) != 0 {
			t.Fatalf("不存在的渠道却建了单：%d 行", len(db.recharges))
		}
	})

	t.Run("支付方式只认余额与沙箱", func(t *testing.T) {
		svcCtx, db := newTestSvc(t, defaultPaymentConf())
		db.seedWallet(7, 1000, "CNY")
		for _, method := range []rpc.PayMethod{rpc.PayMethod_PAY_METHOD_UNSPECIFIED, rpc.PayMethod(99)} {
			in := payReq("order-m", "req-m", 7, 100)
			in.Method = method
			_, err := NewCreatePaymentLogic(context.Background(), svcCtx).CreatePayment(in)
			requireStatus(t, err, codes.InvalidArgument, "method="+method.String())
		}
		requireBalance(t, db, 7, 1000)
		if len(db.payments) != 0 {
			t.Fatal("未受理的支付方式却建了单")
		}
	})

	t.Run("退款不退渠道", func(t *testing.T) {
		svcCtx, db := newTestSvc(t, defaultPaymentConf())
		paidRefundablePayment(db, "PM_nc", "order-nc", 7, 1000)
		db.seedWallet(7, 0, "CNY")
		in := refundReq("PM_nc", 100, "req-nc")
		in.ToBalance = false
		_, err := NewRefundPaymentLogic(context.Background(), svcCtx).RefundPayment(in)
		requireStatus(t, err, codes.FailedPrecondition, "not configured")
		requireFlows(t, db, 0)
	})

	t.Run("沙箱关闭时整条链路都不可用", func(t *testing.T) {
		cfg := defaultPaymentConf()
		cfg.AllowedChannels = []string{"ALIPAY"}
		svcCtx, db := newTestSvc(t, cfg)
		ctx := context.Background()

		_, err := NewOpenRechargeLogic(ctx, svcCtx).OpenRecharge(openReq(7, 100, "req-off-chain"))
		requireSentinel(t, err, model.ErrChannelNotConfigured, codes.FailedPrecondition)
		in := payReq("order-off", "req-off-pay", 7, 100)
		in.Method = rpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL
		_, err = NewCreatePaymentLogic(ctx, svcCtx).CreatePayment(in)
		requireSentinel(t, err, model.ErrChannelNotConfigured, codes.FailedPrecondition)
		if len(db.recharges) != 0 || len(db.payments) != 0 || len(db.flows) != 0 || len(db.wallets) != 0 {
			t.Fatal("渠道未配置却留下了台账")
		}
	})
}

// --- GetPayment ---

func TestGetPaymentByKey(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paid := paidBalancePayment(db, "PM_g1", "order-g1", "req-g1", 7, 1000)
	paid.Subject = "会员月卡"

	byNo, err := NewGetPaymentLogic(context.Background(), svcCtx).
		GetPayment(&rpc.GetPaymentReq{PaymentNo: "PM_g1"})
	if err != nil {
		t.Fatalf("GetPayment by payment_no: %v", err)
	}
	if !byNo.Found || byNo.Payment.PaymentNo != "PM_g1" || byNo.Payment.BizOrderNo != "order-g1" {
		t.Fatalf("按单号读取结论不符：%+v", byNo.Payment)
	}
	byOrder, err := NewGetPaymentLogic(context.Background(), svcCtx).
		GetPayment(&rpc.GetPaymentReq{BizOrderNo: " order-g1 "})
	if err != nil {
		t.Fatalf("GetPayment by biz_order_no: %v", err)
	}
	if !byOrder.Found || byOrder.Payment.PaymentNo != paid.PaymentNo {
		t.Fatalf("按订单号读取结论不符：%+v", byOrder.Payment)
	}
	if byOrder.Payment.Subject != paid.Subject || byOrder.Payment.Method != rpc.PayMethod_PAY_METHOD_BALANCE {
		t.Fatalf("投影丢了台账字段：subject=%q method=%s", byOrder.Payment.Subject, byOrder.Payment.Method)
	}
	requireFlows(t, db, 0)
	requireBalance(t, db, 7, 0)
}

func TestGetPaymentNotFoundIsNotAnError(t *testing.T) {
	svcCtx, _ := newTestSvc(t, defaultPaymentConf())

	reply, err := NewGetPaymentLogic(context.Background(), svcCtx).
		GetPayment(&rpc.GetPaymentReq{PaymentNo: "PM_ghost"})
	if err != nil {
		t.Fatalf("读不到单据不该报错: %v", err)
	}
	if reply.Found || reply.Payment != nil {
		t.Fatalf("读不到却返回了单据：%+v", reply.Payment)
	}
}

func TestGetPaymentKeyValidation(t *testing.T) {
	svcCtx, _ := newTestSvc(t, defaultPaymentConf())
	logic := NewGetPaymentLogic(context.Background(), svcCtx)

	_, err := logic.GetPayment(&rpc.GetPaymentReq{})
	requireSentinel(t, err, model.ErrGetPaymentKeyRequired, codes.InvalidArgument)
	_, err = logic.GetPayment(&rpc.GetPaymentReq{PaymentNo: "PM_a", BizOrderNo: "order-a"})
	requireSentinel(t, err, model.ErrGetPaymentKeyExclusive, codes.InvalidArgument)
	_, err = logic.GetPayment(&rpc.GetPaymentReq{PaymentNo: strings.Repeat("p", 41)})
	requireStatus(t, err, codes.InvalidArgument, "payment_no too long")
	_, err = logic.GetPayment(&rpc.GetPaymentReq{BizOrderNo: strings.Repeat("o", 65)})
	requireStatus(t, err, codes.InvalidArgument, "biz_order_no too long")
}

// --- 列表接口的有界性口径 ---

// TestCrossUserListingRequiresBoundedWindow mid=0 是运营面跨用户查询：
// 资金表行数只增不减，不给窗口或超窗都必须拒绝，而不是悄悄扫全表。
func TestCrossUserListingRequiresBoundedWindow(t *testing.T) {
	now := fakeNow()
	cases := []struct {
		name     string
		from, to int64
		sentinel error
	}{
		{"不给窗口", 0, 0, model.ErrListWindowRequired},
		{"只给起点", now - 3600, 0, model.ErrListWindowRequired},
		{"只给终点", 0, now, model.ErrListWindowRequired},
		{"窗口倒置", now, now - 3600, model.ErrInvalidTimeRange},
		{"窗口超上限", now - 90*24*3600, now, model.ErrListWindowTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, _ := newTestSvc(t, defaultPaymentConf())
			ctx := context.Background()

			_, err := NewListFlowsLogic(ctx, svcCtx).ListFlows(&rpc.ListFlowsReq{
				Mid: 0, FromTs: tc.from, ToTs: tc.to, Page: 1, Size: 20})
			requireSentinel(t, err, tc.sentinel, codes.InvalidArgument)
			_, err = NewListPaymentsLogic(ctx, svcCtx).ListPayments(&rpc.ListPaymentsReq{
				Mid: 0, FromTs: tc.from, ToTs: tc.to, Page: 1, Size: 20})
			requireSentinel(t, err, tc.sentinel, codes.InvalidArgument)
			_, err = NewListRechargesLogic(ctx, svcCtx).ListRecharges(&rpc.ListRechargesReq{
				Mid: 0, FromTs: tc.from, ToTs: tc.to, Page: 1, Size: 20})
			requireSentinel(t, err, tc.sentinel, codes.InvalidArgument)
			_, err = NewListRefundsLogic(ctx, svcCtx).ListRefunds(&rpc.ListRefundsReq{
				Mid: 0, FromTs: tc.from, ToTs: tc.to, Page: 1, Size: 20})
			requireSentinel(t, err, tc.sentinel, codes.InvalidArgument)
		})
	}
}

// TestPerUserListingNeedsNoWindow 指定 mid 时集合天然有界，不强制时间窗。
func TestPerUserListingNeedsNoWindow(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 500, "CNY")
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_l1", RequestId: "req-l1", Mid: 7,
		AmountMinor: 1000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})
	db.seedFlow(&model.Flow{Mid: 7, BizType: model.FlowBizAdminAdjust, BizNo: "AJ_l1",
		DeltaMinor: 500, BalanceAfterMinor: 500, Currency: "CNY", RequestId: "req-l0"})
	paidRefundablePayment(db, "PM_l1", "order-l1", 7, 1000)

	ctx := context.Background()
	flows, err := NewListFlowsLogic(ctx, svcCtx).ListFlows(&rpc.ListFlowsReq{Mid: 7})
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	if flows.Total != 1 || len(flows.Flows) != 1 || flows.Flows[0].BizNo != "AJ_l1" {
		t.Fatalf("流水分页结论不符：total=%d flows=%+v", flows.Total, flows.Flows)
	}
	if flows.Page != 1 || flows.Size != defaultListSize {
		t.Fatalf("page/size 未归一：%d/%d，期望 1/%d", flows.Page, flows.Size, defaultListSize)
	}
	pays, err := NewListPaymentsLogic(ctx, svcCtx).ListPayments(&rpc.ListPaymentsReq{Mid: 7})
	if err != nil {
		t.Fatalf("ListPayments: %v", err)
	}
	if pays.Total != 1 || pays.Payments[0].PaymentNo != "PM_l1" {
		t.Fatalf("支付分页结论不符：%+v", pays.Payments)
	}
	if pays.Payments[0].RefundedMinor != 0 || pays.Payments[0].AmountMinor != 1000 {
		t.Fatalf("支付投影丢了金额/退款进度字段：%+v", pays.Payments[0])
	}
	recharges, err := NewListRechargesLogic(ctx, svcCtx).ListRecharges(&rpc.ListRechargesReq{Mid: 7})
	if err != nil {
		t.Fatalf("ListRecharges: %v", err)
	}
	if recharges.Total != 1 || recharges.Recharges[0].RechargeNo != "RC_l1" {
		t.Fatalf("充值分页结论不符：%+v", recharges.Recharges)
	}
	refunds, err := NewListRefundsLogic(ctx, svcCtx).ListRefunds(&rpc.ListRefundsReq{Mid: 7})
	if err != nil {
		t.Fatalf("ListRefunds: %v", err)
	}
	if refunds.Total != 0 || len(refunds.Refunds) != 0 {
		t.Fatalf("空结果应是合法空台账：%+v", refunds)
	}
	// 读侧一律不动台账
	requireBalance(t, db, 7, 500)
	requireFlows(t, db, 1)
}

// TestListRefundsByPaymentNoNeedsNoWindow 给了 payment_no 就按单号收敛，
// 可以省掉跨用户时间窗；但两者都没有仍然要拒。
func TestListRefundsByPaymentNoNeedsNoWindow(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paidRefundablePayment(db, "PM_lp", "order-lp", 7, 1000)
	db.seedWallet(7, 0, "CNY")
	db.seedRefund(&model.Refund{RefundNo: "RF_lp", RequestId: "req-lp", PaymentNo: "PM_lp",
		BizOrderNo: "order-lp", Mid: 7, AmountMinor: 400, Currency: "CNY",
		State: model.RefundStateSucceeded, Destination: model.DestinationBalance})

	reply, err := NewListRefundsLogic(context.Background(), svcCtx).
		ListRefunds(&rpc.ListRefundsReq{Mid: 0, PaymentNo: "PM_lp"})
	if err != nil {
		t.Fatalf("按单号收敛的跨用户查询应可用: %v", err)
	}
	if reply.Total != 1 || reply.Refunds[0].RefundNo != "RF_lp" {
		t.Fatalf("按单号查询结论不符：%+v", reply.Refunds)
	}
	if reply.Refunds[0].Destination != model.DestinationBalance {
		t.Fatal("退款投影丢了去向")
	}

	_, err = NewListRefundsLogic(context.Background(), svcCtx).
		ListRefunds(&rpc.ListRefundsReq{Mid: 0, PaymentNo: strings.Repeat("p", 41)})
	requireStatus(t, err, codes.InvalidArgument, "payment_no too long")
}

// TestListFlowsRejectsUnknownBizType 过滤条件写错必须报错：
// 把「查错类型」显示成「没有资金变动」是最危险的运营误读。
func TestListFlowsRejectsUnknownBizType(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedFlow(&model.Flow{Mid: 7, BizType: model.FlowBizRefund, BizNo: "RF_f",
		DeltaMinor: 100, BalanceAfterMinor: 100, Currency: "CNY", RequestId: "req-f"})
	ctx := context.Background()

	for _, bt := range []rpc.FlowBizType{rpc.FlowBizType(9), rpc.FlowBizType(-1)} {
		_, err := NewListFlowsLogic(ctx, svcCtx).ListFlows(&rpc.ListFlowsReq{Mid: 7, BizType: bt})
		requireStatus(t, err, codes.InvalidArgument, "biz_type="+bt.String())
	}
	// 合法但无命中的类型是空结果，不是错误。
	reply, err := NewListFlowsLogic(ctx, svcCtx).ListFlows(&rpc.ListFlowsReq{
		Mid: 7, BizType: rpc.FlowBizType_FLOW_BIZ_TYPE_PAYMENT})
	if err != nil {
		t.Fatalf("空结果不该报错: %v", err)
	}
	if reply.Total != 0 || len(reply.Flows) != 0 {
		t.Fatalf("空结果结论不符：%+v", reply)
	}
	byBizNo, err := NewListFlowsLogic(ctx, svcCtx).ListFlows(&rpc.ListFlowsReq{Mid: 7, BizNo: "RF_f"})
	if err != nil {
		t.Fatalf("按 biz_no 过滤: %v", err)
	}
	if byBizNo.Total != 1 || byBizNo.Flows[0].FlowId == 0 {
		t.Fatalf("按 biz_no 过滤结论不符：%+v", byBizNo.Flows)
	}
}

func TestListingNegativeMidAndOversizePageRejected(t *testing.T) {
	svcCtx, _ := newTestSvc(t, defaultPaymentConf())
	ctx := context.Background()
	cfg := defaultPaymentConf()

	_, err := NewListFlowsLogic(ctx, svcCtx).ListFlows(&rpc.ListFlowsReq{Mid: -1})
	requireSentinel(t, err, model.ErrInvalidMid, codes.InvalidArgument)
	_, err = NewListPaymentsLogic(ctx, svcCtx).ListPayments(&rpc.ListPaymentsReq{Mid: -1})
	requireSentinel(t, err, model.ErrInvalidMid, codes.InvalidArgument)
	_, err = NewListRechargesLogic(ctx, svcCtx).ListRecharges(&rpc.ListRechargesReq{Mid: -1})
	requireSentinel(t, err, model.ErrInvalidMid, codes.InvalidArgument)
	_, err = NewListRefundsLogic(ctx, svcCtx).ListRefunds(&rpc.ListRefundsReq{Mid: -1, PaymentNo: "PM_x"})
	requireSentinel(t, err, model.ErrInvalidMid, codes.InvalidArgument)

	// size 超上限与深翻页都是拒绝，不是「悄悄少给」。
	_, err = NewListFlowsLogic(ctx, svcCtx).ListFlows(&rpc.ListFlowsReq{Mid: 7, Size: cfg.MaxPageSize + 1})
	requireSentinel(t, err, model.ErrPageSizeTooLarge, codes.InvalidArgument)
	_, err = NewListFlowsLogic(ctx, svcCtx).ListFlows(&rpc.ListFlowsReq{Mid: 7, Page: cfg.MaxListOffset + 1})
	requireSentinel(t, err, model.ErrListOffsetTooDeep, codes.InvalidArgument)
}

// TestListRechargesHonorsStateFilter 状态过滤是运营页找「开了没入账的单」的唯一手段，
// 过滤条件丢了就会把别的状态单据混进来。
func TestListRechargesHonorsStateFilter(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_p", RequestId: "req-p", Mid: 7,
		AmountMinor: 1000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStatePending})
	db.seedRecharge(&model.Recharge{RechargeNo: "RC_s", RequestId: "req-s", Mid: 7,
		AmountMinor: 1000, Currency: "CNY", Channel: model.ChannelSandbox, State: model.RechargeStateSuccess})

	reply, err := NewListRechargesLogic(context.Background(), svcCtx).ListRecharges(&rpc.ListRechargesReq{
		Mid: 7, State: rpc.RechargeState_RECHARGE_STATE_PENDING})
	if err != nil {
		t.Fatalf("ListRecharges: %v", err)
	}
	if reply.Total != 1 || len(reply.Recharges) != 1 || reply.Recharges[0].RechargeNo != "RC_p" {
		t.Fatalf("状态过滤失效：total=%d rows=%+v", reply.Total, reply.Recharges)
	}
}

// TestKnownChannelsHasNoRealChannel 配置门禁本身只是「按名字放行」，
// 真正兜住沙箱语义的是：可识别渠道清单里除了 SANDBOX 谁都没有，
// 且写入路径在查配置之前就先按枚举拒掉非沙箱渠道 —— 运营把AllowedChannels写脏也开不了真渠道。
func TestKnownChannelsHasNoRealChannel(t *testing.T) {
	if got := config.KnownChannels(); len(got) != 1 || got[0] != config.ChannelSandbox {
		t.Fatalf("KnownChannels() = %v，本服务只允许 SANDBOX 一个渠道名", got)
	}
	dirty := config.PaymentConf{DefaultCurrency: "CNY", AllowedChannels: []string{"ALIPAY", "WECHAT_PAY", "  "}}
	if dirty.AllowsChannel("") || dirty.AllowsChannel("   ") {
		t.Fatal("空渠道名不能被放行（否则 UNSPECIFIED 会溜进门禁）")
	}
	if !dirty.AllowsChannel(" alipay ") {
		t.Fatal("门禁本身应按配置原文放行——它的职责只是照单执行")
	}

	// 但写入路径不能只依赖门禁：枚举不认识的渠道一律先拒。
	svcCtx, db := newTestSvc(t, dirty)
	in := openReq(7, 100, "req-dirty")
	in.Channel = rpc.PayChannel(99)
	_, err := NewOpenRechargeLogic(context.Background(), svcCtx).OpenRecharge(in)
	requireStatus(t, err, codes.InvalidArgument, "only SANDBOX exists; channel=99")
	if len(db.recharges) != 0 {
		t.Fatal("脏配置下建出了单据")
	}
}
