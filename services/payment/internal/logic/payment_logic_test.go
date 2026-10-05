package logic

// 支付与关单两条写路径：CreatePayment（建单即终态）/ ClosePayment（只关未支付）。
//
// 本文件要证明的口径（createpaymentlogic.go / closepaymentlogic.go 头部注释在此落成断言）：
//  1. BALANCE 是「条件扣减 + 落支付单 + 落流水」同事务：余额不足时三者一个都不写；
//  2. SANDBOX_CHANNEL 受理即 PAID，但钱没经过余额账户 —— 不得动余额、不得写流水；
//  3. biz_order_no 唯一 = 一单一支付：同单号同金额同币种按重放返回，
//     金额或币种不同一律 AlreadyExists 冲突，绝不静默改价；
//  4. 唯一键冲突（并发对手先提交）回滚后回读，收敛成「重放首单」或「号被复用」，
//     不返回未提交数据，也不留下第二次扣款；
//  5. 关闭只推进 PENDING，且不动余额不写流水；已 PAID/已退款的单必须被挡回去
//     并指明走 RefundPayment —— 关闭不是回滚资金的口子。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/payment/internal/config"
	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"google.golang.org/grpc/codes"
)

func payReq(bizOrderNo, requestID string, mid, amount int64) *rpc.CreatePaymentReq {
	return &rpc.CreatePaymentReq{
		BizOrderNo: bizOrderNo, Mid: mid, AmountMinor: amount, Currency: "CNY",
		Method: rpc.PayMethod_PAY_METHOD_BALANCE, Subject: "会员月卡",
		Operator: "user", RequestId: requestID,
	}
}

func mustCreatePayment(t *testing.T, svcCtx *svc.ServiceContext, in *rpc.CreatePaymentReq) *rpc.CreatePaymentReply {
	t.Helper()
	reply, err := NewCreatePaymentLogic(context.Background(), svcCtx).CreatePayment(in)
	if err != nil {
		t.Fatalf("CreatePayment(biz_order_no=%s) 失败: %v", in.BizOrderNo, err)
	}
	return reply
}

func closeReq(paymentNo, requestID string) *rpc.ClosePaymentReq {
	return &rpc.ClosePaymentReq{
		PaymentNo: paymentNo, Operator: "运营工号 A01", RequestId: requestID, Reason: "订单作废",
	}
}

func paidBalancePayment(db *fakeDB, paymentNo, bizOrderNo, requestID string, mid, amount int64) *model.Payment {
	return db.seedPayment(&model.Payment{
		PaymentNo: paymentNo, BizOrderNo: bizOrderNo, RequestId: requestID, Mid: mid,
		AmountMinor: amount, Currency: "CNY", Method: model.MethodBalance,
		State: model.PaymentStatePaid, Operator: "user",
	})
}

// --- CreatePayment: BALANCE 主路径 ---

func TestCreatePaymentBalanceDeductsInOneTransaction(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")

	reply := mustCreatePayment(t, svcCtx, payReq("order-1", "req-1", 7, 2000))

	if reply.Duplicated {
		t.Fatal("首笔受理不该是 duplicated")
	}
	if !strings.HasPrefix(reply.Payment.PaymentNo, "PM_") {
		t.Fatalf("payment_no = %q，应为 PM_<ULID>", reply.Payment.PaymentNo)
	}
	if reply.Payment.State != rpc.PaymentState_PAYMENT_STATE_PAID {
		t.Fatalf("建单即终态应为 PAID，实际 %s", reply.Payment.State)
	}
	if reply.Payment.RefundedMinor != 0 {
		t.Fatalf("新单已退额 = %d，应为 0", reply.Payment.RefundedMinor)
	}
	if reply.Wallet.BalanceMinor != 3000 {
		t.Fatalf("回复余额 = %d，期望 3000", reply.Wallet.BalanceMinor)
	}
	if reply.Payment.PaidAt <= 0 {
		t.Error("PAID 单必须落 paid_at")
	}
	if db.txRuns != 1 {
		t.Fatalf("扣款+落单+落流水应只开 1 个事务，实际 txRuns=%d", db.txRuns)
	}
	requireBalance(t, db, 7, 3000)
	requireFlows(t, db, 1)

	row := db.payments[reply.Payment.PaymentNo]
	if row == nil {
		t.Fatal("支付单没落库")
	}
	if row.LastRequestId != "req-1" || row.RequestId != "req-1" {
		t.Fatalf("支付单未绑定受理请求号：request_id=%q last_request_id=%q", row.RequestId, row.LastRequestId)
	}
	fl := db.flows[0]
	if fl.BizType != model.FlowBizPayment {
		t.Fatalf("流水业务类型 = %d，期望 PAYMENT(%d)", fl.BizType, model.FlowBizPayment)
	}
	if fl.BizNo != row.PaymentNo {
		t.Fatalf("流水 biz_no = %q，必须指向支付单号 %q", fl.BizNo, row.PaymentNo)
	}
	if fl.DeltaMinor != -2000 || fl.BalanceAfterMinor != 3000 {
		t.Fatalf("流水金额不对：delta=%d balance_after=%d", fl.DeltaMinor, fl.BalanceAfterMinor)
	}
	if fl.RequestId != "req-1" || fl.Operator != "user" || fl.Currency != "CNY" {
		t.Fatalf("流水追溯列缺失：%+v", fl)
	}
}

func TestCreatePaymentInsufficientBalanceWritesNothing(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 1000, "CNY")

	_, err := NewCreatePaymentLogic(context.Background(), svcCtx).CreatePayment(payReq("order-2", "req-2", 7, 2000))

	requireSentinel(t, err, model.ErrInsufficientBalance, codes.FailedPrecondition)
	if len(db.payments) != 0 {
		t.Fatalf("余额不足却落了支付单：%d 行", len(db.payments))
	}
	requireFlows(t, db, 0)
	requireBalance(t, db, 7, 1000)
	if db.rollbacks != 1 {
		t.Fatalf("失败事务应回滚 1 次，实际 %d", db.rollbacks)
	}
}

func TestCreatePaymentRejectedWithoutWalletRow(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())

	_, err := NewCreatePaymentLogic(context.Background(), svcCtx).CreatePayment(payReq("order-3", "req-3", 7, 100))

	requireSentinel(t, err, model.ErrInsufficientBalance, codes.FailedPrecondition)
	if _, ok := db.wallets[7]; ok {
		t.Fatal("扣款失败不应留下余额账户行（建行的幂等写入也必须随事务回滚）")
	}
	requireFlows(t, db, 0)
	if len(db.payments) != 0 {
		t.Fatalf("失败了还建单：%d 行", len(db.payments))
	}
}

func TestCreatePaymentWalletCurrencyMismatchNeverConverts(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 9000, "USD") // 账户币种与配置不一致：只能拒，不能按 1:1 记账

	_, err := NewCreatePaymentLogic(context.Background(), svcCtx).CreatePayment(payReq("order-4", "req-4", 7, 100))

	requireSentinel(t, err, model.ErrUnsupportedCurrency, codes.InvalidArgument)
	requireBalance(t, db, 7, 9000)
	requireFlows(t, db, 0)
	if len(db.payments) != 0 {
		t.Fatal("币种不可判等时不得建单")
	}
}

func TestCreatePaymentFlowFailureRollsBackDeduction(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")
	db.flowErr = errors.New("pm_flow InsertTx: connection reset by peer")

	_, err := NewCreatePaymentLogic(context.Background(), svcCtx).CreatePayment(payReq("order-5", "req-5", 7, 2000))

	if err == nil || !strings.Contains(err.Error(), "pm_flow InsertTx") {
		t.Fatalf("写流水失败必须上抛，实际 %v", err)
	}
	// 不允许出现「钱扣了、单在、流水丢了」的半成品台账。
	requireBalance(t, db, 7, 5000)
	if len(db.payments) != 0 {
		t.Fatalf("流水失败后支付单仍在：%d 行", len(db.payments))
	}
	requireFlows(t, db, 0)
}

func TestCreatePaymentDocumentNoFailureWritesNothing(t *testing.T) {
	withFailingIDGen(t)
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")

	_, err := NewCreatePaymentLogic(context.Background(), svcCtx).CreatePayment(payReq("order-6", "req-6", 7, 1000))

	requireSentinel(t, err, model.ErrDocumentNoUnavailable, codes.Internal)
	requireBalance(t, db, 7, 5000)
	requireFlows(t, db, 0)
	if len(db.payments) != 0 || db.txRuns != 0 {
		t.Fatalf("拿不到单据号却进了事务：payments=%d txRuns=%d", len(db.payments), db.txRuns)
	}
}

// --- CreatePayment: SANDBOX_CHANNEL ---

func TestCreatePaymentSandboxChannelNeverTouchesBalance(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")
	in := payReq("order-7", "req-7", 7, 2000)
	in.Method = rpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL

	reply := mustCreatePayment(t, svcCtx, in)

	requireBalance(t, db, 7, 5000)
	requireFlows(t, db, 0) // 钱没经过余额账户，写余额流水就是凭空造账
	if reply.Payment.State != rpc.PaymentState_PAYMENT_STATE_PAID {
		t.Fatalf("沙箱收单应受理即 PAID，实际 %s", reply.Payment.State)
	}
	if reply.Wallet.BalanceMinor != 5000 {
		t.Fatalf("回复余额应是未变动的快照，实际 %d", reply.Wallet.BalanceMinor)
	}
	row := db.payments[reply.Payment.PaymentNo]
	if row.Method != model.MethodSandboxChannel {
		t.Fatalf("method = %d，期望 SANDBOX_CHANNEL(%d)", row.Method, model.MethodSandboxChannel)
	}
	if db.txRuns != 1 {
		t.Fatalf("落单仍应在事务里（txRuns=%d）", db.txRuns)
	}
}

func TestCreatePaymentSandboxChannelRespectsChannelGate(t *testing.T) {
	cfg := defaultPaymentConf()
	cfg.AllowedChannels = []string{"ALIPAY"}
	svcCtx, db := newTestSvc(t, cfg)
	db.seedWallet(7, 5000, "CNY")
	in := payReq("order-8", "req-8", 7, 1000)
	in.Method = rpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL

	_, err := NewCreatePaymentLogic(context.Background(), svcCtx).CreatePayment(in)

	requireSentinel(t, err, model.ErrChannelNotConfigured, codes.FailedPrecondition)
	requireBalance(t, db, 7, 5000)
	if len(db.payments) != 0 {
		t.Fatal("渠道未配置时不得建单（更不能假装成功）")
	}
}

// --- CreatePayment: 幂等与一单一支付 ---

func TestCreatePaymentReplayByRequestIDDoesNotDeductTwice(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")
	first := mustCreatePayment(t, svcCtx, payReq("order-9", "req-9", 7, 2000))

	// 同 request_id 重放，金额也改了：幂等键优先，结论仍是首单。
	again := mustCreatePayment(t, svcCtx, payReq("order-9", "req-9", 7, 9999))

	if !again.Duplicated {
		t.Fatal("重放必须 duplicated=true")
	}
	if again.Payment.PaymentNo != first.Payment.PaymentNo {
		t.Fatalf("重放返回了另一张单：%s vs %s", again.Payment.PaymentNo, first.Payment.PaymentNo)
	}
	if again.Payment.AmountMinor != 2000 {
		t.Fatalf("重放单金额 = %d，应保持首单的 2000", again.Payment.AmountMinor)
	}
	requireBalance(t, db, 7, 3000)
	requireFlows(t, db, 1)
	if len(db.payments) != 1 {
		t.Fatalf("重放多出了单据：%d 行", len(db.payments))
	}
	if db.txRuns != 1 {
		t.Fatalf("重放不该再进事务（txRuns=%d）", db.txRuns)
	}
}

func TestCreatePaymentSameOrderSameMoneyReplays(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")
	first := mustCreatePayment(t, svcCtx, payReq("order-10", "req-10", 7, 1500))

	// 换了 request_id 但订单号、金额、币种都一样：客户端丢了响应，按重放处理。
	again := mustCreatePayment(t, svcCtx, payReq("order-10", "req-10-new", 7, 1500))

	if !again.Duplicated || again.Payment.PaymentNo != first.Payment.PaymentNo {
		t.Fatalf("同单同价应为重放首单，实际 duplicated=%v payment_no=%s",
			again.Duplicated, again.Payment.PaymentNo)
	}
	requireBalance(t, db, 7, 3500)
	requireFlows(t, db, 1)
}

func TestCreatePaymentSameOrderDifferentAmountConflicts(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")
	first := mustCreatePayment(t, svcCtx, payReq("order-11", "req-11", 7, 1500))

	_, err := NewCreatePaymentLogic(context.Background(), svcCtx).
		CreatePayment(payReq("order-11", "req-11-cheap", 7, 100))

	requireStatus(t, err, codes.AlreadyExists, "biz_order_no already has a payment")
	requireBalance(t, db, 7, 3500) // 绝不为新金额再扣一次
	requireFlows(t, db, 1)
	if len(db.payments) != 1 || db.payments[first.Payment.PaymentNo].AmountMinor != 1500 {
		t.Fatalf("冲突路径改了台账：%d 行", len(db.payments))
	}
}

func TestCreatePaymentSameOrderDifferentCurrencyConflicts(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	cfg := defaultPaymentConf()
	svcCtx.Config.Payment = cfg
	db.seedWallet(7, 5000, "CNY")
	paid := paidBalancePayment(db, "PM_existing", "order-12", "req-12", 7, 1500)
	paid.Currency = "HKD" // 直接改已落库的行：模拟历史上另一币种的同名订单

	_, err := NewCreatePaymentLogic(context.Background(), svcCtx).
		CreatePayment(payReq("order-12", "req-12-USD", 7, 1500))

	requireSentinel(t, err, model.ErrPaymentOrderConflict, codes.AlreadyExists)
	requireBalance(t, db, 7, 5000)
	requireFlows(t, db, 0)
}

// TestCreatePaymentConcurrentDuplicateResolvesToWinner 并发对手先提交：
// 本事务 1062 回滚后必须把结论收敛成「重放对手那张单」，不能二次扣款。
func TestCreatePaymentConcurrentDuplicateResolvesToWinner(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")
	db.paymentSteal = func(db *fakeDB) {
		paidBalancePayment(db, "PM_winner", "order-13", "req-13", 7, 2000)
	}

	reply, err := NewCreatePaymentLogic(context.Background(), svcCtx).
		CreatePayment(payReq("order-13", "req-13", 7, 2000))
	if err != nil {
		t.Fatalf("并发重复应收敛成重放，实际报错: %v", err)
	}
	if !reply.Duplicated || reply.Payment.PaymentNo != "PM_winner" {
		t.Fatalf("重放结论不符：duplicated=%v payment_no=%s", reply.Duplicated, reply.Payment.PaymentNo)
	}
	// 本次失败尝试的扣减已随事务回滚（对手自己那笔的扣减属于对手事务，不在本假实现范围内）。
	requireBalance(t, db, 7, 5000)
	if len(db.payments) != 1 {
		t.Fatalf("并发下多出了单据：%d 行", len(db.payments))
	}
	requireFlows(t, db, 0)
}

// TestCreatePaymentDuplicateWithoutRowsAsksNewRequestID 唯一键撞了却回查不到任何单据：
// 只能要求换号，不能猜一张单返回。
func TestCreatePaymentDuplicateWithoutRowsAsksNewRequestID(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")
	db.paymentDup = true

	_, err := NewCreatePaymentLogic(context.Background(), svcCtx).
		CreatePayment(payReq("order-14", "req-14", 7, 1000))

	requireSentinel(t, err, model.ErrRequestIDReused, codes.AlreadyExists)
	if !strings.Contains(err.Error(), "nothing was charged or credited") {
		t.Fatalf("冲突错误要说明台账未变动，实际 %v", err)
	}
	requireBalance(t, db, 7, 5000)
	requireFlows(t, db, 0)
	if len(db.payments) != 0 {
		t.Fatalf("失败了还建单：%d 行", len(db.payments))
	}
}

// --- CreatePayment: 入参校验 ---

func TestCreatePaymentValidationMatrix(t *testing.T) {
	cases := []struct {
		name     string
		patch    func(*rpc.CreatePaymentReq)
		sentinel error
		code     codes.Code
		msg      string
	}{
		{"缺订单号", func(r *rpc.CreatePaymentReq) { r.BizOrderNo = "  " },
			model.ErrBizOrderNoRequired, codes.InvalidArgument, ""},
		{"订单号超长", func(r *rpc.CreatePaymentReq) { r.BizOrderNo = strings.Repeat("o", 65) },
			nil, codes.InvalidArgument, "biz_order_no too long"},
		{"mid 非法", func(r *rpc.CreatePaymentReq) { r.Mid = 0 },
			model.ErrInvalidMid, codes.InvalidArgument, ""},
		{"金额非正", func(r *rpc.CreatePaymentReq) { r.AmountMinor = 0 },
			model.ErrAmountNotPositive, codes.InvalidArgument, ""},
		{"负金额", func(r *rpc.CreatePaymentReq) { r.AmountMinor = -100 },
			model.ErrAmountNotPositive, codes.InvalidArgument, ""},
		{"外币币种", func(r *rpc.CreatePaymentReq) { r.Currency = "USD" },
			model.ErrUnsupportedCurrency, codes.InvalidArgument, ""},
		{"缺幂等键", func(r *rpc.CreatePaymentReq) { r.RequestId = "" },
			model.ErrRequestIDRequired, codes.InvalidArgument, ""},
		{"幂等键超长", func(r *rpc.CreatePaymentReq) { r.RequestId = strings.Repeat("r", 65) },
			nil, codes.InvalidArgument, "request_id too long"},
		{"缺操作人", func(r *rpc.CreatePaymentReq) { r.Operator = " " },
			model.ErrOperatorRequired, codes.InvalidArgument, ""},
		{"摘要超长", func(r *rpc.CreatePaymentReq) { r.Subject = strings.Repeat("会", 101) },
			nil, codes.InvalidArgument, "subject too long"},
		{"过期时间在过去", func(r *rpc.CreatePaymentReq) { r.ExpireAt = fakeNow() - 60 },
			model.ErrExpireInPast, codes.InvalidArgument, ""},
		{"过期时间为负", func(r *rpc.CreatePaymentReq) { r.ExpireAt = -1 },
			model.ErrExpireInPast, codes.InvalidArgument, ""},
		{"支付方式未指定", func(r *rpc.CreatePaymentReq) { r.Method = rpc.PayMethod_PAY_METHOD_UNSPECIFIED },
			nil, codes.InvalidArgument, "payment: unsupported pay method; method=PAY_METHOD_UNSPECIFIED"},
		{"支付方式为枚举外的值", func(r *rpc.CreatePaymentReq) { r.Method = rpc.PayMethod(99) },
			nil, codes.InvalidArgument, "method=99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t, defaultPaymentConf())
			db.seedWallet(7, 5000, "CNY")
			in := payReq("order-valid", "req-valid", 7, 1000)
			tc.patch(in)

			_, err := NewCreatePaymentLogic(context.Background(), svcCtx).CreatePayment(in)

			if tc.sentinel != nil {
				requireSentinel(t, err, tc.sentinel, tc.code)
			} else {
				requireStatus(t, err, tc.code, tc.msg)
			}
			requireBalance(t, db, 7, 5000)
			requireFlows(t, db, 0)
			if len(db.payments) != 0 {
				t.Fatalf("入参不合法却建了单：%d 行", len(db.payments))
			}
		})
	}
}

func TestCreatePaymentAcceptsFutureExpireAndLowerCaseCurrency(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")
	in := payReq("order-15", "req-15", 7, 1000)
	in.Currency = " cny "
	in.ExpireAt = fakeNow() + 600

	reply := mustCreatePayment(t, svcCtx, in)

	if reply.Payment.Currency != "CNY" {
		t.Fatalf("币种未归一：%q", reply.Payment.Currency)
	}
	if row := db.payments[reply.Payment.PaymentNo]; row.ExpireAt != in.ExpireAt {
		t.Fatalf("expire_at = %d，期望原样落库 %d", row.ExpireAt, in.ExpireAt)
	}
}

// --- ClosePayment ---

func TestClosePaymentPendingWritesAuditFieldsAndNoMoney(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedWallet(7, 5000, "CNY")
	db.seedPayment(&model.Payment{PaymentNo: "PM_p1", BizOrderNo: "order-16", RequestId: "req-16",
		Mid: 7, AmountMinor: 1000, Currency: "CNY", Method: model.MethodBalance,
		State: model.PaymentStatePending, Operator: "user"})

	reply, err := NewClosePaymentLogic(context.Background(), svcCtx).ClosePayment(closeReq("PM_p1", "req-close"))
	if err != nil {
		t.Fatalf("关闭 PENDING 单失败: %v", err)
	}
	if reply.Duplicated {
		t.Fatal("首次关闭不该 duplicated")
	}
	if reply.Payment.State != rpc.PaymentState_PAYMENT_STATE_CLOSED {
		t.Fatalf("state = %s，期望 CLOSED", reply.Payment.State)
	}
	if reply.Payment.Operator != "运营工号 A01" || reply.Payment.Remark != "订单作废" {
		t.Fatalf("关闭动作未留审计列：operator=%q remark=%q", reply.Payment.Operator, reply.Payment.Remark)
	}
	row := db.payments["PM_p1"]
	if row.LastRequestId != "req-close" {
		t.Fatalf("last_request_id = %q，关闭留痕缺失", row.LastRequestId)
	}
	requireBalance(t, db, 7, 5000) // PENDING 从未扣过款，关闭不得改余额
	requireFlows(t, db, 0)
}

func TestClosePaymentTwiceIsDuplicated(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedPayment(&model.Payment{PaymentNo: "PM_c1", BizOrderNo: "order-17", RequestId: "req-17",
		Mid: 7, AmountMinor: 1000, Currency: "CNY", Method: model.MethodBalance,
		State: model.PaymentStateClosed, Operator: "user", Remark: "订单作废"})

	reply, err := NewClosePaymentLogic(context.Background(), svcCtx).ClosePayment(closeReq("PM_c1", "req-close-2"))
	if err != nil {
		t.Fatalf("重复关闭应按幂等返回: %v", err)
	}
	if !reply.Duplicated || reply.Payment.PaymentNo != "PM_c1" {
		t.Fatalf("重复关闭结论不符: duplicated=%v payment=%+v", reply.Duplicated, reply.Payment)
	}
	requireFlows(t, db, 0)
}

func TestClosePaymentPaidRefusesAndPointsToRefund(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	paid := paidBalancePayment(db, "PM_paid", "order-18", "req-18", 7, 1000)
	db.seedWallet(7, 4000, "CNY")

	_, err := NewClosePaymentLogic(context.Background(), svcCtx).ClosePayment(closeReq("PM_paid", "req-close-3"))

	requireSentinel(t, err, model.ErrPaymentAlreadyPaid, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "RefundPayment") {
		t.Fatalf("拒绝理由要指明退回路径，实际 %v", err)
	}
	if db.payments["PM_paid"].State != paid.State {
		t.Fatal("关闭失败却改了状态")
	}
	requireBalance(t, db, 7, 4000)
	requireFlows(t, db, 0)
}

func TestClosePaymentRefundedStatesRefuse(t *testing.T) {
	for _, state := range []int32{model.PaymentStateRefunded, model.PaymentStatePartiallyRefunded} {
		svcCtx, db := newTestSvc(t, defaultPaymentConf())
		db.seedPayment(&model.Payment{PaymentNo: "PM_s", BizOrderNo: "order-19", RequestId: "req-19",
			Mid: 7, AmountMinor: 1000, Currency: "CNY", Method: model.MethodBalance, State: state})

		_, err := NewClosePaymentLogic(context.Background(), svcCtx).ClosePayment(closeReq("PM_s", "req-close-4"))

		requireSentinel(t, err, model.ErrPaymentAlreadyPaid, codes.FailedPrecondition)
		requireFlows(t, db, 0)
	}
}

func TestClosePaymentFailedIsNotPending(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedPayment(&model.Payment{PaymentNo: "PM_f", BizOrderNo: "order-20", RequestId: "req-20",
		Mid: 7, AmountMinor: 1000, Currency: "CNY", Method: model.MethodBalance,
		State: model.PaymentStateFailed})

	_, err := NewClosePaymentLogic(context.Background(), svcCtx).ClosePayment(closeReq("PM_f", "req-close-5"))

	requireStatus(t, err, codes.FailedPrecondition, "payment_state=PAYMENT_STATE_FAILED")
}

func TestClosePaymentNotFound(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())

	_, err := NewClosePaymentLogic(context.Background(), svcCtx).ClosePayment(closeReq("PM_ghost", "req-close-6"))

	requireSentinel(t, err, model.ErrPaymentNotFound, codes.NotFound)
	requireFlows(t, db, 0)
}

// TestClosePaymentConcurrentlyPaid 检查与推进之间被并发付了款：
// CAS 0 行时按真实状态给结论，绝不把「已付款」说成「已关闭」。
func TestClosePaymentConcurrentlyPaid(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedPayment(&model.Payment{PaymentNo: "PM_race", BizOrderNo: "order-21", RequestId: "req-21",
		Mid: 7, AmountMinor: 1000, Currency: "CNY", Method: model.MethodBalance,
		State: model.PaymentStatePending})
	db.closeSteal = func(db *fakeDB) {
		row := db.payments["PM_race"]
		row.State = model.PaymentStatePaid
	}

	_, err := NewClosePaymentLogic(context.Background(), svcCtx).ClosePayment(closeReq("PM_race", "req-close-7"))

	requireSentinel(t, err, model.ErrPaymentAlreadyPaid, codes.FailedPrecondition)
	if db.payments["PM_race"].State != model.PaymentStatePaid {
		t.Fatal("对手已提交的付款被本次失败覆盖了")
	}
}

func TestClosePaymentCasMissIsAborted(t *testing.T) {
	svcCtx, db := newTestSvc(t, defaultPaymentConf())
	db.seedPayment(&model.Payment{PaymentNo: "PM_miss", BizOrderNo: "order-22", RequestId: "req-22",
		Mid: 7, AmountMinor: 1000, Currency: "CNY", Method: model.MethodBalance,
		State: model.PaymentStatePending})
	db.closeMiss = true // 条件未命中但状态仍是 PENDING：纯并发抖动

	_, err := NewClosePaymentLogic(context.Background(), svcCtx).ClosePayment(closeReq("PM_miss", "req-close-8"))

	requireSentinel(t, err, model.ErrConcurrentUpdate, codes.Aborted)
	requireFlows(t, db, 0)
}

func TestClosePaymentValidation(t *testing.T) {
	cases := []struct {
		name     string
		patch    func(*rpc.ClosePaymentReq)
		sentinel error
		msg      string
	}{
		{"缺单号", func(r *rpc.ClosePaymentReq) { r.PaymentNo = " " }, model.ErrPaymentNoRequired, ""},
		{"单号超长", func(r *rpc.ClosePaymentReq) { r.PaymentNo = strings.Repeat("p", 41) },
			nil, "payment_no too long"},
		{"缺幂等键", func(r *rpc.ClosePaymentReq) { r.RequestId = "" }, model.ErrRequestIDRequired, ""},
		{"缺操作人", func(r *rpc.ClosePaymentReq) { r.Operator = "" }, model.ErrOperatorRequired, ""},
		{"缺理由", func(r *rpc.ClosePaymentReq) { r.Reason = "  " }, model.ErrReasonRequired, ""},
		{"理由超长", func(r *rpc.ClosePaymentReq) { r.Reason = strings.Repeat("作", 101) },
			nil, "reason too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svcCtx, db := newTestSvc(t, defaultPaymentConf())
			db.seedPayment(&model.Payment{PaymentNo: "PM_v", BizOrderNo: "order-23", RequestId: "req-23",
				Mid: 7, AmountMinor: 1000, Currency: "CNY", Method: model.MethodBalance,
				State: model.PaymentStatePending})
			in := closeReq("PM_v", "req-close-9")
			tc.patch(in)

			_, err := NewClosePaymentLogic(context.Background(), svcCtx).ClosePayment(in)

			if tc.sentinel != nil {
				requireSentinel(t, err, tc.sentinel, codes.InvalidArgument)
			} else {
				requireStatus(t, err, codes.InvalidArgument, tc.msg)
			}
			if db.payments["PM_v"].State != model.PaymentStatePending {
				t.Fatal("入参不合法却推进了状态")
			}
		})
	}
}

// TestPaymentChainKeepsSandboxGateWithoutConfig 缺省配置（AllowedChannels 为空）时
// 沙箱收单可用，且任何真实渠道都不会被放行。
func TestPaymentChainKeepsSandboxGateWithoutConfig(t *testing.T) {
	cfg := defaultPaymentConf()
	cfg.AllowedChannels = nil
	svcCtx, db := newTestSvc(t, cfg)
	db.seedWallet(7, 100, "CNY")
	in := payReq("order-24", "req-24", 7, 1000)
	in.Method = rpc.PayMethod_PAY_METHOD_SANDBOX_CHANNEL

	if _, err := NewCreatePaymentLogic(context.Background(), svcCtx).CreatePayment(in); err != nil {
		t.Fatalf("缺省渠道白名单应只放行沙箱: %v", err)
	}
	for _, name := range []string{"ALIPAY", "WECHAT", "", "99"} {
		if (config.PaymentConf{}).AllowsChannel(name) {
			t.Fatalf("默认口径放行了渠道 %q", name)
		}
	}
}
